package schedule

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct{ expr, from, want string }{
		{"0 2 * * *", "2026-03-10 01:00", "2026-03-10 02:00"},
		{"0 2 * * *", "2026-03-10 02:00", "2026-03-11 02:00"},
		{"*/15 * * * *", "2026-03-10 10:07", "2026-03-10 10:15"},
		{"30 6 * * 1-5", "2026-03-13 07:00", "2026-03-16 06:30"}, // Friday to Monday
		{"0 0 1 * *", "2026-01-31 12:00", "2026-02-01 00:00"},
		{"0 3 * * 7", "2026-03-10 00:00", "2026-03-15 03:00"}, // 7 is Sunday
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 12 13 * 5", "2026-03-10 00:00", "2026-03-13 12:00"}, // either day field
		{"@hourly", "2026-12-31 23:30", "2027-01-01 00:00"},
	}
	for _, c := range cases {
		spec, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := spec.Next(at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%s after %s: got %s, want %s", c.expr, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "a * * * *", "5-1 * * * *"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("%q was accepted", expr)
		}
	}
	if spec, _ := Parse("0 0 31 2 *"); !spec.Next(time.Now()).IsZero() {
		t.Error("31 February should never fire")
	}
}
