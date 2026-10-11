package pgmy

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// Stmt is one DDL statement to run on the target.
type Stmt struct {
	Kind string
	Name string
	SQL  string
	Soft bool
	err  error
}

// Column is a source column with its translation.
type Column struct {
	Name string
	// Kind names the conversion a value needs on its way to MySQL:
	// "bool", "bytea", "timestamptz", "timetz", "array", "numarray" or ""
	// for values that are passed through as text.
	Kind  string
	Type  string // MySQL column type
	Auto  bool   // becomes AUTO_INCREMENT
	IsKey bool   // integer, text or uuid: comparable across the two engines
	JSON  bool
}

// Table is a source table with its name and columns on the target.
type Table struct {
	pgpg.Table
	Target  string
	Columns []Column
	byName  map[string]*Column
	PK      []string
}

// QName returns the quoted name of the table on the target.
func (t *Table) QName() string { return mysql.Quote(t.Target) }

// Plan is the source plan together with its translation for MySQL.
type Plan struct {
	*pgpg.Plan
	Tables []*Table
	// bySource finds a table by its PostgreSQL schema and name.
	bySource map[string]*Table
	Pre      []Stmt // tables
	Post     []Stmt // indexes and foreign keys
	// Notes list everything that was translated with a change in meaning
	// or could not be carried over.
	Notes []string
	// Collisions lists target names claimed by more than one source table.
	Collisions []string
}

func (p *Plan) table(schema, name string) *Table { return p.bySource[schema+"."+name] }

var (
	sizeRe    = regexp.MustCompile(`\(([0-9, ]+)\)`)
	numericRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
	castRe    = regexp.MustCompile(`^'((?:[^']|'')*)'::[a-zA-Z_ ."\[\]()0-9]+$`)
)

var numericArrays = map[string]bool{"_int2": true, "_int4": true, "_int8": true, "_numeric": true, "_float4": true, "_float8": true, "_bool": true}

// mysqlType maps a PostgreSQL type to a MySQL type that can hold every value
// of it. keyed says the column is part of a key or index, which MySQL cannot
// build on an unbounded text column. It returns the conversion kind as well.
func mysqlType(typname, typtype, formatted string, keyed bool) (typ, kind string) {
	n := ""
	if m := sizeRe.FindStringSubmatch(formatted); m != nil {
		n = strings.ReplaceAll(m[1], " ", "")
	}
	text := func() string {
		if keyed {
			return "VARCHAR(255)"
		}
		return "LONGTEXT"
	}
	switch {
	case typtype == "e":
		// Enum labels are kept as text; the list itself is added by the caller.
		return "ENUM", ""
	case strings.HasPrefix(typname, "_"):
		if numericArrays[typname] {
			return "JSON", "numarray"
		}
		return "JSON", "array"
	}
	switch typname {
	case "int2":
		return "SMALLINT", ""
	case "int4":
		return "INT", ""
	case "int8":
		return "BIGINT", ""
	case "oid":
		return "INT UNSIGNED", ""
	case "numeric":
		// MySQL's DECIMAL stops at 65 digits, 30 of them after the point.
		if n == "" {
			return "DECIMAL(65,20)", ""
		}
		return "DECIMAL(" + n + ")", ""
	case "float4":
		return "FLOAT", ""
	case "float8":
		return "DOUBLE", ""
	case "bool":
		return "TINYINT(1)", "bool"
	case "varchar":
		if n == "" {
			return text(), ""
		}
		return "VARCHAR(" + n + ")", ""
	case "bpchar":
		if n == "" {
			n = "1"
		}
		return "CHAR(" + n + ")", ""
	case "text", "name", "xml":
		return text(), ""
	case "bytea":
		if keyed {
			return "VARBINARY(255)", "bytea"
		}
		return "LONGBLOB", "bytea"
	case "date":
		return "DATE", ""
	case "timestamp":
		return "DATETIME(6)", ""
	case "timestamptz":
		// Stored in UTC. MySQL's own TIMESTAMP type ends in 2038.
		return "DATETIME(6)", "timestamptz"
	case "time":
		return "TIME(6)", ""
	case "timetz":
		return "TIME(6)", "timetz"
	case "uuid":
		return "CHAR(36)", ""
	case "json", "jsonb":
		return "JSON", ""
	case "interval", "inet", "cidr", "macaddr", "money":
		return "VARCHAR(64)", ""
	}
	return text(), ""
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// columnDefault translates a column default, or returns a note when it has
// no equivalent.
func columnDefault(table, column, def, typ string) (string, string) {
	if def == "" {
		return "", ""
	}
	// MySQL does not allow literal defaults on these types.
	switch typ {
	case "LONGTEXT", "LONGBLOB", "JSON":
		return "", fmt.Sprintf("%s.%s: the default was dropped, since MySQL does not allow one on %s columns", table, column, typ)
	}
	lower := strings.ToLower(def)
	switch {
	case numericRe.MatchString(def):
		return def, ""
	case lower == "true":
		return "1", ""
	case lower == "false":
		return "0", ""
	case lower == "now()" || strings.HasPrefix(lower, "current_timestamp") || lower == "transaction_timestamp()":
		if strings.HasPrefix(typ, "DATETIME") {
			return "CURRENT_TIMESTAMP(6)", ""
		}
	case castRe.MatchString(def):
		return literal(strings.ReplaceAll(castRe.FindStringSubmatch(def)[1], "''", "'")), ""
	}
	return "", fmt.Sprintf("%s.%s: the default (%s) was not translated", table, column, def)
}

// clip shortens a name to MySQL's 64-character identifier limit.
func clip(name string) string {
	if len(name) > 64 {
		return name[:64]
	}
	return name
}

type index struct {
	name    string
	unique  bool
	cols    []string
	primary bool
}

type foreignKey struct {
	name, parentSchema, parentTable string
	cols, parentCols                []string
	onUpdate, onDelete              string
}

var fkActions = map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "NO ACTION"}

// BuildPlan reads the source catalog and translates it. The connection's
// search_path is cleared by the PostgreSQL planner, so type names in the
// catalog come back schema-qualified.
func BuildPlan(ctx context.Context, src *pgx.Conn, cfg migrate.Config) (*Plan, error) {
	base, err := pgpg.BuildPlan(ctx, src, cfg)
	if err != nil {
		return nil, err
	}
	p := &Plan{Plan: base, bySource: map[string]*Table{}}
	oids := make([]uint32, len(base.Tables))
	byOID := map[uint32]*Table{}
	schemas := map[string]bool{}
	for _, t := range base.Tables {
		schemas[t.Schema] = true
	}
	claimed := map[string]string{}
	for i, t := range base.Tables {
		oids[i] = t.OID
		// With one schema the table keeps its name. With several, tables
		// outside "public" carry their schema as a prefix.
		target := t.Name
		if len(schemas) > 1 && t.Schema != "public" {
			target = t.Schema + "_" + t.Name
		}
		target = clip(target)
		if other, taken := claimed[strings.ToLower(target)]; taken {
			p.Collisions = append(p.Collisions, fmt.Sprintf("%s.%s and %s both become %s", t.Schema, t.Name, other, target))
		}
		claimed[strings.ToLower(target)] = t.Schema + "." + t.Name
		table := &Table{Table: t, Target: target, byName: map[string]*Column{}}
		p.Tables = append(p.Tables, table)
		p.bySource[t.Schema+"."+t.Name] = table
		byOID[t.OID] = table
	}

	indexes, err := loadIndexes(ctx, src, base.VersionNum, oids, byOID, p)
	if err != nil {
		return nil, err
	}
	fks, err := loadForeignKeys(ctx, src, oids)
	if err != nil {
		return nil, err
	}
	// Columns that take part in a key, an index or a foreign key need a
	// bounded type in MySQL.
	keyed := map[uint32]map[string]bool{}
	mark := func(oid uint32, cols []string) {
		if keyed[oid] == nil {
			keyed[oid] = map[string]bool{}
		}
		for _, c := range cols {
			keyed[oid][c] = true
		}
	}
	for oid, list := range indexes {
		for _, ix := range list {
			mark(oid, ix.cols)
		}
	}
	for oid, list := range fks {
		for _, fk := range list {
			mark(oid, fk.cols)
			if parent := p.table(fk.parentSchema, fk.parentTable); parent != nil {
				mark(parent.OID, fk.parentCols)
			}
		}
	}

	if err := p.loadColumns(ctx, src, oids, byOID, keyed); err != nil {
		return nil, err
	}
	p.buildDDL(indexes, fks)
	return p, nil
}

func loadIndexes(ctx context.Context, src *pgx.Conn, version int, oids []uint32, byOID map[uint32]*Table, p *Plan) (map[uint32][]index, error) {
	keyAtts := "i.indnkeyatts"
	if version < 110000 {
		keyAtts = "i.indnatts"
	}
	rows, err := src.Query(ctx, `SELECT i.indrelid, ic.relname, i.indisprimary, i.indisunique, i.indpred IS NOT NULL, i.indexprs IS NOT NULL, am.amname,
			coalesce((SELECT string_agg(a.attname, E'\n' ORDER BY k.ord)
				FROM unnest(i.indkey) WITH ORDINALITY k(attnum, ord)
				JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
				WHERE k.ord <= `+keyAtts+`), '')
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_catalog.pg_am am ON am.oid = ic.relam
		WHERE i.indrelid = ANY($1) ORDER BY 1, 2`, oids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32][]index{}
	for rows.Next() {
		var oid uint32
		var ix index
		var partial, expression bool
		var method, cols string
		if err := rows.Scan(&oid, &ix.name, &ix.primary, &ix.unique, &partial, &expression, &method, &cols); err != nil {
			return nil, err
		}
		t := byOID[oid]
		switch {
		case partial:
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the partial index %s was not translated", t.Schema, t.Name, ix.name))
			continue
		case expression:
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the expression index %s was not translated", t.Schema, t.Name, ix.name))
			continue
		case method != "btree" && method != "hash":
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the %s index %s was not translated", t.Schema, t.Name, method, ix.name))
			continue
		}
		ix.cols = strings.Split(cols, "\n")
		out[oid] = append(out[oid], ix)
	}
	return out, rows.Err()
}

func loadForeignKeys(ctx context.Context, src *pgx.Conn, oids []uint32) (map[uint32][]foreignKey, error) {
	const columns = `(SELECT string_agg(a.attname, E'\n' ORDER BY k.ord) FROM unnest(%s) WITH ORDINALITY k(attnum, ord)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = %s AND a.attnum = k.attnum)`
	rows, err := src.Query(ctx, `SELECT c.conrelid, c.conname, pn.nspname, pr.relname, c.confupdtype::text, c.confdeltype::text,
			`+fmt.Sprintf(columns, "c.conkey", "c.conrelid")+`, `+fmt.Sprintf(columns, "c.confkey", "c.confrelid")+`
		FROM pg_catalog.pg_constraint c
		JOIN pg_catalog.pg_class pr ON pr.oid = c.confrelid JOIN pg_catalog.pg_namespace pn ON pn.oid = pr.relnamespace
		WHERE c.contype = 'f' AND c.conrelid = ANY($1) ORDER BY 1, 2`, oids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32][]foreignKey{}
	for rows.Next() {
		var oid uint32
		var fk foreignKey
		var cols, parentCols string
		if err := rows.Scan(&oid, &fk.name, &fk.parentSchema, &fk.parentTable, &fk.onUpdate, &fk.onDelete, &cols, &parentCols); err != nil {
			return nil, err
		}
		fk.cols, fk.parentCols = strings.Split(cols, "\n"), strings.Split(parentCols, "\n")
		out[oid] = append(out[oid], fk)
	}
	return out, rows.Err()
}

// loadColumns reads every column and decides its MySQL type. Generated
// columns are left out: PostgreSQL does not replicate their values, so a
// copy of them on the target would go stale.
func (p *Plan) loadColumns(ctx context.Context, src *pgx.Conn, oids []uint32, byOID map[uint32]*Table, keyed map[uint32]map[string]bool) error {
	generated := "''"
	if p.VersionNum >= 120000 {
		generated = "a.attgenerated::text"
	}
	rows, err := src.Query(ctx, `SELECT a.attrelid, a.attname, bt.typname, bt.typtype::text, pg_catalog.format_type(a.atttypid, a.atttypmod),
			a.attnotnull, coalesce(pg_catalog.pg_get_expr(d.adbin, d.adrelid), ''), a.attidentity::text, `+generated+`,
			coalesce((SELECT string_agg(e.enumlabel, E'\n' ORDER BY e.enumsortorder) FROM pg_catalog.pg_enum e WHERE e.enumtypid = bt.oid), '')
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
		-- A domain is translated as the type it is built on.
		JOIN pg_catalog.pg_type bt ON bt.oid = CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.oid END
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = ANY($1) AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attrelid, a.attnum`, oids)
	if err != nil {
		return err
	}
	defer rows.Close()
	defs := map[uint32][]string{}
	for rows.Next() {
		var oid uint32
		var name, typname, typtype, formatted, def, identity, gen, enumLabels string
		var notNull bool
		if err := rows.Scan(&oid, &name, &typname, &typtype, &formatted, &notNull, &def, &identity, &gen, &enumLabels); err != nil {
			return err
		}
		t := byOID[oid]
		label := t.Schema + "." + t.Name
		if gen != "" {
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the generated column was not migrated; its values are not part of the replication stream", label, name))
			continue
		}
		isKeyed := keyed[oid][name]
		typ, kind := mysqlType(typname, typtype, formatted, isKeyed)
		col := Column{Name: name, Kind: kind, Type: typ, JSON: typ == "JSON"}
		switch typname {
		case "int2", "int4", "int8", "text", "varchar", "bpchar", "uuid":
			col.IsKey = true
		}
		if typ == "ENUM" {
			var quoted []string
			for _, l := range strings.Split(enumLabels, "\n") {
				quoted = append(quoted, literal(l))
			}
			typ = "ENUM(" + strings.Join(quoted, ", ") + ")"
			col.Type = typ
		}
		switch {
		case isKeyed && typ == "VARCHAR(255)":
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: unbounded text in a key or index becomes VARCHAR(255); a longer value will stop the migration", label, name))
		case kind == "array" || kind == "numarray":
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the array becomes a JSON array", label, name))
		case kind == "timestamptz":
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: timestamp with time zone becomes DATETIME(6) holding UTC", label, name))
		}

		line := mysql.Quote(name) + " " + typ
		serial := identity != "" || strings.HasPrefix(def, "nextval(")
		if !notNull {
			line += " NULL"
		} else {
			line += " NOT NULL"
		}
		switch {
		case serial && strings.Contains(typ, "INT") && !t.hasAuto():
			col.Auto = true
			line += " AUTO_INCREMENT"
		case serial:
			p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the sequence default was not translated; MySQL allows one AUTO_INCREMENT column per table", label, name))
		default:
			if d, note := columnDefault(label, name, def, typ); d != "" {
				line += " DEFAULT " + d
			} else if note != "" {
				p.Notes = append(p.Notes, note)
			}
		}
		defs[oid] = append(defs[oid], line)
		t.Columns = append(t.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range p.Tables {
		for i := range t.Columns {
			t.byName[t.Columns[i].Name] = &t.Columns[i]
		}
		t.Table.Cols = nil
		for _, c := range t.Columns {
			t.Table.Cols = append(t.Table.Cols, c.Name)
		}
		p.Pre = append(p.Pre, Stmt{Kind: "table", Name: t.Target, SQL: strings.Join(defs[t.OID], ",\n  ")})
	}
	return nil
}

func (t *Table) hasAuto() bool {
	for _, c := range t.Columns {
		if c.Auto {
			return true
		}
	}
	return false
}

func quoteAll(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = mysql.Quote(c)
	}
	return strings.Join(q, ", ")
}

// buildDDL completes the CREATE TABLE statements with their primary keys and
// produces the statements for secondary indexes and foreign keys.
func (p *Plan) buildDDL(indexes map[uint32][]index, fks map[uint32][]foreignKey) {
	for i, t := range p.Tables {
		body := p.Pre[i].SQL
		leading := map[string]bool{}
		for _, ix := range indexes[t.OID] {
			if !t.has(ix.cols) {
				continue
			}
			leading[ix.cols[0]] = true
			if ix.primary {
				t.PK = ix.cols
				body += ",\n  PRIMARY KEY (" + quoteAll(ix.cols) + ")"
				continue
			}
			unique := ""
			if ix.unique {
				unique = "UNIQUE "
			}
			p.Post = append(p.Post, Stmt{Kind: "index", Name: t.Target + "." + ix.name, Soft: true,
				SQL: "CREATE " + unique + "INDEX " + mysql.Quote(clip(ix.name)) + " ON " + t.QName() + " (" + quoteAll(ix.cols) + ")"})
		}
		// MySQL requires an AUTO_INCREMENT column to lead some index.
		for _, c := range t.Columns {
			if c.Auto && !leading[c.Name] {
				body += ",\n  KEY (" + mysql.Quote(c.Name) + ")"
			}
		}
		p.Pre[i].SQL = "CREATE TABLE " + t.QName() + " (\n  " + body + "\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"

		for _, fk := range fks[t.OID] {
			parent := p.table(fk.parentSchema, fk.parentTable)
			if parent == nil {
				p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: foreign key %s refers to %s.%s, which is not being migrated, and was left out",
					t.Schema, t.Name, fk.name, fk.parentSchema, fk.parentTable))
				continue
			}
			if !t.has(fk.cols) || !parent.has(fk.parentCols) {
				continue
			}
			p.Post = append(p.Post, Stmt{Kind: "foreign key", Name: t.Target + "." + fk.name, Soft: true,
				SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s) ON UPDATE %s ON DELETE %s",
					t.QName(), mysql.Quote(clip(t.Target+"_"+fk.name)), quoteAll(fk.cols), parent.QName(), quoteAll(fk.parentCols),
					fkActions[fk.onUpdate], fkActions[fk.onDelete])})
		}
	}
}

// has reports whether every named column was migrated.
func (t *Table) has(cols []string) bool {
	for _, c := range cols {
		if t.byName[c] == nil {
			return false
		}
	}
	return len(cols) > 0
}
