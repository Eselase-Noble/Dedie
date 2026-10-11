// Package orasrc is the Oracle source side shared by every pipeline that
// migrates out of Oracle.
//
// It knows what is in the source and how to read it, and nothing about
// where the data goes: the catalog is loaded as plain facts for each
// pipeline to translate into its target's dialect, values are read as
// text in one fixed form per kind of column, and changes are followed
// through an interface the pipeline implements for its target.
//
// Tables are read with flashback queries, so all of them can be read as
// of one system change number (SCN) without holding a lock on the source.
// Changes after that point are found with LogMiner. The redo stream is
// used only to learn which rows a committed transaction touched; each row
// is then read back from the source as of that commit and stored whole.
// That keeps large objects and every data type on the same code path as
// the copy, and makes replaying a change harmless.
package orasrc

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

// Column is a source column.
type Column struct {
	Name     string // as Oracle stores it
	DataType string
	// Precision and Scale are those of a NUMBER; Scale is also the
	// fractional seconds of a TIMESTAMP.
	Precision  sql.NullInt64
	Scale      sql.NullInt64
	CharLength int64
	Nullable   bool
	Virtual    bool
	Default    string
	// IdentitySeq is the sequence behind an identity column.
	IdentitySeq string
	// Unsupported marks a column whose type cannot be migrated: one that
	// Classify does not know, or a user-defined type. Its Kind is KindText
	// so that code which walks the columns need not treat it specially;
	// preflight stops a job that has any.
	Unsupported bool
	// Kind is how the column's values are read.
	Kind Kind
}

// Table is a source table.
type Table struct {
	Owner, Name string
	Cols        []Column
	// PK holds the positions in Cols of the primary key's columns, in key
	// order. It is empty for a table without an enabled primary key.
	PK      []int
	EstRows int64
}

// Source is the table's quoted name in Oracle.
func (t *Table) Source() string { return oracle.QName(t.Owner, t.Name) }

// Label names the table in messages.
func (t *Table) Label() string { return t.Owner + "." + t.Name }

// Catalog is what will be migrated, as the source describes it.
type Catalog struct {
	// Owners are the Oracle schemas in scope.
	Owners []string
	Tables []*Table
	// Sequences are listed when whole schemas are migrated; a table-level
	// migration takes only what its identity columns need.
	Sequences []Sequence
	// Indexes, Checks and ForeignKeys belong to the tables in scope.
	Indexes     []Index
	Checks      []Check
	ForeignKeys []ForeignKey
	// Unsupported lists columns whose type cannot be migrated.
	Unsupported []string
	byName      map[string]*Table
}

// Table finds a table by its Oracle owner and name.
func (c *Catalog) Table(owner, name string) *Table { return c.byName[owner+"."+name] }

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ownerList is the schemas in scope as a list of SQL string literals.
func (c *Catalog) ownerList() string {
	quoted := make([]string, len(c.Owners))
	for i, o := range c.Owners {
		quoted[i] = literal(o)
	}
	return strings.Join(quoted, ", ")
}

// scopeOf works out which Oracle schemas and tables the options select.
// Names are matched without regard to case, as Oracle does for unquoted
// names. only is nil when every table of the schemas is wanted.
func scopeOf(cfg migrate.Config, user string) (owners []string, only, exclude map[string]bool) {
	seen := map[string]bool{}
	addOwner := func(o string) {
		o = strings.ToUpper(strings.TrimSpace(o))
		if o != "" && !seen[o] {
			seen[o] = true
			owners = append(owners, o)
		}
	}
	full := func(name string) string {
		name = strings.ToUpper(strings.TrimSpace(name))
		if !strings.Contains(name, ".") {
			name = strings.ToUpper(user) + "." + name
		}
		return name
	}
	exclude = map[string]bool{}
	for _, t := range cfg.ExcludeTables {
		exclude[full(t)] = true
	}
	if len(cfg.Tables) > 0 {
		only = map[string]bool{}
		for _, t := range cfg.Tables {
			name := full(t)
			only[name] = true
			addOwner(name[:strings.Index(name, ".")])
		}
		return owners, only, exclude
	}
	for _, s := range cfg.Schemas {
		addOwner(s)
	}
	if len(owners) == 0 {
		addOwner(user)
	}
	return owners, nil, exclude
}

// query runs a catalog query and returns its rows as text; NULL comes
// back as an empty string.
func query(ctx context.Context, src oracle.Queryer, text string, args ...any) ([][]string, error) {
	rows, err := src.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, oracle.Friendly(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		raw := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, oracle.Friendly(err)
		}
		row := make([]string, len(cols))
		for i, v := range raw {
			row[i] = v.String
		}
		out = append(out, row)
	}
	return out, oracle.Friendly(rows.Err())
}

func nullInt(s string) sql.NullInt64 {
	var n sql.NullInt64
	if s != "" {
		if _, err := fmt.Sscan(s, &n.Int64); err == nil {
			n.Valid = true
		}
	}
	return n
}

// LoadCatalog reads from the source's data dictionary everything the
// options put in scope. It gathers facts only; what they become on the
// target is for the pipeline to decide.
func LoadCatalog(ctx context.Context, src oracle.Queryer, cfg migrate.Config) (*Catalog, error) {
	var user string
	if err := src.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM dual`).Scan(&user); err != nil {
		return nil, oracle.Friendly(err)
	}
	owners, only, exclude := scopeOf(cfg, user)
	c := &Catalog{Owners: owners, byName: map[string]*Table{}}
	in := "IN (" + c.ownerList() + ")"

	known, err := query(ctx, src, `SELECT username FROM all_users WHERE username `+in)
	if err != nil {
		return nil, err
	}
	if len(known) != len(owners) {
		have := map[string]bool{}
		for _, r := range known {
			have[r[0]] = true
		}
		for _, o := range owners {
			if !have[o] {
				return nil, fmt.Errorf("the source has no schema named %s", o)
			}
		}
	}

	// Tables that only exist to serve another object are left out: nested
	// and overflow tables, materialized view containers, external tables, the recycle bin.
	rows, err := query(ctx, src, `SELECT t.owner, t.table_name, NVL(t.num_rows, 0) FROM all_tables t
		WHERE t.owner `+in+` AND t.nested = 'NO' AND t.secondary = 'N' AND t.dropped = 'NO' AND t.temporary = 'N'
		  AND (t.iot_type IS NULL OR t.iot_type = 'IOT')
		  AND NOT EXISTS (SELECT 1 FROM all_external_tables e WHERE e.owner = t.owner AND e.table_name = t.table_name)
		  AND NOT EXISTS (SELECT 1 FROM all_mviews m WHERE m.owner = t.owner AND m.container_name = t.table_name)
		ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		key := strings.ToUpper(r[0] + "." + r[1])
		if exclude[key] || (only != nil && !only[key]) {
			continue
		}
		delete(only, key)
		t := &Table{Owner: r[0], Name: r[1]}
		fmt.Sscan(r[2], &t.EstRows)
		c.Tables = append(c.Tables, t)
		c.byName[r[0]+"."+r[1]] = t
	}
	if len(only) > 0 {
		var missing []string
		for name := range only {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("these tables do not exist on the source or cannot be read by this account: %s", strings.Join(missing, ", "))
	}
	if len(c.Tables) == 0 {
		return nil, fmt.Errorf("there are no tables to migrate in %s; name the schemas to migrate if they are not the connecting user's own", strings.Join(owners, ", "))
	}

	if err := c.loadColumns(ctx, src, in); err != nil {
		return nil, err
	}
	if err := c.loadKeys(ctx, src, in); err != nil {
		return nil, err
	}
	// Sequences are carried over for whole schemas; a table-level
	// migration takes only what its identity columns need.
	if only == nil {
		if err := c.loadSequences(ctx, src, in); err != nil {
			return nil, err
		}
	}
	if err := c.loadForeignKeys(ctx, src, in); err != nil {
		return nil, err
	}
	if err := c.loadIndexes(ctx, src, in); err != nil {
		return nil, err
	}
	if err := c.loadChecks(ctx, src, in); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Catalog) loadColumns(ctx context.Context, src oracle.Queryer, in string) error {
	// ALL_TAB_COLS also lists the hidden columns Oracle adds for itself.
	rows, err := query(ctx, src, `SELECT c.owner, c.table_name, c.column_name, c.data_type, c.data_type_owner, c.data_precision, c.data_scale,
			c.char_length, c.nullable, c.virtual_column, c.data_default,
			(SELECT MAX(i.sequence_name) FROM all_tab_identity_cols i WHERE i.owner = c.owner AND i.table_name = c.table_name AND i.column_name = c.column_name)
		FROM all_tab_cols c WHERE c.owner `+in+` AND c.hidden_column = 'NO' ORDER BY c.owner, c.table_name, c.column_id`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		t := c.Table(r[0], r[1])
		if t == nil {
			continue
		}
		col := Column{Name: r[2], DataType: r[3], Precision: nullInt(r[5]), Scale: nullInt(r[6]),
			Nullable: r[8] == "Y", Virtual: r[9] == "YES", Default: strings.TrimSpace(r[10]), IdentitySeq: r[11]}
		fmt.Sscan(r[7], &col.CharLength)
		// A type with an owner other than Oracle's own is user-defined.
		userType := r[4] != "" && r[4] != "SYS" && r[4] != "PUBLIC"
		kind, ok := Classify(col.DataType)
		if !ok || userType {
			c.Unsupported = append(c.Unsupported, fmt.Sprintf("%s.%s (%s)", t.Label(), col.Name, col.DataType))
			col.Unsupported, kind = true, KindText
		}
		col.Kind = kind
		t.Cols = append(t.Cols, col)
	}
	return nil
}

func (c *Catalog) loadKeys(ctx context.Context, src oracle.Queryer, in string) error {
	rows, err := query(ctx, src, `SELECT k.owner, k.table_name, c.column_name FROM all_constraints k
		JOIN all_cons_columns c ON c.owner = k.owner AND c.constraint_name = k.constraint_name
		WHERE k.owner `+in+` AND k.constraint_type = 'P' AND k.status = 'ENABLED' ORDER BY 1, 2, c.position`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		t := c.Table(r[0], r[1])
		if t == nil {
			continue
		}
		for i, col := range t.Cols {
			if col.Name == r[2] {
				t.PK = append(t.PK, i)
			}
		}
	}
	return nil
}
