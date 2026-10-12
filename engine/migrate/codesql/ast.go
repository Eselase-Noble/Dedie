package codesql

// The syntax tree is shared by the three source languages. PL/SQL,
// PL/pgSQL and MySQL's stored programs are close relatives: blocks with
// declarations, the same handful of control statements, cursors, and SQL
// statements in between. Each parser reads its own spelling into these
// nodes; where the languages differ in meaning rather than spelling, the
// emitters ask which language the tree came from.

// ident is one part of a name.
type ident struct {
	name   string
	quoted bool // written in quotes in the source, so its case is exact
}

// routine is a whole function, procedure or trigger.
type routine struct {
	kind    string // "function", "procedure" or "trigger"
	name    []ident
	params  []*param
	returns *dataType
	trigger *triggerHead
	body    *block

	deterministic bool   // the source promises the same result for the same arguments
	strict        bool   // PostgreSQL's STRICT: a NULL argument makes the result NULL without running the body
	sqlBody       bool   // a PostgreSQL function written in the SQL language
	conflict      string // PL/pgSQL's #variable_conflict setting, if the body has one
}

type param struct {
	name string
	mode string // "IN", "OUT" or "INOUT"
	typ  *dataType
	def  expr
	line int
}

// triggerHead is everything a trigger says before its body.
type triggerHead struct {
	name       []ident
	timing     string   // "BEFORE", "AFTER" or "INSTEAD OF"
	events     []string // "INSERT", "UPDATE", "DELETE"
	updateOf   []ident  // the columns of UPDATE OF
	table      []ident
	forEachRow bool
	when       expr
	// newName and oldName are what the body calls the two rows, in upper
	// case: NEW and OLD unless a REFERENCING clause renames them.
	newName, oldName string
	line             int
}

// block is BEGIN ... END with what belongs to it.
type block struct {
	label    string
	decls    []decl
	body     []stmt
	handlers []*handler
	line     int
}

// handler is one way a block deals with an error. PL/SQL and PL/pgSQL
// write them after EXCEPTION; MySQL declares them at the top of the block.
type handler struct {
	conds []condition
	body  []stmt
	// action is MySQL's choice between leaving the block when the
	// handler is done ("EXIT") and resuming after the statement that
	// failed ("CONTINUE"). The other two languages always leave.
	action string
	line   int
}

// condition names what a handler catches.
type condition struct {
	// kind is "others" (any error), "notfound" (MySQL's NOT FOUND),
	// "warning" (MySQL's SQLWARNING), "name", "sqlstate" or "code"
	// (a MySQL error number).
	kind  string
	value string
}

type decl interface{ declLine() int }

type varDecl struct {
	name     string
	typ      *dataType
	constant bool
	notNull  bool
	def      expr
	line     int
}

type cursorDecl struct {
	name   string
	params []*param
	query  *sqlStmt
	line   int
}

// exceptionDecl is a named error of the routine's own: PL/SQL's
// "name EXCEPTION", optionally tied to an error number by PRAGMA
// EXCEPTION_INIT, or MySQL's "name CONDITION FOR ...".
type exceptionDecl struct {
	name string
	// code is the Oracle error number, 0 when the exception has none.
	code int
	// cond is what a MySQL condition stands for.
	cond *condition
	line int
}

// unsupportedDecl is a declaration the converter has no translation for.
type unsupportedDecl struct {
	what string
	line int
}

func (d *varDecl) declLine() int         { return d.line }
func (d *cursorDecl) declLine() int      { return d.line }
func (d *exceptionDecl) declLine() int   { return d.line }
func (d *unsupportedDecl) declLine() int { return d.line }

type stmt interface{ stmtLine() int }

type (
	blockStmt struct{ b *block }

	assignStmt struct {
		target expr
		value  expr
		line   int
	}

	ifStmt struct {
		branches []ifBranch
		els      []stmt
		hasElse  bool
		line     int
	}

	// caseStmt is the CASE statement, not the expression.
	caseStmt struct {
		operand  expr // nil for the searched form
		branches []caseBranch
		els      []stmt
		hasElse  bool
		line     int
	}

	// loopStmt is every kind of loop. kind says which:
	//
	//	"loop"   LOOP ... END LOOP
	//	"while"  cond is tested before each round
	//	"repeat" cond is tested after each round and ends the loop when true (MySQL)
	//	"range"  variable runs from lo to hi
	//	"query"  variable is a record that takes each row of query
	//	"cursor" variable is a record that takes each row of the named cursor
	loopStmt struct {
		kind     string
		label    string
		cond     expr
		variable string
		lo, hi   expr
		step     expr
		reverse  bool
		query    *sqlStmt
		cursor   string
		args     []expr
		body     []stmt
		line     int
	}

	// exitStmt leaves a loop or a labelled block (EXIT, LEAVE); with
	// next set it starts the next round instead (CONTINUE, ITERATE).
	exitStmt struct {
		next  bool
		label string
		when  expr
		line  int
	}

	returnStmt struct {
		value expr
		line  int
	}

	nullStmt struct{ line int }

	// raiseStmt raises an error or passes on the one being handled.
	raiseStmt struct {
		reraise bool
		// name is a declared exception or a predefined one.
		name string
		// sqlstate and message describe an error made on the spot.
		sqlstate string
		message  expr
		// oraCode is the first argument of RAISE_APPLICATION_ERROR.
		oraCode expr
		// level is PL/pgSQL's: EXCEPTION, or NOTICE and its kind, which
		// only inform. format and args are its message template.
		level  string
		format string
		args   []expr
		hasFmt bool
		line   int
	}

	openStmt struct {
		cursor string
		args   []expr
		line   int
	}

	fetchStmt struct {
		cursor string
		into   []expr
		line   int
	}

	closeStmt struct {
		cursor string
		line   int
	}

	// sqlExec is an SQL statement run for its effect or for the values
	// it puts INTO variables.
	sqlExec struct {
		sql  *sqlStmt
		line int
	}

	callStmt struct {
		name     []ident
		args     []expr
		argNames []string // parallel to args; a name where the argument is written name => value
		line     int
	}

	// outputStmt prints a line for whoever is watching: PL/SQL's
	// DBMS_OUTPUT.PUT_LINE.
	outputStmt struct {
		value expr
		line  int
	}

	// txStmt is COMMIT or ROLLBACK.
	txStmt struct {
		verb string
		line int
	}

	// diagStmt reads the number of rows the last statement touched into
	// a variable: PL/pgSQL's GET DIAGNOSTICS v = ROW_COUNT.
	diagStmt struct {
		target expr
		line   int
	}

	// performStmt evaluates something and throws the result away
	// (PL/pgSQL's PERFORM).
	performStmt struct {
		value expr
		query *sqlStmt
		line  int
	}

	// unsupportedStmt is a statement the converter has no translation for.
	unsupportedStmt struct {
		what string
		line int
	}
)

type ifBranch struct {
	cond expr
	body []stmt
}

type caseBranch struct {
	values []expr // the values of a simple CASE, or the one condition of a searched CASE
	body   []stmt
}

func (s *blockStmt) stmtLine() int       { return s.b.line }
func (s *assignStmt) stmtLine() int      { return s.line }
func (s *ifStmt) stmtLine() int          { return s.line }
func (s *caseStmt) stmtLine() int        { return s.line }
func (s *loopStmt) stmtLine() int        { return s.line }
func (s *exitStmt) stmtLine() int        { return s.line }
func (s *returnStmt) stmtLine() int      { return s.line }
func (s *nullStmt) stmtLine() int        { return s.line }
func (s *raiseStmt) stmtLine() int       { return s.line }
func (s *openStmt) stmtLine() int        { return s.line }
func (s *fetchStmt) stmtLine() int       { return s.line }
func (s *closeStmt) stmtLine() int       { return s.line }
func (s *sqlExec) stmtLine() int         { return s.line }
func (s *callStmt) stmtLine() int        { return s.line }
func (s *outputStmt) stmtLine() int      { return s.line }
func (s *txStmt) stmtLine() int          { return s.line }
func (s *diagStmt) stmtLine() int        { return s.line }
func (s *performStmt) stmtLine() int     { return s.line }
func (s *unsupportedStmt) stmtLine() int { return s.line }

// Expressions.

type expr interface{ exprLine() int }

type (
	// litExpr is a literal. kind is "number", "string", "null", "bool",
	// "date", "timestamp" or "interval"; text is the value, and for an
	// interval unit holds what follows the string (DAY, YEAR TO MONTH).
	litExpr struct {
		kind string
		text string
		unit string
		line int
	}

	// nameExpr is a reference to a variable, a column, a record field or
	// a pseudo-column. bind marks Oracle's :NEW and :OLD.
	nameExpr struct {
		parts []ident
		bind  bool
		line  int
	}

	callExpr struct {
		name     []ident
		args     []expr
		argNames []string // parallel to args; a name where the argument is written name => value
		distinct bool
		star     bool // COUNT(*)
		// over is an analytic window, kept as the SQL it is.
		over []sqlPart
		// withinOrder is the ordering of an ordered aggregate
		// (LISTAGG ... WITHIN GROUP, string_agg(... ORDER BY)).
		withinOrder []sqlPart
		line        int
	}

	binExpr struct {
		op   string // upper case: "+", "||", "=", "AND", "DIV", "IS DISTINCT FROM", ...
		l, r expr
		line int
	}

	unExpr struct {
		op   string // "-", "+", "NOT"
		x    expr
		line int
	}

	// parenExpr is a parenthesised expression, or a row of them.
	parenExpr struct {
		items []expr
		line  int
	}

	caseExpr struct {
		operand expr
		whens   []caseWhen
		els     expr
		line    int
	}

	// isExpr is "x IS [NOT] NULL", and the same with TRUE or FALSE.
	isExpr struct {
		x    expr
		not  bool
		what string
		line int
	}

	betweenExpr struct {
		x, lo, hi expr
		not       bool
		line      int
	}

	inExpr struct {
		x    expr
		not  bool
		list []expr
		sub  *sqlStmt
		line int
	}

	likeExpr struct {
		x, pattern, escape expr
		not                bool
		insensitive        bool // PostgreSQL's ILIKE
		line               int
	}

	existsExpr struct {
		sub  *sqlStmt
		line int
	}

	// subExpr is a query in parentheses used as a value.
	subExpr struct {
		sub  *sqlStmt
		line int
	}

	castExpr struct {
		x    expr
		typ  *dataType
		line int
	}

	// attrExpr is a cursor attribute: c%FOUND, SQL%ROWCOUNT.
	attrExpr struct {
		cursor string // upper case; "SQL" for the implicit cursor
		attr   string // upper case
		line   int
	}

	extractExpr struct {
		field string // upper case
		x     expr
		line  int
	}

	// intervalExpr is MySQL's "INTERVAL n unit", which only exists next
	// to a date.
	intervalExpr struct {
		n    expr
		unit string
		line int
	}

	// starExpr is "*" or "t.*" in a select list.
	starExpr struct {
		qualifier []ident
		line      int
	}

	// rawExpr is something the expression parser recognised as valid but
	// has no translation for.
	rawExpr struct {
		what string
		line int
	}
)

type caseWhen struct{ cond, result expr }

func (e *litExpr) exprLine() int      { return e.line }
func (e *nameExpr) exprLine() int     { return e.line }
func (e *callExpr) exprLine() int     { return e.line }
func (e *binExpr) exprLine() int      { return e.line }
func (e *unExpr) exprLine() int       { return e.line }
func (e *parenExpr) exprLine() int    { return e.line }
func (e *caseExpr) exprLine() int     { return e.line }
func (e *isExpr) exprLine() int       { return e.line }
func (e *betweenExpr) exprLine() int  { return e.line }
func (e *inExpr) exprLine() int       { return e.line }
func (e *likeExpr) exprLine() int     { return e.line }
func (e *existsExpr) exprLine() int   { return e.line }
func (e *subExpr) exprLine() int      { return e.line }
func (e *castExpr) exprLine() int     { return e.line }
func (e *attrExpr) exprLine() int     { return e.line }
func (e *extractExpr) exprLine() int  { return e.line }
func (e *intervalExpr) exprLine() int { return e.line }
func (e *starExpr) exprLine() int     { return e.line }
func (e *rawExpr) exprLine() int      { return e.line }

// SQL statements.

// sqlStmt is one SELECT, INSERT, UPDATE or DELETE. It is not a full
// grammar of SQL: the statement is kept as the sequence of its keywords,
// expressions and table names, which is what the rewriting needs.
// Expressions are parsed, because that is where the dialects differ;
// the clause structure around them is nearly the same everywhere and is
// carried over word for word.
type sqlStmt struct {
	verb  string // "SELECT", "INSERT", "UPDATE", "DELETE"
	parts []sqlPart
	// into are the variables the result goes to; strict is PL/pgSQL's
	// INTO STRICT. The place they were written is marked by a part.
	into   []expr
	strict bool
	// tables are the tables the statement names at its own level, used
	// to tell columns from variables.
	tables []*tableRef
	// columns are the names and expressions of a SELECT's result, as far
	// as they can be told, for typing the record of a loop over it.
	columns []selectItem
	// simple says a SELECT has nothing after its WHERE clause: no
	// grouping, ordering, set operation or row limit.
	simple bool
	// plain says a SELECT has no set operation and no row limit, so one
	// can be added at its end.
	plain bool
	// returning says an INSERT, UPDATE or DELETE hands rows back.
	returning bool
	line      int
}

type selectItem struct {
	name string
	e    expr
}

type tableRef struct {
	name  []ident
	alias string
	sub   *sqlStmt
	line  int
}

// sqlPart is one piece of an SQL statement; exactly one field is set.
type sqlPart struct {
	kw     string    // keywords and punctuation, written as they are
	e      expr      // an expression
	table  *tableRef // a table in FROM, or the target of a change
	col    *ident    // a column named where no expression can stand: INSERT's list, SET's left side
	alias  *ident    // an alias of a select item
	into   bool      // where INTO and its variables were written
	sub    *sqlStmt  // a nested statement: the SELECT of an INSERT, the next arm of a UNION
	order  *orderKey // one key of an ORDER BY
	limit  *limitPart
	concat bool // glue: no space before the next part
}

// orderKey is one expression of an ORDER BY with what was said about it.
type orderKey struct {
	e     expr
	desc  bool
	nulls string // "FIRST", "LAST" or empty when not said
}

// limitPart is a row limit, however the source spells it.
type limitPart struct {
	count, offset expr
}
