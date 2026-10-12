package codesql

import "testing"

// shop is a small catalog for the tests that need to know the type of
// a column.
var shop = Options{
	Tables: []Table{
		{Schema: "SHOP", Name: "EMP", Columns: []Column{{Name: "ID", Type: "NUMBER(9)"}, {Name: "NAME", Type: "VARCHAR2(40)"},
			{Name: "SALARY", Type: "NUMBER(12,2)"}, {Name: "HIRED", Type: "DATE"}, {Name: "DEPT_ID", Type: "NUMBER(4)"}, {Name: "Note", Type: "VARCHAR2(100)"}}},
		{Schema: "SHOP", Name: "DEPT", Columns: []Column{{Name: "ID", Type: "NUMBER(4)"}, {Name: "NAME", Type: "VARCHAR2(40)"}, {Name: "BUDGET", Type: "NUMBER"}}},
		{Schema: "SHOP", Name: "LOG", Columns: []Column{{Name: "AT", Type: "TIMESTAMP(6)"}, {Name: "MSG", Type: "VARCHAR2(200)"}, {Name: "N", Type: "NUMBER(9)"}}},
	},
	Routines: []Routine{{Name: "BONUS", Returns: "NUMBER"}, {Name: "LOG_IT"}},
}

func TestOracleToPostgresWhole(t *testing.T) {
	golden(t, "oracle", "postgres", Object{Schema: "SHOP", Source: `
CREATE OR REPLACE FUNCTION shop.net_pay(p_emp IN NUMBER, p_rate NUMBER DEFAULT 0.2) RETURN NUMBER IS
  v_salary emp.salary%TYPE;
  v_name   VARCHAR2(40);
  c_floor  CONSTANT NUMBER(6,2) := 100;
BEGIN
  SELECT salary, name INTO v_salary, v_name FROM emp WHERE id = p_emp;
  IF v_salary IS NULL THEN
    RETURN c_floor;
  ELSIF v_salary < c_floor THEN
    RETURN v_salary;
  END IF;
  RETURN ROUND(v_salary - v_salary * p_rate, 2) + bonus(p_emp);
EXCEPTION
  WHEN NO_DATA_FOUND THEN
    RETURN NULL;
END net_pay;
/`}, shop, Converted, `
CREATE FUNCTION shop.net_pay(p_emp numeric, p_rate numeric DEFAULT 0.2) RETURNS numeric
LANGUAGE plpgsql
AS $body$
#variable_conflict use_column
DECLARE
  v_salary shop.emp.salary%TYPE;
  v_name varchar(40);
  c_floor CONSTANT numeric(6,2) := 100;
BEGIN
  SELECT salary, name INTO STRICT v_salary, v_name FROM shop.emp WHERE id = p_emp;
  IF v_salary IS NULL THEN
    RETURN c_floor;
  ELSIF v_salary < c_floor THEN
    RETURN v_salary;
  END IF;
  RETURN round(v_salary - v_salary * p_rate, 2) + shop.bonus(p_emp);
EXCEPTION
  WHEN no_data_found THEN
    RETURN NULL;
END;
$body$`)

	golden(t, "oracle", "postgres", Object{Schema: "SHOP", Source: `
TRIGGER emp_stamp BEFORE INSERT OR UPDATE OF salary, name OR DELETE ON emp
REFERENCING NEW AS n OLD AS o FOR EACH ROW
BEGIN
  IF DELETING THEN
    INSERT INTO log (at, msg, n) VALUES (SYSTIMESTAMP, 'gone', :o.id);
    RETURN;
  END IF;
  :n.name := INITCAP(:n.name);
END;`}, shop, Converted, `
CREATE FUNCTION shop.emp_stamp_fn() RETURNS trigger
LANGUAGE plpgsql
AS $body$
#variable_conflict use_column
BEGIN
  IF (TG_OP = 'DELETE') THEN
    INSERT INTO shop.log (at, msg, n) VALUES (clock_timestamp(), 'gone', OLD.id);
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    RETURN NEW;
  END IF;
  NEW.name := initcap(NEW.name);
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$body$`, `
CREATE TRIGGER emp_stamp BEFORE INSERT OR UPDATE OF salary, name OR DELETE ON shop.emp
FOR EACH ROW
EXECUTE FUNCTION shop.emp_stamp_fn()`)
}

func fn(body string) string {
	return "FUNCTION f(p_n NUMBER, p_s VARCHAR2, p_d DATE, p_i PLS_INTEGER) RETURN NUMBER IS\n" + body
}

func proc(body string) string {
	return "PROCEDURE p(p_n NUMBER, p_s VARCHAR2, p_d DATE, p_i PLS_INTEGER) IS\n" + body
}

func TestOracleToPostgresConverted(t *testing.T) {
	runConversions(t, "oracle", "postgres", []conversion{
		{name: "types", status: Converted, src: proc(`
  a VARCHAR2(10 CHAR); b NUMBER(5); c NUMBER(12); d NUMBER(20); e NUMBER(8,3); f DATE; g TIMESTAMP(3);
  h BOOLEAN := TRUE; i PLS_INTEGER; j CLOB; k CHAR(3); l BINARY_DOUBLE; m INTEGER; n TIMESTAMP WITH LOCAL TIME ZONE;
BEGIN NULL; END;`),
			code: []string{"p(p_n numeric, p_s text, p_d timestamp, p_i integer)", "a varchar(10);", "b integer;", "c bigint;", "d numeric(20,0);", "e numeric(8,3);",
				"f timestamp(0);", "g timestamp(3);", "h boolean := TRUE;", "i integer;", "j text;", "k char(3);", "l double precision;", "m numeric(38,0);", "n timestamptz(6);"}},
		{name: "control flow", status: Converted, src: fn(`
  v NUMBER := 0;
BEGIN
  <<outer>>
  FOR i IN 1..p_i LOOP
    FOR j IN REVERSE 1..3 LOOP
      CONTINUE outer WHEN j = 2;
      EXIT WHEN i > 5;
      v := v + i * j;
    END LOOP;
  END LOOP outer;
  WHILE v > 100 LOOP v := v - 100; END LOOP;
  CASE WHEN v = 1 THEN v := 10; WHEN v = 2 THEN v := 20; ELSE NULL; END CASE;
  CASE p_s WHEN 'a' THEN v := 1; ELSE v := 2; END CASE;
  LOOP v := v + 1; EXIT WHEN v > 3; END LOOP;
  RETURN v;
END;`),
			code: []string{"<<outer>>", "FOR i IN 1..p_i LOOP", "FOR j IN REVERSE 3..1 LOOP", "CONTINUE outer WHEN j = 2;", "EXIT WHEN i > 5;", "END LOOP outer;",
				"WHILE v > 100 LOOP", "CASE\n", "WHEN v = 1 THEN", "CASE p_s", "WHEN 'a' THEN", "END CASE;", "ELSE\n      NULL;"}},
		{name: "null handling", status: Converted, src: fn(`
  v VARCHAR2(20);
BEGIN
  v := NVL(p_s, 'x');
  v := NVL2(p_s, 'set', 'unset');
  v := '';
  IF p_s = '' THEN v := NULL; END IF;
  SELECT DECODE(p_i, 1, 'one', 2, 'two', 'many'), DECODE(p_s, NULL, 'none', p_s) INTO v, v FROM dual;
  RETURN COALESCE(p_n, p_i, 0);
END;`),
			code: []string{"v := COALESCE(p_s, 'x');", "CASE WHEN p_s IS NOT NULL THEN 'set' ELSE 'unset' END", "v := NULL;", "IF p_s = NULL THEN",
				"CASE p_i WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'many' END", "CASE WHEN p_s IS NOT DISTINCT FROM NULL THEN 'none' ELSE p_s END", "INTO STRICT v, v;"},
			absent: []string{"dual", "FROM dual"}},
		{name: "text", status: Converted, src: fn(`
  v VARCHAR2(200);
BEGIN
  v := p_s || '-' || p_i || '-' || p_n;
  v := p_s || p_s;
  v := 'a' || 'b';
  v := UPPER(p_s) || LOWER(p_s) || INITCAP(p_s);
  v := SUBSTR(p_s, 2) || SUBSTR(p_s, 1, 3) || SUBSTR(p_s, 0, 1);
  v := TRIM(p_s) || LTRIM(p_s, 'x') || RTRIM(p_s) || TRIM(BOTH '*' FROM p_s);
  v := LPAD(p_i, 5, '0') || RPAD(p_s, 5);
  v := REPLACE(p_s, 'a', 'b') || REPLACE(p_s, 'a') || REPLACE(p_s, v, v);
  v := TO_CHAR(p_i) || TO_CHAR(p_n) || TO_CHAR(p_d, 'YYYY-MM-DD HH24:MI:SS') || TO_CHAR(p_n, 'FM990.00');
  IF p_s LIKE 'a%' AND p_s NOT LIKE '%\_%' AND p_s LIKE v AND p_s LIKE 'x!_%' ESCAPE '!' THEN NULL; END IF;
  RETURN LENGTH(p_s) + INSTR(p_s, 'x') + ASCII(p_s) + TO_NUMBER(p_s);
END;`),
			code: []string{`concat(p_s, '-', p_i, '-', regexp_replace(trim_scale(p_n)::text, '^(-?)0\.', '\1.'))`, "NULLIF(concat(p_s, p_s), '')", "v := 'a' || 'b';",
				"upper(p_s)", "initcap(p_s)", "NULLIF(substr(p_s, 2), '')", "NULLIF(substr(p_s, 1, 3), '')", "NULLIF(substr(p_s, 1, 1), '')",
				"NULLIF(trim(p_s), '')", "NULLIF(ltrim(p_s, 'x'), '')", "NULLIF(btrim(p_s, '*'), '')", "NULLIF(lpad(p_i::text, 5, '0'), '')", "NULLIF(rpad(p_s, 5), '')",
				"NULLIF(replace(p_s, 'a', 'b'), '')", "NULLIF(replace(p_s, 'a', ''), '')", "replace(p_s, COALESCE(v, ''), COALESCE(v, ''))",
				"p_i::text", "to_char(p_d, 'YYYY-MM-DD HH24:MI:SS')", "to_char(p_n, 'FM990.00')",
				`p_s LIKE 'a%' AND p_s NOT LIKE '%\_%' ESCAPE '' AND p_s LIKE v ESCAPE '' AND p_s LIKE 'x!_%' ESCAPE '!'`,
				"length(p_s) + strpos(p_s, 'x') + ascii(p_s) + CAST(p_s AS numeric)"}},
		{name: "dates", status: Converted, src: fn(`
  v DATE;
  n NUMBER(10);
BEGIN
  v := SYSDATE;
  v := p_d + 7;
  v := p_d - p_i;
  v := 1 + p_d;
  v := p_d + 1/24;
  v := TRUNC(p_d);
  v := TRUNC(p_d, 'MM');
  v := TRUNC(SYSDATE, 'YYYY');
  v := LAST_DAY(p_d);
  v := TO_DATE(p_s, 'YYYY-MM-DD');
  v := DATE '2024-02-29';
  v := p_d + INTERVAL '2' HOUR;
  n := TRUNC(SYSDATE - p_d);
  n := EXTRACT(YEAR FROM p_d);
  RETURN n;
END;`),
			code: []string{"v := date_trunc('second', clock_timestamp())::timestamp(0);", "v := p_d + 7 * INTERVAL '1 day';", "v := p_d - p_i * INTERVAL '1 day';",
				"v := p_d + 1 * INTERVAL '1 day';", "v := (p_d + 1 / 24::numeric * INTERVAL '1 day')::timestamp(0);", "v := date_trunc('day', p_d);",
				"v := date_trunc('month', p_d);", "date_trunc('year', date_trunc('second', clock_timestamp())::timestamp(0))",
				"(date_trunc('month', p_d) + INTERVAL '1 month' - INTERVAL '1 day' + (p_d - date_trunc('day', p_d)))",
				"to_timestamp(p_s, 'YYYY-MM-DD')::timestamp(0)", "TIMESTAMP '2024-02-29 00:00:00'", "p_d + INTERVAL '2' HOUR",
				"n := trunc((EXTRACT(EPOCH FROM (date_trunc('second', clock_timestamp())::timestamp(0) - p_d)) / 86400));", "n := EXTRACT(YEAR FROM p_d);"}},
		{name: "numbers", status: Converted, src: fn(`
  v NUMBER(10,2);
  w PLS_INTEGER;
BEGIN
  v := p_i / 4;
  v := ROUND(p_n / 3, 2);
  v := p_n * 2 - ABS(p_n) + CEIL(p_n) + FLOOR(p_n) + SIGN(p_n) + POWER(p_n, 2) + MOD(p_i, 7) + TRUNC(p_n, 1) + ROUND(p_n);
  w := p_i / p_i;
  v := 2 ** 3;
  RETURN v;
END;`),
			code: []string{"v := p_i / 4::numeric;", "v := round(p_n / 3, 2);", "mod(p_i, 7)", "trunc(p_n, 1)", "round(p_n)", "w := p_i / p_i::numeric;", "power(2, 3)"}},
		{name: "queries", schema: "SHOP", opt: shop, status: Converted, src: proc(`
  v_n NUMBER;
  v_name emp.name%TYPE;
BEGIN
  SELECT COUNT(*), MAX(e.name) INTO v_n, v_name FROM emp e JOIN dept d ON d.id = e.dept_id WHERE e.salary > p_n AND d.name = p_s;
  SELECT e.name INTO v_name FROM emp e WHERE e.id = (SELECT MIN(id) FROM emp WHERE hired > p_d) AND EXISTS (SELECT 1 FROM dept WHERE id = e.dept_id);
  INSERT INTO log (at, msg, n) SELECT SYSTIMESTAMP, name, id FROM emp WHERE dept_id IN (SELECT id FROM dept MINUS SELECT 1 FROM dual);
  UPDATE emp e SET e.salary = e.salary * 1.1, "Note" = 'raised' WHERE e.id BETWEEN 1 AND p_i;
  DELETE emp WHERE hired < ADD_DAYS;
  DELETE FROM log WHERE n IS NULL;
  INSERT INTO log (at, msg, n) VALUES (SYSTIMESTAMP, v_name, seq_log.NEXTVAL) RETURNING n INTO v_n;
  SELECT x.total INTO v_n FROM (SELECT SUM(salary) total FROM emp) x;
  SELECT name INTO v_name FROM emp ORDER BY hired DESC, name FETCH FIRST 1 ROWS ONLY;
END;`),
			code: []string{"SELECT COUNT(*), MAX(e.name) INTO STRICT v_n, v_name FROM shop.emp e JOIN shop.dept d ON d.id = e.dept_id WHERE e.salary > p_n AND d.name = p_s;",
				"(SELECT MIN(id) FROM shop.emp WHERE hired > p_d)", "EXISTS (SELECT 1 FROM shop.dept WHERE id = e.dept_id)",
				"INSERT INTO shop.log (at, msg, n) SELECT clock_timestamp(), name, id FROM shop.emp WHERE dept_id IN (SELECT id FROM shop.dept EXCEPT SELECT 1);",
				`UPDATE shop.emp e SET salary = e.salary * 1.1, "Note" = 'raised' WHERE e.id BETWEEN 1 AND p_i;`, "DELETE FROM shop.emp WHERE hired < add_days;",
				"nextval('shop.seq_log')) RETURNING n INTO v_n;", "FROM (SELECT SUM(salary) total FROM shop.emp) x;", "ORDER BY hired DESC, name LIMIT 1;"}},
		{name: "cursors", schema: "SHOP", opt: shop, status: Converted, src: proc(`
  CURSOR c_dept (p_min NUMBER) IS SELECT id, name FROM dept WHERE budget >= p_min;
  CURSOR c_all IS SELECT * FROM emp;
  v_id dept.id%TYPE;
  v_name dept.name%TYPE;
BEGIN
  OPEN c_dept(p_n);
  LOOP
    FETCH c_dept INTO v_id, v_name;
    EXIT WHEN c_dept%NOTFOUND;
    log_it(v_name);
  END LOOP;
  CLOSE c_dept;
  FOR r IN c_all LOOP
    log_it(r.name || r.salary);
  END LOOP;
  FOR r IN (SELECT id, UPPER(name) AS big FROM emp) LOOP
    log_it(r.big || r.id);
  END LOOP;
END;`),
			code: []string{"c_dept CURSOR (p_min numeric) FOR SELECT id, name FROM shop.dept WHERE budget >= p_min;", "c_all CURSOR FOR SELECT * FROM shop.emp;",
				"OPEN c_dept(p_n);", "FETCH c_dept INTO v_id, v_name;", "EXIT WHEN NOT FOUND;", "CALL shop.log_it(v_name);", "CLOSE c_dept;", "FOR r IN c_all LOOP",
				"r record;", "FOR r IN SELECT id, upper(name) big FROM shop.emp LOOP", "concat(r.big, r.id)"}},
		{name: "exceptions", schema: "SHOP", opt: shop, status: Converted, src: proc(`
  e_low EXCEPTION;
  e_dup EXCEPTION;
  e_app EXCEPTION;
  PRAGMA EXCEPTION_INIT(e_dup, -1);
  PRAGMA EXCEPTION_INIT(e_app, -20007);
  v NUMBER;
BEGIN
  IF p_n < 0 THEN RAISE e_low; END IF;
  IF p_n = 0 THEN RAISE_APPLICATION_ERROR(-20007, 'zero'); END IF;
  BEGIN
    v := ROUND(1 / p_n, 4);
  EXCEPTION
    WHEN ZERO_DIVIDE OR e_dup THEN v := 0;
    WHEN e_app THEN RAISE;
    WHEN OTHERS THEN DBMS_OUTPUT.PUT_LINE('oops'); RAISE NO_DATA_FOUND;
  END;
EXCEPTION
  WHEN e_low THEN NULL;
  WHEN TOO_MANY_ROWS THEN NULL;
END;`),
			code: []string{"RAISE EXCEPTION USING ERRCODE = 'U", "MESSAGE = 'e_low';", "RAISE EXCEPTION '%', 'zero' USING ERRCODE = '45007';",
				"WHEN division_by_zero OR unique_violation THEN", "WHEN SQLSTATE '45007' THEN\n      RAISE;", "WHEN others THEN\n      RAISE NOTICE '%', 'oops';\n      RAISE no_data_found;",
				"WHEN SQLSTATE 'U", "WHEN too_many_rows THEN\n    NULL;"}},
		{name: "names", status: Converted, src: `FUNCTION "Odd Name"(p_user NUMBER) RETURN NUMBER IS
  "Limit" NUMBER := 1; offset NUMBER := 2;
BEGIN RETURN p_user + "Limit" + offset + "UPPER_Q"(1); END;`,
			opt:  Options{Routines: []Routine{{Name: "UPPER_Q", Returns: "NUMBER(4)"}}},
			code: []string{`CREATE FUNCTION "Odd Name"(p_user numeric)`, `"limit" numeric := 1;`, `"offset" numeric := 2;`, `RETURN p_user + "limit" + "offset" + upper_q(1);`}},
		{name: "statement trigger", schema: "SHOP", opt: shop, status: Converted, src: `TRIGGER dept_touch AFTER INSERT OR DELETE ON dept
BEGIN INSERT INTO log (msg) VALUES ('dept changed'); END;`,
			code: []string{"FOR EACH STATEMENT", "RETURN NULL;", "AFTER INSERT OR DELETE ON shop.dept"}},
		{name: "commented and quoted text is left alone", status: Converted, src: fn(`
BEGIN
  -- NVL(SYSDATE) || '' FROM dual
  RETURN LENGTH('NVL(a, b) || SYSDATE /* x */ -- y');
END;`),
			code: []string{"RETURN length('NVL(a, b) || SYSDATE /* x */ -- y');"}},
	})
}

func TestOracleToPostgresReview(t *testing.T) {
	runConversions(t, "oracle", "postgres", []conversion{
		{name: "division digits", status: Review, src: fn(`BEGIN RETURN p_n / 3; END;`), code: []string{"RETURN p_n / 3;"}, notes: []string{"line 2: the division p_n / 3 keeps a different number of digits"}},
		{name: "average", schema: "SHOP", opt: shop, status: Review, src: fn(`v NUMBER; BEGIN SELECT AVG(salary) INTO v FROM emp; RETURN v; END;`), notes: []string{"AVG keeps a different number of digits"}},
		{name: "date difference", status: Review, src: fn(`BEGIN RETURN SYSDATE - p_d; END;`), notes: []string{"number of days with a fraction"}},
		{name: "unknown column types", status: Review, src: fn(`v NUMBER; BEGIN SELECT qty + 1 INTO v FROM things WHERE id = p_i; RETURN v; END;`),
			code: []string{"SELECT qty + 1 INTO STRICT v FROM things WHERE id = p_i;"}, notes: []string{"qty + 1: the types are not known here", "NO_DATA_FOUND"}},
		{name: "greatest", status: Review, src: fn(`BEGIN RETURN GREATEST(p_n, p_i); END;`), code: []string{"GREATEST(p_n, p_i)"}, notes: []string{"GREATEST gives NULL in Oracle when any argument is NULL"}},
		{name: "add_months", status: Review, src: fn(`v DATE; BEGIN v := ADD_MONTHS(p_d, 3); RETURN 1; END;`), code: []string{"(p_d + 3 * INTERVAL '1 month')"}, notes: []string{"ADD_MONTHS moves the last day"}},
		{name: "user", status: Review, src: fn(`v VARCHAR2(30); BEGIN v := USER; RETURN 1; END;`), code: []string{"v := current_user;"}, notes: []string{"USER is the Oracle account in upper case"}},
		{name: "mod by a variable", status: Review, src: fn(`BEGIN RETURN MOD(p_n, p_i); END;`), notes: []string{"MOD gives its first argument in Oracle when the second is 0"}},
		{name: "substr from a variable", status: Review, src: fn(`v VARCHAR2(9); BEGIN v := SUBSTR(p_s, p_i, 2); RETURN 1; END;`), notes: []string{"SUBSTR starts at p_i"}},
		{name: "substr from the end", status: Review, src: fn(`v VARCHAR2(9); BEGIN v := SUBSTR(p_s, -3); RETURN 1; END;`), code: []string{"right(p_s, 3)"}, notes: []string{"3 characters from the end"}},
		{name: "date as text", status: Review, src: fn(`v VARCHAR2(40); BEGIN v := 'on ' || p_d; v := TO_CHAR(p_d); RETURN 1; END;`), notes: []string{"NLS_DATE_FORMAT"}},
		{name: "date compared with text", status: Review, src: fn(`BEGIN IF p_d > '01-JAN-24' THEN RETURN 1; END IF; RETURN 0; END;`), notes: []string{"a date is compared with text"}},
		{name: "odd date format", status: Review, src: fn(`v VARCHAR2(40); BEGIN v := TO_CHAR(p_d, 'DD-MON-RR'); RETURN 1; END;`), notes: []string{"the date format element RR"}},
		{name: "handler after a change", schema: "SHOP", opt: shop, status: Review, src: proc(`
BEGIN
  UPDATE dept SET budget = 0 WHERE id = p_i;
  INSERT INTO log (msg) VALUES (p_s);
EXCEPTION WHEN OTHERS THEN NULL;
END;`), notes: []string{"line 6: the block changes data and has an error handler"}},
		{name: "error message", status: Review, src: proc(`v VARCHAR2(200); BEGIN NULL; EXCEPTION WHEN OTHERS THEN v := SQLERRM; END;`), code: []string{"v := SQLERRM;"}, notes: []string{"the text of an error message is read"}},
		{name: "value_error", status: Review, src: proc(`BEGIN NULL; EXCEPTION WHEN VALUE_ERROR THEN NULL; END;`), code: []string{"WHEN data_exception THEN"}, notes: []string{"VALUE_ERROR is caught as data_exception"}},
		{name: "commit", status: Review, src: proc(`BEGIN COMMIT; END;`), code: []string{"COMMIT;"}, notes: []string{"COMMIT ends the transaction inside the procedure"}},
		{name: "unknown function", status: Review, src: fn(`BEGIN RETURN helper(p_n) + hr.pay(p_i); END;`), code: []string{"helper(p_n) + hr.pay(p_i)"},
			notes: []string{"helper(...) is not a built-in function the converter knows", "pay(...) is taken to be the function PAY of the schema hr"}},
		{name: "unknown name", status: Review, src: fn(`BEGIN RETURN c_limit; END;`), notes: []string{"c_limit is not a variable or a column known here"}},
		{name: "fetched value read after the loop", schema: "SHOP", opt: shop, status: Review, src: fn(`
  CURSOR c IS SELECT id FROM emp; v emp.id%TYPE;
BEGIN
  OPEN c;
  LOOP FETCH c INTO v; EXIT WHEN c%NOTFOUND; END LOOP;
  CLOSE c;
  RETURN v;
END;`), notes: []string{"FETCH leaves its variables as they were in Oracle and sets them to NULL in PostgreSQL"}},
		{name: "cursor attribute far from its fetch", schema: "SHOP", opt: shop, status: Review, src: proc(`
  CURSOR c IS SELECT id FROM emp; v emp.id%TYPE;
BEGIN
  OPEN c;
  FETCH c INTO v;
  UPDATE dept SET budget = 1;
  IF c%FOUND THEN NULL; END IF;
  CLOSE c;
END;`), notes: []string{"c%FOUND is read somewhere other than right after a FETCH", "FETCH leaves its variables"}},
		{name: "no row in a function", schema: "SHOP", opt: shop, status: Review, src: fn(`v emp.name%TYPE; BEGIN SELECT name INTO v FROM emp WHERE id = p_i; RETURN 1; END;`),
			notes: []string{"an Oracle SQL statement calling the function reads that as NULL"}},
		{name: "returning from an update", schema: "SHOP", opt: shop, status: Review, src: proc(`v NUMBER; BEGIN UPDATE dept SET budget = 1 WHERE id = p_i RETURNING budget INTO v; END;`),
			notes: []string{"RETURNING INTO: when no row is changed"}},
		{name: "updating a column", schema: "SHOP", opt: shop, status: Review, src: `TRIGGER t BEFORE UPDATE ON emp FOR EACH ROW BEGIN IF UPDATING('SALARY') THEN :NEW.hired := :OLD.hired; END IF; END;`,
			code: []string{"(TG_OP = 'UPDATE' AND NEW.salary IS DISTINCT FROM OLD.salary)"}, notes: []string{"UPDATING('SALARY') is true in Oracle when the statement sets the column"}},
		{name: "time zone type", status: Review, src: proc(`v TIMESTAMP WITH TIME ZONE; BEGIN v := SYSTIMESTAMP; END;`), notes: []string{"keeps the zone each value was entered in"}},
	})
}

func TestOracleRefused(t *testing.T) {
	cases := []conversion{
		{name: "package", src: `CREATE OR REPLACE PACKAGE BODY pay AS PROCEDURE p IS BEGIN NULL; END; END pay;`, notes: []string{"line 1: a package"}},
		{name: "object type", src: `CREATE TYPE t AS OBJECT (a NUMBER);`, notes: []string{"an object type"}},
		{name: "autonomous transaction", src: proc(`PRAGMA AUTONOMOUS_TRANSACTION; BEGIN COMMIT; END;`), notes: []string{"line 2: an autonomous transaction"}},
		{name: "dynamic sql", src: proc(`BEGIN EXECUTE IMMEDIATE 'DELETE FROM ' || p_s; END;`), notes: []string{"dynamic SQL (EXECUTE IMMEDIATE)"}},
		{name: "connect by", src: fn(`v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM emp START WITH mgr IS NULL CONNECT BY PRIOR id = mgr; RETURN v; END;`), notes: []string{"a hierarchical query (CONNECT BY)"}},
		{name: "connect by level", src: fn(`v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM dual CONNECT BY LEVEL <= 10; RETURN v; END;`), notes: []string{"a hierarchical query (CONNECT BY)"}},
		{name: "bulk collect", src: proc(`TYPE t IS TABLE OF NUMBER; v t; BEGIN SELECT id BULK COLLECT INTO v FROM emp; END;`), notes: []string{"BULK COLLECT"}},
		{name: "collection type", src: proc(`TYPE t IS TABLE OF NUMBER INDEX BY PLS_INTEGER; BEGIN NULL; END;`), notes: []string{"the collection type t"}},
		{name: "record type", src: proc(`TYPE r IS RECORD (a NUMBER, b DATE); BEGIN NULL; END;`), notes: []string{"the record type r"}},
		{name: "rowtype variable", src: proc(`v emp%ROWTYPE; BEGIN NULL; END;`), notes: []string{"a whole row (emp%ROWTYPE)"}},
		{name: "forall", src: proc(`BEGIN FORALL i IN 1..3 DELETE FROM emp WHERE id = i; END;`), notes: []string{"FORALL"}},
		{name: "pipelined", src: `FUNCTION f RETURN t_tab PIPELINED IS BEGIN PIPE ROW(1); RETURN; END;`, notes: []string{"a pipelined function"}},
		{name: "ref cursor", src: proc(`c SYS_REFCURSOR; BEGIN OPEN c FOR SELECT 1 FROM dual; END;`), notes: []string{"a cursor variable (OPEN ... FOR)"}},
		{name: "merge", src: proc(`BEGIN MERGE INTO a USING b ON (a.id = b.id) WHEN MATCHED THEN UPDATE SET a.x = b.x; END;`), notes: []string{"MERGE"}},
		{name: "goto", src: proc(`BEGIN GOTO done; <<done>> NULL; END;`), notes: []string{"GOTO"}},
		{name: "savepoint", src: proc(`BEGIN SAVEPOINT a; ROLLBACK TO a; END;`), notes: []string{"a savepoint"}},
		{name: "outer join operator", src: fn(`v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM a, b WHERE a.id = b.id(+); RETURN v; END;`), notes: []string{"an outer join written with (+)"}},
		{name: "rownum", src: fn(`v NUMBER; BEGIN SELECT id INTO v FROM emp WHERE ROWNUM = 1; RETURN v; END;`), notes: []string{"the pseudo-column ROWNUM"}},
		{name: "supplied package", src: proc(`BEGIN DBMS_LOCK.SLEEP(1); END;`), notes: []string{"DBMS_LOCK.SLEEP belongs to a package Oracle supplies"}},
		{name: "supplied function", src: fn(`BEGIN RETURN UTL_RAW.LENGTH(p_s); END;`), notes: []string{"UTL_RAW.LENGTH belongs to a package Oracle supplies"}},
		{name: "built-in without a rewrite", src: fn(`BEGIN RETURN MONTHS_BETWEEN(SYSDATE, p_d); END;`), notes: []string{"the built-in function MONTHS_BETWEEN has no translation"}},
		{name: "regular expression", src: fn(`BEGIN IF REGEXP_LIKE(p_s, '^a') THEN RETURN 1; END IF; RETURN 0; END;`), notes: []string{"REGEXP_LIKE"}},
		{name: "sqlcode", src: proc(`v NUMBER; BEGIN NULL; EXCEPTION WHEN OTHERS THEN v := SQLCODE; END;`), notes: []string{"SQLCODE"}},
		{name: "to_date without format", src: fn(`v DATE; BEGIN v := TO_DATE(p_s); RETURN 1; END;`), notes: []string{"NLS_DATE_FORMAT"}},
		{name: "nested routine", src: proc(`PROCEDURE inner IS BEGIN NULL; END; BEGIN inner; END;`), notes: []string{"a procedure declared inside the routine"}},
		{name: "instead of trigger", src: `TRIGGER t INSTEAD OF INSERT ON v FOR EACH ROW BEGIN NULL; END;`, notes: []string{"an INSTEAD OF trigger"}},
		{name: "compound trigger", src: `TRIGGER t FOR UPDATE ON emp COMPOUND TRIGGER BEFORE STATEMENT IS BEGIN NULL; END BEFORE STATEMENT; END;`, notes: []string{"a compound trigger"}},
		{name: "ddl trigger", src: `TRIGGER t AFTER CREATE ON SCHEMA BEGIN NULL; END;`, notes: []string{"a trigger on CREATE"}},
		{name: "logon trigger", src: `TRIGGER t AFTER LOGON ON DATABASE BEGIN NULL; END;`, notes: []string{"a trigger on LOGON"}},
		{name: "disabled trigger", src: "TRIGGER t BEFORE INSERT ON emp FOR EACH ROW BEGIN NULL; END;\n/\nALTER TRIGGER t DISABLE;", notes: []string{"a trigger that is disabled on the source"}},
		{name: "trigger calling a procedure", src: `TRIGGER t BEFORE INSERT ON emp FOR EACH ROW CALL audit_it(:NEW.id)`, notes: []string{"a trigger that only calls a procedure"}},
		{name: "external routine", src: `FUNCTION f(a NUMBER) RETURN NUMBER AS LANGUAGE JAVA NAME 'X.f(int) return int';`, notes: []string{"a routine written in another language"}},
		{name: "unknown exception", src: proc(`BEGIN NULL; EXCEPTION WHEN STORAGE_ERROR THEN NULL; END;`), notes: []string{"a handler for STORAGE_ERROR"}},
		{name: "not PL/SQL", src: `this is not a routine`, notes: []string{"could not be read as a routine"}},
		{name: "broken", src: proc(`BEGIN IF THEN END;`), notes: []string{"line 2: the text could not be read as a routine: expected a value, found THEN"}},
	}
	for i := range cases {
		cases[i].status = Unconverted
	}
	runConversions(t, "oracle", "postgres", cases)
	runConversions(t, "oracle", "mysql", cases)
}

func TestOracleToPostgresRefused(t *testing.T) {
	cases := []conversion{
		{name: "out parameter of a function", src: `FUNCTION f(a NUMBER, b OUT NUMBER) RETURN NUMBER IS BEGIN b := 1; RETURN 1; END;`, notes: []string{"the function has the OUT parameter b"}},
		{name: "default in the middle", src: `PROCEDURE p(a NUMBER DEFAULT 1, b NUMBER) IS BEGIN NULL; END;`, notes: []string{"the parameter b has no default and follows one that has"}},
		{name: "commit in a function", src: fn(`BEGIN COMMIT; RETURN 1; END;`), notes: []string{"COMMIT in a function"}},
		{name: "commit under a handler", src: proc(`BEGIN COMMIT; EXCEPTION WHEN OTHERS THEN NULL; END;`), notes: []string{"COMMIT inside a block with an error handler"}},
		{name: "when reading a missing row", src: `TRIGGER t BEFORE INSERT OR DELETE ON emp FOR EACH ROW WHEN (NEW.id > 0) BEGIN NULL; END;`, notes: []string{"the WHEN clause reads a row the trigger does not have on DELETE"}},
		{name: "computed error number", src: proc(`BEGIN RAISE_APPLICATION_ERROR(-20000 - p_i, 'x'); END;`), notes: []string{"an error number that is not a literal"}},
		{name: "reserved helper name", src: proc(`dedie_rowcount NUMBER; BEGIN dedie_rowcount := 1; END;`), notes: []string{"which the converter needs for itself"}},
	}
	for i := range cases {
		cases[i].status = Unconverted
	}
	runConversions(t, "oracle", "postgres", cases)
}
