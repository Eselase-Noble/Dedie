package orapg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
	"dedie/engine/migrate/pgpg"
)

// Column is a source column with its translation.
type Column struct {
	orasrc.Column
	Target string // its name on the target
	PGType string
}

// Table is a source table and where it goes.
type Table struct {
	*orasrc.Table
	Schema, Target string
	// Cols are the source table's columns, in the same order, each with
	// its translation. They take the place of the embedded table's own.
	Cols []Column
}

// QName is the table's quoted name in PostgreSQL.
func (t *Table) QName() string { return pgx.Identifier{t.Schema, t.Target}.Sanitize() }

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
	// ident is the quoted name of a sequence or index the statement
	// creates, for the preflight to look up on the target; see claims.
	ident string
}

// Plan is what will be migrated and how it is translated.
type Plan struct {
	// Catalog is the source as it describes itself; everything else here
	// is derived from it.
	Catalog *orasrc.Catalog
	Tables  []*Table
	// Pre creates schemas, sequences and tables; Post builds indexes and
	// check constraints once the data is in; Final adds foreign keys.
	// Foreign keys wait until the end because rows are applied table by
	// table while changes are followed, not in the order the source
	// wrote them.
	Pre, Post, Final []Stmt
	Sequences        []Sequence
	// Notes list everything that was translated with a change in meaning
	// or could not be carried over. Preflight shows them to the operator.
	Notes  []string
	byName map[string]*Table
	bySrc  map[*orasrc.Table]*Table
}

// Table finds a table by its Oracle owner and name.
func (p *Plan) Table(owner, name string) *Table { return p.byName[owner+"."+name] }

// of finds the translation of a table of the catalog.
func (p *Plan) of(t *orasrc.Table) *Table { return p.bySrc[t] }

// translate picks the PostgreSQL type for a column. It returns a note
// when the translation changes meaning, and false when the type cannot
// be migrated.
func translate(c orasrc.Column) (pgType, note string, ok bool) {
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
		pgType = "text"
		if c.CharLength > 0 {
			pgType = fmt.Sprintf("varchar(%d)", c.CharLength)
		}
	case t == "CHAR" || t == "NCHAR":
		pgType = fmt.Sprintf("char(%d)", max(c.CharLength, 1))
	case t == "CLOB" || t == "NCLOB" || t == "LONG":
		pgType = "text"
	case t == "NUMBER":
		p, s := c.Precision, c.Scale
		switch {
		case !p.Valid:
			// NUMBER without a precision holds any number.
			pgType = "numeric"
		case s.Int64 == 0 && p.Int64 <= 4:
			pgType = "smallint"
		case s.Int64 == 0 && p.Int64 <= 9:
			pgType = "integer"
		case s.Int64 == 0 && p.Int64 <= 18:
			pgType = "bigint"
		case s.Int64 < 0 || s.Int64 > p.Int64:
			pgType = "numeric"
		default:
			pgType = fmt.Sprintf("numeric(%d,%d)", p.Int64, s.Int64)
		}
	case t == "FLOAT":
		// FLOAT is a decimal number with a binary precision; up to 53
		// bits fits a double.
		pgType = "numeric"
		if c.Precision.Valid && c.Precision.Int64 <= 53 {
			pgType = "double precision"
		}
	case t == "BINARY_FLOAT":
		pgType = "real"
	case t == "BINARY_DOUBLE":
		pgType = "double precision"
	case t == "DATE":
		// An Oracle DATE carries a time of day.
		pgType = "timestamp(0)"
	case strings.HasPrefix(t, "TIMESTAMP") && strings.HasSuffix(t, "WITH LOCAL TIME ZONE"):
		pgType, note = "timestamptz"+fraction(), lossy
	case strings.HasPrefix(t, "TIMESTAMP") && strings.HasSuffix(t, "WITH TIME ZONE"):
		pgType, note = "timestamptz"+fraction(), lossy
		if note == "" {
			note = "TIMESTAMP WITH TIME ZONE keeps the zone each value was entered in; PostgreSQL keeps the instant only"
		}
	case strings.HasPrefix(t, "TIMESTAMP"):
		pgType, note = "timestamp"+fraction(), lossy
	case strings.HasPrefix(t, "INTERVAL YEAR"):
		pgType = "interval year to month"
	case strings.HasPrefix(t, "INTERVAL DAY"):
		pgType = "interval day to second"
	case t == "RAW" || t == "LONG RAW" || t == "BLOB":
		pgType = "bytea"
	case t == "ROWID" || t == "UROWID":
		pgType, note = "text", "a ROWID is stored as text and no longer points at a row"
	case t == "XMLTYPE":
		pgType = "xml"
	case t == "JSON":
		pgType = "jsonb"
	case t == "BOOLEAN":
		pgType = "boolean"
	default:
		return "", "", false
	}
	return pgType, note, true
}

// BuildPlan reads the source catalog and translates it.
func BuildPlan(ctx context.Context, src oracle.Queryer, cfg migrate.Config) (*Plan, error) {
	cat, err := orasrc.LoadCatalog(ctx, src, cfg)
	if err != nil {
		return nil, err
	}
	p := &Plan{Catalog: cat, byName: map[string]*Table{}, bySrc: map[*orasrc.Table]*Table{}}
	for _, st := range cat.Tables {
		t := &Table{Table: st, Schema: pgName(st.Owner), Target: pgName(st.Name)}
		for _, sc := range st.Cols {
			c := Column{Column: sc, Target: pgName(sc.Name)}
			var note string
			var ok bool
			if c.PGType, note, ok = translate(sc); !ok || sc.Unsupported {
				c.PGType = "text"
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
		p.Tables = append(p.Tables, t)
		p.byName[t.Label()] = t
		p.bySrc[st] = t
	}
	p.sequences()
	// Foreign keys are translated before the tables are written out,
	// because they can change a column's type.
	p.foreignKeys()
	p.buildTables()
	p.indexes()
	p.checks()
	return p, nil
}

// bigintMax is the largest value a PostgreSQL sequence can reach.
const bigintMax = "9223372036854775807"

// sequences writes the CREATE SEQUENCE statements.
func (p *Plan) sequences() {
	for _, src := range p.Catalog.Sequences {
		s := Sequence{Owner: src.Owner, Name: src.Name, Schema: pgName(src.Owner), Target: pgName(src.Name)}
		// Oracle's default bounds are wider than a bigint; anything out
		// of range becomes "no limit".
		bound := func(word, v string) string {
			digits := strings.TrimPrefix(v, "-")
			if len(digits) > len(bigintMax) || (len(digits) == len(bigintMax) && digits > bigintMax) {
				return " NO " + word
			}
			return " " + word + " " + v
		}
		ddl := "CREATE SEQUENCE " + pgx.Identifier{s.Schema, s.Target}.Sanitize() + " INCREMENT BY " + src.Increment +
			bound("MINVALUE", src.Min) + bound("MAXVALUE", src.Max) + " START WITH " + src.Last
		if src.Cache != "" && src.Cache != "0" {
			ddl += " CACHE " + src.Cache
		}
		if src.Cycle {
			ddl += " CYCLE"
		}
		p.Sequences = append(p.Sequences, s)
		p.Pre = append(p.Pre, Stmt{Kind: "sequence", Name: s.Schema + "." + s.Target, SQL: ddl, Soft: true,
			ident: pgx.Identifier{s.Schema, s.Target}.Sanitize()})
	}
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

// claims lists every name the plan takes on the target, for the preflight
// to look up there. Apart from the tables these are the sequences and the
// indexes, read off the statements that create them, so what is checked
// and what is created cannot drift apart. Check constraints and foreign
// keys are named within their table, and a primary key's index gets a free
// name from PostgreSQL.
func (p *Plan) claims() []pgpg.Claim {
	var out []pgpg.Claim
	for _, t := range p.Tables {
		out = append(out, pgpg.Claim{Kind: "table", Name: t.Schema + "." + t.Target, Ident: t.QName()})
	}
	for _, list := range [][]Stmt{p.Pre, p.Post, p.Final} {
		for _, s := range list {
			if s.ident != "" {
				out = append(out, pgpg.Claim{Kind: s.Kind, Name: s.Name, Ident: s.ident})
			}
		}
	}
	return out
}

// keep removes from the plan every statement that would create an object
// the job was told to leave as it is on the target, and takes a kept
// sequence off the list of those whose position is set at the end.
func (p *Plan) keep(kept []string) {
	if len(kept) == 0 {
		return
	}
	is := map[string]bool{}
	for _, k := range kept {
		is[k] = true
	}
	for _, list := range []*[]Stmt{&p.Pre, &p.Post, &p.Final} {
		out := (*list)[:0:0]
		for _, s := range *list {
			if s.ident == "" || !is[pgpg.KeptKey(s.Kind, s.Name)] {
				out = append(out, s)
			}
		}
		*list = out
	}
	sequences := p.Sequences[:0:0]
	for _, s := range p.Sequences {
		if !is[pgpg.KeptKey("sequence", s.Schema+"."+s.Target)] {
			sequences = append(sequences, s)
		}
	}
	p.Sequences = sequences
}

// sources translates the names tables have on the target into the names
// the job's table selection knows them by on the source.
func (p *Plan) sources(targets []string) []string {
	var out []string
	for _, target := range targets {
		for _, t := range p.Tables {
			if t.Schema+"."+t.Target == target {
				out = append(out, t.Label())
			}
		}
	}
	return out
}
