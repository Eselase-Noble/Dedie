// Package codesql converts stored functions, procedures and triggers
// from one database's procedural language to another's: Oracle's PL/SQL
// and MySQL's stored programs to PostgreSQL's PL/pgSQL. It is a library
// without side effects: it is
// given the definition of a routine as the source database prints it and
// returns the statements that create the same routine on the target.
//
// # What a result means
//
// The converter is best effort and says so for each routine. A result
// has one of three statuses:
//
//   - Converted: every construct in the routine was recognised and has a
//     rewrite that keeps its behaviour.
//   - Review: the routine was rewritten, and at least one rewrite can
//     behave differently from the original. Each such place has a note
//     that names the line and says what to check.
//   - Unconverted: the routine uses something that has no rewrite. No
//     code is returned, only the reasons.
//
// The status is decided per construct, by the tables further down. The
// rule behind them is that a wrong guess must never look like a
// success: where the meaning of a piece of code depends on something
// the converter cannot see, such as the type of a column it was not
// told about, it writes the likeliest translation and marks the routine
// for review.
//
// # How it works
//
// The text is split into tokens by a lexer that knows each dialect's
// strings, quoted names and comments, so that nothing inside them is
// ever taken for code. A parser reads the tokens into a small syntax
// tree: the header with its parameters, the declarations, and the
// statements with their blocks, branches, loops, cursors and handlers.
// SQL statements and expressions inside are parsed too, as far as the
// rewriting needs: expressions completely, the clauses around them as
// the sequence of keywords, tables and expressions they are.
//
// An emitter then writes the tree in the target language. While doing
// so it keeps track of the type of every variable, parameter and, where
// the pipeline described the tables, column. Many rewrites depend on it:
// "d + 1" adds a day when d is a date and a number otherwise, and only
// one of the two is spelled the same on the target.
//
// # What the caller supplies
//
// Options carries what a migration pipeline knows and the routine's
// text does not say: the tables with their columns, their types and
// their names on the target, and the other routines being migrated.
// All of it is optional, and the more of it is given, the more routines
// come out Converted rather than Review. Types are translated exactly
// as the pipelines translate the types of columns, so a variable
// declared like a column gets the type that column got.
//
// # Assumptions
//
// Some differences between the databases lie outside any one routine.
// The converter assumes the following and does not repeat it in the
// notes of every routine; Assumptions returns the list for a route so
// that a pipeline can show it once.
//
// # Oracle to PostgreSQL
//
// Converted:
//
//   - Functions and procedures with IN parameters and defaults at the
//     end of the list; procedures with OUT and IN OUT parameters.
//   - Row and statement triggers on INSERT, UPDATE [OF columns] and
//     DELETE, before or after, with WHEN, REFERENCING, :NEW and :OLD,
//     INSERTING, UPDATING and DELETING. The body becomes a trigger
//     function, which returns the row for a BEFORE row trigger.
//   - Declarations of variables and constants of the built-in types,
//     column%TYPE, cursors with and without parameters, exceptions and
//     PRAGMA EXCEPTION_INIT for the error numbers -20000 to -20999 and
//     for the common constraint errors.
//   - Blocks, IF, CASE, LOOP, WHILE, FOR over a range (also REVERSE),
//     over a query and over a cursor, EXIT and CONTINUE with labels and
//     WHEN, RETURN, NULL.
//   - OPEN, FETCH and CLOSE; cursor%FOUND and %NOTFOUND right after the
//     FETCH they are about; SQL%ROWCOUNT, SQL%FOUND and SQL%NOTFOUND.
//   - SELECT ... INTO, which becomes INTO STRICT and so raises the same
//     errors; INSERT, UPDATE and DELETE, INSERT ... RETURNING INTO;
//     joins, subqueries, set operations (MINUS becomes EXCEPT), FETCH
//     FIRST; "FROM dual" is dropped; sequence.NEXTVAL and CURRVAL.
//   - Exception handlers for NO_DATA_FOUND, TOO_MANY_ROWS,
//     DUP_VAL_ON_INDEX, ZERO_DIVIDE, CASE_NOT_FOUND, OTHERS and declared
//     exceptions, RAISE, RAISE_APPLICATION_ERROR (the error -20nnn
//     becomes SQLSTATE 45nnn) and DBMS_OUTPUT.PUT_LINE (RAISE NOTICE).
//   - The empty string, which is NULL in Oracle: ” is written NULL, and
//     the functions that can produce an empty string are wrapped in
//     NULLIF(..., ”). "||" becomes concat(), which skips NULLs as
//     Oracle does.
//   - NVL, NVL2, DECODE, COALESCE, NULLIF, UPPER, LOWER, INITCAP,
//     LENGTH, SUBSTR from a fixed position, INSTR with two arguments,
//     TRIM, LTRIM, RTRIM, LPAD, RPAD, REPLACE, CHR, ASCII, TO_CHAR of a
//     number, and of a date with a format of common elements, TO_NUMBER,
//     TO_DATE with a format, TRUNC and ROUND, ABS, CEIL, FLOOR, SIGN,
//     POWER, SQRT, MOD by a constant, LAST_DAY, EXTRACT, COUNT, SUM,
//     MIN, MAX, LISTAGG.
//   - Date arithmetic where the types are known: a date plus a number of
//     days, a date minus a date inside ROUND or TRUNC, SYSDATE and
//     SYSTIMESTAMP.
//   - Division: the divisor is cast so that two integers are not divided
//     as integers. It counts as converted where the result is rounded,
//     by ROUND or TRUNC or by the variable it is stored in.
//
// Review, with what the note asks to check:
//
//   - A division or AVG whose result is not rounded: PostgreSQL carries
//     fewer digits than Oracle's 38.
//   - Arithmetic, TRUNC, ROUND or TO_CHAR on a value whose type is not
//     known (a column of a table the pipeline did not describe, the
//     result of an unknown function): it may be a date.
//   - A date turned into text without a format, or compared with text:
//     Oracle uses the session's NLS_DATE_FORMAT.
//   - A binary floating point number turned into text.
//   - SUBSTR from a position that is computed or negative; MOD by a
//     variable (Oracle gives the dividend for a divisor of 0);
//     GREATEST and LEAST (NULL handling); ADD_MONTHS (end of month);
//     date formats with elements PostgreSQL may read differently.
//   - A block that changes data and has a handler that can run after
//     the change: PostgreSQL undoes the whole block, Oracle only the
//     statement that failed.
//   - SQLERRM, USER, VALUE_ERROR and INVALID_NUMBER: the texts and the
//     classes of errors differ.
//   - A query in a function that may find no row without a handler:
//     Oracle turns NO_DATA_FOUND into a NULL result for the calling SQL
//     statement.
//   - A FETCH whose variables are read after the cursor is exhausted;
//     cursor%FOUND away from its FETCH.
//   - UPDATE or DELETE ... RETURNING INTO (no row changed), UPDATING
//     ('column'), COMMIT and ROLLBACK in a procedure, TIMESTAMP WITH
//     TIME ZONE, ROWID variables, a call to a routine the pipeline did
//     not name.
//
// Unconverted:
//
//   - Packages and anything that refers to one; object types; routines
//     in another language.
//   - PRAGMA AUTONOMOUS_TRANSACTION, EXECUTE IMMEDIATE and DBMS_SQL,
//     savepoints, GOTO, COMMIT in a function or under a handler.
//   - CONNECT BY, the (+) outer join, ROWNUM, ROWID and LEVEL, MERGE,
//     multi-table INSERT, database links, analytic window frames.
//   - Collections and records: TYPE declarations, %ROWTYPE variables,
//     BULK COLLECT, FORALL, pipelined functions, cursor variables.
//   - Functions with OUT parameters, defaults before parameters without
//     one, routines declared inside the routine.
//   - INSTEAD OF, compound, DDL and system triggers, triggers that are
//     disabled, that CALL a procedure or have FOLLOWS or PRECEDES.
//   - Calls into DBMS_, UTL_ and the other supplied packages, and the
//     built-in functions without a rewrite (MONTHS_BETWEEN, NEXT_DAY,
//     the REGEXP_ family, SYS_GUID, TO_DATE without a format, SQLCODE
//     and others).
//
// # MySQL to PostgreSQL
//
// Converted:
//
//   - Functions, procedures with IN, OUT and INOUT parameters, and
//     triggers (MySQL has row triggers on one event only). An argument
//     and a result are rounded to the scale their DECIMAL declares, as
//     MySQL does.
//   - DECLARE of variables, cursors, conditions and handlers; SET; IF,
//     CASE, LOOP, WHILE, REPEAT, LEAVE and ITERATE, also of a labelled
//     block; RETURN; OPEN, FETCH, CLOSE; CALL.
//   - Truth values. MySQL has none: a comparison is 1 or 0. A comparison
//     stored in a variable is cast to a number, and a number used as a
//     condition is compared with 0.
//   - Handlers. NOT FOUND handlers are written out after each FETCH and
//     around each SELECT ... INTO that can find nothing, which also
//     keeps the variables as they were, as MySQL does. EXIT handlers
//     for SQLEXCEPTION, for an SQLSTATE and for the common error numbers
//     (1062, 1048, 1172, 1451, 1452) become EXCEPTION clauses. SIGNAL
//     and RESIGNAL become RAISE with the same SQLSTATE.
//   - SELECT ... INTO, INSERT, UPDATE and DELETE of one table, joins,
//     subqueries, LIMIT with an offset; ORDER BY keeps MySQL's place for
//     NULLs; backticks and double-quoted strings.
//   - Division: the dividend is cast so that integers are not divided as
//     integers, and a divisor of 0 gives NULL as it does in MySQL. DIV
//     and MOD.
//   - IFNULL, IF, COALESCE, NULLIF, CONCAT, CONCAT_WS, UPPER, LOWER,
//     LENGTH (bytes), CHAR_LENGTH, SUBSTRING, LEFT and RIGHT with fixed
//     positions, TRIM, REPLACE, LPAD, RPAD, REPEAT, REVERSE, INSTR,
//     LOCATE, ROUND, TRUNCATE, FLOOR, CEILING, ABS, SIGN, POW, NOW,
//     CURDATE, DATE_ADD, DATE_SUB and INTERVAL arithmetic, DATEDIFF,
//     TIMESTAMPDIFF in seconds to weeks, DATE_FORMAT, YEAR and its
//     relatives, LAST_DAY, DATE, EXTRACT, CAST, COUNT, SUM, MIN, MAX.
//
// Review:
//
//   - Any comparison, LIKE, IN, BETWEEN, search or ordering of text,
//     unless Options.ExactText says the source compares text exactly:
//     MySQL's default collations ignore case and trailing spaces.
//   - A division or AVG whose result is not rounded to four decimal
//     places or fewer.
//   - A block that changes data and has an error handler: PostgreSQL
//     undoes the block.
//   - SUBSTRING, LEFT and RIGHT with computed positions; GREATEST and
//     LEAST; text cast to a number or used in arithmetic; floating point
//     numbers and times with fractional seconds written as text;
//     TIMESTAMPDIFF in months and years; ROUND of a floating point
//     number; LAST_INSERT_ID.
//   - START TRANSACTION, COMMIT and ROLLBACK in a procedure; FOLLOWS and
//     PRECEDES on a trigger; a FETCH whose variables are read after the
//     cursor is exhausted; a call to a routine the pipeline did not
//     name; values of unknown type where the type decides the result.
//
// Unconverted:
//
//   - A SELECT whose rows go to the caller; session variables (@name);
//     PREPARE and EXECUTE; GET DIAGNOSTICS and ROW_COUNT(); savepoints.
//   - CONTINUE handlers for errors, handlers for SQLWARNING, RESIGNAL
//     that changes the error, COMMIT or ROLLBACK inside a block with a
//     handler or in a function.
//   - INSERT ... ON DUPLICATE KEY UPDATE, INSERT IGNORE, REPLACE,
//     UPDATE and DELETE of several tables or with ORDER BY and LIMIT.
//   - GROUP_CONCAT, regular expressions, bit operators, the BINARY
//     operator, arithmetic on a date as if it were a number, date
//     formats without a PostgreSQL counterpart, spatial types, DDL
//     inside a routine, version-specific comments that hold code.
package codesql
