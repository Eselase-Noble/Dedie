package oradef

import (
	"testing"

	"dedie/engine/migrate/orasrc"
	"dedie/engine/migrate/viewsql"
)

func TestViewSource(t *testing.T) {
	v := &viewsql.View{Schema: "SHOP", Name: "TOTALS", Definition: "select customer_id, sum(amount) total\nfrom invoices group by customer_id\n",
		Columns: []viewsql.Column{{Name: "CUSTOMER_ID"}, {Name: "TOTAL"}}}
	want := `CREATE OR REPLACE VIEW "SHOP"."TOTALS" ("CUSTOMER_ID", "TOTAL") AS` + "\nselect customer_id, sum(amount) total\nfrom invoices group by customer_id"
	if got := viewSource(v); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// A view whose text the account may not read has no statement.
	if got := viewSource(&viewsql.View{Schema: "SHOP", Name: "HIDDEN"}); got != "" {
		t.Errorf("a view without a definition is written as %q", got)
	}
}

func TestDates(t *testing.T) {
	cat := &orasrc.Catalog{Tables: []*orasrc.Table{{Owner: "SHOP", Name: "SALES", Cols: []orasrc.Column{
		{Name: "ID", DataType: "NUMBER"}, {Name: "SOLD", DataType: "DATE"}, {Name: "Seen", DataType: "TIMESTAMP(6) WITH TIME ZONE"}, {Name: "NOTE", DataType: "VARCHAR2"},
	}}}}
	isDate := Dates(cat)
	for name, want := range map[string]bool{"SOLD": true, "sold": true, "SEEN": true, "ID": false, "NOTE": false, "ELSEWHERE": false} {
		if isDate(name) != want {
			t.Errorf("%s: date is %v, want %v", name, isDate(name), want)
		}
	}
}
