package codesql

import (
	"fmt"
	"strings"
)

// sql writes a nested SQL statement: a subquery, the query of a cursor.
func (g *gen) sql(s *sqlStmt) string {
	text, _ := g.statement(s, "")
	return text
}

// sqlTyped writes a subquery used as a value and reports the type of
// its one column.
func (g *gen) sqlTyped(s *sqlStmt, t *typ) string {
	text, cols := g.statement(s, "")
	if len(cols) == 1 {
		*t = cols[0]
	}
	return text
}

// statement writes one SQL statement. into is what stands where the
// source wrote INTO and its variables, already written for the target;
// it is empty for a statement that has none. The second result is the
// type of each column of a SELECT's result, as far as it is known.
func (g *gen) statement(s *sqlStmt, into string) (string, []typ) {
	// The tables first: names in every clause may be their columns.
	var level []*boundTable
	subs := map[*tableRef]string{}
	for _, ref := range s.tables {
		b := &boundTable{ref: ref}
		if ref.sub != nil {
			text, cols := g.statement(ref.sub, "")
			subs[ref] = text
			b.sub = map[string]typ{}
			for i, c := range ref.sub.columns {
				if c.name != "" && i < len(cols) {
					b.sub[strings.ToUpper(c.name)] = cols[i]
				}
			}
		} else {
			b.table = g.findTable(ref.name)
		}
		level = append(level, b)
	}
	g.bound = append(g.bound, level)
	defer func() { g.bound = g.bound[:len(g.bound)-1] }()

	dual := g.from == Oracle && g.to == Postgres && len(s.tables) == 1 && isDual(s.tables[0])
	cols := make([]typ, len(s.columns))
	for i := range cols {
		cols[i] = tUnknown
	}
	var out []string
	for i := 0; i < len(s.parts); i++ {
		p := s.parts[i]
		switch {
		case p.kw == "FROM" && dual:
			// PostgreSQL selects without a table.
			i++
		case p.kw != "":
			out = append(out, p.kw)
		case p.e != nil && p.cond:
			out = append(out, g.cond(p.e))
		case p.e != nil:
			text, t := g.value(p.e)
			for k := range s.columns {
				if s.columns[k].e == p.e {
					cols[k] = t
				}
			}
			out = append(out, text)
		case p.table != nil:
			out = append(out, g.tablePart(p.table, subs))
		case p.col != nil:
			out = append(out, g.columnPart(p.col, level))
		case p.alias != nil:
			out = append(out, g.quote(g.fold(*p.alias)))
		case p.into:
			if into != "" {
				out = append(out, into)
			}
		case p.sub != nil:
			text, _ := g.statement(p.sub, "")
			out = append(out, text)
		case p.order != nil:
			out = append(out, g.orderPart(p.order))
		case p.limit != nil:
			out = append(out, g.limitPart(p.limit))
		}
	}
	return tidy(strings.Join(out, " ")), cols
}

// parts writes a run of SQL parts that is not a statement of its own:
// the window of an analytic function.
func (g *gen) parts(list []sqlPart) string {
	var out []string
	for _, p := range list {
		switch {
		case p.kw != "":
			out = append(out, p.kw)
		case p.e != nil:
			text, _ := g.value(p.e)
			out = append(out, text)
		case p.order != nil:
			out = append(out, g.orderPart(p.order))
		}
	}
	return tidy(strings.Join(out, " "))
}

// tidy removes the spaces a plain join puts around punctuation.
func tidy(s string) string {
	s = strings.ReplaceAll(s, " ,", ",")
	s = strings.ReplaceAll(s, "( ", "(")
	s = strings.ReplaceAll(s, " )", ")")
	return s
}

func isDual(ref *tableRef) bool {
	return ref.sub == nil && len(ref.name) >= 1 && strings.EqualFold(ref.name[len(ref.name)-1].name, "DUAL") &&
		(len(ref.name) == 1 || strings.EqualFold(ref.name[0].name, "SYS"))
}

func (g *gen) tablePart(ref *tableRef, subs map[*tableRef]string) string {
	alias := ""
	if ref.alias != "" {
		alias = " " + g.quote(g.fold(ident{name: ref.alias}))
	}
	if ref.sub != nil {
		if alias == "" {
			// A subquery in FROM needs a name on both targets.
			g.labels++
			alias = fmt.Sprintf(" sq%d", g.labels)
		}
		return "(" + subs[ref] + ")" + alias
	}
	if isDual(ref) {
		return "dual" + alias
	}
	if len(ref.name) > 2 {
		g.refuse(ref.line, "the table name %s has more than a schema in front of it", ref.name[len(ref.name)-1].name)
	}
	return g.tableName(ref.name) + alias
}

// columnPart writes a column named where only a column can stand. Such a
// column belongs to the table the statement changes, which is the first.
func (g *gen) columnPart(col *ident, level []*boundTable) string {
	if len(level) > 0 && level[0].table != nil {
		if c := level[0].table.column(g, *col); c != nil {
			return g.columnName(c)
		}
	}
	if v := g.lookup(col.name); v != nil && v.kind == "cursor" {
		// The cursor of WHERE CURRENT OF.
		if g.to == MySQL {
			g.refuse(v.cursor.line, "WHERE CURRENT OF %s: MySQL cannot change the row a cursor is on", v.name)
		}
		return v.out
	}
	return g.ident(*col)
}

// orderPart writes one key of an ORDER BY so that NULLs sort where the
// source put them. Oracle and PostgreSQL treat NULL as larger than any
// value, MySQL as smaller.
func (g *gen) orderPart(k *orderKey) string {
	text, t := g.value(k.e)
	if g.from == MySQL && !g.opt.ExactText && t.class == cStr {
		g.note(k.e.exprLine(), "rows are ordered by text; MySQL's default collations ignore case, and PostgreSQL orders by its own collation")
	}
	dir := ""
	if k.desc {
		dir = " DESC"
	}
	// first says whether the source puts NULLs first for this key.
	first := k.desc
	if g.from == MySQL {
		first = !k.desc
	}
	switch k.nulls {
	case "FIRST":
		first = true
	case "LAST":
		first = false
	}
	if g.to == Postgres {
		switch {
		case first && !k.desc:
			return text + " NULLS FIRST"
		case !first && k.desc:
			return text + " DESC NULLS LAST"
		}
		return text + dir
	}
	if _, position := intLit(k.e); position {
		g.note(k.e.exprLine(), "rows are ordered by a column position; if that column can be NULL, MySQL sorts the NULLs at the other end from %s", g.from)
		return text + dir
	}
	if first == !k.desc {
		// MySQL's own order.
		return text + dir
	}
	if first {
		return wrap(text, k.e) + " IS NOT NULL, " + text + dir
	}
	return wrap(text, k.e) + " IS NULL, " + text + dir
}

func (g *gen) limitPart(l *limitPart) string {
	var count, offset string
	if l.count != nil {
		count, _ = g.value(l.count)
	}
	if l.offset != nil {
		offset, _ = g.value(l.offset)
	}
	switch {
	case count != "" && offset != "":
		return "LIMIT " + count + " OFFSET " + offset
	case count != "":
		return "LIMIT " + count
	case g.to == MySQL:
		// MySQL has no OFFSET without a LIMIT.
		return "LIMIT 18446744073709551615 OFFSET " + offset
	}
	return "OFFSET " + offset
}

// reads reports whether a statement, or anything nested in it, reads a
// table.
func reads(s *sqlStmt) bool {
	for _, t := range s.tables {
		if !isDual(t) {
			return true
		}
	}
	return false
}
