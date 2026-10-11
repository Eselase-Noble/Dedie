package oramy

import (
	"bytes"
	"testing"

	"dedie/engine/migrate/orasrc"
)

func column(dataType, ddl string) Column {
	kind, _ := orasrc.Classify(dataType)
	return Column{Column: orasrc.Column{DataType: dataType, Kind: kind}, Type: myType{DDL: ddl}}
}

func TestMyValue(t *testing.T) {
	cases := []struct {
		c    Column
		in   any
		want any
	}{
		{column("VARCHAR2", "varchar(10)"), nil, nil},
		{column("VARCHAR2", "varchar(10)"), "padded  ", "padded  "},
		{column("CLOB", "longtext"), []byte("as bytes"), "as bytes"},
		{column("CHAR", "char(5)"), "ab   ", "ab"},
		{column("CHAR", "varchar(300)"), "ab   ", "ab   "},
		{column("NUMBER", "decimal(65,30)"), ".5", ".5"},
		{column("DATE", "datetime"), "2024-02-29 13:14:15", "2024-02-29 13:14:15"},
		{column("TIMESTAMP(9)", "datetime(6)"), "2024-02-29 13:14:15.123456789", "2024-02-29 13:14:15.123456789"},
		{column("TIMESTAMP(6) WITH TIME ZONE", "datetime(6)"), "2024-02-29 13:14:15.500000000 +05:30", "2024-02-29 07:44:15.500000000"},
		{column("TIMESTAMP(6) WITH TIME ZONE", "datetime(6)"), "2024-01-01 00:30:00.000000000 -08:00", "2024-01-01 08:30:00.000000000"},
		{column("TIMESTAMP(6) WITH TIME ZONE", "datetime(6)"), "not a timestamp", "not a timestamp"},
		{column("TIMESTAMP(6) WITH LOCAL TIME ZONE", "datetime(6)"), "2024-02-29 13:14:15.000000000+00", "2024-02-29 13:14:15.000000000"},
		{column("INTERVAL YEAR(2) TO MONTH", "varchar(64)"), "+02-06", "P2Y6M"},
		{column("INTERVAL DAY(2) TO SECOND(6)", "varchar(64)"), "-01 02:03:04.500000", "P-1DT-2H-3M-4.5S"},
		{column("INTERVAL DAY(2) TO SECOND(6)", "varchar(64)"), "+03 00:00:10.000000", "P3DT0H0M10S"},
		{column("INTERVAL DAY(2) TO SECOND(0)", "varchar(64)"), "+00 00:00:00", "P0DT0H0M0S"},
		{column("BOOLEAN", "tinyint(1)"), "TRUE", "1"},
		{column("BOOLEAN", "tinyint(1)"), "FALSE", "0"},
		{column("JSON", "json"), []byte(`{"a": 1}`), `{"a": 1}`},
	}
	for _, tc := range cases {
		if got := myValue(tc.c, tc.in); got != tc.want {
			t.Errorf("%s %v -> %#v, want %#v", tc.c.DataType, tc.in, got, tc.want)
		}
	}
	raw := column("RAW", "varbinary(16)")
	if got, ok := myValue(raw, []byte{0, 0xff}).([]byte); !ok || !bytes.Equal(got, []byte{0, 0xff}) {
		t.Errorf("RAW value -> %#v", got)
	}
	if got, ok := myKey(raw, "00FF10").([]byte); !ok || !bytes.Equal(got, []byte{0, 0xff, 0x10}) {
		t.Errorf("RAW key -> %#v", got)
	}
	if got := myKey(column("CHAR", "char(3)"), "A  "); got != "A" {
		t.Errorf("CHAR key -> %#v", got)
	}
}

func TestKeyBind(t *testing.T) {
	cases := []struct {
		c    Column
		want string
	}{
		{Column{Type: typeBigint}, "CAST(? AS SIGNED)"},
		{Column{Type: decimalType(65, 30)}, "CAST(? AS decimal(65,30))"},
		{Column{Type: typeDouble}, "(? + 0E0)"},
		{Column{Type: datetimeType(0)}, "CAST(? AS datetime)"},
		{Column{Type: datetimeType(6)}, "CAST(? AS datetime(6))"},
		{Column{Type: varcharType(20)}, "?"},
		{Column{Type: myType{DDL: "varbinary(16)"}}, "?"},
	}
	for _, tc := range cases {
		if got := keyBind(tc.c); got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.c.Type.DDL, got, tc.want)
		}
	}
}
