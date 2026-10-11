package orapg

import (
	"database/sql"
	"testing"

	"dedie/engine/migrate/orasrc"
)

func TestTranslate(t *testing.T) {
	n := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	cases := []struct {
		c    orasrc.Column
		want string
	}{
		{orasrc.Column{DataType: "NUMBER"}, "numeric"},
		{orasrc.Column{DataType: "NUMBER", Precision: n(4), Scale: n(0)}, "smallint"},
		{orasrc.Column{DataType: "NUMBER", Precision: n(9), Scale: n(0)}, "integer"},
		{orasrc.Column{DataType: "NUMBER", Precision: n(18), Scale: n(0)}, "bigint"},
		{orasrc.Column{DataType: "NUMBER", Precision: n(19), Scale: n(0)}, "numeric(19,0)"},
		{orasrc.Column{DataType: "NUMBER", Precision: n(12), Scale: n(2)}, "numeric(12,2)"},
		{orasrc.Column{DataType: "NUMBER", Precision: n(5), Scale: n(-2)}, "numeric"},
		{orasrc.Column{DataType: "FLOAT", Precision: n(126)}, "numeric"},
		{orasrc.Column{DataType: "VARCHAR2", CharLength: 50}, "varchar(50)"},
		{orasrc.Column{DataType: "CHAR", CharLength: 3}, "char(3)"},
		{orasrc.Column{DataType: "DATE"}, "timestamp(0)"},
		{orasrc.Column{DataType: "TIMESTAMP(9)", Scale: n(9)}, "timestamp(6)"},
		{orasrc.Column{DataType: "TIMESTAMP(3) WITH TIME ZONE", Scale: n(3)}, "timestamptz(3)"},
		{orasrc.Column{DataType: "TIMESTAMP(6) WITH LOCAL TIME ZONE", Scale: n(6)}, "timestamptz(6)"},
		{orasrc.Column{DataType: "INTERVAL DAY(2) TO SECOND(6)"}, "interval day to second"},
		{orasrc.Column{DataType: "BLOB"}, "bytea"},
		{orasrc.Column{DataType: "CLOB"}, "text"},
	}
	for _, tc := range cases {
		if got, _, ok := translate(tc.c); !ok || got != tc.want {
			t.Errorf("%s -> %q (ok %v), want %q", tc.c.DataType, got, ok, tc.want)
		}
	}
	if _, _, ok := translate(orasrc.Column{DataType: "SDO_GEOMETRY"}); ok {
		t.Error("SDO_GEOMETRY should be unsupported")
	}
}
