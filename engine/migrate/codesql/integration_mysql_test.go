package codesql

import (
	"context"
	"testing"
)

// The MySQL fixture, and the same tables as the MySQL to PostgreSQL
// pipeline creates them: TINYINT becomes smallint, DATETIME timestamp,
// DECIMAL numeric.

var shopTables = []string{"customers", "orders", "stock", "audit"}

var mysqlFixture = []string{
	`CREATE TABLE customers (id INT PRIMARY KEY, name VARCHAR(60), email VARCHAR(120), tier VARCHAR(10),
		credit DECIMAL(12,2), created DATETIME, active TINYINT(1))`,
	`CREATE TABLE orders (id INT PRIMARY KEY, customer_id INT, sku VARCHAR(20), qty INT, amount DECIMAL(12,2),
		placed DATE, state VARCHAR(12))`,
	`CREATE TABLE stock (sku VARCHAR(20) PRIMARY KEY, qty INT, price DECIMAL(10,2), note VARCHAR(100))`,
	`CREATE TABLE audit (seq INT, tbl VARCHAR(30), action VARCHAR(10), key_id INT, old_val VARCHAR(200), new_val VARCHAR(200))`,
}

var mysqlPGFixture = []string{
	`CREATE TABLE customers (id integer PRIMARY KEY, name varchar(60), email varchar(120), tier varchar(10),
		credit numeric(12,2), created timestamp, active smallint)`,
	`CREATE TABLE orders (id integer PRIMARY KEY, customer_id integer, sku varchar(20), qty integer, amount numeric(12,2),
		placed date, state varchar(12))`,
	`CREATE TABLE stock (sku varchar(20) PRIMARY KEY, qty integer, price numeric(10,2), note varchar(100))`,
	`CREATE TABLE audit (seq integer, tbl varchar(30), action varchar(10), key_id integer, old_val varchar(200), new_val varchar(200))`,
}

var shopSeed = []string{
	`INSERT INTO customers VALUES (1, 'Ama Mensah', 'ama@example.com', 'gold', 500.00, TIMESTAMP '2023-01-15 09:30:00', 1)`,
	`INSERT INTO customers VALUES (2, 'Kofi Boateng', 'kofi@example.com', 'silver', 120.50, TIMESTAMP '2023-06-01 14:00:00', 1)`,
	`INSERT INTO customers VALUES (3, 'Esi Owusu', NULL, NULL, 0, TIMESTAMP '2024-02-29 00:00:00', 0)`,
	`INSERT INTO orders VALUES (10, 1, 'BOLT-8', 4, 19.96, DATE '2024-01-10', 'open')`,
	`INSERT INTO orders VALUES (11, 1, 'NUT-8', 100, 12.00, DATE '2024-03-05', 'shipped')`,
	`INSERT INTO orders VALUES (12, 2, 'BOLT-8', 1, 4.99, DATE '2024-03-20', 'open')`,
	`INSERT INTO orders VALUES (13, 2, 'GEAR-1', 2, 150.00, DATE '2023-12-31', 'open')`,
	`INSERT INTO stock VALUES ('BOLT-8', 250, 4.99, NULL)`,
	`INSERT INTO stock VALUES ('NUT-8', 1000, 0.12, 'bulk only')`,
	`INSERT INTO stock VALUES ('GEAR-1', 3, 75.00, NULL)`,
}

var mysqlCases = []routineCase{
	{kind: "function", name: "valid_email", want: Converted, ddl: `
CREATE FUNCTION valid_email(p_email VARCHAR(120)) RETURNS TINYINT DETERMINISTIC
BEGIN
  DECLARE v_at INT;
  -- An address needs one @ with something on both sides and a dot after it.
  IF p_email IS NULL OR CHAR_LENGTH(TRIM(p_email)) < 3 THEN
    RETURN 0;
  END IF;
  SET v_at = LOCATE('@', p_email);
  IF v_at <= 1 OR INSTR(p_email, ' ') > 0 THEN
    RETURN 0;
  END IF;
  RETURN p_email LIKE '%@%.%' AND NOT (p_email LIKE '%@%@%') AND v_at < CHAR_LENGTH(p_email);
END`,
		steps: []step{{fn: `valid_email('ama@example.com')`}, {fn: `valid_email('ama.example.com')`}, {fn: `valid_email('@example.com')`},
			{fn: `valid_email('a b@example.com')`}, {fn: `valid_email(NULL)`}, {fn: `valid_email('  ')`}, {fn: `valid_email('a@b@c.d')`}}},

	{kind: "function", name: "price", want: Converted, ddl: `
CREATE FUNCTION price(p_amount DECIMAL(12,2), p_tier VARCHAR(10), p_tax_pct DECIMAL(6,3)) RETURNS DECIMAL(12,2) DETERMINISTIC
BEGIN
  DECLARE v_discount DECIMAL(5,2) DEFAULT 0;
  DECLARE v_net DECIMAL(12,2);
  IF p_amount IS NULL OR p_amount < 0 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'amount must not be negative';
  END IF;
  SET v_discount = CASE p_tier WHEN 'gold' THEN 15 WHEN 'silver' THEN 7.5 ELSE 0 END;
  SET v_net = ROUND(p_amount * (100 - v_discount) / 100, 2);
  RETURN v_net + v_net * IFNULL(p_tax_pct, 12.5) / 100;
END`,
		steps: []step{{fn: `price(100, 'none', NULL)`}, {fn: `price(19.99, 'gold', 12.5)`}, {fn: `price(19.994, 'silver', 0)`},
			{fn: `price(0.05, 'gold', 33.333)`}, {fn: `price(-1, 'gold', 1)`}, {fn: `price(NULL, 'gold', 1)`}, {fn: `price(10, NULL, 7)`}}},

	{kind: "function", name: "due_date", want: Converted, ddl: `
CREATE FUNCTION due_date(p_placed DATE, p_days INT) RETURNS DATE DETERMINISTIC
BEGIN
  DECLARE v_due DATE;
  -- Payment is due a number of days after the order, never on the
  -- first of a month, and at the latest at the end of the next month.
  SET v_due = DATE_ADD(p_placed, INTERVAL IFNULL(p_days, 30) DAY);
  IF DAYOFMONTH(v_due) = 1 THEN
    SET v_due = v_due + INTERVAL 1 DAY;
  END IF;
  IF v_due > LAST_DAY(p_placed + INTERVAL 1 MONTH) THEN
    SET v_due = LAST_DAY(DATE_ADD(p_placed, INTERVAL 1 MONTH));
  END IF;
  IF DATEDIFF(v_due, p_placed) < 0 OR YEAR(v_due) < 2000 THEN
    RETURN NULL;
  END IF;
  RETURN v_due;
END`,
		steps: []step{{fn: `due_date(DATE '2024-01-31', NULL)`}, {fn: `due_date(DATE '2024-01-31', 1)`}, {fn: `due_date(DATE '2024-02-28', 2)`},
			{fn: `due_date(NULL, 3)`}, {fn: `due_date(DATE '2023-12-25', -40)`}, {fn: `due_date(DATE '2024-01-15', 90)`}}},

	{kind: "function", name: "label", want: Converted, ddl: `
CREATE FUNCTION label(p_id INT) RETURNS VARCHAR(200) READS SQL DATA
BEGIN
  DECLARE v_name VARCHAR(60);
  DECLARE v_tier VARCHAR(10);
  DECLARE v_created DATETIME;
  DECLARE v_active TINYINT(1) DEFAULT 0;
  SELECT name, tier, created, active INTO v_name, v_tier, v_created, v_active FROM customers WHERE id = p_id;
  IF v_name IS NULL THEN
    RETURN CONCAT('no customer ', p_id);
  END IF;
  RETURN CONCAT('C', LPAD(p_id, 6, '0'), ' ', UPPER(LEFT(v_name, 1)), '. ', SUBSTRING(v_name, 2, 3), ' [',
    IFNULL(v_tier, 'none'), '] since ', DATE_FORMAT(v_created, '%Y-%m-%d %H:%i'), IF(v_active, '', ' (inactive)'),
    ' ', YEAR(v_created) * 100 + MONTH(v_created));
END`,
		steps: []step{{fn: `label(1)`}, {fn: `label(3)`}, {fn: `label(99)`}}},

	{kind: "function", name: "text_ops", want: Converted, ddl: `
CREATE FUNCTION text_ops(p_s VARCHAR(50), p_n DECIMAL(10,2), p_i INT) RETURNS VARCHAR(600) DETERMINISTIC
RETURN CONCAT_WS('|', SUBSTRING(p_s, 2), SUBSTRING(p_s, 1, 3), MID(p_s, 2, 2), UPPER(p_s), LCASE(p_s), LTRIM(p_s), RTRIM(p_s), TRIM(p_s), TRIM(BOTH 'a' FROM p_s),
  TRIM(LEADING 'c' FROM p_s), LPAD(p_s, 6, '*'), RPAD(p_s, 6, '-'), LPAD(p_s, 2, '*'), REPLACE(p_s, 'a', 'XY'), CHAR_LENGTH(p_s), LENGTH(p_s), INSTR(p_s, 'c'),
  LOCATE('b', p_s), POSITION('ab' IN p_s), IFNULL(p_s, 'null'), IF(p_s IS NULL, 'n', 'y'), p_n, p_i, ROUND(p_n, 1), ROUND(p_n), TRUNCATE(p_n, 1), MOD(p_i, 3),
  p_i DIV 2, p_i % 4, ABS(p_n), SIGN(p_n), CEILING(p_n), FLOOR(p_n), LEFT(p_s, 2), RIGHT(p_s, 2), REVERSE(p_s), REPEAT(p_s, 2), CONCAT(p_s, p_i),
  NULLIF(p_i, 3), p_i > 2, p_s LIKE 'a%', p_s = 'a', ISNULL(p_s), ROUND(p_i / 4, 3), p_n * 3, p_n + p_i, COALESCE(NULL, p_i, 5), CAST(p_n AS SIGNED),
  CAST(p_i AS DECIMAL(8,3)), CASE WHEN p_i BETWEEN 1 AND 5 THEN 'low' WHEN p_i IN (7, 9) THEN 'odd' ELSE 'other' END, p_s <=> NULL, NOT p_i, p_i AND p_n)`,
		steps: []step{{fn: `text_ops('abcabc', 12.5, 7)`}, {fn: `text_ops('  a b  ', 0.25, -4)`}, {fn: `text_ops(NULL, -0.5, 0)`}, {fn: `text_ops('a', 1000, 3)`},
			{fn: `text_ops('caab', -12.345, 10)`}, {fn: `text_ops('', 5.005, NULL)`}}},

	{kind: "function", name: "date_ops", want: Converted, ddl: `
CREATE FUNCTION date_ops(p_d DATE, p_t DATETIME, p_n INT) RETURNS VARCHAR(600) DETERMINISTIC
RETURN CONCAT_WS('|', DATE_ADD(p_d, INTERVAL p_n DAY), p_d - INTERVAL 1 MONTH, DATE_ADD(p_t, INTERVAL 90 MINUTE), p_t + INTERVAL p_n HOUR,
  DATE_SUB(p_t, INTERVAL 1 YEAR), p_d + INTERVAL 1 QUARTER, p_d + INTERVAL 2 WEEK, ADDDATE(p_d, 10), DATEDIFF(p_t, p_d), DATEDIFF(p_d, '2024-01-01'),
  TIMESTAMPDIFF(HOUR, p_d, p_t), TIMESTAMPDIFF(DAY, p_t, p_d), TIMESTAMPDIFF(MINUTE, p_d, p_t), TIMESTAMPDIFF(SECOND, p_t, p_d), YEAR(p_d), MONTH(p_d), DAY(p_d),
  HOUR(p_t), MINUTE(p_t), SECOND(p_t), QUARTER(p_d), DAYOFWEEK(p_d), WEEKDAY(p_d), DAYOFYEAR(p_d), LAST_DAY(p_d), DATE(p_t),
  DATE_FORMAT(p_t, '%Y-%m-%d %H:%i:%s'), DATE_FORMAT(p_t, '%d/%m/%y %h:%i %p, %W %e %M %b %a %j %c'), EXTRACT(YEAR FROM p_t), p_d < p_t, p_d + INTERVAL 36 HOUR,
  LAST_DAY(p_d + INTERVAL 1 MONTH), DATE_ADD(p_d, INTERVAL 1 MONTH) = p_t)`,
		steps: []step{{fn: `date_ops(DATE '2024-02-29', TIMESTAMP '2024-03-01 13:45:10', 3)`}, {fn: `date_ops(DATE '2024-12-31', TIMESTAMP '2024-12-31 23:59:59', -5)`},
			{fn: `date_ops(DATE '2023-01-01', TIMESTAMP '2022-06-15 00:00:00', 0)`}, {fn: `date_ops(NULL, TIMESTAMP '2024-07-04 06:30:00', 1)`},
			{fn: `date_ops(DATE '2024-01-31', NULL, 1)`}}},

	{kind: "procedure", name: "late_fees", want: Converted, ddl: `
CREATE PROCEDURE late_fees(IN p_before DATE, IN p_fee DECIMAL(10,2), OUT p_changed INT, OUT p_total DECIMAL(12,2))
BEGIN
  DECLARE v_id INT;
  DECLARE v_amount DECIMAL(12,2);
  DECLARE done INT DEFAULT FALSE;
  DECLARE c_open CURSOR FOR SELECT id, amount FROM orders WHERE state = 'open' AND placed < p_before ORDER BY id;
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = TRUE;
  SET p_changed = 0, p_total = 0;
  OPEN c_open;
  fees: LOOP
    FETCH c_open INTO v_id, v_amount;
    IF done THEN
      LEAVE fees;
    END IF;
    UPDATE orders SET amount = amount + p_fee, state = 'late' WHERE id = v_id;
    SET p_changed = p_changed + 1;
    SET p_total = p_total + v_amount + p_fee;
  END LOOP;
  CLOSE c_open;
END`,
		steps: []step{{proc: "late_fees", in: []string{`DATE '2024-02-01'`, `2.50`}, outs: 2},
			{proc: "late_fees", in: []string{`DATE '2000-01-01'`, `1`}, outs: 2}}},

	{kind: "procedure", name: "find_price", want: Converted, ddl: `
CREATE PROCEDURE find_price(IN p_sku VARCHAR(20), OUT p_price DECIMAL(10,2), OUT p_status VARCHAR(30))
BEGIN
  SET p_price = -1;
  SET p_status = 'unknown';
  BEGIN
    DECLARE EXIT HANDLER FOR 1172 SET p_price = -2, p_status = 'ambiguous';
    -- A query that finds nothing leaves the variables as they are.
    SELECT price, 'found' INTO p_price, p_status FROM stock WHERE sku LIKE p_sku;
  END;
  IF p_price > 50 THEN
    SET p_status = CONCAT(p_status, ', dear');
  END IF;
END`,
		steps: []step{{proc: "find_price", in: []string{`'GEAR-1'`}, outs: 2}, {proc: "find_price", in: []string{`'BOLT-8'`}, outs: 2},
			{proc: "find_price", in: []string{`'NOPE'`}, outs: 2}, {proc: "find_price", in: []string{`'%-8'`}, outs: 2}}},

	{kind: "procedure", name: "place_order", want: Converted, ddl: `
CREATE PROCEDURE place_order(IN p_order INT, IN p_customer INT, IN p_sku VARCHAR(20), IN p_qty INT)
BEGIN
  DECLARE v_have INT;
  DECLARE v_price DECIMAL(10,2);
  DECLARE v_credit DECIMAL(12,2);
  DECLARE v_cost DECIMAL(12,2);
  DECLARE v_msg VARCHAR(100);
  IF p_qty IS NULL OR p_qty <= 0 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quantity must be positive';
  END IF;
  SELECT qty, price INTO v_have, v_price FROM stock WHERE sku = p_sku;
  SELECT credit INTO v_credit FROM customers WHERE id = p_customer;
  IF v_have IS NULL OR v_credit IS NULL THEN
    SIGNAL SQLSTATE '45001' SET MESSAGE_TEXT = 'no such item or customer';
  END IF;
  SET v_cost = v_price * p_qty;
  IF v_have < p_qty THEN
    SET v_msg = CONCAT('only ', v_have, ' of ', p_sku, ' in stock');
    SIGNAL SQLSTATE '45002' SET MESSAGE_TEXT = v_msg;
  ELSEIF v_cost > v_credit THEN
    SIGNAL SQLSTATE '45003' SET MESSAGE_TEXT = 'not enough credit';
  END IF;
  UPDATE stock SET qty = qty - p_qty WHERE sku = p_sku;
  UPDATE customers SET credit = credit - v_cost WHERE id = p_customer;
  INSERT INTO orders (id, customer_id, sku, qty, amount, placed, state)
    VALUES (p_order, p_customer, p_sku, p_qty, v_cost, DATE '2024-04-01', 'open');
END`,
		steps: []step{{proc: "place_order", in: []string{`20`, `1`, `'BOLT-8'`, `10`}}, {proc: "place_order", in: []string{`21`, `1`, `'GEAR-1'`, `5`}},
			{proc: "place_order", in: []string{`22`, `2`, `'GEAR-1'`, `2`}}, {proc: "place_order", in: []string{`23`, `2`, `'NOPE'`, `1`}},
			{proc: "place_order", in: []string{`24`, `1`, `'NUT-8'`, `0`}}, {proc: "place_order", in: []string{`25`, `1`, `'NUT-8'`, `1000`}}}},

	{kind: "procedure", name: "restock", want: Converted, ddl: `
CREATE PROCEDURE restock(IN p_sku VARCHAR(20), IN p_qty INT, IN p_price DECIMAL(10,2))
body: BEGIN
  DECLARE v_round INT DEFAULT 0;
  DECLARE v_left INT;
  IF p_price <= 0 THEN
    INSERT INTO audit (seq, tbl, action) VALUES (0, 'stock', 'badprice');
    LEAVE body;
  END IF;
  -- Add the item, or top it up if it is there already.
  BEGIN
    DECLARE EXIT HANDLER FOR 1062
      UPDATE stock SET qty = qty + p_qty, price = p_price WHERE sku = p_sku;
    INSERT INTO stock (sku, qty, price) VALUES (p_sku, p_qty, p_price);
  END;
  WHILE v_round < 2 DO
    SET v_round = v_round + 1;
    UPDATE stock SET note = CONCAT('restocked ', v_round, 'x') WHERE sku = p_sku AND qty > 500;
  END WHILE;
  SET v_left = p_qty;
  REPEAT
    SET v_left = v_left DIV 10;
    INSERT INTO audit (seq, tbl, action, key_id) VALUES (v_round, 'stock', 'tens', v_left % 7);
  UNTIL v_left < 10 END REPEAT;
END`,
		steps: []step{{proc: "restock", in: []string{`'WASHER-3'`, `40`, `0.03`}}, {proc: "restock", in: []string{`'BOLT-8'`, `300`, `5.25`}},
			{proc: "restock", in: []string{`'GEAR-1'`, `1`, `0`}}}},

	{kind: "trigger", name: "customers_bi", table: "customers", want: Converted, ddl: `
CREATE TRIGGER customers_bi BEFORE INSERT ON customers FOR EACH ROW
BEGIN
  SET NEW.email = LOWER(TRIM(NEW.email));
  SET NEW.tier = IFNULL(NEW.tier, 'new');
  IF NEW.credit IS NULL THEN
    SET NEW.credit = 25, NEW.active = 1;
  END IF;
  IF NEW.created IS NULL THEN
    SET NEW.created = TIMESTAMP '2024-01-01 00:00:00';
  END IF;
END`,
		steps: []step{{exec: `INSERT INTO customers (id, name, email) VALUES (7, 'Yaw Darko', '  Yaw@Example.COM ')`},
			{exec: `INSERT INTO customers (id, name, email, tier, credit) VALUES (8, 'Adwoa Sarpong', NULL, 'gold', 10)`}}},

	{kind: "trigger", name: "customers_bu", table: "customers", want: Converted, ddl: `
CREATE TRIGGER customers_bu BEFORE UPDATE ON customers FOR EACH ROW
  IF NEW.credit < 0 THEN
    SET NEW.credit = OLD.credit, NEW.active = 0;
  END IF`,
		steps: []step{{exec: `UPDATE customers SET credit = credit - 200 WHERE id IN (1, 2)`},
			{exec: `UPDATE customers SET email = 'ESI@EXAMPLE.COM' WHERE id = 3`}}},

	{kind: "trigger", name: "orders_au", table: "orders", want: Converted, ddl: `
CREATE TRIGGER orders_au AFTER UPDATE ON orders FOR EACH ROW
BEGIN
  IF NOT (NEW.state <=> OLD.state) OR NEW.qty <> OLD.qty THEN
    INSERT INTO audit (seq, tbl, action, key_id, old_val, new_val)
      VALUES (NEW.qty - OLD.qty, 'orders', 'update', NEW.id, OLD.state, NEW.state);
  END IF;
END`,
		steps: []step{{exec: `UPDATE orders SET state = 'paid', qty = qty + 1 WHERE customer_id = 2`}, {exec: `UPDATE orders SET amount = amount + 1`},
			{exec: `UPDATE orders SET state = NULL WHERE id = 10`}}},

	{kind: "trigger", name: "orders_ad", table: "orders", want: Converted, ddl: `
CREATE TRIGGER orders_ad AFTER DELETE ON orders FOR EACH ROW
  INSERT INTO audit (seq, tbl, action, key_id, old_val) VALUES (OLD.qty, 'orders', 'delete', OLD.id, CONCAT(OLD.sku, ' x', OLD.qty, ' @', OLD.amount))`,
		steps: []step{{exec: `DELETE FROM orders WHERE id IN (10, 13)`}}},

	{kind: "trigger", name: "customers_bd", table: "customers", want: Converted, ddl: `
CREATE TRIGGER customers_bd BEFORE DELETE ON customers FOR EACH ROW
BEGIN
  DECLARE v_orders INT;
  SELECT COUNT(*) INTO v_orders FROM orders WHERE customer_id = OLD.id;
  IF v_orders > 0 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'customer still has orders';
  END IF;
END`,
		steps: []step{{exec: `DELETE FROM customers WHERE id = 3`}, {exec: `DELETE FROM customers WHERE id = 1`}, {exec: `DELETE FROM customers`}}},

	// Rewritten with a caveat.

	{kind: "function", name: "avg_order", want: Review, ddl: `
CREATE FUNCTION avg_order(p_customer INT) RETURNS DECIMAL(20,10) READS SQL DATA
BEGIN
  DECLARE v_sum DECIMAL(12,2);
  DECLARE v_n INT;
  SELECT SUM(amount), COUNT(*) INTO v_sum, v_n FROM orders WHERE customer_id = p_customer;
  RETURN v_sum / v_n * 3 / 7;
END`,
		steps: []step{{fn: `avg_order(1)`}, {fn: `avg_order(2)`}, {fn: `avg_order(3)`}}},

	{kind: "function", name: "initials", want: Review, ddl: `
CREATE FUNCTION initials(p_name VARCHAR(60)) RETURNS VARCHAR(10) DETERMINISTIC
BEGIN
  DECLARE v_space INT;
  SET v_space = LOCATE(' ', p_name);
  RETURN CONCAT(LEFT(p_name, 1), SUBSTRING(p_name, v_space + 1, 1));
END`,
		steps: []step{{fn: `initials('Ama Mensah')`}, {fn: `initials('Esi')`}, {fn: `initials(NULL)`}}},

	{kind: "procedure", name: "transfer", want: Review, ddl: `
CREATE PROCEDURE transfer(IN p_from INT, IN p_to INT, IN p_amount DECIMAL(12,2))
BEGIN
  DECLARE v_left DECIMAL(12,2);
  DECLARE EXIT HANDLER FOR SQLSTATE '45000'
    INSERT INTO audit (seq, tbl, action, key_id) VALUES (1, 'customers', 'short', p_from);
  UPDATE customers SET credit = credit - p_amount WHERE id = p_from;
  SELECT credit INTO v_left FROM customers WHERE id = p_from;
  IF v_left < 0 THEN
    SIGNAL SQLSTATE '45000';
  END IF;
  UPDATE customers SET credit = credit + p_amount WHERE id = p_to;
END`,
		steps: []step{{proc: "transfer", in: []string{`1`, `2`, `100`}}, {proc: "transfer", in: []string{`2`, `1`, `9999`}}}},
}

// mysqlLooseCases run in a database with MySQL's default collation,
// which ignores case, and without the option that says text compares
// exactly.
var mysqlLooseCases = []routineCase{
	{kind: "function", name: "is_gold", want: Review, ddl: `
CREATE FUNCTION is_gold(p_tier VARCHAR(10)) RETURNS INT DETERMINISTIC
  RETURN p_tier = 'gold' OR p_tier LIKE 'plat%'`,
		steps: []step{{fn: `is_gold('gold')`}, {fn: `is_gold('GOLD')`}, {fn: `is_gold('Platinum')`}, {fn: `is_gold('silver')`}}},
}

func signatures(from string, cases []routineCase) []Routine {
	var out []Routine
	for _, c := range cases {
		if r, ok := Signature(from, Object{Source: c.ddl}); ok {
			out = append(out, r)
		}
	}
	return out
}

func TestMySQLToPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	testConfig(t, "DEDIE_TEST_TARGET")
	run := func(t *testing.T, collation string, exact bool, cases []routineCase) {
		// utf8mb4_0900_bin compares text byte by byte and counts
		// trailing spaces, as PostgreSQL does.
		src := openMySQL(t, ctx, "DEDIE_TEST_MYSQL_SOURCE", "dedie_cs_src", collation)
		defer src.close()
		dst := openPostgres(t, ctx, "DEDIE_TEST_TARGET", "dedie_cs")
		defer dst.close()
		for _, stmt := range mysqlFixture {
			if err := src.exec(stmt); err != nil {
				t.Fatalf("MySQL fixture: %v\n%s", err, stmt)
			}
		}
		for _, stmt := range mysqlPGFixture {
			if err := dst.exec(stmt); err != nil {
				t.Fatalf("PostgreSQL fixture: %v\n%s", err, stmt)
			}
		}
		tables, err := src.tables(shopTables)
		if err != nil {
			t.Fatal(err)
		}
		runCorpus(t, corpus{
			from: "mysql", to: "postgres", src: src, dst: dst,
			tables: shopTables, targets: shopTables, seed: shopSeed,
			opt:   Options{Tables: tables, Routines: signatures("mysql", cases), TargetSchema: "dedie_cs", ExactText: exact},
			cases: cases,
			create: func(t *testing.T, c routineCase) {
				if err := src.exec(c.ddl); err != nil {
					t.Fatalf("creating %s on MySQL: %v", c.name, err)
				}
			},
		})
	}
	t.Run("exact", func(t *testing.T) { run(t, "utf8mb4_0900_bin", true, mysqlCases) })
	t.Run("ignoring case", func(t *testing.T) { run(t, "utf8mb4_0900_ai_ci", false, mysqlLooseCases) })
}
