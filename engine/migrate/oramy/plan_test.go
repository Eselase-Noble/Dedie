package oramy

import (
	"strings"
	"testing"

	"dedie/engine/migrate/orasrc"
)

// col makes a source column the way the catalog would describe it.
func col(name, dataType string, mutate ...func(*orasrc.Column)) orasrc.Column {
	c := orasrc.Column{Name: name, DataType: dataType, Nullable: true}
	c.Kind, _ = orasrc.Classify(dataType)
	for _, m := range mutate {
		m(&c)
	}
	return c
}

func chars(n int64) func(*orasrc.Column) { return func(c *orasrc.Column) { c.CharLength = n } }
func digits(p, s int64) func(*orasrc.Column) {
	return func(c *orasrc.Column) { c.Precision, c.Scale = num(p), num(s) }
}
func def(d string) func(*orasrc.Column) { return func(c *orasrc.Column) { c.Default = d } }
func notNull(c *orasrc.Column)          { c.Nullable = false }

const current = 80046

func noteWith(p *Plan, fragments ...string) bool {
next:
	for _, n := range p.Notes {
		for _, f := range fragments {
			if !strings.Contains(n, f) {
				continue next
			}
		}
		return true
	}
	return false
}

func ddl(p *Plan, list []Stmt, name string) string {
	for _, s := range list {
		if s.Name == name {
			return s.SQL
		}
	}
	return ""
}

func TestPlanTable(t *testing.T) {
	orders := &orasrc.Table{Owner: "SHOP", Name: "ORDERS", PK: []int{0}, Cols: []orasrc.Column{
		col("ID", "NUMBER", notNull, func(c *orasrc.Column) { c.IdentitySeq = "ISEQ$$_1" }),
		col("CODE", "VARCHAR2", chars(20), notNull, def("'new'")),
		col("PATH", "VARCHAR2", chars(100), def(`'C:\tmp'`)),
		col("TOTAL", "NUMBER", digits(12, 2), def("0")),
		col("TICKET", "NUMBER", def(`"SHOP"."ORDER_SEQ"."NEXTVAL"`)),
		col("PLACED", "DATE", def("SYSDATE")),
		col("SEEN", "TIMESTAMP(3)", func(c *orasrc.Column) { c.Scale = num(3) }, def("SYSTIMESTAMP")),
		col("ZONED", "TIMESTAMP(6) WITH TIME ZONE", func(c *orasrc.Column) { c.Scale = num(6) }, def("CURRENT_TIMESTAMP")),
		col("TOKEN", "RAW", def("SYS_GUID()")),
		col("NOTE", "CLOB", def("'none'")),
		col("ODD", "VARCHAR2", chars(10), def("TO_CHAR(SYSDATE, 'YYYY')")),
		col("CODE_UPPER", "VARCHAR2", chars(20), func(c *orasrc.Column) { c.Virtual, c.Default = true, `UPPER("CODE")` }),
		col("Mixed", "VARCHAR2", chars(5)),
	}}
	cat := &orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{orders},
		Checks: []orasrc.Check{{Owner: "SHOP", Table: "ORDERS", Name: "ORDERS_TOTAL", Condition: `"TOTAL" >= 0 AND code <> 'x'`}}}
	p := newPlan(cat, map[string]int64{"SHOP.ORDERS.TOKEN": 16}, current, nil)

	want := "CREATE TABLE `orders` (\n" +
		"  `id` bigint NOT NULL AUTO_INCREMENT,\n" +
		"  `code` varchar(20) NOT NULL DEFAULT 'new',\n" +
		"  `path` varchar(100) DEFAULT 'C:\\\\tmp',\n" +
		"  `total` decimal(12,2) DEFAULT 0,\n" +
		"  `ticket` decimal(65,30),\n" +
		"  `placed` datetime DEFAULT CURRENT_TIMESTAMP,\n" +
		"  `seen` datetime(3) DEFAULT CURRENT_TIMESTAMP(3),\n" +
		"  `zoned` datetime(6) DEFAULT (UTC_TIMESTAMP(6)),\n" +
		"  `token` varbinary(16) DEFAULT (UNHEX(REPLACE(UUID(), '-', ''))),\n" +
		"  `note` longtext DEFAULT ('none'),\n" +
		"  `odd` varchar(10),\n" +
		"  `code_upper` varchar(20),\n" +
		"  `Mixed` varchar(5),\n" +
		"  PRIMARY KEY (`id`)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin"
	if got := ddl(p, p.Pre, "orders"); got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}
	if got := ddl(p, p.Post, "orders orders_total"); got != "ALTER TABLE `orders` ADD CONSTRAINT `orders_total` CHECK (`total` >= 0 AND code <> 'x')" {
		t.Errorf("check constraint: %s", got)
	}
	for _, fragments := range [][]string{
		{"SHOP.ORDERS.TICKET", "without a precision"},
		{"SHOP.ORDERS.TICKET", "NEXTVAL", "MySQL has no sequences"},
		{"SHOP.ORDERS.ODD", "TO_CHAR(SYSDATE, 'YYYY')", "was not translated"},
		{"SHOP.ORDERS.CODE_UPPER", "virtual column"},
		{"SHOP.ORDERS.ZONED", "in UTC"},
	} {
		if !noteWith(p, fragments...) {
			t.Errorf("no note with %q in:\n%s", fragments, strings.Join(p.Notes, "\n"))
		}
	}

	// A release without expression defaults and enforced checks, and
	// without the collation that counts trailing blanks.
	old := newPlan(cat, nil, 80011, nil)
	got := ddl(old, old.Pre, "orders")
	for _, piece := range []string{"`zoned` datetime(6),", "`token` varbinary(2000),", "`note` longtext,", "COLLATE=utf8mb4_bin"} {
		if !strings.Contains(got, piece) {
			t.Errorf("MySQL 8.0.11: %q missing from\n%s", piece, got)
		}
	}
	if len(old.Post) != 0 || !noteWith(old, "ORDERS_TOTAL", "8.0.16") {
		t.Errorf("MySQL 8.0.11: check constraint kept: %v\n%s", old.Post, strings.Join(old.Notes, "\n"))
	}
}

// An identity column that does not lead the primary key needs an index
// of its own, and keeps a small integer type when it has one.
func TestPlanIdentity(t *testing.T) {
	tbl := &orasrc.Table{Owner: "SHOP", Name: "LINES", PK: []int{0, 1}, Cols: []orasrc.Column{
		col("ORDER_ID", "NUMBER", digits(9, 0), notNull),
		col("LINE", "NUMBER", digits(4, 0), notNull, func(c *orasrc.Column) { c.IdentitySeq = "ISEQ$$_2" }),
	}}
	p := newPlan(&orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{tbl}}, nil, current, nil)
	got := ddl(p, p.Pre, "lines")
	for _, piece := range []string{"`line` smallint NOT NULL AUTO_INCREMENT", "PRIMARY KEY (`order_id`, `line`)", "KEY (`line`)"} {
		if !strings.Contains(got, piece) {
			t.Errorf("%q missing from\n%s", piece, got)
		}
	}
}

// Declared lengths that add up to more than a MySQL row can hold move
// the longest columns into TEXT, sparing the key and what is indexed.
func TestPlanFitRow(t *testing.T) {
	wide := &orasrc.Table{Owner: "SHOP", Name: "WIDE", PK: []int{0}, Cols: []orasrc.Column{
		col("CODE", "VARCHAR2", chars(100), notNull),
		col("A", "VARCHAR2", chars(4000)),
		col("B", "VARCHAR2", chars(4000)),
		col("C", "VARCHAR2", chars(4000)),
		col("D", "VARCHAR2", chars(4000)),
		col("E", "VARCHAR2", chars(3000)),
		col("TAG", "VARCHAR2", chars(30)),
		col("BODY", "CLOB"),
	}}
	cat := &orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{wide}, Indexes: []orasrc.Index{
		{Owner: "SHOP", Table: "WIDE", Name: "WIDE_A", Type: "NORMAL", Columns: []string{"A"}},
		{Owner: "SHOP", Table: "WIDE", Name: "WIDE_E", Type: "NORMAL", Columns: []string{"E", "TAG"}},
		{Owner: "SHOP", Table: "WIDE", Name: "WIDE_TAG", Type: "NORMAL", Unique: true, Columns: []string{"TAG", "CODE"}},
		{Owner: "SHOP", Table: "WIDE", Name: "WIDE_BODY", Type: "DOMAIN", Columns: []string{"BODY"}},
		{Owner: "SHOP", Table: "WIDE", Name: "WIDE_FN", Type: "FUNCTION-BASED NORMAL", Columns: []string{"SYS_NC00009$"}},
		{Owner: "SHOP", Table: "WIDE", Name: "WIDE_BITS", Type: "BITMAP", Columns: []string{"TAG"}},
	}}
	p := newPlan(cat, nil, current, nil)
	var types []string
	for _, c := range p.Table("SHOP", "WIDE").Cols {
		types = append(types, c.Target+" "+c.Type.DDL)
	}
	// 5 * 16,002 bytes do not fit; without B the rest does. A and E are
	// indexed, so B, C and D are first in line, the longest of them first.
	if got, want := strings.Join(types, ", "), "code varchar(100), a varchar(4000), b text, c varchar(4000), d varchar(4000), e varchar(3000), tag varchar(30), body longtext"; got != want {
		t.Errorf("columns: %s\n     want: %s", got, want)
	}
	if !noteWith(p, "SHOP.WIDE.B", "stored as TEXT instead of VARCHAR(4000)") {
		t.Errorf("no note for the moved column in:\n%s", strings.Join(p.Notes, "\n"))
	}
	var made []string
	for _, s := range p.Post {
		made = append(made, s.SQL)
	}
	want := []string{
		"CREATE UNIQUE INDEX `wide_tag` ON `wide` (`tag`, `code`)",
		"CREATE INDEX `wide_bits` ON `wide` (`tag`)",
	}
	if strings.Join(made, "\n") != strings.Join(want, "\n") {
		t.Errorf("indexes:\n%s\nwant:\n%s", strings.Join(made, "\n"), strings.Join(want, "\n"))
	}
	for _, fragments := range [][]string{
		{"WIDE_A", "16000 bytes"},
		{"WIDE_E", "12120 bytes"},
		{"WIDE_BODY", "domain index"},
		{"WIDE_FN", "function-based"},
		{"WIDE_BITS", "B-tree"},
	} {
		if !noteWith(p, fragments...) {
			t.Errorf("no note with %q in:\n%s", fragments, strings.Join(p.Notes, "\n"))
		}
	}
	if len(p.LongKeys) != 0 {
		t.Errorf("long keys: %v", p.LongKeys)
	}

	// A primary key MySQL cannot build is for preflight to refuse.
	long := &orasrc.Table{Owner: "SHOP", Name: "LONGKEY", PK: []int{0}, Cols: []orasrc.Column{col("PATH", "VARCHAR2", chars(1000), notNull)}}
	p = newPlan(&orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{long}}, nil, current, nil)
	if len(p.LongKeys) != 1 || p.LongKeys[0] != "SHOP.LONGKEY" {
		t.Errorf("long keys: %v", p.LongKeys)
	}
}

func TestPlanForeignKeys(t *testing.T) {
	regions := &orasrc.Table{Owner: "SHOP", Name: "REGIONS", PK: []int{0}, Cols: []orasrc.Column{col("ID", "NUMBER", digits(4, 0), notNull)}}
	customers := &orasrc.Table{Owner: "SHOP", Name: "CUSTOMERS", PK: []int{0}, Cols: []orasrc.Column{
		col("ID", "NUMBER", notNull, func(c *orasrc.Column) { c.IdentitySeq = "ISEQ$$_1" }),
		col("REGION", "NUMBER", digits(9, 0)),
	}}
	invoices := &orasrc.Table{Owner: "SHOP", Name: "INVOICES", PK: []int{0}, Cols: []orasrc.Column{
		col("ID", "NUMBER", digits(18, 0), notNull),
		col("CUSTOMER_ID", "NUMBER", notNull),
		col("REGION", "NUMBER"),
		col("VENDOR", "NUMBER"),
	}}
	cat := &orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{customers, invoices, regions}, ForeignKeys: []orasrc.ForeignKey{
		// Listed before the key it leans on is itself aligned.
		{Owner: "SHOP", Table: "INVOICES", Name: "INV_REGION", Columns: []string{"REGION"}, RefOwner: "SHOP", RefTable: "CUSTOMERS", RefColumns: []string{"REGION"}, DeleteRule: "SET NULL", Deferrable: true},
		{Owner: "SHOP", Table: "INVOICES", Name: "INV_CUST", Columns: []string{"CUSTOMER_ID"}, RefOwner: "SHOP", RefTable: "CUSTOMERS", RefColumns: []string{"ID"}, DeleteRule: "CASCADE"},
		{Owner: "SHOP", Table: "CUSTOMERS", Name: "CUST_REGION", Columns: []string{"REGION"}, RefOwner: "SHOP", RefTable: "REGIONS", RefColumns: []string{"ID"}, DeleteRule: "NO ACTION"},
		{Owner: "SHOP", Table: "INVOICES", Name: "INV_VENDOR", Columns: []string{"VENDOR"}, RefOwner: "OTHER", RefTable: "VENDORS", RefColumns: []string{"ID"}},
	}}
	p := newPlan(cat, nil, current, nil)
	var types []string
	for _, c := range p.Table("SHOP", "INVOICES").Cols {
		types = append(types, c.Target+" "+c.Type.DDL)
	}
	if got, want := strings.Join(types, ", "), "id bigint, customer_id bigint, region smallint, vendor decimal(65,30)"; got != want {
		t.Errorf("columns: %s, want %s", got, want)
	}
	var made []string
	for _, s := range p.Final {
		made = append(made, s.SQL)
	}
	want := []string{
		"ALTER TABLE `invoices` ADD CONSTRAINT `inv_region` FOREIGN KEY (`region`) REFERENCES `customers` (`region`) ON DELETE SET NULL",
		"ALTER TABLE `invoices` ADD CONSTRAINT `inv_cust` FOREIGN KEY (`customer_id`) REFERENCES `customers` (`id`) ON DELETE CASCADE",
		"ALTER TABLE `customers` ADD CONSTRAINT `cust_region` FOREIGN KEY (`region`) REFERENCES `regions` (`id`)",
	}
	if strings.Join(made, "\n") != strings.Join(want, "\n") {
		t.Errorf("foreign keys:\n%s\nwant:\n%s", strings.Join(made, "\n"), strings.Join(want, "\n"))
	}
	for _, fragments := range [][]string{
		{"SHOP.INVOICES.CUSTOMER_ID", "stored as BIGINT instead of DECIMAL(65,30)", "SHOP.CUSTOMERS.ID"},
		{"SHOP.INVOICES.REGION", "stored as SMALLINT instead of DECIMAL(65,30)"},
		{"SHOP.CUSTOMERS.REGION", "stored as SMALLINT instead of INT", "SHOP.REGIONS.ID"},
		{"INV_VENDOR", "OTHER.VENDORS", "left out"},
		{"INV_REGION", "deferrable"},
	} {
		if !noteWith(p, fragments...) {
			t.Errorf("no note with %q in:\n%s", fragments, strings.Join(p.Notes, "\n"))
		}
	}
}

func TestPlanCollisions(t *testing.T) {
	table := func(owner, name string, cols ...string) *orasrc.Table {
		t := &orasrc.Table{Owner: owner, Name: name}
		for _, c := range cols {
			t.Cols = append(t.Cols, col(c, "NUMBER"))
		}
		return t
	}
	cat := &orasrc.Catalog{Owners: []string{"A", "B"}, Tables: []*orasrc.Table{
		table("A", "ORDERS", "ID"), table("A", "Orders", "ID"), table("A", "PLAIN", "ID", "Id"), table("B", "ORDERS", "ID"),
	}}
	p := newPlan(cat, nil, current, nil)
	if got, want := strings.Join(p.Collisions, "; "), "columns ID and Id of A.PLAIN; A.ORDERS and B.ORDERS"; got != want {
		t.Errorf("collisions: %s, want %s", got, want)
	}
	if got, want := strings.Join(p.CaseCollisions, "; "), "A.ORDERS and A.Orders and B.ORDERS"; got != want {
		t.Errorf("case collisions: %s, want %s", got, want)
	}
}

func TestKeyExprs(t *testing.T) {
	tbl := &orasrc.Table{Owner: "SHOP", Name: "KEYS", PK: []int{0, 1, 2, 3, 4}, Cols: []orasrc.Column{
		col("ID", "NUMBER", digits(9, 0)), col("FREE", "NUMBER"), col("PRICE", "NUMBER", digits(12, 2)),
		col("CODE", "CHAR", chars(3)), col("DAY", "DATE"), col("NAME", "VARCHAR2", chars(20)),
	}}
	p := newPlan(&orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{tbl}}, nil, current, nil)
	source, target, ok := keyExprs(p.Tables[0], true)
	want := []string{
		"CAST(`id` AS CHAR)",
		"TRIM(TRAILING '.' FROM TRIM(TRAILING '0' FROM CAST(ROUND(`free`, 20) AS CHAR)))",
		"TRIM(TRAILING '.' FROM TRIM(TRAILING '0' FROM CAST(`price` AS CHAR)))",
		"TRIM(TRAILING ' ' FROM `code`)",
		"DATE_FORMAT(`day`, '%Y-%m-%d %H:%i:%s')",
	}
	if !ok || len(source) != len(want) || strings.Join(target, "\n") != strings.Join(want, "\n") {
		t.Errorf("key expressions (ok %v):\n%s\nwant:\n%s", ok, strings.Join(target, "\n"), strings.Join(want, "\n"))
	}
	// Without UTF-8 on the source a text key does not hash alike, and a
	// DOUBLE never prints as the NUMBER did.
	if _, _, ok := keyExprs(p.Tables[0], false); ok {
		t.Error("a CHAR key compared without UTF-8 on the source")
	}
	approx := &orasrc.Table{Owner: "SHOP", Name: "APPROX", PK: []int{0}, Cols: []orasrc.Column{col("ID", "NUMBER", digits(38, 35))}}
	p = newPlan(&orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{approx}}, nil, current, nil)
	if _, _, ok := keyExprs(p.Tables[0], true); ok {
		t.Error("a key stored as DOUBLE compared by its text")
	}
}

func TestDeleteSQL(t *testing.T) {
	one := &orasrc.Table{Owner: "SHOP", Name: "ONE", PK: []int{0}, Cols: []orasrc.Column{col("ID", "NUMBER", digits(9, 0))}}
	two := &orasrc.Table{Owner: "SHOP", Name: "TWO", PK: []int{1, 0}, Cols: []orasrc.Column{col("DAY", "DATE"), col("CODE", "VARCHAR2", chars(5))}}
	p := newPlan(&orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{one, two}}, nil, current, nil)
	if got, want := deleteSQL(p.Tables[0], 3), "DELETE FROM `one` WHERE `id` IN (CAST(? AS SIGNED), CAST(? AS SIGNED), CAST(? AS SIGNED))"; got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
	if got, want := deleteSQL(p.Tables[1], 2), "DELETE FROM `two` WHERE (`code`, `day`) IN ((?, CAST(? AS datetime)), (?, CAST(? AS datetime)))"; got != want {
		t.Errorf("%s\nwant %s", got, want)
	}
}

// A table left out because the target already has it can still be
// referred to, and a constraint whose name the target keeps is not made.
func TestPlanExisting(t *testing.T) {
	invoices := &orasrc.Table{Owner: "SHOP", Name: "INVOICES", PK: []int{0}, Cols: []orasrc.Column{
		col("ID", "NUMBER", digits(18, 0), notNull), col("CUSTOMER_ID", "NUMBER", notNull), col("AMOUNT", "NUMBER", digits(12, 2)),
	}}
	cat := &orasrc.Catalog{Owners: []string{"SHOP"}, Tables: []*orasrc.Table{invoices},
		ForeignKeys: []orasrc.ForeignKey{
			{Owner: "SHOP", Table: "INVOICES", Name: "INV_CUST", Columns: []string{"CUSTOMER_ID"}, RefOwner: "SHOP", RefTable: "CUSTOMERS", RefColumns: []string{"ID"}, DeleteRule: "CASCADE"},
			{Owner: "SHOP", Table: "INVOICES", Name: "INV_VENDOR", Columns: []string{"ID"}, RefOwner: "SHOP", RefTable: "VENDORS", RefColumns: []string{"ID"}},
		},
		Checks: []orasrc.Check{
			{Owner: "SHOP", Table: "INVOICES", Name: "INV_AMOUNT", Condition: "amount >= 0"},
			{Owner: "SHOP", Table: "INVOICES", Name: "INV_ID", Condition: "id > 0"},
		}}
	p := newPlan(cat, nil, current, []string{"SHOP.CUSTOMERS"})
	if len(p.Final) != 1 || p.Final[0].SQL != "ALTER TABLE `invoices` ADD CONSTRAINT `inv_cust` FOREIGN KEY (`customer_id`) REFERENCES `customers` (`id`) ON DELETE CASCADE" {
		t.Errorf("foreign key to a kept table: %v", p.Final)
	}
	if !noteWith(p, "INV_VENDOR", "left out") || noteWith(p, "INV_CUST") {
		t.Errorf("notes:\n%s", strings.Join(p.Notes, "\n"))
	}
	p.leaveOut([]string{"check constraint inv_amount", "foreign key inv_id"})
	if len(p.Post) != 1 || p.Post[0].constraint != "inv_id" || len(p.Final) != 1 {
		t.Errorf("after leaving out: %v %v", p.Post, p.Final)
	}
	p.leaveOut([]string{"foreign key INV_CUST"})
	if len(p.Final) != 0 {
		t.Errorf("foreign key kept: %v", p.Final)
	}
}
