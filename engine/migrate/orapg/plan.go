package orapg

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
)

// valueKind says how a column's values are read from Oracle and written
// to PostgreSQL.
type valueKind int

const (
	kText valueKind = iota
	kNumber
	kDate
	kTimestamp
	kTimestampTZ
	kTimestampLTZ
	kIntervalYM
	kIntervalDS
	kBinary
	kRowid
	kXML
	kJSON
	kBool
)

// Column is a source column with its translation.
type Column struct {
	Name     string // as Oracle stores it
	Target   string // on the target
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
	PGType      string
	Kind        valueKind
}

// Table is a source table and where it goes.
type Table struct {
	Owner, Name    string // source
	Schema, Target string // target
	Cols           []Column
	PK             []int
	EstRows        int64
}

// Source is the table's quoted name in Oracle.
func (t *Table) Source() string { return oracle.QName(t.Owner, t.Name) }

// QName is the table's quoted name in PostgreSQL.
func (t *Table) QName() string { return pgx.Identifier{t.Schema, t.Target}.Sanitize() }

// Label names the table in messages.
func (t *Table) Label() string { return t.Owner + "." + t.Name }

// Sequence is a source sequence that is recreated on the target.
type Sequence struct {
	Owner, Name string
	Schema      string
	Target      string
}

// Stmt is one DDL statement to run on the target.
type Stmt struct {
	Kind string
	Name string
	SQL  string
	Soft bool
	err  error
}

// Plan is what will be migrated and how it is translated.
type Plan struct {
	// Owners are the Oracle schemas in scope.
	Owners []string
	Tables []*Table
	// Pre creates schemas, sequences and tables; Post builds indexes and
	// check constraints once the data is in; Final adds foreign keys.
	// Foreign keys wait until the end because rows are applied table by
	// table while changes are followed, not in the order the source
	// wrote them.
	Pre, Post, Final []Stmt
	Sequences        []Sequence
	// Notes list everything that was translated with a change in meaning
	// or could not be carried over. Preflight shows them to the operator.
	Notes []string
	// Unsupported lists columns whose type cannot be migrated.
	Unsupported []string
	byName      map[string]*Table
}

// Table finds a table by its Oracle owner and name.
func (p *Plan) Table(owner, name string) *Table { return p.byName[owner+"."+name] }

// translate sets a column's PostgreSQL type and how its values are read.
// It returns a note when the translation changes meaning, and false when
// the type cannot be migrated.
func translate(c *Column) (note string, ok bool) {
	t := c.DataType
	fraction := func() string {
		n := int64(6)
		if c.Scale.Valid && c.Scale.Int64 < 6 {
			n = c.Scale.Int64
		}
		return fmt.Sprintf("(%d)", n)
	}
	lossy := ""
	if c.Scale.Valid && c.Scale.Int64 > 6 {
		lossy = fmt.Sprintf("%s keeps %d fractional digits of a second; PostgreSQL keeps 6, so values are rounded to the microsecond", t, c.Scale.Int64)
	}
	switch {
	case t == "VARCHAR2" || t == "NVARCHAR2" || t == "VARCHAR":
		c.Kind, c.PGType = kText, "text"
		if c.CharLength > 0 {
			c.PGType = fmt.Sprintf("varchar(%d)", c.CharLength)
		}
	case t == "CHAR" || t == "NCHAR":
		c.Kind, c.PGType = kText, fmt.Sprintf("char(%d)", max(c.CharLength, 1))
	case t == "CLOB" || t == "NCLOB" || t == "LONG":
		c.Kind, c.PGType = kText, "text"
	case t == "NUMBER":
		c.Kind = kNumber
		p, s := c.Precision, c.Scale
		switch {
		case !p.Valid:
			// NUMBER without a precision holds any number.
			c.PGType = "numeric"
		case s.Int64 == 0 && p.Int64 <= 4:
			c.PGType = "smallint"
		case s.Int64 == 0 && p.Int64 <= 9:
			c.PGType = "integer"
		case s.Int64 == 0 && p.Int64 <= 18:
			c.PGType = "bigint"
		case s.Int64 < 0 || s.Int64 > p.Int64:
			c.PGType = "numeric"
		default:
			c.PGType = fmt.Sprintf("numeric(%d,%d)", p.Int64, s.Int64)
		}
	case t == "FLOAT":
		// FLOAT is a decimal number with a binary precision; up to 53
		// bits fits a double.
		c.Kind, c.PGType = kNumber, "numeric"
		if c.Precision.Valid && c.Precision.Int64 <= 53 {
			c.PGType = "double precision"
		}
	case t == "BINARY_FLOAT":
		c.Kind, c.PGType = kNumber, "real"
	case t == "BINARY_DOUBLE":
		c.Kind, c.PGType = kNumber, "double precision"
	case t == "DATE":
		// An Oracle DATE carries a time of day.
		c.Kind, c.PGType = kDate, "timestamp(0)"
	case strings.HasPrefix(t, "TIMESTAMP") && strings.HasSuffix(t, "WITH LOCAL TIME ZONE"):
		c.Kind, c.PGType, note = kTimestampLTZ, "timestamptz"+fraction(), lossy
	case strings.HasPrefix(t, "TIMESTAMP") && strings.HasSuffix(t, "WITH TIME ZONE"):
		c.Kind, c.PGType, note = kTimestampTZ, "timestamptz"+fraction(), lossy
		if note == "" {
			note = "TIMESTAMP WITH TIME ZONE keeps the zone each value was entered in; PostgreSQL keeps the instant only"
		}
	case strings.HasPrefix(t, "TIMESTAMP"):
		c.Kind, c.PGType, note = kTimestamp, "timestamp"+fraction(), lossy
	case strings.HasPrefix(t, "INTERVAL YEAR"):
		c.Kind, c.PGType = kIntervalYM, "interval year to month"
	case strings.HasPrefix(t, "INTERVAL DAY"):
		c.Kind, c.PGType = kIntervalDS, "interval day to second"
	case t == "RAW" || t == "LONG RAW" || t == "BLOB":
		c.Kind, c.PGType = kBinary, "bytea"
	case t == "ROWID" || t == "UROWID":
		c.Kind, c.PGType, note = kRowid, "text", "a ROWID is stored as text and no longer points at a row"
	case t == "XMLTYPE":
		c.Kind, c.PGType = kXML, "xml"
	case t == "JSON":
		c.Kind, c.PGType = kJSON, "jsonb"
	case t == "BOOLEAN":
		c.Kind, c.PGType = kBool, "boolean"
	default:
		return "", false
	}
	return note, true
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

// BuildPlan reads the source catalog and translates it.
func BuildPlan(ctx context.Context, src oracle.Queryer, cfg migrate.Config) (*Plan, error) {
	var user string
	if err := src.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM dual`).Scan(&user); err != nil {
		return nil, oracle.Friendly(err)
	}
	owners, only, exclude := scopeOf(cfg, user)
	p := &Plan{Owners: owners, byName: map[string]*Table{}}
	quoted := make([]string, len(owners))
	for i, o := range owners {
		quoted[i] = literal(o)
	}
	in := "IN (" + strings.Join(quoted, ", ") + ")"

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
	// and overflow tables, materialized view containers, the recycle bin.
	rows, err := query(ctx, src, `SELECT t.owner, t.table_name, NVL(t.num_rows, 0) FROM all_tables t
		WHERE t.owner `+in+` AND t.nested = 'NO' AND t.secondary = 'N' AND t.dropped = 'NO' AND t.temporary = 'N'
		  AND (t.iot_type IS NULL OR t.iot_type = 'IOT') AND NVL(t.external, 'NO') = 'NO'
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
		t := &Table{Owner: r[0], Name: r[1], Schema: pgName(r[0]), Target: pgName(r[1])}
		fmt.Sscan(r[2], &t.EstRows)
		p.Tables = append(p.Tables, t)
		p.byName[r[0]+"."+r[1]] = t
	}
	if len(only) > 0 {
		var missing []string
		for name := range only {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("these tables do not exist on the source or cannot be read by this account: %s", strings.Join(missing, ", "))
	}
	if len(p.Tables) == 0 {
		return nil, fmt.Errorf("there are no tables to migrate in %s; name the schemas to migrate if they are not the connecting user's own", strings.Join(owners, ", "))
	}

	if err := p.loadColumns(ctx, src, in); err != nil {
		return nil, err
	}
	if err := p.loadKeys(ctx, src, in); err != nil {
		return nil, err
	}
	// Sequences are carried over for whole schemas; a table-level
	// migration takes only what its identity columns need.
	if only == nil {
		if err := p.loadSequences(ctx, src, in); err != nil {
			return nil, err
		}
	}
	// Foreign keys are read before the tables are written out, because
	// they can change a column's type.
	if err := p.loadForeignKeys(ctx, src, in); err != nil {
		return nil, err
	}
	p.buildTables()
	if err := p.loadIndexes(ctx, src, in); err != nil {
		return nil, err
	}
	if err := p.loadChecks(ctx, src, in); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Plan) loadColumns(ctx context.Context, src oracle.Queryer, in string) error {
	// ALL_TAB_COLS also lists the hidden columns Oracle adds for itself.
	rows, err := query(ctx, src, `SELECT c.owner, c.table_name, c.column_name, c.data_type, c.data_type_owner, c.data_precision, c.data_scale,
			c.char_length, c.nullable, c.virtual_column, c.data_default,
			(SELECT MAX(i.sequence_name) FROM all_tab_identity_cols i WHERE i.owner = c.owner AND i.table_name = c.table_name AND i.column_name = c.column_name)
		FROM all_tab_cols c WHERE c.owner `+in+` AND c.hidden_column = 'NO' ORDER BY c.owner, c.table_name, c.column_id`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		t := p.Table(r[0], r[1])
		if t == nil {
			continue
		}
		c := Column{Name: r[2], Target: pgName(r[2]), DataType: r[3], Precision: nullInt(r[5]), Scale: nullInt(r[6]),
			Nullable: r[8] == "Y", Virtual: r[9] == "YES", Default: strings.TrimSpace(r[10]), IdentitySeq: r[11]}
		fmt.Sscan(r[7], &c.CharLength)
		// A type with an owner other than Oracle's own is user-defined.
		userType := r[4] != "" && r[4] != "SYS" && r[4] != "PUBLIC"
		note, ok := translate(&c)
		if !ok || userType {
			p.Unsupported = append(p.Unsupported, fmt.Sprintf("%s.%s (%s)", t.Label(), c.Name, c.DataType))
			c.Kind, c.PGType = kText, "text"
		}
		// PostgreSQL only makes identity columns of its integer types. A
		// wider NUMBER behind one still only ever holds whole numbers,
		// and a bigint outlasts any sequence.
		if c.IdentitySeq != "" && c.PGType != "smallint" && c.PGType != "integer" {
			c.PGType = "bigint"
		}
		if note != "" {
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: %s", t.Label(), c.Name, note))
		}
		t.Cols = append(t.Cols, c)
	}
	return nil
}

func (p *Plan) loadKeys(ctx context.Context, src oracle.Queryer, in string) error {
	rows, err := query(ctx, src, `SELECT k.owner, k.table_name, c.column_name FROM all_constraints k
		JOIN all_cons_columns c ON c.owner = k.owner AND c.constraint_name = k.constraint_name
		WHERE k.owner `+in+` AND k.constraint_type = 'P' AND k.status = 'ENABLED' ORDER BY 1, 2, c.position`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		t := p.Table(r[0], r[1])
		if t == nil {
			continue
		}
		for i, c := range t.Cols {
			if c.Name == r[2] {
				t.PK = append(t.PK, i)
			}
		}
	}
	return nil
}

// bigintMax is the largest value a PostgreSQL sequence can reach.
const bigintMax = "9223372036854775807"

func (p *Plan) loadSequences(ctx context.Context, src oracle.Queryer, in string) error {
	// Sequences named ISEQ$$ belong to identity columns.
	rows, err := query(ctx, src, `SELECT sequence_owner, sequence_name, TO_CHAR(min_value), TO_CHAR(max_value), TO_CHAR(increment_by),
			cycle_flag, TO_CHAR(cache_size), TO_CHAR(last_number)
		FROM all_sequences WHERE sequence_owner `+in+` AND sequence_name NOT LIKE 'ISEQ$$%' ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		s := Sequence{Owner: r[0], Name: r[1], Schema: pgName(r[0]), Target: pgName(r[1])}
		// Oracle's default bounds are wider than a bigint; anything out
		// of range becomes "no limit".
		bound := func(word, v string) string {
			digits := strings.TrimPrefix(v, "-")
			if len(digits) > len(bigintMax) || (len(digits) == len(bigintMax) && digits > bigintMax) {
				return " NO " + word
			}
			return " " + word + " " + v
		}
		ddl := "CREATE SEQUENCE " + pgx.Identifier{s.Schema, s.Target}.Sanitize() + " INCREMENT BY " + r[4] +
			bound("MINVALUE", r[2]) + bound("MAXVALUE", r[3]) + " START WITH " + r[7]
		if r[6] != "" && r[6] != "0" {
			ddl += " CACHE " + r[6]
		}
		if r[5] == "Y" {
			ddl += " CYCLE"
		}
		p.Sequences = append(p.Sequences, s)
		p.Pre = append(p.Pre, Stmt{Kind: "sequence", Name: s.Schema + "." + s.Target, SQL: ddl, Soft: true})
	}
	return nil
}

// buildTables writes the CREATE TABLE statements. Schemas come first and
// tables after the sequences their defaults may call.
func (p *Plan) buildTables() {
	var schemas []Stmt
	seen := map[string]bool{}
	for _, t := range p.Tables {
		if !seen[t.Schema] {
			seen[t.Schema] = true
			schemas = append(schemas, Stmt{Kind: "schema", Name: t.Schema, SQL: "CREATE SCHEMA IF NOT EXISTS " + qident(t.Schema)})
		}
	}
	p.Pre = append(schemas, p.Pre...)

	for _, t := range p.Tables {
		var defs, pk []string
		for _, c := range t.Cols {
			def := qident(c.Target) + " " + c.PGType
			switch {
			case c.IdentitySeq != "":
				// BY DEFAULT, so the copy can store the source's values.
				def += " GENERATED BY DEFAULT AS IDENTITY"
			case c.Virtual:
				p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the virtual column becomes an ordinary column holding the same values", t.Label(), c.Name))
			case c.Default != "":
				if d, ok := p.columnDefault(c); ok {
					if d != "" {
						def += " DEFAULT " + d
					}
				} else {
					p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the default (%s) was not translated", t.Label(), c.Name, c.Default))
				}
			}
			if !c.Nullable {
				def += " NOT NULL"
			}
			defs = append(defs, def)
		}
		for _, i := range t.PK {
			pk = append(pk, qident(t.Cols[i].Target))
		}
		if len(pk) > 0 {
			defs = append(defs, "PRIMARY KEY ("+strings.Join(pk, ", ")+")")
		}
		p.Pre = append(p.Pre, Stmt{Kind: "table", Name: t.Schema + "." + t.Target,
			SQL: "CREATE TABLE " + t.QName() + " (\n  " + strings.Join(defs, ",\n  ") + "\n)"})
	}
}
