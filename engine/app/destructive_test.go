package app

import "testing"

func TestDestructive(t *testing.T) {
	cases := []struct {
		kind, sql, want string
	}{
		{"postgres", "select * from orders", ""},
		{"postgres", "DROP TABLE orders", "DROP TABLE"},
		{"postgres", "  drop   schema billing cascade;", "DROP SCHEMA"},
		{"mysql", "TRUNCATE TABLE stock", "TRUNCATE TABLE"},
		{"postgres", "truncate orders", "TRUNCATE"},
		{"postgres", "drop owned by app", "DROP"},
		{"postgres", "select 1; DROP INDEX orders_customer", "DROP INDEX"},
		{"postgres", "ALTER TABLE orders DROP COLUMN total", "ALTER ... DROP"},
		{"postgres", "ALTER TABLE orders ADD COLUMN note text", ""},
		{"postgres", "DELETE FROM orders", "DELETE without WHERE"},
		{"postgres", "delete from orders where id = 4", ""},
		{"mysql", "UPDATE stock SET quantity = 0", "UPDATE without WHERE"},
		{"mysql", "UPDATE stock SET quantity = 0 WHERE id = 9", ""},
		// Text that only mentions a keyword is not a statement.
		{"postgres", "select 'DROP TABLE orders'", ""},
		{"postgres", "-- DROP TABLE orders\nselect 1", ""},
		{"postgres", "/* drop table orders */ select 1", ""},
		{"mysql", "# drop table orders\nselect 1", ""},
		{"postgres", `select "drop" from t`, ""},
		{"mysql", "select `truncate` from t", ""},
		{"postgres", "insert into notes values ('it''s; drop table x')", ""},
		{"postgres", "DO $$ begin drop table x; end $$", ""},
		{"postgres", "select $tag$ drop table x $tag$; drop table y", "DROP TABLE"},
		// The WHERE has to belong to the statement, not to a quoted value.
		{"postgres", "delete from orders -- where id = 1", "DELETE without WHERE"},
		{"postgres", "update t set note = 'where'", "UPDATE without WHERE"},
		{"postgres", "with old as (select id from orders where placed_at < now()) delete from orders using old", "WITH ... DELETE"},
		{"postgres", "with x as (select 1) select * from x for update", ""},
		{"oracle", "DROP USER scott CASCADE", "DROP USER"},
		{"mongodb", `{"find": "orders", "filter": {}}`, ""},
		{"mongodb", `{"drop": "orders"}`, "drop"},
		{"mongodb", ` {"dropDatabase": 1}`, "dropDatabase"},
		{"mongodb", `{"delete": "orders", "deletes": [{"q": {}, "limit": 0}]}`, "delete with an empty filter"},
		{"mongodb", `{"delete": "orders", "deletes": [{"q": {"status": "old"}, "limit": 0}]}`, ""},
		{"mongodb", `not json`, ""},
	}
	for _, c := range cases {
		if got := destructive(c.kind, c.sql); got != c.want {
			t.Errorf("destructive(%s, %q) = %q, want %q", c.kind, c.sql, got, c.want)
		}
	}
}
