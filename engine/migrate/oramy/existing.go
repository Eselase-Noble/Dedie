package oramy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector/mysql"
	"dedie/engine/connector/oracle"
	"dedie/engine/migrate"
	"dedie/engine/migrate/storedcode"
	"dedie/engine/migrate/viewsql"
)

// Kinds of object that can stand in the way of the migration.
const (
	kindTable = "table"
	// A view shares its namespace with tables.
	kindView = "view"
	// Foreign keys and check constraints are named per database in
	// MySQL, not per table, so one on a table the migration has nothing
	// to do with can still hold a name the migration needs.
	kindForeignKey = "foreign key"
	kindCheck      = "check constraint"
	// Converted stored code. Functions and procedures have a namespace
	// each, and triggers are named per database.
	kindFunction  = "function"
	kindProcedure = "procedure"
	kindTrigger   = "trigger"
)

// found is something in the target database that has the name of an
// object the plan would create.
type found struct {
	Kind string
	// Name is the table's or view's name, or the constraint's.
	Name string
	// Holder is the table a constraint is on.
	Holder string
	// Label is the source table whose place a table or view takes.
	Label string
	// ForView says the name is wanted for a translated view, not for a
	// table.
	ForView bool
}

// isTable reports whether the object takes the place of a table.
func (f found) isTable() bool { return (f.Kind == kindTable || f.Kind == kindView) && !f.ForView }

// String names the object the way the operator is shown it.
func (f found) String() string {
	if f.Holder != "" {
		return f.Name + " on " + f.Holder
	}
	return f.Name
}

// existingChunk is how many names one lookup asks for.
const existingChunk = 1000

// lookup runs a query once for each chunk of names and gathers its rows
// of text.
func lookup(ctx context.Context, dst *sql.DB, query string, names []string) ([][]string, error) {
	var out [][]string
	for len(names) > 0 {
		n := min(len(names), existingChunk)
		args := make([]any, n)
		for i, name := range names[:n] {
			args[i] = name
		}
		names = names[n:]
		rows, err := dst.QueryContext(ctx, fmt.Sprintf(query, strings.TrimSuffix(strings.Repeat("?,", n), ",")), args...)
		if err != nil {
			return nil, fmt.Errorf("target: %s", message(err))
		}
		for rows.Next() {
			var a, b, c string
			if err := rows.Scan(&a, &b, &c); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, []string{a, b, c})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// existing finds what the target database already has under the names
// the plan would use, for its tables and for the views it translated:
// tables first, then everything else. A constraint
// on a table that is itself in the way is not listed; it shares the
// table's fate, whatever that is.
func existing(ctx context.Context, dst *sql.DB, plan *Plan) ([]found, error) {
	var out []found
	targets := make([]string, len(plan.Tables))
	labels := map[string]string{}
	for i, t := range plan.Tables {
		targets[i] = t.Target
		// The server may fold names to lower case.
		labels[t.Target], labels[strings.ToLower(t.Target)] = t.Label(), t.Label()
	}
	forView := map[string]bool{}
	for _, v := range viewsql.Kept(plan.Views) {
		targets = append(targets, v.Target.Bare)
		forView[strings.ToLower(v.Target.Bare)] = true
		labels[v.Target.Bare], labels[strings.ToLower(v.Target.Bare)] = v.Source(), v.Source()
	}
	rows, err := lookup(ctx, dst, `SELECT table_name, IF(table_type = 'VIEW', 'view', 'table'), '' FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name IN (%s) ORDER BY 2, 1`, targets)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, r := range rows {
		label, ok := labels[r[0]]
		if !ok {
			label = labels[strings.ToLower(r[0])]
		}
		taken[r[0]] = true
		out = append(out, found{Kind: r[1], Name: r[0], Label: label, ForView: forView[strings.ToLower(r[0])]})
	}

	var constraints []string
	wanted := map[string]bool{}
	for _, list := range [][]Stmt{plan.Post, plan.Final} {
		for _, s := range list {
			if s.constraint != "" {
				constraints = append(constraints, s.constraint)
				wanted[s.Kind+" "+strings.ToLower(s.constraint)] = true
			}
		}
	}
	rows, err = lookup(ctx, dst, `SELECT constraint_name, IF(constraint_type = 'CHECK', 'check constraint', 'foreign key'), table_name
		FROM information_schema.table_constraints
		WHERE constraint_schema = DATABASE() AND constraint_type IN ('FOREIGN KEY', 'CHECK') AND constraint_name IN (%s) ORDER BY 2 DESC, 1`, constraints)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		// The two kinds are named apart from each other.
		if wanted[r[1]+" "+strings.ToLower(r[0])] && !taken[r[2]] {
			out = append(out, found{Kind: r[1], Name: r[0], Holder: r[2]})
		}
	}

	// The converted functions, procedures and triggers the job creates.
	names := map[string][]string{}
	for _, o := range storedcode.Planned(plan.Code) {
		for _, c := range o.Creates {
			names[c.Kind] = append(names[c.Kind], c.Name)
		}
	}
	for _, q := range []struct{ kind, query string }{
		{kindFunction, `SELECT routine_name, '', '' FROM information_schema.routines WHERE routine_schema = DATABASE() AND routine_type = 'FUNCTION' AND routine_name IN (%s) ORDER BY 1`},
		{kindProcedure, `SELECT routine_name, '', '' FROM information_schema.routines WHERE routine_schema = DATABASE() AND routine_type = 'PROCEDURE' AND routine_name IN (%s) ORDER BY 1`},
		{kindTrigger, `SELECT trigger_name, '', '' FROM information_schema.triggers WHERE trigger_schema = DATABASE() AND trigger_name IN (%s) ORDER BY 1`},
	} {
		if rows, err = lookup(ctx, dst, q.query, names[q.kind]); err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, found{Kind: q.kind, Name: r[0]})
		}
	}
	return out, nil
}

// existingShown is how many names of one kind a check spells out; the
// report's own list always has them all.
const existingShown = 20

// listed names objects for a check's detail, grouped by kind when they
// are of more than one.
func listed(objects []found, byKind bool) string {
	var kinds []string
	names := map[string][]string{}
	for _, f := range objects {
		kind := ""
		if byKind {
			kind = f.Kind + "s: "
		}
		if _, ok := names[kind]; !ok {
			kinds = append(kinds, kind)
		}
		names[kind] = append(names[kind], f.String())
	}
	groups := make([]string, len(kinds))
	for i, kind := range kinds {
		list, more := names[kind], ""
		if len(list) > existingShown {
			more = fmt.Sprintf(" and %d more", len(list)-existingShown)
			list = list[:existingShown]
		}
		groups[i] = kind + strings.Join(list, ", ") + more
	}
	return strings.Join(groups, "; ")
}

// checkExisting reports what the target database already holds under a
// name of the migration. Everything found goes into the report's list,
// whatever the policy: it is what the administrator is shown when asked
// to choose one. The two checks then say what the chosen policy does
// about it, and only refuse the job when none was chosen.
func checkExisting(objects []found, database, policy string, r *migrate.Report) {
	var tables, others []found
	for _, f := range objects {
		r.Existing = append(r.Existing, migrate.ExistingObject{Kind: f.Kind, Name: f.String()})
		if f.Kind == kindTable {
			tables = append(tables, f)
		} else {
			others = append(others, f)
		}
	}
	switch {
	case len(tables) == 0:
		r.Add("Target is empty", "ok", "no conflicting tables in target database "+database)
	case policy == migrate.ExistingSkip:
		r.Add("Target is empty", "warn", "these tables already exist in the target database. As chosen, they are kept exactly as they are and left out of the migration: "+
			"no rows are copied into them, changes to their source are not followed and they are not validated: "+listed(tables, false))
	case policy == migrate.ExistingReplace:
		r.Add("Target is empty", "warn", "these tables already exist in the target database. As chosen, they will be DROPPED, with all the data they hold there, "+
			"and created again from the source: "+listed(tables, false))
	default:
		r.Add("Target is empty", "fail", "these tables already exist in the target database: "+listed(tables, false))
	}
	switch {
	case len(others) == 0:
		r.Add("Existing objects", "ok", "nothing on the target has the name of an object to be migrated")
	case policy == migrate.ExistingSkip:
		r.Add("Existing objects", "warn", "the target already has these. As chosen, they are kept exactly as they are and the source's are left out; "+
			"a table whose name a view holds is not migrated: "+listed(others, true))
	case policy == migrate.ExistingReplace:
		r.Add("Existing objects", "warn", "the target already has these. As chosen, they will be DROPPED and created again from the source: "+listed(others, true))
	default:
		r.Add("Existing objects", "fail", "the target already has these: "+listed(others, true)+
			". Drop or rename them, migrate into an empty database, or choose whether what exists is skipped or replaced")
	}
}

// settleExisting decides, once, what happens to the objects that were on
// the target before the job created anything, and records the decision
// in the job's state. It has to be once: as soon as the job has created
// its own tables, everything "exists", and a retry could not tell its
// own tables from the ones it was told to leave alone.
//
// ours says the target already carries this job's mark. Then what is
// found there was left by an earlier attempt of the job itself, and
// there is nothing to decide.
func (r *runner) settleExisting(ctx context.Context, ours bool) error {
	if r.st.ExistingDecided {
		return nil
	}
	policy := r.cfg.Existing()
	if !ours && policy != migrate.ExistingFail {
		objects, err := existing(ctx, r.dst, r.plan)
		if err != nil {
			return err
		}
		if policy == migrate.ExistingReplace {
			if err := r.dropExisting(ctx, objects); err != nil {
				return err
			}
		} else {
			if err := r.keepExisting(objects); err != nil {
				return err
			}
			if kept := len(objects); kept > 0 {
				r.rep.Event("info", fmt.Sprintf("left %d existing object(s) on the target as they are, as chosen: %s", kept, listed(objects, true)))
			}
		}
	}
	r.st.ExistingDecided = true
	r.save()
	return nil
}

// keepExisting records in the job's state what it leaves out because the
// target's own is to stay: the tables whose names are taken, the views,
// and the constraints. It fails when that leaves no table to migrate.
func (r *runner) keepExisting(objects []found) error {
	for _, f := range objects {
		switch {
		case f.isTable():
			r.st.SkippedTables = append(r.st.SkippedTables, f.Label)
		case f.ForView:
			// Whatever holds the name, it is a view that is not created.
			r.st.SkippedObjects = append(r.st.SkippedObjects, kindView+" "+f.Name)
		default:
			r.st.SkippedObjects = append(r.st.SkippedObjects, f.Kind+" "+f.Name)
		}
	}
	if len(r.st.SkippedTables) == len(r.plan.Tables) {
		return fmt.Errorf("every table of the migration already exists on the target and existing tables are skipped, so there is nothing to migrate")
	}
	return nil
}

// drop is the statement that removes the object.
func (f found) drop() string {
	switch f.Kind {
	case kindTable:
		return "DROP TABLE " + mysql.Quote(f.Name)
	case kindView:
		return "DROP VIEW " + mysql.Quote(f.Name)
	case kindForeignKey:
		return "ALTER TABLE " + mysql.Quote(f.Holder) + " DROP FOREIGN KEY " + mysql.Quote(f.Name)
	case kindCheck:
		return "ALTER TABLE " + mysql.Quote(f.Holder) + " DROP CHECK " + mysql.Quote(f.Name)
	case kindFunction, kindProcedure, kindTrigger:
		return "DROP " + strings.ToUpper(f.Kind) + " " + mysql.Quote(f.Name)
	}
	return ""
}

// dropExisting removes the objects in the way, for a job told to replace
// them. Foreign key checks are off while it does, so that a table other
// tables refer to can go; the checks are on again before the connection
// returns to the pool.
func (r *runner) dropExisting(ctx context.Context, objects []found) error {
	if len(objects) == 0 {
		return nil
	}
	conn, err := r.dst.Conn(ctx)
	if err != nil {
		return fmt.Errorf("target: %s", message(err))
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET foreign_key_checks = 0"); err != nil {
		return fmt.Errorf("target: %s", message(err))
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SET foreign_key_checks = 1")
	tables, others := 0, 0
	for _, f := range objects {
		if f.Kind == kindTable {
			tables++
		} else {
			others++
		}
		if _, err := conn.ExecContext(ctx, f.drop()); err != nil {
			return fmt.Errorf("drop existing %s %s: %s", f.Kind, f, message(err))
		}
	}
	msg := fmt.Sprintf("dropped %d existing table(s) on the target, as chosen", tables)
	if others > 0 {
		msg += fmt.Sprintf(", and %d other object(s) with the name of one to be migrated", others)
	}
	r.rep.Event("info", msg+": "+listed(objects, true))
	return nil
}

// scope returns the job's options with the tables left out that the
// decision on existing objects skips. They are excluded exactly as if
// the operator had excluded them, so that nothing downstream (the copy,
// key logging, the redo stream, validation) knows of them.
func (r *runner) scope(ctx context.Context) (migrate.Config, error) {
	cfg := r.cfg
	if len(r.st.SkippedTables) == 0 {
		return cfg, nil
	}
	skipped := map[string]bool{}
	for _, label := range r.st.SkippedTables {
		skipped[strings.ToUpper(label)] = true
	}
	cfg.ExcludeTables = append(cfg.ExcludeTables[:len(cfg.ExcludeTables):len(cfg.ExcludeTables)], r.st.SkippedTables...)
	if len(cfg.Tables) == 0 {
		return cfg, nil
	}
	// A table that is both asked for and excluded is reported as missing
	// by the catalog, so a skipped table also leaves the list of those
	// asked for. A name without a schema means the connecting user's.
	var user string
	if err := r.src.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM dual`).Scan(&user); err != nil {
		return cfg, oracle.Friendly(err)
	}
	var tables []string
	for _, name := range cfg.Tables {
		full := strings.ToUpper(strings.TrimSpace(name))
		if !strings.Contains(full, ".") {
			full = strings.ToUpper(user) + "." + full
		}
		if !skipped[full] {
			tables = append(tables, name)
		}
	}
	cfg.Tables = tables
	return cfg, nil
}

// buildPlan reads and translates the source for this job, without what
// the decision on existing objects leaves out.
func (r *runner) buildPlan(ctx context.Context) error {
	cfg, err := r.scope(ctx)
	if err != nil {
		return err
	}
	plan, err := BuildPlan(ctx, r.src, cfg, r.targetVersion, r.st.SkippedTables...)
	if err != nil {
		return err
	}
	plan.leaveOut(r.st.SkippedObjects)
	r.plan = plan
	return nil
}
