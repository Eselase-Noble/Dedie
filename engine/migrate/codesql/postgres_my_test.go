package codesql

import "testing"

// depot is a small PostgreSQL catalog for the tests that need to know
// the type of a column.
var depot = Options{
	Tables: []Table{
		{Schema: "public", Name: "emp", Columns: []Column{{Name: "id", Type: "integer"}, {Name: "name", Type: "character varying(40)"},
			{Name: "salary", Type: "numeric(12,2)"}, {Name: "hired", Type: "date"}, {Name: "seen", Type: "timestamp without time zone"},
			{Name: "active", Type: "boolean"}, {Name: "stamp", Type: "timestamp with time zone"}}},
		{Schema: "audit", Name: "log", Target: "audit_log", Columns: []Column{{Name: "at", Type: "timestamp without time zone"}, {Name: "msg", Type: "text"}, {Name: "n", Type: "bigint"}}},
	},
	Routines: []Routine{{Name: "bonus", Returns: "numeric(10,2)"}, {Name: "log_it"}},
}

func TestPostgresToMySQLWhole(t *testing.T) {
	golden(t, "postgres", "mysql", Object{Source: `CREATE OR REPLACE FUNCTION public.net_pay(p_emp integer, p_rate numeric)
 RETURNS numeric
 LANGUAGE plpgsql
 STABLE STRICT
AS $function$
DECLARE
  v_salary numeric(12,2);
  v_name varchar(40);
BEGIN
  SELECT salary, name INTO STRICT v_salary, v_name FROM emp WHERE id = p_emp;
  IF v_salary IS NULL THEN
    RETURN 0;
  END IF;
  RETURN round(v_salary - v_salary * p_rate, 2) + bonus(p_emp);
EXCEPTION
  WHEN no_data_found THEN
    RETURN NULL;
END;
$function$
`}, depot, Converted, "CREATE FUNCTION net_pay(p_emp INT, p_rate DECIMAL(65,20)) RETURNS DECIMAL(65,20)\n"+`    READS SQL DATA
BEGIN
  DECLARE v_salary DECIMAL(12,2);
  DECLARE v_name VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
  DECLARE EXIT HANDLER FOR SQLSTATE 'P0002'
  BEGIN
    RETURN NULL;
  END;
  IF p_emp IS NULL OR p_rate IS NULL THEN
    RETURN NULL;
  END IF;
  BEGIN
    DECLARE EXIT HANDLER FOR NOT FOUND SIGNAL SQLSTATE 'P0002' SET MESSAGE_TEXT = 'query returned no rows';
    SELECT salary, name INTO v_salary, v_name FROM emp WHERE id = p_emp;
  END;
  IF v_salary IS NULL THEN
    RETURN 0;
  END IF;
  RETURN ROUND(v_salary - v_salary * p_rate, 2) + bonus(p_emp);
END`)

	golden(t, "postgres", "mysql", Object{Source: `CREATE TRIGGER emp_stamp BEFORE INSERT OR UPDATE OR DELETE ON public.emp FOR EACH ROW WHEN ((pg_trigger_depth() = 0)) EXECUTE FUNCTION emp_stamp_fn();
CREATE OR REPLACE FUNCTION public.emp_stamp_fn()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
  IF TG_OP = 'DELETE' THEN
    INSERT INTO audit.log (at, msg, n) VALUES (LOCALTIMESTAMP, 'gone from ' || TG_TABLE_NAME, OLD.id);
    RETURN OLD;
  END IF;
  NEW.name := upper(NEW.name);
  IF TG_OP IN ('UPDATE') AND NEW.salary < OLD.salary THEN
    RAISE EXCEPTION 'pay cut for %', OLD.name USING ERRCODE = 'ZZ001';
  END IF;
  RETURN NEW;
END;
$function$`}, Options{Tables: depot.Tables, Routines: []Routine{{Name: "pg_trigger_depth", Returns: "integer"}}}, Converted, `
CREATE TRIGGER emp_stamp_ins BEFORE INSERT ON emp
FOR EACH ROW
BEGIN
  IF (pg_trigger_depth() = 0) THEN
    SET NEW.name = UPPER(NEW.name);
  END IF;
END`, `
CREATE TRIGGER emp_stamp_upd BEFORE UPDATE ON emp
FOR EACH ROW
BEGIN
  DECLARE dedie_message TEXT;
  IF (pg_trigger_depth() = 0) THEN
    SET NEW.name = UPPER(NEW.name);
    IF NEW.salary < OLD.salary THEN
      SET dedie_message = CONCAT('pay cut for ', COALESCE(OLD.name, '<NULL>'));
      SIGNAL SQLSTATE 'ZZ001' SET MESSAGE_TEXT = dedie_message;
    END IF;
  END IF;
END`, `
CREATE TRIGGER emp_stamp_del BEFORE DELETE ON emp
FOR EACH ROW
BEGIN
  IF (pg_trigger_depth() = 0) THEN
    INSERT INTO audit_log (at, msg, n) VALUES (NOW(6), CONCAT('gone from ', 'emp'), OLD.id);
  END IF;
END`)
}

func pgFn(body string) string {
	return "CREATE OR REPLACE FUNCTION public.f(p_n numeric, p_s text, p_d date, p_i integer, p_t timestamp without time zone, p_b boolean)\n RETURNS integer\n LANGUAGE plpgsql\nAS $function$\n" + body + "\n$function$"
}

func pgProc(body string) string {
	return "CREATE OR REPLACE PROCEDURE public.p(IN p_n numeric, IN p_s text, INOUT p_i integer)\n LANGUAGE plpgsql\nAS $procedure$\n" + body + "\n$procedure$"
}

func TestPostgresToMySQLConverted(t *testing.T) {
	runConversions(t, "postgres", "mysql", []conversion{
		{name: "types", opt: depot, status: Converted, src: pgProc(`DECLARE
  a smallint; b int; c bigint; d numeric(8,3); e real; f double precision; g varchar(20); h text; i char(3); j boolean := true;
  k date; l timestamp; m uuid; n jsonb; o bytea; q emp.salary%TYPE; r CONSTANT integer := 5; s character varying(9);
BEGIN NULL; END`),
			code: []string{"p(IN p_n DECIMAL(65,20), IN p_s LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin, INOUT p_i INT)", "DECLARE a SMALLINT;", "DECLARE b INT;", "DECLARE c BIGINT;",
				"DECLARE d DECIMAL(8,3);", "DECLARE e FLOAT;", "DECLARE f DOUBLE;", "DECLARE g VARCHAR(20) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;", "DECLARE h LONGTEXT CHARACTER SET",
				"DECLARE i CHAR(3) CHARACTER SET", "DECLARE j TINYINT(1) DEFAULT TRUE;", "DECLARE k DATE;", "DECLARE l DATETIME(6);", "DECLARE m CHAR(36) CHARACTER SET", "DECLARE n JSON;",
				"DECLARE o LONGBLOB;", "DECLARE q DECIMAL(12,2);", "DECLARE r INT DEFAULT 5;", "DECLARE s VARCHAR(9) CHARACTER SET", "BEGIN END;"}},
		{name: "control flow", opt: depot, status: Converted, src: pgFn(`DECLARE v integer := 0;
BEGIN
  IF p_i IS NULL THEN v := 1; ELSIF p_i = 0 THEN v := 2; ELSE v := 3; END IF;
  <<outer_loop>>
  FOR i IN 1..p_i LOOP
    FOR j IN REVERSE 5..1 BY 2 LOOP
      CONTINUE WHEN j = 3;
      EXIT outer_loop WHEN i > 5;
      v := v + i * j;
    END LOOP;
  END LOOP;
  WHILE v > 100 LOOP v := v - 100; END LOOP;
  LOOP v := v + 1; EXIT WHEN v > 3; END LOOP;
  CASE p_i WHEN 1, 2 THEN v := 0; ELSE NULL; END CASE;
  CASE WHEN v > 1 THEN v := 1; END CASE;
  RETURN v;
END`),
			code: []string{"ELSEIF p_i = 0 THEN", "DECLARE i INT DEFAULT 1;", "DECLARE i_end INT DEFAULT p_i;", "outer_loop: WHILE i <= i_end DO", "DECLARE j INT DEFAULT 5;", "DECLARE j_end INT DEFAULT 1;",
				"WHILE j >= j_end DO", "next2: BEGIN", "LEAVE next2;", "LEAVE outer_loop;", "SET j = j - 2;", "SET i = i + 1;",
				"WHILE v > 100 DO", "LOOP\n    SET v = v + 1;\n    IF v > 3 THEN\n      LEAVE loop", "CASE p_i\n    WHEN 1 THEN\n      SET v = 0;\n    WHEN 2 THEN\n      SET v = 0;\n    ELSE\n      BEGIN END;", "END CASE;"}},
		{name: "text", opt: depot, status: Converted, src: pgFn(`DECLARE v text;
BEGIN
  v := p_s || '-' || p_i || '-' || p_d;
  v := concat(p_s, p_i, NULL) || concat_ws(',', p_s, p_i);
  v := upper(p_s) || lower(p_s) || reverse(p_s) || repeat(p_s, 2) || replace(p_s, 'a', 'b');
  v := substr(p_s, 2) || substring(p_s, 1, 3) || substring(p_s FROM 2 FOR 3) || left(p_s, 2) || right(p_s, 3);
  v := trim(p_s) || ltrim(p_s) || rtrim(p_s) || btrim(p_s, 'x') || trim(leading '0' from p_s) || lpad(p_i::text, 5, '0') || rpad(p_s, 5);
  v := 'it''s a \ backslash' || E'tab\there';
  v := to_char(p_t, 'YYYY-MM-DD HH24:MI:SS') || p_i::text || p_d::varchar || CAST(p_i AS text);
  IF p_s LIKE 'a%' AND p_s NOT LIKE '%\_%' AND p_s LIKE '50!%' ESCAPE '!' THEN v := NULL; END IF;
  RETURN length(p_s) + char_length(p_s) + octet_length(p_s) + strpos(p_s, 'x') + position('x' IN p_s);
END`),
			code: []string{"SET v = CONCAT(p_s, '-', CAST(p_i AS CHAR), '-', CAST(p_d AS CHAR));", "CONCAT(CONCAT_WS('', p_s, CAST(p_i AS CHAR), NULL), CONCAT_WS(',', p_s, CAST(p_i AS CHAR)))",
				"CONCAT(UPPER(p_s), LOWER(p_s), REVERSE(p_s), REPEAT(p_s, 2), REPLACE(p_s, 'a', 'b'))", "CONCAT(SUBSTRING(p_s, 2), SUBSTRING(p_s, 1, 3), SUBSTRING(p_s, 2, 3), LEFT(p_s, 2), RIGHT(p_s, 3))",
				"CONCAT(TRIM(p_s), LTRIM(p_s), RTRIM(p_s), TRIM(BOTH 'x' FROM p_s), TRIM(LEADING '0' FROM p_s), LPAD(CAST(p_i AS CHAR), 5, '0'), RPAD(p_s, 5, ' '))",
				`SET v = CONCAT('it''s a \\ backslash', 'tab	here');`, "CONCAT(DATE_FORMAT(p_t, '%Y-%m-%d %H:%i:%s'), CAST(p_i AS CHAR), CAST(p_d AS CHAR), CAST(p_i AS CHAR))",
				`p_s LIKE 'a%' AND p_s NOT LIKE '%\\_%' AND p_s LIKE '50!%' ESCAPE '!'`, "RETURN CHAR_LENGTH(p_s) + CHAR_LENGTH(p_s) + LENGTH(p_s) + LOCATE('x', p_s) + LOCATE('x', p_s);"}},
		{name: "dates", opt: depot, status: Converted, src: pgFn(`DECLARE d date; t timestamp; n integer;
BEGIN
  d := CURRENT_DATE; t := LOCALTIMESTAMP;
  d := p_d + 7; d := p_d - p_i; n := p_d - d;
  t := p_t + interval '2 hours'; t := p_t - INTERVAL '30 minutes'; t := p_d + interval '1 month'; t := p_t + p_i * interval '1 day';
  t := p_t + '1 week'::interval; t := date_trunc('day', p_t); t := date_trunc('month', p_t);
  n := extract(year FROM p_d) + extract(month FROM p_t) + date_part('day', p_d) + extract(dow FROM p_d) + extract(isodow FROM p_d) + extract(doy FROM p_d);
  d := DATE '2024-02-29'; t := TIMESTAMP '2024-02-29 10:00:00'; d := '2024-01-01'::date; d := p_t::date; t := p_d::timestamp;
  RETURN n;
END`),
			code: []string{"SET d = CURDATE();", "SET t = NOW(6);", "SET d = (p_d + INTERVAL 7 DAY);", "SET d = (p_d - INTERVAL p_i DAY);", "SET n = DATEDIFF(p_d, d);",
				"SET t = (p_t + INTERVAL 2 HOUR);", "SET t = (p_t - INTERVAL 30 MINUTE);", "SET t = CAST(p_d + INTERVAL 1 MONTH AS DATETIME);", "SET t = (p_t + INTERVAL p_i DAY);",
				"SET t = (p_t + INTERVAL 1 WEEK);", "SET t = CAST(DATE(p_t) AS DATETIME(6));", "SET t = CAST(DATE_FORMAT(p_t, '%Y-%m-01') AS DATETIME(6));",
				"EXTRACT(YEAR FROM p_d) + EXTRACT(MONTH FROM p_t) + EXTRACT(DAY FROM p_d) + (DAYOFWEEK(p_d) - 1) + (WEEKDAY(p_d) + 1) + DAYOFYEAR(p_d)",
				"SET d = DATE '2024-02-29';", "SET t = TIMESTAMP '2024-02-29 10:00:00';", "SET d = CAST('2024-01-01' AS DATE);", "SET d = CAST(p_t AS DATE);", "SET t = CAST(p_d AS DATETIME(6));"}},
		{name: "numbers", opt: depot, status: Converted, src: pgFn(`DECLARE v numeric(10,2); w integer;
BEGIN
  w := p_i / 4; w := p_i % 4; w := mod(p_i, 3); v := round(p_n / 3, 2); v := round(p_i::numeric / 3, 2);
  v := p_n * 2 - abs(p_n) + ceil(p_n) + floor(p_n) + sign(p_n) + trunc(p_n, 1) + trunc(p_n) + round(p_n) + 2 ^ 3 + power(p_n, 2);
  v := coalesce(p_n, 0) + nullif(p_i, 0) + p_n::integer + CAST(p_s AS numeric(10,2));
  RETURN v;
END`),
			code: []string{"SET w = p_i DIV 4;", "SET w = p_i % 4;", "SET w = (p_i % 3);", "SET v = ROUND(p_n / 3, 2);", "SET v = ROUND(p_i / 3, 2);",
				"ABS(p_n) + CEILING(p_n) + FLOOR(p_n) + SIGN(p_n) + TRUNCATE(p_n, 1) + TRUNCATE(p_n, 0) + ROUND(p_n) + POW(2, 3) + POW(p_n, 2)",
				"COALESCE(p_n, 0) + NULLIF(p_i, 0) + CAST(p_n AS SIGNED) + CAST(p_s AS DECIMAL(10,2))"}},
		{name: "truth values", opt: depot, status: Converted, src: pgFn(`DECLARE ok boolean := false;
BEGIN
  ok := p_i > 3 OR p_b;
  IF ok AND NOT p_b THEN ok := p_b IS NOT TRUE; END IF;
  IF p_s IS DISTINCT FROM 'x' AND p_i IS NOT DISTINCT FROM NULL THEN RETURN 1; END IF;
  RETURN CASE WHEN ok THEN 1 ELSE 0 END;
END`),
			code: []string{"DECLARE ok TINYINT(1) DEFAULT FALSE;", "SET ok = p_i > 3 OR p_b;", "IF ok AND NOT p_b THEN", "SET ok = p_b IS NOT TRUE;", "IF NOT (p_s <=> 'x') AND p_i <=> NULL THEN"}},
		{name: "queries", opt: depot, status: Converted, src: pgProc(`DECLARE v_n integer; v_name varchar(40);
BEGIN
  SELECT count(*), max(e.name) INTO v_n, v_name FROM emp e WHERE e.salary > p_n AND e.active;
  SELECT name INTO v_name FROM emp WHERE id = p_i ORDER BY hired DESC, name;
  SELECT name INTO STRICT v_name FROM public.emp WHERE id = p_i;
  INSERT INTO audit.log (at, msg, n) SELECT LOCALTIMESTAMP, name, id FROM emp WHERE id IN (SELECT id FROM emp WHERE salary IS NULL) ORDER BY id NULLS FIRST LIMIT 10 OFFSET 5;
  UPDATE emp e SET salary = e.salary * 1.1, active = false WHERE e.id BETWEEN 1 AND p_i;
  DELETE FROM emp WHERE hired < '2000-01-01' AND NOT EXISTS (SELECT 1 FROM audit.log l WHERE l.n = emp.id);
  CALL log_it(v_name, bonus(p_i));
  PERFORM bonus(p_i);
END`),
			code: []string{"SELECT COUNT(*), MAX(e.name) INTO v_n, v_name FROM emp e WHERE e.salary > p_n AND e.active;",
				"DECLARE CONTINUE HANDLER FOR NOT FOUND SET v_name = NULL;\n    SELECT name INTO v_name FROM emp WHERE id = p_i ORDER BY hired IS NOT NULL, hired DESC, name IS NULL, name LIMIT 1;",
				"DECLARE EXIT HANDLER FOR NOT FOUND SIGNAL SQLSTATE 'P0002' SET MESSAGE_TEXT = 'query returned no rows';\n    SELECT name INTO v_name FROM emp WHERE id = p_i;",
				"INSERT INTO audit_log (at, msg, n) SELECT NOW(6), name, id FROM emp WHERE id IN (SELECT id FROM emp WHERE salary IS NULL) ORDER BY id LIMIT 10 OFFSET 5;",
				"UPDATE emp e SET salary = e.salary * 1.1, active = FALSE WHERE e.id BETWEEN 1 AND p_i;", "NOT EXISTS (SELECT 1 FROM audit_log l WHERE l.n = emp.id)",
				"CALL log_it(v_name, bonus(p_i));", "DO bonus(p_i);"}},
		{name: "record loops", opt: depot, status: Converted, src: pgProc(`DECLARE r record; c CURSOR FOR SELECT id, upper(name) AS big, salary * 2 AS twice, hired FROM emp;
BEGIN
  FOR r IN SELECT * FROM emp WHERE active LOOP
    CALL log_it(r.name, r.salary);
  END LOOP;
  FOR x IN c LOOP
    p_i := p_i + x.id + length(x.big);
    EXIT WHEN x.twice > 100;
  END LOOP;
END`),
			code: []string{"DECLARE r_id INT;", "DECLARE r_name VARCHAR(40) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;", "DECLARE r_salary DECIMAL(12,2);", "DECLARE r_stamp DATETIME(6);", "DECLARE r_done TINYINT DEFAULT 0;",
				"DECLARE r_cursor CURSOR FOR SELECT * FROM emp WHERE active;", "DECLARE CONTINUE HANDLER FOR NOT FOUND SET r_done = 1;", "OPEN r_cursor;",
				"SET r_done = 0;\n      FETCH r_cursor INTO r_id, r_name, r_salary, r_hired, r_seen, r_active, r_stamp;\n      IF r_done = 1 THEN\n        LEAVE loop1;\n      END IF;\n      CALL log_it(r_name, r_salary);",
				"CLOSE r_cursor;", "DECLARE x_big LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;", "DECLARE x_twice DECIMAL(65,2);", "DECLARE x_hired DATE;", "OPEN c;", "FETCH c INTO x_id, x_big, x_twice, x_hired;",
				"SET p_i = p_i + x_id + CHAR_LENGTH(x_big);", "IF x_twice > 100 THEN\n        LEAVE loop2;", "CLOSE c;"}},
		{name: "errors", opt: depot, status: Converted, src: pgProc(`DECLARE v integer := 0;
BEGIN
  IF p_n < 0 THEN RAISE EXCEPTION 'negative: % and 100%%', p_i; END IF;
  IF p_n = 0 THEN RAISE EXCEPTION USING ERRCODE = 'ZZ123', MESSAGE = 'zero'; END IF;
  IF p_n = 1 THEN RAISE SQLSTATE 'ZZ124'; END IF;
  BEGIN
    v := 1;
  EXCEPTION
    WHEN unique_violation OR foreign_key_violation THEN v := 1;
    WHEN SQLSTATE 'ZZ123' THEN v := 3; RAISE;
    WHEN raise_exception THEN v := 5;
    WHEN OTHERS THEN v := 4;
  END;
END`),
			code: []string{"SET dedie_message = CONCAT('negative: ', COALESCE(CAST(p_i AS CHAR), '<NULL>'), ' and 100%');", "SIGNAL SQLSTATE 'P0001' SET MESSAGE_TEXT = dedie_message;",
				"SIGNAL SQLSTATE 'ZZ123' SET MESSAGE_TEXT = 'zero';", "SIGNAL SQLSTATE 'ZZ124';", "DECLARE EXIT HANDLER FOR 1062, 1451, 1452", "DECLARE EXIT HANDLER FOR SQLSTATE 'ZZ123'\n    BEGIN\n      SET v = 3;\n      RESIGNAL;",
				"DECLARE EXIT HANDLER FOR SQLSTATE 'P0001'", "DECLARE EXIT HANDLER FOR SQLEXCEPTION"}},
		{name: "sql function", opt: depot, status: Converted, src: `CREATE OR REPLACE FUNCTION public.top_name(integer, numeric)
 RETURNS character varying
 LANGUAGE sql
 STABLE
AS $function$ SELECT name FROM emp WHERE id > $1 AND salary > $2 ORDER BY salary DESC $function$`,
			code: []string{"CREATE FUNCTION top_name(p1 INT, p2 DECIMAL(65,20)) RETURNS LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin\n    READS SQL DATA",
				"RETURN (SELECT name FROM emp WHERE id > p1 AND salary > p2 ORDER BY salary IS NOT NULL, salary DESC LIMIT 1);"}},
		{name: "sql function of an expression", status: Converted, src: `CREATE FUNCTION add_tax(amount numeric, rate numeric) RETURNS numeric LANGUAGE sql IMMUTABLE RETURN round(amount * (1 + rate), 2)`,
			code: []string{"DETERMINISTIC NO SQL", "RETURN ROUND(amount * (1 + rate), 2);"}},
		{name: "names", opt: depot, status: Converted, src: `CREATE FUNCTION "Odd Name"("Limit" integer, key text) RETURNS integer LANGUAGE plpgsql AS $$ DECLARE "Offset" integer := 1; BEGIN RETURN "Limit" + "Offset" + length(KEY); END $$`,
			code: []string{"CREATE FUNCTION `Odd Name`(`limit` INT, `key` LONGTEXT", "DECLARE offset INT DEFAULT 1;", "RETURN `limit` + offset + CHAR_LENGTH(`key`);"}},
	})
}

func TestPostgresToMySQLReview(t *testing.T) {
	runConversions(t, "postgres", "mysql", []conversion{
		{name: "division by a variable", opt: depot, status: Review, src: pgFn(`BEGIN RETURN p_i / p_i; END`), code: []string{"RETURN p_i DIV p_i;"}, notes: []string{"raises an error in PostgreSQL when the divisor is 0 and gives NULL in MySQL"}},
		{name: "division digits", opt: depot, status: Review, src: pgProc(`BEGIN p_i := 1; INSERT INTO audit.log (n, msg) VALUES (1, (p_n / 3)::text); END`), notes: []string{"the division p_n / 3 keeps a different number of digits"}},
		{name: "division of unknown types", status: Review, src: pgFn(`DECLARE v numeric(9,2); BEGIN SELECT a / 2 INTO v FROM things LIMIT 1; RETURN 1; END`), notes: []string{"the types in the division a / 2 are not known here"}},
		{name: "sum of unknown types", status: Review, src: pgFn(`DECLARE v integer; BEGIN SELECT a + 1 INTO v FROM things LIMIT 1; RETURN 1; END`), notes: []string{"a + 1: the types are not known here"}},
		{name: "greatest", opt: depot, status: Review, src: pgFn(`BEGIN RETURN greatest(p_i, 3); END`), notes: []string{"GREATEST gives NULL in MySQL when any argument is NULL; PostgreSQL ignores the NULL arguments"}},
		{name: "substring from a variable", opt: depot, status: Review, src: pgFn(`BEGIN RETURN length(substr(p_s, p_i)); END`), notes: []string{"substr starts at p_i"}},
		{name: "number without a scale as text", opt: depot, status: Review, src: pgFn(`BEGIN RETURN length('x' || p_n); END`), notes: []string{"its type has no fixed scale in PostgreSQL"}},
		{name: "truth value as text", opt: depot, status: Review, src: pgFn(`BEGIN RETURN length('x' || p_b); END`), notes: []string{"the truth value p_b is joined to text"}},
		{name: "timestamp as text", opt: depot, status: Review, src: pgFn(`BEGIN RETURN length('x' || p_t); END`), notes: []string{"the timestamp p_t is joined to text"}},
		{name: "current time with zone", opt: depot, status: Review, src: pgFn(`DECLARE t timestamptz := now(); BEGIN RETURN 1; END`), code: []string{"DEFAULT UTC_TIMESTAMP(6)"}, notes: []string{"becomes UTC_TIMESTAMP(6)", "a timestamp with time zone becomes DATETIME(6)"}},
		{name: "default parameter", status: Review, src: `CREATE FUNCTION f(a integer, b integer DEFAULT 2) RETURNS integer LANGUAGE sql IMMUTABLE RETURN a * b`, notes: []string{"the parameter b has a default value"}},
		{name: "void function", opt: depot, status: Review, src: `CREATE FUNCTION touch(p integer) RETURNS void LANGUAGE plpgsql AS $$ BEGIN UPDATE emp SET seen = LOCALTIMESTAMP WHERE id = p; END $$`,
			code: []string{"CREATE PROCEDURE touch(IN p INT)"}, notes: []string{"the function returns void and becomes a procedure"}},
		{name: "function that changes data", opt: depot, status: Review, src: pgFn(`BEGIN DELETE FROM emp WHERE id = p_i; RETURN 1; END`), code: []string{"MODIFIES SQL DATA"}, notes: []string{"log_bin_trust_function_creators"}},
		{name: "notice", opt: depot, status: Review, src: pgProc(`BEGIN RAISE NOTICE 'at %', p_i; p_i := 1; END`), absent: []string{"SIGNAL"}, notes: []string{"RAISE NOTICE is dropped"}},
		{name: "detail and hint", opt: depot, status: Review, src: pgProc(`BEGIN RAISE EXCEPTION 'no' USING DETAIL = 'why', HINT = 'how'; END`), notes: []string{"the DETAIL and HINT of the error is dropped"}},
		{name: "handler after a change", opt: depot, status: Review, src: pgProc(`BEGIN UPDATE emp SET salary = 0; INSERT INTO emp (id) VALUES (p_i); EXCEPTION WHEN OTHERS THEN p_i := -1; END`),
			notes: []string{"when the handler runs, PostgreSQL has undone everything the block did, where MySQL keeps"}},
		{name: "error text", opt: depot, status: Review, src: pgProc(`BEGIN p_i := 1; EXCEPTION WHEN OTHERS THEN INSERT INTO audit.log (msg) VALUES (SQLERRM || SQLSTATE); END`),
			code:  []string{"GET DIAGNOSTICS CONDITION 1 dedie_errtext = MESSAGE_TEXT, dedie_errstate = RETURNED_SQLSTATE;", "CONCAT(dedie_errtext, dedie_errstate)", "DECLARE dedie_errtext TEXT;"},
			notes: []string{"the text of an error message is read", "SQLSTATE is read"}},
		{name: "rows touched by an update", opt: depot, status: Review, src: pgProc(`BEGIN UPDATE emp SET salary = 0 WHERE id = p_i; IF NOT FOUND THEN p_i := -1; END IF; GET DIAGNOSTICS p_i = ROW_COUNT; END`),
			code: []string{"SET dedie_rowcount = ROW_COUNT();\n  SET dedie_found = (dedie_rowcount > 0);", "IF NOT (dedie_found = 1) THEN"}, notes: []string{"MySQL counts the rows whose values changed"}},
		{name: "fetched value read after the loop", opt: depot, status: Review, src: pgProc(`DECLARE c CURSOR FOR SELECT id FROM emp; v integer;
BEGIN OPEN c; LOOP FETCH c INTO v; EXIT WHEN NOT FOUND; END LOOP; CLOSE c; p_i := v; END`), notes: []string{"FETCH leaves its variables as they were in"}},
		{name: "commit", opt: depot, status: Review, src: pgProc(`BEGIN UPDATE emp SET salary = 0; COMMIT; END`), code: []string{"COMMIT;"}, notes: []string{"COMMIT ends the transaction inside the procedure"}},
		{name: "unknown function", opt: depot, status: Review, src: pgFn(`BEGIN RETURN helper(p_i); END`), notes: []string{"helper(...) is not a built-in function the converter knows"}},
		{name: "not null variable", opt: depot, status: Review, src: pgFn(`DECLARE v integer NOT NULL := 1; BEGIN RETURN v; END`), notes: []string{"declared NOT NULL, which MySQL cannot enforce"}},
		{name: "order by position", opt: depot, status: Review, src: pgFn(`DECLARE v integer; BEGIN SELECT id INTO v FROM emp ORDER BY 1; RETURN v; END`), notes: []string{"rows are ordered by a column position"}},
		{name: "extract second", opt: depot, status: Review, src: pgFn(`BEGIN RETURN extract(second FROM p_t); END`), notes: []string{"EXTRACT(SECOND ...) includes the fraction"}},
	})
}

func TestPostgresToMySQLRefused(t *testing.T) {
	cases := []conversion{
		{name: "set-returning function", src: `CREATE FUNCTION f() RETURNS SETOF integer LANGUAGE plpgsql AS $$ BEGIN RETURN NEXT 1; END $$`, notes: []string{"RETURN NEXT: the function returns rows"}},
		{name: "returns table", src: `CREATE FUNCTION f() RETURNS TABLE(a integer, b text) LANGUAGE sql AS $$ SELECT 1, 'x' $$`, notes: []string{"a function in SQL whose last query gives several columns"}},
		{name: "return query", src: `CREATE FUNCTION f() RETURNS SETOF emp LANGUAGE plpgsql AS $$ BEGIN RETURN QUERY SELECT * FROM emp; END $$`, notes: []string{"RETURN QUERY"}},
		{name: "out parameter of a function", src: `CREATE FUNCTION f(a integer, OUT b integer) RETURNS integer LANGUAGE plpgsql AS $$ BEGIN b := a; END $$`, notes: []string{"the function has the OUT parameter b"}},
		{name: "array type", src: pgFn(`DECLARE a integer[]; BEGIN RETURN 1; END`), notes: []string{"the type INTEGER[] of the variable a has no translation"}},
		{name: "array element", src: pgFn(`DECLARE a integer[]; BEGIN RETURN a[1]; END`), notes: []string{"an array element"}},
		{name: "any of an array", src: pgFn(`BEGIN RETURN (p_i = ANY(ARRAY[1,2]))::int; END`), notes: []string{"a comparison with ANY or ALL"}},
		{name: "foreach", src: pgFn(`DECLARE x integer; BEGIN FOREACH x IN ARRAY ARRAY[1,2] LOOP NULL; END LOOP; RETURN 1; END`), notes: []string{"FOREACH over an array"}},
		{name: "dynamic sql", src: pgProc(`BEGIN EXECUTE 'DELETE FROM ' || p_s; END`), notes: []string{"dynamic SQL (EXECUTE)"}},
		{name: "loop over dynamic sql", src: pgProc(`DECLARE r record; BEGIN FOR r IN EXECUTE 'SELECT 1' LOOP NULL; END LOOP; END`), notes: []string{"a FOR loop over dynamic SQL"}},
		{name: "returning", src: pgProc(`BEGIN INSERT INTO emp (id) VALUES (1) RETURNING id INTO p_i; END`), notes: []string{"INSERT ... RETURNING"}},
		{name: "on conflict", src: pgProc(`BEGIN INSERT INTO emp (id) VALUES (1) ON CONFLICT (id) DO NOTHING; END`), notes: []string{"INSERT ... ON CONFLICT"}},
		{name: "update from", src: pgProc(`BEGIN UPDATE emp SET salary = d.x FROM dept d WHERE d.id = emp.id; END`), notes: []string{"UPDATE ... FROM"}},
		{name: "delete using", src: pgProc(`BEGIN DELETE FROM emp USING dept d WHERE d.id = emp.id; END`), notes: []string{"a DELETE that joins another table"}},
		{name: "record variable", src: pgProc(`DECLARE r record; BEGIN SELECT * INTO r FROM emp LIMIT 1; p_i := r.id; END`), notes: []string{"the record r is used as a whole"}},
		{name: "rowtype variable", src: pgProc(`DECLARE r emp%ROWTYPE; BEGIN NULL; END`), notes: []string{"a whole row (emp%ROWTYPE)"}},
		{name: "loop record of unknown shape", src: pgProc(`DECLARE r record; BEGIN FOR r IN SELECT * FROM things LOOP NULL; END LOOP; END`), notes: []string{"reads every column (*) of a table the converter was not told about"}},
		{name: "loop column of unknown type", src: pgProc(`DECLARE r record; BEGIN FOR r IN SELECT a FROM things LOOP p_i := r.a; END LOOP; END`), notes: []string{"the type of r.a is not known"}},
		{name: "cursor with parameters", src: pgProc(`DECLARE c CURSOR (k integer) FOR SELECT id FROM emp WHERE id = k; BEGIN OPEN c(1); CLOSE c; END`), notes: []string{"the cursor c takes parameters"}},
		{name: "ilike", src: pgFn(`BEGIN RETURN (p_s ILIKE 'a%')::int; END`), notes: []string{"ILIKE"}},
		{name: "regular expression", src: pgFn(`BEGIN IF p_s ~ '^a' THEN RETURN 1; END IF; RETURN 0; END`), notes: []string{"a regular expression operator"}},
		{name: "sequence", src: pgFn(`BEGIN RETURN nextval('emp_id_seq'); END`), notes: []string{"the built-in function NEXTVAL has no translation"}},
		{name: "string_agg", src: pgFn(`DECLARE v text; BEGIN SELECT string_agg(name, ',') INTO v FROM emp; RETURN 1; END`), notes: []string{"STRING_AGG"}},
		{name: "interval variable", src: pgFn(`DECLARE i interval := '1 day'; BEGIN RETURN 1; END`), notes: []string{"the type INTERVAL of the variable i has no translation"}},
		{name: "timestamp difference", opt: depot, src: pgFn(`DECLARE t timestamp; BEGIN IF p_t - t > interval '1 day' THEN RETURN 1; END IF; RETURN 0; END`), notes: []string{"gives an interval, a type MySQL does not have"}},
		{name: "division by zero handler", src: pgProc(`BEGIN p_i := 1; EXCEPTION WHEN division_by_zero THEN p_i := 0; END`), notes: []string{"a handler for division by zero: MySQL gives NULL"}},
		{name: "handler for a postgres error code", src: pgProc(`BEGIN p_i := 1; EXCEPTION WHEN SQLSTATE '22003' THEN p_i := 0; END`), notes: []string{"a handler for SQLSTATE 22003"}},
		{name: "others before another handler", src: pgProc(`BEGIN p_i := 1; EXCEPTION WHEN OTHERS THEN p_i := 0; WHEN unique_violation THEN p_i := 2; END`), notes: []string{"a handler for every error that is not the last handler"}},
		{name: "return in an inner handler", src: pgProc(`BEGIN BEGIN p_i := 1; EXCEPTION WHEN OTHERS THEN RETURN; END; p_i := 2; END`), notes: []string{"RETURN inside an error handler"}},
		{name: "exit from a handler", src: pgProc(`BEGIN LOOP BEGIN p_i := 1; EXCEPTION WHEN OTHERS THEN EXIT; END; END LOOP; END`), notes: []string{"leaves a loop from inside an error handler"}},
		{name: "stacked diagnostics", src: pgProc(`DECLARE m text; BEGIN p_i := 1; EXCEPTION WHEN OTHERS THEN GET STACKED DIAGNOSTICS m = MESSAGE_TEXT; END`), notes: []string{"GET STACKED DIAGNOSTICS"}},
		{name: "raise a built-in error", src: pgProc(`BEGIN RAISE unique_violation; END`), notes: []string{"RAISE unique_violation"}},
		{name: "commit in a function", src: pgFn(`BEGIN COMMIT; RETURN 1; END`), notes: []string{"COMMIT in a function"}},
		{name: "use_column", src: "CREATE FUNCTION f(id integer) RETURNS integer LANGUAGE plpgsql AS $$\n#variable_conflict use_column\nBEGIN RETURN id; END $$", notes: []string{"#variable_conflict use_column"}},
		{name: "another language", src: `CREATE FUNCTION f() RETURNS integer LANGUAGE plpython3u AS $$ return 1 $$`, notes: []string{"a routine written in plpython3u"}},
		{name: "trigger function alone", src: `CREATE FUNCTION f() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`, notes: []string{"a trigger function on its own"}},
		{name: "trigger without its function", src: `CREATE TRIGGER t BEFORE INSERT ON emp FOR EACH ROW EXECUTE FUNCTION f()`, notes: []string{"the trigger comes without the function it executes"}},
		{name: "statement trigger", src: pgTrigger("AFTER INSERT ON emp FOR EACH STATEMENT", "RETURN NULL;"), notes: []string{"a statement-level trigger"}},
		{name: "instead of trigger", src: pgTrigger("INSTEAD OF INSERT ON emp_view FOR EACH ROW", "RETURN NEW;"), notes: []string{"an INSTEAD OF trigger"}},
		{name: "update of columns", src: pgTrigger("BEFORE UPDATE OF salary ON emp FOR EACH ROW", "RETURN NEW;"), notes: []string{"a trigger on UPDATE OF certain columns"}},
		{name: "truncate trigger", src: pgTrigger("AFTER TRUNCATE ON emp FOR EACH STATEMENT", "RETURN NULL;"), notes: []string{"a trigger on TRUNCATE"}},
		{name: "transition tables", src: pgTrigger("AFTER INSERT ON emp REFERENCING NEW TABLE AS added FOR EACH STATEMENT", "RETURN NULL;"), notes: []string{"transition tables"}},
		{name: "trigger arguments", src: "CREATE TRIGGER t BEFORE INSERT ON emp FOR EACH ROW EXECUTE FUNCTION f('a');\nCREATE FUNCTION f() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$", notes: []string{"a trigger that passes arguments"}},
		{name: "trigger that skips the row", src: pgTrigger("BEFORE INSERT ON emp FOR EACH ROW", "IF NEW.id < 0 THEN RETURN NULL; END IF; RETURN NEW;"), notes: []string{"the trigger returns NULL, which makes PostgreSQL skip the row"}},
		{name: "trigger that keeps the old row", src: pgTrigger("BEFORE UPDATE ON emp FOR EACH ROW", "RETURN OLD;"), notes: []string{"on UPDATE the trigger returns OLD rather than NEW"}},
		{name: "trigger that changes its own table", opt: depot, src: pgTrigger("AFTER INSERT ON emp FOR EACH ROW", "UPDATE emp SET seen = LOCALTIMESTAMP WHERE id = NEW.id; RETURN NULL;"), notes: []string{"the trigger changes the table it belongs to"}},
		{name: "constraint trigger", src: `CREATE CONSTRAINT TRIGGER t AFTER INSERT ON emp DEFERRABLE FOR EACH ROW EXECUTE FUNCTION f()`, notes: []string{"a constraint trigger"}},
	}
	for i := range cases {
		cases[i].status = Unconverted
	}
	runConversions(t, "postgres", "mysql", cases)
}

func pgTrigger(head, body string) string {
	return "CREATE TRIGGER t " + head + " EXECUTE FUNCTION f();\nCREATE OR REPLACE FUNCTION public.f()\n RETURNS trigger\n LANGUAGE plpgsql\nAS $function$\nBEGIN\n  " + body + "\nEND;\n$function$"
}
