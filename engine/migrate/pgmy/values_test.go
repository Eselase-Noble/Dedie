package pgmy

import "testing"

func TestArrayToJSON(t *testing.T) {
	cases := []struct {
		in      string
		numeric bool
		want    string
	}{
		{`{}`, false, `[]`},
		{`{a,b}`, false, `["a","b"]`},
		{`{a,"b c","d\"e",NULL}`, false, `["a","b c","d\"e",null]`},
		{`{1,2,3}`, true, `[1,2,3]`},
		{`{{1,2},{3,4}}`, true, `[[1,2],[3,4]]`},
		{`{t,f,NULL}`, true, `[true,false,null]`},
		{`{1.5,NaN}`, true, `[1.5,"NaN"]`},
		{`[0:1]={x,y}`, false, `["x","y"]`},
	}
	for _, c := range cases {
		if got := arrayToJSON(c.in, c.numeric); got != c.want {
			t.Errorf("arrayToJSON(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestCopyFields(t *testing.T) {
	fields := copyFields([]byte("1\t\\N\t\ta\\tb\\nc\\\\d"), nil)
	if len(fields) != 4 || string(fields[0]) != "1" || fields[1] != nil || fields[2] == nil || len(fields[2]) != 0 || string(fields[3]) != "a\tb\nc\\d" {
		t.Fatalf("unexpected fields: %q", fields)
	}
}

func TestStripOffset(t *testing.T) {
	for in, want := range map[string]string{
		"2024-02-29 13:14:15.123456+00": "2024-02-29 13:14:15.123456",
		"2024-02-29 13:14:15+00":        "2024-02-29 13:14:15",
		"06:30:00+00":                   "06:30:00",
		"2024-02-29 13:14:15":           "2024-02-29 13:14:15",
	} {
		if got := stripOffset(in); got != want {
			t.Errorf("stripOffset(%s) = %s, want %s", in, got, want)
		}
	}
}
