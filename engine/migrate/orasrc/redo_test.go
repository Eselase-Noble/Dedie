package orasrc

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
	r, err := ParseRedo(OpInsert, `insert into "DEDIE_TEST"."T2"("A","B","C","D","E","F","G") values ('1','k''1, x',UNISTR('\00FC\D83D\DE00 a\\b'),TO_TIMESTAMP_TZ('2026-10-11 17:53:49.065189000'),TO_DSINTERVAL('-01 02:03:04.500000'),NULL,HEXTORAW('deadbeef'));`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "k'1, x", "C": "ü😀 a\\b", "D": "2026-10-11 17:53:49.065189000", "E": "-01 02:03:04.500000", "F": "<null>", "G": "deadbeef"}
	for k, w := range want {
		if got := val(r.Values, k); got != w {
			t.Errorf("insert %s = %q, want %q", k, got, w)
		}
	}

	r, err = ParseRedo(OpUpdate, `update "S"."T" set "B" = 'k2', "N" = NULL, "X" = 'a' || UNISTR('\00e9') where "A" = '1' and "B" = 'and "Z" = ''1''' and "N" IS NULL and ROWID = 'AAAVKsAAAAAAK+1AAA';`)
	if err != nil {
		t.Fatal(err)
	}
	if val(r.Values, "B") != "k2" || val(r.Values, "N") != "<null>" || val(r.Values, "X") != "aé" {
		t.Errorf("update set = %v %v %v", val(r.Values, "B"), val(r.Values, "N"), val(r.Values, "X"))
	}
	if val(r.Where, "A") != "1" || val(r.Where, "B") != `and "Z" = '1'` || val(r.Where, "N") != "<null>" || val(r.Where, "Z") != "<absent>" || val(r.Where, "ROWID") != "<absent>" {
		t.Errorf("update where = %v", r.Where)
	}

	r, err = ParseRedo(OpDelete, `delete from "S"."T" where "A" = '1' and "D" = TO_DATE('2024-01-02 00:00:00', 'YYYY-MM-DD HH24:MI:SS') and ROWID = 'AAA';`)
	if err != nil || val(r.Where, "A") != "1" || val(r.Where, "D") != "2024-01-02 00:00:00" {
		t.Errorf("delete = %v, %v", r, err)
	}

	r, err = ParseRedo(OpLobLocator, "DECLARE \n loc_c CLOB; \n buf_c VARCHAR2(6222); \nBEGIN\n select \"NOTE\" into loc_c from \"S\".\"T\" where \"ID\" = '2' and \"NAME\" = 'it''s' for update;")
	if err != nil || val(r.Where, "ID") != "2" || val(r.Where, "NAME") != "it's" {
		t.Errorf("lob locator = %v, %v", r, err)
	}
}

func TestRedoKeys(t *testing.T) {
	tbl := &Table{Owner: "S", Name: "T", Cols: []Column{{Name: "A"}, {Name: "B"}, {Name: "C"}}, PK: []int{0, 1}}
	check := func(op int, sql string, gone, now []string) {
		t.Helper()
		r, err := ParseRedo(op, sql)
		if err != nil {
			t.Fatal(err)
		}
		g, n, err := r.Keys(tbl, op)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(g, gone) || !reflect.DeepEqual(n, now) {
			t.Errorf("%s: gone %v now %v, want %v %v", sql, g, n, gone, now)
		}
	}
	check(OpInsert, `insert into "S"."T"("A","B","C") values ('1','x',NULL);`, nil, []string{"1", "x"})
	check(OpDelete, `delete from "S"."T" where "A" = '1' and "B" = 'x' and "C" IS NULL;`, []string{"1", "x"}, nil)
	check(OpUpdate, `update "S"."T" set "C" = 'v' where "A" = '1' and "B" = 'x' and "C" IS NULL;`, nil, []string{"1", "x"})
	check(OpUpdate, `update "S"."T" set "B" = 'y' where "A" = '1' and "B" = 'x';`, []string{"1", "x"}, []string{"1", "y"})

	r, _ := ParseRedo(OpUpdate, `update "S"."T" set "C" = 'v' where "C" = 'u' and ROWID = 'AAA';`)
	if _, _, err := r.Keys(tbl, OpUpdate); err == nil {
		t.Error("an update without its key should be an error")
	}
}

func TestInterval(t *testing.T) {
	for in, want := range map[string]string{
		"-01 02:03:04.500000": "P-1DT-2H-3M-4.500000S",
		"+00 00:00:00.000000": "P0DT0H0M0.000000S",
		"+10 12:00:59":        "P10DT12H0M59S",
	} {
		if got := ISOInterval(KindIntervalDS, in); got != want {
			t.Errorf("interval(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ISOInterval(KindIntervalYM, "-01-06"); got != "P-1Y-6M" {
		t.Errorf("year-month = %q", got)
	}
}

func TestClassify(t *testing.T) {
	for dataType, want := range map[string]Kind{
		"NVARCHAR2":                         KindText,
		"NUMBER":                            KindNumber,
		"BINARY_DOUBLE":                     KindNumber,
		"DATE":                              KindDate,
		"TIMESTAMP(9)":                      KindTimestamp,
		"TIMESTAMP(3) WITH TIME ZONE":       KindTimestampTZ,
		"TIMESTAMP(6) WITH LOCAL TIME ZONE": KindTimestampLTZ,
		"INTERVAL YEAR(2) TO MONTH":         KindIntervalYM,
		"INTERVAL DAY(2) TO SECOND(6)":      KindIntervalDS,
		"LONG RAW":                          KindBinary,
		"UROWID":                            KindRowid,
	} {
		if got, ok := Classify(dataType); !ok || got != want {
			t.Errorf("Classify(%q) = %v, %v; want %v", dataType, got, ok, want)
		}
	}
	if _, ok := Classify("SDO_GEOMETRY"); ok {
		t.Error("SDO_GEOMETRY should be unsupported")
	}
}
