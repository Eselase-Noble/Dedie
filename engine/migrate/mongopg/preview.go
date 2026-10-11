package mongopg

import (
	"context"
	"fmt"

	"dedie/engine/connector"
	"dedie/engine/connector/mongodb"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

// Preview returns the statements a job with these options would run on the
// target, without running any: one CREATE TABLE for each collection now in
// scope, with the columns its fields are lifted into. It lists the
// collections and looks on the target for tables of their names exactly as
// the schema phase does, and writes each table with the function the job
// creates it with.
//
// Tables are all a collection becomes, so every statement belongs to the
// schema phase. Left out is what a job runs for its own bookkeeping: the
// checkpoint table, emptying the tables before a copy is repeated, and
// ANALYZE. So are the ADD COLUMN IF NOT EXISTS statements sent after each
// CREATE TABLE: they do something only when a job is run again with more
// lifted fields than its tables were created with, and nothing on a table
// that is new. A collection created while the job follows changes gets its
// table then.
func Preview(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) ([]migrate.Statement, error) {
	cfg.Defaults()
	r := &runner{cfg: cfg, st: &State{}, scope: newScope(cfg)}
	var err error
	if r.columns, err = allColumns(cfg); err != nil {
		return nil, err
	}
	if r.client, err = mongodb.Connect(ctx, srcCfg); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer r.client.Disconnect(context.WithoutCancel(ctx))
	r.db = r.client.Database(srcCfg.Database)
	if r.dst, err = postgres.Connect(ctx, dstCfg); err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer func() { r.dst.Close(context.WithoutCancel(ctx)) }()
	if r.list, err = r.collections(ctx); err != nil {
		return nil, err
	}
	if err := checkNames(r.list); err != nil {
		return nil, err
	}

	found, err := pgpg.FindExisting(ctx, r.dst, claims(r.list))
	if err != nil {
		return nil, err
	}
	r.st.SkippedTables, _, r.st.DropFirst = pgpg.Decide(cfg.Existing(), found)
	if len(r.st.SkippedTables) > 0 {
		if r.list, err = r.collections(ctx); err != nil {
			return nil, err
		}
		if len(r.list) == 0 {
			return nil, errNothingLeft
		}
	}

	out := pgpg.DropStatements(found, r.st.DropFirst)
	for _, c := range r.list {
		out = append(out, migrate.Statement{Phase: migrate.PhaseSchema, Kind: "table", Name: c.Name, SQL: createTable(c.Name, r.columns[c.Name])})
	}
	return out, nil
}
