package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"dedie/engine/app"
	"dedie/engine/migrate"
	"dedie/engine/migrate/pgpg"
)

func (c *cli) migrateCreate(args []string) error {
	fs := flag.NewFlagSet("migrate create", flag.ExitOnError)
	var in app.MigrationInput
	fs.StringVar(&in.Name, "name", "", "name for this migration")
	fs.StringVar(&in.SourceID, "source", "", "source connection")
	fs.StringVar(&in.TargetID, "target", "", "target connection")
	schemas := fs.String("schemas", "", "schema level: comma-separated schemas to migrate (PostgreSQL and Oracle)")
	tables := fs.String("tables", "", "table level: comma-separated tables to migrate (schema.table for PostgreSQL and Oracle, table for MySQL)")
	exclude := fs.String("exclude", "", "comma-separated tables to leave out")
	fs.IntVar(&in.Config.SnapshotWorkers, "workers", 4, "tables copied in parallel during the initial copy")
	fs.StringVar(&in.Config.Validation, "validation", "checksum", "cutover validation: checksum, count or none")
	fs.BoolVar(&in.Config.FenceSource, "fence", false, "at cutover, make the source read-only and disconnect its clients")
	columns := fs.String("columns", "", "MongoDB sources: fields to lift into typed columns, as collection:field:type separated by commas, for example users:email:text,users:address.city:text,orders:total:numeric")
	sync := fs.Bool("sync", false, "keep the target following the source continuously instead of planning a cutover")
	offline := fs.Bool("offline", false, "copy once from a consistent snapshot, validate and finish, without following changes; the source needs no replication set up")
	fs.StringVar(&in.Config.OnExisting, "on-existing", "", "when the target already has an object being migrated: fail (default; preflight then asks you), skip (leave it and do not migrate it) or replace (drop it and create it from the source)")
	fs.StringVar(&in.Config.Code, "code", "", "between two kinds of database, what to do with stored procedures, functions and triggers: converted (default; create those whose conversion can be vouched for, hand over the rest with a draft), review (also create those that need a person's check) or off (convert nothing)")
	fs.BoolVar(&in.Config.KeepSlot, "keep-slot", false, "keep the replication slot on the source after completion")
	fs.Parse(args)
	in.Config.Schemas, in.Config.Tables, in.Config.ExcludeTables = splitList(*schemas), splitList(*tables), splitList(*exclude)
	switch {
	case *sync && *offline:
		return errors.New("--sync and --offline cannot be combined")
	case *sync:
		in.Config.Mode = migrate.ModeSync
	case *offline:
		in.Config.Mode = migrate.ModeOffline
	}
	for _, item := range splitList(*columns) {
		parts := strings.SplitN(item, ":", 3)
		if len(parts) != 3 {
			return fmt.Errorf("--columns: %q is not collection:field:type", item)
		}
		if in.Config.Columns == nil {
			in.Config.Columns = map[string][]migrate.ColumnSpec{}
		}
		in.Config.Columns[parts[0]] = append(in.Config.Columns[parts[0]], migrate.ColumnSpec{Field: parts[1], Type: parts[2]})
	}
	j, err := c.be.CreateMigration(in)
	if err != nil {
		return err
	}
	fmt.Printf("Created migration %s (%s level). Next: dedie migrate preflight %s\n", j.ID, in.Config.Scope(), j.ID)
	return nil
}

func (c *cli) migrateList() error {
	jobs, err := c.be.ListMigrations()
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
	report, err := c.be.PreflightMigration(id)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, ch := range report.Checks {
		rows = append(rows, []string{ch.Status, ch.Name, ch.Detail})
	}
	table("RESULT\tCHECK\tDETAIL", rows)
	if len(report.Existing) > 0 {
		chosen, err := c.askOnExisting(id, report)
		if err != nil {
			return err
		}
		if chosen {
			// The answer changes what preflight has to say.
			return c.migratePreflight(args)
		}
	}
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
	if err := c.be.MigrationAction(id, action); err != nil {
		return err
	}
	fmt.Printf("%s: %s requested\n", id, action)
	return nil
}

// migrateRun runs a job and prints progress until it completes, pauses or
// fails. In local mode the job executes in this process, and Ctrl+C hands
// it back to the queue to resume from its checkpoint. Against a server the
// server executes it, and Ctrl+C only stops watching.
func (c *cli) migrateRun(args []string) error {
	id, err := oneArg(args, "migration id")
	if err != nil {
		return err
	}
	d, err := c.be.GetMigration(id)
	if err != nil {
		return err
	}
	if d.Status != app.StatusQueued {
		if err := c.be.MigrationAction(id, "start"); err != nil {
			return err
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.be.RunJob(id) }()

	var lastEvent int64
	show := func() {
		d, err := c.be.GetMigration(id)
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
			d, err := c.be.GetMigration(id)
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
	d, err := c.be.GetMigration(id)
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
		position := "applied " + st.AppliedLSN
		// Sources that report a position of their own also report lag
		// against it.
		if st.SourceLSN != "" {
			position += " of " + st.SourceLSN + " (lag " + humanBytes(st.LagBytes) + ")"
		}
		fmt.Printf("  %s, %d transactions, %d row changes, %d anomalies\n", position, st.TxApplied, st.RowsApplied, st.Anomalies)
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

// askOnExisting shows what the target already holds and lets the
// administrator decide what the migration does about it. It asks only
// when nothing has been decided yet and there is someone to ask; it
// reports whether a new choice was recorded.
func (c *cli) askOnExisting(id string, report *migrate.Report) (bool, error) {
	d, err := c.be.GetMigration(id)
	if err != nil {
		return false, err
	}
	var cfg migrate.Config
	_ = json.Unmarshal(d.Config, &cfg)
	if cfg.OnExisting != "" && cfg.OnExisting != migrate.ExistingFail {
		return false, nil
	}
	byKind := map[string][]string{}
	var kinds []string
	for _, o := range report.Existing {
		if byKind[o.Kind] == nil {
			kinds = append(kinds, o.Kind)
		}
		byKind[o.Kind] = append(byKind[o.Kind], o.Name)
	}
	fmt.Printf("\nThe target already has %d object(s) with the name of one being migrated:\n", len(report.Existing))
	for _, k := range kinds {
		names := byKind[k]
		more := ""
		if len(names) > 12 {
			more = fmt.Sprintf(" and %d more", len(names)-12)
			names = names[:12]
		}
		fmt.Printf("  %-10s %s%s\n", k+":", strings.Join(names, ", "), more)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Printf("\nDecide with: dedie migrate existing %s fail|skip|replace\n", id)
		return false, nil
	}
	fmt.Print(`
What should the migration do about them?
  1) Refuse   do not start; I will deal with them myself (the default)
  2) Skip     leave them exactly as they are and do not migrate them;
              an existing table receives no rows and is not followed
  3) Replace  DROP each one on the target and create it from the source;
              a dropped table loses the data it holds there
`)
	answer, err := prompt("Choose 1, 2 or 3")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "2", "skip":
		return true, c.be.SetOnExisting(id, migrate.ExistingSkip)
	case "3", "replace":
		typed, err := prompt(fmt.Sprintf("This drops %d object(s) on the target. Type REPLACE to confirm", len(report.Existing)))
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(typed) != "REPLACE" {
			fmt.Println("Not confirmed; nothing was changed.")
			return false, nil
		}
		return true, c.be.SetOnExisting(id, migrate.ExistingReplace)
	}
	return false, nil
}

// migrateExisting records the decision without a prompt, for scripts.
func (c *cli) migrateExisting(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: dedie migrate existing <id> fail|skip|replace")
	}
	if err := c.be.SetOnExisting(args[0], args[1]); err != nil {
		return err
	}
	fmt.Printf("%s: objects that already exist on the target will be handled with %q. Next: dedie migrate preflight %s\n", args[0], args[1], args[0])
	return nil
}

// migratePreview prints what the migration would run on the target as a
// script: readable on a terminal, and reviewable in a change request when
// redirected to a file.
func (c *cli) migratePreview(args []string) error {
	id, err := oneArg(args, "migration id")
	if err != nil {
		return err
	}
	list, err := c.be.PreviewMigration(id)
	if err != nil {
		return err
	}
	phase := ""
	for _, st := range list {
		if st.Phase != phase {
			phase = st.Phase
			fmt.Printf("-- ===== %s =====\n\n", map[string]string{
				migrate.PhaseSchema:   "before the copy",
				migrate.PhaseIndexing: "after the copy",
				migrate.PhaseCutover:  "at the end, after validation",
			}[phase])
		}
		if st.End == "//" {
			// A MySQL routine has semicolons inside; the client has to
			// be told that something else ends it.
			fmt.Printf("-- %s %s\nDELIMITER //\n%s\n//\nDELIMITER ;\n\n", st.Kind, st.Name, strings.TrimSpace(st.SQL))
			continue
		}
		if st.End != "" {
			fmt.Printf("-- %s %s\n%s\n%s\n\n", st.Kind, st.Name, strings.TrimSpace(st.SQL), st.End)
			continue
		}
		fmt.Printf("-- %s %s\n%s;\n\n", st.Kind, st.Name, strings.TrimRight(strings.TrimSpace(st.SQL), ";"))
	}
	fmt.Fprintf(os.Stderr, "%d statement(s). Nothing was run.\n", len(list))
	return nil
}

// migrateLeftovers prints what the migration leaves behind on the source
// as a script, each object under the reason it is not migrated. It is
// where converting them by hand starts: redirected to a file, it is
// everything there is to convert, in one place, with a converted draft
// wherever the converter got that far.
func (c *cli) migrateLeftovers(args []string) error {
	id, err := oneArg(args, "migration id")
	if err != nil {
		return err
	}
	list, err := c.be.MigrationLeftovers(id)
	if err != nil {
		return err
	}
	fmt.Print(leftoverScript(list))
	fmt.Fprintf(os.Stderr, "%d object(s) are not migrated or want checking. Nothing was run.\n", len(list))
	return nil
}

// leftoverScript writes out what a migration leaves behind. An object
// that was converted is given as the converted draft, in the target's
// dialect, under the notes on what to check, with the original after it
// as a comment to compare against. Anything else is given as the source
// defines it.
func leftoverScript(list []migrate.Leftover) string {
	var b strings.Builder
	b.WriteString("-- What this migration does not carry over, or carried over with a caveat.\n")
	b.WriteString("-- A converted draft is in the target's dialect; everything else is as the\n")
	b.WriteString("-- source defines it, and has to be converted before it runs on the target.\n\n")
	for _, l := range list {
		head := l.Kind + " " + l.Name
		if l.Status != "" {
			head += "  [" + l.Status + "]"
		}
		fmt.Fprintf(&b, "-- %s\n-- %s\n", head, strings.ReplaceAll(l.Reason, "\n", " "))
		for _, note := range l.Notes {
			fmt.Fprintf(&b, "--   %s\n", note)
		}
		source := strings.TrimSpace(l.Source)
		original := source
		switch {
		case source == "":
			original = "-- (the source did not show its definition)"
		case l.End != "":
			original += "\n" + l.End
		default:
			original = strings.TrimRight(source, ";") + ";"
		}
		if draft := strings.TrimSpace(l.Draft); draft != "" {
			b.WriteString("-- Converted draft:\n" + draft + "\n-- The original on the source:\n")
			for _, line := range strings.Split(original, "\n") {
				b.WriteString("--   " + line + "\n")
			}
			b.WriteString("\n")
			continue
		}
		b.WriteString(original + "\n\n")
	}
	return b.String()
}
