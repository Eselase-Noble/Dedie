package main

import (
	"errors"
	"strings"
	"testing"

	"dedie/engine/app"
)

// nameTaker stands in for a backend: it remembers the name it was last
// given, as both real ones do.
type nameTaker struct {
	backend
	name string
}

func (n *nameTaker) Confirm(name string) { n.name = name }

func TestConfirmedPassesTheGivenNameAndClearsIt(t *testing.T) {
	be := &nameTaker{}
	c := &cli{be: be}
	refusal := &app.Error{Status: 428, Code: app.CodeConfirmProduction, Message: "billing is a production connection. Type its name to edit its rows."}

	// An action on a connection that asks nothing runs once, as it is.
	calls := 0
	if err := c.confirmed("", func() error { calls++; return nil }); err != nil || calls != 1 {
		t.Errorf("an action that needs no name: %d call(s), %v", calls, err)
	}

	// The name from the flag accompanies the action, and not what follows.
	var sent string
	err := c.confirmed("billing", func() error {
		sent = be.name
		return nil
	})
	if err != nil || sent != "billing" {
		t.Errorf("the action ran with %q (%v), want the name from the flag", sent, err)
	}
	if be.name != "" {
		t.Errorf("the name %q was left set for later requests", be.name)
	}

	// A wrong name from the flag is not asked about again: the refusal is
	// the answer.
	calls = 0
	err = c.confirmed("biling", func() error { calls++; return refusal })
	if calls != 1 || !asksForProductionName(err) {
		t.Errorf("a wrong name from the flag: %d call(s), %v", calls, err)
	}

	// Without a terminal to ask on (as under `go test`), the user is told
	// which flag to pass.
	err = c.confirmed("", func() error { return refusal })
	if err == nil || !strings.Contains(err.Error(), "--confirm-production") || !strings.Contains(err.Error(), "billing") {
		t.Errorf("a script without the flag was told: %v", err)
	}
}

func TestAsksForProductionName(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&app.Error{Status: 428, Code: app.CodeConfirmProduction}, true},
		{&apiError{Status: 428, Code: app.CodeConfirmProduction}, true},
		{&app.Error{Status: 403, Message: "this connection is marked read-only"}, false},
		{&apiError{Status: 404}, false},
		{errors.New("could not reach the server"), false},
		{nil, false},
	} {
		if got := asksForProductionName(c.err); got != c.want {
			t.Errorf("asksForProductionName(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
