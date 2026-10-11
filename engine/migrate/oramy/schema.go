package oramy

import (
	"fmt"
	"regexp"
	"strings"

	"dedie/engine/connector/mysql"
	"dedie/engine/migrate/orasrc"
)

var (
	numberRe   = regexp.MustCompile(`^-?([0-9]+(\.[0-9]*)?|\.[0-9]+)$`)
	stringRe   = regexp.MustCompile(`^'([^']|'')*'$`)
	nowRe      = regexp.MustCompile(`(?i)^(SYSDATE|CURRENT_DATE|((SYSTIMESTAMP|CURRENT_TIMESTAMP|LOCALTIMESTAMP)(\s*\(\s*[0-9]\s*\))?))$`)
	nextvalRe  = regexp.MustCompile(`(?i)^(?:"?([A-Za-z0-9_$#]+)"?\.)?"?([A-Za-z0-9_$#]+)"?\."?NEXTVAL"?$`)
	quotedName = regexp.MustCompile(`"([^"]+)"`)
)

// columnDefault translates a column default. It returns the default as
// MySQL writes it, empty when the column gets none, and a note when the
// original was not carried over.
func (p *Plan) columnDefault(c Column) (def, note string) {
	d := c.Default
	up := strings.ToUpper(d)
	untranslated := fmt.Sprintf("the default (%s) was not translated", d)
	// A default that is an expression has to be written in parentheses,
	// which MySQL has only understood since 8.0.13.
	expr := func(e string) (string, string) {
		if !p.target.exprDefaults() {
			return "", untranslated
		}
		return "(" + e + ")", ""
	}
	switch {
	case up == "NULL":
		return "", ""
	case numberRe.MatchString(d) && c.Kind == orasrc.KindNumber:
		return d, ""
	case stringRe.MatchString(d) && c.Kind == orasrc.KindText:
		// A quoted default for a date is read with the session's date
		// format in Oracle, which the target does not share; only text
		// is taken. A backslash means itself in Oracle and starts an
		// escape in MySQL.
		lit := strings.ReplaceAll(d, `\`, `\\`)
		if c.Type.Long {
			// TEXT takes no literal default, only an expression.
			return expr(lit)
		}
		return lit, ""
	case nowRe.MatchString(d):
		switch c.Kind {
		case orasrc.KindDate, orasrc.KindTimestamp:
			// The fractional digits must be those of the column.
			return "CURRENT_TIMESTAMP" + strings.TrimPrefix(c.Type.DDL, "datetime"), ""
		case orasrc.KindTimestampTZ, orasrc.KindTimestampLTZ:
			// These columns hold UTC, whatever the session's time zone.
			return expr("UTC_TIMESTAMP(6)")
		}
	case up == "SYS_GUID()" && c.Kind == orasrc.KindBinary && !c.Type.Long:
		return expr("UNHEX(REPLACE(UUID(), '-', ''))")
	case up == "USER" && c.Kind == orasrc.KindText:
		// USER() is name@host in MySQL.
		return expr("SUBSTRING_INDEX(USER(), '@', 1)")
	case (up == "TRUE" || up == "FALSE") && c.Kind == orasrc.KindBool:
		if up == "TRUE" {
			return "1", ""
		}
		return "0", ""
	case nextvalRe.MatchString(d):
		return "", fmt.Sprintf("the default (%s) was dropped: MySQL has no sequences, so whoever inserts a row has to supply the value", d)
	}
	return "", untranslated
}

// columns maps Oracle column names to the target's columns, or reports
// false when a name is not a column of the table.
func (t *Table) columns(names []string) ([]*Column, bool) {
	var out []*Column
	for _, name := range names {
		c := t.col(name)
		if c == nil {
			return nil, false
		}
		out = append(out, c)
	}
	return out, len(out) > 0
}

// quoted lists the columns' names as MySQL writes them.
func quoted(cols []*Column) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = mysql.Quote(c.Target)
	}
	return strings.Join(names, ", ")
}

// indexes translates secondary indexes, including the unique indexes
// behind unique constraints. The primary key's index comes with the
// table. An index MySQL cannot build is left out with a note rather than
// left to fail the job: one over a column that became TEXT or a large
// object, or one whose key could pass InnoDB's 3072 bytes.
func (p *Plan) indexes() {
	for _, x := range p.Catalog.Indexes {
		t := p.Table(x.Owner, x.Table)
		index, kind := x.Name, x.Type
		cols, plain := t.columns(x.Columns)
		switch {
		case strings.HasPrefix(kind, "FUNCTION-BASED") || !plain:
			// Descending columns are stored as expressions too.
			p.note(t.Label(), fmt.Sprintf("the function-based index %s was not translated", index))
			continue
		case kind == "BITMAP":
			p.note(t.Label(), fmt.Sprintf("the bitmap index %s becomes an ordinary B-tree index", index))
		case kind != "NORMAL" && kind != "NORMAL/REV":
			p.note(t.Label(), fmt.Sprintf("the %s index %s was not translated", strings.ToLower(kind), index))
			continue
		}
		size, ok := keyBytes(cols)
		if !ok {
			p.note(t.Label(), fmt.Sprintf("the index %s was not translated: it covers a column stored as TEXT, BLOB or JSON, which MySQL cannot index in full", index))
			continue
		}
		if size > maxKeyBytes {
			p.note(t.Label(), fmt.Sprintf("the index %s was not translated: its key could be %d bytes long, and MySQL allows %d", index, size, maxKeyBytes))
			continue
		}
		unique := ""
		if x.Unique {
			unique = "UNIQUE "
		}
		name := clip(myName(index))
		p.Post = append(p.Post, Stmt{Kind: "index", Name: t.Target + "." + name, Soft: true,
			SQL: "CREATE " + unique + "INDEX " + mysql.Quote(name) + " ON " + t.QName() + " (" + quoted(cols) + ")"})
	}
}

// checks carries check constraints over as they are written. Most are
// plain comparisons that mean the same in both databases; one that uses
// an Oracle-only function fails to apply and is reported then.
func (p *Plan) checks() {
	for _, k := range p.Catalog.Checks {
		t := p.Table(k.Owner, k.Table)
		if !p.target.checks() {
			p.note(t.Label(), fmt.Sprintf("the check constraint %s was left out: MySQL enforces check constraints from 8.0.16 on", k.Name))
			continue
		}
		cond := quotedName.ReplaceAllStringFunc(k.Condition, func(q string) string {
			for _, c := range t.Cols {
				if q == `"`+c.Name+`"` {
					return mysql.Quote(c.Target)
				}
			}
			return q
		})
		name := clip(myName(k.Name))
		p.Post = append(p.Post, Stmt{Kind: kindCheck, Name: t.Target + " " + name, Soft: true, constraint: name,
			SQL: "ALTER TABLE " + t.QName() + " ADD CONSTRAINT " + mysql.Quote(name) + " CHECK (" + cond + ")"})
	}
}

// link is a foreign key between two migrated tables.
type link struct {
	key        orasrc.ForeignKey
	child      *Table
	parent     *Table
	cols, refs []*Column
}

// foreignKeys translates the enabled foreign keys between migrated
// tables, with their ON DELETE rule.
func (p *Plan) foreignKeys() {
	var links []link
	for _, k := range p.Catalog.ForeignKeys {
		t := p.Table(k.Owner, k.Table)
		parent := p.Table(k.RefOwner, k.RefTable)
		if parent == nil && p.kept[k.RefOwner+"."+k.RefTable] {
			p.keptParent(t, k)
			continue
		}
		if parent == nil {
			p.note(t.Label(), fmt.Sprintf("foreign key %s refers to %s.%s, which is not being migrated, and was left out", k.Name, k.RefOwner, k.RefTable))
			continue
		}
		cols, ok1 := t.columns(k.Columns)
		refs, ok2 := parent.columns(k.RefColumns)
		if !ok1 || !ok2 || len(cols) != len(refs) {
			continue
		}
		links = append(links, link{key: k, child: t, parent: parent, cols: cols, refs: refs})
	}
	p.alignKeyTypes(links)
	for _, l := range links {
		p.foreignKey(l.child, l.key, quoted(l.cols), l.parent.QName(), quoted(l.refs))
	}
}

// foreignKey writes the statement that adds one foreign key.
func (p *Plan) foreignKey(child *Table, k orasrc.ForeignKey, cols, parent, refs string) {
	name := clip(myName(k.Name))
	ddl := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)", child.QName(), mysql.Quote(name), cols, parent, refs)
	if k.DeleteRule == "CASCADE" || k.DeleteRule == "SET NULL" {
		ddl += " ON DELETE " + k.DeleteRule
	}
	if k.Deferrable {
		p.note(child.Label(), fmt.Sprintf("foreign key %s is deferrable in Oracle; MySQL checks every foreign key statement by statement", k.Name))
	}
	p.Final = append(p.Final, Stmt{Kind: kindForeignKey, Name: child.Target + " " + name, Soft: true, constraint: name, SQL: ddl})
}

// keptParent translates a foreign key to a table that is not migrated
// because the target already has one of its name, which stays as it is.
// The key is still worth trying: the table on the target is most likely
// an earlier copy of the same one. Its columns are not known here, so
// they are named the way the migration would have named them and no
// types are aligned; if the two do not fit, adding the key fails and is
// reported, like any other foreign key that cannot be added.
func (p *Plan) keptParent(child *Table, k orasrc.ForeignKey) {
	cols, ok := child.columns(k.Columns)
	if !ok || len(cols) != len(k.RefColumns) {
		return
	}
	refs := make([]string, len(k.RefColumns))
	for i, name := range k.RefColumns {
		refs[i] = mysql.Quote(myName(name))
	}
	p.foreignKey(child, k, quoted(cols), mysql.Quote(myName(k.RefTable)), strings.Join(refs, ", "))
}

// alignKeyTypes gives the columns of a foreign key the type of the key
// they refer to. In Oracle every NUMBER compares with every other, but
// MySQL only ties columns of the same integer type or the same DECIMAL
// together, and two declarations of one key often differ: NUMBER on one
// side, NUMBER(10) on the other. Every value of the referring column
// exists in the key, so the key's type holds them all.
//
// A key may itself refer to another, so the types are passed along until
// nothing changes any more; the bound only matters to keys that refer to
// each other in a circle with different types, which no bound can settle.
func (p *Plan) alignKeyTypes(links []link) {
	was := map[*Column]string{}
	for pass := 0; pass <= len(links); pass++ {
		changed := false
		for _, l := range links {
			for i, c := range l.cols {
				ref := l.refs[i]
				// An identity column keeps the integer type it counts in.
				if c.Kind != orasrc.KindNumber || ref.Kind != orasrc.KindNumber || c.Type == ref.Type || c.Auto {
					continue
				}
				if _, seen := was[c]; !seen {
					was[c] = c.Type.DDL
				}
				c.Type, changed = ref.Type, true
			}
		}
		if !changed {
			break
		}
	}
	for _, l := range links {
		for i, c := range l.cols {
			if before, ok := was[c]; ok && before != c.Type.DDL {
				ref := l.refs[i]
				p.note(l.child.Label()+"."+c.Name, fmt.Sprintf("stored as %s instead of %s, the type of the key it refers to (%s.%s)",
					strings.ToUpper(c.Type.DDL), strings.ToUpper(before), l.parent.Label(), ref.Name))
				delete(was, c)
			}
		}
	}
}
