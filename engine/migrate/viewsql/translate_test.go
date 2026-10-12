package viewsql

import (
	"errors"
	"strings"
	"testing"
)

// lowered is how the pipelines out of Oracle name things on the target:
// a name Oracle keeps in upper case becomes lower case.
func lowered(name string) string {
	if name == strings.ToUpper(name) {
		return strings.ToLower(name)
	}
	return name
}

func same(name string) string { return name }

// route builds the options for one pair of dialects over a small schema:
// customers and invoices in the view's own schema, and a table in a
// second schema that the target knows under another name.
func route(from, to Dialect) Options {
	o := Options{From: from, To: to, Schema: "shop", Ident: same}
	other := "audit"
	if from == Oracle {
		o.Schema, other, o.Ident = "SHOP", "AUDIT", lowered
	}
	quote := func(name string) string { return `"` + name + `"` }
	if to == MySQL {
		quote = func(name string) string { return "`" + name + "`" }
	}
	o.Relation = func(schema, name string) (Relation, bool) {
		switch {
		case schema == o.Schema && (strings.EqualFold(name, "customers") || strings.EqualFold(name, "invoices")):
			bare := o.Ident(name)
			if to == Postgres {
				return Relation{Quoted: quote("public") + "." + quote(bare), Bare: bare}, true
			}
			return Relation{Quoted: quote(bare), Bare: bare}, true
		case schema == other && strings.EqualFold(name, "log"):
			return Relation{Quoted: quote("audit_log"), Bare: "audit_log"}, true
		}
		return Relation{}, false
	}
	return o
}

type translation struct {
	name, in, want string
}

func checkTranslations(t *testing.T, o Options, cases []translation) {
	t.Helper()
	for _, c := range cases {
		got, _, err := Translate(c.in, o)
		if err != nil {
			t.Errorf("%s: %v\n   in: %s", c.name, err, c.in)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n   in: %s\n  got: %s\n want: %s", c.name, c.in, got, c.want)
		}
	}
}

type refusal struct {
	name, in, reason string
}

// checkRefusals makes sure each definition is turned down, and for the
// reason an administrator would be told.
func checkRefusals(t *testing.T, o Options, cases []refusal) {
	t.Helper()
	for _, c := range cases {
		got, _, err := Translate(c.in, o)
		var u *Unsupported
		if !errors.As(err, &u) {
			t.Errorf("%s: translated to %q, want it left out (%s)", c.name, got, c.reason)
			continue
		}
		if !strings.Contains(u.Reason, c.reason) {
			t.Errorf("%s: left out because %q, want a reason with %q", c.name, u.Reason, c.reason)
		}
	}
}

// The MySQL definitions are written the way MySQL stores them: every name
// in backticks and qualified with the database, keywords in lower case.
func TestMySQLToPostgres(t *testing.T) {
	o := route(MySQL, Postgres)
	checkTranslations(t, o, []translation{
		{"join and aggregate",
			"select `c`.`id` AS `id`,coalesce(sum(`i`.`amount`),0) AS `total` from (`shop`.`customers` `c` left join `shop`.`invoices` `i` on((`i`.`customer_id` = `c`.`id`))) group by `c`.`id`",
			`SELECT "c"."id" AS "id", COALESCE(SUM("i"."amount"), 0) AS "total" FROM ("public"."customers" "c" LEFT JOIN "public"."invoices" "i" ON (("i"."customer_id" = "c"."id"))) GROUP BY "c"."id"`},
		{"columns qualified with the database",
			"select `shop`.`customers`.`id` AS `id`,`shop`.`customers`.`Name` AS `Name` from `shop`.`customers` where (`shop`.`customers`.`id` > 10)",
			`SELECT "customers"."id" AS "id", "customers"."Name" AS "Name" FROM "public"."customers" WHERE ("customers"."id" > 10)`},
		{"ifnull and if",
			"select ifnull(`c`.`name`,'none') AS `n`,if((`c`.`id` > 5),'big','small') AS `size` from `shop`.`customers` `c`",
			`SELECT COALESCE("c"."name", 'none') AS "n", CASE WHEN ("c"."id" > 5) THEN 'big' ELSE 'small' END AS "size" FROM "public"."customers" "c"`},
		{"concat is null with any null",
			"select concat(`c`.`name`,' #',`c`.`id`) AS `label` from `shop`.`customers` `c`",
			`SELECT (("c"."name")::text || (' #')::text || ("c"."id")::text) AS "label" FROM "public"."customers" "c"`},
		{"length counts bytes",
			"select length(`c`.`name`) AS `b`,char_length(`c`.`name`) AS `c` from `shop`.`customers` `c`",
			`SELECT octet_length("c"."name") AS "b", char_length("c"."name") AS "c" FROM "public"."customers" "c"`},
		{"string escapes",
			`select 'it\'s' AS ` + "`a`" + `,'back\\slash' AS ` + "`b`" + `,'50\%' AS ` + "`c`",
			`SELECT 'it''s' AS "a", E'back\\slash' AS "b", E'50\\%' AS "c"`},
		{"a keyword inside a literal and a name is not SQL",
			"select 'ifnull(a, b) from dual' AS `ifnull`,`c`.`from` AS `select` from `shop`.`customers` `c`",
			`SELECT 'ifnull(a, b) from dual' AS "ifnull", "c"."from" AS "select" FROM "public"."customers" "c"`},
		{"null-safe comparison",
			"select `c`.`id` AS `id` from `shop`.`customers` `c` where (`c`.`a` <=> `c`.`b`)",
			`SELECT "c"."id" AS "id" FROM "public"."customers" "c" WHERE ("c"."a" IS NOT DISTINCT FROM "c"."b")`},
		{"cast",
			"select cast(`c`.`id` as char charset utf8mb4) AS `a`,cast(`c`.`balance` as decimal(10,2)) AS `b`,cast(`c`.`n` as signed) AS `c` from `shop`.`customers` `c`",
			`SELECT CAST("c"."id" AS text) AS "a", CAST("c"."balance" AS numeric(10,2)) AS "b", CAST("c"."n" AS bigint) AS "c" FROM "public"."customers" "c"`},
		{"dates",
			"select year(`c`.`born`) AS `y`,date(`c`.`created`) AS `d`,now() AS `n` from `shop`.`customers` `c` where (`c`.`born` > DATE'2000-01-31')",
			`SELECT EXTRACT(YEAR FROM "c"."born") AS "y", CAST("c"."created" AS date) AS "d", LOCALTIMESTAMP(0) AS "n" FROM "public"."customers" "c" WHERE ("c"."born" > DATE '2000-01-31')`},
		{"substring from a fixed position",
			"select substr(`c`.`name`,1,3) AS `s`,left(`c`.`name`,2) AS `l` from `shop`.`customers` `c`",
			`SELECT substr("c"."name", 1, 3) AS "s", left("c"."name", 2) AS "l" FROM "public"."customers" "c"`},
		{"union, in, case, limit",
			"select `c`.`id` AS `id` from `shop`.`customers` `c` where (`c`.`id` in (1,2,3)) union all select (case when (`i`.`amount` is null) then 0 else 1 end) AS `id` from `shop`.`invoices` `i` order by `id` desc limit 5",
			`SELECT "c"."id" AS "id" FROM "public"."customers" "c" WHERE ("c"."id" IN (1, 2, 3)) UNION ALL SELECT (CASE WHEN ("i"."amount" IS NULL) THEN 0 ELSE 1 END) AS "id" FROM "public"."invoices" "i" ORDER BY "id" DESC LIMIT 5`},
		{"subquery and exists",
			"select `c`.`id` AS `id`,(select count(0) from `shop`.`invoices` `i` where (`i`.`customer_id` = `c`.`id`)) AS `n` from `shop`.`customers` `c` where exists(select 1 from `shop`.`invoices`)",
			`SELECT "c"."id" AS "id", (SELECT COUNT(0) FROM "public"."invoices" "i" WHERE ("i"."customer_id" = "c"."id")) AS "n" FROM "public"."customers" "c" WHERE EXISTS (SELECT 1 FROM "public"."invoices")`},
		{"a table the target knows under another name keeps its own as an alias",
			"select `audit`.`log`.`at` AS `at` from `audit`.`log`",
			`SELECT "log"."at" AS "at" FROM "audit_log" AS "log"`},
	})
	checkRefusals(t, o, []refusal{
		{"window function", "select row_number() OVER (ORDER BY `c`.`id` ) AS `n` from `shop`.`customers` `c`", "ROW_NUMBER()"},
		{"unknown function", "select date_format(`c`.`born`,'%Y') AS `y` from `shop`.`customers` `c`", "DATE_FORMAT()"},
		{"stored function", "select `shop`.`tax`(`c`.`id`) AS `t` from `shop`.`customers` `c`", "a function defined in the source database"},
		{"table outside the migration", "select `o`.`id` AS `id` from `shop`.`orders` `o`", "reads shop.orders, which is not part of the migration"},
		{"greatest skips nulls on the target", "select greatest(`c`.`a`,`c`.`b`) AS `g` from `shop`.`customers` `c`", "GREATEST()"},
		{"substring from the end", "select substr(`c`.`name`,-(3)) AS `s` from `shop`.`customers` `c`", "plain positive number"},
		{"integer division operator", "select (`c`.`a` DIV 2) AS `h` from `shop`.`customers` `c`", "DIV"},
		{"regular expression", "select regexp_like(`c`.`name`,'^a') AS `m` from `shop`.`customers` `c`", "REGEXP_LIKE()"},
		{"interval arithmetic", "select (`c`.`born` + interval 1 day) AS `d` from `shop`.`customers` `c`", "INTERVAL"},
		{"common table expression", "with `x` as (select 1 AS `a`) select `x`.`a` AS `a` from `x`", "WITH"},
		{"user variable", "select @rownum AS `r`", "@"},
		{"cast to unsigned", "select cast(`c`.`id` as unsigned) AS `u` from `shop`.`customers` `c`", "UNSIGNED"},
		{"bitwise operator", "select (`c`.`flags` & 4) AS `f` from `shop`.`customers` `c`", "operator &"},
		{"pipes", "select (`c`.`a` || `c`.`b`) AS `x` from `shop`.`customers` `c`", "||"},
		{"version comment", "select /*!80000 1 */ AS `a`", "version-specific comment"},
		{"hexadecimal literal", "select 0x4142 AS `a`", "number written in a form"},
		{"empty", "", "did not show its definition"},
	})
}

// The PostgreSQL definitions are written the way pg_get_viewdef prints
// them with an empty search_path: tables qualified, a cast on nearly
// every literal, brackets around every join.
func TestPostgresToMySQL(t *testing.T) {
	o := route(Postgres, MySQL)
	checkTranslations(t, o, []translation{
		{"join and aggregate",
			" SELECT c.id,\n    COALESCE(sum(i.amount), (0)::numeric) AS total\n   FROM (shop.customers c\n     LEFT JOIN shop.invoices i ON ((i.customer_id = c.id)))\n  GROUP BY c.id;",
			"SELECT `c`.`id`, COALESCE(SUM(`i`.`amount`), (0)) AS `total` FROM (`customers` `c` LEFT JOIN `invoices` `i` ON ((`i`.`customer_id` = `c`.`id`))) GROUP BY `c`.`id`"},
		{"casts on literals are dropped, a cast of a column keeps exact comparison",
			"SELECT customers.id FROM shop.customers WHERE ((customers.status = 'open'::text) AND (customers.born > '2000-01-31'::date) AND ((customers.kind)::text = 'a'::text))",
			"SELECT `customers`.`id` FROM `customers` WHERE ((`customers`.`status` = 'open') AND (`customers`.`born` > CAST('2000-01-31' AS DATE)) AND ((CAST((`customers`.`kind`) AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_bin) = 'a'))"},
		{"concatenation, a list of values, a cast of an aggregate",
			"SELECT c.id, ((c.name)::text || ' x'::text) AS label, (c.total / (2)::numeric) AS half, (count(*))::integer AS n FROM shop.customers c WHERE (c.status = ANY (ARRAY['a'::text, 'b'::text])) GROUP BY c.id",
			"SELECT `c`.`id`, (CONCAT((CAST((`c`.`name`) AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_bin), ' x')) AS `label`, (`c`.`total` / (2)) AS `half`, CAST((COUNT(*)) AS SIGNED) AS `n` FROM `customers` `c` WHERE (`c`.`status` IN ('a', 'b')) GROUP BY `c`.`id`"},
		{"not in a list, like as PostgreSQL stores it",
			`SELECT c.id FROM shop.customers c WHERE (((c.kind)::text <> ALL ((ARRAY['a'::character varying, 'b'::character varying])::text[])) AND (c.name ~~ 'A%'::text) AND (c.name !~~ '%\_z'::text))`,
			"SELECT `c`.`id` FROM `customers` `c` WHERE (((CAST((`c`.`kind`) AS CHAR CHARACTER SET utf8mb4) COLLATE utf8mb4_bin) NOT IN ('a', 'b')) AND (`c`.`name` LIKE 'A%') AND (`c`.`name` NOT LIKE '%\\\\_z'))"},
		{"functions, and a table the target knows under another name",
			`SELECT log.at, "substring"(log.msg, 1, 10) AS head, length(log.msg) AS n, now() AS seen, CURRENT_DATE AS today, date_part('year'::text, log.at) AS y, EXTRACT(month FROM log.at) AS m FROM audit.log WHERE (log.ok IS TRUE)`,
			"SELECT `log`.`at`, SUBSTRING(`log`.`msg`, 1, 10) AS `head`, CHAR_LENGTH(`log`.`msg`) AS `n`, NOW(6) AS `seen`, CURRENT_DATE AS `today`, EXTRACT(YEAR FROM `log`.`at`) AS `y`, EXTRACT(MONTH FROM `log`.`at`) AS `m` FROM `audit_log` AS `log` WHERE (`log`.`ok` IS TRUE)"},
		{"quoted names, concat that skips NULL, literals with a backslash",
			`SELECT c."Mixed Case" AS "Odd name", concat(c.a, c.b) AS ab, round((c.x)::numeric, 2) AS r, (c.created)::timestamp(3) without time zone AS ts, NULL::text AS nothing, E'tab\there' AS e, 'back\slash' AS b FROM shop.customers c ORDER BY c.id LIMIT 10`,
			"SELECT `c`.`Mixed Case` AS `Odd name`, CONCAT_WS('', `c`.`a`, `c`.`b`) AS `ab`, ROUND(CAST((`c`.`x`) AS DECIMAL(65,30)), 2) AS `r`, CAST((`c`.`created`) AS DATETIME(3)) AS `ts`, NULL AS `nothing`, 'tab\there' AS `e`, 'back\\\\slash' AS `b` FROM `customers` `c` ORDER BY `c`.`id` LIMIT 10"},
		{"a column named like a construct of another dialect",
			"SELECT customers.level, customers.start, customers.filter FROM shop.customers",
			"SELECT `customers`.`level`, `customers`.`start`, `customers`.`filter` FROM `customers`"},
	})
	checkRefusals(t, o, []refusal{
		{"window function", "SELECT c.id, row_number() OVER (ORDER BY c.id) AS n FROM shop.customers c", "ROW_NUMBER()"},
		{"aggregate filter", "SELECT count(*) FILTER (WHERE (c.id > 1)) AS n FROM shop.customers c", "FILTER"},
		{"array column", "SELECT c.tags[1] AS first FROM shop.customers c", "array"},
		{"array type", "SELECT (c.tags)::text[] AS tags FROM shop.customers c", "array"},
		{"json operator", "SELECT (c.profile ->> 'n'::text) AS n FROM shop.customers c", "operator ->>"},
		{"regular expression", "SELECT c.id FROM shop.customers c WHERE (c.name ~ '^a'::text)", "operator ~"},
		{"case-insensitive like", "SELECT c.id FROM shop.customers c WHERE (c.name ~~* 'a%'::text)", "operator ~~*"},
		{"interval", "SELECT (c.born + '1 day'::interval) AS d FROM shop.customers c", "casts to interval"},
		{"time zone", "SELECT (c.seen)::timestamp with time zone AS d FROM shop.customers c", "casts to timestamp with time zone"},
		{"own function", "SELECT shop.tax(c.id) AS t FROM shop.customers c", "a function defined in the source database"},
		{"own type", "SELECT (c.kind)::shop.mood AS k FROM shop.customers c", "a type defined in the source database"},
		{"set-returning function", "SELECT g.g FROM generate_series(1, 3) g(g)", "GENERATE_SERIES()"},
		{"is distinct from", "SELECT c.id FROM shop.customers c WHERE (c.a IS DISTINCT FROM c.b)", "IS DISTINCT FROM"},
		{"greatest skips nulls on the source", "SELECT GREATEST(c.a, c.b) AS g FROM shop.customers c", "GREATEST()"},
		{"full join", "SELECT c.id FROM (shop.customers c FULL JOIN shop.invoices i ON ((i.customer_id = c.id)))", "FULL OUTER JOIN"},
		{"except", "SELECT c.id FROM shop.customers c EXCEPT SELECT i.id FROM shop.invoices i", "EXCEPT"},
		{"common table expression", "WITH x AS (SELECT 1 AS a) SELECT x.a FROM x", "WITH"},
		{"table outside the migration", "SELECT o.id FROM shop.orders o", "reads shop.orders, which is not part of the migration"},
		{"concatenation next to arithmetic", "SELECT c.a || c.b + 1 AS x FROM shop.customers c", "mixes || with arithmetic"},
		{"dollar quoting", "SELECT $$a$$ AS x", "character"},
	})
}

// Oracle keeps a view's text as it was typed, so these are written the
// way people write them: unquoted names, comments, the old join syntax.
func TestOracleToPostgres(t *testing.T) {
	o := route(Oracle, Postgres)
	checkTranslations(t, o, []translation{
		{"concatenation, NVL, exact division, comments",
			"SELECT c.id, c.first_name || ' ' || c.last_name AS full_name, NVL(c.nick, 'none') nick, c.total / c.qty AS unit -- per item\n FROM customers c /* all of them */ WHERE c.status = 'open' AND c.note IS NOT NULL WITH READ ONLY",
			`SELECT "c"."id", NULLIF(concat("c"."first_name", ' ', "c"."last_name"), '') AS "full_name", COALESCE("c"."nick", 'none') "nick", "c"."total" / ("c"."qty")::numeric AS "unit" FROM "public"."customers" "c" WHERE "c"."status" = 'open' AND "c"."note" IS NOT NULL`},
		{"DECODE, NVL2, SYSDATE, quoted names, the comma join",
			`SELECT DECODE(c.kind, 'a', 1, 'b', 2, 0) AS k, NVL2(c.x, 'y', 'n') AS has, SYSDATE AS now, c."MixedCol", c."UPPER" FROM shop.customers c, invoices i WHERE c.id = i.customer_id AND c.name LIKE 'A%' ORDER BY c.id NULLS FIRST`,
			`SELECT CASE WHEN ("c"."kind") IS NOT DISTINCT FROM ('a') THEN 1 WHEN ("c"."kind") IS NOT DISTINCT FROM ('b') THEN 2 ELSE 0 END AS "k", CASE WHEN "c"."x" IS NOT NULL THEN 'y' ELSE 'n' END AS "has", LOCALTIMESTAMP(0) AS "now", "c"."MixedCol", "c"."upper" FROM "public"."customers" "c", "public"."invoices" "i" WHERE "c"."id" = "i"."customer_id" AND "c"."name" LIKE 'A%' ORDER BY "c"."id" NULLS FIRST`},
		{"dual is not needed, and an empty string is NULL",
			"SELECT 1 AS one, 'x' || '' AS x FROM dual",
			`SELECT 1 AS "one", NULLIF(concat('x', NULL), '') AS "x"`},
		{"minus", "SELECT id FROM customers MINUS SELECT customer_id FROM invoices",
			`SELECT "id" FROM "public"."customers" EXCEPT SELECT "customer_id" FROM "public"."invoices"`},
		{"rewrites reach into CASE, casts and other schemas",
			"select l.at, cast(l.n as number(10,2)) n, case when l.a / 2 > 1 then l.b || 'x' else q'[it's]' end c from audit.log l",
			`SELECT "l"."at", CAST("l"."n" AS numeric(10,2)) "n", CASE WHEN "l"."a" / (2)::numeric > 1 THEN NULLIF(concat("l"."b", 'x'), '') ELSE 'it''s' END "c" FROM "audit_log" "l"`},
		{"a function name inside a literal, a quoted name and a comment stays as it is",
			`SELECT 'NVL(a, b) || SYSDATE' AS "NVL(x)", c.id /* NVL(c.id, 0) FROM dual */ FROM customers c`,
			`SELECT 'NVL(a, b) || SYSDATE' AS "NVL(x)", "c"."id" FROM "public"."customers" "c"`},
		{"a table without an alias, known under another name",
			"select log.at, customers.id from audit.log join customers on customers.id = log.cid",
			`SELECT "log"."at", "customers"."id" FROM "audit_log" AS "log" JOIN "public"."customers" ON "customers"."id" = "log"."cid"`},
	})
	checkRefusals(t, o, []refusal{
		{"old outer join", "SELECT c.id FROM customers c, invoices i WHERE c.id = i.customer_id (+)", "(+) outer join"},
		{"hierarchy", "SELECT c.id FROM customers c CONNECT BY PRIOR c.id = c.parent", "CONNECT BY"},
		{"rownum", "SELECT c.id FROM customers c WHERE ROWNUM < 10", "ROWNUM"},
		{"to_char", "SELECT TO_CHAR(c.born, 'YYYY') AS y FROM customers c", "TO_CHAR()"},
		{"package function", "SELECT billing.tax(c.id) AS t FROM customers c", "a function defined in the source database"},
		{"analytic function", "SELECT c.id, RANK() OVER (ORDER BY c.id) r FROM customers c", "RANK()"},
		{"listagg", "SELECT LISTAGG(c.name, ',') WITHIN GROUP (ORDER BY c.name) n FROM customers c", "LISTAGG()"},
		{"database link", "SELECT c.id FROM customers@remote c", "@"},
		{"interval literal", "SELECT c.born + INTERVAL '1' DAY AS d FROM customers c", "INTERVAL"},
		{"fetch first", "SELECT c.id FROM customers c FETCH FIRST 5 ROWS ONLY", "FETCH"},
		{"cast to text", "SELECT CAST(c.id AS VARCHAR2(10)) AS t FROM customers c", "casts to VARCHAR2"},
		{"substr from the end", "SELECT SUBSTR(c.name, -3) AS s FROM customers c", "plain positive number"},
		{"greatest", "SELECT GREATEST(c.a, c.b) AS g FROM customers c", "GREATEST()"},
		{"like with a backslash", `SELECT c.id FROM customers c WHERE c.name LIKE 'a\_%'`, "backslash"},
		{"concatenation next to arithmetic", "SELECT c.a + c.b || 'x' AS x FROM customers c", "mixes || with arithmetic"},
		{"dual joined", "SELECT c.id FROM dual, customers c", "DUAL"},
		{"with check option", "SELECT c.id FROM customers c WITH CHECK OPTION", "WITH"},
		{"subquery factoring", "WITH x AS (SELECT 1 a FROM dual) SELECT a FROM x", "WITH"},
		{"table outside the migration", "SELECT o.id FROM orders o", "reads SHOP.ORDERS, which is not part of the migration"},
		{"synonym in another schema", "SELECT o.id FROM hr.orders o", "reads HR.ORDERS, which is not part of the migration"},
	})
}

func TestOracleToMySQL(t *testing.T) {
	o := route(Oracle, MySQL)
	checkTranslations(t, o, []translation{
		{"concatenation that takes NULL for an empty string",
			"SELECT c.id, c.first_name || ' ' || c.last_name AS full_name, NVL(c.nick, 'none') nick, c.total / c.qty AS unit FROM customers c WHERE c.status = 'open'",
			"SELECT `c`.`id`, NULLIF(CONCAT_WS('', `c`.`first_name`, ' ', `c`.`last_name`), '') AS `full_name`, COALESCE(`c`.`nick`, 'none') `nick`, `c`.`total` / `c`.`qty` AS `unit` FROM `customers` `c` WHERE `c`.`status` = 'open'"},
		{"functions",
			"SELECT DECODE(c.kind, 'a', 1, 'b', 2) AS k, SYSDATE AS now, LENGTH(c.name) n, SUBSTR(c.name, 1, 3) s, TRUNC(c.x) t, GREATEST(c.a, c.b) g, COUNT(*) cnt FROM shop.customers c GROUP BY c.kind",
			"SELECT CASE WHEN (`c`.`kind`) <=> ('a') THEN 1 WHEN (`c`.`kind`) <=> ('b') THEN 2 END AS `k`, NOW() AS `now`, CHAR_LENGTH(`c`.`name`) `n`, SUBSTRING(`c`.`name`, 1, 3) `s`, TRUNCATE(`c`.`x`, 0) `t`, GREATEST(`c`.`a`, `c`.`b`) `g`, COUNT(*) `cnt` FROM `customers` `c` GROUP BY `c`.`kind`"},
		{"dual exists in MySQL", "SELECT 1 AS one FROM dual", "SELECT 1 AS `one` FROM DUAL"},
		{"a literal with a backslash", `SELECT 'a\b' AS x FROM dual`, "SELECT 'a\\\\b' AS `x` FROM DUAL"},
		{"join across schemas",
			"select l.at from audit.log l join customers on customers.id = l.cid",
			"SELECT `l`.`at` FROM `audit_log` `l` JOIN `customers` ON `customers`.`id` = `l`.`cid`"},
		{"cast to a number", "SELECT CAST(c.x AS NUMBER(12,2)) AS x, CAST(c.y AS INTEGER) AS y FROM customers c",
			"SELECT CAST(`c`.`x` AS DECIMAL(12,2)) AS `x`, CAST(`c`.`y` AS DECIMAL(38,0)) AS `y` FROM `customers` `c`"},
	})
	checkRefusals(t, o, []refusal{
		{"minus", "SELECT id FROM customers MINUS SELECT customer_id FROM invoices", "MINUS"},
		{"nulls first", "SELECT c.id FROM customers c ORDER BY c.id NULLS FIRST", "NULLS FIRST"},
		{"full join", "SELECT c.id FROM customers c FULL OUTER JOIN invoices i ON i.customer_id = c.id", "FULL OUTER JOIN"},
		{"number without a precision", "SELECT CAST(c.x AS NUMBER) AS x FROM customers c", "casts to NUMBER"},
		{"to_date", "SELECT TO_DATE('2024-01-01', 'YYYY-MM-DD') AS d FROM dual", "TO_DATE()"},
		{"old outer join", "SELECT c.id FROM customers c, invoices i WHERE c.id = i.customer_id(+)", "(+) outer join"},
	})
}

func TestTranslateReportsWhatIsRead(t *testing.T) {
	_, reads, err := Translate("select c.id from customers c join audit.log l on l.cid = c.id where exists (select 1 from invoices i, customers c2)", route(Oracle, Postgres))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range reads {
		names = append(names, r.String())
	}
	if got, want := strings.Join(names, " "), "SHOP.CUSTOMERS AUDIT.LOG SHOP.INVOICES"; got != want {
		t.Errorf("reads %q, want %q", got, want)
	}
}

func TestLexKeepsLiteralsWhole(t *testing.T) {
	cases := []struct {
		dialect Dialect
		in      string
		want    []string
	}{
		{MySQL, "select `a``b`, 'it''s', \"dq\", 'x\\ny' -- gone\n# gone too\n/* and this */ from t;", []string{"select", "a`b", ",", "it's", ",", "dq", ",", "x\ny", "from", "t"}},
		{Postgres, `select "a""b", 'x\ny', E'x\ny', 1.5e-3::numeric`, []string{"select", `a"b`, ",", `x\ny`, ",", "x\ny", ",", "1.5e-3", "::", "numeric"}},
		{Oracle, `select q'{it's}', n'x', "Col", a||b from t`, []string{"select", "it's", ",", "x", ",", "Col", ",", "a", "||", "b", "from", "t"}},
	}
	for _, c := range cases {
		toks, err := lex(c.dialect, c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		var got []string
		for _, tok := range toks {
			got = append(got, tok.text)
		}
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("%s:\n  got: %q\n want: %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"select 'never ends", `select "never ends`, "select /* never ends", "select (1", "select 1)"} {
		toks, err := lex(Oracle, bad)
		if err == nil {
			_, err = nest(toks)
		}
		if err == nil {
			t.Errorf("%s: read without complaint", bad)
		}
	}
}
