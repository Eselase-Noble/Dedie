package pgpg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
	"dedie/engine/connector/postgres"
)

// Fence makes the source database read-only for new sessions and disconnects
// existing clients so they pick the setting up. It stops well-behaved
// applications from writing after the final sync; it is not a security
// boundary, since a session may still override the setting.
func Fence(ctx context.Context, src *pgx.Conn) error {
	var db string
	if err := src.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		return err
	}
	if _, err := src.Exec(ctx, "ALTER DATABASE "+qident(db)+" SET default_transaction_read_only = on"); err != nil {
		return err
	}
	_, err := src.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
			AND backend_type = 'client backend' AND application_name <> 'dedie'`)
	return err
}

// Unfence undoes Fence. It opens its own connection because new sessions on
// a fenced database start read-only and must opt out first.
func Unfence(ctx context.Context, srcCfg connector.Config) error {
	ctx = context.WithoutCancel(ctx)
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		return err
	}
	defer src.Close(ctx)
	var db string
	if err := src.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		return err
	}
	if _, err := src.Exec(ctx, "SET default_transaction_read_only = off"); err != nil {
		return err
	}
	_, err = src.Exec(ctx, "ALTER DATABASE "+qident(db)+" RESET default_transaction_read_only")
	return err
}

// syncSequences copies sequence positions. Logical replication carries row
// changes only, so without this the target would hand out ids already used.
// Two kinds of sequence are not the job's to move: one the job was told to
// keep as the target has it, and the sequence behind an identity column of
// a table that is not being migrated, which the target has only if the
// table was there already.
func syncSequences(ctx context.Context, src, dst *pgx.Conn, plan *Plan) (int, error) {
	rows, err := src.Query(ctx, `SELECT s.schemaname, s.sequencename, s.last_value, format('%I.%I', s.schemaname, s.sequencename),
			coalesce((SELECT x.refobjid FROM pg_catalog.pg_depend x WHERE x.classid='pg_catalog.pg_class'::regclass
				AND x.objid=format('%I.%I', s.schemaname, s.sequencename)::regclass AND x.deptype='i' LIMIT 1), 0)::oid
		FROM pg_catalog.pg_sequences s
		WHERE s.schemaname = ANY($1) AND s.last_value IS NOT NULL`, plan.Schemas)
	if err != nil {
		return 0, err
	}
	type seq struct {
		schema, name, ident string
		last                int64
		identityOf          uint32
	}
	seqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (seq, error) {
		var s seq
		return s, r.Scan(&s.schema, &s.name, &s.last, &s.ident, &s.identityOf)
	})
	if err != nil {
		return 0, err
	}
	migrated := map[uint32]bool{}
	for _, t := range plan.Tables {
		migrated[t.OID] = true
	}
	n := 0
	for _, s := range seqs {
		if plan.kept[KeptKey("sequence", s.ident)] || (s.identityOf != 0 && !migrated[s.identityOf]) {
			continue
		}
		if _, err := dst.Exec(ctx, `SELECT setval(format('%I.%I', $1::text, $2::text)::regclass, $3, true)`, s.schema, s.name, s.last); err != nil {
			return n, fmt.Errorf("sequence %s.%s: %s", s.schema, s.name, pgMessage(err))
		}
		n++
	}
	return n, nil
}

// DropSlot removes the replication slot, waiting briefly for a WAL sender
// that is still shutting down. A forgotten slot makes the source retain WAL
// without bound, so callers treat failure here as worth reporting.
func DropSlot(ctx context.Context, src *pgx.Conn, slot string) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		var exists, active bool
		err := src.QueryRow(ctx, `SELECT count(*) > 0, coalesce(bool_or(active), false) FROM pg_replication_slots WHERE slot_name=$1`, slot).Scan(&exists, &active)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if !active {
			_, err := src.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slot)
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("replication slot %s is still in use", slot)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// CleanupSource removes what a job created on the source: its replication
// slot and its publication.
func CleanupSource(ctx context.Context, st *State, srcCfg connector.Config, rep Reporter) {
	// An offline job named neither, because it created neither.
	if st.Slot == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	src, err := postgres.Connect(ctx, srcCfg)
	if err != nil {
		rep.Event("warn", "could not connect to the source to remove the replication slot "+st.Slot+": "+pgMessage(err))
		return
	}
	defer src.Close(ctx)
	src.Exec(ctx, "SET default_transaction_read_only = off")
	if err := DropSlot(ctx, src, st.Slot); err != nil {
		rep.Event("warn", "replication slot "+st.Slot+" was not removed, drop it manually: "+pgMessage(err))
	}
	if _, err := src.Exec(ctx, "DROP PUBLICATION IF EXISTS "+qident(st.Publication)); err != nil {
		rep.Event("warn", "publication "+st.Publication+" was not removed: "+pgMessage(err))
	}
}

// cleanup removes what the job created outside the migrated data: the slot
// and publication on the source and the checkpoint on the target.
func cleanup(ctx context.Context, jobID string, st *State, srcCfg, dstCfg connector.Config, rep Reporter) {
	ctx = context.WithoutCancel(ctx)
	CleanupSource(ctx, st, srcCfg, rep)
	if dst, err := postgres.Connect(ctx, dstCfg); err == nil {
		defer dst.Close(ctx)
		var exists bool
		_ = dst.QueryRow(ctx, `SELECT to_regclass('`+metaSchema+`.checkpoint') IS NOT NULL`).Scan(&exists)
		if exists {
			dst.Exec(ctx, `DELETE FROM `+metaSchema+`.checkpoint WHERE job_id=$1`, jobID)
			var left int
			if dst.QueryRow(ctx, `SELECT count(*) FROM `+metaSchema+`.checkpoint`).Scan(&left) == nil && left == 0 {
				dst.Exec(ctx, `DROP SCHEMA `+metaSchema+` CASCADE`)
			}
		}
	}
}
