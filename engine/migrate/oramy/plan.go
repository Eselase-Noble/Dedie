package oramy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
)

// Limits of InnoDB that decide what a table can look like.
const (
	// maxRowBytes is the most the columns of a row may add up to, text
	// and blob columns counted by their pointers.
	maxRowBytes = 65535
	// maxKeyBytes is the longest an index key may be.
	maxKeyBytes = 3072
)

// Column is a source column with its translation.
type Column struct {
	orasrc.Column
	Target string // its name on the target
	Type   myType
	// Auto marks the identity column, which becomes AUTO_INCREMENT.
	Auto bool
}

// rowBytes is what the column adds to the size of a row.
func (c *Column) rowBytes() int {
	if c.Type.Var {
		return c.Type.Bytes + 2
	}
	return c.Type.Bytes
}

// Table is a source table and where it goes.
type Table struct {
	*orasrc.Table
	Target string
	// Cols are the source table's columns, in the same order, each with
	// its translation. They take the place of the embedded table's own.
	Cols []Column
}

// QName is the table's quoted name in MySQL.
func (t *Table) QName() string { return mysql.Quote(t.Target) }

// Schema names the table's Oracle schema the way progress is reported,
// which is how the other pipelines out of Oracle name it.
func (t *Table) Schema() string { return myName(t.Owner) }

// col finds a column by its Oracle name.
func (t *Table) col(name string) *Column {
	for i := range t.Cols {
		if t.Cols[i].Name == name {
			return &t.Cols[i]
		}
	}
	return nil
}

// Stmt is one DDL statement to run on the target.
type Stmt struct {
	Kind string
	Name string
	SQL  string
	Soft bool
	// constraint is the name a foreign key or check constraint takes.
	// MySQL names both per database rather than per table, so the name
	// may be taken by something the migration did not create.
	constraint string
}

// Plan is what will be migrated and how it is translated.
type Plan struct {
	// Catalog is the source as it describes itself; everything else here
	// is derived from it.
	Catalog *orasrc.Catalog
	Tables  []*Table
	// Pre creates the tables; Post builds indexes and check constraints
	// once the data is in; Final adds foreign keys. Foreign keys wait
	// until the end because rows are applied table by table while changes
	// are followed, not in the order the source wrote them.
	Pre, Post, Final []Stmt
	// Notes list everything that was translated with a change in meaning
	// or could not be carried over. Preflight shows them to the operator.
	Notes []string
	// Collisions lists the target names more than one source table would
	// get, and the tables whose columns differ only in case, which MySQL
	// does not tell apart. CaseCollisions lists the tables whose names
	// differ only in case; that matters on a server which folds names.
	Collisions, CaseCollisions []string
	// LongKeys lists the primary keys MySQL cannot build.
	LongKeys []string

	target features
	// kept holds the tables left out because the target already has them
	// and they are to stay as they are, by label.
	kept   map[string]bool
	byName map[string]*Table
	bySrc  map[*orasrc.Table]*Table
	// noted keeps the subjects of each note, and noteOrder the notes in
	// the order they first came up.
	noted     map[string][]string
	noteOrder []string
}

// Table finds a table by its Oracle owner and name.
func (p *Plan) Table(owner, name string) *Table { return p.byName[owner+"."+name] }

// of finds the translation of a table of the catalog.
func (p *Plan) of(t *orasrc.Table) *Table { return p.bySrc[t] }

// note records that something was translated with a change. Subjects
// that share a note are listed together, so that a schema with a hundred
// columns of one kind yields one line and not a hundred.
func (p *Plan) note(subject, text string) {
	if _, seen := p.noted[text]; !seen {
		p.noteOrder = append(p.noteOrder, text)
	}
	p.noted[text] = append(p.noted[text], subject)
}

// BuildPlan reads the source catalog and translates it for a target of
// the given release (see mysql.VersionNum). kept names the source tables,
// as OWNER.NAME, that the options exclude because the target already has
// a table of their name which is to stay as it is: a foreign key that
// refers to one of them is still translated.
func BuildPlan(ctx context.Context, src oracle.Queryer, cfg migrate.Config, targetVersion int, kept ...string) (*Plan, error) {
	cat, err := orasrc.LoadCatalog(ctx, src, cfg)
	if err != nil {
		return nil, err
	}
	raw, err := rawLengths(ctx, src, cat)
	if err != nil {
		return nil, err
	}
	return newPlan(cat, raw, targetVersion, kept), nil
}

// rawLengths reads the declared length of every RAW column in scope,
// keyed by OWNER.TABLE.COLUMN. The shared catalog carries the length of
// text columns only, and a VARBINARY needs one.
func rawLengths(ctx context.Context, src oracle.Queryer, cat *orasrc.Catalog) (map[string]int64, error) {
	owners := make([]string, len(cat.Owners))
	for i, o := range cat.Owners {
		owners[i] = literal(o)
	}
	rows, err := src.QueryContext(ctx, `SELECT owner, table_name, column_name, data_length FROM all_tab_cols
		WHERE owner IN (`+strings.Join(owners, ", ")+`) AND data_type = 'RAW' AND hidden_column = 'NO'`)
	if err != nil {
		return nil, oracle.Friendly(err)
	}
	defer rows.Close()
	lengths := map[string]int64{}
	for rows.Next() {
		var owner, table, column string
		var length int64
		if err := rows.Scan(&owner, &table, &column, &length); err != nil {
			return nil, oracle.Friendly(err)
		}
		lengths[owner+"."+table+"."+column] = length
	}
	return lengths, oracle.Friendly(rows.Err())
}

// newPlan translates a catalog. It asks nothing of either database.
func newPlan(cat *orasrc.Catalog, rawLength map[string]int64, targetVersion int, kept []string) *Plan {
	p := &Plan{Catalog: cat, target: features{targetVersion}, kept: map[string]bool{}, byName: map[string]*Table{}, bySrc: map[*orasrc.Table]*Table{}, noted: map[string][]string{}}
	for _, label := range kept {
		p.kept[label] = true
	}
	for _, st := range cat.Tables {
		t := &Table{Table: st, Target: myName(st.Name)}
		for _, sc := range st.Cols {
			c := Column{Column: sc, Target: myName(sc.Name), Auto: sc.IdentitySeq != ""}
			var note string
			var ok bool
			if c.Type, note, ok = translate(sc, rawLength[t.Label()+"."+sc.Name]); !ok || sc.Unsupported {
				// Preflight stops a job that has such a column; the type
				// only keeps the rest of the plan well formed.
				c.Type, note = typeLongText, ""
			}
			// MySQL only counts in its integer types. A wider NUMBER behind
			// an identity column still only ever holds whole numbers, and a
			// bigint outlasts any sequence.
			if c.Auto && !c.Type.integer() {
				c.Type, note = typeBigint, ""
			}
			if note != "" {
				p.note(t.Label()+"."+c.Name, note)
			}
			t.Cols = append(t.Cols, c)
		}
		p.Tables = append(p.Tables, t)
		p.byName[t.Label()] = t
		p.bySrc[st] = t
	}
	p.collisions()
	// Foreign keys are translated before the tables are written out,
	// because they can change a column's type.
	p.foreignKeys()
	for _, t := range p.Tables {
		p.fitRow(t)
	}
	p.buildTables()
	p.indexes()
	p.checks()

	for _, text := range p.noteOrder {
		p.Notes = append(p.Notes, strings.Join(p.noted[text], ", ")+": "+text)
	}
	return p
}

// collisions finds the names that would clash on the target. Tables of
// every schema share the one database, and MySQL never tells the case of
// a column name apart.
func (p *Plan) collisions() {
	exact, folded := map[string][]string{}, map[string][]string{}
	for _, t := range p.Tables {
		exact[t.Target] = append(exact[t.Target], t.Label())
		folded[strings.ToLower(t.Target)] = append(folded[strings.ToLower(t.Target)], t.Label())

		cols := map[string]string{}
		for _, c := range t.Cols {
			low := strings.ToLower(c.Target)
			if other, taken := cols[low]; taken {
				p.Collisions = append(p.Collisions, fmt.Sprintf("columns %s and %s of %s", other, c.Name, t.Label()))
			}
			cols[low] = c.Name
		}
	}
	for _, t := range p.Tables {
		// Each group is reported once, at its first table.
		if labels := exact[t.Target]; len(labels) > 1 && labels[0] == t.Label() {
			p.Collisions = append(p.Collisions, strings.Join(labels, " and "))
		}
		low := strings.ToLower(t.Target)
		if labels := folded[low]; len(labels) > len(exact[t.Target]) && labels[0] == t.Label() {
			p.CaseCollisions = append(p.CaseCollisions, strings.Join(labels, " and "))
		}
	}
}

// fitRow keeps a table within the size MySQL allows a row. An Oracle
// table may declare any number of VARCHAR2(4000) columns, because it
// only stores what a row really holds; MySQL adds up the declared
// lengths, at four bytes a character, and refuses a table whose total
// passes 65,535 bytes. Columns are moved out of the row, into TEXT and
// BLOB, until the rest fits: the longest first, and those no key or
// index needs before the others, since a TEXT column cannot be indexed
// in full. Columns of the primary key stay as they are.
func (p *Plan) fitRow(t *Table) {
	size := (len(t.Cols) + 7) / 8 // one bit a column says whether it is NULL
	for i := range t.Cols {
		size += t.Cols[i].rowBytes()
	}
	if size <= maxRowBytes {
		return
	}
	pinned, indexed := map[string]bool{}, map[string]bool{}
	for _, i := range t.PK {
		pinned[t.Cols[i].Name] = true
	}
	for _, x := range p.Catalog.Indexes {
		if x.Owner == t.Owner && x.Table == t.Name {
			for _, name := range x.Columns {
				indexed[name] = true
			}
		}
	}
	for _, k := range p.Catalog.ForeignKeys {
		if k.Owner == t.Owner && k.Table == t.Name {
			for _, name := range k.Columns {
				indexed[name] = true
			}
		}
		if k.RefOwner == t.Owner && k.RefTable == t.Name {
			for _, name := range k.RefColumns {
				indexed[name] = true
			}
		}
	}
	var movable []*Column
	for i := range t.Cols {
		if c := &t.Cols[i]; c.Type.Var && !pinned[c.Name] && c.rowBytes() > longBytes {
			movable = append(movable, c)
		}
	}
	sort.SliceStable(movable, func(a, b int) bool {
		if indexed[movable[a].Name] != indexed[movable[b].Name] {
			return !indexed[movable[a].Name]
		}
		return movable[a].Type.Bytes > movable[b].Type.Bytes
	})
	for _, c := range movable {
		if size <= maxRowBytes {
			break
		}
		size -= c.rowBytes() - longBytes
		was := c.Type.DDL
		// The longest VARCHAR2 and RAW are 32,767 bytes, which both fit.
		c.Type = typeText
		if c.Kind == orasrc.KindBinary {
			c.Type = typeBlob
		}
		p.note(t.Label()+"."+c.Name, fmt.Sprintf("stored as %s instead of %s, because the columns of the table together pass MySQL's limit of 65,535 bytes a row; "+
			"the declared length is no longer enforced", strings.ToUpper(c.Type.DDL), strings.ToUpper(was)))
	}
}

// keyBytes adds up the longest an index key over the given columns can
// be. It reports false when one of them cannot be indexed in full.
func keyBytes(cols []*Column) (int, bool) {
	total := 0
	for _, c := range cols {
		if c.Type.Long {
			return 0, false
		}
		total += c.Type.Bytes
	}
	return total, true
}

// buildTables writes the CREATE TABLE statements.
func (p *Plan) buildTables() {
	for _, t := range p.Tables {
		var defs, pk []string
		var key []*Column
		for i := range t.Cols {
			c := &t.Cols[i]
			def := mysql.Quote(c.Target) + " " + c.Type.DDL
			if !c.Nullable {
				def += " NOT NULL"
			}
			switch {
			case c.Auto:
				// The copy stores the source's values; the counter is moved
				// past them when the migration ends.
				def += " AUTO_INCREMENT"
			case c.Virtual:
				p.note(t.Label()+"."+c.Name, "the virtual column becomes an ordinary column holding the same values")
			case c.Default != "":
				d, why := p.columnDefault(*c)
				if d != "" {
					def += " DEFAULT " + d
				}
				if why != "" {
					p.note(t.Label()+"."+c.Name, why)
				}
			}
			defs = append(defs, def)
		}
		for _, i := range t.PK {
			pk = append(pk, mysql.Quote(t.Cols[i].Target))
			key = append(key, &t.Cols[i])
		}
		if len(pk) > 0 {
			defs = append(defs, "PRIMARY KEY ("+strings.Join(pk, ", ")+")")
			if n, ok := keyBytes(key); !ok || n > maxKeyBytes {
				p.LongKeys = append(p.LongKeys, t.Label())
			}
		}
		// MySQL wants an AUTO_INCREMENT column to lead an index.
		for i, c := range t.Cols {
			if c.Auto && (len(t.PK) == 0 || t.PK[0] != i) {
				defs = append(defs, "KEY ("+mysql.Quote(c.Target)+")")
			}
		}
		p.Pre = append(p.Pre, Stmt{Kind: "table", Name: t.Target,
			SQL: "CREATE TABLE " + t.QName() + " (\n  " + strings.Join(defs, ",\n  ") + "\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=" + p.target.collation()})
	}
}

// longNames lists the tables and columns whose names MySQL cannot take.
func (p *Plan) longNames() []string {
	var long []string
	for _, t := range p.Tables {
		if utf8.RuneCountInString(t.Target) > maxNameLength {
			long = append(long, t.Label())
		}
		for _, c := range t.Cols {
			if utf8.RuneCountInString(c.Target) > maxNameLength {
				long = append(long, t.Label()+"."+c.Name)
			}
		}
	}
	return long
}

// leaveOut removes the constraints whose names the target already has
// in use and which are to stay as they are. Each is named by its kind
// and its name on the target.
func (p *Plan) leaveOut(constraints []string) {
	if len(constraints) == 0 {
		return
	}
	skip := map[string]bool{}
	for _, c := range constraints {
		skip[strings.ToLower(c)] = true
	}
	filter := func(list []Stmt) []Stmt {
		out := list[:0:0]
		for _, s := range list {
			if s.constraint == "" || !skip[strings.ToLower(s.Kind+" "+s.constraint)] {
				out = append(out, s)
			}
		}
		return out
	}
	p.Post, p.Final = filter(p.Post), filter(p.Final)
}
