package storedcode

import (
	"context"
	"fmt"
	"strings"

	"dedie/engine/migrate"
	"dedie/engine/migrate/codesql"
	"dedie/engine/migrate/viewsql"
)

// What became of an object the job set out to create.
const (
	// Done: every statement that creates it ran.
	Done = "created"
	// NotCreated: the target refused it, or its name was taken by the
	// time it was its turn. Nothing of it is on the target.
	NotCreated = "not created"
	// creating marks an object the job is about to create. It is saved
	// before the first statement runs, so that a job interrupted here
	// knows what it finds on the target for its own.
	creating = "creating"
)

// Outcome is what became of one object. A job keeps these in its state.
type Outcome struct {
	Kind string `json:"kind"`
	// Name is the object's name on the source.
	Name   string `json:"name"`
	Status string `json:"status"`
	// Review says the object was created although the converter marked
	// it for review.
	Review bool   `json:"review,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (o Outcome) key() string { return o.Kind + " " + o.Name }

// Summary counts what a finished job did with views and stored code, for
// the one line an interface shows about it.
type Summary struct {
	// Created are on the target and need no attention.
	Created int `json:"created"`
	// Review are on the target and want checking: stored code created
	// although it was marked for review, and views that could not be
	// verified in time.
	Review int `json:"review"`
	// Left are not on the target: they are for the administrator.
	Left int `json:"left"`
}

// Views adds a plan's views and what became of them to a summary.
func (s *Summary) Views(views []*viewsql.View, results []viewsql.Result) {
	s.Left += len(viewsql.LeftOut(views))
	for _, r := range results {
		switch {
		case r.Lost():
			s.Left++
		case r.Status == viewsql.Unverified:
			s.Review++
		case r.Status == viewsql.Verified:
			s.Created++
		}
	}
}

// Count adds a plan's stored code and its outcomes to a summary.
func (s *Summary) Count(objects []*Object, outcomes []Outcome) {
	byKey := map[string]Outcome{}
	for _, out := range outcomes {
		byKey[out.key()] = out
	}
	for _, o := range objects {
		switch out := byKey[o.key()]; {
		case out.Status == Done && out.Review:
			s.Review++
		case out.Status == Done:
			s.Created++
		default:
			s.Left++
		}
	}
}

// Target is the target database as far as creating stored code goes.
type Target struct {
	// Exec runs one statement. It reports, without an error, when the
	// target already has what the statement creates. An error is worded
	// for the operator.
	Exec func(ctx context.Context, sql string) (existed bool, err error)
	// Previous is what an earlier attempt of the same job recorded, and
	// Save stores the record as it grows; between them a job that was
	// interrupted here does not repeat what it finished and can tell its
	// own objects from ones that appeared under the same names.
	Previous []Outcome
	Save     func([]Outcome)
	Event    func(level, message string)
}

// Finish creates the stored code a job planned: functions and procedures
// first, then triggers. It must run last, when nothing is copied or
// applied any more: a trigger created earlier would fire for the rows the
// migration writes, on top of what the source's trigger already did to
// them.
//
// An object the target refuses never fails the job. Whatever of it was
// created by then is dropped again, and the object is reported with the
// target's message. The only error is the context's.
func (t Target) Finish(ctx context.Context, objects []*Object) ([]Outcome, error) {
	previous := map[string]Outcome{}
	for _, out := range t.Previous {
		previous[out.key()] = out
	}
	var outcomes []Outcome
	save := func() {
		if t.Save != nil {
			t.Save(outcomes)
		}
	}
	created, review, failed := 0, 0, 0
	for _, o := range Planned(objects) {
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}
		out, known := previous[o.key()]
		if !known || out.Status == creating {
			out = Outcome{Kind: o.Kind, Name: o.Label, Status: creating, Review: o.Status == codesql.Review}
			outcomes = append(outcomes, out)
			save()
			out.Status, out.Detail = t.one(ctx, o, known)
			if err := ctx.Err(); err != nil {
				return outcomes, err
			}
			outcomes[len(outcomes)-1] = out
			save()
			switch {
			case out.Status != Done:
				t.Event("warn", fmt.Sprintf("%s %s was not created on the target: %s", o.Kind, o.Label, out.Detail))
			case out.Review:
				t.Event("warn", fmt.Sprintf("created %s %s, which needs review: %s", o.Kind, o.Label, summary(o.Notes)))
			}
		} else {
			outcomes = append(outcomes, out)
		}
		switch {
		case out.Status != Done:
			failed++
		case out.Review:
			review++
		default:
			created++
		}
	}
	if len(outcomes) > 0 {
		t.Event("info", fmt.Sprintf("stored code: %d created, %d created that need review, %d refused by the target", created, review, failed))
	}
	return outcomes, nil
}

// one creates a single object. started says an earlier attempt of the job
// had begun on it, so what is found under its names is the job's own.
func (t Target) one(ctx context.Context, o *Object, started bool) (status, detail string) {
	var done []Created
	undo := func() string {
		left := ""
		for i := len(done) - 1; i >= 0; i-- {
			if _, err := t.Exec(context.WithoutCancel(ctx), done[i].Drop); err != nil {
				left += fmt.Sprintf(". The %s %s, created for it, could not be dropped again (%s): drop it by hand", done[i].Kind, done[i].Name, err)
			}
		}
		return left
	}
	for _, c := range o.Creates {
		existed, err := t.Exec(ctx, c.SQL)
		switch {
		case err != nil:
			return NotCreated, "the target did not accept the converted " + c.Kind + ": " + err.Error() + undo()
		case existed && !started:
			// Whatever was there when the job began was dealt with then,
			// as the administrator chose. This arrived since, and is
			// not the job's to replace or to drop.
			return NotCreated, fmt.Sprintf("the target has had a %s called %s since the job started, which was left as it is", c.Kind, c.Name) + undo()
		}
		done = append(done, c)
	}
	return Done, ""
}

// draft writes the converted statements as a script of the target.
func draft(to string, o *Object) string {
	if len(o.Creates) == 0 {
		return ""
	}
	var b strings.Builder
	if to == "mysql" {
		// The bodies have semicolons inside.
		b.WriteString("DELIMITER //\n")
		for _, c := range o.Creates {
			b.WriteString(strings.TrimSpace(c.SQL) + "\n//\n")
		}
		b.WriteString("DELIMITER ;")
		return b.String()
	}
	for i, c := range o.Creates {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimSpace(c.SQL) + ";")
	}
	return b.String()
}

// Leftovers lists the stored code a job does not create, or created with
// a caveat, each with its definition on the source and, where the
// converter got that far, the converted draft and its notes. outcomes is
// what became of the objects the job tried; nil before it ran. to is the
// target's connector kind.
func Leftovers(to string, objects []*Object, outcomes []Outcome) []migrate.Leftover {
	byKey := map[string]Outcome{}
	for _, out := range outcomes {
		byKey[out.key()] = out
	}
	var list []migrate.Leftover
	for _, o := range objects {
		l := migrate.Leftover{Kind: o.Kind, Name: o.Label, Source: o.Definition, End: o.End, Status: o.Status, Notes: o.Notes, Reason: o.Reason, Draft: draft(to, o)}
		out, tried := byKey[o.key()]
		switch {
		case tried && out.Status == Done && !out.Review:
			continue
		case tried && out.Status == Done:
			l.Status = "created, needs review"
			l.Reason = "it is on the target, converted, but some of what it does may behave differently there; check it against the notes"
		case tried && out.Status == NotCreated:
			l.Status = NotCreated
			l.Reason = out.Detail
		case o.Create:
			// The job has yet to create it; it is not left behind.
			continue
		}
		if strings.TrimSpace(l.Source) == "" {
			l.Reason += "; the source account may not read its definition, so it is not shown here"
		}
		list = append(list, l)
	}
	return list
}
