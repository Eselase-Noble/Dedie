package viewsql

import (
	"fmt"
	"strings"
)

// View is one view of the source and what a migration does with it.
type View struct {
	// Schema and Name are the view's on the source; in MySQL the schema
	// is the database.
	Schema, Name string
	// Columns are the view's columns on the source, in order.
	Columns []Column
	// Definition is the view's query as the source keeps it.
	Definition string

	// The rest is filled in by Plan.

	// Label is the name the view gets on the target, as the operator is
	// shown it, and Target the same in SQL.
	Label  string
	Target Relation
	// SQL is the statement that creates the view on the target. It is
	// empty when the view is left out, and Reason then says why.
	SQL    string
	Reason string
	// needs are the other views this one reads.
	needs []*View
}

// Source is the view's name on the source, as the operator is shown it.
func (v *View) Source() string { return v.Schema + "." + v.Name }

// Naming is how a pipeline names things on its target.
type Naming struct {
	From, To Dialect
	// Table finds a migrated table on the target by its source name.
	Table func(schema, name string) (Relation, bool)
	// View says what a view of the source is called on the target. A
	// reason is returned instead when it cannot have a name there, for
	// instance because a table takes it.
	View func(schema, name string) (label string, target Relation, reason string)
	// Ident maps the name of a column or an alias.
	Ident func(name string) string
	// Temporal reports whether some migrated table has a date or time
	// column of this name; see Options. A pipeline into MySQL sets it.
	Temporal func(column string) bool
}

// Quote writes a name as a quoted identifier of the dialect.
func Quote(d Dialect, name string) string {
	if d == MySQL {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Plan translates the views and decides which of them the migration will
// try to create. It returns those first, each after the views it reads,
// and then the ones that are left out, each with its Reason.
//
// A view is left out when its definition uses something the translator
// has no rule for, when it reads a table that is not migrated, and when
// it reads another view that is itself left out.
func Plan(views []*View, n Naming) []*View {
	byRef := map[Ref]*View{}
	// A view can be read by another, so its date columns count as well.
	dates := map[string]bool{}
	for _, v := range views {
		v.Label, v.Target, v.Reason = n.View(v.Schema, v.Name)
		v.SQL, v.needs = "", nil
		byRef[Ref{v.Schema, v.Name}] = v
		for _, c := range v.Columns {
			if c.Class == Temporal {
				dates[strings.ToLower(c.Name)] = true
			}
		}
	}
	var temporal func(string) bool
	if n.Temporal != nil {
		temporal = func(column string) bool { return dates[strings.ToLower(column)] || n.Temporal(column) }
	}
	for _, v := range views {
		if v.Reason != "" {
			continue
		}
		query, reads, err := Translate(v.Definition, Options{From: n.From, To: n.To, Schema: v.Schema, Ident: n.Ident, Temporal: temporal,
			Relation: func(schema, name string) (Relation, bool) {
				if rel, ok := n.Table(schema, name); ok {
					return rel, true
				}
				if other := byRef[Ref{schema, name}]; other != nil && other != v {
					return other.Target, true
				}
				return Relation{}, false
			}})
		if err != nil {
			v.Reason = err.Error()
			continue
		}
		for _, r := range reads {
			if other := byRef[r]; other != nil {
				if _, table := n.Table(r.Schema, r.Name); !table {
					v.needs = append(v.needs, other)
				}
			}
		}
		// The columns are named explicitly, after the source's: left to
		// itself each database has its own name for a column that is an
		// expression without an alias.
		cols := ""
		if len(v.Columns) > 0 {
			names := make([]string, len(v.Columns))
			for i, c := range v.Columns {
				names[i] = Quote(n.To, n.Ident(c.Name))
			}
			cols = " (" + strings.Join(names, ", ") + ")"
		}
		v.SQL = "CREATE VIEW " + v.Target.Quoted + cols + " AS " + query
	}
	// A view that reads one that is left out cannot be created either.
	for changed := true; changed; {
		changed = false
		for _, v := range views {
			if v.Reason != "" {
				continue
			}
			for _, need := range v.needs {
				if need.Reason != "" {
					v.Reason = fmt.Sprintf("it reads the view %s, which is left out", need.Source())
					v.SQL = ""
					changed = true
					break
				}
			}
		}
	}

	var kept, left []*View
	placed := map[*View]bool{}
	var place func(v *View)
	place = func(v *View) {
		if placed[v] {
			return
		}
		placed[v] = true
		for _, need := range v.needs {
			place(need)
		}
		kept = append(kept, v)
	}
	for _, v := range views {
		if v.Reason == "" {
			place(v)
		} else {
			left = append(left, v)
		}
	}
	return append(kept, left...)
}

// Kept returns the views of a plan that the migration will try to create.
func Kept(views []*View) []*View {
	var out []*View
	for _, v := range views {
		if v.SQL != "" {
			out = append(out, v)
		}
	}
	return out
}

// LeftOut returns the views of a plan that stay behind.
func LeftOut(views []*View) []*View {
	var out []*View
	for _, v := range views {
		if v.SQL == "" {
			out = append(out, v)
		}
	}
	return out
}

// describedViews is how many names a preflight finding spells out.
const describedViews = 40

// Describe words the preflight finding about a plan's views: which will
// be attempted, and which are left out and why. The status is "warn" when
// some are left out.
func Describe(views []*View) (status, detail string) {
	kept, left := Kept(views), LeftOut(views)
	status = "ok"
	var lines []string
	if len(kept) > 0 {
		names := make([]string, 0, len(kept))
		for i, v := range kept {
			if i == describedViews {
				names = append(names, fmt.Sprintf("and %d more", len(kept)-i))
				break
			}
			names = append(names, v.Source())
		}
		lines = append(lines, fmt.Sprintf("%d view(s) are translated and created at the end, once the tables have been validated. "+
			"Each is then compared with the source, and one that does not return the same there is dropped again and reported: %s",
			len(kept), strings.Join(names, ", ")))
	}
	if len(left) > 0 {
		status = "warn"
		lines = append(lines, fmt.Sprintf("%d view(s) are left out; their definitions are in the script of what is not migrated:", len(left)))
		for i, v := range left {
			if i == describedViews {
				lines = append(lines, fmt.Sprintf("and %d more", len(left)-i))
				break
			}
			lines = append(lines, v.Source()+": "+v.Reason)
		}
	}
	return status, strings.Join(lines, "\n")
}
