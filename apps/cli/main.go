// Command dedie is the command-line interface to the dedie engine. It
// works directly on the data directory, so it needs no running server; a
// migration started here can be watched in the web interface and vice versa.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"dedie/engine/app"
)

const usage = `dedie - database migration and administration

Usage:
  dedie <command> [arguments]

Setup
  init                         create the organization and its first owner
  user add                     add a user to the organization

Connections
  conn add                     save a database connection
  conn list                    list saved connections
  conn test <name>             connect and show server details
  conn rm <name>               delete a connection

Query
  query <connection> <sql>     run a statement (read-only unless --write)

Migrations
  migrate create               define a migration between two connections
  migrate list                 list migrations
  migrate preflight <id>       check both databases without changing them
  migrate run <id>             run in the foreground until paused or complete
  migrate start <id>           queue for a running dedie-server to execute
  migrate status <id>          show phase, lag and per-table progress
  migrate pause|resume|cutover|abort <id>

Backups
  backup create <connection>   take an encrypted logical backup
  backup list                  list backups
  backup verify <id>           check integrity of a backup file
  backup restore <id>          restore into a connection
  backup rm <id>               delete a backup

Audit
  audit list                   show recent audit entries
  audit verify                 verify the audit log has not been altered

Environment
  DEDIE_DATA_DIR     data directory (default ~/.dedie)
  DEDIE_MASTER_KEY   base64 32-byte master key (default: <data dir>/master.key)
  DEDIE_PASSWORD     password for commands that would otherwise prompt

Run "dedie <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := app.Open(app.DefaultDataDir())
	if err != nil {
		fatal(err)
	}
	err = dispatch(ctx, a, os.Args[1:])
	a.Close()
	if err != nil {
		fatal(err)
	}
}

func dispatch(ctx context.Context, a *app.App, args []string) error {
	c := &cli{ctx: ctx, app: a}
	cmd, rest := args[0], args[1:]
	if cmd == "init" {
		return c.initOrg(rest)
	}
	var err error
	if c.actor, err = a.LocalActor(ctx); err != nil {
		return err
	}
	sub := ""
	if len(rest) > 0 {
		sub, rest = rest[0], rest[1:]
	}
	switch cmd {
	case "user":
		if sub == "add" {
			return c.userAdd(rest)
		}
	case "conn":
		switch sub {
		case "add":
			return c.connAdd(rest)
		case "list":
			return c.connList()
		case "test":
			return c.connTest(rest)
		case "rm":
			return c.connRemove(rest)
		}
	case "query":
		return c.query(args[1:])
	case "migrate":
		switch sub {
		case "create":
			return c.migrateCreate(rest)
		case "list":
			return c.migrateList()
		case "preflight":
			return c.migratePreflight(rest)
		case "run":
			return c.migrateRun(rest)
		case "status":
			return c.migrateStatus(rest)
		case "start", "pause", "resume", "cutover", "abort", "delete":
			return c.migrateAction(sub, rest)
		}
	case "backup":
		switch sub {
		case "create":
			return c.backupCreate(rest)
		case "list":
			return c.backupList()
		case "verify":
			return c.backupVerify(rest)
		case "restore":
			return c.backupRestore(rest)
		case "rm":
			return c.backupRemove(rest)
		}
	case "audit":
		switch sub {
		case "list":
			return c.auditList(rest)
		case "verify":
			return c.auditVerify()
		}
	}
	return errors.New("unknown command; run `dedie help`")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "dedie:", err)
	os.Exit(1)
}
