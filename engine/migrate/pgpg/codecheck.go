package pgpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dedie/engine/migrate/storedcode"
)

// The functions here serve the pipelines that create converted functions,
// procedures and triggers in PostgreSQL; see package storedcode. This
// pipeline copies such definitions as they are.

// codeClaim is the name one created object takes on the target. A
// function or procedure is looked for by its name alone: the databases
// the code comes from have one routine to a name.
func codeClaim(c storedcode.Created) Claim {
	if c.Kind == "trigger" {
		return Claim{Kind: "trigger", Name: c.Name, Ident: c.Table + "." + c.Quoted}
	}
	return Claim{Kind: "routine", Name: c.Name, Ident: c.Quoted}
}

// CodeClaims lists the names the stored code of a plan takes on the
// target, for the preflight to look up there.
func CodeClaims(objects []*storedcode.Object) []Claim {
	var out []Claim
	for _, o := range storedcode.Planned(objects) {
		for _, c := range o.Creates {
			out = append(out, codeClaim(c))
		}
	}
	return out
}

// KeepCode takes out of a plan the stored code whose names the target
// holds and the job was told to leave as they are; kept is the list a job
// keeps in its state.
func KeepCode(objects []*storedcode.Object, kept []string) {
	if len(kept) == 0 {
		return
	}
	is := map[string]bool{}
	for _, k := range kept {
		is[k] = true
	}
	storedcode.Keep(objects, func(c storedcode.Created) bool {
		claim := codeClaim(c)
		return is[KeptKey(claim.Kind, claim.Name)]
	})
}

// CodeExec returns what runs one statement of stored code on the target
// and tells an object that is already there from one that was refused.
// conn gives the connection to use, which a pipeline may have replaced
// since.
func CodeExec(conn func(ctx context.Context) (*pgx.Conn, error)) func(ctx context.Context, sql string) (bool, error) {
	return func(ctx context.Context, sql string) (bool, error) {
		c, err := conn(ctx)
		if err != nil {
			return false, err
		}
		_, err = c.PgConn().Exec(ctx, sql).ReadAll()
		if err == nil {
			return false, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42723" || pgErr.Code == "42710") {
			// duplicate_function, and duplicate_object for a trigger.
			return true, nil
		}
		return false, errors.New(pgMessage(err))
	}
}
