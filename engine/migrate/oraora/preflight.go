package oraora

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/orasrc"
)

// origin names a source database, to tell its table definitions from
// another's in the cache.
func origin(cfg connector.Config) string {
	return fmt.Sprintf("%s:%d/%s", strings.ToLower(cfg.Host), cfg.Port, strings.ToUpper(cfg.Database))
}

// Preflight inspects both databases without changing either and reports
// everything that would stop or weaken the migration: where each schema
// goes, what the target already holds, what arrives differing from the
// source and what is not carried over.
func Preflight(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	src, err := oracle.Open(ctx, srcCfg)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := oracle.Open(ctx, dstCfg)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()
	plan, err := BuildPlan(ctx, src, dst, origin(srcCfg), cfg, kept{}, nil)
	if err != nil {
		return nil, err
	}
	report, _, err := preflight(ctx, src, dst, plan, cfg)
	return report, err
}

// charset reads a database's character sets: the one for ordinary text
// and the national one.
func charset(ctx context.Context, db *sql.DB) (text, national string) {
	_ = db.QueryRowContext(ctx, `SELECT value FROM nls_database_parameters WHERE parameter = 'NLS_CHARACTERSET'`).Scan(&text)
	_ = db.QueryRowContext(ctx, `SELECT value FROM nls_database_parameters WHERE parameter = 'NLS_NCHAR_CHARACTERSET'`).Scan(&national)
	return text, national
}

// preflight runs the checks for a plan. It also returns what it found on
// the target under the names of objects to be migrated, which the schema
// phase acts on.
func preflight(ctx context.Context, src, dst *sql.DB, plan *Plan, cfg migrate.Config) (*migrate.Report, []clash, error) {
	r := &migrate.Report{Tables: len(plan.Tables)}
	_, srcVer, err := oracle.Version(ctx, src)
	if err != nil {
		return nil, nil, fmt.Errorf("source: %w", err)
	}
	_, dstVer, err := oracle.Version(ctx, dst)
	if err != nil {
		return nil, nil, fmt.Errorf("target: %w", err)
	}
	// A newer release accepts the definitions an older one writes; the
	// reverse does not hold.
	switch from, to := release(srcVer), release(dstVer); {
	case to < from:
		r.Add("Versions", "fail", fmt.Sprintf("the target (Oracle %s) is an older release than the source (Oracle %s). Definitions written by a newer release are not always accepted by an older one; migrating to an older release is not supported", dstVer, srcVer))
	case to == from:
		r.Add("Versions", "warn", fmt.Sprintf("Oracle %s → Oracle %s: both are the same release, so this is a move, not an upgrade", srcVer, dstVer))
	default:
		r.Add("Versions", "ok", "Oracle "+srcVer+" → Oracle "+dstVer)
	}
	orasrc.CheckCatalog(plan.Catalog, srcVer, r)

	if len(plan.MapProblems) > 0 {
		// With nowhere to put a schema there is nothing further to check.
		r.Add("Schema mapping", "fail", strings.Join(plan.MapProblems, "\n"))
		r.Finish()
		return r, nil, nil
	}
	r.Add("Schema mapping", "ok", strings.Join(plan.MapNotes, "\n"))

	orasrc.CheckConsistentRead(ctx, src, plan.Catalog, r)
	orasrc.CheckChanges(ctx, src, plan.Catalog, cfg, r)

	if !cfg.Offline() {
		// LogMiner reports a change to a row of such a table as one it
		// cannot describe, so the row cannot be found again.
		var unmined []string
		for _, t := range plan.Tables {
			for _, c := range t.Cols {
				if c.Kind == orasrc.KindJSON || c.Kind == orasrc.KindBool {
					unmined = append(unmined, t.Label())
					break
				}
			}
		}
		if len(unmined) > 0 {
			r.Add("Tables LogMiner cannot follow", "warn", "these tables have a JSON or BOOLEAN column. They are copied, but LogMiner does not describe changes to such tables, so changes made to them after the copy are reported and not carried over; stop writes to them before the job starts, or migrate offline: "+strings.Join(unmined, ", "))
		}
	}

	srcText, srcNational := charset(ctx, src)
	dstText, dstNational := charset(ctx, dst)
	if srcText != dstText || srcNational != dstNational {
		r.Add("Character set", "warn", fmt.Sprintf("the source stores text as %s (national: %s) and the target as %s (national: %s). A character the target's set does not have is replaced on the way, and where a character takes more bytes on the target, a value may no longer fit a column whose length is counted in bytes",
			srcText, srcNational, dstText, dstNational))
	}

	clashes, err := findClashes(ctx, dst, plan)
	if err != nil {
		return nil, nil, err
	}
	checkExisting(r, clashes, cfg.Existing())
	checkPrivileges(plan, r)

	if len(plan.Notes) > 0 {
		r.Add("Carried over with changes", "warn", strings.Join(plan.Notes, "\n"))
	}
	r.Add("Behaviour that differs", "warn", strings.Join([]string{
		"Tablespaces, storage settings and other physical attributes are not carried over: every table and index is created with the target's defaults.",
		"Stored code, views and synonyms are created after the copy, and indexes and constraints are built then. Foreign keys and triggers are added at the very end, after the data has been validated, so they are not in force on the target while the job runs.",
		"Apart from a table, an object that cannot be created on the target is reported with Oracle's message and does not stop the migration. Stored code and views that do not compile on the target are listed when it ends.",
	}, "\n"))
	left := "users, roles, grants, profiles, tablespaces, database links, scheduler jobs, queues, materialized views, external tables, public synonyms and anything in a schema outside the scope are not carried over; recreate what you need on the target"
	if cfg.Scope() == "table" {
		left += ". A table-level migration takes the tables with their indexes, constraints and triggers, and the schema's sequences; views, stored code and synonyms are left out"
	}
	if len(plan.Left) > 0 {
		left += ". Found on the source and not migrated: " + strings.Join(plan.Left, ", ")
	}
	r.Add("Not migrated", "warn", left)

	for _, side := range []struct {
		name string
		db   *sql.DB
	}{{"Source", src}, {"Target", dst}} {
		var proto sql.NullString
		_ = side.db.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','NETWORK_PROTOCOL') FROM dual`).Scan(&proto)
		if !strings.EqualFold(proto.String, "tcps") {
			r.Add(side.name+" encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
		}
	}
	r.Finish()
	return r, clashes, nil
}

// checkPrivileges reports whether the target account can build what the
// plan holds, in its own schema and in others.
func checkPrivileges(plan *Plan, r *migrate.Report) {
	t := plan.Target
	var problems, warnings []string
	own, others := false, false
	for _, o := range plan.Catalog.Owners {
		if plan.Schemas[o] == t.User {
			own = true
		} else {
			others = true
		}
	}
	if others {
		if missing := t.missing(anyPrivs); len(missing) > 0 {
			problems = append(problems, "to build in a schema other than its own the target account needs "+strings.Join(missing, ", "))
		}
	}
	if own {
		// Which of the privileges matter depends on what there is to create.
		needs := map[string]string{"table": "CREATE TABLE", "temporary table": "CREATE TABLE", "sequence": "CREATE SEQUENCE", "view": "CREATE VIEW", "synonym": "CREATE SYNONYM",
			"function": "CREATE PROCEDURE", "procedure": "CREATE PROCEDURE", "package": "CREATE PROCEDURE", "package body": "CREATE PROCEDURE",
			"type": "CREATE TYPE", "type body": "CREATE TYPE", "trigger": "CREATE TRIGGER"}
		lacking := map[string]bool{}
		for _, list := range [][]Stmt{plan.Pre, plan.Code, plan.Post, plan.Final} {
			for _, s := range list {
				if priv := needs[s.Kind]; priv != "" && s.Schema == t.User && !t.Privs[priv] && !lacking[priv] {
					lacking[priv] = true
					if priv == "CREATE TABLE" {
						problems = append(problems, "the target account needs CREATE TABLE")
					} else {
						warnings = append(warnings, priv)
					}
				}
			}
		}
	}
	switch {
	case len(problems) > 0:
		r.Add("Target privileges", "fail", strings.Join(problems, "; "))
	case len(warnings) > 0:
		r.Add("Target privileges", "warn", "the target account lacks "+strings.Join(warnings, ", ")+"; the objects that need it will be reported as not created")
	default:
		r.Add("Target privileges", "ok", "the target account may create the objects to be migrated. Their schema also needs a quota on its default tablespace, which is where the tables go")
	}
}
