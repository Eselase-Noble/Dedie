package mymy

import (
	"context"
	"errors"
	"strings"

	driver "github.com/go-sql-driver/mysql"

	"dedie/engine/migrate/pgpg"
	"dedie/engine/migrate/storedcode"
)

// The functions here serve the pipelines that create converted functions,
// procedures and triggers in MySQL; see package storedcode. This pipeline
// copies such definitions as they are.

// CodeObjects lists the names the stored code of a plan takes in the
// target database, for the preflight to look up there.
func CodeObjects(objects []*storedcode.Object) []Object {
	var out []Object
	for _, o := range storedcode.Planned(objects) {
		for _, c := range o.Creates {
			out = append(out, Object{Kind: c.Kind, Name: c.Name})
		}
	}
	return out
}

// KeepCode takes out of a plan the stored code whose names the target
// holds and the job was told to leave as they are; kept is the list a job
// keeps in its state. The server may have folded a name to lower case.
func KeepCode(objects []*storedcode.Object, kept []string) {
	if len(kept) == 0 {
		return
	}
	is := map[string]bool{}
	for _, k := range kept {
		is[strings.ToLower(k)] = true
	}
	storedcode.Keep(objects, func(c storedcode.Created) bool {
		return is[strings.ToLower(pgpg.KeptKey(c.Kind, c.Name))]
	})
}

// BinlogRefusal explains MySQL's error 1418 to the administrator, who
// can do something about it; the server's own message assumes a reader
// who knows why the binary log cares.
const BinlogRefusal = "the target writes a binary log and its setting log_bin_trust_function_creators is off, and MySQL then refuses a function that " +
	"is not declared DETERMINISTIC, NO SQL or READS SQL DATA, because it could not be replayed safely on a replica. " +
	"Set log_bin_trust_function_creators = 1 on the target and create the function from the converted draft"

// CodeExec returns what runs one statement of stored code on the target
// and tells an object that is already there from one that was refused.
func CodeExec(db Querier) func(ctx context.Context, sql string) (bool, error) {
	return func(ctx context.Context, sql string) (bool, error) {
		_, err := db.ExecContext(ctx, sql)
		if err == nil {
			return false, nil
		}
		var myErr *driver.MySQLError
		if errors.As(err, &myErr) {
			switch myErr.Number {
			case 1304, 1359: // the routine, the trigger already exists
				return true, nil
			case 1418:
				return false, errors.New(BinlogRefusal)
			}
		}
		return false, errors.New(message(err))
	}
}
