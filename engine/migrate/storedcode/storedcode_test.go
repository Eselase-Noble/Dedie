package storedcode

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dedie/engine/migrate"
	"dedie/engine/migrate/codesql"
)

func TestCreatedReadsWhatAStatementCreates(t *testing.T) {
	cases := []struct {
		to, sql string
		want    Created
	}{
		{"postgres", "CREATE FUNCTION public.fee(amount numeric) RETURNS numeric\nLANGUAGE plpgsql\nAS $body$ BEGIN RETURN 1; END; $body$",
			Created{Kind: "function", Name: "public.fee", Quoted: "public.fee", Drop: "DROP ROUTINE IF EXISTS public.fee"}},
		{"postgres", `CREATE PROCEDURE "My Schema"."do it"(IN a integer)` + "\nLANGUAGE plpgsql",
			Created{Kind: "procedure", Name: "My Schema.do it", Quoted: `"My Schema"."do it"`, Drop: `DROP ROUTINE IF EXISTS "My Schema"."do it"`}},
		{"postgres", "CREATE FUNCTION shop.stamp_fn() RETURNS trigger\nLANGUAGE plpgsql",
			Created{Kind: "function", Name: "shop.stamp_fn", Quoted: "shop.stamp_fn", Drop: "DROP FUNCTION IF EXISTS shop.stamp_fn()"}},
		{"postgres", `CREATE TRIGGER stamp BEFORE INSERT OR UPDATE OF "a", b ON shop."Led ON ger"` + "\nFOR EACH ROW\nEXECUTE FUNCTION shop.stamp_fn()",
			Created{Kind: "trigger", Name: "stamp", Quoted: "stamp", Table: `shop."Led ON ger"`, Drop: `DROP TRIGGER IF EXISTS stamp ON shop."Led ON ger"`}},
		{"mysql", "CREATE TRIGGER `stamp_ins` BEFORE INSERT ON `ledger`\nFOR EACH ROW\nBEGIN END",
			Created{Kind: "trigger", Name: "stamp_ins", Quoted: "`stamp_ins`", Table: "`ledger`", Drop: "DROP TRIGGER IF EXISTS `stamp_ins`"}},
		{"mysql", "CREATE FUNCTION fee(amount DECIMAL(12,2)) RETURNS DECIMAL(12,2)\n    DETERMINISTIC\nBEGIN RETURN 1; END",
			Created{Kind: "function", Name: "fee", Quoted: "fee", Drop: "DROP FUNCTION IF EXISTS fee"}},
	}
	for _, c := range cases {
		got, ok := created(c.to, c.sql)
		c.want.SQL = c.sql
		if !ok || got != c.want {
			t.Errorf("%s:\n got %+v (%v)\nwant %+v", strings.SplitN(c.sql, "\n", 2)[0], got, ok, c.want)
		}
	}
	for _, sql := range []string{"CREATE TABLE t (a int)", "CREATE FUNCTION", "CREATE TRIGGER x BEFORE INSERT\nON t", "ALTER TABLE t ENABLE TRIGGER x"} {
		if _, ok := created("postgres", sql); ok {
			t.Errorf("%q was taken for a statement that creates a routine", sql)
		}
	}
}

const (
	feeSource   = "FUNCTION fee(amount NUMBER) RETURN NUMBER IS\nBEGIN\n  RETURN ROUND(amount * 0.015, 2);\nEND;"
	ratioSource = "FUNCTION ratio(a NUMBER, b NUMBER) RETURN NUMBER IS\nBEGIN\n  RETURN a / b;\nEND;"
	wipeSource  = "PROCEDURE wipe(tab VARCHAR2) IS\nBEGIN\n  EXECUTE IMMEDIATE 'DELETE FROM ' || tab;\nEND;"
	stampSource = "TRIGGER stamp BEFORE INSERT OR UPDATE ON ledger FOR EACH ROW\nBEGIN\n  :NEW.entries := NVL(:NEW.entries, 0) + 1;\nEND;"
)

func fixture() []*Object {
	object := func(kind, name, source string) *Object {
		return &Object{Kind: kind, Schema: "SHOP", Name: name, Label: "SHOP." + name, Source: source, Definition: "CREATE OR REPLACE " + source, End: "/", Why: "it is written in PL/SQL"}
	}
	stamp := object("trigger", "STAMP", stampSource)
	stamp.Table = "LEDGER"
	off := object("trigger", "OFF", stampSource)
	off.Disabled = true
	// The trigger is listed first, to see that it is created last.
	return []*Object{stamp, object("function", "FEE", feeSource), object("function", "RATIO", ratioSource), object("procedure", "WIPE", wipeSource), off,
		{Kind: "package", Schema: "SHOP", Name: "PKG", Label: "SHOP.PKG", Definition: "CREATE OR REPLACE PACKAGE pkg AS END;", End: "/", Why: "packages are not converted"}}
}

var ledger = codesql.Options{Tables: []codesql.Table{{Schema: "SHOP", Name: "LEDGER", Columns: []codesql.Column{{Name: "ID", Type: "NUMBER(9)"}, {Name: "ENTRIES", Type: "NUMBER(9)"}}}}}

func plan(objects []*Object) string {
	var out []string
	for _, o := range objects {
		s := o.Name + "=" + o.Status
		if o.Create {
			s += "+"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestPlanFollowsTheSetting(t *testing.T) {
	for mode, want := range map[string]string{
		migrate.CodeConverted: "STAMP=converted+ FEE=converted+ RATIO=review WIPE=unconverted OFF=unconverted PKG=unconverted",
		migrate.CodeReview:    "STAMP=converted+ FEE=converted+ RATIO=review+ WIPE=unconverted OFF=unconverted PKG=unconverted",
		migrate.CodeOff:       "STAMP= FEE= RATIO= WIPE= OFF= PKG=",
	} {
		objects := fixture()
		Plan("oracle", "postgres", objects, ledger, mode)
		if got := plan(objects); got != want {
			t.Errorf("%s:\n got %s\nwant %s", mode, got, want)
		}
		for _, o := range objects {
			if !o.Create && o.Reason == "" {
				t.Errorf("%s: %s is not created and no reason is given", mode, o.Name)
			}
		}
	}
	objects := fixture()
	Plan("oracle", "postgres", objects, ledger, migrate.CodeConverted)
	var order []string
	for _, o := range Planned(objects) {
		order = append(order, o.Name)
	}
	if got := strings.Join(order, " "); got != "FEE STAMP" {
		t.Errorf("order of creation %q, want routines before triggers", got)
	}
	if len(objects[0].Creates) != 2 || objects[0].Creates[0].Kind != "function" || objects[0].Creates[1].Kind != "trigger" {
		t.Errorf("the trigger creates %+v", objects[0].Creates)
	}
	if r := objects[4].Reason; !strings.Contains(r, "disabled on the source") {
		t.Errorf("disabled trigger: %s", r)
	}
	status, detail := Describe("oracle", "postgres", objects[:5], migrate.CodeConverted, "")
	for _, part := range []string{"1 function(s), 0 procedure(s) and 1 trigger(s) are converted and created at the end", "1 need review and are not created", "function SHOP.RATIO",
		"2 cannot be converted", "procedure SHOP.WIPE", "What the conversion takes for granted:"} {
		if !strings.Contains(detail, part) {
			t.Errorf("described without %q:\n%s", part, detail)
		}
	}
	if status != "warn" {
		t.Errorf("status %s", status)
	}
}

// fakeTarget takes statements and remembers what exists.
type fakeTarget struct {
	exists map[string]bool // by the first line of the statement that creates it
	refuse string          // a statement with this in its first line is refused
	ran    []string
	events []string
}

func (f *fakeTarget) target(previous []Outcome, save func([]Outcome)) Target {
	return Target{Previous: previous, Save: save, Event: func(level, msg string) { f.events = append(f.events, level+": "+msg) },
		Exec: func(ctx context.Context, sql string) (bool, error) {
			head := strings.SplitN(sql, "\n", 2)[0]
			f.ran = append(f.ran, head)
			switch {
			case strings.HasPrefix(head, "DROP "):
				return false, nil
			case f.refuse != "" && strings.Contains(head, f.refuse):
				return false, errors.New("relation does not exist")
			case f.exists[head]:
				return true, nil
			}
			f.exists[head] = true
			return false, nil
		}}
}

func results(outcomes []Outcome) string {
	var out []string
	for _, o := range outcomes {
		s := o.Name + "=" + o.Status
		if o.Review {
			s += " (review)"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

func TestFinishTakesBackWhatTheTargetRefuses(t *testing.T) {
	objects := fixture()
	Plan("oracle", "postgres", objects, ledger, migrate.CodeReview)
	f := &fakeTarget{exists: map[string]bool{}, refuse: "CREATE TRIGGER"}
	var saved [][]Outcome
	outcomes, err := f.target(nil, func(o []Outcome) { saved = append(saved, append([]Outcome(nil), o...)) }).Finish(context.Background(), objects)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := results(outcomes), "SHOP.FEE=created; SHOP.RATIO=created (review); SHOP.STAMP=not created"; got != want {
		t.Errorf("outcomes %q, want %q", got, want)
	}
	if !strings.Contains(outcomes[2].Detail, "did not accept the converted trigger: relation does not exist") {
		t.Errorf("detail: %s", outcomes[2].Detail)
	}
	// The trigger's function was created and then dropped again.
	if last := f.ran[len(f.ran)-1]; last != "DROP FUNCTION IF EXISTS shop.stamp_fn()" {
		t.Errorf("ran %q", f.ran)
	}
	// Each object was on record as begun before its first statement ran.
	begun := 0
	for _, list := range saved {
		if list[len(list)-1].Status == creating {
			begun++
		}
	}
	if begun != 3 {
		t.Errorf("%d object(s) recorded before they were created, want 3", begun)
	}

	var s Summary
	s.Count(objects, outcomes)
	if s != (Summary{Created: 1, Review: 1, Left: 4}) {
		t.Errorf("summary %+v", s)
	}
	left := Leftovers("postgres", objects, outcomes)
	var listed []string
	for _, l := range left {
		listed = append(listed, l.Name+" ["+l.Status+"]")
	}
	if got, want := strings.Join(listed, "; "), "SHOP.STAMP [not created]; SHOP.RATIO [created, needs review]; SHOP.WIPE [unconverted]; SHOP.OFF [unconverted]; SHOP.PKG [unconverted]"; got != want {
		t.Errorf("left:\n got %s\nwant %s", got, want)
	}
	if left[0].Draft == "" || !strings.HasSuffix(left[1].Draft, "$body$;") || left[2].Draft != "" || len(left[1].Notes) == 0 {
		t.Errorf("drafts and notes: %+v", left[:3])
	}
}

// TestFinishAgain is a job that was interrupted while it created its code
// and is run again: what it finished is not run twice, the object it had
// begun is completed although half of it exists, and a routine that
// appeared under a name the job never reached is somebody else's.
func TestFinishAgain(t *testing.T) {
	objects := fixture()
	Plan("oracle", "postgres", objects, ledger, migrate.CodeReview)
	f := &fakeTarget{exists: map[string]bool{
		"CREATE FUNCTION shop.fee(amount numeric) RETURNS numeric":         true,
		"CREATE FUNCTION shop.ratio(a numeric, b numeric) RETURNS numeric": true,
		"CREATE FUNCTION shop.stamp_fn() RETURNS trigger":                  true,
	}}
	previous := []Outcome{{Kind: "function", Name: "SHOP.FEE", Status: Done}, {Kind: "trigger", Name: "SHOP.STAMP", Status: creating}}
	outcomes, err := f.target(previous, nil).Finish(context.Background(), objects)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := results(outcomes), "SHOP.FEE=created; SHOP.RATIO=not created (review); SHOP.STAMP=created"; got != want {
		t.Errorf("outcomes %q, want %q", got, want)
	}
	if !strings.Contains(outcomes[1].Detail, "since the job started, which was left as it is") {
		t.Errorf("the routine that is not the job's: %s", outcomes[1].Detail)
	}
	for _, ran := range f.ran {
		if strings.Contains(ran, "shop.fee") || strings.HasPrefix(ran, "DROP ") {
			t.Errorf("ran %q", ran)
		}
	}
}
