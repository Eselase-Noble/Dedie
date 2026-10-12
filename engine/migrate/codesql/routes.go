package codesql

// Assumptions lists what the converter takes for granted on a route:
// the differences between two databases that no single routine shows
// and that are therefore not repeated in every routine's notes. A
// pipeline shows them once, next to the routines it converted.
func Assumptions(from, to string) []string {
	f, t := dialect(from), dialect(to)
	var out []string
	switch {
	case f == Oracle && t == Postgres:
		out = []string{
			"No text passed to a routine or stored in a table is the empty string. Oracle has none: it stores NULL. PostgreSQL keeps '' apart from NULL, and a converted routine that is handed one may treat it differently from NULL.",
			"The current date and time are taken from the PostgreSQL session's time zone; Oracle's SYSDATE is in the time zone of the database server.",
			"Text is compared and sorted by the collation of the PostgreSQL database; Oracle compares byte by byte unless NLS_SORT says otherwise.",
			"A routine runs with the rights of its caller, PostgreSQL's default; Oracle's default is the rights of its owner.",
			"Error messages reach the client without the ORA- number in front; RAISE_APPLICATION_ERROR(-20nnn, ...) raises SQLSTATE 45nnn.",
			"The names of months and days in dates written as text are English, as they are with Oracle's default language.",
			"A function called from an SQL statement that fails with NO_DATA_FOUND makes the statement fail; Oracle would continue with NULL.",
			"Where Oracle converts between text and numbers on its own, in a comparison or an assignment, PostgreSQL is left to do the same; where it will not, the routine fails with a type error when it runs.",
			"PostgreSQL 14 or later: procedures with OUT parameters, and trim_scale for numbers written as text.",
			"Comments are not carried over.",
		}
	case f == MySQL && t == Postgres:
		out = []string{
			"The MySQL server runs in a strict SQL mode, so a value that does not fit is an error and not a silently changed value, and without PIPES_AS_CONCAT, so || means OR.",
			"The current date and time are the time the calling statement began, in the PostgreSQL session's time zone.",
			"MySQL does not fire triggers for rows changed by a cascading foreign key; PostgreSQL does.",
			"A routine runs with the rights of its caller, PostgreSQL's default; MySQL's default is the rights of its definer.",
			"Text longer than a VARCHAR parameter or result declares is passed on as it is; MySQL would reject it.",
			"Where MySQL converts between text, numbers and dates on its own, as in COALESCE over values of different types, PostgreSQL is left to do the same; where it will not, the routine fails with a type error when it is created or run.",
			"PostgreSQL 14 or later: procedures with OUT parameters.",
			"Comments are not carried over.",
		}
	case f == Oracle && t == MySQL:
		out = []string{
			"No text passed to a routine or stored in a table is the empty string. Oracle has none: it stores NULL. MySQL keeps '' apart from NULL, and a converted routine that is handed one may treat it differently from NULL.",
			"The tables on MySQL compare text byte by byte, as the migration creates them; the variables of a converted routine are declared with the same collation (Options.Collation).",
			"NUMBER without a precision becomes DECIMAL(65,30), as for columns: 35 digits before the decimal point and 30 after it.",
			"The current date and time are the time the calling statement began, in the MySQL session's time zone; Oracle's SYSDATE is the clock of the database server.",
			"The MySQL server runs in a strict SQL mode, so a value that does not fit is an error.",
			"Error messages reach the client without the ORA- number in front and are at most 128 characters long; RAISE_APPLICATION_ERROR(-20nnn, ...) raises SQLSTATE 45nnn.",
			"A function called from an SQL statement that fails with NO_DATA_FOUND makes the statement fail; Oracle would continue with NULL.",
			"MySQL fires the triggers of a table in the order they were created. Create them in the order Oracle fires them, where that matters.",
			"Comments are not carried over.",
		}
	case f == Postgres && t == MySQL:
		out = []string{
			"The tables on MySQL compare text byte by byte, as the migration creates them; the variables of a converted routine are declared with the same collation. With utf8mb4_bin, which older servers need, trailing spaces are ignored in comparisons; utf8mb4_0900_bin counts them as PostgreSQL does.",
			"numeric without a precision becomes DECIMAL(65,20), as for columns: 45 digits before the decimal point and 20 after it.",
			"The MySQL server runs in a strict SQL mode, so a value that does not fit is an error.",
			"MySQL has no schemas inside a database: routines and tables from every schema share one namespace, under the names the migration gave the tables.",
			"The message of an error raised by a routine is at most 128 characters long in MySQL.",
			"A routine runs with the rights of the account that creates it (SQL SECURITY DEFINER), MySQL's default; PostgreSQL's default is the rights of the caller.",
			"MySQL fires the triggers of a table in the order they were created; PostgreSQL in the order of their names. Create them in that order.",
			"Comments are not carried over.",
		}
	}
	return out
}
