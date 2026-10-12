// Package storedcode is what the pipelines between two kinds of database
// share in moving stored functions, procedures and triggers: deciding
// which of them a job creates, creating them when the data is in, and
// handing the rest to the administrator.
//
// The conversion itself is package codesql's, which says of every routine
// whether it vouches for the result. This package acts on that verdict
// and never second-guesses it: a routine marked for review is created
// only in a job that was told to, and one without a conversion never.
package storedcode

import (
	"fmt"
	"strings"

	"dedie/engine/migrate"
	"dedie/engine/migrate/codesql"
)

// Object is one function, procedure or trigger of the source (or, for
// Oracle, a package or type, which is only ever listed) and what a
// migration does with it.
type Object struct {
	// Kind is "function", "procedure" or "trigger"; anything else is
	// never converted.
	Kind string
	// Schema and Name are the object's on the source, and Table the
	// table of a trigger.
	Schema, Name, Table string
	// Label is the object's name as the operator is shown it.
	Label string
	// Source is the definition in the form the converter reads; see
	// codesql.Object.
	Source string
	// Definition is the statement that defines the object on the source,
	// for the script of what is not migrated, and End what closes it
	// there when a semicolon does not.
	Definition, End string
	// Why is the reason given for leaving the object behind when no
	// conversion is tried: the job has conversion switched off, or the
	// object is of a kind that has none.
	Why string
	// Disabled says a trigger is switched off on the source. It is not
	// created: on the target it would start to fire.
	Disabled bool

	// The rest is filled in by Plan.

	// Status is the converter's verdict: codesql.Converted, Review or
	// Unconverted. It is empty when the converter was not asked.
	Status string
	// Notes are the converter's: what to check, or why not.
	Notes []string
	// Creates are the statements that create the object on the target,
	// one for each thing they create: a trigger is a function and a
	// trigger in PostgreSQL, and one trigger for each event in MySQL.
	Creates []Created
	// Create says the job will create the object. When it does not,
	// Reason says why.
	Create bool
	Reason string
}

// Created is one object a statement creates on the target.
type Created struct {
	// Kind is "function", "procedure" or "trigger".
	Kind string
	// Name is how the operator is shown it, and Quoted the same in SQL.
	// Table is the quoted table of a trigger.
	Name, Quoted, Table string
	SQL                 string
	// Drop removes it again.
	Drop string
}

// key tells objects apart in a job's state.
func (o *Object) key() string { return o.Kind + " " + o.Label }

// convertible reports whether the converter takes objects of this kind.
func (o *Object) convertible() bool {
	return o.Kind == "function" || o.Kind == "procedure" || o.Kind == "trigger"
}

// Plan asks the converter about every object and decides, by the job's
// setting, which the job will create. from and to are connector kinds.
// opt describes the migrated tables; the routines the objects may call
// are added here, from the objects themselves.
func Plan(from, to string, objects []*Object, opt codesql.Options, mode string) {
	if mode == migrate.CodeOff {
		for _, o := range objects {
			o.Reason = o.Why
			if o.convertible() {
				o.Reason += "; converting stored code is switched off for this migration"
			}
		}
		return
	}
	// A routine may call any other, so all of them are named first.
	for _, o := range objects {
		if o.Kind == "function" || o.Kind == "procedure" {
			if r, ok := codesql.Signature(from, codesql.Object{Kind: o.Kind, Schema: o.Schema, Name: o.Name, Source: o.Source}); ok {
				opt.Routines = append(opt.Routines, r)
			}
		}
	}
	for _, o := range objects {
		switch {
		case !o.convertible():
			o.Status, o.Reason = codesql.Unconverted, o.Why
			continue
		case o.Disabled:
			o.Status, o.Reason = codesql.Unconverted, "it is disabled on the source; created on the target it would start to fire"
			continue
		case strings.TrimSpace(o.Source) == "":
			o.Status, o.Reason = codesql.Unconverted, "the source account may not read its definition"
			continue
		}
		res := codesql.Convert(from, to, codesql.Object{Kind: o.Kind, Schema: o.Schema, Name: o.Name, Table: o.Table, Source: o.Source}, opt)
		o.Status, o.Notes = res.Status, res.Notes
		if res.Status == codesql.Unconverted {
			o.Reason = "it uses something that has no conversion: " + summary(res.Notes)
			continue
		}
		for _, sql := range res.Statements {
			c, ok := created(to, sql)
			if !ok {
				// Not a statement this package knows how to undo or to
				// look for on the target; better left to a person.
				o.Status, o.Creates = codesql.Unconverted, nil
				o.Reason = "its conversion is not in a form the migration can create"
				break
			}
			o.Creates = append(o.Creates, c)
		}
		switch {
		case o.Status == codesql.Unconverted:
		case res.Status == codesql.Converted, mode == migrate.CodeReview:
			o.Create = true
		default:
			o.Reason = fmt.Sprintf("it was converted, but %d place(s) may behave differently and have to be checked by a person, so it is not created. "+
				"The converted draft and the notes are given here; a migration set to create such routines as well would create it", len(res.Notes))
		}
	}
}

// summary is the first note, and how many more there are.
func summary(notes []string) string {
	switch len(notes) {
	case 0:
		return "the converter gave no reason"
	case 1:
		return notes[0]
	}
	return fmt.Sprintf("%s (and %d more)", notes[0], len(notes)-1)
}

// name reads a possibly qualified, possibly quoted name from the start of
// s and returns it as written, and unquoted for display.
func name(s string) (quoted, shown, rest string) {
	var parts []string
	i := 0
	for {
		start := i
		switch {
		case i < len(s) && (s[i] == '"' || s[i] == '`'):
			q := s[i]
			var b strings.Builder
			for i++; i < len(s); i++ {
				if s[i] == q {
					if i+1 < len(s) && s[i+1] == q {
						i++
					} else {
						break
					}
				}
				b.WriteByte(s[i])
			}
			i++
			parts = append(parts, b.String())
		default:
			for i < len(s) && (s[i] == '_' || s[i] == '$' || s[i] >= 0x80 || (s[i] >= '0' && s[i] <= '9') || (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z')) {
				i++
			}
			parts = append(parts, s[start:min(i, len(s))])
		}
		if i >= len(s) || s[i] != '.' || parts[len(parts)-1] == "" {
			break
		}
		i++
	}
	i = min(i, len(s))
	return s[:i], strings.Join(parts, "."), s[i:]
}

// created reads off a statement what it creates, and writes the statement
// that removes it again. The converter writes one CREATE for each object,
// with the kind and the name on its first line.
func created(to, sql string) (Created, bool) {
	c := Created{SQL: sql}
	head, _, _ := strings.Cut(sql, "\n")
	for _, kind := range []string{"function", "procedure", "trigger"} {
		prefix := "CREATE " + strings.ToUpper(kind) + " "
		if !strings.HasPrefix(head, prefix) {
			continue
		}
		c.Kind = kind
		var rest string
		c.Quoted, c.Name, rest = name(head[len(prefix):])
		if c.Name == "" {
			return c, false
		}
		upper := strings.ToUpper(kind)
		switch {
		case kind == "trigger":
			// The table is what follows the ON that ends the line; an
			// " ON " earlier on may be inside a quoted column name.
			for at := 0; c.Table == ""; {
				next := strings.Index(rest[at:], " ON ")
				if next < 0 {
					return c, false
				}
				at += next + 4
				if table, shown, after := name(rest[at:]); shown != "" && after == "" {
					c.Table = table
				}
			}
			c.Drop = "DROP TRIGGER IF EXISTS " + c.Quoted
			if to != "mysql" {
				// A PostgreSQL trigger is named within its table.
				c.Drop += " ON " + c.Table
			}
		case !strings.HasPrefix(rest, "("):
			return c, false
		case to == "mysql":
			c.Drop = "DROP " + upper + " IF EXISTS " + c.Quoted
		case strings.HasPrefix(rest, "() RETURNS trigger"):
			c.Drop = "DROP FUNCTION IF EXISTS " + c.Quoted + "()"
		default:
			// Without its arguments the statement only works while the
			// name is not shared, which is how the job left it.
			c.Drop = "DROP ROUTINE IF EXISTS " + c.Quoted
		}
		return c, true
	}
	return c, false
}

// Planned returns the objects a job will create: functions and
// procedures first, in the order given, then triggers, so that a trigger
// finds the routines it calls. The pipeline lists the triggers of a table
// in the order they are to be created.
func Planned(objects []*Object) []*Object {
	var routines, triggers []*Object
	for _, o := range objects {
		switch {
		case !o.Create:
		case o.Kind == "trigger":
			triggers = append(triggers, o)
		default:
			routines = append(routines, o)
		}
	}
	return append(routines, triggers...)
}

// Keep takes out of the plan an object the target already has under one
// of its names and is to keep. taken says whether a name is.
func Keep(objects []*Object, taken func(c Created) bool) {
	for _, o := range objects {
		if !o.Create {
			continue
		}
		for _, c := range o.Creates {
			if taken(c) {
				o.Create = false
				o.Reason = fmt.Sprintf("the target already has a %s called %s, which is kept as it is", c.Kind, c.Name)
				break
			}
		}
	}
}

// Statements lists what a job will run to create the stored code, for a
// preview. end closes a statement in a script; see migrate.Statement.
func Statements(objects []*Object, end string) []migrate.Statement {
	var out []migrate.Statement
	for _, o := range Planned(objects) {
		for _, c := range o.Creates {
			out = append(out, migrate.Statement{Phase: migrate.PhaseCutover, Kind: c.Kind, Name: c.Name, SQL: c.SQL, End: end})
		}
	}
	return out
}

// described is how many names of one sort a preflight finding spells out.
const described = 20

func list(objects []*Object) string {
	names := make([]string, 0, described+1)
	for i, o := range objects {
		if i == described {
			names = append(names, fmt.Sprintf("and %d more", len(objects)-i))
			break
		}
		names = append(names, o.Kind+" "+o.Label)
	}
	return strings.Join(names, ", ")
}

// Describe words the preflight finding about a plan's stored code: what
// will be created, what needs review, what cannot be converted, and what
// the converter takes for granted on the route. scope is a sentence about
// what the job looks at, for a job that does not look at everything. The
// status is "warn" unless everything is created and nothing wants review.
func Describe(from, to string, objects []*Object, mode, scope string) (status, detail string) {
	var lines []string
	if scope != "" {
		lines = append(lines, scope)
	}
	if mode == migrate.CodeOff {
		if len(objects) > 0 {
			lines = append(lines, fmt.Sprintf("Converting stored code is switched off for this migration. %d object(s) are left for you, with their definitions in the script of what is not migrated: %s", len(objects), list(objects)))
			return "warn", strings.Join(lines, "\n")
		}
		return "ok", strings.Join(append(lines, "The source has no stored code in scope."), "\n")
	}
	var create, checked, review, never []*Object
	for _, o := range objects {
		switch {
		case o.Create && o.Status == codesql.Review:
			checked = append(checked, o)
		case o.Create:
			create = append(create, o)
		case o.Status == codesql.Review:
			review = append(review, o)
		default:
			never = append(never, o)
		}
	}
	if len(objects) == 0 {
		return "ok", strings.Join(append(lines, "The source has no stored code in scope."), "\n")
	}
	status = "ok"
	count := func(objs []*Object) string {
		n := map[string]int{}
		for _, o := range objs {
			n[o.Kind]++
		}
		return fmt.Sprintf("%d function(s), %d procedure(s) and %d trigger(s)", n["function"], n["procedure"], n["trigger"])
	}
	if len(create) > 0 {
		lines = append(lines, fmt.Sprintf("%s are converted and created at the end, after the tables have been validated and the views created, so that no trigger fires for a row the migration writes: %s", count(create), list(create)))
	}
	if len(checked) > 0 {
		status = "warn"
		lines = append(lines, fmt.Sprintf("%s are converted and created as well, as chosen, although some of what they do may behave differently on the target. Check each against its notes in the list of what is not migrated: %s", count(checked), list(checked)))
	}
	if len(review) > 0 {
		status = "warn"
		lines = append(lines, fmt.Sprintf("%d need review and are not created: they were converted, but some of what they do may behave differently on the target. Their drafts and notes are in the script of what is not migrated: %s", len(review), list(review)))
	}
	if len(never) > 0 {
		status = "warn"
		lines = append(lines, fmt.Sprintf("%d cannot be converted and are left for you, with their definitions and the reasons: %s", len(never), list(never)))
	}
	if len(create)+len(checked)+len(review) > 0 {
		if assumed := codesql.Assumptions(from, to); len(assumed) > 0 {
			lines = append(lines, "What the conversion takes for granted:\n- "+strings.Join(assumed, "\n- "))
		}
	}
	return status, strings.Join(lines, "\n")
}
