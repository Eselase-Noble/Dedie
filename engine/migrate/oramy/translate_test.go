package oramy

import (
	"database/sql"
	"strings"
	"testing"

	"dedie/engine/migrate/orasrc"
)

func num(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func TestTranslate(t *testing.T) {
	cases := []struct {
		c    orasrc.Column
		raw  int64
		want string
		// note is a fragment the note must contain; empty means no note.
		note string
	}{
		{c: orasrc.Column{DataType: "NUMBER"}, want: "decimal(65,30)", note: "without a precision"},
		{c: orasrc.Column{DataType: "NUMBER", Scale: num(0)}, want: "decimal(38,0)"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(2), Scale: num(0)}, want: "tinyint"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(3), Scale: num(0)}, want: "smallint"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(4), Scale: num(0)}, want: "smallint"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(9), Scale: num(0)}, want: "int"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(10), Scale: num(0)}, want: "bigint"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(18), Scale: num(0)}, want: "bigint"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(19), Scale: num(0)}, want: "decimal(19,0)"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(38), Scale: num(0)}, want: "decimal(38,0)"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(12), Scale: num(2)}, want: "decimal(12,2)"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(38), Scale: num(30)}, want: "decimal(38,30)"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(2), Scale: num(5)}, want: "decimal(5,5)"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(5), Scale: num(-2)}, want: "int"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(38), Scale: num(-20)}, want: "decimal(58,0)"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(38), Scale: num(-84)}, want: "double", note: "DOUBLE"},
		{c: orasrc.Column{DataType: "NUMBER", Precision: num(38), Scale: num(31)}, want: "double", note: "DOUBLE"},
		{c: orasrc.Column{DataType: "FLOAT", Precision: num(126)}, want: "double", note: "38 significant digits"},
		{c: orasrc.Column{DataType: "FLOAT", Precision: num(53)}, want: "double"},
		{c: orasrc.Column{DataType: "BINARY_FLOAT"}, want: "float"},
		{c: orasrc.Column{DataType: "BINARY_DOUBLE"}, want: "double"},
		{c: orasrc.Column{DataType: "VARCHAR2", CharLength: 50}, want: "varchar(50)"},
		{c: orasrc.Column{DataType: "NVARCHAR2", CharLength: 4000}, want: "varchar(4000)"},
		{c: orasrc.Column{DataType: "VARCHAR2", CharLength: 32767}, want: "text", note: "TEXT"},
		{c: orasrc.Column{DataType: "CHAR", CharLength: 3}, want: "char(3)"},
		{c: orasrc.Column{DataType: "CHAR"}, want: "char(1)"},
		{c: orasrc.Column{DataType: "NCHAR", CharLength: 2000}, want: "varchar(2000)", note: "blank padding"},
		{c: orasrc.Column{DataType: "CLOB"}, want: "longtext"},
		{c: orasrc.Column{DataType: "NCLOB"}, want: "longtext"},
		{c: orasrc.Column{DataType: "LONG"}, want: "longtext"},
		{c: orasrc.Column{DataType: "RAW"}, raw: 16, want: "varbinary(16)"},
		{c: orasrc.Column{DataType: "RAW"}, want: "varbinary(2000)"},
		{c: orasrc.Column{DataType: "BLOB"}, want: "longblob"},
		{c: orasrc.Column{DataType: "LONG RAW"}, want: "longblob"},
		{c: orasrc.Column{DataType: "DATE"}, want: "datetime"},
		{c: orasrc.Column{DataType: "TIMESTAMP(0)", Scale: num(0)}, want: "datetime"},
		{c: orasrc.Column{DataType: "TIMESTAMP(3)", Scale: num(3)}, want: "datetime(3)"},
		{c: orasrc.Column{DataType: "TIMESTAMP(6)", Scale: num(6)}, want: "datetime(6)"},
		{c: orasrc.Column{DataType: "TIMESTAMP(9)", Scale: num(9)}, want: "datetime(6)", note: "rounded to the microsecond"},
		{c: orasrc.Column{DataType: "TIMESTAMP(3) WITH TIME ZONE", Scale: num(3)}, want: "datetime(6)", note: "in UTC"},
		{c: orasrc.Column{DataType: "TIMESTAMP(9) WITH TIME ZONE", Scale: num(9)}, want: "datetime(6)", note: "rounded to the microsecond"},
		{c: orasrc.Column{DataType: "TIMESTAMP(6) WITH LOCAL TIME ZONE", Scale: num(6)}, want: "datetime(6)", note: "holding UTC"},
		{c: orasrc.Column{DataType: "INTERVAL YEAR(2) TO MONTH"}, want: "varchar(64)", note: "ISO 8601"},
		{c: orasrc.Column{DataType: "INTERVAL DAY(2) TO SECOND(6)"}, want: "varchar(64)", note: "ISO 8601"},
		{c: orasrc.Column{DataType: "ROWID"}, want: "varchar(18)", note: "no longer points"},
		{c: orasrc.Column{DataType: "UROWID"}, want: "text", note: "no longer points"},
		{c: orasrc.Column{DataType: "XMLTYPE"}, want: "longtext", note: "well formed"},
		{c: orasrc.Column{DataType: "JSON"}, want: "json"},
		{c: orasrc.Column{DataType: "BOOLEAN"}, want: "tinyint(1)"},
	}
	for _, tc := range cases {
		got, note, ok := translate(tc.c, tc.raw)
		if !ok || got.DDL != tc.want {
			t.Errorf("%s -> %q (ok %v), want %q", tc.c.DataType, got.DDL, ok, tc.want)
		}
		if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
			t.Errorf("%s -> note %q, want one containing %q", tc.c.DataType, note, tc.note)
		}
	}
	if _, _, ok := translate(orasrc.Column{DataType: "SDO_GEOMETRY"}, 0); ok {
		t.Error("SDO_GEOMETRY should be unsupported")
	}
}

// The sizes decide which columns fit a row and which indexes can be built.
func TestTypeSizes(t *testing.T) {
	cases := []struct {
		c    orasrc.Column
		want int
		long bool
	}{
		{orasrc.Column{DataType: "NUMBER", Precision: num(9), Scale: num(0)}, 4, false},
		{orasrc.Column{DataType: "NUMBER", Precision: num(12), Scale: num(2)}, 6, false},
		{orasrc.Column{DataType: "NUMBER"}, 30, false},
		{orasrc.Column{DataType: "VARCHAR2", CharLength: 768}, 3072, false},
		{orasrc.Column{DataType: "CHAR", CharLength: 10}, 40, false},
		{orasrc.Column{DataType: "TIMESTAMP(6)", Scale: num(6)}, 8, false},
		{orasrc.Column{DataType: "CLOB"}, longBytes, true},
	}
	for _, tc := range cases {
		if got, _, _ := translate(tc.c, 0); got.Bytes != tc.want || got.Long != tc.long {
			t.Errorf("%s: %d bytes (long %v), want %d (long %v)", tc.c.DataType, got.Bytes, got.Long, tc.want, tc.long)
		}
	}
}
