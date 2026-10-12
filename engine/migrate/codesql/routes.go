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
			"A function called from an SQL statement that fails with NO_DATA_FOUND makes the statement fail; Oracle would continue with NULL.",
			"PostgreSQL 14 or later: procedures with OUT parameters, and trim_scale for numbers written as text.",
			"Comments are not carried over.",
		}
	}
	return out
}
