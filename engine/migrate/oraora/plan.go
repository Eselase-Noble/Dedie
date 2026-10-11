package oraora

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
)

// Table is a source table and the schema that receives it. Its name and
// its columns are the same on both sides.
type Table struct {
	*orasrc.Table
	Schema string
	// stored holds the positions in Cols of the columns that have values
	// of their own. A virtual column is computed on the target as it is
	// on the source and cannot be written to.
	stored []int
}

// QName is the table's quoted name on the target.
func (t *Table) QName() string { return oracle.QName(t.Schema, t.Name) }

// Target names the table on the target in messages.
func (t *Table) Target() string { return t.Schema + "." + t.Name }

// Stmt is one DDL statement to run on the target.
type Stmt struct {
	// Kind is what messages call the thing the statement makes.
	Kind string
	// Object is the kind of object whose name the statement takes on the
	// target, or empty when it takes none: a constraint or a comment is
	// part of its table. A package body goes by "package" and a type
	// body by "type", because they stand or fall with the specification.
	Object string
	// Schema and Name are the object's on the target.
	Schema, Name string
	// Table is the table or view the object belongs to, if any.
	Table string
	SQL   string
	// Soft statements may fail without failing the job; the failure is
	// reported to the operator. Everything but a table and its primary
	// key is soft, because any of it can depend on something outside the
	// migration's scope.
	Soft bool
}

// Label names the statement's object in messages.
func (s Stmt) Label() string {
	if s.Table != "" && s.Object == "" {
		return s.Schema + "." + s.Table + " " + s.Name
	}
	return s.Schema + "." + s.Name
}

// Sequence is a source sequence and the schema that receives it.
type Sequence struct {
	Owner, Name string
	Schema      string
}

// Identity is an identity column of a migrated table.
type Identity struct {
	Table  *Table
	Column string
	// Seq is the sequence behind the column on the source.
	Seq string
	// Always says the column is GENERATED ALWAYS on the source. It is
	// created BY DEFAULT, so that the source's values can be stored, and
	// made ALWAYS again at the end. OnNull is the ON NULL of BY DEFAULT.
	Always, OnNull bool
}

// Plan is what will be migrated and the statements that build it.
type Plan struct {
	// Catalog is the source as it describes itself.
	Catalog *orasrc.Catalog
	Tables  []*Table
	// Target is the receiving server, and Schemas the schema there that
	// receives each source schema. MapNotes says why in words; a schema
	// that cannot be received is in MapProblems.
	Target      *target
	Schemas     map[string]string
	MapNotes    []string
	MapProblems []string
	// Pre creates sequences and tables. Code creates types, stored code,
	// views and synonyms once the data is in, so that a dependency that
	// does not compile cannot hold up the copy; Post then builds indexes
	// and constraints over the finished tables. Final adds foreign keys
	// and triggers when the migration ends: foreign keys because rows
	// are applied table by table while changes are followed, not in the
	// order the source wrote them, and triggers so that they do not fire
	// for rows that were copied or replicated.
	Pre, Code, Post, Final []Stmt
	Sequences              []Sequence
	Identities             []Identity
	// Notes list what arrives differing from the source, and Left what
	// was found on the source that is not carried over. Preflight shows
	// both to the operator.
	Notes, Left []string

	defs  *definitions
	bySrc map[*orasrc.Table]*Table
}

// of finds the plan's table for a table of the catalog, or nil when the
// table takes no part in the migration.
func (p *Plan) of(t *orasrc.Table) *Table { return p.bySrc[t] }

// checkpointName is the quoted name of the checkpoint table, which lives
// in the schema that receives the first of the source schemas.
func (p *Plan) checkpointName() string {
	return oracle.QName(p.Schemas[p.Catalog.Owners[0]], checkpointTable)
}

// kept is what was already on the target and stays as it is; see
// migrate.ExistingSkip.
type kept struct {
	// sources are the kept tables by their name on the source.
	sources []string
	// objects holds the other kept objects, each as "kind SCHEMA.NAME".
	objects map[string]bool
}

func keptOf(st *State) kept {
	k := kept{sources: st.SkippedSources, objects: map[string]bool{}}
	for _, o := range st.SkippedObjects {
		k.objects[o] = true
	}
	return k
}

// without takes the kept tables out of the operator's selection, the
// same way an excluded table is left out. user is the schema an
// unqualified table name stands in.
func without(cfg migrate.Config, sources []string, user string) (migrate.Config, error) {
	if len(sources) == 0 {
		return cfg, nil
	}
	skip := map[string]bool{}
	for _, s := range sources {
		skip[strings.ToUpper(s)] = true
	}
	cfg.ExcludeTables = append(cfg.ExcludeTables[:len(cfg.ExcludeTables):len(cfg.ExcludeTables)], sources...)
	if len(cfg.Tables) == 0 {
		return cfg, nil
	}
	// A table asked for by name and then left out would be reported as
	// missing, so it is taken off the list as well.
	var tables []string
	for _, name := range cfg.Tables {
		full := strings.ToUpper(strings.TrimSpace(name))
		if !strings.Contains(full, ".") {
			full = strings.ToUpper(user) + "." + full
		}
		if !skip[full] {
			tables = append(tables, name)
		}
	}
	if len(tables) == 0 {
		return cfg, errors.New("every table selected for migration already exists on the target and is kept as it is: there is nothing to migrate")
	}
	cfg.Tables = tables
	return cfg, nil
}

// BuildPlan reads the source's catalog and definitions and works out
// what to create on the target. origin tells this source from others in
// the cache of table definitions. prev, when given, is an earlier plan
// for the same job whose definitions are used again: reading them is the
// slow part, and it is the same whatever is kept.
func BuildPlan(ctx context.Context, src, dst *sql.DB, origin string, cfg migrate.Config, keep kept, prev *Plan) (*Plan, error) {
	var user string
	if err := src.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM dual`).Scan(&user); err != nil {
		return nil, oracle.Friendly(err)
	}
	scoped, err := without(cfg, keep.sources, user)
	if err != nil {
		return nil, err
	}
	cat, err := orasrc.LoadCatalog(ctx, src, scoped)
	if err != nil {
		if len(keep.sources) > 0 && strings.Contains(err.Error(), "there are no tables to migrate") {
			return nil, errors.New("every table in scope already exists on the target and is kept as it is: there is nothing to migrate")
		}
		return nil, err
	}
	p := &Plan{Catalog: cat, bySrc: map[*orasrc.Table]*Table{}}
	if p.Target, err = loadTarget(ctx, dst, cat.Owners); err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	p.Schemas, p.MapNotes, p.MapProblems = mapSchemas(cat.Owners, p.Target)
	if len(p.MapProblems) > 0 {
		// Nothing can be planned for a schema with nowhere to go; the
		// preflight reports the problems.
		return p, nil
	}
	if prev != nil && prev.defs != nil {
		p.defs = prev.defs
	} else if p.defs, err = readDefinitions(ctx, src, origin, cat, p.Schemas, cfg.Scope() == "table"); err != nil {
		return nil, err
	}
	p.assemble(keep)
	return p, nil
}

// stmtKinds says, for each object type read with fetchDefinitions, what
// its statements are called and whose name they take.
var stmtKinds = map[string][2]string{
	"SEQUENCE":     {"sequence", "sequence"},
	"TYPE_SPEC":    {"type", "type"},
	"TYPE_BODY":    {"type body", "type"},
	"FUNCTION":     {"function", "function"},
	"PROCEDURE":    {"procedure", "procedure"},
	"PACKAGE_SPEC": {"package", "package"},
	"PACKAGE_BODY": {"package body", "package"},
	"VIEW":         {"view", "view"},
	"SYNONYM":      {"synonym", "synonym"},
	"TRIGGER":      {"trigger", "trigger"},
	"INDEX":        {"index", "index"},
}

// codeTypes are created in this order: specifications before the bodies
// and views that use them. The order only saves recompiling, since each
// of these can be created before what it depends on exists.
var codeTypes = []string{"TYPE_SPEC", "FUNCTION", "PROCEDURE", "PACKAGE_SPEC", "TYPE_BODY", "PACKAGE_BODY", "VIEW", "SYNONYM"}

// assemble turns the definitions into the statements of each phase,
// leaving out whatever is kept on the target.
func (p *Plan) assemble(keep kept) {
	d := p.defs
	// has holds the tables and views the plan creates, by owner and name:
	// what indexes, constraints, comments and triggers may attach to.
	has := map[string]bool{}
	for _, st := range p.Catalog.Tables {
		t := &Table{Table: st, Schema: p.Schemas[st.Owner]}
		for i, c := range st.Cols {
			if !c.Virtual {
				t.stored = append(t.stored, i)
			}
		}
		p.Tables = append(p.Tables, t)
		p.bySrc[st] = t
		has[st.Label()] = true
	}

	// stmt makes the statement for a definition read with fetchDefinitions.
	stmt := func(objectType, owner string, def definition) Stmt {
		k := stmtKinds[objectType]
		return Stmt{Kind: k[0], Object: k[1], Schema: p.Schemas[owner], Name: def.Name, Table: def.Base, Soft: true,
			SQL: createOnly(requalify(strings.TrimSpace(def.Text), owner, p.Schemas[owner]))}
	}
	isKept := func(s Stmt) bool { return keep.objects[s.Object+" "+s.Schema+"."+s.Name] }

	var tables, temporary []Stmt
	for _, owner := range p.Catalog.Owners {
		schema := p.Schemas[owner]
		for _, def := range d.objects[owner]["SEQUENCE"] {
			// The sequence behind an identity column comes with its table.
			if s := stmt("SEQUENCE", owner, def); !strings.HasPrefix(def.Name, "ISEQ$$") && !isKept(s) {
				p.Pre = append(p.Pre, s)
				p.Sequences = append(p.Sequences, Sequence{Owner: owner, Name: def.Name, Schema: schema})
			}
		}
		for _, name := range d.temporary[owner] {
			s := Stmt{Kind: "temporary table", Object: "table", Schema: schema, Name: name, Soft: true,
				SQL: requalify(strings.TrimSpace(d.tables[owner+"."+name]), owner, schema)}
			if !isKept(s) {
				temporary = append(temporary, s)
			}
		}
		for _, objectType := range codeTypes {
			for _, def := range d.objects[owner][objectType] {
				s := stmt(objectType, owner, def)
				if isKept(s) {
					continue
				}
				p.Code = append(p.Code, s)
				if objectType == "VIEW" {
					has[owner+"."+def.Name] = true
				}
			}
		}
	}
	for _, t := range p.Tables {
		tables = append(tables, Stmt{Kind: "table", Object: "table", Schema: t.Schema, Name: t.Name,
			SQL: acceptValues(requalify(strings.TrimSpace(d.tables[t.Label()]), t.Owner, t.Schema))})
	}
	// Sequences first: a column default may call one.
	p.Pre = append(append(p.Pre, tables...), temporary...)

	var comments []Stmt
	for _, owner := range p.Catalog.Owners {
		for _, def := range d.objects[owner]["INDEX"] {
			// Only the indexes somebody named are created by name. One
			// that Oracle made for a constraint or a large object comes
			// back with the constraint or the column.
			table, named := d.indexTable[owner+"."+def.Name]
			s := stmt("INDEX", owner, def)
			s.Table = table
			if named && p.Catalog.Table(owner, table) != nil && !isKept(s) {
				p.Post = append(p.Post, s)
			}
		}
	}
	for _, owner := range p.Catalog.Owners {
		schema := p.Schemas[owner]
		for _, def := range d.objects[owner]["CONSTRAINT"] {
			if p.Catalog.Table(owner, def.Base) == nil {
				continue
			}
			text := requalify(strings.TrimSpace(def.Text), owner, schema)
			// Without its primary key a table's changes cannot be applied.
			p.Post = append(p.Post, Stmt{Kind: "constraint", Schema: schema, Name: def.Name, Table: def.Base, SQL: text,
				Soft: !strings.Contains(text, " PRIMARY KEY ")})
		}
		for _, def := range d.objects[owner]["COMMENT"] {
			if on := commentedObject(def.Text); on != "" && has[owner+"."+on] {
				comments = append(comments, Stmt{Kind: "comment", Schema: schema, Name: "comment", Table: on, Soft: true,
					SQL: requalify(strings.TrimSpace(def.Text), owner, schema)})
			}
		}
		for _, def := range d.objects[owner]["REF_CONSTRAINT"] {
			if p.Catalog.Table(owner, def.Base) != nil {
				p.Final = append(p.Final, Stmt{Kind: "foreign key", Schema: schema, Name: def.Name, Table: def.Base, Soft: true,
					SQL: requalify(strings.TrimSpace(def.Text), owner, schema)})
			}
		}
	}
	p.Post = append(p.Post, comments...)

	for _, owner := range p.Catalog.Owners {
		// A trigger comes as two statements: it is created, then enabled
		// or disabled as it was. Only the first names its table.
		on := map[string]string{}
		schemaLevel := 0
		for _, def := range d.objects[owner]["TRIGGER"] {
			if def.Verb == "CREATE" {
				on[def.Name] = def.Base
				if def.Base == "" {
					schemaLevel++
				}
			}
			s := stmt("TRIGGER", owner, def)
			s.Table = on[def.Name]
			if s.Table != "" && has[owner+"."+s.Table] && !isKept(s) {
				p.Final = append(p.Final, s)
			}
		}
		if schemaLevel > 0 {
			p.Left = append(p.Left, fmt.Sprintf("%d trigger(s) of %s that fire for the schema or the database rather than for a table", schemaLevel, owner))
		}
	}

	for _, row := range d.identities {
		if t := p.Catalog.Table(row[0], row[1]); t != nil {
			p.Identities = append(p.Identities, Identity{Table: p.of(t), Column: row[2], Seq: row[4], Always: row[3] == "ALWAYS", OnNull: row[5] == "YES"})
		}
	}
	p.Left = append(p.Left, d.left...)
	p.notes()
}

// notes records what the operator should know about how the plan's
// objects arrive.
func (p *Plan) notes() {
	for _, owner := range p.Catalog.Owners {
		schema := p.Schemas[owner]
		if schema == owner {
			continue
		}
		// What was written by a person keeps the names its author gave it.
		var stray []string
		seen := map[string]bool{}
		for _, list := range [][]Stmt{p.Pre, p.Code, p.Post, p.Final} {
			for _, s := range list {
				if s.Schema == schema && mentions(s.SQL, owner) && !seen[s.Kind+s.Label()] {
					seen[s.Kind+s.Label()] = true
					stray = append(stray, s.Kind+" "+s.Label())
				}
			}
		}
		if len(stray) > 0 {
			sort.Strings(stray)
			p.Notes = append(p.Notes, fmt.Sprintf("these name the schema %s in their own text, which is carried over as written and still points at %s on the target; change them there if they should use %s: %s",
				owner, owner, schema, strings.Join(stray, ", ")))
		}
	}
	var always, long, zoned, rowid []string
	for _, id := range p.Identities {
		if id.Always {
			always = append(always, id.Table.Label()+"."+id.Column)
		}
	}
	for _, t := range p.Tables {
		for _, c := range t.Cols {
			switch {
			case c.DataType == "LONG" || c.DataType == "LONG RAW":
				long = append(long, t.Label()+"."+c.Name)
			case c.Kind == orasrc.KindTimestampTZ:
				zoned = append(zoned, t.Label()+"."+c.Name)
			case c.Kind == orasrc.KindRowid:
				rowid = append(rowid, t.Label()+"."+c.Name)
			}
		}
	}
	if len(always) > 0 {
		p.Notes = append(p.Notes, "these identity columns are GENERATED ALWAYS; they are created GENERATED BY DEFAULT so that the source's values can be stored, and made ALWAYS again when the migration ends: "+strings.Join(always, ", "))
	}
	if len(zoned) > 0 {
		p.Notes = append(p.Notes, "TIMESTAMP WITH TIME ZONE values keep their instant, their local time and their offset from UTC; a value stored with a region name such as Europe/Paris arrives with the offset that region had at that moment: "+strings.Join(zoned, ", "))
	}
	if len(long) > 0 {
		p.Notes = append(p.Notes, "LONG and LONG RAW values are carried as ordinary strings and byte strings, which holds up to the server's limit for those (4000 bytes, or 32767 with extended string sizes); a longer value stops the job with Oracle's error rather than being cut. Consider converting the columns to CLOB or BLOB first: "+strings.Join(long, ", "))
	}
	if len(rowid) > 0 {
		p.Notes = append(p.Notes, "ROWID values are copied as they are, and on the target no longer point at the rows they did: "+strings.Join(rowid, ", "))
	}
}
