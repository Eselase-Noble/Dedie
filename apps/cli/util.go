package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"dedie/engine/app"
)

type cli struct {
	ctx   context.Context
	app   *app.App
	actor app.Actor
}

// table prints aligned columns.
func table(header string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, header)
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	w.Flush()
}

// readPassword takes the password from DEDIE_PASSWORD or prompts for it
// without echo. It is never accepted as a flag, which would expose it in
// shell history and the process list.
func readPassword(prompt string) (string, error) {
	if pw := os.Getenv("DEDIE_PASSWORD"); pw != "" {
		return pw, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no terminal to prompt on; set DEDIE_PASSWORD")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

// parse reads flags that may appear before, between or after positional
// arguments (the standard library stops at the first positional one) and
// returns the positional arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func oneArg(args []string, what string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("expected exactly one argument: %s", what)
	}
	return args[0], nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
