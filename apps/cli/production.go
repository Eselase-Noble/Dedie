package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"dedie/engine/app"
)

// confirmFlag adds the flag with which a script supplies the name of a
// production connection that a person would be asked to type.
func confirmFlag(fs *flag.FlagSet) *string {
	return fs.String("confirm-production", "", "the name of the production connection this acts on; asked for on a terminal when left out")
}

// asksForProductionName reports whether err is the refusal of an action on
// a production connection for want of its name.
func asksForProductionName(err error) bool {
	var local *app.Error
	if errors.As(err, &local) {
		return local.Code == app.CodeConfirmProduction
	}
	var remote *apiError
	return errors.As(err, &remote) && remote.Code == app.CodeConfirmProduction
}

// confirmed runs an action that a production connection lets through only
// when its name has been typed. given is the name from --confirm-production.
// Without it, the action is tried first: most connections are not
// production and ask nothing. If this one does, the name is asked for on a
// terminal and the action tried once more; a script is told which flag to
// pass.
func (c *cli) confirmed(given string, do func() error) error {
	c.be.Confirm(given)
	defer c.be.Confirm("")
	err := do()
	if given != "" || !asksForProductionName(err) {
		return err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("%w Pass --confirm-production <name>", err)
	}
	fmt.Fprintln(os.Stderr, err.Error())
	typed, perr := prompt("Connection name")
	if perr != nil {
		return perr
	}
	c.be.Confirm(strings.TrimSpace(typed))
	if err = do(); asksForProductionName(err) {
		return errors.New("that is not the connection's name; nothing was changed")
	}
	return err
}
