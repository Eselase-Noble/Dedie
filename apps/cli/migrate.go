package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"time"

	"dedie/engine/app"
	"dedie/engine/migrate/pgpg"
)

func (c *cli) migrateCreate(args []string) error {
	fs := flag.NewFlagSet("migrate create", flag.ExitOnError)
	var in app.MigrationInput
	fs.StringVar(&in.Name, "name", "", "name for this migration")
	fs.StringVar(&in.SourceID, "source", "", "source connection")
	fs.StringVar(&in.TargetID, "target", "", "target connection")
	schemas := fs.String("schemas", "", "comma-separated schemas to migrate (default: all)")
	exclude := fs.String("exclude", "", "comma-separated schema.table names to leave out")
	fs.IntVar(&in.Config.SnapshotWorkers, "workers", 4, "tables copied in parallel during the initial copy")
	fs.StringVar(&in.Config.Validation, "validation", "checksum", "cutover validation: checksum, count or none")
	fs.BoolVar(&in.Config.FenceSource, "fence", false, "at cutover, make the source read-only and disconnect its clients")
	fs.BoolVar(&in.Config.KeepSlot, "keep-slot", false, "keep the replication slot on the source after completion")
	fs.Parse(args)
	in.Config.Schemas, in.Config.ExcludeTables = splitList(*schemas), splitList(*exclude)
	j, err := c.app.CreateMigration(c.ctx, c.actor, in)
	if err != nil {
		return err
	}
	fmt.Printf("Created migration %s. Next: dedie migrate preflight %s\n", j.ID, j.ID)
	return nil
}

func (c *cli) migrateList() error {
	jobs, err := c.app.ListMigrations(c.ctx, c.actor)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, j := range jobs {
		rows = append(rows, []string{j.ID, j.Name, j.Status, j.Phase, j.CreatedAt.Local().Format("2006-01-02 15:04")})
	}
	table("ID\tNAME\tSTATUS\tPHASE\tCREATED", rows)
	return nil
}

func (c *cli) migratePreflight(args []string) error {
	id, err := oneArg(args, "migration id")
	if err != nil {
		return err
	}
	report, err := c.app.PreflightMigration(c.ctx, c.actor, id)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, ch := range report.Checks {
		rows = append(rows, []string{ch.Status, ch.Name, ch.Detail})
	}
	table("RESULT\tCHECK\tDETAIL", rows)
	if !report.OK {
		return fmt.Errorf("preflight failed; fix the items marked fail and run it again")
	}
	fmt.Printf("\nPreflight passed. Next: dedie migrate run %s\n", id)
	return nil
}

func (c *cli) migrateAction(action string, args []string) error {
	id, err := oneArg(args, "migration id")
	if err != nil {
		return err
	}
	if err := c.app.MigrationAction(c.ctx, c.actor, id, action); err != nil {
		return err
	}
	fmt.Printf("%s: %s requested\n", id, action)
	return nil
}

// migrateRun executes a job in this process and prints progress until it
// completes, pauses or fails. Ctrl+C hands the job back to the queue; it
// resumes from its checkpoint on the next run.
func (c *cli) migrateRun(args []string) error {
	id, err := oneArg(args, "migration id")
	if err != nil {
		return err
	}
	d, err := c.app.GetMigration(c.ctx, c.actor, id)
	if err != nil {
		return err
	}
	if d.Status != app.StatusQueued {
		if err := c.app.MigrationAction(c.ctx, c.actor, id, "start"); err != nil {
			return err
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.app.RunJobForeground(c.ctx, id) }()

	var lastEvent int64
	show := func() {
		d, err := c.app.GetMigration(c.ctx, c.actor, id)
		if err != nil {
			return
		}
		for i := len(d.Events) - 1; i >= 0; i-- {
			if e := d.Events[i]; e.ID > lastEvent {
				lastEvent = e.ID
				fmt.Printf("%s  %-5s %s\n", e.Time.Local().Format("15:04:05"), e.Level, e.Message)
			}
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			show()
			if err != nil {
				return err
			}
			d, err := c.app.GetMigration(c.ctx, c.actor, id)
			if err != nil {
				return err
			}
			if d.Status == app.StatusFailed {
				return fmt.Errorf("migration failed: %s", d.Error)
			}
			fmt.Println("Migration is now", d.Status)
			return nil
		case <-tick.C:
			show()
		}
	}
}

func (c *cli) migrateStatus(args []string) error {
	id, err := oneArg(args, "migration id")
	if err != nil {
		return err
	}
	d, err := c.app.GetMigration(c.ctx, c.actor, id)
	if err != nil {
		return err
	}
	var st pgpg.State
	_ = json.Unmarshal(d.State, &st)
	fmt.Printf("%s  %s\n  status: %s   phase: %s\n", d.ID, d.Name, d.Status, d.Phase)
	if d.Error != "" {
		fmt.Println("  error: ", d.Error)
	}
	if st.SourceVersion != "" {
		fmt.Printf("  %s -> %s\n", st.SourceVersion, st.TargetVersion)
	}
	if st.AppliedLSN != "" {
		fmt.Printf("  applied %s of %s (lag %s), %d transactions, %d row changes, %d anomalies\n",
			st.AppliedLSN, st.SourceLSN, humanBytes(st.LagBytes), st.TxApplied, st.RowsApplied, st.Anomalies)
	}
	fmt.Println()
	var rows [][]string
	for _, t := range d.Tables {
		counts := ""
		if t.SrcCount >= 0 {
			counts = strconv.FormatInt(t.SrcCount, 10) + " / " + strconv.FormatInt(t.DstCount, 10)
		}
		rows = append(rows, []string{t.Schema + "." + t.Table, t.Status, strconv.FormatInt(t.RowsCopied, 10), counts, t.Validation})
	}
	table("TABLE\tSTATUS\tROWS COPIED\tSOURCE / TARGET\tVALIDATION", rows)
	return nil
}
