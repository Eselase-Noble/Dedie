package mypg

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"dedie/engine/migrate"
	"dedie/engine/migrate/mymy"
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

// Plan is the source plan together with its translation for PostgreSQL.
type Plan struct {
	*mymy.Plan
	Pre  []Stmt // tables
	Post []Stmt // indexes and foreign keys
	// Notes list everything that was translated with a change in meaning
	// or could not be carried over. Preflight shows them to the operator.
	Notes []string
	// enums holds, per table and column position, the labels of ENUM and
	// SET columns, which the binary log refers to by number.
	enums map[string]map[int][]string
}

var (
	sizeRe    = regexp.MustCompile(`\(([^)]*)\)`)
	numericRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
)

// supportedCharsets are the text encodings the pipeline can convert to the
// UTF-8 that PostgreSQL stores.
var supportedCharsets = map[string]bool{"": true, "utf8mb4": true, "utf8": true, "utf8mb3": true, "latin1": true, "ascii": true, "binary": true}

// size returns the text inside the parentheses of a column type, such as
// "12,2" for "decimal(12,2)".
func size(columnType string) string {
	if m := sizeRe.FindStringSubmatch(columnType); m != nil {
		return m[1]
	}
	return ""
}

// labels parses the value list of an ENUM or SET column type.
func labels(columnType string) []string {
	list := size(columnType)
	var out []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(list); i++ {
		ch := list[i]
		switch {
		case !in && ch == '\'':
			in = true
		case in && ch == '\'' && i+1 < len(list) && list[i+1] == '\'':
			cur.WriteByte('\'')
			i++
		case in && ch == '\'':
			in = false
			out = append(out, cur.String())
			cur.Reset()
		case in:
			cur.WriteByte(ch)
		}
	}
	return out
}

// pgType maps a MySQL column to a PostgreSQL type that can hold every value
// the source column can. Where the two differ in range or meaning, the wider
// or more general type is chosen.
func pgType(c mymy.Column) string {
	n := size(c.ColumnType)
	withSize := func(base string) string {
		if n != "" {
			return base + "(" + n + ")"
		}
		return base
	}
	switch c.DataType {
	case "tinyint":
		return "smallint"
	case "smallint":
		if c.Unsigned {
			return "integer"
		}
		return "smallint"
	case "mediumint":
		return "integer"
	case "int", "integer":
		if c.Unsigned {
			return "bigint"
		}
		return "integer"
	case "bigint":
		if c.Unsigned {
			return "numeric(20,0)"
		}
		return "bigint"
	case "decimal", "numeric":
		return withSize("numeric")
	case "float":
		return "real"
	case "double", "real":
		return "double precision"
	case "bit":
		return "bigint"
	case "year":
		return "smallint"
	case "char", "varchar":
		// CHAR pads with spaces in PostgreSQL but not in MySQL's output.
		return withSize("varchar")
	case "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		return "text"
	case "json":
		return "jsonb"
	case "date":
		return "date"
	case "datetime":
		return withSize("timestamp")
	case "timestamp":
		// A MySQL TIMESTAMP is a point in time, stored in UTC.
		return withSize("timestamptz")
	case "time":
		return withSize("time")
	}
	// Binary strings, BLOBs and spatial values.
	return "bytea"
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// columnDefault translates a column default. It returns an empty string, and
// a note when something was dropped, if the default has no direct equivalent.
func columnDefault(table string, c mymy.Column) (string, string) {
	if c.Default == nil || c.AutoIncrement {
		return "", ""
	}
	def := *c.Default
	if strings.HasPrefix(strings.ToLower(def), "current_timestamp") {
		return "CURRENT_TIMESTAMP", ""
	}
	if strings.Contains(c.Extra, "default_generated") {
		return "", fmt.Sprintf("%s.%s: the expression default (%s) was not translated", table, c.Name, def)
	}
	switch c.DataType {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "float", "double", "real", "year":
		if numericRe.MatchString(def) {
			return def, ""
		}
		return "", ""
	case "bit", "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return "", fmt.Sprintf("%s.%s: the default of this binary column was not translated", table, c.Name)
	case "date", "datetime", "timestamp":
		if strings.HasPrefix(def, "0000-00-00") {
			return "", fmt.Sprintf("%s.%s: the zero-date default was dropped", table, c.Name)
		}
	}
	return literal(def), ""
}

// BuildPlan reads the source catalog and translates it.
func BuildPlan(ctx context.Context, src *sql.DB, database string, cfg migrate.Config) (*Plan, error) {
	base, err := mymy.BuildPlan(ctx, src, database, cfg)
	if err != nil {
		return nil, err
	}
	p := &Plan{Plan: base, enums: map[string]map[int][]string{}}

	for _, t := range base.Tables {
		var defs, pk []string
		p.enums[t.Name] = map[int][]string{}
		for i, c := range t.Cols {
			def := qident(c.Name) + " " + pgType(c)
			if c.AutoIncrement {
				def += " GENERATED BY DEFAULT AS IDENTITY"
			}
			if d, note := columnDefault(t.Name, c); d != "" {
				def += " DEFAULT " + d
			} else if note != "" {
				p.Notes = append(p.Notes, note)
			}
			if !c.Nullable {
				def += " NOT NULL"
			}
			defs = append(defs, def)

			switch {
			case c.DataType == "enum" || c.DataType == "set":
				values := labels(c.ColumnType)
				p.enums[t.Name][i] = values
				if c.DataType == "enum" {
					quoted := make([]string, 0, len(values)+1)
					for _, v := range values {
						quoted = append(quoted, literal(v))
					}
					// MySQL stores an empty string for an invalid value.
					quoted = append(quoted, "''")
					defs = append(defs, fmt.Sprintf("CONSTRAINT %s CHECK (%s IN (%s))",
						qident(clip(t.Name+"_"+c.Name+"_enum")), qident(c.Name), strings.Join(quoted, ", ")))
				}
			case c.Generated:
				p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: the generated column becomes an ordinary column holding the same values", t.Name, c.Name))
			case strings.Contains(c.Extra, "on update"):
				p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: ON UPDATE CURRENT_TIMESTAMP has no equivalent; add a trigger if the application relies on it", t.Name, c.Name))
			case !c.Binary && pgType(c) == "bytea":
				p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: %s is stored as raw bytes (bytea)", t.Name, c.Name, c.DataType))
			}
		}
		for _, i := range t.PK {
			pk = append(pk, qident(t.Cols[i].Name))
		}
		if len(pk) > 0 {
			defs = append(defs, "PRIMARY KEY ("+strings.Join(pk, ", ")+")")
		}
		p.Pre = append(p.Pre, Stmt{Kind: "table", Name: t.Name,
			SQL: "CREATE TABLE " + qname(t.Name) + " (\n  " + strings.Join(defs, ",\n  ") + "\n)"})
	}
	if err := p.loadIndexes(ctx, src); err != nil {
		return nil, err
	}
	if err := p.loadForeignKeys(ctx, src); err != nil {
		return nil, err
	}
	return p, nil
}

// clip shortens a name to PostgreSQL's 63-byte identifier limit.
func clip(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
}

// loadIndexes translates secondary indexes. Index names are unique per table
// in MySQL but per schema in PostgreSQL, so each is prefixed with its table.
func (p *Plan) loadIndexes(ctx context.Context, src *sql.DB) error {
	rows, err := mymy.QueryStrings(ctx, src, `SELECT table_name, index_name, non_unique, index_type,
			COALESCE(GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR '\n'), ''), SUM(column_name IS NULL), SUM(sub_part IS NOT NULL)
		FROM information_schema.statistics WHERE table_schema = ? AND index_name <> 'PRIMARY'
		GROUP BY table_name, index_name, non_unique, index_type ORDER BY 1, 2`, p.Database)
	if err != nil {
		return err
	}
	for _, r := range rows {
		table, index, nonUnique, kind, cols, expressions, prefixes := r[0], r[1], r[2], r[3], r[4], r[5], r[6]
		if p.Table(table) == nil {
			continue
		}
		switch {
		case kind == "FULLTEXT" || kind == "SPATIAL":
			p.Notes = append(p.Notes, fmt.Sprintf("%s: the %s index %s was not translated", table, kind, index))
			continue
		case expressions != "0":
			p.Notes = append(p.Notes, fmt.Sprintf("%s: the functional index %s was not translated", table, index))
			continue
		case prefixes != "0":
			p.Notes = append(p.Notes, fmt.Sprintf("%s: index %s covers a prefix of a column in MySQL and the whole column in PostgreSQL", table, index))
		}
		var quoted []string
		for _, c := range strings.Split(cols, "\n") {
			quoted = append(quoted, qident(c))
		}
		unique := ""
		if nonUnique == "0" {
			unique = "UNIQUE "
		}
		name := clip(table + "_" + index)
		p.Post = append(p.Post, Stmt{Kind: "index", Name: name, Soft: true,
			SQL: "CREATE " + unique + "INDEX " + qident(name) + " ON " + qname(table) + " (" + strings.Join(quoted, ", ") + ")"})
	}
	return nil
}

// loadForeignKeys translates foreign keys between migrated tables, keeping
// their ON UPDATE and ON DELETE rules. MySQL does not log the rows changed by
// a cascade, so the target must perform the same cascade itself.
func (p *Plan) loadForeignKeys(ctx context.Context, src *sql.DB) error {
	rows, err := mymy.QueryStrings(ctx, src, `SELECT k.constraint_name, k.table_name, k.referenced_table_name,
			GROUP_CONCAT(k.column_name ORDER BY k.ordinal_position SEPARATOR '\n'),
			GROUP_CONCAT(k.referenced_column_name ORDER BY k.ordinal_position SEPARATOR '\n'),
			MAX(r.update_rule), MAX(r.delete_rule)
		FROM information_schema.key_column_usage k
		JOIN information_schema.referential_constraints r
			ON r.constraint_schema = k.constraint_schema AND r.constraint_name = k.constraint_name AND r.table_name = k.table_name
		WHERE k.table_schema = ? AND k.referenced_table_schema = k.table_schema AND k.referenced_table_name IS NOT NULL
		GROUP BY k.constraint_name, k.table_name, k.referenced_table_name ORDER BY 2, 1`, p.Database)
	if err != nil {
		return err
	}
	quoteList := func(lines string) string {
		var out []string
		for _, c := range strings.Split(lines, "\n") {
			out = append(out, qident(c))
		}
		return strings.Join(out, ", ")
	}
	for _, r := range rows {
		name, table, parent := r[0], r[1], r[2]
		if p.Table(table) == nil {
			continue
		}
		if p.Table(parent) == nil {
			p.Notes = append(p.Notes, fmt.Sprintf("%s: foreign key %s refers to %s, which is not being migrated, and was left out", table, name, parent))
			continue
		}
		p.Post = append(p.Post, Stmt{Kind: "foreign key", Name: table + " " + name, Soft: true,
			SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s) ON UPDATE %s ON DELETE %s",
				qname(table), qident(clip(name)), quoteList(r[3]), qname(parent), quoteList(r[4]), r[5], r[6])})
	}
	return nil
}

// claims lists every name the plan takes in the target schema, for the
// preflight to look up there. Apart from the tables these are the indexes,
// read off the statements that create them, so what is checked and what is
// created cannot drift apart. Foreign keys are named within their table,
// and a primary key's index gets a free name from PostgreSQL.
func (p *Plan) claims() []pgpg.Claim {
	var out []pgpg.Claim
	for _, t := range p.Tables {
		out = append(out, pgpg.Claim{Kind: "table", Name: t.Name, Ident: qname(t.Name)})
	}
	for _, s := range p.Post {
		if s.Kind == "index" {
			out = append(out, pgpg.Claim{Kind: s.Kind, Name: s.Name, Ident: qname(s.Name)})
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
	out := p.Post[:0:0]
	for _, s := range p.Post {
		if s.Kind != "index" || !is[pgpg.KeptKey(s.Kind, s.Name)] {
			out = append(out, s)
		}
	}
	p.Post = out
}
