package codesql

import (
	"context"
	"testing"
)

// The Oracle fixture: customers, their orders, stock and an audit trail.
// The same tables stand on the target as the pipelines would have made
// them, with every type translated as orapg and oramy translate columns.

var oracleTables = []string{"DEDIE_CS_CUSTOMERS", "DEDIE_CS_ORDERS", "DEDIE_CS_STOCK", "DEDIE_CS_AUDIT"}

var oracleDrops = []string{
	`DROP TABLE dedie_cs_audit PURGE`,
	`DROP TABLE dedie_cs_orders PURGE`,
	`DROP TABLE dedie_cs_stock PURGE`,
	`DROP TABLE dedie_cs_customers PURGE`,
	`DROP SEQUENCE dedie_cs_seq`,
}

var oracleFixture = []string{
	`CREATE TABLE dedie_cs_customers (id NUMBER(9) PRIMARY KEY, name VARCHAR2(60), email VARCHAR2(120),
		tier VARCHAR2(10), credit NUMBER(12,2), created DATE)`,
	`CREATE TABLE dedie_cs_orders (id NUMBER(9) PRIMARY KEY, customer_id NUMBER(9), sku VARCHAR2(20), qty NUMBER(9),
		amount NUMBER(12,2), placed DATE, state VARCHAR2(12))`,
	`CREATE TABLE dedie_cs_stock (sku VARCHAR2(20) PRIMARY KEY, qty NUMBER(9), price NUMBER(10,2), note VARCHAR2(100))`,
	`CREATE TABLE dedie_cs_audit (seq NUMBER(9), tbl VARCHAR2(30), action VARCHAR2(10), key_id NUMBER(9),
		old_val VARCHAR2(200), new_val VARCHAR2(200))`,
	`CREATE SEQUENCE dedie_cs_seq START WITH 1000 NOCACHE`,
}

var oraclePGFixture = []string{
	`CREATE TABLE dedie_cs_customers (id integer PRIMARY KEY, name varchar(60), email varchar(120),
		tier varchar(10), credit numeric(12,2), created timestamp(0))`,
	`CREATE TABLE dedie_cs_orders (id integer PRIMARY KEY, customer_id integer, sku varchar(20), qty integer,
		amount numeric(12,2), placed timestamp(0), state varchar(12))`,
	`CREATE TABLE dedie_cs_stock (sku varchar(20) PRIMARY KEY, qty integer, price numeric(10,2), note varchar(100))`,
	`CREATE TABLE dedie_cs_audit (seq integer, tbl varchar(30), action varchar(10), key_id integer,
		old_val varchar(200), new_val varchar(200))`,
	`CREATE SEQUENCE dedie_cs_seq START WITH 1000`,
}

// The seed is written so that all three databases read it the same way.
var oracleSeed = []string{
	`INSERT INTO dedie_cs_customers VALUES (1, 'Ama Mensah', 'ama@example.com', 'gold', 500.00, TIMESTAMP '2023-01-15 09:30:00')`,
	`INSERT INTO dedie_cs_customers VALUES (2, 'Kofi Boateng', 'kofi@example.com', 'silver', 120.50, TIMESTAMP '2023-06-01 14:00:00')`,
	`INSERT INTO dedie_cs_customers VALUES (3, 'Esi Owusu', NULL, NULL, 0, TIMESTAMP '2024-02-29 00:00:00')`,
	`INSERT INTO dedie_cs_orders VALUES (10, 1, 'BOLT-8', 4, 19.96, TIMESTAMP '2024-01-10 10:00:00', 'open')`,
	`INSERT INTO dedie_cs_orders VALUES (11, 1, 'NUT-8', 100, 12.00, TIMESTAMP '2024-03-05 16:45:00', 'shipped')`,
	`INSERT INTO dedie_cs_orders VALUES (12, 2, 'BOLT-8', 1, 4.99, TIMESTAMP '2024-03-20 08:15:00', 'open')`,
	`INSERT INTO dedie_cs_orders VALUES (13, 2, 'GEAR-1', 2, 150.00, TIMESTAMP '2023-12-31 23:59:59', 'open')`,
	`INSERT INTO dedie_cs_stock VALUES ('BOLT-8', 250, 4.99, NULL)`,
	`INSERT INTO dedie_cs_stock VALUES ('NUT-8', 1000, 0.12, 'bulk only')`,
	`INSERT INTO dedie_cs_stock VALUES ('GEAR-1', 3, 75.00, NULL)`,
}

// oracleCases are PL/SQL routines of the kinds applications are made
// of. Each is written to say what it is expected to become.
var oracleCases = []routineCase{
	{kind: "function", name: "DEDIE_CS_VALID_EMAIL", want: Converted, ddl: `
CREATE OR REPLACE FUNCTION dedie_cs_valid_email(p_email IN VARCHAR2) RETURN NUMBER IS
  v_at PLS_INTEGER;
  v_domain VARCHAR2(200);
BEGIN
  -- An address needs one @ with something on both sides and a dot after it.
  IF p_email IS NULL OR LENGTH(TRIM(p_email)) < 3 THEN
    RETURN 0;
  END IF;
  v_at := INSTR(p_email, '@');
  IF v_at <= 1 OR p_email LIKE '% %' THEN
    RETURN 0;
  END IF;
  v_domain := LOWER(REPLACE(p_email, UPPER(RTRIM(LTRIM(p_email))), ''));
  IF p_email NOT LIKE '%@%.%' OR p_email LIKE '%@%@%' THEN
    RETURN 0;
  END IF;
  RETURN 1;
END dedie_cs_valid_email;`,
		steps: []step{{fn: `dedie_cs_valid_email('ama@example.com')`}, {fn: `dedie_cs_valid_email('ama.example.com')`},
			{fn: `dedie_cs_valid_email('@example.com')`}, {fn: `dedie_cs_valid_email('a b@example.com')`},
			{fn: `dedie_cs_valid_email(NULL)`}, {fn: `dedie_cs_valid_email('  ')`}, {fn: `dedie_cs_valid_email('a@b@c.d')`}}},

	{kind: "function", name: "DEDIE_CS_PRICE", want: Converted, ddl: `
CREATE OR REPLACE FUNCTION dedie_cs_price(p_amount NUMBER, p_tier VARCHAR2 DEFAULT 'none', p_tax_pct NUMBER DEFAULT 12.5)
RETURN NUMBER DETERMINISTIC IS
  v_discount NUMBER(5,2) := 0;
  v_net NUMBER(12,2);
BEGIN
  IF p_amount IS NULL OR p_amount < 0 THEN
    RAISE_APPLICATION_ERROR(-20010, 'amount must not be negative');
  END IF;
  v_discount := CASE p_tier WHEN 'gold' THEN 15 WHEN 'silver' THEN 7.5 ELSE 0 END;
  v_net := ROUND(p_amount * (100 - v_discount) / 100, 2);
  RETURN ROUND(v_net + v_net * p_tax_pct / 100, 2);
END;`,
		steps: []step{{fn: `dedie_cs_price(100)`}, {fn: `dedie_cs_price(19.99, 'gold')`}, {fn: `dedie_cs_price(19.99, 'silver', 0)`},
			{fn: `dedie_cs_price(0.05, 'gold', 33.333)`}, {fn: `dedie_cs_price(-1)`}, {fn: `dedie_cs_price(NULL)`}, {fn: `dedie_cs_price(10, NULL)`}}},

	{kind: "function", name: "DEDIE_CS_DUE_DATE", want: Converted, ddl: `
CREATE OR REPLACE FUNCTION dedie_cs_due_date(p_placed DATE, p_days PLS_INTEGER DEFAULT 30) RETURN DATE IS
  v_due DATE;
BEGIN
  -- Payment is due a number of days after the day of the order, and
  -- never on the first of a month.
  v_due := TRUNC(p_placed) + p_days;
  IF TO_CHAR(v_due, 'DD') = '01' THEN
    v_due := v_due + 1;
  END IF;
  IF v_due < TRUNC(p_placed, 'MM') THEN
    RETURN NULL;
  END IF;
  RETURN v_due;
END;`,
		steps: []step{{fn: `dedie_cs_due_date(TIMESTAMP '2024-01-31 17:20:00')`}, {fn: `dedie_cs_due_date(TIMESTAMP '2024-01-31 17:20:00', 1)`},
			{fn: `dedie_cs_due_date(TIMESTAMP '2024-02-28 00:00:00', 2)`}, {fn: `dedie_cs_due_date(NULL)`},
			{fn: `dedie_cs_due_date(TIMESTAMP '2023-12-25 23:59:59', -40)`}}},

	{kind: "function", name: "DEDIE_CS_LABEL", want: Converted, ddl: `
CREATE OR REPLACE FUNCTION dedie_cs_label(p_id NUMBER) RETURN VARCHAR2 IS
  v_name dedie_cs_customers.name%TYPE;
  v_tier dedie_cs_customers.tier%TYPE;
  v_created dedie_cs_customers.created%TYPE;
BEGIN
  SELECT name, tier, created INTO v_name, v_tier, v_created FROM dedie_cs_customers WHERE id = p_id;
  RETURN 'C' || LPAD(TO_CHAR(TRUNC(p_id)), 6, '0') || ' ' || UPPER(SUBSTR(v_name, 1, 1)) || '. '
    || INITCAP(SUBSTR(v_name, INSTR(v_name, ' ') + 1)) || ' [' || NVL(v_tier, 'none') || '] since '
    || TO_CHAR(v_created, 'YYYY-MM-DD HH24:MI') || NVL2(v_tier, '', ' (new)');
EXCEPTION
  WHEN NO_DATA_FOUND THEN
    RETURN NULL;
END;`,
		steps: []step{{fn: `dedie_cs_label(1)`}, {fn: `dedie_cs_label(3)`}, {fn: `dedie_cs_label(99)`}}},

	{kind: "function", name: "DEDIE_CS_TIER", want: Converted, ddl: `
CREATE OR REPLACE FUNCTION dedie_cs_tier(p_customer NUMBER) RETURN VARCHAR2 IS
  v_total NUMBER;
  v_open NUMBER;
  v_tier VARCHAR2(10);
BEGIN
  SELECT NVL(SUM(amount), 0), COUNT(CASE WHEN state = 'open' THEN 1 END) INTO v_total, v_open
    FROM dedie_cs_orders WHERE customer_id = p_customer;
  SELECT DECODE(SIGN(v_total - 100), 1, 'gold', 0, 'silver', DECODE(v_open, 0, 'none', 'bronze')) INTO v_tier FROM dual;
  RETURN v_tier;
END;`,
		steps: []step{{fn: `dedie_cs_tier(1)`}, {fn: `dedie_cs_tier(2)`}, {fn: `dedie_cs_tier(3)`}, {fn: `dedie_cs_tier(NULL)`}}},

	{kind: "procedure", name: "DEDIE_CS_LATE_FEES", want: Converted, ddl: `
CREATE OR REPLACE PROCEDURE dedie_cs_late_fees(p_before IN DATE, p_fee IN NUMBER, p_changed OUT NUMBER, p_total OUT NUMBER) IS
  CURSOR c_open IS
    SELECT id, amount FROM dedie_cs_orders WHERE state = 'open' AND placed < p_before ORDER BY id;
  v_id dedie_cs_orders.id%TYPE;
  v_amount dedie_cs_orders.amount%TYPE;
BEGIN
  p_changed := 0;
  p_total := 0;
  OPEN c_open;
  LOOP
    FETCH c_open INTO v_id, v_amount;
    EXIT WHEN c_open%NOTFOUND;
    UPDATE dedie_cs_orders SET amount = amount + p_fee, state = 'late' WHERE id = v_id;
    p_changed := p_changed + 1;
    p_total := p_total + v_amount + p_fee;
  END LOOP;
  CLOSE c_open;
END;`,
		steps: []step{{proc: "dedie_cs_late_fees", in: []string{`TIMESTAMP '2024-02-01 00:00:00'`, `2.50`}, outs: 2},
			{proc: "dedie_cs_late_fees", in: []string{`TIMESTAMP '2000-01-01 00:00:00'`, `1`}, outs: 2}}},

	{kind: "procedure", name: "DEDIE_CS_SNAPSHOT", want: Converted, ddl: `
CREATE OR REPLACE PROCEDURE dedie_cs_snapshot(p_min_qty IN NUMBER DEFAULT 0) IS
  v_seq NUMBER;
  v_rows PLS_INTEGER := 0;
BEGIN
  FOR r IN (SELECT s.sku, s.qty, s.note FROM dedie_cs_stock s WHERE s.qty >= p_min_qty ORDER BY s.sku) LOOP
    v_seq := dedie_cs_seq.NEXTVAL;
    INSERT INTO dedie_cs_audit (seq, tbl, action, key_id, old_val, new_val)
      VALUES (v_seq, 'stock', 'snap', r.qty, r.sku, NVL(r.note, '-'));
    v_rows := v_rows + SQL%ROWCOUNT;
  END LOOP;
  FOR i IN REVERSE 1..3 LOOP
    CONTINUE WHEN i = 2;
    INSERT INTO dedie_cs_audit (seq, tbl, action, key_id) VALUES (i, 'stock', 'mark', v_rows);
  END LOOP;
END;`,
		steps: []step{{proc: "dedie_cs_snapshot", in: []string{`100`}}, {proc: "dedie_cs_snapshot"}}},

	{kind: "procedure", name: "DEDIE_CS_FIND_PRICE", want: Converted, ddl: `
CREATE OR REPLACE PROCEDURE dedie_cs_find_price(p_sku IN VARCHAR2, p_price OUT NUMBER, p_status OUT VARCHAR2) IS
BEGIN
  BEGIN
    SELECT price INTO p_price FROM dedie_cs_stock WHERE sku LIKE p_sku;
    p_status := 'found';
  EXCEPTION
    WHEN NO_DATA_FOUND THEN
      p_price := -1;
      p_status := 'unknown';
    WHEN TOO_MANY_ROWS THEN
      p_price := -2;
      p_status := 'ambiguous';
  END;
  IF p_price > 50 THEN
    p_status := p_status || ', dear';
  END IF;
END;`,
		steps: []step{{proc: "dedie_cs_find_price", in: []string{`'GEAR-1'`}, outs: 2}, {proc: "dedie_cs_find_price", in: []string{`'BOLT-8'`}, outs: 2},
			{proc: "dedie_cs_find_price", in: []string{`'NOPE'`}, outs: 2}, {proc: "dedie_cs_find_price", in: []string{`'%-8'`}, outs: 2}}},

	{kind: "procedure", name: "DEDIE_CS_PLACE_ORDER", want: Converted, ddl: `
CREATE OR REPLACE PROCEDURE dedie_cs_place_order(p_order IN NUMBER, p_customer IN NUMBER, p_sku IN VARCHAR2, p_qty IN NUMBER) IS
  v_have dedie_cs_stock.qty%TYPE;
  v_price dedie_cs_stock.price%TYPE;
  v_credit dedie_cs_customers.credit%TYPE;
  v_cost NUMBER(12,2);
BEGIN
  IF p_qty IS NULL OR p_qty <= 0 THEN
    RAISE_APPLICATION_ERROR(-20001, 'quantity must be positive');
  END IF;
  SELECT qty, price INTO v_have, v_price FROM dedie_cs_stock WHERE sku = p_sku;
  SELECT credit INTO v_credit FROM dedie_cs_customers WHERE id = p_customer;
  v_cost := v_price * p_qty;
  IF v_have < p_qty THEN
    RAISE_APPLICATION_ERROR(-20002, 'only ' || v_have || ' of ' || p_sku || ' in stock');
  ELSIF v_cost > v_credit THEN
    RAISE_APPLICATION_ERROR(-20003, 'not enough credit');
  END IF;
  UPDATE dedie_cs_stock SET qty = qty - p_qty WHERE sku = p_sku;
  UPDATE dedie_cs_customers SET credit = credit - v_cost WHERE id = p_customer;
  INSERT INTO dedie_cs_orders (id, customer_id, sku, qty, amount, placed, state)
    VALUES (p_order, p_customer, p_sku, p_qty, v_cost, TIMESTAMP '2024-04-01 12:00:00', 'open');
END;`,
		steps: []step{{proc: "dedie_cs_place_order", in: []string{`20`, `1`, `'BOLT-8'`, `10`}},
			{proc: "dedie_cs_place_order", in: []string{`21`, `1`, `'GEAR-1'`, `5`}},
			{proc: "dedie_cs_place_order", in: []string{`22`, `2`, `'GEAR-1'`, `2`}},
			{proc: "dedie_cs_place_order", in: []string{`23`, `2`, `'NOPE'`, `1`}},
			{proc: "dedie_cs_place_order", in: []string{`24`, `1`, `'NUT-8'`, `0`}},
			{proc: "dedie_cs_place_order", in: []string{`25`, `1`, `'NUT-8'`, `1000`}}}},

	{kind: "procedure", name: "DEDIE_CS_RESTOCK", want: Converted, ddl: `
CREATE OR REPLACE PROCEDURE dedie_cs_restock(p_sku IN VARCHAR2, p_qty IN NUMBER, p_price IN NUMBER) IS
  e_bad_price EXCEPTION;
  v_round PLS_INTEGER := 0;
BEGIN
  IF p_price <= 0 THEN
    RAISE e_bad_price;
  END IF;
  -- Add the item, or top it up if it is there already.
  BEGIN
    INSERT INTO dedie_cs_stock (sku, qty, price) VALUES (p_sku, p_qty, p_price);
  EXCEPTION
    WHEN DUP_VAL_ON_INDEX THEN
      UPDATE dedie_cs_stock SET qty = qty + p_qty, price = p_price WHERE sku = p_sku;
  END;
  WHILE v_round < 2 LOOP
    v_round := v_round + 1;
    UPDATE dedie_cs_stock SET note = 'restocked ' || v_round || 'x' WHERE sku = p_sku AND qty > 500;
    EXIT WHEN SQL%NOTFOUND;
  END LOOP;
EXCEPTION
  WHEN e_bad_price THEN
    INSERT INTO dedie_cs_audit (seq, tbl, action) VALUES (0, 'stock', 'badprice');
END;`,
		steps: []step{{proc: "dedie_cs_restock", in: []string{`'WASHER-3'`, `40`, `0.03`}}, {proc: "dedie_cs_restock", in: []string{`'BOLT-8'`, `300`, `5.25`}},
			{proc: "dedie_cs_restock", in: []string{`'GEAR-1'`, `1`, `0`}}}},

	{kind: "trigger", name: "DEDIE_CS_CUST_BIU", table: "DEDIE_CS_CUSTOMERS", want: Converted, ddl: `
CREATE OR REPLACE TRIGGER dedie_cs_cust_biu BEFORE INSERT OR UPDATE ON dedie_cs_customers FOR EACH ROW
BEGIN
  :NEW.email := LOWER(TRIM(:NEW.email));
  IF INSERTING THEN
    :NEW.tier := NVL(:NEW.tier, 'new');
    IF :NEW.credit IS NULL THEN
      :NEW.credit := 25;
    END IF;
  ELSIF :NEW.credit < 0 THEN
    :NEW.credit := :OLD.credit;
  END IF;
  IF :NEW.created IS NULL THEN
    :NEW.created := TIMESTAMP '2024-01-01 00:00:00';
  END IF;
END;`,
		steps: []step{{exec: `INSERT INTO dedie_cs_customers (id, name, email) VALUES (7, 'Yaw Darko', '  Yaw@Example.COM ')`},
			{exec: `INSERT INTO dedie_cs_customers (id, name, email, tier, credit) VALUES (8, 'Adwoa Sarpong', NULL, 'gold', 10)`},
			{exec: `UPDATE dedie_cs_customers SET credit = credit - 200 WHERE id IN (1, 2)`},
			{exec: `UPDATE dedie_cs_customers SET email = 'ESI@EXAMPLE.COM', created = NULL WHERE id = 3`}}},

	{kind: "trigger", name: "DEDIE_CS_ORDERS_AUD", table: "DEDIE_CS_ORDERS", want: Converted, ddl: `
CREATE OR REPLACE TRIGGER dedie_cs_orders_aud AFTER INSERT OR UPDATE OR DELETE ON dedie_cs_orders FOR EACH ROW
DECLARE
  v_action VARCHAR2(10);
BEGIN
  IF INSERTING THEN
    v_action := 'insert';
  ELSIF UPDATING THEN
    v_action := 'update';
  ELSE
    v_action := 'delete';
  END IF;
  INSERT INTO dedie_cs_audit (seq, tbl, action, key_id, old_val, new_val)
    VALUES (NVL(:NEW.qty, 0) + NVL(:OLD.qty, 0), 'orders', v_action, NVL(:NEW.id, :OLD.id), :OLD.state, :NEW.state);
END;`,
		steps: []step{{exec: `INSERT INTO dedie_cs_orders VALUES (30, 3, 'NUT-8', 5, 0.60, TIMESTAMP '2024-05-01 00:00:00', 'open')`},
			{exec: `UPDATE dedie_cs_orders SET state = 'paid', qty = qty + 1 WHERE customer_id = 2`},
			{exec: `DELETE FROM dedie_cs_orders WHERE id IN (10, 30)`}}},

	{kind: "trigger", name: "DEDIE_CS_CUST_BD", table: "DEDIE_CS_CUSTOMERS", want: Converted, ddl: `
CREATE OR REPLACE TRIGGER dedie_cs_cust_bd BEFORE DELETE ON dedie_cs_customers FOR EACH ROW
DECLARE
  v_orders NUMBER;
BEGIN
  SELECT COUNT(*) INTO v_orders FROM dedie_cs_orders WHERE customer_id = :OLD.id;
  IF v_orders > 0 THEN
    RAISE_APPLICATION_ERROR(-20020, 'customer ' || :OLD.id || ' still has ' || v_orders || ' orders');
  END IF;
END;`,
		steps: []step{{exec: `DELETE FROM dedie_cs_customers WHERE id = 3`}, {exec: `DELETE FROM dedie_cs_customers WHERE id = 1`},
			{exec: `DELETE FROM dedie_cs_customers`}}},

	{kind: "trigger", name: "DEDIE_CS_STOCK_BU", table: "DEDIE_CS_STOCK", want: Converted, ddl: `
CREATE OR REPLACE TRIGGER dedie_cs_stock_bu BEFORE UPDATE OF qty ON dedie_cs_stock FOR EACH ROW WHEN (NEW.qty < OLD.qty)
BEGIN
  IF :NEW.qty < 0 THEN
    :NEW.qty := 0;
    :NEW.note := 'oversold';
  ELSIF :NEW.qty = 0 THEN
    :NEW.note := 'sold out';
  END IF;
END;`,
		steps: []step{{exec: `UPDATE dedie_cs_stock SET qty = qty - 3 WHERE sku IN ('GEAR-1', 'BOLT-8')`},
			{exec: `UPDATE dedie_cs_stock SET qty = qty - 5000`}, {exec: `UPDATE dedie_cs_stock SET qty = 10, price = price + 1`}}},

	// The rest are rewritten with a caveat. They are run all the same,
	// and the log says whether the caveat showed.

	{kind: "function", name: "DEDIE_CS_AVG_ORDER", want: Review, ddl: `
CREATE OR REPLACE FUNCTION dedie_cs_avg_order(p_customer NUMBER) RETURN NUMBER IS
  v_sum NUMBER;
  v_n NUMBER;
BEGIN
  SELECT SUM(amount), COUNT(*) INTO v_sum, v_n FROM dedie_cs_orders WHERE customer_id = p_customer;
  RETURN v_sum / v_n;
END;`,
		steps: []step{{fn: `dedie_cs_avg_order(1)`}, {fn: `dedie_cs_avg_order(2)`}}},

	{kind: "function", name: "DEDIE_CS_RENEWAL", want: Review, ddl: `
CREATE OR REPLACE FUNCTION dedie_cs_renewal(p_id NUMBER) RETURN VARCHAR2 IS
  v_created DATE;
  v_credit NUMBER(12,2);
BEGIN
  SELECT created, credit INTO v_created, v_credit FROM dedie_cs_customers WHERE id = p_id;
  RETURN TO_CHAR(ADD_MONTHS(v_created, 12), 'YYYY-MM-DD') || ' credit ' || v_credit;
END;`,
		steps: []step{{fn: `dedie_cs_renewal(1)`}, {fn: `dedie_cs_renewal(2)`}, {fn: `dedie_cs_renewal(3)`}}},

	{kind: "procedure", name: "DEDIE_CS_TRANSFER", want: Review, ddl: `
CREATE OR REPLACE PROCEDURE dedie_cs_transfer(p_from IN NUMBER, p_to IN NUMBER, p_amount IN NUMBER) IS
  e_short EXCEPTION;
  v_left NUMBER;
  v_msg VARCHAR2(200);
BEGIN
  UPDATE dedie_cs_customers SET credit = credit - p_amount WHERE id = p_from RETURNING credit INTO v_left;
  IF v_left < 0 THEN
    RAISE e_short;
  END IF;
  UPDATE dedie_cs_customers SET credit = credit + p_amount WHERE id = p_to;
EXCEPTION
  WHEN e_short THEN
    INSERT INTO dedie_cs_audit (seq, tbl, action, key_id) VALUES (1, 'customers', 'short', p_from);
  WHEN OTHERS THEN
    v_msg := SUBSTR(SQLERRM, 1, 200);
    INSERT INTO dedie_cs_audit (seq, tbl, action, key_id, new_val) VALUES (2, 'customers', 'error', p_from, v_msg);
END;`,
		steps: []step{{proc: "dedie_cs_transfer", in: []string{`1`, `2`, `100`}}, {proc: "dedie_cs_transfer", in: []string{`2`, `1`, `9999`}}}},
}

// oracleOptions describes the fixture to the converter the way a
// pipeline would: from the source's own dictionary.
func oracleOptions(t *testing.T, src *oraEngine, targetSchema string) Options {
	tables, err := src.tables(oracleTables)
	if err != nil {
		t.Fatal(err)
	}
	var routines []Routine
	for _, c := range oracleCases {
		if r, ok := Signature("oracle", Object{Source: c.ddl}); ok {
			routines = append(routines, r)
		}
	}
	return Options{Tables: tables, Routines: routines, TargetSchema: targetSchema}
}

func setupOracle(t *testing.T, src *oraEngine) {
	for _, stmt := range oracleDrops {
		src.exec(stmt)
	}
	for _, c := range oracleCases {
		src.drop(c.kind, c.name, c.table)
	}
	t.Cleanup(func() {
		for _, c := range oracleCases {
			src.drop(c.kind, c.name, c.table)
		}
		for _, stmt := range oracleDrops {
			src.exec(stmt)
		}
		src.close()
	})
	for _, stmt := range oracleFixture {
		if err := src.exec(stmt); err != nil {
			t.Fatalf("Oracle fixture: %v\n%s", err, stmt)
		}
	}
}

func TestOracleToPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	testConfig(t, "DEDIE_TEST_TARGET")
	src := openOracle(t, ctx)
	setupOracle(t, src)
	dst := openPostgres(t, ctx, "DEDIE_TEST_TARGET", "dedie_cs")
	defer dst.close()
	for _, stmt := range oraclePGFixture {
		if err := dst.exec(stmt); err != nil {
			t.Fatalf("PostgreSQL fixture: %v\n%s", err, stmt)
		}
	}
	runCorpus(t, corpus{
		from: "oracle", to: "postgres", src: src, dst: dst,
		tables: oracleTables, targets: oracleTables, seed: oracleSeed,
		opt:    oracleOptions(t, src, "dedie_cs"),
		cases:  oracleCases,
		create: func(t *testing.T, c routineCase) { src.create(t, c.ddl, c.name) },
	})
}
