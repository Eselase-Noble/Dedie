package codesql

import (
	"strings"
	"testing"
)

// conversion is one routine and what must come of it. A test names the
// status, the pieces of code that must appear in the result and the
// notes that must be given; pieces rather than whole routines, so that
// each test says which construct it is about.
type conversion struct {
	name   string
	src    string
	opt    Options
	schema string
	status string
	// code are fragments the statements must contain.
	code []string
	// absent are fragments they must not contain.
	absent []string
	// notes are fragments of notes that must be given.
	notes []string
}

func runConversions(t *testing.T, from, to string, cases []conversion) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Convert(from, to, Object{Schema: c.schema, Source: c.src}, c.opt)
			all := strings.Join(res.Statements, ";\n")
			notes := strings.Join(res.Notes, "\n")
			fail := func(format string, args ...any) {
				t.Helper()
				t.Errorf(format, args...)
				t.Logf("status %s\n%s\nnotes:\n%s", res.Status, all, notes)
			}
			if res.Status != c.status {
				fail("status is %s, want %s", res.Status, c.status)
				return
			}
			if res.Status == Unconverted && len(res.Statements) > 0 {
				fail("an unconverted routine comes with code")
			}
			if res.Status == Unconverted && len(res.Notes) == 0 {
				fail("an unconverted routine comes without a reason")
			}
			if res.Status == Review && len(res.Notes) == 0 {
				fail("a routine for review comes without a note")
			}
			for _, want := range c.code {
				if !strings.Contains(all, want) {
					fail("the code lacks %q", want)
					return
				}
			}
			for _, bad := range c.absent {
				if strings.Contains(all, bad) {
					fail("the code contains %q", bad)
					return
				}
			}
			for _, want := range c.notes {
				if !strings.Contains(notes, want) {
					fail("no note mentions %q", want)
					return
				}
			}
		})
	}
}

// golden compares a whole result with the text it must be.
func golden(t *testing.T, from, to string, obj Object, opt Options, status string, want ...string) {
	t.Helper()
	res := Convert(from, to, obj, opt)
	if res.Status != status {
		t.Fatalf("status is %s, want %s\nnotes: %s", res.Status, status, strings.Join(res.Notes, "\n"))
	}
	if len(res.Statements) != len(want) {
		t.Fatalf("%d statements, want %d:\n%s", len(res.Statements), len(want), strings.Join(res.Statements, ";\n"))
	}
	for i := range want {
		if got, w := res.Statements[i], strings.TrimSpace(want[i]); got != w {
			t.Errorf("statement %d is\n%s\nwant\n%s", i+1, got, w)
		}
	}
}

func TestUnsupportedRoute(t *testing.T) {
	for _, pair := range [][2]string{{"postgres", "oracle"}, {"mysql", "oracle"}, {"oracle", "oracle"}, {"mysql", "mysql"}, {"sqlite", "postgres"}} {
		res := Convert(pair[0], pair[1], Object{Source: "x"}, Options{})
		if res.Status != Unconverted || len(res.Notes) != 1 || len(res.Statements) != 0 {
			t.Errorf("%s to %s: %+v", pair[0], pair[1], res)
		}
	}
}

func TestLex(t *testing.T) {
	kinds := func(d Dialect, s string) string {
		toks, err := lex(d, s)
		if err != nil {
			return "error: " + err.Error()
		}
		var out []string
		for _, tk := range toks {
			out = append(out, map[tokenKind]string{tWord: "w", tQuoted: "q", tString: "s", tNumber: "n", tOp: "o", tBind: "b", tParam: "p", tUserVar: "u"}[tk.kind]+":"+tk.text)
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		d    Dialect
		in   string
		want string
	}{
		{Oracle, `x := q'[it's]' || 'a''b' -- c`, `w:x o::= s:it's o:|| s:a'b`},
		{Oracle, `FOR i IN 1..10 LOOP :NEW.a := 1.5e3; /* x */ END`, `w:FOR w:i w:IN n:1 o:.. n:10 w:LOOP b:NEW o:. w:a o::= n:1.5e3 o:; w:END`},
		{Oracle, `"Mixed Case"."X" <<lbl>> a => b`, `q:Mixed Case o:. q:X o:<< w:lbl o:>> w:a o:=> w:b`},
		{Postgres, `$body$ a 'b' $body$ || $$x$$ || E'a\nb' || $1::int`, "s: a 'b'  o:|| s:x o:|| s:a\nb o:|| p:1 o::: w:int"},
		{MySQL, "`a``b` = \"x\\\"y\" # c\n-- d\n @v := a--1", "q:a`b o:= s:x\"y u:v o::= w:a o:- o:- n:1"},
		{MySQL, `'50\%' 'tab\tx'`, "s:50\\% s:tab\tx"},
		{Oracle, `'never closed`, `error: line 1: a string literal is never closed`},
		{MySQL, `/*!50003 CREATE */`, `error: line 1: a version-specific comment (/*! ... */) holds code`},
	} {
		if got := kinds(c.d, c.in); got != c.want {
			t.Errorf("%s: %s\n got %q\nwant %q", c.d, c.in, got, c.want)
		}
	}
	toks, _ := lex(Oracle, "a\n/* two\nlines */ b\n'x\ny' c")
	if toks[1].line != 3 || toks[3].line != 5 {
		t.Errorf("lines are %d and %d, want 3 and 5", toks[1].line, toks[3].line)
	}
}
