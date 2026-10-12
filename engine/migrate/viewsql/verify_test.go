package viewsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func ptr(s string) *string { return &s }

func row(values ...string) []*string {
	out := make([]*string, len(values))
	for i, v := range values {
		if v != "NULL" {
			out[i] = ptr(v)
		}
	}
	return out
}

func TestCompare(t *testing.T) {
	src := []Column{{"ID", Numeric}, {"NAME", Text}, {"BORN", Temporal}}
	dst := []Column{{"id", Numeric}, {"name", Text}, {"born", Temporal}}
	names := []string{"id", "name", "born"}
	base := row("3", "3", "6", "2", "9", "3", "NULL")
	cases := []struct {
		name   string
		dst    []Column
		b      []*string
		reason string
	}{
		{"equal", dst, row("3", "3", "6", "2", "9", "3", "NULL"), ""},
		{"totals written differently", dst, row("3", "3", "6.000", "2", "9", "3", "NULL"), ""},
		{"rows", dst, row("2", "3", "6", "2", "9", "3", "NULL"), "3 row(s) on the source and 2 on the target"},
		{"non-null count", dst, row("3", "3", "6", "3", "9", "3", "NULL"), "column NAME has 2 value(s) that are not NULL on the source and 3 on the target"},
		{"sum", dst, row("3", "3", "5", "2", "9", "3", "NULL"), "column ID add up to 6 on the source and 5 on the target"},
		{"text length", dst, row("3", "3", "6", "2", "10", "3", "NULL"), "column NAME is 9 character(s) long in all on the source and 10 on the target"},
		{"column count", dst[:2], row("3", "3", "6", "2", "9"), "3 column(s) on the source and 2 on the target"},
		{"column name", []Column{{"id", Numeric}, {"label", Text}, {"born", Temporal}}, base, "column 2 is called NAME on the source and label on the target"},
		{"a number turned into text", []Column{{"id", Text}, {"name", Text}, {"born", Temporal}}, base, "column ID is a number on the source and text on the target"},
		{"a type that is only counted", []Column{{"id", Numeric}, {"name", Other}, {"born", Temporal}}, row("3", "3", "6", "2", "NULL", "3", "NULL"), ""},
		{"a number turned into something else", []Column{{"id", Other}, {"name", Text}, {"born", Temporal}}, base, "column ID is a number on the source and of another type on the target"},
		{"a date turned into a number", []Column{{"id", Numeric}, {"name", Text}, {"born", Numeric}}, base, "column BORN is a date or time on the source and a number on the target"},
	}
	for _, c := range cases {
		got := Compare(names, src, c.dst, base, c.b, false)
		if (c.reason == "") != (got == "") || !strings.Contains(got, c.reason) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.reason)
		}
	}
	if got := Compare([]string{"ID"}, src[:1], []Column{{"id", Numeric}}, row("0", "0", "NULL"), row("0", "0", "NULL"), true); got != "" {
		t.Errorf("names that differ only in case on a target that folds them: %q", got)
	}
}

func TestSameTotal(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"10", "10.0000", true},
		{".5", "0.5", true},
		{"1E+3", "1000", true},
		{"NULL", "0", true},
		{"0.33333333333333333333", "0.3333333333333333333333333333333333333333", true},
		{"0.3333", "0.33333333", false}, // four decimal places is not the same answer
		{"2", "2.5", false},
		{"100000000000000000001", "100000000000000000002", true},
		{"NaN", "NaN", true},
		{"NaN", "1", false},
	}
	for _, c := range cases {
		if got := sameTotal(row(c.a)[0], row(c.b)[0], exactTolerance); got != c.want {
			t.Errorf("%s and %s: same is %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestSummarySQL(t *testing.T) {
	cols := []Column{{"n", Numeric}, {"t", Text}, {"d", Temporal}}
	for d, want := range map[Dialect]string{
		MySQL:    "SELECT CAST(COUNT(*) AS CHAR), CAST(COUNT(CASE WHEN `n` IS NOT NULL THEN 1 END) AS CHAR), CAST(SUM(`n`) AS CHAR), CAST(COUNT(CASE WHEN `t` IS NOT NULL THEN 1 END) AS CHAR), CAST(SUM(CHAR_LENGTH(RTRIM(`t`))) AS CHAR), CAST(COUNT(CASE WHEN `d` IS NOT NULL THEN 1 END) AS CHAR), NULL FROM `v`",
		Postgres: `SELECT (COUNT(*))::text, (COUNT(CASE WHEN "n" IS NOT NULL THEN 1 END))::text, (SUM("n"))::text, (COUNT(CASE WHEN "t" IS NOT NULL THEN 1 END))::text, (SUM(length(rtrim("t"::text))))::text, (COUNT(CASE WHEN "d" IS NOT NULL THEN 1 END))::text, NULL::text FROM "v"`,
	} {
		if got := SummarySQL(d, Quote(d, "v"), cols); got != want {
			t.Errorf("%s:\n  got: %s\n want: %s", d, got, want)
		}
	}
}

// fakeDB answers the checker's questions from a table of rows per view.
type fakeDB struct {
	created, dropped []string
	refuse, there    map[string]bool
	source, target   map[string][]*string
	slow             map[string]bool
	events           []string
}

func (f *fakeDB) checker() Checker {
	relation := func(query string) string { return query[strings.LastIndex(query, " FROM ")+6:] }
	return Checker{From: Oracle, To: Postgres, Ident: lowered, Timeout: 50 * time.Millisecond,
		SourceRelations: func(v *View) []string { return []string{v.Name + " AS OF SCN 1", v.Name} },
		SourceRow: func(ctx context.Context, query string) ([]*string, error) {
			name := relation(query)
			if strings.Contains(name, " AS OF ") {
				return nil, errors.New("ORA-01555: snapshot too old")
			}
			if f.slow[name] {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return f.source[name], nil
		},
		TargetRow: func(ctx context.Context, query string) ([]*string, error) {
			return f.target[strings.Trim(relation(query), `"`)], nil
		},
		Create: func(ctx context.Context, v *View) (bool, error) {
			if f.refuse[v.Name] {
				return false, errors.New("syntax error")
			}
			if f.there[v.Name] {
				return true, nil
			}
			f.created = append(f.created, v.Name)
			return false, nil
		},
		Drop: func(ctx context.Context, v *View) error { f.dropped = append(f.dropped, v.Name); return nil },
		TargetColumns: func(ctx context.Context, v *View) ([]Column, error) {
			return []Column{{"n", Numeric}}, nil
		},
		Event: func(level, msg string) { f.events = append(f.events, level+": "+msg) },
	}
}

func TestFinish(t *testing.T) {
	naming := Naming{From: Oracle, To: Postgres, Ident: lowered,
		Table: func(schema, name string) (Relation, bool) {
			return Relation{Quoted: `"t"`, Bare: "t"}, name == "T"
		},
		View: func(schema, name string) (string, Relation, string) {
			return lowered(name), Relation{Quoted: Quote(Postgres, lowered(name)), Bare: lowered(name)}, ""
		}}
	view := func(name, def string) *View {
		return &View{Schema: "S", Name: name, Columns: []Column{{"N", Numeric}}, Definition: def}
	}
	// ON_GOOD is listed before the view it reads, to see that Plan puts
	// it after; ON_BAD reads a view that fails verification.
	views := Plan([]*View{
		view("ON_GOOD", "select n from good"), view("GOOD", "select n from t"), view("BAD", "select n / 2 n from t"),
		view("ON_BAD", "select n from bad"), view("REFUSED", "select n from t"), view("SLOW", "select n from t"),
		view("UNTRANSLATED", "select rownum n from t"), view("ON_UNTRANSLATED", "select n from untranslated"),
	}, naming)

	var order, left []string
	for _, v := range views {
		if v.SQL != "" {
			order = append(order, v.Name)
		} else {
			left = append(left, v.Name+": "+v.Reason)
		}
	}
	if got, want := strings.Join(order, " "), "GOOD ON_GOOD BAD ON_BAD REFUSED SLOW"; got != want {
		t.Errorf("order of creation %q, want %q", got, want)
	}
	if got, want := strings.Join(left, "; "), "UNTRANSLATED: it uses ROWNUM; ON_UNTRANSLATED: it reads the view S.UNTRANSLATED, which is left out"; got != want {
		t.Errorf("left out %q, want %q", got, want)
	}
	if got, want := views[0].SQL, `CREATE VIEW "good" ("n") AS SELECT "n" FROM "t"`; got != want {
		t.Errorf("statement %q, want %q", got, want)
	}
	status, detail := Describe(views)
	if status != "warn" || !strings.Contains(detail, "6 view(s) are translated") || !strings.Contains(detail, "S.UNTRANSLATED: it uses ROWNUM") {
		t.Errorf("described as %s: %s", status, detail)
	}

	same := row("2", "2", "30")
	db := &fakeDB{refuse: map[string]bool{"REFUSED": true}, slow: map[string]bool{"SLOW": true},
		source: map[string][]*string{"GOOD": same, "ON_GOOD": same, "BAD": row("2", "2", "15")},
		target: map[string][]*string{"good": same, "on_good": same, "bad": row("2", "2", "14"), "slow": same}}
	results, err := db.checker().Finish(context.Background(), views)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range results {
		got = append(got, r.Name+"="+r.Status)
	}
	want := "S.GOOD=verified S.ON_GOOD=verified S.BAD=dropped S.ON_BAD=not created S.REFUSED=not created S.SLOW=not verified"
	if strings.Join(got, " ") != want {
		t.Errorf("results %q, want %q", got, want)
	}
	for i, wantDetail := range map[int]string{2: "add up to 15 on the source and 14 on the target", 3: "reads the view S.BAD, which was not kept",
		4: "did not accept the translated definition: syntax error", 5: "on the source took longer than 50ms"} {
		if !strings.Contains(results[i].Detail, wantDetail) {
			t.Errorf("%s: detail %q, want it to say %q", results[i].Name, results[i].Detail, wantDetail)
		}
	}
	// Only the view that was created and then failed is dropped: the one
	// the target refused and the one that was never tried are not there.
	if got := strings.Join(db.dropped, " "); got != "BAD" {
		t.Errorf("dropped %q, want only BAD", got)
	}
	if got := strings.Join(db.created, " "); got != "GOOD ON_GOOD BAD SLOW" {
		t.Errorf("created %q", got)
	}
}

// TestFinishAgain is a job that was interrupted while it created its views
// and is run again: what it finished stays finished, the view it had
// begun is taken up, and a view of the same name as one it never reached
// is somebody else's and is left alone.
func TestFinishAgain(t *testing.T) {
	naming := Naming{From: Oracle, To: Postgres, Ident: lowered,
		Table: func(schema, name string) (Relation, bool) { return Relation{Quoted: `"t"`, Bare: "t"}, name == "T" },
		View: func(schema, name string) (string, Relation, string) {
			return lowered(name), Relation{Quoted: Quote(Postgres, lowered(name)), Bare: lowered(name)}, ""
		}}
	view := func(name string) *View {
		return &View{Schema: "S", Name: name, Columns: []Column{{"N", Numeric}}, Definition: "select n from t"}
	}
	views := Plan([]*View{view("DONE"), view("BEGUN"), view("FOREIGN"), view("FRESH")}, naming)
	same := row("2", "2", "30")
	db := &fakeDB{there: map[string]bool{"DONE": true, "BEGUN": true, "FOREIGN": true},
		source: map[string][]*string{"BEGUN": same, "FRESH": same}, target: map[string][]*string{"begun": row("2", "2", "31"), "fresh": same}}
	c := db.checker()
	c.Previous = []Result{{Name: "S.DONE", Target: "done", Status: Verified}, {Name: "S.BEGUN", Target: "begun", Status: creating}}
	var saved [][]Result
	c.Save = func(r []Result) { saved = append(saved, append([]Result(nil), r...)) }
	results, err := c.Finish(context.Background(), views)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range results {
		got = append(got, r.Name+"="+r.Status)
	}
	if want := "S.DONE=verified S.BEGUN=dropped S.FOREIGN=not created S.FRESH=verified"; strings.Join(got, " ") != want {
		t.Errorf("results %q, want %q", got, want)
	}
	if !strings.Contains(results[2].Detail, "since the job started, which was left as it is") {
		t.Errorf("the view that is not the job's: %q", results[2].Detail)
	}
	// Only the view the job began itself is compared and dropped.
	if strings.Join(db.dropped, " ") != "BEGUN" || strings.Join(db.created, " ") != "FRESH" {
		t.Errorf("dropped %q, created %q", db.dropped, db.created)
	}
	// Every view is on record as begun before it is created.
	begun := 0
	for _, list := range saved {
		if last := list[len(list)-1]; last.Status == creating {
			begun++
		}
	}
	if begun != 3 {
		t.Errorf("%d view(s) were recorded before they were created, want 3: %+v", begun, saved)
	}
}
