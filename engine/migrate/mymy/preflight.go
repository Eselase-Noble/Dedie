package mymy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
)

// Preflight inspects both databases without changing either and reports
// everything that would stop or weaken the migration.
func Preflight(ctx context.Context, srcCfg, dstCfg connector.Config, cfg migrate.Config) (*migrate.Report, error) {
	src, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer dst.Close()
	plan, err := BuildPlan(ctx, src, srcCfg.Database, cfg)
	if err != nil {
		return nil, err
	}
	return preflight(ctx, src, dst, plan, cfg)
}

func variable(ctx context.Context, db *sql.DB, name string) string {
	var k string
	var v sql.NullString
	_ = db.QueryRowContext(ctx, "SHOW GLOBAL VARIABLES LIKE '"+name+"'").Scan(&k, &v)
	return v.String
}

// CheckSource records every finding about the source side: version, binary
// log settings, privileges and the tables selected. It is shared by all
// pipelines that read from MySQL. It returns the source's version string.
// An offline job never reads the binary log, so none of the checks on it
// apply.
func CheckSource(ctx context.Context, src *sql.DB, plan *Plan, cfg migrate.Config, r *migrate.Report) (string, error) {
	var srcVer string
	if err := src.QueryRowContext(ctx, "SELECT VERSION()").Scan(&srcVer); err != nil {
		return "", err
	}
	srcNum := mysql.VersionNum(srcVer)
	switch {
	case strings.Contains(strings.ToLower(srcVer), "mariadb"):
		r.Add("Source version", "fail", "MariaDB is not supported as a source; its binary log and catalog differ from MySQL's")
	case srcNum < 50600:
		r.Add("Source version", "fail", "MySQL "+srcVer+" is too old; version 5.6 or newer is required")
	}

	if cfg.Offline() {
		r.Add("Offline copy", "warn", offlineNote)
	} else {
		checkBinlog(ctx, src, r)
	}

	if len(plan.Tables) == 0 {
		r.Add("Tables", "fail", "no table can be migrated from database "+plan.Database)
	} else {
		r.Add("Tables", "ok", fmt.Sprintf("%d table(s) will be migrated from database %s", len(plan.Tables), plan.Database))
	}
	if len(plan.Missing) > 0 {
		r.Add("Selected tables", "fail", "these tables were selected but do not exist in "+plan.Database+": "+strings.Join(plan.Missing, ", "))
	}
	if len(plan.Skipped) > 0 {
		lines := make([]string, len(plan.Skipped))
		for i, s := range plan.Skipped {
			lines[i] = s.Name + ": " + s.Reason
		}
		r.Add("Tables that will be skipped", "warn", strings.Join(lines, "\n"))
	}
	// The remaining findings concern the stream and the cutover, which an
	// offline job does not have.
	if len(plan.NoKey) > 0 && !cfg.Offline() {
		r.Add("Tables without a primary key", "warn", strings.Join(plan.NoKey, ", ")+
			"\nChanges to these tables are matched on every column, which is slow on large tables and can miss rows that hold approximate numbers. Add a primary key if you can.")
	}
	if cfg.FenceSource && srcNum < 80022 && !cfg.Offline() {
		r.Add("Read-only cutover", "fail", "making a single database read-only needs MySQL 8.0.22 or newer on the source; turn the option off and stop application writes yourself at cutover")
	}
	if !mysql.Encrypted(ctx, src) {
		r.Add("Source encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
	}
	return srcVer, nil
}

// offlineNote is what the operator must know before starting an offline job.
const offlineNote = "the tables are copied once, from a single snapshot; changes made to the source after the copy starts are not carried over. Stop application writes to the source before starting"

// checkBinlog records whether the source's binary log can feed the stream.
func checkBinlog(ctx context.Context, src *sql.DB, r *migrate.Report) {
	if strings.EqualFold(variable(ctx, src, "log_bin"), "ON") {
		r.Add("Binary logging", "ok", "enabled")
	} else {
		r.Add("Binary logging", "fail", "log_bin is off; enable binary logging on the source and restart it")
	}
	if format := variable(ctx, src, "binlog_format"); strings.EqualFold(format, "ROW") {
		r.Add("Binary log format", "ok", "ROW")
	} else {
		r.Add("Binary log format", "fail", fmt.Sprintf("binlog_format is %s; set it to ROW", format))
	}
	// Older servers have no such variable and always log full images.
	if image := variable(ctx, src, "binlog_row_image"); image == "" || strings.EqualFold(image, "FULL") {
		r.Add("Row images", "ok", "FULL")
	} else {
		r.Add("Row images", "fail", fmt.Sprintf("binlog_row_image is %s; set it to FULL so every change carries the whole row", image))
	}
	if _, err := BinlogStatus(ctx, src); err != nil {
		r.Add("Replication privileges", "fail", "cannot read the binary log position ("+message(err)+"); the source account needs REPLICATION CLIENT and REPLICATION SLAVE")
	} else {
		r.Add("Replication privileges", "ok", "source account can read the binary log position")
	}
	if id := variable(ctx, src, "server_id"); id == "0" {
		r.Add("Server id", "fail", "server_id is 0 on the source; replication clients cannot connect until it is set")
	}
}

func preflight(ctx context.Context, src, dst *sql.DB, plan *Plan, cfg migrate.Config) (*migrate.Report, error) {
	r := &migrate.Report{Tables: len(plan.Tables)}
	srcVer, err := CheckSource(ctx, src, plan, cfg, r)
	if err != nil {
		return nil, err
	}
	var dstVer string
	if err := dst.QueryRowContext(ctx, "SELECT VERSION()").Scan(&dstVer); err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(strings.ToLower(dstVer), "mariadb"):
		r.Add("Versions", "fail", "MariaDB is not supported as a target by this pipeline")
	case mysql.VersionNum(dstVer)/100 < mysql.VersionNum(srcVer)/100:
		r.Add("Versions", "fail", fmt.Sprintf("target (%s) is older than source (%s); downgrades are not supported", dstVer, srcVer))
	default:
		r.Add("Versions", "ok", srcVer+" → "+dstVer)
	}

	var dstDB sql.NullString
	if err := dst.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dstDB); err != nil {
		return nil, err
	}
	names := make([]any, 0, len(plan.Tables)+1)
	names = append(names, dstDB.String)
	marks := make([]string, 0, len(plan.Tables))
	for _, t := range plan.Tables {
		names = append(names, t.Name)
		marks = append(marks, "?")
	}
	if len(marks) > 0 {
		existing, err := QueryStrings(ctx, dst, `SELECT table_name FROM information_schema.tables
			WHERE table_schema = ? AND table_name IN (`+strings.Join(marks, ",")+`) ORDER BY 1`, names...)
		if err != nil {
			return nil, err
		}
		if len(existing) > 0 {
			list := make([]string, len(existing))
			for i, e := range existing {
				list[i] = e[0]
			}
			r.Add("Target is empty", "fail", "these tables already exist in the target database: "+strings.Join(list, ", "))
		} else {
			r.Add("Target is empty", "ok", "no conflicting tables in target database "+dstDB.String)
		}
	}

	// Objects the pipeline does not carry over. Reported, never silently dropped.
	r.Add("Not migrated", "warn", "recreate these on the target yourself: user accounts and privileges. Views, routines, triggers and events are created under the migrating account, not their original DEFINER")

	if !mysql.Encrypted(ctx, dst) {
		r.Add("Target encryption", "warn", "the connection is not using TLS; data will cross the network in clear text")
	}
	r.Finish()
	return r, nil
}
