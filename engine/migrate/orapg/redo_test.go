package orapg

import (
	"reflect"
	"testing"
)

func val(m map[string]*string, k string) string {
	v, ok := m[k]
	switch {
	case !ok:
		return "<absent>"
	case v == nil:
		return "<null>"
	}
	return *v
}

func TestParseRedo(t *testing.T) {
	r, err := parseRedo(opInsert, `insert into "DEDIE_TEST"."T2"("A","B","C","D","E","F","G") values ('1','k''1, x',UNISTR('\00FC\D83D\DE00 a\\b'),TO_TIMESTAMP_TZ('2026-10-11 17:53:49.065189000'),TO_DSINTERVAL('-01 02:03:04.500000'),NULL,HEXTORAW('deadbeef'));`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "k'1, x", "C": "ü😀 a\\b", "D": "2026-10-11 17:53:49.065189000", "E": "-01 02:03:04.500000", "F": "<null>", "G": "deadbeef"}
	for k, w := range want {
		if got := val(r.values, k); got != w {
			t.Errorf("insert %s = %q, want %q", k, got, w)
		}
	}

	r, err = parseRedo(opUpdate, `update "S"."T" set "B" = 'k2', "N" = NULL, "X" = 'a' || UNISTR('\00e9') where "A" = '1' and "B" = 'and "Z" = ''1''' and "N" IS NULL and ROWID = 'AAAVKsAAAAAAK+1AAA';`)
	if err != nil {
		t.Fatal(err)
	}
	if val(r.values, "B") != "k2" || val(r.values, "N") != "<null>" || val(r.values, "X") != "aé" {
		t.Errorf("update set = %v %v %v", val(r.values, "B"), val(r.values, "N"), val(r.values, "X"))
	}
	if val(r.where, "A") != "1" || val(r.where, "B") != `and "Z" = '1'` || val(r.where, "N") != "<null>" || val(r.where, "Z") != "<absent>" || val(r.where, "ROWID") != "<absent>" {
		t.Errorf("update where = %v", r.where)
	}

	r, err = parseRedo(opDelete, `delete from "S"."T" where "A" = '1' and "D" = TO_DATE('2024-01-02 00:00:00', 'YYYY-MM-DD HH24:MI:SS') and ROWID = 'AAA';`)
	if err != nil || val(r.where, "A") != "1" || val(r.where, "D") != "2024-01-02 00:00:00" {
		t.Errorf("delete = %v, %v", r, err)
	}

	r, err = parseRedo(opLobLocator, "DECLARE \n loc_c CLOB; \n buf_c VARCHAR2(6222); \nBEGIN\n select \"NOTE\" into loc_c from \"S\".\"T\" where \"ID\" = '2' and \"NAME\" = 'it''s' for update;")
	if err != nil || val(r.where, "ID") != "2" || val(r.where, "NAME") != "it's" {
		t.Errorf("lob locator = %v, %v", r, err)
	}
}

func TestRedoKeys(t *testing.T) {
	tbl := &Table{Owner: "S", Name: "T", Cols: []Column{{Name: "A"}, {Name: "B"}, {Name: "C"}}, PK: []int{0, 1}}
	check := func(op int, sql string, gone, now []string) {
		t.Helper()
		r, err := parseRedo(op, sql)
		if err != nil {
			t.Fatal(err)
		}
		g, n, err := r.keys(tbl, op)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(g, gone) || !reflect.DeepEqual(n, now) {
			t.Errorf("%s: gone %v now %v, want %v %v", sql, g, n, gone, now)
		}
	}
	check(opInsert, `insert into "S"."T"("A","B","C") values ('1','x',NULL);`, nil, []string{"1", "x"})
	check(opDelete, `delete from "S"."T" where "A" = '1' and "B" = 'x' and "C" IS NULL;`, []string{"1", "x"}, nil)
	check(opUpdate, `update "S"."T" set "C" = 'v' where "A" = '1' and "B" = 'x' and "C" IS NULL;`, nil, []string{"1", "x"})
	check(opUpdate, `update "S"."T" set "B" = 'y' where "A" = '1' and "B" = 'x';`, []string{"1", "x"}, []string{"1", "y"})

	r, _ := parseRedo(opUpdate, `update "S"."T" set "C" = 'v' where "C" = 'u' and ROWID = 'AAA';`)
	if _, _, err := r.keys(tbl, opUpdate); err == nil {
		t.Error("an update without its key should be an error")
	}
}

func TestInterval(t *testing.T) {
	for in, want := range map[string]string{
		"-01 02:03:04.500000": "P-1DT-2H-3M-4.500000S",
		"+00 00:00:00.000000": "P0DT0H0M0.000000S",
		"+10 12:00:59":        "P10DT12H0M59S",
	} {
		if got := interval(kIntervalDS, in); got != want {
			t.Errorf("interval(%q) = %q, want %q", in, got, want)
		}
	}
	if got := interval(kIntervalYM, "-01-06"); got != "P-1Y-6M" {
		t.Errorf("year-month = %q", got)
	}
}

func TestTranslate(t *testing.T) {
	n := func(v int64) (out struct {
		Int64 int64
		Valid bool
	}) {
		out.Int64, out.Valid = v, true
		return
	}
	_ = n
	cases := []struct {
		c    Column
		want string
	}{
		{Column{DataType: "NUMBER"}, "numeric"},
		{Column{DataType: "NUMBER", Precision: nullInt("4"), Scale: nullInt("0")}, "smallint"},
		{Column{DataType: "NUMBER", Precision: nullInt("9"), Scale: nullInt("0")}, "integer"},
		{Column{DataType: "NUMBER", Precision: nullInt("18"), Scale: nullInt("0")}, "bigint"},
		{Column{DataType: "NUMBER", Precision: nullInt("19"), Scale: nullInt("0")}, "numeric(19,0)"},
		{Column{DataType: "NUMBER", Precision: nullInt("12"), Scale: nullInt("2")}, "numeric(12,2)"},
		{Column{DataType: "NUMBER", Precision: nullInt("5"), Scale: nullInt("-2")}, "numeric"},
		{Column{DataType: "FLOAT", Precision: nullInt("126")}, "numeric"},
		{Column{DataType: "VARCHAR2", CharLength: 50}, "varchar(50)"},
		{Column{DataType: "CHAR", CharLength: 3}, "char(3)"},
		{Column{DataType: "DATE"}, "timestamp(0)"},
		{Column{DataType: "TIMESTAMP(9)", Scale: nullInt("9")}, "timestamp(6)"},
		{Column{DataType: "TIMESTAMP(3) WITH TIME ZONE", Scale: nullInt("3")}, "timestamptz(3)"},
		{Column{DataType: "TIMESTAMP(6) WITH LOCAL TIME ZONE", Scale: nullInt("6")}, "timestamptz(6)"},
		{Column{DataType: "INTERVAL DAY(2) TO SECOND(6)"}, "interval day to second"},
		{Column{DataType: "BLOB"}, "bytea"},
		{Column{DataType: "CLOB"}, "text"},
	}
	for _, tc := range cases {
		c := tc.c
		if _, ok := translate(&c); !ok || c.PGType != tc.want {
			t.Errorf("%s -> %q (ok %v), want %q", tc.c.DataType, c.PGType, ok, tc.want)
		}
	}
	c := Column{DataType: "SDO_GEOMETRY"}
	if _, ok := translate(&c); ok {
		t.Error("SDO_GEOMETRY should be unsupported")
	}
}
