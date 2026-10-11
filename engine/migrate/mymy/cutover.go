package mymy

import (
	"context"
	"database/sql"
	"fmt"

	"dedie/engine/connector"
	"dedie/engine/connector/mysql"
	"dedie/engine/migrate"
)

// fence makes the source database read-only (MySQL 8.0.22 and newer). The
// statement waits for transactions that are changing the database to finish,
// and from then on the server refuses writes to it from every session.
//
// While it waits, the server queues new statements on the database behind
// it. The wait is therefore capped: if a long transaction is in the way, the
// fence gives up and cutover is cancelled rather than stalling the source.
func fence(ctx context.Context, src *sql.DB, database string) error {
	conn, err := src.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION lock_wait_timeout = 20"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "ALTER DATABASE "+mysql.Quote(database)+" READ ONLY = 1")
	return err
}

// unfence undoes fence.
func unfence(ctx context.Context, srcCfg connector.Config) error {
	ctx = context.WithoutCancel(ctx)
	src, err := mysql.Open(ctx, srcCfg, nil)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = src.ExecContext(ctx, "ALTER DATABASE "+mysql.Quote(srcCfg.Database)+" READ ONLY = 0")
	return err
}

// syncAutoIncrement copies each table's next AUTO_INCREMENT value. Replayed
// inserts already push the target's counters past every id that exists, but
// not past ids the source handed out to rows that were later deleted, and
// reusing those could collide with references held outside the database.
func syncAutoIncrement(ctx context.Context, src, dst *sql.DB, plan *Plan) (int, error) {
	conn, err := src.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	// Ask for current figures rather than cached statistics.
	conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")
	rows, err := queryStrings(ctx, conn, `SELECT table_name, auto_increment FROM information_schema.tables
		WHERE table_schema = ? AND auto_increment IS NOT NULL`, plan.Database)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		t := plan.byName[r[0]]
		if t == nil {
			continue
		}
		var next uint64
		if _, err := fmt.Sscan(r[1], &next); err != nil {
			continue
		}
		if _, err := dst.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", t.QName(), next)); err != nil {
			return n, fmt.Errorf("%s: %s", t.Name, message(err))
		}
		n++
	}
	return n, nil
}

// cleanup removes the checkpoint table from the target. The source holds
// nothing for the job: a binary log reader leaves no trace once it disconnects.
func cleanup(ctx context.Context, dstCfg connector.Config, rep migrate.Reporter) {
	ctx = context.WithoutCancel(ctx)
	dst, err := mysql.Open(ctx, dstCfg, nil)
	if err != nil {
		rep.Event("warn", "could not connect to the target to remove the table "+checkpointTable+": "+message(err))
		return
	}
	defer dst.Close()
	if _, err := dst.ExecContext(ctx, "DROP TABLE IF EXISTS `"+checkpointTable+"`"); err != nil {
		rep.Event("warn", "table "+checkpointTable+" was not removed from the target: "+message(err))
	}
}
