package orapg

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/migrate/orasrc"
)

var (
	numberRe   = regexp.MustCompile(`^-?([0-9]+(\.[0-9]*)?|\.[0-9]+)$`)
	stringRe   = regexp.MustCompile(`^'([^']|'')*'$`)
	nowRe      = regexp.MustCompile(`(?i)^(SYSTIMESTAMP|CURRENT_TIMESTAMP|LOCALTIMESTAMP)(\s*\(\s*[0-9]\s*\))?$`)
	nextvalRe  = regexp.MustCompile(`(?i)^(?:"?([A-Za-z0-9_$#]+)"?\.)?"?([A-Za-z0-9_$#]+)"?\."?NEXTVAL"?$`)
	quotedName = regexp.MustCompile(`"([^"]+)"`)
)

// columnDefault translates a column default. It returns false when the
// expression has no safe equivalent; an empty result means no default.
func (p *Plan) columnDefault(c Column) (string, bool) {
	d := c.Default
	up := strings.ToUpper(d)
	switch {
	case up == "NULL":
		return "", true
	case numberRe.MatchString(d) && c.Kind == orasrc.KindNumber:
		return d, true
	case stringRe.MatchString(d):
		// A quoted default for a date is read with the session's date
		// format in Oracle, which the target does not share.
		return d, c.Kind == orasrc.KindText
	case up == "SYSDATE" || up == "CURRENT_DATE":
		if c.Kind == orasrc.KindDate {
			return "LOCALTIMESTAMP(0)", true
		}
		return "CURRENT_TIMESTAMP", c.Kind == orasrc.KindTimestamp || c.Kind == orasrc.KindTimestampTZ || c.Kind == orasrc.KindTimestampLTZ
	case nowRe.MatchString(d):
		switch c.Kind {
		case orasrc.KindDate:
			return "LOCALTIMESTAMP(0)", true
		case orasrc.KindTimestamp:
			return "LOCALTIMESTAMP", true
		case orasrc.KindTimestampTZ, orasrc.KindTimestampLTZ:
			return "CURRENT_TIMESTAMP", true
		}
	case up == "SYS_GUID()" && c.Kind == orasrc.KindBinary:
		return "decode(replace(gen_random_uuid()::text, '-', ''), 'hex')", true
	case up == "USER" && c.Kind == orasrc.KindText:
		return "CURRENT_USER", true
	case (up == "TRUE" || up == "FALSE") && c.Kind == orasrc.KindBool:
		return up, true
	}
	if m := nextvalRe.FindStringSubmatch(d); m != nil && c.Kind == orasrc.KindNumber {
		for _, s := range p.Sequences {
			if strings.EqualFold(s.Name, m[2]) && (m[1] == "" || strings.EqualFold(s.Owner, m[1])) {
				return "nextval(" + literal(pgx.Identifier{s.Schema, s.Target}.Sanitize()) + "::regclass)", true
			}
		}
	}
	return "", false
}

// columns maps Oracle column names to quoted target names, or reports
// false when a name is not a column of the table.
func (t *Table) columns(names []string) (string, bool) {
	var out []string
	for _, name := range names {
		found := false
		for _, c := range t.Cols {
			if c.Name == name {
				out = append(out, qident(c.Target))
				found = true
			}
		}
		if !found {
			return "", false
		}
	}
	return strings.Join(out, ", "), true
}

// indexes translates secondary indexes, including the unique indexes
// behind unique constraints. The primary key's index comes with the table.
func (p *Plan) indexes() {
	for _, x := range p.Catalog.Indexes {
		t := p.Table(x.Owner, x.Table)
		index, kind := x.Name, x.Type
		cols, plain := t.columns(x.Columns)
		switch {
		case strings.HasPrefix(kind, "FUNCTION-BASED") || !plain:
			// Descending columns are stored as expressions too.
			p.Notes = append(p.Notes, fmt.Sprintf("%s: the function-based index %s was not translated", t.Label(), index))
			continue
		case kind == "BITMAP":
			p.Notes = append(p.Notes, fmt.Sprintf("%s: the bitmap index %s becomes an ordinary B-tree index", t.Label(), index))
		case kind != "NORMAL" && kind != "NORMAL/REV":
			p.Notes = append(p.Notes, fmt.Sprintf("%s: the %s index %s was not translated", t.Label(), strings.ToLower(kind), index))
			continue
		}
		unique := ""
		if x.Unique {
			unique = "UNIQUE "
		}
		name := clip(pgName(index))
		p.Post = append(p.Post, Stmt{Kind: "index", Name: t.Schema + "." + name, Soft: true, ident: pgx.Identifier{t.Schema, name}.Sanitize(),
			SQL: "CREATE " + unique + "INDEX " + qident(name) + " ON " + t.QName() + " (" + cols + ")"})
	}
}

// checks carries check constraints over as they are written. Most are
// plain comparisons that mean the same in both databases; one that uses
// an Oracle-only function fails to apply and is reported then.
func (p *Plan) checks() {
	for _, k := range p.Catalog.Checks {
		t := p.Table(k.Owner, k.Table)
		cond := quotedName.ReplaceAllStringFunc(k.Condition, func(q string) string {
			for _, c := range t.Cols {
				if q == `"`+c.Name+`"` {
					return qident(c.Target)
				}
			}
			return q
		})
		name := clip(pgName(k.Name))
		p.Post = append(p.Post, Stmt{Kind: "check constraint", Name: t.Schema + "." + t.Target + " " + name, Soft: true,
			SQL: "ALTER TABLE " + t.QName() + " ADD CONSTRAINT " + qident(name) + " CHECK (" + cond + ")"})
	}
}

// foreignKeys translates the enabled foreign keys between migrated
// tables, with their ON DELETE rule.
func (p *Plan) foreignKeys() {
	for _, k := range p.Catalog.ForeignKeys {
		t := p.Table(k.Owner, k.Table)
		parent := p.Table(k.RefOwner, k.RefTable)
		if parent == nil {
			p.Notes = append(p.Notes, fmt.Sprintf("%s: foreign key %s refers to %s.%s, which is not being migrated, and was left out", t.Label(), k.Name, k.RefOwner, k.RefTable))
			continue
		}
		cols, ok1 := t.columns(k.Columns)
		refs, ok2 := parent.columns(k.RefColumns)
		if !ok1 || !ok2 {
			continue
		}
		p.alignKeyTypes(t, parent, k.Columns, k.RefColumns)
		name := clip(pgName(k.Name))
		ddl := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)", t.QName(), qident(name), cols, parent.QName(), refs)
		if k.DeleteRule == "CASCADE" || k.DeleteRule == "SET NULL" {
			ddl += " ON DELETE " + k.DeleteRule
		}
		if k.Deferrable {
			ddl += " DEFERRABLE"
			if k.Deferred {
				ddl += " INITIALLY DEFERRED"
			}
		}
		p.Final = append(p.Final, Stmt{Kind: "foreign key", Name: t.Schema + "." + t.Target + " " + name, Soft: true, SQL: ddl})
	}
}

// alignKeyTypes gives the columns of a foreign key the type of the key
// they refer to. In Oracle every NUMBER compares with every other, but
// PostgreSQL will not tie a numeric column to an integer key, and two
// declarations of the same key often differ: NUMBER on one side,
// NUMBER(10) on the other. Every value of the referring column exists in
// the key, so the key's type holds them all.
func (p *Plan) alignKeyTypes(child, parent *Table, cols, refs []string) {
	find := func(t *Table, name string) *Column {
		for i := range t.Cols {
			if t.Cols[i].Name == name {
				return &t.Cols[i]
			}
		}
		return nil
	}
	for i := range cols {
		if i >= len(refs) {
			break
		}
		c, ref := find(child, cols[i]), find(parent, refs[i])
		if c == nil || ref == nil || c.Kind != orasrc.KindNumber || ref.Kind != orasrc.KindNumber || c.PGType == ref.PGType {
			continue
		}
		p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: stored as %s instead of %s, the type of the key it refers to (%s.%s)",
			child.Label(), c.Name, ref.PGType, c.PGType, parent.Label(), ref.Name))
		c.PGType = ref.PGType
	}
}
