package codesql

import "testing"

func TestOracleToMySQLWhole(t *testing.T) {
	golden(t, "oracle", "mysql", Object{Schema: "SHOP", Source: `
CREATE OR REPLACE PROCEDURE shop.raise_pay(p_dept IN NUMBER, p_pct IN NUMBER, p_count OUT NUMBER) IS
  CURSOR c IS SELECT id, salary FROM emp WHERE dept_id = p_dept ORDER BY id;
  v_id emp.id%TYPE;
  v_salary emp.salary%TYPE;
  e_too_much EXCEPTION;
BEGIN
  IF p_pct > 50 THEN
    RAISE e_too_much;
  END IF;
  p_count := 0;
  OPEN c;
  LOOP
    FETCH c INTO v_id, v_salary;
    EXIT WHEN c%NOTFOUND;
    UPDATE emp SET salary = ROUND(v_salary * (1 + p_pct / 100), 2) WHERE id = v_id;
    p_count := p_count + 1;
  END LOOP;
  CLOSE c;
EXCEPTION
  WHEN e_too_much THEN
    RAISE_APPLICATION_ERROR(-20001, 'a raise of ' || p_pct || ' percent is too much');
END;`}, shop, Review, `
CREATE PROCEDURE raise_pay(IN p_dept decimal(65,30), IN p_pct decimal(65,30), OUT p_count decimal(65,30))
BEGIN
  DECLARE v_id int;
  DECLARE v_salary decimal(12,2);
  DECLARE dedie_found TINYINT DEFAULT 0;
  DECLARE dedie_message TEXT;
  DECLARE c CURSOR FOR SELECT id, salary FROM emp WHERE dept_id = p_dept ORDER BY id IS NULL, id;
  DECLARE EXIT HANDLER FOR SQLSTATE 'UM9WM'
  BEGIN
    SET dedie_message = COALESCE(CONCAT_WS('', 'a raise of ', CAST(p_pct AS CHAR), ' percent is too much'), ' ');
    SIGNAL SQLSTATE '45001' SET MESSAGE_TEXT = dedie_message;
  END;
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET dedie_found = 0;
  IF p_pct > 50 THEN
    SIGNAL SQLSTATE 'UM9WM' SET MESSAGE_TEXT = 'e_too_much';
  END IF;
  SET p_count = 0;
  OPEN c;
  loop1: LOOP
    SET dedie_found = 1;
    FETCH c INTO v_id, v_salary;
    IF (dedie_found = 0) THEN
      LEAVE loop1;
    END IF;
    UPDATE emp SET salary = ROUND(v_salary * (1 + p_pct / 100), 2) WHERE id = v_id;
    SET p_count = p_count + 1;
  END LOOP;
  CLOSE c;
END`)

	golden(t, "oracle", "mysql", Object{Schema: "SHOP", Source: `
TRIGGER emp_stamp BEFORE INSERT OR UPDATE OR DELETE ON emp FOR EACH ROW WHEN (NVL(NEW.dept_id, OLD.dept_id) > 0)
BEGIN
  IF DELETING THEN
    INSERT INTO log (at, msg, n) VALUES (SYSTIMESTAMP, 'gone', :OLD.id);
    RETURN;
  END IF;
  :NEW.name := UPPER(:NEW.name);
  IF UPDATING AND :NEW.salary < :OLD.salary THEN
    :NEW.salary := :OLD.salary;
  END IF;
END;`}, shop, Converted, `
CREATE TRIGGER emp_stamp_ins BEFORE INSERT ON emp
FOR EACH ROW
BEGIN
  IF COALESCE(NEW.dept_id, NULL) > 0 THEN
    SET NEW.name = UPPER(NEW.name);
  END IF;
END`, `
CREATE TRIGGER emp_stamp_upd BEFORE UPDATE ON emp
FOR EACH ROW
BEGIN
  IF COALESCE(NEW.dept_id, OLD.dept_id) > 0 THEN
    SET NEW.name = UPPER(NEW.name);
    IF NEW.salary < OLD.salary THEN
      SET NEW.salary = OLD.salary;
    END IF;
  END IF;
END`, `
CREATE TRIGGER emp_stamp_del BEFORE DELETE ON emp
FOR EACH ROW
BEGIN
  IF COALESCE(NULL, OLD.dept_id) > 0 THEN
    INSERT INTO log (at, msg, n) VALUES (NOW(6), 'gone', OLD.id);
  END IF;
END`)
}

func TestOracleToMySQLConverted(t *testing.T) {
	runConversions(t, "oracle", "mysql", []conversion{
		{name: "types", schema: "SHOP", opt: shop, status: Converted, src: proc(`
  a VARCHAR2(10 CHAR); b NUMBER(2); c NUMBER(4); d NUMBER(9); e NUMBER(18); f NUMBER(30); g NUMBER(8,3); h DATE; i TIMESTAMP(3);
  j BOOLEAN := TRUE; k PLS_INTEGER; l CLOB; m CHAR(3); n BINARY_DOUBLE; o INTEGER; q emp.salary%TYPE; r NUMBER;
BEGIN NULL; END;`),
			code: []string{"p(IN p_n decimal(65,30), IN p_s text CHARACTER SET utf8mb4 COLLATE utf8mb4_bin, IN p_d datetime, IN p_i int)", "DECLARE a varchar(10) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;",
				"DECLARE b tinyint;", "DECLARE c smallint;", "DECLARE d int;", "DECLARE e bigint;", "DECLARE f decimal(30,0);", "DECLARE g decimal(8,3);", "DECLARE h datetime;", "DECLARE i datetime(3);",
				"DECLARE j tinyint(1) DEFAULT TRUE;", "DECLARE k int;", "DECLARE l longtext CHARACTER SET", "DECLARE m char(3) CHARACTER SET", "DECLARE n double;", "DECLARE o decimal(38,0);",
				"DECLARE q decimal(12,2);", "DECLARE r decimal(65,30);", "BEGIN END;"}},
		{name: "null handling and text", status: Converted, src: fn(`
  v VARCHAR2(200);
BEGIN
  v := NVL(p_s, 'x') || NVL2(p_s, 'set', 'unset');
  v := '';
  v := p_s || '-' || p_i;
  v := p_s || p_s;
  v := UPPER(p_s) || LOWER(p_s) || SUBSTR(p_s, 2) || SUBSTR(p_s, 1, 3) || SUBSTR(p_s, -3) || TRIM(p_s) || LTRIM(p_s) || RTRIM(p_s, 'x') || TRIM(BOTH '*' FROM p_s);
  v := LPAD(p_i, 5, '0') || RPAD(p_s, 5) || REPLACE(p_s, 'a', 'b') || REPLACE(p_s, 'a') || TO_CHAR(p_i) || TO_CHAR(p_d, 'YYYY-MM-DD HH24:MI:SS');
  IF p_s LIKE 'a%' AND p_s LIKE v THEN NULL; END IF;
  SELECT DECODE(p_i, 1, 'one', 'many'), DECODE(p_s, NULL, 'none', p_s) INTO v, v FROM dual;
  RETURN LENGTH(p_s) + INSTR(p_s, 'x');
END;`),
			code: []string{"SET v = NULLIF(CONCAT_WS('', COALESCE(p_s, 'x'), CASE WHEN p_s IS NOT NULL THEN 'set' ELSE 'unset' END), '');", "SET v = NULL;", "SET v = CONCAT_WS('', p_s, '-', p_i);",
				"SET v = NULLIF(CONCAT_WS('', p_s, p_s), '');", "NULLIF(SUBSTRING(p_s, 2), '')", "NULLIF(SUBSTRING(p_s, -3), '')", "NULLIF(TRIM(p_s), '')", "NULLIF(TRIM(TRAILING 'x' FROM p_s), '')",
				"NULLIF(TRIM(BOTH '*' FROM p_s), '')", "NULLIF(LPAD(CAST(p_i AS CHAR), 5, '0'), '')", "NULLIF(RPAD(p_s, 5, ' '), '')", "NULLIF(REPLACE(p_s, 'a', ''), '')", "CAST(p_i AS CHAR)",
				"DATE_FORMAT(p_d, '%Y-%m-%d %H:%i:%s')", "p_s LIKE 'a%' AND p_s LIKE v ESCAPE ''", "CASE p_i WHEN 1 THEN 'one' ELSE 'many' END", "CASE WHEN p_s <=> NULL THEN 'none' ELSE p_s END",
				"INTO v, v FROM dual;", "RETURN CHAR_LENGTH(p_s) + INSTR(p_s, 'x');"}},
		{name: "dates", status: Converted, src: fn(`
  v DATE; n NUMBER(10);
BEGIN
  v := SYSDATE; v := p_d + 7; v := p_d - p_i; v := p_d + 1/24; v := TRUNC(p_d); v := TRUNC(p_d, 'MM'); v := LAST_DAY(p_d);
  v := DATE '2024-02-29'; v := p_d + INTERVAL '2' HOUR; n := TRUNC(SYSDATE - p_d); n := EXTRACT(YEAR FROM p_d);
  RETURN n;
END;`),
			code: []string{"SET v = NOW();", "SET v = (p_d + INTERVAL 7 DAY);", "SET v = (p_d - INTERVAL p_i DAY);", "SET v = (p_d + INTERVAL ROUND(1 / 24 * 86400) SECOND);",
				"SET v = CAST(DATE(p_d) AS DATETIME);", "SET v = CAST(DATE_FORMAT(p_d, '%Y-%m-01') AS DATETIME);", "SET v = TIMESTAMP(LAST_DAY(p_d), TIME(p_d));",
				"SET v = TIMESTAMP '2024-02-29 00:00:00';", "SET v = (p_d + INTERVAL 2 HOUR);", "SET n = TRUNCATE((TIMESTAMPDIFF(SECOND, p_d, NOW()) / 86400), 0);", "SET n = EXTRACT(YEAR FROM p_d);"}},
		{name: "loops and records", schema: "SHOP", opt: shop, status: Converted, src: proc(`
  CURSOR c_all IS SELECT * FROM dept;
  v NUMBER(9) := 0;
BEGIN
  FOR r IN (SELECT e.id, e.name, e.salary * 2 AS twice, e.hired FROM emp e WHERE e.hired < p_d) LOOP
    v := v + r.id;
    EXIT WHEN r.twice > 1000;
  END LOOP;
  FOR d IN c_all LOOP
    v := v + d.id;
  END LOOP;
  FOR i IN REVERSE 1..p_i LOOP
    CONTINUE WHEN MOD(i, 2) = 0;
    v := v + i;
  END LOOP;
  WHILE v > 10 LOOP v := v - 10; END LOOP;
END;`),
			code: []string{"DECLARE r_id int;", "DECLARE r_name varchar(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;", "DECLARE r_twice DECIMAL(65,2);", "DECLARE r_hired datetime;",
				"DECLARE r_cursor CURSOR FOR SELECT e.id, e.name, e.salary * 2 twice, e.hired FROM emp e WHERE e.hired < p_d;", "FETCH r_cursor INTO r_id, r_name, r_twice, r_hired;",
				"SET v = v + r_id;", "IF r_twice > 1000 THEN", "DECLARE d_id smallint;", "DECLARE d_budget decimal(65,30);", "OPEN c_all;", "FETCH c_all INTO d_id, d_name, d_budget;", "CLOSE c_all;",
				"DECLARE i INT DEFAULT p_i;", "DECLARE i_end INT DEFAULT 1;", "WHILE i >= i_end DO", "IF MOD(i, 2) = 0 THEN", "SET i = i - 1;", "WHILE v > 10 DO"}},
		{name: "errors", schema: "SHOP", opt: shop, status: Converted, src: proc(`
  e_dup EXCEPTION;
  PRAGMA EXCEPTION_INIT(e_dup, -1);
  v NUMBER;
BEGIN
  IF p_n = 0 THEN RAISE_APPLICATION_ERROR(-20007, 'zero'); END IF;
  BEGIN
    INSERT INTO dept (id) VALUES (p_i);
  EXCEPTION
    WHEN DUP_VAL_ON_INDEX OR e_dup THEN v := 0;
    WHEN NO_DATA_FOUND THEN RAISE;
    WHEN OTHERS THEN v := 1; RAISE NO_DATA_FOUND;
  END;
END;`),
			code: []string{"SIGNAL SQLSTATE '45007' SET MESSAGE_TEXT = 'zero';", "DECLARE EXIT HANDLER FOR 1062, 1062", "DECLARE EXIT HANDLER FOR SQLSTATE 'P0002'\n    BEGIN\n      RESIGNAL;",
				"DECLARE EXIT HANDLER FOR SQLEXCEPTION\n    BEGIN\n      SET v = 1;\n      SIGNAL SQLSTATE 'P0002' SET MESSAGE_TEXT = 'no data found';"}},
		{name: "a column named like a variable", schema: "SHOP", opt: shop, status: Converted, src: `PROCEDURE p(id NUMBER, name VARCHAR2) IS
  salary NUMBER := 1;
BEGIN
  UPDATE emp SET salary = salary + 1 WHERE id = id AND dept_id = p.id;
  UPDATE emp e SET e.name = name WHERE e.id > 0;
END;`,
			code:  []string{"UPDATE emp SET salary = emp.salary + 1 WHERE emp.id = emp.id AND dept_id = id;", "UPDATE emp e SET name = e.name WHERE e.id > 0;"},
			notes: nil},
	})
}

func TestOracleToMySQLReview(t *testing.T) {
	runConversions(t, "oracle", "mysql", []conversion{
		{name: "default parameter", status: Review, src: `FUNCTION f(a NUMBER, b NUMBER DEFAULT 2) RETURN NUMBER IS BEGIN RETURN a * b; END;`, notes: []string{"the parameter b has a default value"}},
		{name: "number as text", status: Review, src: fn(`v VARCHAR2(40); BEGIN v := 'n=' || p_n; RETURN 1; END;`), notes: []string{"Oracle writes 0.5 as .5 and drops trailing zeros, MySQL does not"}},
		{name: "division", status: Review, src: fn(`BEGIN RETURN ROUND(p_n / p_i, 2); END;`), notes: []string{"raises an error in Oracle when the divisor is 0 and gives NULL in MySQL"}},
		{name: "division digits", status: Review, src: fn(`BEGIN RETURN p_n / 3; END;`), notes: []string{"keeps a different number of digits in MySQL"}},
		{name: "rows touched by an update", schema: "SHOP", opt: shop, status: Review, src: proc(`BEGIN UPDATE dept SET budget = 0 WHERE id = p_i; IF SQL%ROWCOUNT = 0 THEN NULL; END IF; END;`),
			code: []string{"SET dedie_rowcount = ROW_COUNT();", "IF dedie_rowcount = 0 THEN"}, notes: []string{"MySQL counts the rows whose values changed, Oracle the rows that matched"}},
		{name: "output", status: Review, src: proc(`BEGIN DBMS_OUTPUT.PUT_LINE('done'); END;`), code: []string{"BEGIN END;"}, notes: []string{"DBMS_OUTPUT.PUT_LINE is dropped"}},
		{name: "to_date", status: Review, src: fn(`v DATE; BEGIN v := TO_DATE(p_s, 'YYYY-MM-DD'); RETURN 1; END;`), code: []string{"CAST(STR_TO_DATE(p_s, '%Y-%m-%d') AS DATETIME)"}, notes: []string{"STR_TO_DATE gives NULL instead"}},
		{name: "to_number", status: Review, src: fn(`BEGIN RETURN TO_NUMBER(p_s); END;`), notes: []string{"TO_NUMBER raises an error in Oracle"}},
		{name: "substr from a variable", status: Review, src: fn(`v VARCHAR2(9); BEGIN v := SUBSTR(p_s, p_i); RETURN 1; END;`), notes: []string{"if that can be 0, Oracle starts at the first character and MySQL gives nothing"}},
		{name: "function that changes data", schema: "SHOP", opt: shop, status: Review, src: fn(`BEGIN DELETE FROM dept WHERE id = p_i; RETURN 1; END;`), code: []string{"MODIFIES SQL DATA"}, notes: []string{"log_bin_trust_function_creators"}},
		{name: "commit", schema: "SHOP", opt: shop, status: Review, src: proc(`BEGIN DELETE FROM dept; COMMIT; END;`), notes: []string{"COMMIT ends the transaction inside the procedure"}},
		{name: "variable that may be a column", status: Review, src: proc(`v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM things WHERE kind = p_s; END;`), notes: []string{"p_s is a variable; if a table of this statement also has a column of that name"}},
		{name: "order by position", schema: "SHOP", opt: shop, status: Review, src: proc(`BEGIN FOR r IN (SELECT id FROM dept ORDER BY 1) LOOP NULL; END LOOP; END;`), notes: []string{"rows are ordered by a column position"}},
	})
}

func TestOracleToMySQLRefused(t *testing.T) {
	cases := []conversion{
		{name: "sequence", src: fn(`BEGIN RETURN my_seq.NEXTVAL; END;`), notes: []string{"the sequence my_seq: MySQL has no sequences"}},
		{name: "out parameter of a function", src: `FUNCTION f(a NUMBER, b OUT NUMBER) RETURN NUMBER IS BEGIN b := 1; RETURN 1; END;`, notes: []string{"a MySQL function only takes values in"}},
		{name: "returning", src: proc(`v NUMBER; BEGIN INSERT INTO t (a) VALUES (1) RETURNING id INTO v; END;`), notes: []string{"INSERT ... RETURNING"}},
		{name: "type from an unknown column", src: proc(`v things.id%TYPE; BEGIN NULL; END;`), notes: []string{"MySQL needs the type spelled out"}},
		{name: "loop over an unknown table", src: proc(`BEGIN FOR r IN (SELECT * FROM things) LOOP NULL; END LOOP; END;`), notes: []string{"reads every column (*) of a table the converter was not told about"}},
		{name: "cursor with parameters", schema: "SHOP", opt: shop, src: proc(`CURSOR c (k NUMBER) IS SELECT id FROM dept WHERE id = k; BEGIN OPEN c(1); CLOSE c; END;`), notes: []string{"the cursor c takes parameters"}},
		{name: "statement trigger", src: `TRIGGER t AFTER INSERT ON dept BEGIN NULL; END;`, notes: []string{"a statement-level trigger"}},
		{name: "update of a column", src: `TRIGGER t BEFORE UPDATE OF salary ON emp FOR EACH ROW BEGIN NULL; END;`, notes: []string{"a trigger on UPDATE OF certain columns"}},
		{name: "trigger on its own table", schema: "SHOP", opt: shop, src: `TRIGGER t AFTER INSERT ON dept FOR EACH ROW BEGIN UPDATE dept SET budget = 0; END;`, notes: []string{"the trigger changes the table it belongs to"}},
		{name: "zero_divide handler", src: proc(`v NUMBER; BEGIN v := 1; EXCEPTION WHEN ZERO_DIVIDE THEN v := 0; END;`), notes: []string{"a handler for division by zero"}},
		{name: "raise of a built-in error", src: proc(`BEGIN RAISE TOO_MANY_ROWS; END;`), notes: []string{"RAISE TOO_MANY_ROWS"}},
		{name: "number format", src: fn(`v VARCHAR2(20); BEGIN v := TO_CHAR(p_n, '990.00'); RETURN 1; END;`), notes: []string{"MySQL has no formats for numbers"}},
		{name: "month names", src: fn(`v VARCHAR2(20); BEGIN v := TO_CHAR(p_d, 'DD-MON-YYYY'); RETURN 1; END;`), notes: []string{"the date format element MON has no MySQL counterpart"}},
		{name: "initcap", src: fn(`v VARCHAR2(20); BEGIN v := INITCAP(p_s); RETURN 1; END;`), notes: []string{"the built-in function INITCAP has no translation"}},
		{name: "listagg", src: fn(`v VARCHAR2(200); BEGIN SELECT LISTAGG(name, ',') WITHIN GROUP (ORDER BY name) INTO v FROM emp; RETURN 1; END;`), notes: []string{"LISTAGG"}},
		{name: "where current of", schema: "SHOP", opt: shop, src: proc(`CURSOR c IS SELECT id FROM dept FOR UPDATE; v NUMBER; BEGIN OPEN c; FETCH c INTO v; UPDATE dept SET budget = 0 WHERE CURRENT OF c; CLOSE c; END;`), notes: []string{"WHERE CURRENT OF c"}},
		{name: "commit in a trigger", src: `TRIGGER t AFTER INSERT ON emp FOR EACH ROW BEGIN COMMIT; END;`, notes: []string{"COMMIT in a trigger"}},
	}
	for i := range cases {
		cases[i].status = Unconverted
	}
	runConversions(t, "oracle", "mysql", cases)
}
