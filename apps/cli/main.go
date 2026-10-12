// Command dedie is the command-line interface to Dedie. It works in two
// modes. After `dedie login <server>` every command runs on that Dedie
// server through its API, from any machine. Without a sign-in it works
// directly on this machine's data directory, which is how the server host
// itself is set up and administered.
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
  dedie [--local] <command> [arguments]

Where commands run
  login <server address>       sign in to a Dedie server; later commands run there
  logout                       sign out and revoke the saved token
  status                       show the server and user in use
  Without a sign-in, or with --local, commands work on this machine's data
  directory: the folder where a Dedie server keeps its users, connections,
  jobs and backups.

Setup
  init                         create the organization and its first owner (local)
  user add                     add a user to the organization

Connections
  conn add                     save a database connection; --env labels it
                               production, staging or development
  conn env <name> <environment>
                               label a saved connection, or "none" to take
                               the label off
  conn list                    list saved connections
  conn test <name>             connect and show server details
  conn rm <name>               delete a connection

Query
  query <connection> <sql>     run a statement (read-only unless --write)

Migrations
  migrate create               define a migration between two connections
  migrate list                 list migrations
  migrate preflight <id>       check both databases without changing them; if
                               the target already has some of the objects,
                               it lists them and asks what to do
  migrate preview <id>         print the statements the migration would run
                               on the target, as a script, without running them
  migrate leftovers <id>       print what the migration does not carry over
                               (views it could not translate, stored code it
                               could not convert or that needs review), as a
                               script with the converted drafts and the source's
                               own definitions
  migrate existing <id> fail|skip|replace
                               answer that question without a prompt
  migrate run <id>             run in the foreground until paused or complete
  migrate start <id>           queue for a running dedie-server to execute
  migrate status <id>          show phase, lag and per-table progress
  migrate pause|resume|cutover|abort <id>

Backups
  backup create <connection>   take a backup: --type full, schema, data, tables
                               (logical) or physical; --level full,
                               differential or incremental; --location <name>;
                               --plain for an unencrypted file
  backup list                  list backups
  backup verify <id>           check integrity of a backup file
  backup restore <id>          restore into a connection or a new database
  backup export <id> <file>    write the decrypted archive to a file
  backup extract <id> <dir>    unpack a physical backup into a data directory
  backup recover <connection>  build a data directory that recovers a
                               PostgreSQL server to a moment in time
  backup rm <id>               delete a backup
  backup schedule add|list|rm  recurring backups with retention
  backup location add|list|rm  directories backups may be written to

Overview
  overview                     what needs attention, then every connection
                               with its environment, whether it answers and
                               when it was last backed up

Notifications
  notifications                list what happened recently: migrations,
                               backups, databases that stopped answering.
                               Unread ones are marked; --unread shows only
                               those, --read-all marks everything as read

Audit
  audit list                   show recent audit entries
  audit verify                 verify the audit log has not been altered

Environment
  DEDIE_SERVER       server address, with DEDIE_TOKEN: run remotely without
                     a saved sign-in (for CI)
  DEDIE_TOKEN        API token created on the Account page
  DEDIE_CA_CERT      PEM file for a server with a privately signed certificate
  DEDIE_DATA_DIR     data directory in local mode (default ~/.dedie)
  DEDIE_MASTER_KEY   base64 32-byte master key (default: <data dir>/master.key)
  DEDIE_PASSWORD     password for commands that would otherwise prompt

Production connections
  Anything that destroys or disrupts on a connection labelled production
  (a DROP, TRUNCATE or unfiltered DELETE or UPDATE with query --write, a
  restore over it, a migration that replaces objects on it, a cutover that
  fences it) asks you to type the connection's name. In a script, pass it
  with --confirm-production <name>.

Run "dedie <command> -h" for the flags of a command.
`

func main() {
	args := os.Args[1:]
	// --local ignores a saved sign-in and works on this machine's data
	// directory.
	forceLocal := false
	if len(args) > 0 && args[0] == "--local" {
		forceLocal, args = true, args[1:]
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &cli{ctx: ctx}
	if err := c.run(args, forceLocal); err != nil {
		if c.app != nil {
			c.app.Close()
		}
		fatal(err)
	}
	if c.app != nil {
		c.app.Close()
	}
}

// run picks where the command executes and dispatches it.
func (c *cli) run(args []string, forceLocal bool) error {
	switch args[0] {
	case "login":
		return c.login(args[1:])
	case "logout":
		return c.logout()
	case "status":
		return c.status(forceLocal)
	}
	p, err := loadProfile()
	if err != nil {
		return err
	}
	if p != nil && !forceLocal && args[0] != "init" {
		if c.be, err = c.connect(p); err != nil {
			return err
		}
		return c.dispatch(args)
	}
	if c.app, err = app.Open(dataDir()); err != nil {
		return err
	}
	if args[0] == "init" {
		return c.initOrg(args[1:])
	}
	actor, err := c.app.LocalActor(c.ctx)
	if err != nil {
		return err
	}
	c.be = &local{ctx: c.ctx, app: c.app, actor: actor}
	return c.dispatch(args)
}

func (c *cli) dispatch(args []string) error {
	cmd, rest := args[0], args[1:]
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
		case "env":
			return c.connEnv(rest)
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
		case "existing":
			return c.migrateExisting(rest)
		case "preview":
			return c.migratePreview(rest)
		case "leftovers":
			return c.migrateLeftovers(rest)
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
		case "export":
			return c.backupExport(rest)
		case "extract":
			return c.backupExtract(rest)
		case "recover":
			return c.backupRecover(rest)
		case "location":
			action := ""
			if len(rest) > 0 {
				action, rest = rest[0], rest[1:]
			}
			switch action {
			case "add":
				return c.locationAdd(rest)
			case "list":
				return c.locationList()
			case "rm":
				return c.locationRemove(rest)
			}
		case "schedule":
			action := ""
			if len(rest) > 0 {
				action, rest = rest[0], rest[1:]
			}
			switch action {
			case "add":
				return c.scheduleAdd(rest)
			case "list":
				return c.scheduleList()
			case "rm":
				return c.scheduleRemove(rest)
			}
		}
	case "overview":
		return c.overview()
	case "notifications":
		return c.notifications(args[1:])
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
