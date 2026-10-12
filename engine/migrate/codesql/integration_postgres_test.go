package codesql

import (
	"context"
	"testing"
)

// The PostgreSQL fixture, and the same tables as the PostgreSQL to MySQL
// pipeline creates them: boolean becomes TINYINT(1), timestamp
// DATETIME(6), and every table compares text byte by byte.

var postgresFixture = []string{
	`CREATE TABLE customers (id integer PRIMARY KEY, name varchar(60), email varchar(120), tier varchar(10),
		credit numeric(12,2), created timestamp, active boolean)`,
	`CREATE TABLE orders (id integer PRIMARY KEY, customer_id integer, sku varchar(20), qty integer, amount numeric(12,2),
		placed date, state varchar(12))`,
	`CREATE TABLE stock (sku varchar(20) PRIMARY KEY, qty integer, price numeric(10,2), note varchar(100))`,
	`CREATE TABLE audit (seq integer, tbl varchar(30), action varchar(10), key_id integer, old_val varchar(200), new_val varchar(200))`,
}

const mysqlTableOptions = ` ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`

var postgresMySQLFixture = []string{
	`CREATE TABLE customers (id INT PRIMARY KEY, name VARCHAR(60), email VARCHAR(120), tier VARCHAR(10),
		credit DECIMAL(12,2), created DATETIME(6), active TINYINT(1))` + mysqlTableOptions,
	`CREATE TABLE orders (id INT PRIMARY KEY, customer_id INT, sku VARCHAR(20), qty INT, amount DECIMAL(12,2),
		placed DATE, state VARCHAR(12))` + mysqlTableOptions,
	`CREATE TABLE stock (sku VARCHAR(20) PRIMARY KEY, qty INT, price DECIMAL(10,2), note VARCHAR(100))` + mysqlTableOptions,
	`CREATE TABLE audit (seq INT, tbl VARCHAR(30), action VARCHAR(10), key_id INT, old_val VARCHAR(200), new_val VARCHAR(200))` + mysqlTableOptions,
}

var postgresSeed = []string{
	`INSERT INTO customers VALUES (1, 'Ama Mensah', 'ama@example.com', 'gold', 500.00, TIMESTAMP '2023-01-15 09:30:00', TRUE)`,
	`INSERT INTO customers VALUES (2, 'Kofi Boateng', 'kofi@example.com', 'silver', 120.50, TIMESTAMP '2023-06-01 14:00:00', TRUE)`,
	`INSERT INTO customers VALUES (3, 'Esi Owusu', NULL, NULL, 0, TIMESTAMP '2024-02-29 00:00:00', FALSE)`,
	`INSERT INTO orders VALUES (10, 1, 'BOLT-8', 4, 19.96, DATE '2024-01-10', 'open')`,
	`INSERT INTO orders VALUES (11, 1, 'NUT-8', 100, 12.00, DATE '2024-03-05', 'shipped')`,
	`INSERT INTO orders VALUES (12, 2, 'BOLT-8', 1, 4.99, DATE '2024-03-20', 'open')`,
	`INSERT INTO orders VALUES (13, 2, 'GEAR-1', 2, 150.00, DATE '2023-12-31', 'open')`,
	`INSERT INTO stock VALUES ('BOLT-8', 250, 4.99, NULL)`,
	`INSERT INTO stock VALUES ('NUT-8', 1000, 0.12, 'bulk only')`,
	`INSERT INTO stock VALUES ('GEAR-1', 3, 75.00, NULL)`,
}

var postgresCases = []routineCase{
	{kind: "function", name: "valid_email", want: Converted, ddl: `
CREATE FUNCTION valid_email(p_email text) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
  v_at integer;
BEGIN
  -- An address needs one @ with something on both sides and a dot after it.
  IF p_email IS NULL OR length(trim(p_email)) < 3 THEN
    RETURN false;
  END IF;
  v_at := strpos(p_email, '@');
  IF v_at <= 1 OR position(' ' IN p_email) > 0 THEN
    RETURN false;
  END IF;
  RETURN p_email LIKE '%@%.%' AND p_email NOT LIKE '%@%@%' AND p_email = lower(p_email);
END $$`,
		steps: []step{{fn: `valid_email('ama@example.com')`}, {fn: `valid_email('ama.example.com')`}, {fn: `valid_email('@example.com')`},
			{fn: `valid_email('a b@example.com')`}, {fn: `valid_email(NULL)`}, {fn: `valid_email('  ')`}, {fn: `valid_email('a@b@c.d')`},
			{fn: `valid_email('Ama@Example.com')`}}},

	{kind: "function", name: "price", want: Converted, ddl: `
CREATE FUNCTION price(p_amount numeric, p_tier text, p_tax_pct numeric) RETURNS numeric LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
  v_discount numeric(5,2) := 0;
  v_net numeric(12,2);
BEGIN
  IF p_amount IS NULL OR p_amount < 0 THEN
    RAISE EXCEPTION 'amount must not be negative, tier %', p_tier;
  END IF;
  v_discount := CASE p_tier WHEN 'gold' THEN 15 WHEN 'silver' THEN 7.5 ELSE 0 END;
  v_net := round(p_amount * (100 - v_discount) / 100, 2);
  RETURN round(v_net + v_net * coalesce(p_tax_pct, 12.5) / 100, 2);
END $$`,
		steps: []step{{fn: `price(100, 'none', NULL)`}, {fn: `price(19.99, 'gold', 12.5)`}, {fn: `price(19.99, 'silver', 0)`},
			{fn: `price(0.05, 'gold', 33.333)`}, {fn: `price(-1, 'gold', 1)`}, {fn: `price(NULL, 'gold', 1)`}, {fn: `price(10, NULL, 7)`}, {fn: `price(10, 'GOLD', 7)`}}},

	{kind: "function", name: "due_date", want: Converted, ddl: `
CREATE FUNCTION due_date(p_placed date, p_days integer) RETURNS date LANGUAGE plpgsql IMMUTABLE STRICT AS $$
DECLARE
  v_due date;
BEGIN
  -- Payment is due a number of days after the order and never on the
  -- first of a month.
  v_due := p_placed + p_days;
  IF extract(day FROM v_due) = 1 THEN
    v_due := v_due + 1;
  END IF;
  IF v_due - p_placed < 0 OR extract(year FROM v_due) < 2000 THEN
    RETURN NULL;
  END IF;
  RETURN v_due;
END $$`,
		steps: []step{{fn: `due_date(DATE '2024-01-31', 30)`}, {fn: `due_date(DATE '2024-01-31', 1)`}, {fn: `due_date(DATE '2024-02-28', 2)`},
			{fn: `due_date(NULL, 3)`}, {fn: `due_date(DATE '2023-12-25', -40)`}, {fn: `due_date(DATE '2024-01-15', NULL)`}}},

	{kind: "function", name: "label", want: Converted, ddl: `
CREATE FUNCTION label(p_id integer) RETURNS text LANGUAGE plpgsql STABLE AS $$
DECLARE
  v_name varchar(60);
  v_tier varchar(10);
  v_created timestamp;
  v_active boolean;
BEGIN
  SELECT name, tier, created, active INTO STRICT v_name, v_tier, v_created, v_active FROM customers WHERE id = p_id;
  RETURN 'C' || lpad(p_id::text, 6, '0') || ' ' || upper(substr(v_name, 1, 1)) || '. ' || substr(v_name, 2, 3) || ' ['
    || coalesce(v_tier, 'none') || '] since ' || to_char(v_created, 'YYYY-MM-DD HH24:MI')
    || CASE WHEN v_active THEN '' ELSE ' (inactive)' END;
EXCEPTION
  WHEN no_data_found THEN
    RETURN 'no customer ' || p_id;
END $$`,
		steps: []step{{fn: `label(1)`}, {fn: `label(3)`}, {fn: `label(99)`}}},

	{kind: "function", name: "text_ops", want: Converted, ddl: `
CREATE FUNCTION text_ops(p_s text, p_amount numeric, p_i integer) RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
  p_n numeric(10,2) := p_amount;
BEGIN
  RETURN concat_ws('|', substr(p_s, 2), substring(p_s, 1, 3), substring(p_s FROM 2 FOR 2), upper(p_s), lower(p_s), ltrim(p_s), rtrim(p_s), trim(p_s), btrim(p_s, 'a'),
    trim(leading 'c' from p_s), lpad(p_s, 6, '*'), rpad(p_s, 6, '-'), lpad(p_s, 2), replace(p_s, 'a', 'XY'), length(p_s), octet_length(p_s), strpos(p_s, 'c'),
    position('ab' IN p_s), coalesce(p_s, 'null'), CASE WHEN p_s IS NULL THEN 'n' ELSE 'y' END, p_n, p_i, round(p_n, 1), round(p_n), trunc(p_n, 1), trunc(p_n), mod(p_i, 3),
    p_i / 2, p_i % 4, abs(p_n), sign(p_n), ceil(p_n), floor(p_n), left(p_s, 2), right(p_s, 2), reverse(p_s), repeat(p_s, 2), p_s || p_i, nullif(p_i, 3),
    round(p_i::numeric / 4, 3), p_n * 3, p_n + p_i, coalesce(NULL, p_i, 5), p_n::integer, p_i::numeric(8,3),
    CASE WHEN p_i BETWEEN 1 AND 5 THEN 'low' WHEN p_i IN (7, 9) THEN 'odd' ELSE 'other' END,
    CASE WHEN p_s IS NOT DISTINCT FROM NULL THEN 'same' ELSE 'not' END, CASE WHEN p_s = 'A' OR p_s LIKE 'CA%' THEN 'upper' WHEN p_s = 'a' OR p_s LIKE 'ca%' THEN 'lower' END,
    concat(p_s, NULL, p_i), 'it''s \\ ' || coalesce(p_s, '?'));
END $$`,
		steps: []step{{fn: `text_ops('abcabc', 12.5, 7)`}, {fn: `text_ops('  a b  ', 0.25, -4)`}, {fn: `text_ops(NULL, -0.5, 0)`}, {fn: `text_ops('a', 1000, 3)`},
			{fn: `text_ops('caab', -12.345, 10)`}, {fn: `text_ops('', 5.005, NULL)`}}},

	{kind: "function", name: "date_ops", want: Converted, ddl: `
CREATE FUNCTION date_ops(p_d date, p_t timestamp, p_n integer) RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
  RETURN concat_ws('|', p_d + p_n, p_d - 7, p_d - DATE '2024-01-01', to_char(p_t + interval '90 minutes', 'YYYY-MM-DD HH24:MI:SS'),
    to_char(p_t - p_n * interval '1 hour', 'YYYY-MM-DD HH24:MI:SS'), to_char(p_d + interval '1 month', 'YYYY-MM-DD HH24:MI:SS'),
    to_char(p_t + INTERVAL '2 weeks', 'YYYY-MM-DD'), to_char(p_t - '1 year'::interval, 'YYYY-MM-DD'), to_char(date_trunc('day', p_t), 'YYYY-MM-DD HH24:MI:SS'),
    to_char(date_trunc('month', p_t), 'YYYY-MM-DD'), to_char(date_trunc('year', p_t), 'YYYY-MM-DD'), to_char(date_trunc('hour', p_t), 'HH24:MI:SS'),
    extract(year FROM p_d), extract(month FROM p_d), extract(day FROM p_d), extract(hour FROM p_t), extract(minute FROM p_t), extract(quarter FROM p_d),
    extract(dow FROM p_d), extract(isodow FROM p_d), extract(doy FROM p_d), date_part('year', p_t), p_t::date, to_char(p_d::timestamp, 'YYYY-MM-DD HH24:MI'),
    to_char(p_t, 'DD/MM/YY HH12:MI, FMDD FMMM'), CASE WHEN p_d < p_t THEN 'before' WHEN p_d + 1 > p_t THEN 'eve' ELSE 'after' END);
END $$`,
		steps: []step{{fn: `date_ops(DATE '2024-02-29', TIMESTAMP '2024-03-01 13:45:10', 3)`}, {fn: `date_ops(DATE '2024-12-31', TIMESTAMP '2024-12-31 23:59:59', -5)`},
			{fn: `date_ops(DATE '2023-01-01', TIMESTAMP '2022-06-15 00:00:00', 0)`}, {fn: `date_ops(NULL, TIMESTAMP '2024-07-04 06:30:00', 1)`},
			{fn: `date_ops(DATE '2024-01-31', NULL, 1)`}}},

	{kind: "function", name: "order_total", want: Converted, ddl: `
CREATE FUNCTION order_total(p_customer integer, p_state text) RETURNS numeric LANGUAGE sql STABLE AS $$
  SELECT coalesce(sum(amount), 0) FROM orders WHERE customer_id = $1 AND (p_state IS NULL OR state = p_state)
$$`,
		steps: []step{{fn: `order_total(1, NULL)`}, {fn: `order_total(2, 'open')`}, {fn: `order_total(3, NULL)`}, {fn: `order_total(2, 'OPEN')`}}},

	{kind: "procedure", name: "late_fees", want: Converted, ddl: `
CREATE PROCEDURE late_fees(p_before date, p_fee numeric, INOUT p_changed integer, INOUT p_total numeric) LANGUAGE plpgsql AS $$
DECLARE
  c_open CURSOR FOR SELECT id, amount FROM orders WHERE state = 'open' AND placed < p_before ORDER BY id;
  v_id integer;
  v_amount numeric(12,2);
BEGIN
  p_changed := 0;
  p_total := 0;
  OPEN c_open;
  LOOP
    FETCH c_open INTO v_id, v_amount;
    EXIT WHEN NOT FOUND;
    UPDATE orders SET amount = amount + p_fee, state = 'late' WHERE id = v_id;
    p_changed := p_changed + 1;
    p_total := p_total + v_amount + p_fee;
  END LOOP;
  CLOSE c_open;
END $$`,
		steps: []step{{proc: "late_fees", in: []string{`DATE '2024-02-01'`, `2.50`}, outs: 2},
			{proc: "late_fees", in: []string{`DATE '2000-01-01'`, `1`}, outs: 2}}},

	{kind: "procedure", name: "snapshot", want: Converted, ddl: `
CREATE PROCEDURE snapshot(p_min_qty integer) LANGUAGE plpgsql AS $$
DECLARE
  v_rows integer := 0;
  r record;
BEGIN
  FOR r IN SELECT s.sku, s.qty, s.note, s.price * 2 AS double_price FROM stock s WHERE s.qty >= p_min_qty ORDER BY s.note, s.sku LOOP
    v_rows := v_rows + 1;
    INSERT INTO audit (seq, tbl, action, key_id, old_val, new_val)
      VALUES (v_rows, 'stock', 'snap', r.qty, r.sku, coalesce(r.note, '-') || ' ' || r.double_price);
  END LOOP;
  FOR i IN REVERSE 3..1 LOOP
    CONTINUE WHEN i = 2;
    INSERT INTO audit (seq, tbl, action, key_id) VALUES (i, 'stock', 'mark', v_rows);
  END LOOP;
  <<outer_loop>>
  FOR i IN 1..3 LOOP
    FOR j IN 1..3 LOOP
      EXIT outer_loop WHEN i * j > 4;
      INSERT INTO audit (seq, tbl, action, key_id) VALUES (i, 'stock', 'grid', j);
    END LOOP;
  END LOOP;
END $$`,
		steps: []step{{proc: "snapshot", in: []string{`100`}}, {proc: "snapshot", in: []string{`0`}}}},

	{kind: "procedure", name: "find_price", want: Converted, ddl: `
CREATE PROCEDURE find_price(p_sku text, INOUT p_price numeric, INOUT p_status text) LANGUAGE plpgsql AS $$
DECLARE
  v_note varchar(100);
BEGIN
  BEGIN
    SELECT price INTO STRICT p_price FROM stock WHERE sku LIKE p_sku;
    p_status := 'found';
  EXCEPTION
    WHEN no_data_found THEN
      p_price := -1;
      p_status := 'unknown';
    WHEN too_many_rows THEN
      p_price := -2;
      p_status := 'ambiguous';
  END;
  -- Without STRICT a query that finds nothing leaves NULL behind.
  v_note := 'unset';
  SELECT note INTO v_note FROM stock WHERE sku = p_sku;
  IF NOT FOUND THEN
    p_status := p_status || ', no note';
  ELSIF v_note IS NULL THEN
    p_status := p_status || ', empty note';
  END IF;
  IF p_price > 50 THEN
    p_status := p_status || ', dear';
  END IF;
END $$`,
		steps: []step{{proc: "find_price", in: []string{`'GEAR-1'`}, outs: 2}, {proc: "find_price", in: []string{`'NUT-8'`}, outs: 2},
			{proc: "find_price", in: []string{`'NOPE'`}, outs: 2}, {proc: "find_price", in: []string{`'%-8'`}, outs: 2}}},

	{kind: "procedure", name: "place_order", want: Converted, ddl: `
CREATE PROCEDURE place_order(p_order integer, p_customer integer, p_sku text, p_qty integer) LANGUAGE plpgsql AS $$
DECLARE
  v_have integer;
  v_price numeric(10,2);
  v_credit numeric(12,2);
  v_cost numeric(12,2);
BEGIN
  IF p_qty IS NULL OR p_qty <= 0 THEN
    RAISE EXCEPTION 'quantity must be positive';
  END IF;
  SELECT qty, price INTO STRICT v_have, v_price FROM stock WHERE sku = p_sku;
  SELECT credit INTO STRICT v_credit FROM customers WHERE id = p_customer;
  v_cost := v_price * p_qty;
  IF v_have < p_qty THEN
    RAISE EXCEPTION 'only % of % in stock', v_have, p_sku USING ERRCODE = '45002';
  ELSIF v_cost > v_credit THEN
    RAISE EXCEPTION USING ERRCODE = '45003', MESSAGE = 'not enough credit';
  END IF;
  UPDATE stock SET qty = qty - p_qty WHERE sku = p_sku;
  UPDATE customers SET credit = credit - v_cost WHERE id = p_customer;
  INSERT INTO orders (id, customer_id, sku, qty, amount, placed, state)
    VALUES (p_order, p_customer, p_sku, p_qty, v_cost, DATE '2024-04-01', 'open');
END $$`,
		steps: []step{{proc: "place_order", in: []string{`20`, `1`, `'BOLT-8'`, `10`}}, {proc: "place_order", in: []string{`21`, `1`, `'GEAR-1'`, `5`}},
			{proc: "place_order", in: []string{`22`, `2`, `'GEAR-1'`, `2`}}, {proc: "place_order", in: []string{`23`, `2`, `'NOPE'`, `1`}},
			{proc: "place_order", in: []string{`24`, `1`, `'NUT-8'`, `0`}}, {proc: "place_order", in: []string{`25`, `1`, `'NUT-8'`, `1000`}}}},

	{kind: "procedure", name: "restock", want: Converted, ddl: `
CREATE PROCEDURE restock(p_sku text, p_qty integer, p_price numeric) LANGUAGE plpgsql AS $$
DECLARE
  v_round integer := 0;
  v_gone integer;
BEGIN
  IF p_price <= 0 THEN
    INSERT INTO audit (seq, tbl, action) VALUES (0, 'stock', 'badprice');
    RETURN;
  END IF;
  -- Add the item, or top it up if it is there already.
  BEGIN
    INSERT INTO stock (sku, qty, price) VALUES (p_sku, p_qty, p_price);
  EXCEPTION
    WHEN unique_violation THEN
      UPDATE stock SET qty = qty + p_qty, price = p_price WHERE sku = p_sku;
  END;
  WHILE v_round < 2 LOOP
    v_round := v_round + 1;
    UPDATE stock SET note = 'restocked ' || v_round || 'x' WHERE sku = p_sku AND qty > 500;
  END LOOP;
  DELETE FROM orders WHERE sku = p_sku AND state = 'shipped';
  GET DIAGNOSTICS v_gone = ROW_COUNT;
  INSERT INTO audit (seq, tbl, action, key_id) VALUES (v_round, 'orders', 'purged', v_gone + p_qty / 7 + p_qty % 7);
END $$`,
		steps: []step{{proc: "restock", in: []string{`'WASHER-3'`, `40`, `0.03`}}, {proc: "restock", in: []string{`'NUT-8'`, `300`, `5.25`}},
			{proc: "restock", in: []string{`'GEAR-1'`, `1`, `0`}}}},

	{kind: "trigger", name: "customers_biu", table: "customers", want: Converted, ddl: `
CREATE FUNCTION customers_biu_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.email := lower(trim(NEW.email));
  IF TG_OP = 'INSERT' THEN
    NEW.tier := coalesce(NEW.tier, 'new');
    IF NEW.credit IS NULL THEN
      NEW.credit := 25;
      NEW.active := true;
    END IF;
  ELSIF NEW.credit < 0 THEN
    NEW.credit := OLD.credit;
    NEW.active := false;
  END IF;
  IF NEW.created IS NULL THEN
    NEW.created := TIMESTAMP '2024-01-01 00:00:00';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER customers_biu BEFORE INSERT OR UPDATE ON customers FOR EACH ROW EXECUTE FUNCTION customers_biu_fn()`,
		steps: []step{{exec: `INSERT INTO customers (id, name, email) VALUES (7, 'Yaw Darko', '  Yaw@Example.COM ')`},
			{exec: `INSERT INTO customers (id, name, email, tier, credit) VALUES (8, 'Adwoa Sarpong', NULL, 'gold', 10)`},
			{exec: `UPDATE customers SET credit = credit - 200 WHERE id IN (1, 2)`},
			{exec: `UPDATE customers SET email = 'ESI@EXAMPLE.COM', created = NULL WHERE id = 3`}}},

	{kind: "trigger", name: "orders_aud", table: "orders", want: Converted, ddl: `
CREATE FUNCTION orders_aud_fn() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  v_action varchar(10);
BEGIN
  IF TG_OP = 'DELETE' THEN
    INSERT INTO audit (seq, tbl, action, key_id, old_val) VALUES (OLD.qty, TG_TABLE_NAME, 'delete', OLD.id, OLD.state);
    RETURN OLD;
  END IF;
  v_action := lower(TG_OP);
  IF TG_OP = 'UPDATE' AND NEW.state IS NOT DISTINCT FROM OLD.state THEN
    RETURN NEW;
  END IF;
  INSERT INTO audit (seq, tbl, action, key_id, old_val, new_val)
    VALUES (NEW.qty, TG_TABLE_NAME, v_action, NEW.id, CASE WHEN TG_OP = 'UPDATE' THEN OLD.state END, NEW.state);
  RETURN NEW;
END $$;
CREATE TRIGGER orders_aud AFTER INSERT OR UPDATE OR DELETE ON orders FOR EACH ROW EXECUTE FUNCTION orders_aud_fn()`,
		steps: []step{{exec: `INSERT INTO orders VALUES (30, 3, 'NUT-8', 5, 0.60, DATE '2024-05-01', 'open')`},
			{exec: `UPDATE orders SET state = 'paid', qty = qty + 1 WHERE customer_id = 2`}, {exec: `UPDATE orders SET amount = amount + 1`},
			{exec: `DELETE FROM orders WHERE id IN (10, 30)`}}},

	{kind: "trigger", name: "customers_bd", table: "customers", want: Converted, ddl: `
CREATE FUNCTION customers_bd_fn() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  v_orders integer;
BEGIN
  SELECT count(*) INTO v_orders FROM orders WHERE customer_id = OLD.id;
  IF v_orders > 0 THEN
    RAISE EXCEPTION 'customer % still has % orders', OLD.id, v_orders;
  END IF;
  RETURN OLD;
END $$;
CREATE TRIGGER customers_bd BEFORE DELETE ON customers FOR EACH ROW EXECUTE FUNCTION customers_bd_fn()`,
		steps: []step{{exec: `DELETE FROM customers WHERE id = 3`}, {exec: `DELETE FROM customers WHERE id = 1`}, {exec: `DELETE FROM customers`}}},

	{kind: "trigger", name: "stock_bu", table: "stock", want: Converted, ddl: `
CREATE FUNCTION stock_bu_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.qty < 0 THEN
    NEW.qty := 0;
    NEW.note := 'oversold';
  ELSIF NEW.qty = 0 THEN
    NEW.note := 'sold out';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER stock_bu BEFORE UPDATE ON stock FOR EACH ROW WHEN (NEW.qty < OLD.qty) EXECUTE FUNCTION stock_bu_fn()`,
		steps: []step{{exec: `UPDATE stock SET qty = qty - 3 WHERE sku IN ('GEAR-1', 'BOLT-8')`}, {exec: `UPDATE stock SET qty = qty - 5000`},
			{exec: `UPDATE stock SET qty = 10, price = price + 1`}}},

	// Rewritten with a caveat.

	{kind: "function", name: "avg_order", want: Review, ddl: `
CREATE FUNCTION avg_order(p_customer integer) RETURNS numeric LANGUAGE plpgsql STABLE AS $$
DECLARE
  v_sum numeric(12,2);
  v_n integer;
BEGIN
  SELECT sum(amount), count(*) INTO v_sum, v_n FROM orders WHERE customer_id = p_customer;
  RETURN v_sum / v_n * 3 / 7;
END $$`,
		steps: []step{{fn: `avg_order(1)`}, {fn: `avg_order(2)`}, {fn: `avg_order(3)`}}},

	{kind: "function", name: "split_evenly", want: Review, ddl: `
CREATE FUNCTION split_evenly(p_total integer, p_parts integer DEFAULT 3) RETURNS integer LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
  RAISE NOTICE 'sharing % among %', p_total, p_parts;
  RETURN p_total / p_parts;
END $$`,
		steps: []step{{fn: `split_evenly(10, 3)`}, {fn: `split_evenly(-7, 2)`}, {fn: `split_evenly(1, 0)`}}},

	{kind: "procedure", name: "transfer", want: Review, ddl: `
CREATE PROCEDURE transfer(p_from integer, p_to integer, p_amount numeric) LANGUAGE plpgsql AS $$
DECLARE
  v_left numeric(12,2);
BEGIN
  UPDATE customers SET credit = credit - p_amount WHERE id = p_from;
  SELECT credit INTO v_left FROM customers WHERE id = p_from;
  IF v_left < 0 THEN
    RAISE EXCEPTION 'short' USING ERRCODE = '45000';
  END IF;
  UPDATE customers SET credit = credit + p_amount WHERE id = p_to;
EXCEPTION
  WHEN SQLSTATE '45000' THEN
    INSERT INTO audit (seq, tbl, action, key_id) VALUES (1, 'customers', 'short', p_from);
END $$`,
		steps: []step{{proc: "transfer", in: []string{`1`, `2`, `100`}}, {proc: "transfer", in: []string{`2`, `1`, `9999`}}}},
}

func TestPostgresToMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	testConfig(t, "DEDIE_TEST_MYSQL_TARGET")
	src := openPostgres(t, ctx, "DEDIE_TEST_SOURCE", "dedie_cs_src")
	defer src.close()
	dst := openMySQL(t, ctx, "DEDIE_TEST_MYSQL_TARGET", "dedie_cs", "utf8mb4_0900_ai_ci")
	defer dst.close()
	for _, stmt := range postgresFixture {
		if err := src.exec(stmt); err != nil {
			t.Fatalf("PostgreSQL fixture: %v\n%s", err, stmt)
		}
	}
	for _, stmt := range postgresMySQLFixture {
		if err := dst.exec(stmt); err != nil {
			t.Fatalf("MySQL fixture: %v\n%s", err, stmt)
		}
	}
	tables, err := src.tables(shopTables)
	if err != nil {
		t.Fatal(err)
	}
	runCorpus(t, corpus{
		from: "postgres", to: "mysql", src: src, dst: dst,
		tables: shopTables, targets: shopTables, seed: postgresSeed,
		opt:   Options{Tables: tables, Routines: signatures("postgres", postgresCases)},
		cases: postgresCases,
		create: func(t *testing.T, c routineCase) {
			if err := src.exec(c.ddl); err != nil {
				t.Fatalf("creating %s on PostgreSQL: %v", c.name, err)
			}
		},
	})
}
