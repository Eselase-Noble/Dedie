package codesql

import "testing"

// store is a small MySQL catalog for the tests that need to know the
// type of a column.
var store = Options{
	ExactText:    true,
	TargetSchema: "public",
	Tables: []Table{
		{Schema: "shop", Name: "emp", Columns: []Column{{Name: "id", Type: "int"}, {Name: "name", Type: "varchar(40)"},
			{Name: "salary", Type: "decimal(12,2)"}, {Name: "hired", Type: "date"}, {Name: "seen", Type: "datetime"}, {Name: "isActive", Type: "tinyint(1)"}}},
		{Schema: "shop", Name: "Log", Target: "Log", Columns: []Column{{Name: "at", Type: "timestamp"}, {Name: "msg", Type: "text"}, {Name: "n", Type: "int unsigned"}}},
	},
	Routines: []Routine{{Name: "bonus", Returns: "decimal(10,2)"}, {Name: "log_it"}},
}

func TestMySQLToPostgresWhole(t *testing.T) {
	golden(t, "mysql", "postgres", Object{Schema: "shop", Source: "CREATE DEFINER=`root`@`%` PROCEDURE `raise_pay`(IN `p_pct` decimal(5,2), OUT p_count int)\n" +
		"    MODIFIES SQL DATA\n    COMMENT 'yearly raise'\n" + `BEGIN
  DECLARE v_id INT;
  DECLARE v_salary DECIMAL(12,2);
  DECLARE done TINYINT DEFAULT 0;
  DECLARE cur CURSOR FOR SELECT id, salary FROM emp WHERE isactive = 1 ORDER BY id;
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;
  SET p_count = 0;
  OPEN cur;
  each_emp: LOOP
    FETCH cur INTO v_id, v_salary;
    IF done THEN LEAVE each_emp; END IF;
    IF v_salary IS NULL THEN ITERATE each_emp; END IF;
    UPDATE emp SET salary = ROUND(v_salary * (1 + p_pct / 100), 2) WHERE id = v_id;
    SET p_count = p_count + 1;
  END LOOP each_emp;
  CLOSE cur;
END`}, store, Converted, `
CREATE PROCEDURE public.raise_pay(p_pct numeric, OUT p_count integer)
LANGUAGE plpgsql
AS $body$
#variable_conflict use_variable
DECLARE
  v_id integer;
  v_salary numeric(12,2);
  done smallint := 0;
  cur CURSOR FOR SELECT id, salary FROM public.emp WHERE "isActive" = 1 ORDER BY id NULLS FIRST;
BEGIN
  p_pct := p_pct::numeric(5,2);
  p_count := 0;
  OPEN cur;
  <<each_emp>>
  LOOP
    FETCH cur INTO v_id, v_salary;
    IF NOT FOUND THEN
      done := 1;
    END IF;
    IF done <> 0 THEN
      EXIT each_emp;
    END IF;
    IF v_salary IS NULL THEN
      CONTINUE each_emp;
    END IF;
    UPDATE public.emp SET salary = round(v_salary * (1 + p_pct / 100), 2) WHERE id = v_id;
    p_count := p_count + 1;
  END LOOP each_emp;
  CLOSE cur;
END;
$body$`)

	golden(t, "mysql", "postgres", Object{Schema: "shop", Source: "CREATE DEFINER=`app`@`localhost` TRIGGER `emp_bi` BEFORE INSERT ON `emp` FOR EACH ROW " +
		"BEGIN\n  SET NEW.name = TRIM(NEW.name);\n  IF NEW.hired IS NULL THEN SET NEW.hired = CURDATE(); END IF;\nEND"}, store, Converted, `
CREATE FUNCTION public.emp_bi_fn() RETURNS trigger
LANGUAGE plpgsql
AS $body$
#variable_conflict use_variable
BEGIN
  NEW.name := btrim(NEW.name, ' ');
  IF NEW.hired IS NULL THEN
    NEW.hired := statement_timestamp()::date;
  END IF;
  RETURN NEW;
END;
$body$`, `
CREATE TRIGGER emp_bi BEFORE INSERT ON public.emp
FOR EACH ROW
EXECUTE FUNCTION public.emp_bi_fn()`)
}

func myFn(body string) string {
	return "CREATE FUNCTION f(p_n DECIMAL(10,2), p_s VARCHAR(50), p_d DATE, p_i INT, p_t DATETIME) RETURNS INT DETERMINISTIC\n" + body
}

func myProc(body string) string {
	return "CREATE PROCEDURE p(IN p_n DECIMAL(10,2), IN p_s VARCHAR(50), INOUT p_i INT)\n" + body
}

func TestMySQLToPostgresConverted(t *testing.T) {
	runConversions(t, "mysql", "postgres", []conversion{
		{name: "types", opt: store, status: Converted, src: myProc(`BEGIN
  DECLARE a TINYINT; DECLARE b SMALLINT UNSIGNED; DECLARE c INT UNSIGNED; DECLARE d BIGINT; DECLARE e BIGINT UNSIGNED;
  DECLARE f DECIMAL(8,3); DECLARE g, h DOUBLE; DECLARE i VARCHAR(20) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; DECLARE j TEXT;
  DECLARE k DATETIME(3); DECLARE l TIMESTAMP; DECLARE m BOOLEAN DEFAULT TRUE; DECLARE n CHAR(3); DECLARE o JSON; DECLARE q BLOB; DECLARE r FLOAT;
END`),
			code: []string{"p(p_n numeric, p_s varchar, INOUT p_i integer)", "a smallint;", "b integer;", "c bigint;", "d bigint;", "e numeric(20,0);", "f numeric(8,3);",
				"g double precision;", "h double precision;", "i varchar(20);", "j text;", "k timestamp(3);", "l timestamptz;", "m smallint := 1;", "n varchar(3);", "o jsonb;", "q bytea;", "r real;"}},
		{name: "truth values are numbers", opt: store, status: Converted, src: myFn(`BEGIN
  DECLARE ok INT DEFAULT FALSE;
  SET ok = p_i > 3;
  SET ok = (p_s IS NULL) + (p_i BETWEEN 1 AND 5);
  IF ok THEN SET ok = NOT ok; END IF;
  WHILE NOT ok DO SET ok = TRUE; END WHILE;
  IF p_i AND !ok OR ISNULL(p_s) THEN RETURN IF(ok, 1, 2); END IF;
  RETURN p_i = 1 || p_i <=> NULL;
END`),
			code: []string{"ok integer := 0;", "ok := (p_i > 3)::int;", "ok := (p_s IS NULL)::int + (p_i BETWEEN 1 AND 5)::int;", "IF ok <> 0 THEN", "ok := (NOT ok <> 0)::int;",
				"WHILE NOT ok <> 0 LOOP", "ok := 1;", "IF (p_i <> 0 AND NOT ok <> 0) OR (p_s IS NULL)::int <> 0 THEN", "RETURN CASE WHEN ok <> 0 THEN 1 ELSE 2 END;",
				"RETURN (p_i = 1 OR p_i IS NOT DISTINCT FROM NULL)::int;"}},
		{name: "control flow", opt: store, status: Converted, src: myFn(`main: BEGIN
  DECLARE v INT DEFAULT 0;
  IF p_i IS NULL THEN LEAVE main; ELSEIF p_i = 0 THEN SET v = 1; ELSE SET v = 2; END IF;
  REPEAT SET v = v + 1; UNTIL v > 10 END REPEAT;
  outer_loop: WHILE v < 100 DO
    inner_loop: LOOP
      SET v = v * 2;
      IF v > 50 THEN LEAVE outer_loop; END IF;
      IF v % 3 = 0 THEN ITERATE outer_loop; END IF;
      LEAVE inner_loop;
    END LOOP inner_loop;
  END WHILE outer_loop;
  CASE p_i WHEN 1 THEN SET v = 0; ELSE BEGIN END; END CASE;
  CASE WHEN v > 1 THEN SET v = 1; WHEN v < 0 THEN SET v = 0; END CASE;
  RETURN v;
END`),
			code: []string{"<<main>>", "EXIT main;", "ELSIF p_i = 0 THEN", "LOOP\n    v := v + 1;\n    EXIT WHEN v > 10;\n  END LOOP;", "<<outer_loop>>\n  WHILE v < 100 LOOP",
				"EXIT outer_loop;", "IF v % 3 = 0 THEN", "CONTINUE outer_loop;", "EXIT inner_loop;", "END LOOP inner_loop;", "END LOOP outer_loop;", "CASE p_i", "END CASE;"}},
		{name: "text", opt: store, status: Converted, src: myFn(`BEGIN
  DECLARE v VARCHAR(200);
  SET v = CONCAT(p_s, '-', p_i, '-', p_n, '-', p_d, '-', p_t);
  SET v = CONCAT_WS(',', p_s, p_i, NULL);
  SET v = CONCAT(UPPER(p_s), LCASE(p_s), REVERSE(p_s), REPEAT(p_s, 2), REPLACE(p_s, 'a', 'b'));
  SET v = CONCAT(SUBSTRING(p_s, 2), SUBSTR(p_s, 1, 3), MID(p_s, 2, 1), SUBSTRING(p_s FROM 2 FOR 3), LEFT(p_s, 2), RIGHT(p_s, 3));
  SET v = CONCAT(TRIM(p_s), LTRIM(p_s), RTRIM(p_s), TRIM(BOTH 'x' FROM p_s), TRIM(LEADING '0' FROM p_s), LPAD(p_i, 5, '0'), RPAD(p_s, 5, ' '));
  SET v = "double \"quoted\" and 'single' with a \\ backslash";
  IF p_s LIKE 'a%' AND p_s NOT LIKE '%\_%' AND p_s LIKE '50!%' ESCAPE '!' THEN SET v = NULL; END IF;
  RETURN LENGTH(p_s) + CHAR_LENGTH(p_s) + INSTR(p_s, 'x') + LOCATE('x', p_s) + POSITION('x' IN p_s);
END`),
			code: []string{"v := (p_s || '-' || p_i::text || '-' || p_n::text || '-' || p_d::text || '-' || p_t::text);", "concat_ws(',', p_s, p_i::text, NULL)",
				"upper(p_s) || lower(p_s) || reverse(p_s) || repeat(p_s, 2) || replace(p_s, 'a', 'b')",
				"substr(p_s, 2) || substr(p_s, 1, 3) || substr(p_s, 2, 1) || substr(p_s, 2, 3) || left(p_s, 2) || right(p_s, 3)",
				"btrim(p_s, ' ') || ltrim(p_s, ' ') || rtrim(p_s, ' ') || btrim(p_s, 'x') || ltrim(p_s, '0') || lpad(p_i::text, 5, '0') || rpad(p_s, 5, ' ')",
				`v := 'double "quoted" and ''single'' with a \ backslash';`, `p_s LIKE 'a%' AND p_s NOT LIKE '%\_%' AND p_s LIKE '50!%' ESCAPE '!'`,
				"RETURN octet_length(p_s) + char_length(p_s) + strpos(p_s, 'x') + strpos(p_s, 'x') + strpos(p_s, 'x');"}},
		{name: "dates", opt: store, status: Converted, src: myFn(`BEGIN
  DECLARE d DATE; DECLARE t DATETIME; DECLARE n INT; DECLARE s VARCHAR(40);
  SET t = NOW(); SET t = CURRENT_TIMESTAMP; SET t = SYSDATE(); SET d = CURDATE(); SET d = CURRENT_DATE();
  SET d = DATE_ADD(p_d, INTERVAL 7 DAY); SET d = p_d - INTERVAL p_i MONTH; SET d = DATE_SUB(p_d, INTERVAL 1 YEAR); SET d = ADDDATE(p_d, 3);
  SET t = p_d + INTERVAL 2 HOUR; SET t = DATE_ADD(p_t, INTERVAL 30 MINUTE); SET t = p_t + INTERVAL 1 QUARTER;
  SET n = DATEDIFF(p_d, p_t); SET n = TIMESTAMPDIFF(HOUR, p_t, NOW()); SET n = TIMESTAMPDIFF(DAY, p_d, p_t);
  SET n = YEAR(p_d) + MONTH(p_d) + DAY(p_d) + HOUR(p_t) + MINUTE(p_t) + SECOND(p_t) + QUARTER(p_d) + DAYOFWEEK(p_d) + WEEKDAY(p_d) + DAYOFYEAR(p_d);
  SET n = EXTRACT(YEAR FROM p_d); SET d = LAST_DAY(p_d); SET d = DATE(p_t);
  SET s = DATE_FORMAT(p_t, '%Y-%m-%d %H:%i:%s'); SET s = DATE_FORMAT(p_d, '%d/%m/%y at %T, %W %e %M');
  SET d = DATE '2024-02-29'; SET t = TIMESTAMP '2024-02-29 10:00:00'; SET d = '2024-01-01';
  RETURN n;
END`),
			code: []string{"t := date_trunc('second', statement_timestamp())::timestamp(0);", "t := date_trunc('second', clock_timestamp())::timestamp(0);", "d := statement_timestamp()::date;",
				"d := (p_d + 7 * INTERVAL '1 day')::date;", "d := (p_d - p_i * INTERVAL '1 month')::date;", "d := (p_d - 1 * INTERVAL '1 year')::date;", "d := (p_d + 3 * INTERVAL '1 day')::date;",
				"t := (p_d + 2 * INTERVAL '1 hour');", "t := (p_t + 30 * INTERVAL '1 minute');", "t := (p_t + 1 * INTERVAL '3 months');",
				"n := (p_d - p_t::date);", "trunc(EXTRACT(EPOCH FROM (date_trunc('second', statement_timestamp())::timestamp(0) - p_t)) / 3600)::bigint", "trunc(EXTRACT(EPOCH FROM (p_t - p_d)) / 86400)::bigint",
				"EXTRACT(YEAR FROM p_d)::int + EXTRACT(MONTH FROM p_d)::int", "trunc(EXTRACT(SECOND FROM p_t))::int", "(EXTRACT(DOW FROM p_d)::int + 1)", "(EXTRACT(ISODOW FROM p_d)::int - 1)", "EXTRACT(DOY FROM p_d)::int",
				"d := (date_trunc('month', p_d) + INTERVAL '1 month - 1 day')::date;", "d := p_t::date;", "to_char(p_t, 'YYYY-MM-DD HH24:MI:SS')",
				`to_char(p_d, 'DD/MM/YY" at "HH24:MI:SS, FMDay FMDD FMMonth')`, "d := DATE '2024-02-29';", "t := TIMESTAMP '2024-02-29 10:00:00';", "d := '2024-01-01';"}},
		{name: "numbers", opt: store, status: Converted, src: myFn(`BEGIN
  DECLARE v DECIMAL(10,2); DECLARE w INT;
  SET v = p_i / 4; SET v = ROUND(p_n / 3, 2); SET w = p_i DIV 4; SET w = p_i % 4; SET w = p_i MOD p_i; SET w = MOD(p_i, 3);
  SET v = p_n * 2 - ABS(p_n) + CEILING(p_n) + FLOOR(p_n) + SIGN(p_n) + TRUNCATE(p_n, 1) + ROUND(p_n) + ROUND(p_i, 1) + POW(p_i, 2);
  SET v = CAST(p_i AS DECIMAL(10,2)) + CAST(p_n AS SIGNED) + IFNULL(p_n, 0) + COALESCE(p_n, p_i, 0);
  RETURN v;
END`),
			code: []string{"v := p_i::numeric / 4;", "v := round(p_n / 3, 2);", "w := p_i / 4;", "w := p_i % 4;", "w := p_i % NULLIF(p_i, 0);", "w := (p_i % 3);",
				"abs(p_n) + ceil(p_n) + floor(p_n) + sign(p_n) + trunc(p_n, 1) + round(p_n) + round(p_i::numeric, 1) + power(p_i, 2)", "CAST(p_i AS numeric(10,2)) + CAST(p_n AS bigint) + COALESCE(p_n, 0) + COALESCE(p_n, p_i, 0)"},
			absent: []string{"statement_timestamp"}},
		{name: "queries", schema: "shop", opt: store, status: Converted, src: myProc(`BEGIN
  DECLARE v_n INT; DECLARE v_name VARCHAR(40);
  SELECT COUNT(*), MAX(e.name) INTO v_n, v_name FROM emp e WHERE e.salary > p_n AND e.isactive;
  SELECT name INTO v_name FROM emp WHERE id = p_i;
  SELECT name FROM emp WHERE hired < p_d ORDER BY salary DESC, name LIMIT 1 INTO v_name;
  INSERT INTO ` + "`Log`" + ` (at, msg, n) SELECT NOW(), name, id FROM emp WHERE id IN (SELECT id FROM emp WHERE salary IS NULL) LIMIT 5, 10;
  INSERT INTO shop.Log (msg, n) VALUES (v_name, 1), ('b', 2);
  UPDATE emp e SET e.salary = e.salary * 1.1, isActive = 0 WHERE e.id BETWEEN 1 AND p_i;
  DELETE FROM emp WHERE hired < '2000-01-01' AND NOT EXISTS (SELECT 1 FROM Log l WHERE l.n = emp.id);
  CALL log_it(v_name, bonus(p_i));
END`),
			code: []string{"SELECT COUNT(*), MAX(e.name) INTO v_n, v_name FROM public.emp e WHERE e.salary > p_n AND e.\"isActive\" <> 0;",
				"BEGIN\n    SELECT name INTO STRICT v_name FROM public.emp WHERE id = p_i;\n  EXCEPTION WHEN no_data_found THEN\n    NULL;\n  END;",
				"SELECT name FROM public.emp WHERE hired < p_d ORDER BY salary DESC NULLS LAST, name NULLS FIRST LIMIT 1 INTO STRICT v_name;",
				`INSERT INTO public."Log" (at, msg, n) SELECT date_trunc('second', statement_timestamp())::timestamp(0), name, id FROM public.emp WHERE id IN (SELECT id FROM public.emp WHERE salary IS NULL) LIMIT 10 OFFSET 5;`,
				`INSERT INTO public."Log" (msg, n) VALUES (v_name, 1), ('b', 2);`, `UPDATE public.emp e SET salary = e.salary * 1.1, "isActive" = 0 WHERE e.id BETWEEN 1 AND p_i;`,
				`NOT EXISTS (SELECT 1 FROM public."Log" l WHERE l.n = public.emp.id)`, "CALL public.log_it(v_name, public.bonus(p_i));"}},
		{name: "errors", opt: store, status: Converted, src: myProc(`BEGIN
  DECLARE dup CONDITION FOR 1062;
  DECLARE custom CONDITION FOR SQLSTATE '45123';
  DECLARE v INT DEFAULT 0;
  IF p_n < 0 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'negative', MYSQL_ERRNO = 3000; END IF;
  IF p_n = 0 THEN SIGNAL custom; END IF;
  BEGIN
    DECLARE EXIT HANDLER FOR dup, 1452 SET v = 1;
    DECLARE EXIT HANDLER FOR SQLSTATE '23000' SET v = 2;
    DECLARE EXIT HANDLER FOR custom BEGIN SET v = 3; RESIGNAL; END;
    DECLARE EXIT HANDLER FOR SQLEXCEPTION SET v = 4;
    SET v = 1 DIV p_i;
  END;
END`),
			code: []string{"RAISE EXCEPTION '%', 'negative' USING ERRCODE = '45000';", "RAISE EXCEPTION '%', 'Unhandled user-defined exception condition' USING ERRCODE = '45123';",
				"WHEN unique_violation OR foreign_key_violation THEN", "WHEN integrity_constraint_violation THEN", "WHEN SQLSTATE '45123' THEN\n      v := 3;\n      RAISE;", "WHEN others THEN\n      v := 4;"}},
		{name: "not found handlers", schema: "shop", opt: store, status: Converted, src: myProc(`BEGIN
  DECLARE v_id INT; DECLARE v_name VARCHAR(40) DEFAULT 'none';
  DECLARE c CURSOR FOR SELECT id FROM emp;
  DECLARE EXIT HANDLER FOR NOT FOUND SET p_i = -1;
  SELECT name INTO v_name FROM emp WHERE id = p_i;
  OPEN c;
  WHILE p_i > 0 DO
    FETCH c INTO v_id;
    SET p_i = p_i - v_id;
  END WHILE;
  CLOSE c;
END`),
			code: []string{"<<block1>>", "EXCEPTION WHEN no_data_found THEN\n    p_i := -1;\n    EXIT block1;", "IF NOT FOUND THEN\n      p_i := -1;\n      EXIT block1;\n    END IF;"}},
		{name: "fetch without a handler", schema: "shop", opt: store, status: Review, src: myProc(`BEGIN
  DECLARE v_id INT; DECLARE c CURSOR FOR SELECT id FROM emp;
  OPEN c; FETCH c INTO v_id; CLOSE c; SET p_i = v_id;
END`),
			code: []string{"RAISE EXCEPTION 'No data - zero rows fetched, selected, or processed' USING ERRCODE = '02000';"}, notes: []string{"FETCH leaves its variables"}},
		{name: "single statement bodies", opt: store, status: Converted, src: "CREATE FUNCTION `twice`(`x` INT) RETURNS int(11)\n    NO SQL\n    DETERMINISTIC\nRETURN x * 2",
			code: []string{"CREATE FUNCTION public.twice(x integer) RETURNS integer", "BEGIN\n  RETURN x * 2;\nEND;"}},
		{name: "names", opt: store, status: Converted, src: "CREATE PROCEDURE `Do It`(IN `Limit` INT, IN userName VARCHAR(9))\nBEGIN DECLARE `Offset` INT; SET `offset` = `limit` + CHAR_LENGTH(USERNAME); END",
			code: []string{`CREATE PROCEDURE public."Do It"("limit" integer, username varchar)`, `"offset" integer;`, `"offset" := "limit" + char_length(username);`}},
	})
}

func TestMySQLToPostgresReview(t *testing.T) {
	loose := store
	loose.ExactText = false
	runConversions(t, "mysql", "postgres", []conversion{
		{name: "text compared", opt: loose, status: Review, src: myFn(`RETURN p_s = 'a' OR p_s IN ('b', 'c') OR p_s BETWEEN 'a' AND 'f' OR p_s LIKE 'x%' OR INSTR(p_s, 'q') > 0`),
			notes: []string{"text is compared (=)", "text is compared (IN)", "text is compared (BETWEEN)", "text is compared (LIKE)", "text is compared (INSTR)"}},
		{name: "ordered by text", schema: "shop", opt: loose, status: Review, src: myFn(`BEGIN DECLARE v INT; SELECT id INTO v FROM emp ORDER BY name LIMIT 1; RETURN v; END`), notes: []string{"rows are ordered by text"}},
		{name: "numbers compared need no note", schema: "shop", opt: loose, status: Converted, src: myFn(`BEGIN DECLARE v INT; SELECT COUNT(*) INTO v FROM emp WHERE id = p_i AND hired < p_d; RETURN v > 0; END`)},
		{name: "division digits", opt: store, status: Review, src: "CREATE FUNCTION f(a INT, b INT) RETURNS DECIMAL(20,10) DETERMINISTIC RETURN a / b",
			code: []string{"RETURN CAST(a::numeric / NULLIF(b, 0) AS numeric(20,10));"}, notes: []string{"the division a / b keeps a different number of digits"}},
		{name: "greatest", opt: store, status: Review, src: myFn(`RETURN GREATEST(p_i, 3)`), notes: []string{"GREATEST gives NULL in MySQL when any argument is NULL"}},
		{name: "substring from a variable", opt: store, status: Review, src: myFn(`RETURN CHAR_LENGTH(SUBSTRING(p_s, p_i))`), notes: []string{"SUBSTRING starts at p_i"}},
		{name: "left by a variable", opt: store, status: Review, src: myFn(`RETURN CHAR_LENGTH(LEFT(p_s, p_i))`), notes: []string{"LEFT takes p_i characters"}},
		{name: "float as text", opt: store, status: Review, src: myFn(`BEGIN DECLARE x DOUBLE DEFAULT 1.5; RETURN CHAR_LENGTH(CONCAT('v', x)); END`), notes: []string{"write floating point numbers differently"}},
		{name: "fractional seconds as text", opt: store, status: Review, src: myFn(`BEGIN DECLARE x DATETIME(6); RETURN CHAR_LENGTH(CONCAT('v', x)); END`), notes: []string{"write fractions of a second and time zones differently"}},
		{name: "unknown column as text", opt: store, status: Review, src: myFn(`BEGIN DECLARE v VARCHAR(20); SELECT CONCAT('n', qty) INTO v FROM things LIMIT 1; RETURN 1; END`), notes: []string{"qty is joined to text and its type is not known here"}},
		{name: "text cast to a number", opt: store, status: Review, src: myFn(`RETURN CAST(p_s AS SIGNED)`), code: []string{"CAST(p_s AS bigint)"}, notes: []string{"text is cast to a number"}},
		{name: "arithmetic on text", opt: store, status: Review, src: myFn(`RETURN p_s + 1`), notes: []string{"does arithmetic on text"}},
		{name: "interval on an unknown value", opt: store, status: Review, src: myFn(`BEGIN DECLARE v DATE; SELECT due + INTERVAL 1 DAY INTO v FROM things LIMIT 1; RETURN 1; END`), notes: []string{"an interval is added to due"}},
		{name: "months between", opt: store, status: Review, src: myFn(`RETURN TIMESTAMPDIFF(MONTH, p_d, p_t)`), code: []string{"age(p_t, p_d)"}, notes: []string{"TIMESTAMPDIFF(MONTH ...) is written with age()"}},
		{name: "last insert id", opt: store, status: Review, src: myProc(`SET p_i = LAST_INSERT_ID()`), code: []string{"p_i := lastval();"}, notes: []string{"LAST_INSERT_ID() becomes lastval()"}},
		{name: "handler after a change", schema: "shop", opt: store, status: Review, src: myProc(`BEGIN
  DECLARE EXIT HANDLER FOR SQLEXCEPTION SET p_i = -1;
  UPDATE emp SET salary = 0;
  INSERT INTO emp (id) VALUES (p_i);
END`), notes: []string{"line 3: the block changes data and has an error handler"}},
		{name: "transactions", schema: "shop", opt: store, status: Review, src: myProc(`BEGIN START TRANSACTION; UPDATE emp SET salary = 0; COMMIT; END`),
			code: []string{"UPDATE public.emp SET salary = 0;\n  COMMIT;"}, absent: []string{"START"}, notes: []string{"COMMIT ends the transaction inside the procedure"}},
		{name: "trigger order", schema: "shop", opt: store, status: Review, src: "CREATE TRIGGER t2 BEFORE UPDATE ON emp FOR EACH ROW FOLLOWS t1 SET NEW.salary = 0",
			notes: []string{"the trigger is declared to fire FOLLOWS t1"}},
		{name: "unknown function", opt: store, status: Review, src: myFn(`RETURN helper(p_i)`), code: []string{"RETURN public.helper(p_i);"}, notes: []string{"helper(...) is not a built-in function the converter knows"}},
		{name: "time zone type", opt: store, status: Review, src: myFn(`BEGIN DECLARE t TIMESTAMP; RETURN TIMESTAMPDIFF(DAY, t, p_t); END`), notes: []string{"TIMESTAMPDIFF of a TIMESTAMP column"}},
	})
}

func TestMySQLToPostgresRefused(t *testing.T) {
	cases := []conversion{
		{name: "result set", src: myProc(`SELECT * FROM emp`), notes: []string{"a SELECT whose rows go to the caller"}},
		{name: "session variable", src: myProc(`SET @total = 1`), notes: []string{"the session variable @total"}},
		{name: "session variable read", src: myFn(`RETURN @total + 1`), notes: []string{"the session variable @total"}},
		{name: "prepared statement", src: myProc(`BEGIN PREPARE s FROM 'SELECT 1'; EXECUTE s; END`), notes: []string{"dynamic SQL (PREPARE)"}},
		{name: "continue handler for errors", src: myProc(`BEGIN DECLARE CONTINUE HANDLER FOR SQLEXCEPTION SET p_i = 1; SET p_i = 2; END`), notes: []string{"a CONTINUE handler for errors"}},
		{name: "warning handler", src: myProc(`BEGIN DECLARE EXIT HANDLER FOR SQLWARNING SET p_i = 1; SET p_i = 2; END`), notes: []string{"a handler for warnings"}},
		{name: "rollback in a handler", src: myProc(`BEGIN DECLARE EXIT HANDLER FOR SQLEXCEPTION BEGIN ROLLBACK; RESIGNAL; END; START TRANSACTION; UPDATE emp SET id = 1; COMMIT; END`),
			notes: []string{"ROLLBACK inside a block with an error handler", "COMMIT inside a block with an error handler"}},
		{name: "on duplicate key", src: myProc(`INSERT INTO emp (id) VALUES (1) ON DUPLICATE KEY UPDATE id = 2`), notes: []string{"INSERT ... ON DUPLICATE"}},
		{name: "insert ignore", src: myProc(`INSERT IGNORE INTO emp (id) VALUES (1)`), notes: []string{"INSERT IGNORE"}},
		{name: "replace", src: myProc(`REPLACE INTO emp (id) VALUES (1)`), notes: []string{"REPLACE INTO"}},
		{name: "multi-table update", src: myProc(`UPDATE emp e JOIN dept d ON d.id = e.dept SET e.salary = 0`), notes: []string{"an UPDATE of more than one table"}},
		{name: "update with limit", src: myProc(`UPDATE emp SET salary = 0 ORDER BY id LIMIT 3`), notes: []string{"an UPDATE with ORDER"}},
		{name: "multi-table delete", src: myProc(`DELETE e FROM emp e JOIN dept d ON d.id = e.dept`), notes: []string{"a DELETE from more than one table"}},
		{name: "get diagnostics", src: myProc(`BEGIN GET DIAGNOSTICS p_i = ROW_COUNT; END`), notes: []string{"GET DIAGNOSTICS"}},
		{name: "row count", src: myProc(`SET p_i = ROW_COUNT()`), notes: []string{"the built-in function ROW_COUNT has no translation"}},
		{name: "group_concat", src: myFn(`BEGIN DECLARE v TEXT; SELECT GROUP_CONCAT(name) INTO v FROM emp; RETURN 1; END`), notes: []string{"GROUP_CONCAT"}},
		{name: "regexp", src: myFn(`RETURN p_s REGEXP '^a'`), notes: []string{"the REGEXP operator"}},
		{name: "binary operator", src: myFn(`RETURN BINARY p_s = 'a'`), notes: []string{"the BINARY operator"}},
		{name: "resignal with changes", src: myProc(`BEGIN DECLARE EXIT HANDLER FOR SQLEXCEPTION RESIGNAL SET MESSAGE_TEXT = 'x'; SET p_i = 1; END`), notes: []string{"RESIGNAL that changes the error"}},
		{name: "signal of a warning", src: myProc(`SIGNAL SQLSTATE '01000' SET MESSAGE_TEXT = 'careful'`), notes: []string{"SIGNAL of a warning"}},
		{name: "unknown error number", src: myProc(`BEGIN DECLARE EXIT HANDLER FOR 1205 SET p_i = 1; SET p_i = 2; END`), notes: []string{"a handler for the MySQL error 1205"}},
		{name: "date as a number", src: myFn(`RETURN p_d + 1`), notes: []string{"does arithmetic on a date as if it were a number"}},
		{name: "odd date format", src: myFn(`RETURN CHAR_LENGTH(DATE_FORMAT(p_d, '%U'))`), notes: []string{"the date format element %U"}},
		{name: "temporary table", src: myProc(`CREATE TEMPORARY TABLE t (a INT)`), notes: []string{"the statement CREATE"}},
		{name: "version comment", src: "CREATE PROCEDURE p() BEGIN /*!50003 SET @a = 1 */; END", notes: []string{"a version-specific comment"}},
		{name: "bitwise operators", src: myFn(`RETURN p_i & 3`), notes: []string{"the & operator"}},
		{name: "enum type", src: myProc(`BEGIN DECLARE e ENUM('a','b'); SET e = 'a'; END`), code: nil, status: Converted},
		{name: "spatial type", src: myProc(`BEGIN DECLARE g GEOMETRY; END`), notes: []string{"the type GEOMETRY of the variable g has no translation"}},
		{name: "not a routine", src: `CREATE TABLE t (a INT)`, notes: []string{"expected FUNCTION, PROCEDURE or TRIGGER"}},
	}
	for i := range cases {
		if cases[i].status == "" {
			cases[i].status = Unconverted
		}
		cases[i].opt = store
	}
	runConversions(t, "mysql", "postgres", cases)
}
