package codesql

import "strings"

// visitor is called for every statement and every expression of a tree.
// Either function may be nil.
type visitor struct {
	stmt func(stmt)
	expr func(expr)
}

func (v visitor) block(b *block) {
	for _, d := range b.decls {
		switch d := d.(type) {
		case *varDecl:
			v.e(d.def)
		case *cursorDecl:
			v.sql(d.query)
		}
	}
	v.stmts(b.body)
	for _, h := range b.handlers {
		v.stmts(h.body)
	}
}

func (v visitor) stmts(list []stmt) {
	for _, s := range list {
		v.s(s)
	}
}

func (v visitor) s(s stmt) {
	if v.stmt != nil {
		v.stmt(s)
	}
	switch s := s.(type) {
	case *blockStmt:
		v.block(s.b)
	case *assignStmt:
		v.e(s.target)
		v.e(s.value)
	case *ifStmt:
		for _, b := range s.branches {
			v.e(b.cond)
			v.stmts(b.body)
		}
		v.stmts(s.els)
	case *caseStmt:
		v.e(s.operand)
		for _, b := range s.branches {
			for _, x := range b.values {
				v.e(x)
			}
			v.stmts(b.body)
		}
		v.stmts(s.els)
	case *loopStmt:
		v.e(s.cond)
		v.e(s.lo)
		v.e(s.hi)
		v.e(s.step)
		v.sql(s.query)
		for _, a := range s.args {
			v.e(a)
		}
		v.stmts(s.body)
	case *exitStmt:
		v.e(s.when)
	case *returnStmt:
		v.e(s.value)
	case *raiseStmt:
		v.e(s.message)
		v.e(s.oraCode)
		for _, a := range s.args {
			v.e(a)
		}
	case *openStmt:
		for _, a := range s.args {
			v.e(a)
		}
	case *fetchStmt:
		for _, t := range s.into {
			v.e(t)
		}
	case *sqlExec:
		v.sql(s.sql)
	case *callStmt:
		for _, a := range s.args {
			v.e(a)
		}
	case *outputStmt:
		v.e(s.value)
	case *diagStmt:
		v.e(s.target)
	case *performStmt:
		v.e(s.value)
		v.sql(s.query)
	}
}

func (v visitor) sql(s *sqlStmt) {
	if s == nil {
		return
	}
	for _, t := range s.into {
		v.e(t)
	}
	for _, t := range s.tables {
		v.sql(t.sub)
	}
	v.parts(s.parts)
}

func (v visitor) parts(list []sqlPart) {
	for _, p := range list {
		switch {
		case p.e != nil:
			v.e(p.e)
		case p.sub != nil:
			v.sql(p.sub)
		case p.order != nil:
			v.e(p.order.e)
		case p.limit != nil:
			v.e(p.limit.count)
			v.e(p.limit.offset)
		}
	}
}

func (v visitor) e(e expr) {
	if e == nil {
		return
	}
	if v.expr != nil {
		v.expr(e)
	}
	switch e := e.(type) {
	case *callExpr:
		for _, a := range e.args {
			v.e(a)
		}
		v.parts(e.over)
		v.parts(e.withinOrder)
	case *binExpr:
		v.e(e.l)
		v.e(e.r)
	case *unExpr:
		v.e(e.x)
	case *parenExpr:
		for _, it := range e.items {
			v.e(it)
		}
	case *caseExpr:
		v.e(e.operand)
		for _, w := range e.whens {
			v.e(w.cond)
			v.e(w.result)
		}
		v.e(e.els)
	case *isExpr:
		v.e(e.x)
	case *betweenExpr:
		v.e(e.x)
		v.e(e.lo)
		v.e(e.hi)
	case *inExpr:
		v.e(e.x)
		for _, it := range e.list {
			v.e(it)
		}
		v.sql(e.sub)
	case *likeExpr:
		v.e(e.x)
		v.e(e.pattern)
		v.e(e.escape)
	case *existsExpr:
		v.sql(e.sub)
	case *subExpr:
		v.sql(e.sub)
	case *castExpr:
		v.e(e.x)
	case *extractExpr:
		v.e(e.x)
	case *intervalExpr:
		v.e(e.n)
	}
}

// references counts how often each name is mentioned in a list of
// statements, by the upper-case first part of the reference.
func references(list []stmt) map[string]int {
	counts := map[string]int{}
	visitor{expr: func(e expr) {
		if n, ok := e.(*nameExpr); ok {
			counts[strings.ToUpper(n.parts[0].name)]++
		}
	}}.stmts(list)
	return counts
}

// anyStmt reports whether a statement of the list, at any depth,
// satisfies the test.
func anyStmt(list []stmt, test func(stmt) bool) bool {
	found := false
	visitor{stmt: func(s stmt) {
		if test(s) {
			found = true
		}
	}}.stmts(list)
	return found
}

// anyExpr reports whether an expression anywhere in the block satisfies
// the test.
func anyExpr(b *block, test func(expr) bool) bool {
	found := false
	visitor{expr: func(e expr) {
		if test(e) {
			found = true
		}
	}}.block(b)
	return found
}

// changes reports whether a statement changes data or may do so: an
// INSERT, UPDATE or DELETE, or a call to a procedure.
func changes(s stmt) bool {
	switch s := s.(type) {
	case *sqlExec:
		return s.sql.verb != "SELECT"
	case *callStmt:
		return true
	}
	return false
}

// countStmts counts the statements of a list at every depth.
func countStmts(list []stmt) int {
	n := 0
	visitor{stmt: func(stmt) { n++ }}.stmts(list)
	return n
}

// singleRow reports whether a query gives exactly one row whatever the
// data: it computes an aggregate and does not group.
func singleRow(s *sqlStmt) bool {
	if !s.simple {
		return false
	}
	// A query of no table, or of Oracle's one-row DUAL, without a
	// condition.
	if !reads(s) {
		filtered := false
		for _, p := range s.parts {
			if p.kw == "WHERE" {
				filtered = true
			}
		}
		if !filtered {
			return true
		}
	}
	for _, c := range s.columns {
		aggregate := false
		visitor{expr: func(e expr) {
			call, ok := e.(*callExpr)
			if !ok || len(call.name) != 1 || call.over != nil {
				return
			}
			switch strings.ToUpper(call.name[0].name) {
			case "COUNT", "SUM", "MIN", "MAX", "AVG":
				aggregate = true
			}
		}}.e(c.e)
		if aggregate {
			return true
		}
	}
	return false
}
