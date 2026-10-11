package mymy

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

var errNoBinlog = errors.New("binary logging is not enabled on the source")

// Stmt is one DDL statement to run on the target. Soft statements may fail
// without failing the job; the failure is reported to the operator.
type Stmt struct {
	Kind string
	Name string
	SQL  string
	Soft bool
	err  error
}

// Column describes a table column as both the copy and the stream need it.
type Column struct {
	Name      string
	DataType  string // information_schema data_type, lower case
	Unsigned  bool
	Generated bool
	// Binary columns hold bytes rather than text and are transferred as such.
	Binary bool

	// The rest describes the column for pipelines that translate the
	// schema to another database.
	ColumnType    string // full type, for example "decimal(12,2)" or "enum('a','b')"
	Charset       string // character set of text columns, lower case
	Nullable      bool
	Default       *string // nil when the column has no default
	Extra         string  // information_schema extra, lower case
	AutoIncrement bool
}

// Table is a source table that will be copied and replicated.
type Table struct {
	Name    string
	Cols    []Column
	PK      []int // indexes into Cols; empty when the table has no primary key
	EstRows int64
}

func (t Table) QName() string { return mysql.Quote(t.Name) }

// writable returns the names of the columns that can be assigned.
func (t Table) writable() []string {
	var out []string
	for _, c := range t.Cols {
		if !c.Generated {
			out = append(out, mysql.Quote(c.Name))
		}
	}
	return out
}

type Skipped struct {
	Name   string
	Reason string
}

// Plan is everything derived from the source catalog.
type Plan struct {
	Database string
	Tables   []Table
	byName   map[string]*Table
	// Missing lists tables the operator asked for that do not exist.
	Missing []string
	Skipped []Skipped
	NoKey   []string
	Pre     []Stmt // tables
	Post    []Stmt // views and stored routines
	Final   []Stmt // triggers and scheduled events
}

var binaryTypes = map[string]bool{
	"binary": true, "varbinary": true, "tinyblob": true, "blob": true, "mediumblob": true, "longblob": true,
	"bit": true, "geometry": true, "point": true, "linestring": true, "polygon": true, "multipoint": true,
	"multilinestring": true, "multipolygon": true, "geomcollection": true, "geometrycollection": true,
}

// definerRe matches the DEFINER clause of views, routines, triggers and
// events. It is removed so objects are created under the migrating account;
// the original account often does not exist on the target server.
var definerRe = regexp.MustCompile("(?i)\\s+DEFINER\\s*=\\s*(`[^`]*`|'[^']*'|[^@\\s]+)@(`[^`]*`|'[^']*'|\\S+)")

// BuildPlan reads the source catalog for the connection's database.
func BuildPlan(ctx context.Context, src Querier, database string, cfg migrate.Config) (*Plan, error) {
	p := &Plan{Database: database, byName: map[string]*Table{}}
	excluded := map[string]bool{}
	for _, e := range cfg.ExcludeTables {
		e = strings.TrimSpace(e)
		excluded[strings.TrimPrefix(e, database+".")] = true
	}
	wanted := map[string]bool{}
	for _, name := range cfg.Tables {
		if name = strings.TrimSpace(name); name != "" {
			wanted[strings.TrimPrefix(name, database+".")] = false
		}
	}
	tableScope := len(wanted) > 0

	rows, err := src.QueryContext(ctx, `SELECT table_name, COALESCE(engine, ''), COALESCE(table_rows, 0)
		FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE' ORDER BY table_name`, database)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t Table
		var engine string
		if err := rows.Scan(&t.Name, &engine, &t.EstRows); err != nil {
			rows.Close()
			return nil, err
		}
		if tableScope {
			if _, ok := wanted[t.Name]; !ok {
				continue
			}
			wanted[t.Name] = true
		}
		switch {
		case excluded[t.Name] || t.Name == checkpointTable:
		case !strings.EqualFold(engine, "InnoDB"):
			// Only a transactional engine can be read at one consistent
			// point without holding a lock for the whole copy.
			p.Skipped = append(p.Skipped, Skipped{t.Name, "storage engine " + engine + " is not transactional; convert the table to InnoDB or migrate it separately"})
		default:
			p.Tables = append(p.Tables, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range p.Tables {
		p.byName[p.Tables[i].Name] = &p.Tables[i]
	}
	for name, found := range wanted {
		if !found {
			p.Missing = append(p.Missing, name)
		}
	}
	sort.Strings(p.Missing)

	rows, err = src.QueryContext(ctx, `SELECT table_name, column_name, LOWER(data_type), column_type LIKE '%unsigned%',
			extra LIKE '%VIRTUAL GENERATED%' OR extra LIKE '%STORED GENERATED%', column_key = 'PRI',
			column_type, LOWER(COALESCE(character_set_name, '')), is_nullable = 'YES', column_default, LOWER(extra)
		FROM information_schema.columns WHERE table_schema = ? ORDER BY table_name, ordinal_position`, database)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var table string
		var c Column
		var pk bool
		var def sql.NullString
		if err := rows.Scan(&table, &c.Name, &c.DataType, &c.Unsigned, &c.Generated, &pk,
			&c.ColumnType, &c.Charset, &c.Nullable, &def, &c.Extra); err != nil {
			rows.Close()
			return nil, err
		}
		if def.Valid {
			c.Default = &def.String
		}
		c.AutoIncrement = strings.Contains(c.Extra, "auto_increment")
		c.Binary = binaryTypes[c.DataType]
		if t := p.byName[table]; t != nil {
			if pk {
				t.PK = append(t.PK, len(t.Cols))
			}
			t.Cols = append(t.Cols, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, t := range p.Tables {
		if len(t.PK) == 0 {
			p.NoKey = append(p.NoKey, t.Name)
		}
		ddl, err := showCreate(ctx, src, "TABLE", t.Name, 1)
		if err != nil {
			return nil, err
		}
		p.Pre = append(p.Pre, Stmt{Kind: "table", Name: t.Name, SQL: ddl})
	}
	// Views, routines and events are standalone objects; a table-level
	// migration moves the named tables and their triggers only.
	if !tableScope {
		if err := p.loadViews(ctx, src); err != nil {
			return nil, err
		}
		if err := p.loadRoutines(ctx, src); err != nil {
			return nil, err
		}
	}
	if err := p.loadTriggersAndEvents(ctx, src, tableScope); err != nil {
		return nil, err
	}
	return p, nil
}

// Table returns the planned table with that name, or nil.
func (p *Plan) Table(name string) *Table { return p.byName[name] }

// showCreate runs SHOW CREATE <kind> and returns the statement, which sits in
// a different column for each kind of object.
func showCreate(ctx context.Context, src Querier, kind, name string, column int) (string, error) {
	rows, err := QueryStrings(ctx, src, "SHOW CREATE "+kind+" "+mysql.Quote(name))
	if err != nil {
		return "", err
	}
	if len(rows) == 0 || len(rows[0]) <= column {
		return "", sql.ErrNoRows
	}
	return rows[0][column], nil
}

// portable prepares a view, routine, trigger or event definition for the
// target: no DEFINER, and no reference to the source database by name.
func (p *Plan) portable(ddl string) string {
	ddl = definerRe.ReplaceAllString(ddl, "")
	return strings.ReplaceAll(ddl, mysql.Quote(p.Database)+".", "")
}

func (p *Plan) loadViews(ctx context.Context, src Querier) error {
	var names []string
	rows, err := QueryStrings(ctx, src, `SELECT table_name FROM information_schema.views WHERE table_schema = ? ORDER BY 1`, p.Database)
	if err != nil {
		return err
	}
	for _, r := range rows {
		names = append(names, r[0])
	}
	for _, name := range names {
		ddl, err := showCreate(ctx, src, "VIEW", name, 1)
		if err != nil {
			p.Post = append(p.Post, Stmt{Kind: "view", Name: name, Soft: true, err: err})
			continue
		}
		p.Post = append(p.Post, Stmt{Kind: "view", Name: name, Soft: true, SQL: p.portable(ddl)})
	}
	return nil
}

func (p *Plan) loadRoutines(ctx context.Context, src Querier) error {
	rows, err := QueryStrings(ctx, src, `SELECT routine_name, routine_type FROM information_schema.routines WHERE routine_schema = ? ORDER BY 1`, p.Database)
	if err != nil {
		return err
	}
	for _, r := range rows {
		name, kind := r[0], strings.ToUpper(r[1])
		ddl, err := showCreate(ctx, src, kind, name, 2)
		// The body is NULL when the account may not read the definition.
		if err == nil && ddl == "" {
			err = errors.New("the source account may not read this routine's definition")
		}
		if err != nil {
			p.Post = append(p.Post, Stmt{Kind: strings.ToLower(kind), Name: name, Soft: true, err: err})
			continue
		}
		p.Post = append(p.Post, Stmt{Kind: strings.ToLower(kind), Name: name, Soft: true, SQL: p.portable(ddl)})
	}
	return nil
}

// loadTriggersAndEvents defers both to cutover. With row-based logging the
// effects of a trigger are already in the stream, so a trigger created
// earlier would apply them a second time; a scheduled event would start
// changing data on the target while it is still a replica.
func (p *Plan) loadTriggersAndEvents(ctx context.Context, src Querier, tableScope bool) error {
	rows, err := QueryStrings(ctx, src, `SELECT trigger_name, event_object_table FROM information_schema.triggers WHERE trigger_schema = ? ORDER BY 1`, p.Database)
	if err != nil {
		return err
	}
	for _, r := range rows {
		// A trigger moves with its table.
		if p.byName[r[1]] == nil {
			continue
		}
		ddl, err := showCreate(ctx, src, "TRIGGER", r[0], 2)
		if err != nil {
			p.Final = append(p.Final, Stmt{Kind: "trigger", Name: r[0], Soft: true, err: err})
			continue
		}
		p.Final = append(p.Final, Stmt{Kind: "trigger", Name: r[0], Soft: true, SQL: p.portable(ddl)})
	}
	if tableScope {
		return nil
	}
	rows, err = QueryStrings(ctx, src, `SELECT event_name FROM information_schema.events WHERE event_schema = ? ORDER BY 1`, p.Database)
	if err != nil {
		return err
	}
	for _, r := range rows {
		ddl, err := showCreate(ctx, src, "EVENT", r[0], 3)
		if err != nil {
			p.Final = append(p.Final, Stmt{Kind: "event", Name: r[0], Soft: true, err: err})
			continue
		}
		p.Final = append(p.Final, Stmt{Kind: "event", Name: r[0], Soft: true, SQL: p.portable(ddl)})
	}
	return nil
}

// objects lists every name the plan takes in the target database, for the
// preflight to look up there. Apart from the tables they are read off the
// statements themselves, so what is checked and what is created cannot
// drift apart. A statement without SQL is a definition the source would not
// show; it is reported and never run.
func (p *Plan) objects() []Object {
	var out []Object
	for _, t := range p.Tables {
		out = append(out, Object{Kind: "table", Name: t.Name})
	}
	for _, list := range [][]Stmt{p.Post, p.Final} {
		for _, s := range list {
			if s.SQL != "" {
				out = append(out, Object{Kind: s.Kind, Name: s.Name})
			}
		}
	}
	return out
}

// keep removes from the plan every statement that would create an object
// the job was told to leave as it is on the target.
func (p *Plan) keep(kept []string) {
	if len(kept) == 0 {
		return
	}
	is := map[string]bool{}
	for _, k := range kept {
		is[k] = true
	}
	for _, list := range []*[]Stmt{&p.Post, &p.Final} {
		out := (*list)[:0:0]
		for _, s := range *list {
			if !is[pgpg.KeptKey(s.Kind, s.Name)] {
				out = append(out, s)
			}
		}
		*list = out
	}
}
