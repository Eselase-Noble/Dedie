package mongopg

import (
	"context"
	"fmt"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/connector/mongodb"
	"dedie/engine/connector/postgres"
	"dedie/engine/migrate"
)

// Preflight inspects both databases without changing either.
func Preflight(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	rep := &migrate.Report{}
	defer rep.Finish()

	client, err := mongodb.Connect(ctx, srcCfg)
	if err != nil {
		rep.Add("Source connection", "fail", err.Error())
		return rep, nil
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	version, _ := mongodb.Version(ctx, client)
	rep.Add("Source connection", "ok", "MongoDB "+version)
	if mongodb.VersionNum(version) < 40000 {
		rep.Add("Source version", "fail", "change streams over a whole database need MongoDB 4.0 or newer")
	}
	hello, err := mongodb.ServerHello(ctx, client)
	switch {
	case err != nil:
		rep.Add("Replica set", "fail", err.Error())
	case hello.SetName == "":
		rep.Add("Replica set", "fail", "the source is a standalone server. Changes made during the copy can only be followed on a replica set; a single node can be one (start mongod with --replSet and run rs.initiate())")
	default:
		rep.Add("Replica set", "ok", "member of "+hello.SetName)
	}

	list, err := collections(ctx, client.Database(srcCfg.Database), newScope(cfg))
	switch {
	case err != nil:
		rep.Add("Collections", "fail", err.Error())
	case len(list) == 0:
		rep.Add("Collections", "fail", "there are no collections to migrate in "+srcCfg.Database)
	default:
		rep.Tables = len(list)
		var docs int64
		for _, c := range list {
			docs += c.EstDocs
		}
		rep.Add("Collections", "ok", fmt.Sprintf("%d collection(s), about %d documents", len(list), docs))
		if err := checkNames(list); err != nil {
			rep.Add("Collection names", "fail", err.Error())
		}
	}
	if hello.SetName != "" {
		// Opening the stream proves the account may read it.
		cs, err := client.Database(srcCfg.Database).Watch(ctx, []any{})
		if err != nil {
			rep.Add("Change stream", "fail", "the account cannot open a change stream on "+srcCfg.Database+": "+mongodb.Friendly(err).Error())
		} else {
			cs.Close(context.WithoutCancel(ctx))
			rep.Add("Change stream", "ok", "readable")
		}
	}

	dst, err := postgres.Connect(ctx, dstCfg)
	if err != nil {
		rep.Add("Target connection", "fail", err.Error())
		return rep, nil
	}
	defer dst.Close(context.WithoutCancel(ctx))
	var pgVersion string
	_ = dst.QueryRow(ctx, "SHOW server_version").Scan(&pgVersion)
	rep.Add("Target connection", "ok", "PostgreSQL "+pgVersion)
	var canCreate bool
	if err := dst.QueryRow(ctx, `SELECT has_schema_privilege(current_user, $1, 'CREATE') AND has_database_privilege(current_user, current_database(), 'CREATE')`, targetSchema).Scan(&canCreate); err != nil || !canCreate {
		rep.Add("Target privileges", "fail", "the target account must be able to create tables in schema "+targetSchema+" and a schema named "+metaSchema+" for the checkpoint")
	} else {
		rep.Add("Target privileges", "ok", "may create tables")
	}
	var existing []string
	for _, c := range list {
		var n int64
		if dst.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname=$1 AND tablename=$2`, targetSchema, c.Name).Scan(&n) == nil && n > 0 {
			existing = append(existing, c.Name)
		}
	}
	if len(existing) > 0 {
		rep.Add("Target tables", "warn", "these tables already exist in the target and will be emptied by the copy: "+strings.Join(existing, ", "))
	} else {
		rep.Add("Target tables", "ok", "no table names are taken")
	}
	rep.Add("Mapping", "ok", "each collection becomes a table with _id (text, primary key) and doc (jsonb). Indexes, validators and views are not translated")
	if cfg.FenceSource {
		rep.Add("Cutover", "warn", "a MongoDB source cannot be made read-only by this tool; stop the applications that write to it before cutting over")
	}
	return rep, nil
}
