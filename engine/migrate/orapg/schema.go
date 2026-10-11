package orapg

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector/oracle"
)

var (
	numberRe   = regexp.MustCompile(`^-?([0-9]+(\.[0-9]*)?|\.[0-9]+)$`)
	stringRe   = regexp.MustCompile(`^'([^']|'')*'$`)
	nowRe      = regexp.MustCompile(`(?i)^(SYSTIMESTAMP|CURRENT_TIMESTAMP|LOCALTIMESTAMP)(\s*\(\s*[0-9]\s*\))?$`)
	nextvalRe  = regexp.MustCompile(`(?i)^(?:"?([A-Za-z0-9_$#]+)"?\.)?"?([A-Za-z0-9_$#]+)"?\."?NEXTVAL"?$`)
	notNullRe  = regexp.MustCompile(`^"[^"]+" IS NOT NULL$`)
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
	case numberRe.MatchString(d) && c.Kind == kNumber:
		return d, true
	case stringRe.MatchString(d):
		// A quoted default for a date is read with the session's date
		// format in Oracle, which the target does not share.
		return d, c.Kind == kText
	case up == "SYSDATE" || up == "CURRENT_DATE":
		if c.Kind == kDate {
			return "LOCALTIMESTAMP(0)", true
		}
		return "CURRENT_TIMESTAMP", c.Kind == kTimestamp || c.Kind == kTimestampTZ || c.Kind == kTimestampLTZ
	case nowRe.MatchString(d):
		switch c.Kind {
		case kDate:
			return "LOCALTIMESTAMP(0)", true
		case kTimestamp:
			return "LOCALTIMESTAMP", true
		case kTimestampTZ, kTimestampLTZ:
			return "CURRENT_TIMESTAMP", true
		}
	case up == "SYS_GUID()" && c.Kind == kBinary:
		return "decode(replace(gen_random_uuid()::text, '-', ''), 'hex')", true
	case up == "USER" && c.Kind == kText:
		return "CURRENT_USER", true
	case (up == "TRUE" || up == "FALSE") && c.Kind == kBool:
		return up, true
	}
	if m := nextvalRe.FindStringSubmatch(d); m != nil && c.Kind == kNumber {
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
func (t *Table) columns(lines string) (string, bool) {
	var out []string
	for _, name := range strings.Split(lines, "\n") {
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

// loadIndexes translates secondary indexes, including the unique indexes
// behind unique constraints. The primary key's index comes with the table.
func (p *Plan) loadIndexes(ctx context.Context, src oracle.Queryer, in string) error {
	rows, err := query(ctx, src, `SELECT i.table_owner, i.table_name, i.index_name, i.index_type, i.uniqueness,
			LISTAGG(c.column_name, CHR(10)) WITHIN GROUP (ORDER BY c.column_position)
		FROM all_indexes i JOIN all_ind_columns c ON c.index_owner = i.owner AND c.index_name = i.index_name
		WHERE i.table_owner `+in+` AND i.index_type NOT IN ('LOB', 'IOT - TOP')
		  AND NOT EXISTS (SELECT 1 FROM all_constraints k WHERE k.owner = i.table_owner AND k.table_name = i.table_name
		                  AND k.constraint_type = 'P' AND k.index_name = i.index_name)
		GROUP BY i.table_owner, i.table_name, i.index_name, i.index_type, i.uniqueness ORDER BY 1, 2, 3`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		t := p.Table(r[0], r[1])
		if t == nil {
			continue
		}
		index, kind := r[2], r[3]
		cols, plain := t.columns(r[5])
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
		if r[4] == "UNIQUE" {
			unique = "UNIQUE "
		}
		name := clip(pgName(index))
		p.Post = append(p.Post, Stmt{Kind: "index", Name: t.Schema + "." + name, Soft: true,
			SQL: "CREATE " + unique + "INDEX " + qident(name) + " ON " + t.QName() + " (" + cols + ")"})
	}
	return nil
}

// loadChecks carries check constraints over as they are written. Most are
// plain comparisons that mean the same in both databases; one that uses
// an Oracle-only function fails to apply and is reported then.
func (p *Plan) loadChecks(ctx context.Context, src oracle.Queryer, in string) error {
	rows, err := query(ctx, src, `SELECT owner, table_name, constraint_name, search_condition_vc FROM all_constraints
		WHERE owner `+in+` AND constraint_type = 'C' AND status = 'ENABLED' ORDER BY 1, 2, 3`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		t := p.Table(r[0], r[1])
		cond := strings.TrimSpace(r[3])
		// NOT NULL is stored as a check constraint and is already part of
		// the column definition.
		if t == nil || cond == "" || notNullRe.MatchString(cond) {
			continue
		}
		cond = quotedName.ReplaceAllStringFunc(cond, func(q string) string {
			for _, c := range t.Cols {
				if q == `"`+c.Name+`"` {
					return qident(c.Target)
				}
			}
			return q
		})
		name := clip(pgName(r[2]))
		p.Post = append(p.Post, Stmt{Kind: "check constraint", Name: t.Schema + "." + t.Target + " " + name, Soft: true,
			SQL: "ALTER TABLE " + t.QName() + " ADD CONSTRAINT " + qident(name) + " CHECK (" + cond + ")"})
	}
	return nil
}

// loadForeignKeys translates the enabled foreign keys between migrated
// tables, with their ON DELETE rule.
func (p *Plan) loadForeignKeys(ctx context.Context, src oracle.Queryer, in string) error {
	rows, err := query(ctx, src, `SELECT k.owner, k.table_name, k.constraint_name, k.delete_rule, k.deferrable, k.deferred, r.owner, r.table_name,
			(SELECT LISTAGG(c.column_name, CHR(10)) WITHIN GROUP (ORDER BY c.position) FROM all_cons_columns c
			  WHERE c.owner = k.owner AND c.constraint_name = k.constraint_name),
			(SELECT LISTAGG(c.column_name, CHR(10)) WITHIN GROUP (ORDER BY c.position) FROM all_cons_columns c
			  WHERE c.owner = r.owner AND c.constraint_name = r.constraint_name)
		FROM all_constraints k JOIN all_constraints r ON r.owner = k.r_owner AND r.constraint_name = k.r_constraint_name
		WHERE k.owner `+in+` AND k.constraint_type = 'R' AND k.status = 'ENABLED' ORDER BY 1, 2, 3`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		t := p.Table(r[0], r[1])
		if t == nil {
			continue
		}
		parent := p.Table(r[6], r[7])
		if parent == nil {
			p.Notes = append(p.Notes, fmt.Sprintf("%s: foreign key %s refers to %s.%s, which is not being migrated, and was left out", t.Label(), r[2], r[6], r[7]))
			continue
		}
		cols, ok1 := t.columns(r[8])
		refs, ok2 := parent.columns(r[9])
		if !ok1 || !ok2 {
			continue
		}
		p.alignKeyTypes(t, parent, strings.Split(r[8], "\n"), strings.Split(r[9], "\n"))
		name := clip(pgName(r[2]))
		ddl := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)", t.QName(), qident(name), cols, parent.QName(), refs)
		if r[3] == "CASCADE" || r[3] == "SET NULL" {
			ddl += " ON DELETE " + r[3]
		}
		if r[4] == "DEFERRABLE" {
			ddl += " DEFERRABLE"
			if r[5] == "DEFERRED" {
				ddl += " INITIALLY DEFERRED"
			}
		}
		p.Final = append(p.Final, Stmt{Kind: "foreign key", Name: t.Schema + "." + t.Target + " " + name, Soft: true, SQL: ddl})
	}
	return nil
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
		if c == nil || ref == nil || c.Kind != kNumber || ref.Kind != kNumber || c.PGType == ref.PGType {
			continue
		}
		p.Notes = append(p.Notes, fmt.Sprintf("%s.%s: stored as %s instead of %s, the type of the key it refers to (%s.%s)",
			child.Label(), c.Name, ref.PGType, c.PGType, parent.Label(), ref.Name))
		c.PGType = ref.PGType
	}
}
