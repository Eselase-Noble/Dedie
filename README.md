# Dedie

A query browser, administration console and migration engine for PostgreSQL, MySQL, MongoDB and Oracle.

*Dedie* is Ewe for "safe": the data should arrive safely and stay that way.

Website: https://eselase-noble.github.io/Dedie/ · Downloads: https://github.com/Eselase-Noble/Dedie/releases · Guides: [setting up Dedie](docs/setup.md), [preparing your databases](docs/databases.md)

Dedie moves a production database to a newer server version, or to a different database engine, while the old one stays online, or as a one-time offline copy. It copies a consistent snapshot, follows the source's replication log to stay in step, and switches over in a write pause measured in seconds. The same engine powers a query browser with data editing, scheduled encrypted backups with verified restore, health monitoring with alerts, and administration of database users and connections, behind role-based access control, two-factor sign-in and a tamper-evident audit log.

## Contents

- [What it does](#what-it-does)
- [Documentation](#documentation)
- [Status](#status)
- [Programs](#programs)
- [Quick start](#quick-start)
- [The web interface](#the-web-interface)
- [Notifications](#notifications)
- [The command line](#the-command-line)
- [The desktop application](#the-desktop-application)
- [Migrating a database](#migrating-a-database)
- [Offline migration](#offline-migration)
- [Continuous sync](#continuous-sync)
- [Backups](#backups)
- [Point-in-time recovery](#point-in-time-recovery)
- [Monitoring and alerts](#monitoring-and-alerts)
- [Database users and connections](#database-users-and-connections)
- [Security model](#security-model)
- [HTTP API](#http-api)
- [Architecture](#architecture)
- [Development](#development)
- [Configuration reference](#configuration-reference)
- [Roadmap](#roadmap)

## What it does

**Online migration.** Upgrade PostgreSQL, MySQL or Oracle to a newer version, move a database between MySQL and PostgreSQL in either direction, or move Oracle or MongoDB into an open-source database, with the source in service throughout. A migration can cover a whole database, selected schemas, or selected tables. It can be paused and resumed, survives a crash without losing or repeating a change, and refuses to complete a cutover if source and target differ.

**Offline migration.** Every path can also run as a one-time copy: the source is read as it stood at one moment, the result is validated, and the job finishes by itself. It asks nothing of the source beyond an account that can read it, which suits test copies, maintenance windows and databases you may not reconfigure.

**Continuous sync.** The same pipelines can keep a target following its source indefinitely, for a reporting copy or a warm standby on another engine.

**Query browser.** A full-window workspace with a schema tree and any number of tabs. A query tab is a SQL editor that follows the connection's dialect, completes table and column names, formats SQL, explains and analyzes statements, and can cancel one that is running. A table tab pages through a table with a filter and, when you switch editing on, changes cells, adds rows and deletes them. Results sort, filter and export as CSV, JSON or SQL. Queries can be saved privately or shared with the organization.

**Creating and owning objects.** Create a database, a schema or a table in any database on the server, and hand databases, schemas and tables to another role.

**Backup and recovery.** Logical and physical backups at full, differential and incremental levels, compressed, encrypted by default, and stored in a directory you choose or in S3-compatible object storage. PostgreSQL servers can be recovered to any moment in time. Schedules with retention take them automatically and verify each one. Restore into an existing database, into a new one, or one table at a time.

**Monitoring and alerts.** Every database is checked once a minute. Uptime, response time and migration lag are shown on one page and exported for Prometheus, and alerts go to Slack, a webhook or email.

**Database users and connections.** Create database users, give them roles or a ready-made access level, cap how many connections a user or a database may hold, see which applications are using the server's connections, and disconnect them.

**Server administration.** Live sessions with cancel and terminate, database sizes, replication slots and the WAL they retain, and the settings that matter for replication and security.

**Production safeguards.** Label a connection production, staging or development. The label is shown wherever the connection is named, and anything that destroys or disrupts on a production connection asks you to type its name first.

**Organization controls.** Users with five roles, two-factor sign-in, API tokens, per-connection permissions, a password policy, encrypted credential storage, and a hash-chained audit log that records every statement and every change.

## Documentation

| Guide | Covers |
|---|---|
| [Setting up Dedie](docs/setup.md) | installing from a release or from source, the first start, TLS and reverse proxies, running as a service, the data directory and master key, upgrading, troubleshooting |
| [Preparing your databases](docs/databases.md) | the settings and privileges each kind of source needs, online and offline, with the statements to run; Oracle 12c notes |
| This README | what every feature does and how it behaves |

## Status

| Capability | PostgreSQL | MySQL | Oracle | MongoDB |
|---|---|---|---|---|
| Online version upgrade | Working (source 10+) | Working (source 5.6+) | Working (source 12c+; tested on 23ai) | — |
| Query browser and schema explorer | Working | Working | Working | Working (commands as JSON) |
| Edit rows in the grid | Working | Working | Working | — |
| Logical backups: whole database, structure, data, selected tables | Working | Working | — | — |
| Physical backup of the whole server | Working | Working (Dedie on the database host) | — | — |
| Point-in-time recovery | Working (12+) | — | — | — |
| Backups in S3-compatible object storage | Working | Working | — | — |
| Differential and incremental backups | Working (physical, 17+) | Working (logical, from the binary log) | — | — |
| Backup storage locations, plain or encrypted files | Working | Working | — | — |
| Connection pool view, manual and automatic closing of idle connections | Working | Working | — | — |
| Scheduled backups with retention | Working | Working | — | — |
| Health monitoring and alerts | Working | Working | Working | Working |
| Database users, roles, connection limits | Working | Working | — | — |
| Change object owners | Working | not applicable | not applicable | not applicable |
| Server console | Working | Working | Working (sessions, cancel, end) | Working |

| Migration or sync path | State |
|---|---|
| PostgreSQL → PostgreSQL | Working |
| MySQL → MySQL | Working |
| MySQL → PostgreSQL | Working: tables, keys, indexes and foreign keys are translated; views, procedures, functions and triggers are [converted where that is safe](#views-and-stored-code-across-engines) |
| PostgreSQL → MySQL | Working: tables, keys, indexes and foreign keys are translated; views, functions, procedures and triggers are [converted where that is safe](#views-and-stored-code-across-engines) |
| Oracle → Oracle | Working: everything in the schema is taken from Oracle's own definitions, PL/SQL and triggers included |
| Oracle → PostgreSQL | Working: tables, keys, sequences, identity columns, indexes, check constraints and foreign keys are translated; views and PL/SQL are [converted where that is safe](#views-and-stored-code-across-engines) |
| Oracle → MySQL | Working: tables, keys, identity columns, indexes, check constraints and foreign keys are translated; views and PL/SQL are [converted where that is safe](#views-and-stored-code-across-engines); sequences are not migrated |
| MongoDB → PostgreSQL | Working: each collection becomes a table holding every document as `jsonb`, with chosen fields lifted into typed columns; indexes and validators are not translated |

All eight pipelines are covered by end-to-end tests that migrate a database under write load, pause and resume mid-stream, cut over and compare the result, and by a second test that runs each one offline. The PostgreSQL test runs 16 → 18; the cross-engine tests run MySQL 8.0 → PostgreSQL 18, PostgreSQL 16 → MySQL 8.0 and MongoDB 8.0 → PostgreSQL 18; the Oracle tests run from Oracle 23ai to PostgreSQL 18, MySQL 8.0 and Oracle 23ai. MySQL and MongoDB versions other than 8.0, and Oracle releases other than 23ai (12c included), have not been exercised yet.

Not built yet: SQL Server, two-way sync with conflict handling, backups of Oracle and MongoDB, single sign-on, and external key management. See the [roadmap](#roadmap).

## Programs

All three share one engine, so a migration started in one can be watched or controlled from another.

| Program | What it is | Built from |
|---|---|---|
| `dedie-server` | Web interface, HTTP API and the worker that runs migrations. For teams. | `apps/server` (Go) |
| `dedie` | Command-line interface. Signs in to a Dedie server from any machine, or works directly on the server host's data directory. | `apps/cli` (Go) |
| Dedie Desktop | A native window for one person on their own machine. | `apps/desktop` (JavaFX) |

## Quick start

Requirements:

- Go 1.26 or newer and Node 22 or newer, to build
- JDK 17 or newer and Maven, for the desktop application only
- the database client tools on the machine that runs Dedie, for backups: `pg_dump`, `pg_restore` and `pg_basebackup` (at least as new as the servers you back up), `mysqldump` and `mysql`

```sh
git clone https://github.com/Eselase-Noble/Dedie.git
cd Dedie
make                # builds the web interface, bin/dedie-server and bin/dedie
bin/dedie-server    # http://127.0.0.1:8080
```

The first screen is the sign-in page. On a new installation choose **Create account** there to set up your organization and its owner, then add connections. Further users are added by owners and admins from the Dedie users page.

`dedie-server` keeps its own state in a **data directory** (`~/.dedie` unless told otherwise): its users, the saved connections with their passwords encrypted, migration jobs, the audit log and backup files. Your databases can be anywhere the server can reach over the network; a migration between two database servers in different data centres needs only that Dedie can connect to both.

The server listens on loopback by default and refuses to serve plain HTTP on any other address, because sessions and database credentials would cross the network unencrypted. To expose it:

```sh
bin/dedie-server --addr 0.0.0.0:8443 --tls-cert server.crt --tls-key server.key
# or, behind a reverse proxy that terminates TLS:
bin/dedie-server --addr 127.0.0.1:8080 --behind-tls-proxy
```

## The web interface

| Page | What you do there |
|---|---|
| Overview | See what needs attention, whether each connection answers and when it was last backed up, and what is running. A new installation is walked through its first connection, backup and migration. |
| Connections | Add, edit, test and delete connections, label each with its environment, see which servers are online, and restrict a connection to chosen people. Passwords are write-only; weak SSL modes are flagged. |
| Query browser | Browse schemas; open tables and edit their rows; write, format, explain and save SQL in tabs; sort, filter and export results; create databases, schemas and tables; change owners. |
| Migrations | Create an online or offline migration or a continuous sync at database, schema or table level; see every supported path; run the preflight check and keep its report; preview the schema that will be created; have views and stored code converted between engines and get the rest as a worklist; decide what happens to objects the target already has; watch phase, progress, lag and time remaining; pause, resume, cut over, abort or duplicate. |
| Backups | Take a backup of any type, schedule backups with retention, verify, download, restore into an existing or a new database, and review restore history. |
| Monitoring | Uptime and response time of every database, migrations in progress, alert channels and recent alerts. |
| Server activity | Inspect sessions, sizes, replication slots and settings; cancel or terminate a session. |
| Database users | Create and change users on a database server, set connection limits, and see and disconnect who is connected. |
| Dedie users | Create users, change roles, disable accounts, reset passwords and two-factor sign-in, and set the security policy. |
| Audit log | Read the log and verify that it has not been altered. |
| Notifications | Everything that happened while you were not looking: migrations started, finished, failed or waiting for a decision, backups and restores, and monitoring alerts. |
| Account | Change your password, turn on two-factor sign-in, review your sessions, create API tokens and switch on desktop notifications. |

Navigation and actions a role may not use are hidden; the server enforces the same rules regardless.

**Ctrl+K** (Cmd+K on macOS) opens a palette that jumps to any page, connection or migration and starts the common actions. **?** lists every keyboard shortcut; **G** followed by a letter goes to a page (G then C for Connections, G then Q for the query browser, and so on). The interface is light and works at phone width, where tables turn into one block per row. Dialogs keep the keyboard focus inside them and hand it back when they close. Its typeface, Inter, is shipped with the application (`web/public/fonts`, SIL Open Font License); nothing is fetched from another host, so it looks the same on a closed network and in the desktop application.

### The overview

The first page answers what an operator asks on arriving.

- **Needs attention** lists, most pressing first: connections that have stopped answering, migrations that failed, were paused, or are waiting for a decision about objects on the target, connections whose latest backup failed, schedules that could not run, a write-ahead log archiver that has stopped, and production connections that no backup schedule covers. Each links to the page where it is dealt with. When nothing is waiting it says so.
- **Connections** shows each one with its environment, whether it answered the last check, and when it was last backed up and whether a schedule will do it again.
- **Migrations** puts running ones first, with the progress of their copy.
- **Getting started** appears on a new installation and ticks off three steps as they are done: save a connection, take a backup, create a migration. It goes away when all three are done, or when you hide it.

You are shown only what your role could see elsewhere: a connection restricted to other people does not appear, and backups are left out for roles that may not see them. `dedie overview` prints the same on the command line.

### Environments and production safeguards

A connection can be labelled **production**, **staging** or **development**, in its form or with `dedie conn add --env production` and `dedie conn env <name> <environment>`. The label appears beside the connection's name on every page, in every list a connection is chosen from, and in `dedie conn list`. The query browser shows a red strip for as long as it is pointed at production.

On a connection labelled production, these ask you to type the connection's name before they do anything:

| Action | Where |
|---|---|
| A `DROP`, `TRUNCATE` or `ALTER … DROP`, a `DELETE` or `UPDATE` with no `WHERE`, and on MongoDB `drop`, `dropDatabase`, `dropIndexes` and a `delete` with an empty filter | a query run with writes allowed |
| Editing, adding or deleting rows | the table editor; the name is typed once when editing is switched on in a tab |
| Ending a session, disconnecting sessions, closing idle connections by hand | Server activity, Database users |
| Dropping a database user | Database users |
| Restoring a backup over the connection's own database | Backups; restoring into a new database beside it asks nothing |
| Starting a migration that was told to replace objects the target already has | the migration's page, when the target is production |
| A cutover that fences the source: makes it read-only and disconnects its clients | the migration's page, when the source is production |
| Taking the production label off | the connection's form |

The server enforces this, not the page: a request without the name is refused before the database is contacted, whether it comes from the interface, the command line or a script calling the API. Each confirmed action leaves a `production.confirm` entry in the audit log naming the connection and the action. On the command line you are asked for the name on a terminal; a script passes `--confirm-production <name>`.

The check reads the statement you typed, skipping comments, quoted text and dollar-quoted bodies. It is there to make you look twice at the obvious mistake on the wrong database, and does not look inside a function or procedure that a statement calls. It adds to the other rules and replaces none of them: a role still needs the permission, a read-only connection still refuses writes, and a restricted connection is still hidden from those it is not granted to. Reading, ordinary writes with a `WHERE`, creating objects and taking backups ask nothing. Rules that close idle connections automatically keep running on a production connection once they have been set up.

### Notifications

The bell beside the organization name counts what you have not read. Its panel lists recent notifications, each linking to the page where you can act, and new ones appear as a brief message in the corner while Dedie is open. The browser tab's title carries the count.

| About | You are told when |
|---|---|
| Migrations | one is started, resumed, paused, aborted, completed or fails; a cutover is cancelled because validation found a difference; preflight found objects on the target and is waiting for your decision; a sync falls far behind |
| Backups | a scheduled backup finishes; any backup, verification or restore fails; a schedule could not run; the write-ahead log archiver for point-in-time recovery stops |
| Monitoring | a database stops answering, and when it answers again |
| Your account | an API token is created for it, or it is signed in to from a new address while signed in elsewhere |

You see a notification only if your role could see what it is about, and a connection restricted to chosen people is announced to those people alone. Reading one does not mark it read for anyone else. They are kept for 30 days.

- **In the browser**, Account → *Desktop notifications in this browser* asks for permission to show them as system notifications when the tab is in the background.
- **In the desktop application** they appear through the system tray when the window is not in front, and the window title carries the count.
- **On the command line**, `dedie notifications` lists them (`--unread`, `--read-all`).
- For people who are not looking at Dedie at all, [alert channels](#monitoring-and-alerts) send the monitoring events to Slack, a webhook or email.

### The query browser

- **Tabs.** Open as many query and table tabs as you need; they are remembered for each connection.
- **Tables.** Click a table to page through its rows in primary-key order, with a `WHERE` filter. Switch **Edit data** on to change a cell, add a row or delete one. Each edit is a single statement with its values bound as parameters, applied only if it touches exactly one row, and written to the audit log. A table without a primary key cannot be edited.
- **Queries.** Ctrl/Cmd+Enter runs the statement, or only the selected text. **Explain** shows the plan without running the statement; **Analyze** runs it in a read-only transaction and shows where the time went. **Cancel** stops a running statement at the server.
- **Results.** Click a header to sort, type to filter, click a cell for its full value. Export exactly what is shown as CSV, JSON or `INSERT` statements.
- **Saved queries.** Keep a statement under a name, for yourself or shared with everyone who can run queries.
- **MongoDB.** A MongoDB connection takes database commands as JSON, the form `runCommand` uses, for example `{"find": "orders", "filter": {"status": "open"}, "limit": 50}`. Commands that can write need writes allowed, as SQL does.

## The command line

The CLI works in two modes.

**Against a Dedie server**, from any machine. Sign in once; later commands run on that server through its API, with your role:

```sh
dedie login https://dedie.example.com     # asks for email, password and, if enabled, the two-factor code
dedie status                              # which server, which user

dedie conn add --name prod-16 --host db-old.internal --db shop --user migrator --sslmode verify-full
dedie conn add --kind mysql --name orders --host mysql.internal --db orders --user migrator
dedie conn add --kind mongodb --name events --host mongo.internal --db events --user reader
dedie conn env prod-16 production         # label it; destructive actions on it now ask for its name
dedie conn list
dedie overview                            # what needs attention, connection health, last backups
dedie query prod-16 "select count(*) from orders"      # read-only unless --write
dedie backup create prod-16 --note "before upgrade"
dedie conn add --kind oracle --name erp --host ora.internal --db ORCLPDB1 --user migrator
dedie migrate list
dedie migrate preview <id>                # the statements a migration would run on the target
dedie migrate leftovers <id>              # views and stored code that will not be created, with source and converted drafts
dedie notifications --unread              # what has happened since you last looked
dedie logout                              # revokes the saved sign-in
```

`dedie login` exchanges your password for an API token and stores only the token, in a file readable by you alone. The password is not kept. `dedie logout` revokes the token at the server; it can also be revoked from the Account page. Plain `http://` is accepted only for a server on the same machine. For a server with a privately signed certificate pass `--ca-cert ca.pem`.

In CI, skip the saved sign-in and set `DEDIE_SERVER` and `DEDIE_TOKEN` (a token created on the Account page).

**On the server host itself**, without a sign-in. The CLI then works directly on the data directory, which is how an installation is created and how it is recovered when nobody can sign in:

```sh
dedie init --org "Acme" --name "Ada Lovelace" --email ada@example.com
dedie user add --name "Kofi Mensah" --email kofi@example.com --role operator
dedie audit verify
dedie --local conn list     # --local ignores a saved sign-in
```

In this mode the CLI acts with owner rights, because anyone who can read the data directory and master key already has that access. Its actions are audited under the operating-system user name.

Passwords are prompted for, or read from `DEDIE_PASSWORD`; they are never accepted as flags, where they would end up in shell history and the process list. Run `dedie help` for every command.

## The desktop application

Dedie Desktop is a JavaFX application. It starts the same `dedie-server` engine on a free loopback port, which nothing outside the machine can reach, and shows the interface in its own window. There is no second implementation of anything: queries, migrations, backups and access control behave exactly as they do on a server.

```sh
make desktop
java -jar bin/desktop/dedie-desktop.jar
```

`bin/desktop` is self-contained: the application, its JavaFX libraries and the server program. Closing the window stops the engine cleanly; a running migration is handed back to the queue and resumes the next time the application starts.

The application looks for `dedie-server` beside its own jar, then in a `bin` directory above the working directory, then on the `PATH`. Set `DEDIE_SERVER` to point at it explicitly. During development, `cd apps/desktop && mvn javafx:run` starts it from the source tree.

The window has a native menu bar: **File** starts a new connection, migration, offline migration or continuous sync; **Go** jumps to any page (Ctrl/Cmd+1 to 8), to the notifications, or to anything by name (Ctrl/Cmd+K); **View** zooms and switches to full screen; and **Help** opens the two guides, the engine's log and the data folder. Notifications are shown through the system tray where the desktop has one. Exports and backup downloads open a save dialog, and the window remembers its size, position and zoom.

The engine's log is written to `desktop.log` in the data directory. If the engine stops, the window says so and offers to start it again.

## Migrating a database

```sh
dedie migrate create --name "shop 16 to 18" --source prod-16 --target prod-18 --fence
dedie migrate preflight <id>     # changes nothing; lists everything that would block or weaken the run
dedie migrate run <id>           # or `dedie migrate start <id>` to let a running dedie-server execute it
dedie migrate status <id>
dedie migrate cutover <id>       # when lag is near zero and you are ready to switch
```

Supported pairs are PostgreSQL → PostgreSQL, MySQL → MySQL, Oracle → Oracle, MySQL → PostgreSQL, PostgreSQL → MySQL, Oracle → PostgreSQL, Oracle → MySQL and MongoDB → PostgreSQL. The target must not already contain the tables being migrated. What each source needs beforehand is set out, with the statements to run, in [Preparing your databases](docs/databases.md).

A migration runs in one of three ways:

| Purpose | Flag | What it does |
|---|---|---|
| Online (the default) | none | copies the source, follows every later change, and ends with a cutover you ask for |
| [Offline](#offline-migration) | `--offline` | copies the source once, validates and finishes by itself |
| [Continuous sync](#continuous-sync) | `--sync` | like online, but is not expected to end |

Against a Dedie server, `dedie migrate run` starts the job on the server and follows its progress; Ctrl+C stops watching without stopping the migration.

### Levels

| Level | How to ask for it | What moves |
|---|---|---|
| Database | nothing extra | Every schema and table in the source connection's database |
| Schema | `--schemas public,billing` | Everything in the listed schemas (PostgreSQL and Oracle; in MySQL a schema is the database) |
| Table | `--tables public.orders,public.order_items` | Only the listed tables, with their indexes, constraints and triggers. Views and other standalone objects are left out |

`--exclude` removes tables at any level. PostgreSQL and Oracle tables are named `schema.table`; MySQL tables by name alone. For Oracle the database level means the connecting user's own schema, and names are matched without regard to case, as Oracle does. Preflight fails if a listed table does not exist.

### Reading the schema before you run

**Preview schema** on a migration's page, or `dedie migrate preview <id>`, shows every statement the migration would run on the target, in the order it would run them, with the source's tables already translated for the target engine. Nothing is run and neither database is changed.

```sh
dedie migrate preview <id> > target-schema.sql      # attach it to the change request
```

The statements are grouped by when they run: before the copy (schemas, sequences, tables), after it (indexes, constraints, views, routines) and at the very end (foreign keys on the Oracle paths, triggers on the same-engine ones). All eight paths have a preview; for Oracle to Oracle it shows the definitions the source writes for its own objects, rewritten for the schema that receives them, with each program unit closed by a `/` so that the script runs in SQL*Plus or SQLcl as it is. The preview follows the choice made for [objects that already exist](#when-the-target-already-has-some-of-the-objects): skipped objects are left out, and replaced ones are preceded by their `DROP`. What the job does for its own bookkeeping (its checkpoint table, emptying its own tables before a recopy, moving sequences to the source's position at the end) is not part of it.

### When the target already has some of the objects

Preflight looks on the target for everything the migration would create: tables, and also views, functions, procedures, packages, triggers, sequences, types, events and indexes, whichever of them the path carries. Whatever it finds is listed by kind, and the migration does nothing about it without a decision from you. The web interface asks as soon as preflight finds any; so does `dedie migrate preflight` on a terminal.

| Choice | What the migration does |
|---|---|
| **Refuse** (the default) | Does not start. Preflight fails and names the objects. Nothing on the target is touched |
| **Keep them and skip them** | Leaves each one exactly as it is. An existing table is left out of the migration: no rows are copied into it and changes to its source are not followed. An existing view, routine or sequence stays the target's own version, and its sequence position is not moved |
| **Replace them** | Drops each one on the target and creates it again from the source. A dropped table loses the data it held there, along with whatever depended on it. The web interface and the command line both ask you to type `REPLACE` first |

```sh
dedie migrate create ... --on-existing skip        # decide when creating the migration
dedie migrate existing <id> replace                # or afterwards, without a prompt (for scripts)
```

The choice is recorded in the audit log, and can be made only before the migration has created anything; once it has, the objects on the target are its own. A schema that already exists is not a clash: it is reused, and whatever else it holds is left alone. The target database itself is never created or dropped by a migration.

### Phases

1. **Schema.** Tables and the objects they depend on are recreated on the target from the source's own definitions.
2. **Snapshot.** Every table is copied in parallel from one consistent point in time, and the replication position of that point is recorded. The change stream begins exactly there, so nothing falls between the copy and the stream.
3. **Indexing.** Objects that are cheaper or only possible to build after the bulk load: secondary indexes, foreign keys and views on PostgreSQL; views and stored routines on MySQL.
4. **Streaming.** Each source transaction is applied to the target as one transaction, together with its position in a checkpoint table. If the process dies, it resumes from that checkpoint without losing or repeating a change. A job can stay in this phase for as long as you like.
5. **Cutover.** With `--fence`, the source database is made read-only. The stream is drained to the end, sequences and auto-increment counters are synchronized, every table is compared between source and target (row count plus checksum), triggers are created, and whatever the migration left on either server is removed.

Point your application at the target once cutover completes. Applications are unable to write only during phase 5.

If validation finds a difference, cutover is cancelled, the source is made writable again and streaming continues. Nothing is switched on a mismatch.

Triggers are created at cutover on purpose: created earlier, they would fire for every replicated row and repeat effects the source has already produced.

### PostgreSQL

Requirements on the source:

- `wal_level = logical` (needs a server restart if it is not already set)
- a user with the `REPLICATION` attribute that owns the tables, or a superuser
- a free replication slot and WAL sender
- a primary key or replica identity on every migrated table. A table without one cannot be included, because publishing it would make updates and deletes fail on the source

Not migrated, and reported by preflight so nothing is dropped silently:

- roles, ownership and `GRANT`s
- row-level security policies, rewrite rules, event triggers, user-defined aggregates
- large objects and foreign tables
- partitioned tables, inheritance children and unlogged tables (skipped, with a warning)

Notes:

- A paused or failed job keeps its replication slot, and the source keeps WAL for it. Resume or abort jobs you are not going to finish; the server console highlights slots that retain WAL.
- Fencing sets the database read-only for new sessions and disconnects existing clients. To undo it: `ALTER DATABASE <name> RESET default_transaction_read_only;`

### MySQL

Requirements on the source:

- binary logging on, `binlog_format = ROW`, `binlog_row_image = FULL`
- an account with `SELECT`, `RELOAD`, `REPLICATION SLAVE` and `REPLICATION CLIENT`
- InnoDB tables. Tables on other storage engines are skipped with a warning, since they cannot be read at one consistent point without locking

The snapshot takes a global read lock only for the moment it needs to read the binary log position and open its read transactions, typically a few milliseconds.

Not migrated: user accounts and privileges. Views, routines, triggers and events are created under the migrating account rather than their original `DEFINER`.

Notes:

- The connection's database is what gets migrated; name the same database on the target connection, or a different one to rename it on the way.
- Tables without a primary key are supported, but each change is matched on every column, which is slow on large tables. Preflight lists them.
- Fencing uses `ALTER DATABASE ... READ ONLY = 1` and needs MySQL 8.0.22 or newer on the source. The switch waits at most 20 seconds for transactions in its way; if it cannot get through, cutover is cancelled rather than stalling the source. To undo it: `ALTER DATABASE <name> READ ONLY = 0;`
- A paused job can resume for as long as the source still has the binary logs it needs. Check `binlog_expire_logs_seconds` before pausing for long.
- MariaDB is not supported by this pipeline.

### MySQL to PostgreSQL

The source side is read exactly as for MySQL → MySQL, so the same source requirements apply. The target database must use UTF-8. Tables are created in the target's `public` schema.

How the schema is translated:

| MySQL | PostgreSQL |
|---|---|
| `TINYINT`, `SMALLINT`, `MEDIUMINT`, `INT`, `BIGINT` | the smallest integer type that holds every value; unsigned types move up one size, and `BIGINT UNSIGNED` becomes `numeric(20,0)` |
| `TINYINT(1)` | `smallint`, not `boolean`, so no value is lost |
| `DECIMAL(p,s)`, `FLOAT`, `DOUBLE` | `numeric(p,s)`, `real`, `double precision` |
| `CHAR(n)`, `VARCHAR(n)`, text types | `varchar(n)`, `text` |
| `BINARY`, `VARBINARY`, BLOB types, spatial types | `bytea` |
| `DATE`, `DATETIME`, `TIME`, `YEAR` | `date`, `timestamp`, `time`, `smallint` |
| `TIMESTAMP` | `timestamptz` (the value is a point in time, read in UTC) |
| `JSON` | `jsonb` |
| `ENUM` | `text` with a `CHECK` constraint listing the allowed values |
| `SET`, `BIT(n)` | `text`, `bigint` |
| `AUTO_INCREMENT` | `GENERATED BY DEFAULT AS IDENTITY`, restarted past the source's counter at cutover |

Primary keys, unique and secondary indexes, and foreign keys with their `ON UPDATE` and `ON DELETE` rules are recreated. Text is converted to UTF-8 from `utf8mb4`, `utf8`, `latin1` and `ascii`; other character sets fail preflight.

What changes in meaning, all of it listed by preflight for your schema before anything runs:

- Text comparison is case-sensitive in PostgreSQL, while most MySQL collations are not. Unique keys and lookups that relied on case-insensitive matching behave differently.
- Zero dates (`0000-00-00`) do not exist in PostgreSQL and become `NULL`.
- Generated columns become ordinary columns holding the same values.
- `ON UPDATE CURRENT_TIMESTAMP`, expression defaults, full-text, spatial and functional indexes are not translated.
- Views, stored routines and triggers are written in MySQL's dialect; see [Views and stored code across engines](#views-and-stored-code-across-engines) for what is converted. Scheduled events are not.

Validation at cutover compares row counts and, for tables with an integer or UTF-8 text primary key, the set of key values. Whole rows cannot be checksummed across the two engines, because they print many types differently.

### PostgreSQL to MySQL

The source side is read exactly as for PostgreSQL → PostgreSQL, so the same source requirements apply. The target must be MySQL 8.0 or newer. Tables are created in the target connection's database with the `utf8mb4_bin` collation, so text compares by exact bytes as it does in PostgreSQL.

MySQL has no schemas inside a database. With one source schema, tables keep their names; with several, tables outside `public` are named `schema_table`. Two tables that would end up with the same name fail preflight.

How the schema is translated:

| PostgreSQL | MySQL |
|---|---|
| `smallint`, `integer`, `bigint` | `SMALLINT`, `INT`, `BIGINT` |
| `numeric(p,s)`, `numeric` | `DECIMAL(p,s)`, `DECIMAL(65,20)` |
| `real`, `double precision` | `FLOAT`, `DOUBLE` |
| `boolean` | `TINYINT(1)` holding 0 or 1 |
| `varchar(n)`, `char(n)` | `VARCHAR(n)`, `CHAR(n)` |
| `text` | `LONGTEXT`, or `VARCHAR(255)` when the column is part of a key or index |
| `bytea` | `LONGBLOB` |
| `date`, `time`, `timestamp` | `DATE`, `TIME(6)`, `DATETIME(6)` |
| `timestamptz` | `DATETIME(6)` holding UTC |
| `uuid` | `CHAR(36)` |
| `json`, `jsonb` | `JSON` |
| enum types | `ENUM` with the same labels |
| arrays | `JSON` arrays |
| serial and identity columns | `AUTO_INCREMENT`, continued past the source sequence at cutover |

Primary keys, unique and secondary indexes, and foreign keys with their rules are recreated. Foreign-key checks are off on the connection that applies changes, because PostgreSQL replicates the rows changed by a cascade itself.

What changes or is left out, all of it listed by preflight for your schema:

- Generated columns are not migrated; their values are not part of PostgreSQL's replication stream.
- Partial, expression, GIN and GiST indexes, check constraints, and defaults that are expressions are not translated.
- Values MySQL cannot store stop the migration rather than being altered: infinite dates, `NaN`, and text longer than 255 characters in a key column.
- Views, functions, procedures and triggers are converted where that is safe; see [Views and stored code across engines](#views-and-stored-code-across-engines). Materialized views and custom types are not migrated.
- `TRUNCATE` commits on its own in MySQL, so a source transaction that truncates one table and changes others is not applied atomically.

Validation at cutover compares row counts and, for tables with an integer, text or UUID primary key, the set of key values.

### MongoDB to PostgreSQL

Requirements on the source:

- a replica set member (a single node started with `--replSet` and initiated counts). Changes made during the copy are followed through the change stream, which a standalone server does not have
- MongoDB 4.0 or newer
- an account, looked up in the `admin` database, that can read the database and open a change stream on it

Each collection becomes a table in the target's `public` schema with two columns:

| Column | Holds |
|---|---|
| `_id text PRIMARY KEY` | the document's key: an ObjectId as its 24 hex characters, a string or number as written, anything else as JSON |
| `doc jsonb NOT NULL` | the whole document, in MongoDB's relaxed Extended JSON |

Nothing in a document is dropped or reinterpreted, and documents of different shapes sit side by side. Fields are read from SQL with the `jsonb` operators:

```sql
SELECT _id, doc->>'email', doc->'address'->>'city' FROM users WHERE (doc->>'age')::int > 30;
CREATE INDEX ON users ((doc->>'email'));
```

Types JSON has no form for keep their Extended JSON wrapper, so a date is `{"$date": "2024-02-29T13:14:15Z"}` and a 128-bit decimal is `{"$numberDecimal": "..."}`.

How it stays correct: the cluster time is recorded before the copy begins and the change stream starts there. Every change is applied as "store this document" or "delete this key", so a change the copy already included is harmless to apply again. The stream's resume token is committed on the target in the same transaction as the changes it covers.

What to know:

- **Typed columns.** Fields can be lifted out of the document into columns of their own, for joins, indexes and tools that do not speak JSON:

  ```sh
  dedie migrate create --name "shop docs" --source events --target warehouse \
    --columns "users:email:text,users:address.city:text,users:joined:timestamptz,orders:total:numeric"
  ```

  The table for `users` then has `_id`, `doc`, `email`, `address_city` and `joined`. Types are `text`, `bigint`, `double precision`, `numeric`, `boolean`, `timestamptz` and `jsonb`. Columns are filled during the copy and kept current by the stream. A value whose type does not fit its column exactly (a string in a `bigint` column) is stored as `NULL` there and counted; nothing is guessed, and `doc` always holds the original. In the web interface, **Suggest from a sample** reads a few hundred documents of each collection and proposes the fields whose values share one simple type; review the proposal before using it.
- A level is the whole database or selected collections (`--tables orders,customers`).
- Collections created while the job runs are picked up and get their own table.
- Indexes, validators and views are not translated. Dropping or renaming a collection on the source is reported and leaves the target table as it is.
- `jsonb` cannot store the null character; it is removed from the few strings that contain one, and each case is counted and reported.
- Two documents whose keys read the same as text, such as the number `5` and the string `"5"`, would share a row. Validation catches this as a count mismatch.
- The source cannot be fenced. Stop the applications that write to it before cutting over.
- Validation compares document counts and the set of keys, not document contents.
- A paused job can resume for as long as the source's oplog still reaches back to where it stopped.

### Oracle

Dedie connects to an Oracle **service**, which on a multitenant server is the service of the pluggable database (`FREEPDB1`, `ORCLPDB1`); it goes in the connection's database field. No Oracle client is installed: Dedie speaks the protocol itself. Sources from Oracle 12c (12.1) onwards are accepted.

```sh
dedie conn add --kind oracle --name erp --host ora.internal --db ORCLPDB1 --user migrator
dedie conn add --name erp-pg --host pg.internal --db erp --user migrator --sslmode verify-full
dedie migrate create --name "ERP to PostgreSQL" --source erp --target erp-pg --schemas HR,SALES
```

All three Oracle paths read the source the same way.

**The copy** reads every table with a flashback query as of one system change number (SCN), so the tables are consistent with each other and no lock is held. It uses undo data: if copying a table takes longer than `UNDO_RETENTION`, Oracle answers "snapshot too old" and the retention has to be raised.

**Following changes** means finding the rows that committed transactions touched after the copy. Every such row is then read back from the source by primary key and stored whole on the target; a row that is no longer there is deleted. An insert, an update and a delete are therefore the same instruction, which has three consequences worth knowing:

- large objects and every data type take exactly the path they took in the copy; nothing depends on how a change is described
- applying a change twice is harmless, so a restart simply reads again from the checkpoint, which is committed on the target together with the rows it covers
- rows are applied table by table rather than statement by statement, so foreign keys are created at the very end, after validation

Dedie has two ways of finding the changed rows and picks between them by itself. Preflight says which a job will use, under **Following changes**.

| | The redo stream, with LogMiner | Row versions, from undo data |
|---|---|---|
| Needs on the source | minimal supplemental logging; an account with `LOGMINING`, `SELECT ANY TRANSACTION`, `EXECUTE_CATALOG_ROLE` and `SELECT ANY DICTIONARY`; redo that stays readable, which in practice means `ARCHIVELOG` mode | nothing beyond what the copy needs |
| Cost | reads only what changed | reads the primary key index of every followed table on each pass, so a large or busy source is followed less closely |
| Used for | every table, when the source is set up for it | tables with a `JSON` or `BOOLEAN` column, which LogMiner does not describe; and every table when LogMiner is not set up, not allowed through the connection (a pluggable database on 12c), or has lost its redo |

The last case matters on a source that does not archive its redo logs. From inside a pluggable database LogMiner can then read only the current online log, and a log switch puts earlier changes out of its reach. A job in that position does not stop: it carries on from its last checkpoint through row versions, records the switch in its events, and loses nothing. The one limit of row versions is `UNDO_RETENTION`: a job paused or behind for longer than the source keeps undo has to start again with a new copy.

The statements that set up the redo stream are in [Preparing your databases](docs/databases.md#oracle-as-a-source). An [offline](#offline-migration) migration needs none of this. Every followed table needs a primary key: a table without one is copied, and preflight warns that later changes to it are not followed.

When a job uses the redo stream it switches on primary key supplemental logging for the tables it follows as it starts, and switches it off again when it finishes (`--keep-slot` leaves it on). That is a brief `ALTER TABLE`, which waits up to 30 seconds for transactions holding the table.

Notes:

- The source cannot be fenced. Stop the applications that write to it before asking for cutover.
- Inside a pluggable database of a current release LogMiner finds its redo logs by itself. In a database that is not a container, and on 12c, Dedie registers the online and archived logs that cover the range it needs.
- A table truncated or altered while it is followed through row versions has all of its rows brought over again; the target table keeps the shape it was created with.
- Validation compares row counts and, where the primary key is a number, text or date, the set of keys.
- A time zone given by region name (`Europe/Paris`) arrives as its offset at that instant, and a date before the common era loses its era.
- The paths are tested end to end against Oracle 23ai. A 12c server has not been part of the test runs; run preflight and a trial migration of a copy first.

### Oracle to Oracle

This is the upgrade path: from 12c or newer to the same or a newer release, on another server or in another pluggable database. The source side is as described under [Oracle](#oracle). Nothing is translated: every definition is taken from the source itself with `DBMS_METADATA`, so tables arrive with their partitioning, identity and virtual columns, and function-based indexes, and stored code arrives as written.

| When | What is created |
|---|---|
| Before the copy | sequences and tables |
| After the copy | types, functions, procedures, packages, views and synonyms; then indexes; then primary key, unique, check and `NOT NULL` constraints; then comments |
| At the end, after validation | sequence and identity positions, foreign keys, triggers, `GENERATED ALWAYS` restored on identity columns; invalid objects are compiled and whatever stays invalid is reported |

Triggers and foreign keys wait until the end so that neither fires nor rejects while rows are still arriving. A table and its primary key must be created; anything else that fails is reported with Oracle's message and the job goes on.

Where the objects go:

- A source schema goes to the schema of the same name on the target, which must already exist: Dedie creates no users. The target account must be that schema or hold the `ANY` privileges (`CREATE ANY TABLE` and its companions).
- When exactly one schema is migrated and that is not possible, its objects go into the target connection's own schema, and references to the old schema name in the definitions are rewritten. Preflight states the mapping it will use. A reference a person wrote without quotes inside PL/SQL or a view (`hr.employees`) is not rewritten; preflight lists the objects that contain one.

What to know:

- The target must be the same release or newer. An older target fails preflight; the same release is reported as a move, not an upgrade.
- Physical attributes (tablespaces, storage clauses, LOB storage, compression) are not carried over; the target's defaults apply.
- Values are carried exactly: `NUMBER` to full precision, `TIMESTAMP` to the nanosecond, large objects of any size, and the difference between an empty large object and `NULL`. The end-to-end tests compare every row byte for byte.
- A `LONG` or `LONG RAW` value longer than 4000 bytes (32767 with extended strings) fails the job rather than being cut short; preflight names such columns. Convert them to `CLOB` or `BLOB` first.
- Not migrated: users, roles, grants, profiles, tablespaces, database links, scheduler jobs, queues, public synonyms, materialized views, external tables, and schema- or database-level triggers. Preflight counts what it found of each.
- A table-level migration takes the listed tables with their indexes, constraints and triggers, and the schema's sequences; views, stored code and synonyms are left out.
- To read definitions in schemas other than its own, the source account needs `SELECT_CATALOG_ROLE`.
- While a job runs, a small table named `DEDIE_CHECKPOINT` exists in the receiving schema; it is dropped when the job ends.
- Tested from one schema to another on Oracle 23ai. A 12c source and a cross-release upgrade have not been part of the test runs.

### Oracle to PostgreSQL

Each Oracle schema becomes a PostgreSQL schema. Names that Oracle stores in upper case, which is every name written without quotes, become lower case, since that is what an unquoted name means in each database: `HR.EMPLOYEES.FIRST_NAME` is `hr.employees.first_name`. A name created with quotes and lower-case letters is kept exactly.

| Oracle | PostgreSQL |
|---|---|
| `NUMBER(p)` up to 4, 9, 18 digits | `smallint`, `integer`, `bigint` |
| `NUMBER(p)` beyond 18 digits, `NUMBER(p,s)` | `numeric(p,s)` |
| `NUMBER` without precision, `FLOAT` beyond 53 bits | `numeric` |
| `FLOAT` up to 53 bits, `BINARY_DOUBLE` | `double precision` |
| `BINARY_FLOAT` | `real` |
| `VARCHAR2(n)`, `NVARCHAR2(n)` | `varchar(n)`, counted in characters |
| `CHAR(n)`, `NCHAR(n)` | `char(n)` |
| `CLOB`, `NCLOB`, `LONG` | `text` |
| `BLOB`, `RAW`, `LONG RAW` | `bytea` |
| `DATE` | `timestamp(0)`: an Oracle `DATE` has a time of day |
| `TIMESTAMP(n)` | `timestamp(n)`, at most 6 digits; finer values are rounded to the microsecond |
| `TIMESTAMP WITH TIME ZONE`, `WITH LOCAL TIME ZONE` | `timestamptz`: the instant is kept, the zone it was entered in is not |
| `INTERVAL YEAR TO MONTH`, `DAY TO SECOND` | `interval` |
| `XMLTYPE`, `JSON`, `BOOLEAN` | `xml`, `jsonb`, `boolean` |
| `ROWID`, `UROWID` | `text` |

Also carried over:

- **Identity columns**, as `GENERATED BY DEFAULT AS IDENTITY` on a `bigint`, and **sequences**, both moved past the source's position at the end so the target never hands out a number the source used.
- **Defaults** that mean the same in both: literals, `SYSDATE` and `SYSTIMESTAMP`, `SYS_GUID()`, `USER`, and `sequence.NEXTVAL`. Any other expression is listed by preflight and left out.
- **Indexes**, including the unique indexes behind unique constraints. A bitmap index becomes a B-tree; a function-based index is listed and left out.
- **Check constraints** as written. Most are plain comparisons that mean the same; one that calls an Oracle-only function fails to apply and is reported.
- **Foreign keys** with their `ON DELETE` rule and deferrability. A column that refers to a key takes the key's type, since PostgreSQL will not tie a `numeric` column to an `integer` key.

What to know:

- Views, and PL/SQL functions, procedures and triggers, are converted where that is safe; see [Views and stored code across engines](#views-and-stored-code-across-engines). Not translated: packages, object types, materialized views, synonyms, database links and partitioning. A partitioned table arrives as one ordinary table. Users, roles and grants are not copied.
- An empty string is `NULL` in Oracle and a value of its own in PostgreSQL. The data arrives as `NULL`; an application that writes `''` will now store it.
- Text comparison follows the target database's collation, not the source's `NLS_SORT`.
- Column types that cannot be migrated yet (spatial, object and collection types, `BFILE`) fail preflight, which names the columns; exclude their tables.
- The character `NUL`, which PostgreSQL text cannot hold, is removed from the strings that contain it.

### Oracle to MySQL

The source side is as described under [Oracle](#oracle). MySQL has no schemas inside a database, so every table goes into the target connection's database, with upper-case names in lower case. Preflight fails if two source schemas hold a table of the same name, or if names differ only in case on a server that ignores case.

Requirements on the target: MySQL 8.0 or newer (MariaDB is refused), a connection that names a database, and `utf8mb4` as that database's character set.

| Oracle | MySQL |
|---|---|
| `NUMBER(p)` up to 2, 4, 9, 18 digits | `tinyint`, `smallint`, `int`, `bigint` |
| `NUMBER(p)` beyond 18 digits, `NUMBER(p,s)` | `decimal(p,s)` |
| `NUMBER` without precision | `decimal(65,30)`: exact to 35 digits before the point and 30 after |
| a `NUMBER` wider than `decimal` allows, `FLOAT`, `BINARY_DOUBLE` | `double` |
| `BINARY_FLOAT` | `float` |
| `VARCHAR2(n)`, `NVARCHAR2(n)` | `varchar(n)`; `text` beyond 16383 characters |
| `CHAR(n)`, `NCHAR(n)` | `char(n)`; `varchar(n)` beyond 255 |
| `CLOB`, `NCLOB`, `LONG`, `XMLTYPE` | `longtext` |
| `RAW(n)` | `varbinary(n)` |
| `BLOB`, `LONG RAW` | `longblob` |
| `DATE` | `datetime` |
| `TIMESTAMP(n)` | `datetime(n)`, at most 6 digits |
| `TIMESTAMP WITH TIME ZONE`, `WITH LOCAL TIME ZONE` | `datetime(6)` holding UTC: the instant is kept, the zone is not |
| `INTERVAL YEAR TO MONTH`, `DAY TO SECOND` | `varchar(64)` holding ISO 8601 text, such as `P2Y6M` |
| `JSON`, `BOOLEAN` | `json`, `tinyint(1)` |
| `ROWID`, `UROWID` | `varchar(18)`, `text` |

Also carried over: identity columns as `AUTO_INCREMENT`, with the counter moved past the source's at the end; defaults that mean the same (literals, `SYSDATE`, `SYSTIMESTAMP`, `SYS_GUID()`, `USER`); indexes; check constraints, on MySQL 8.0.16 or newer; and foreign keys, added at the very end. A column that refers to a key takes the key's type.

What to know:

- **Sequences are not migrated**: MySQL has none. A default that calls `sequence.NEXTVAL` is dropped and listed by preflight; an identity column is the equivalent that does carry over.
- Views, and PL/SQL functions, procedures and triggers, are converted where that is safe; see [Views and stored code across engines](#views-and-stored-code-across-engines). MySQL's procedure language is the most limited of the three, so less converts here than on any other path. Not translated: packages, object types, materialized views, synonyms, database links and partitioning. Users, roles and grants are not copied.
- A table whose declared column lengths pass MySQL's 65,535-byte row limit has its longest `varchar` and `varbinary` columns stored as `text` and `blob` instead, and preflight says which.
- An index over a `text`, `blob` or `json` column, or with a key longer than 3072 bytes, is listed and left out. A primary key that long fails preflight. Function-based and descending indexes are not translated.
- Tables are created with a binary `utf8mb4` collation, so text compares by exact value, as it does in Oracle, and not without regard to case as MySQL's default would.
- `NaN` and infinity, which MySQL cannot store, stop the migration, as does a row larger than the server's `max_allowed_packet`.
- A `NUMBER` without precision shows all 30 decimal places in MySQL (`5.000000000000000000000000000000`); it is the same number. Declare a precision on the source first if that matters to you.
- A `CHAR` value comes back without its trailing blanks.
- MySQL names foreign keys and check constraints per database, not per table, so one can clash with a constraint of an unrelated table already there; preflight lists such clashes with the other [objects that already exist](#when-the-target-already-has-some-of-the-objects).
- Validation compares row counts and, where the primary key is a number, text or date, the set of keys.

### Views and stored code across engines

Between two servers of the same kind, views, procedures, functions and triggers are carried over as they are. Between different kinds they are written in different languages (PL/SQL, PL/pgSQL, MySQL's stored program language) and have to be rewritten. Dedie rewrites what it can do safely, says plainly what it could not, and hands you the rest in a form to work from. This applies to MySQL → PostgreSQL, PostgreSQL → MySQL, Oracle → PostgreSQL and Oracle → MySQL.

Nothing here can fail a migration. All of it happens at the very end, after the tables have been validated, so a view never stands on the target unchecked and a trigger never fires for a row that was copied or replicated.

**Views** are translated, created, and then compared with the source: the same columns, the same number of rows, and for each column the same count of values, the same sum where it is a number and the same total length where it is text. A view that the target refuses, or that answers differently, is dropped again and reported. A view using something with no safe translation (window functions, common table expressions, `CONNECT BY`, a function Dedie does not know) is not attempted, and the reason is given. The comparison can only see differences that the rows present at that moment expose.

**Procedures, functions and triggers** are parsed and rewritten, and each ends up in one of three states:

| State | Meaning | What happens |
|---|---|---|
| Converted | Everything in it was recognised and has a rewrite that keeps its behaviour | Created on the target |
| Needs review | Converted, but something in it can behave differently on the target; the notes say what and on which line | Not created unless you ask for it. You get the converted draft and the notes |
| Not converted | It uses something the target has no equivalent for, and the reason is given | Never created. You get the original source |

Typical reasons for review: an Oracle empty string, which is `NULL` there; division that is not rounded; text comparison, which is case-insensitive under most MySQL collations and exact in PostgreSQL; an error handler that runs after the block changed data. Typical reasons for no conversion: Oracle packages, autonomous transactions, `MERGE`, `ROWNUM`, `CONNECT BY`, dynamic SQL and collections; MySQL session variables, prepared statements and procedures that return result sets; PostgreSQL set-returning functions, arrays and statement-level triggers. The full list per route is in the documentation of package `engine/migrate/codesql`.

The converter is tested by running the code, not by reading it: each route has a set of routines that are created on the source, converted, created on the target and called with the same inputs on both, and their results and the rows they leave behind must be equal. That shows the rewrites are right for those routines. It does not show that yours do the same thing on the target as on the source: test converted code before you rely on it, as you would code a colleague had ported.

How much stored code a migration creates is chosen per migration, as **Stored procedures, functions and triggers** in the form or `--code` on the command line:

| Choice | Created on the target |
|---|---|
| `converted` (the default) | what converted cleanly |
| `review` | that, and what needs review; those are listed as "created, needs review" |
| `off` | nothing; views are still attempted |

```sh
dedie migrate create --name "ERP to PostgreSQL" --source erp --target erp-pg --code review
dedie migrate preflight <id>       # "Views" and "Stored code": what will be created, what needs review, what cannot be converted
dedie migrate preview <id>         # the converted statements themselves
dedie migrate leftovers <id> > to-convert.sql
```

**Not migrated** on a migration's page, or `dedie migrate leftovers`, is the worklist for what remains: every view, routine and trigger that was not created, with its reason, its original source and, where there is one, the converted draft with its notes. Once the job has finished, the migration's page also shows the outcome under **Views and stored code**: how many were created, how many were created and need review, and how many are left for you.

What to know:

- A table-level migration converts the triggers of its tables and the views that read only its tables; other routines are left out, and preflight says so.
- A trigger that is disabled on the source is not created.
- On a MySQL target a PostgreSQL or Oracle trigger for several events becomes one trigger per event, named with `_ins`, `_upd` and `_del`.
- A MySQL target that writes a binary log refuses functions that change data unless `log_bin_trust_function_creators` is on; such a function is reported with that explanation.
- [Objects that already exist](#when-the-target-already-has-some-of-the-objects) on the target under the same names are handled by the choice made for the migration, like tables.
- Comments inside routines are not carried over.

### For all pipelines

- Schema changes made on the source while a migration is running are not replicated. Freeze the schema until cutover. `TRUNCATE` is replicated.
- Every pipeline also runs [offline](#offline-migration), as a one-time copy that needs no replication set up on the source.
- Aborting removes what the migration created on the source and leaves the copied tables on the target for inspection.
- With `--fence` the source stays read-only after a successful cutover, which prevents stray writes to the old database.
- Fencing stops well-behaved applications. It is not a security control.
- Checksum validation reads every row on both sides during the write pause. For very large databases choose `--validation count`.

## Offline migration

An offline migration is a one-time copy. Choose **Offline: copy once** when creating it, or pass `--offline`:

```sh
dedie migrate create --name "shop to staging" --source prod --target staging --offline
dedie migrate preflight <id>
dedie migrate run <id>       # schema, copy, indexes, validation; then it is done
```

It runs the schema, snapshot and indexing phases of an online migration, skips streaming, and goes straight to what an online migration does at cutover: counters are synchronized, every table is compared, and the job completes. There is no cutover to ask for.

- **The source needs no replication set up.** No `wal_level = logical` or replication slot on PostgreSQL, no binary log on MySQL, no replica set on MongoDB, no supplemental logging or LogMiner on Oracle. An account that can read the tables is enough, which makes this the way to migrate a database you are not allowed to reconfigure.
- **All tables are read as of one moment**: an exported snapshot on PostgreSQL, consistent-snapshot transactions on MySQL, flashback queries at one SCN on Oracle. MongoDB has no such snapshot that outlasts a long copy, so its collections are read one after another and the source should be kept still.
- **Changes made to the source after the copy starts are not carried over.** Stop the applications that write to it first if the target must end up identical.
- Validation tells you if that rule was broken. From Oracle the target is compared with the source as it was at the snapshot, so later writes do not fail the job. On the other sources it is compared with the source as it is now, and a difference fails the job with a message saying so; empty the target and create the migration again once writes have stopped.
- Tables without a primary key, which an online PostgreSQL migration must leave out, are copied.
- Pausing during the copy starts it over on resume, from a new snapshot.

## Continuous sync

A sync is a migration that is not expected to end. Choose **Keep in sync continuously** when creating it, or pass `--sync`:

```sh
dedie migrate create --name "orders to reporting" --source orders --target warehouse --sync
dedie migrate start <id>
```

The target is filled, then every change on the source is applied to it for as long as the job runs. It works on every supported pair, so a MySQL database can feed a PostgreSQL reporting copy, or MongoDB collections can be kept queryable in SQL.

- It is one-directional. Do not write to the synced tables on the target; a later change from the source would overwrite or collide with what you wrote.
- Monitoring raises an alert when a sync falls more than 512 MB behind its source.
- **Finish sync** drains the remaining changes, validates the target and ends the job, leaving an independent copy. The source is never fenced.
- The source keeps replication data for a sync that is paused or has failed. Resume or abort it; do not leave it.

## Backups

Every backup is described by three things, and the Backups page shows all three for each one:

- **Method**: *logical* or *physical*. A logical backup is a description of the data (`pg_dump`, `mysqldump`) that any server of the same kind can load, whole or a table at a time, into another database, server or newer version. A physical backup is a copy of the server's own data files (`pg_basebackup`): fast for large servers and restored as a whole server of the same major version.
- **Level**: *full*, *differential* or *incremental*.
- **Where it is stored**, and whether the file is encrypted.

| Level | Holds | To restore you need |
|---|---|---|
| Full | everything | this backup |
| Differential | what changed since the last full backup | the full backup and this one |
| Incremental | what changed since the last backup of any level | the full backup and every backup after it, in order |

Differential and incremental backups are available where the database can produce them reliably:

| | Differential and incremental | Requirements |
|---|---|---|
| PostgreSQL, physical | yes, with `pg_basebackup --incremental` | PostgreSQL 17 or newer with `summarize_wal = on`, set before the full backup is taken |
| PostgreSQL, logical | no: a logical PostgreSQL backup is always full | |
| MySQL, logical, whole database | yes, as the binary log written since the backup they build on | binary logging on; the account needs `RELOAD`, `REPLICATION CLIENT` and `REPLICATION SLAVE` |
| MySQL, structure-only, data-only or selected tables | no | |

Dedie works out which backup a differential or incremental one builds on, restores the whole chain in order when you restore one, and refuses to delete a backup that later ones depend on.

Backups are compressed and stored with a SHA-256 of the file.

| `--type` | Method | Contains | PostgreSQL | MySQL |
|---|---|---|---|---|
| `full` | logical | structure and data of the database | `pg_dump` custom format | `mysqldump`, gzip |
| `schema` | logical | structure only: tables, indexes, views, routines | yes | yes |
| `data` | logical | rows only, for a database that already has the structure | yes | yes |
| `tables` | logical | structure and data of the tables you choose | yes | yes |
| `physical` | physical | a copy of the server's data files, covering every database | `pg_basebackup`, needs the `REPLICATION` attribute | the server's clone plugin; see below |

```sh
dedie backup create prod-18 --note "before release"
dedie backup create prod-18 --type tables --tables public.orders,public.order_items
dedie backup create prod-18 --type physical
dedie backup create prod-18 --type physical --level incremental     # since the last backup
dedie backup create prod-18 --type physical --level differential    # since the last full backup
dedie backup create orders --level incremental                      # MySQL: from the binary log
dedie backup list
dedie backup verify <id>          # checksum matches and the whole archive decrypts
dedie backup restore <id> --target staging --clean --confirm shop_staging
dedie backup restore <id> --target staging --new-database shop_check --confirm shop_check
dedie backup restore <id> --target staging --new-database one_table --tables public.orders --confirm one_table
dedie backup export <id> shop.dump            # the decrypted archive, for use outside Dedie
dedie backup extract <id> /var/lib/pg-recovered   # a physical backup, into an empty data directory
```

### Where backups are stored

By default backups are written to `backups/` in Dedie's data directory. An owner or admin can add **storage locations**: other directories on the machine that runs Dedie, such as a second disk or a mounted network share, or buckets in **S3-compatible object storage**. Each backup, and each schedule, chooses its location, and the page shows the full path of every backup file with a button to copy it.

```sh
dedie backup location add --name nas --path /mnt/backups/dedie
dedie backup location list
dedie backup create prod-18 --location nas
dedie backup create prod-18 --location nas --plain      # an unencrypted file in the database's own format
```

- Adding a location checks that Dedie can create and write to the directory. Only owners and admins can add one, because it decides where on the host Dedie writes files.
- Files are named for what they are: `shop-20261011-020000-logical-full-1a2b3c4d.dediebak`.
- **Encrypted (the default).** The file can only be read through Dedie: restore, download or `dedie backup export`.
- **Plain.** With `--plain`, or the checkbox in the dialog, the file is written in the database's own format (`.dump`, `.sql.gz`, `.tar.gz`) and can be used straight from its path with `pg_restore`, `mysql` or `tar`. It is not encrypted; whoever can read the directory can read the data.
- Removing a location leaves what is stored there as it is. A location that still holds backups cannot be removed.

**Object storage.** A location can be a bucket in Amazon S3 or anything that speaks its protocol (Ceph, Cloudflare R2, Backblaze B2, Wasabi, a MinIO server). Backups are streamed to the bucket as they are taken, so no local copy is needed, and are read back from it for verification, restore and download. A backup kept away from the database server and from Dedie is the one that survives losing both.

```sh
dedie backup location add --name offsite --s3-bucket acme-db-backups --s3-endpoint s3.eu-west-1.amazonaws.com \
  --s3-region eu-west-1 --s3-prefix dedie/prod --s3-access-key AKIA...      # prompts for the secret key
dedie backup create prod-18 --location offsite
```

- The bucket must exist. Adding the location writes and removes a small test object to prove the key works.
- The secret key is stored encrypted and never shown again. Give the key only what it needs: to put, get and delete objects under the prefix.
- Backups in a bucket are encrypted by Dedie before they leave, unless you choose a plain file. Turn on the bucket's own encryption and versioning as well.
- One backup can be up to about 640 GB.

### Schedules and retention

A schedule takes a backup of one connection on a cron expression (`minute hour day month weekday`, in the server's time zone; `@hourly`, `@daily`, `@weekly` and `@monthly` also work), verifies it, and then deletes the schedule's backups that fall outside its retention rule.

```sh
dedie backup schedule add --name nightly --connection prod-18 --cron "0 2 * * *" --keep-last 14
dedie backup schedule add --name weekly-physical --connection prod-18 --type physical --cron "0 3 * * 0" --keep-days 60
dedie backup schedule list
```

- A backup is deleted as soon as it is beyond `--keep-last` or older than `--keep-days`. The newest backup is always kept, so a schedule never deletes its only good one.
- A running `dedie-server` takes the backups. After downtime it takes one backup, not one for every slot it missed.
- A schedule has a level and a location like any backup. A common arrangement is one schedule for a weekly full backup and another for daily incrementals. An incremental or differential schedule with no full backup to build on takes a full one on its first run.
- A backup that later ones build on is kept until those have expired, whatever the retention rule says.
- A failed scheduled backup raises an alert.
- Backups taken by hand are never deleted by a schedule.

### Recovery

A restore verifies the file first and requires the name of the database being written to as confirmation.

- **Into a new database.** The safest way to look inside a backup or recover one table: the database is created on the target server and restored into, and nothing existing is touched.
- **Selected tables** can be restored from a PostgreSQL backup.
- **Differential and incremental backups.** For MySQL, restoring one applies the full backup and then each later backup in order; the target database must have the same name as the source. Schema changes are captured when they were made while connected to that database, which is how applications and tools work; a statement that changed it from a session using a different default database is not in the binary log's view of it.
- **Physical MySQL backups.** MySQL makes the copy itself with its clone plugin (8.0.17 or newer), into a directory on its own disk that Dedie then archives. That means Dedie must run on the same machine as MySQL, as a user that can read what the server writes; the account needs `BACKUP_ADMIN`, and the plugin must be installed (`INSTALL PLUGIN clone SONAME 'mysql_clone.so'`). `DEDIE_MYSQL_CLONE_DIR` names the directory the copy is staged in. To recover, `dedie backup extract <id> <dir>` and start `mysqld --datadir=<dir>` of the same version. There are no differential or incremental physical backups for MySQL; use the logical ones.
- **Physical backups** are not restored into a running database. For a differential or incremental one, `dedie backup extract` unpacks the whole chain and combines it with `pg_combinebackup` into one complete data directory. `dedie backup extract` unpacks one into an empty directory; start PostgreSQL of the same major version on that directory and the server comes back as it was when the backup was taken, with every database.

- **PostgreSQL** restores run in a single transaction, so a failure changes nothing. Ownership and privileges are left out unless `--keep-owners` is given, since the target server often has different roles.
- **MySQL** restores replace tables of the same name and cannot be rolled back: MySQL commits at every schema statement, so a restore that fails part-way leaves the tables it had already replaced.

## Point-in-time recovery

For PostgreSQL 12 and newer, Dedie can rebuild a server as it was at any chosen moment: a minute before a bad deployment, or a second before a table was dropped.

It takes two things. A **physical backup** is the starting point. The **write-ahead log** is the record of every change since; Dedie receives it from the server continuously, as it is written, and keeps it. Recovery starts from the backup and replays the log up to the moment you name.

1. On the Backups page, turn on point-in-time recovery for the connection (the account needs the `REPLICATION` attribute).
2. Take a physical backup. Recovery reaches moments *after* a physical backup taken while archiving was on. Schedule one regularly: the newer the backup, the less log there is to replay.
3. To recover, on the machine that runs Dedie:

```sh
dedie --local backup recover prod-18 --to "2026-10-11 15:04:05" --dir /var/lib/postgresql/recovered
pg_ctl -D /var/lib/postgresql/recovered -o "-p 5433" start
```

The first command picks the newest physical backup that finished before that moment, unpacks it (with its differential or incremental chain), and sets it up to replay the archived log to that moment. The second starts a **new** server on it, which replays, stops at the moment and opens for use. The original server is never touched. Leave out `--to` to recover to the latest change received. The moment is given in the time zone of the machine that runs Dedie.

What to know:

- The log is received by a running `dedie-server` through a replication slot, so the source never discards log Dedie has not got yet. The other side of that: if Dedie is stopped while archiving is on, the source keeps that log on its own disk until Dedie returns. Do not leave Dedie stopped for long, or turn archiving off first; the server console flags slots that are holding log.
- The archive is kept under `wal/` in the data directory and is **not encrypted by Dedie**. It contains your data. Keep that disk protected.
- Set a retention in days to delete old log. Keep it at least as long as the oldest physical backup you would recover from.
- Turning archiving off drops the slot and keeps the log already received. Turning it on again starts a new recovery window, which needs a new physical backup.
- Recovery runs on the Dedie server host, which is where the archive is; the recovered server needs PostgreSQL of the same major version installed there.

## Monitoring and alerts

`dedie-server` checks every connection once a minute and keeps seven days of results. The Monitoring page shows, for the last 24 hours, each database's uptime and response time, the migrations in progress with their lag, and recent alerts.

Alerts are raised when:

| Event | Raised when |
|---|---|
| A database stops answering | two checks in a row fail |
| A database answers again | the first check after that succeeds |
| A migration fails | the job stops with an error |
| A migration completes | cutover or finish succeeds |
| A migration falls behind | more than 512 MB of source changes are waiting to be applied |
| A backup fails | a scheduled or manual backup ends with an error |

Every alert is recorded. To be told about them, add a channel on the Monitoring page: a Slack, Teams or Mattermost incoming webhook, a generic webhook that receives the alert as JSON, or email over SMTP. A channel can take all alerts or only the kinds you tick, and **Send test** proves it works before you rely on it. Channel addresses and passwords are stored encrypted and are not shown again.

A generic webhook can be given a signing secret; each request then carries `X-Dedie-Signature: sha256=<HMAC-SHA256 of the body>`. Email is sent only over TLS (STARTTLS, or port 465).

**Prometheus.** `GET /metrics` serves connection status and response time, migration counts, lag and applied changes, and backup age and storage, in the Prometheus text format. Authenticate with an API token of role operator or higher:

```yaml
scrape_configs:
  - job_name: dedie
    metrics_path: /metrics
    authorization:
      credentials: dedie_pat_…
    static_configs:
      - targets: ["dedie.example.com"]
```

**Health check.** `GET /healthz` needs no sign-in and answers 200 while the service is working.

## Database users and connections

The Database users page manages accounts on the database server itself, as distinct from the people who sign in to Dedie. It works through whichever account the connection uses, and the server decides what that account may do.

- **Create a user** with a password (there is a generator), an optional connection limit, membership of roles, and optionally a ready-made level of access to the connection's database: read only, or read and write.
- **Change a user**: new password, enable or disable sign-in, grant or revoke roles, change the limit.
- **Drop a user.** PostgreSQL refuses to drop a role that owns objects or holds privileges. The dialog offers to hand over what the role has in the connection's database first: its objects go to the connecting account and its privileges are revoked. No table or data is dropped.
- **Connection limits** per user on both engines, and per database on PostgreSQL.
- **Connection pools.** For every connected user: how much of its limit it is using, with users at or near their limit flagged, and how many of its connections are active, idle, or idle inside a transaction.
- **Close idle connections by hand.** For one user or the whole server, close connections idle longer than a number of minutes. Connections running a statement are never touched. On PostgreSQL you can also close those idle inside a transaction, which rolls their uncommitted work back. The dialog shows how long the longest-idle connection has been idle and warns when none has reached the time you chose: an application with a connection pool tests its connections every minute or two, which starts their idle time again, so 0 minutes is often the setting that closes them. Dedie waits until the closed sessions are really gone, then tells you how many are idle again a moment later, because a pool opens new connections as soon as it loses some. To hold the number down for good, give the user a connection limit or lower the pool's minimum size in the application.
- **Close idle connections automatically.** A rule per server, applied every minute: close connections idle for more than N minutes, optionally only while the server is above a chosen fullness, with users that must never be disconnected. Dedie's own sessions and those of the connection's own account are always spared. Each pass that closes something is recorded in the audit log.
- **Connections in use**: how close the server is to its limit, and who is holding connections, grouped by user, application, client address and database, with how many are active, idle, or idle inside a transaction.
- **Disconnect** the sessions of a user, an application or a whole database.

On PostgreSQL the password is turned into a SCRAM-SHA-256 verifier before it is sent, so the password itself never reaches the server or its logs. Dedie does not store database user passwords; copy a generated one when you create it. Every change is written to the audit log, without the password.

**Owners.** In the query browser, **Change owner** gives a database, a schema or a table to another role (PostgreSQL; MySQL objects have no owner).

## Security model

**Secrets at rest.** Database passwords are sealed with AES-256-GCM under a per-secret data key, which is itself wrapped by the master key (envelope encryption). Each ciphertext is bound to its organization and connection, so it cannot be copied onto another record. Backups are encrypted as a stream of authenticated chunks; truncation, reordering and modification are all detected on read.

**Master key.** Read from `DEDIE_MASTER_KEY` (32 bytes, base64) or from `master.key` in the data directory, created with mode 0600 on first run. Whoever holds this key can decrypt stored credentials and backups. Keep it out of backups of the data directory, and store a copy somewhere safe: without it, saved connections and backups are unrecoverable.

**Sign-in.** Passwords are hashed with Argon2id. Sessions are random 256-bit tokens kept in an `HttpOnly`, `SameSite=Strict` cookie; only their SHA-256 is stored. Five failed attempts lock an account for fifteen minutes, and sign-in is rate-limited per address. Changing a user's password, role or status ends their sessions.

**Two-factor sign-in.** Any user can add a time-based code from an authenticator app (TOTP, RFC 6238) on their Account page, and receives eight single-use recovery codes. The authenticator secret is stored encrypted. An organization can require it for everyone: a user who has not set it up can reach their Account page and nothing else until they do. An administrator can reset it for someone who has lost their phone and codes.

**Security policy.** Owners and admins set the minimum password length, whether passwords must mix character classes, how long a sign-in lasts, and whether two-factor sign-in is required.

**Sessions.** The Account page lists your sign-ins with device, address and last activity, and signs any of them out.

**API tokens.** For scripts, CI and the CLI. A token acts as the user who created it, capped at a role no higher than their own, optionally with an expiry. It is shown once; only its hash is stored. A token cannot create tokens, change passwords or alter two-factor settings, so a leaked token cannot mint further credentials. Send it as `Authorization: Bearer <token>`.

**Roles.**

| Role | May |
|---|---|
| owner | everything, including managing other owners |
| admin | everything except creating or changing owners |
| operator | connections, read and write queries, migrations, backups, restores, server console, database users, monitoring |
| analyst | read-only queries, view connections and migrations |
| viewer | view connections and migrations |

An organization always keeps at least one active owner.

**Connection permissions.** A connection can be restricted to chosen people. It then disappears for everyone else except owners and admins: from lists, from the query browser, and from the API, which answers as if it did not exist. What a permitted person may do with it still follows their role.

**Production connections.** Destructive and disruptive actions on a connection labelled production are refused until the caller supplies the connection's name, and each one that goes ahead is audited. See [Environments and production safeguards](#environments-and-production-safeguards) for the list. This guards against acting on the wrong database; it gives nobody access they did not have.

**Queries.** The browser runs statements in a read-only session unless the user has write permission, the connection is not marked read-only, and they switch writes on for that run. The database server enforces it. Edits made in the grid bind their values as parameters and are applied only when they touch exactly one row. This is a guard rail; the real boundary is the database account a connection uses, so give each connection the least privilege it needs.

**Audit log.** Sign-ins, every executed statement, every row edited in the grid, and every change to connections, users, database users, migrations, backups, schedules and alert channels are recorded, with the API token used where there was one. Each entry includes a hash of the previous one, so an edited or deleted entry breaks the chain; `dedie audit verify` or the Audit page reports where. Removing entries from the end of the log is not detectable from the log alone, so export it regularly if you need that guarantee.

**Transport.** New connections default to `sslmode=require`; `verify-full` with your CA certificate is recommended, and preflight warns when a migration would run over an unencrypted connection. The web server sets a content security policy, forbids framing, and rejects state-changing requests that lack its custom header or come from another origin.

**Client tools.** Backup tools receive credentials through the environment or a private temporary option file, never on the command line.

**Data directory.** Created with mode 0700; the metadata database and backups with 0600.

## HTTP API

Everything the interface and the remote CLI do goes through a JSON API under `/api/v1`. Authenticate with an API token (`Authorization: Bearer dedie_pat_…`), or with the session cookie, in which case every request that changes state must also carry the header `X-Dedie-Request: 1`. Errors are `{"error": "..."}` with a matching status, and a `code` where a client is expected to react to the condition. One such code is `confirm_production`, with status 428: the action touches a production connection and wants its name. Send the request again with the header `X-Dedie-Confirm` set to the connection's name, percent-encoded.

| Area | Endpoints |
|---|---|
| Setup and session | `GET/POST /setup`, `POST /auth/login`, `POST /auth/logout`, `GET /auth/me` |
| Account | `POST /auth/password`, `GET /auth/sessions`, `DELETE /auth/sessions/{id}`, `POST /auth/totp/{begin,confirm,disable}`, `GET/POST /auth/tokens`, `DELETE /auth/tokens/{id}`, `GET/PUT /auth/policy` |
| Users | `GET/POST /users`, `PATCH /users/{id}` |
| Connections | `GET/POST /connections`, `GET/PUT/DELETE /connections/{id}`, `POST /connections/test`, `POST /connections/{id}/test`, `GET /connections/status`, `GET/PUT /connections/{id}/access` |
| Objects | `POST /connections/{id}/databases`, `POST /connections/{id}/schemas`, `POST /connections/{id}/tables`, `GET /connections/{id}/server-objects`, `POST /connections/{id}/owner` |
| Query | `GET /connections/{id}/catalog`, `POST /connections/{id}/query`, `POST /connections/{id}/rows`, `GET/POST /queries`, `PUT/DELETE /queries/{id}` |
| Server console | `GET /connections/{id}/overview`, `POST /connections/{id}/backends/{pid}/cancel` |
| Database users | `GET/POST /connections/{id}/accounts`, `DELETE /connections/{id}/accounts/{user}`, `POST /connections/{id}/connection-limit`, `POST /connections/{id}/sessions/terminate`, `GET/PUT /connections/{id}/idle-policy` |
| Migrations | `POST /connections/{id}/suggest-columns`, `GET/POST /migrations`, `GET /migrations/{id}`, `POST /migrations/{id}/preflight`, `GET /migrations/{id}/preview`, `GET /migrations/{id}/leftovers`, `POST /migrations/{id}/on-existing`, `POST /migrations/{id}/actions/{start,pause,resume,cutover,abort,delete}` |
| Backups | `GET/POST /backups`, `DELETE /backups/{id}`, `POST /backups/{id}/verify`, `POST /backups/{id}/restore`, `GET /backups/{id}/download`, `GET /restores`, `GET/POST /schedules`, `PUT/DELETE /schedules/{id}`, `POST /schedules/{id}/run`, `GET/POST /backup-locations`, `DELETE /backup-locations/{id}`, `GET /wal-archives`, `PUT /connections/{id}/wal-archive` |
| Overview | `GET /summary` |
| Monitoring | `GET /monitoring`, `GET /alerts/events`, `GET/POST /alerts/channels`, `PUT/DELETE /alerts/channels/{id}`, `POST /alerts/channels/{id}/test` |
| Audit | `GET /audit`, `GET /audit/verify` |

Outside `/api/v1`: `GET /metrics` (Prometheus, token required) and `GET /healthz` (no sign-in).

## Architecture

```
apps/
  server/            dedie-server: HTTP listener, workers, graceful shutdown
  cli/               dedie: one file per command group, with a local and a remote backend
  desktop/           Dedie Desktop: JavaFX window that runs the server as its engine
  internal/httpapi/  routes, middleware and handlers
engine/
  app/               use cases: authorization, auditing, orchestration
  connector/         driver contract and registry
    postgres/        connection, query execution, catalog, administration
    mysql/           the same for MySQL
    mongodb/         the same for MongoDB, with commands in place of SQL
  migrate/           options, phases, reporting and the pipeline registry
    pgpg/            PostgreSQL → PostgreSQL, one file per phase; also the replication reader other pipelines reuse
    mymy/            MySQL → MySQL, one file per phase; also the binary log reader other pipelines reuse
    mypg/            MySQL → PostgreSQL: schema translation, value conversion, PostgreSQL applier
    pgmy/            PostgreSQL → MySQL: the same in the other direction
    mongopg/         MongoDB → PostgreSQL: collection copy and change-stream applier
  backup/            pg_dump / pg_restore / pg_basebackup and mysqldump / mysql drivers
  storage/           where backup files are kept: a directory, or S3-compatible object storage
  schedule/          cron expressions
  notify/            alert delivery: webhook, chat hook, email
  secure/            envelope and stream encryption, password hashing, tokens
  store/             metadata database (SQLite), one file per entity
web/                 Next.js interface, exported statically and embedded in the server
```

**Connections to your databases are reused.** Opening one is the slow part of most requests, above all to a distant server or over TLS, so a session is kept after a request for the next one: up to three per saved connection, for 90 seconds after they were last used, then closed. A kept session is always as good as new. One that ran a statement somebody typed is reset first (PostgreSQL: any open transaction rolled back, then `DISCARD ALL`) or, on engines that cannot restore a session completely, closed. Sessions are never shared between connections with different details, and editing, testing or deleting a connection closes those kept for it. They appear on the database server as idle connections of Dedie's own account and application name, which the [idle rule](#database-users-and-connections) spares.

Every front end calls `engine/app` and nothing beneath it, so a permission check or audit entry cannot be bypassed by using a different interface.

Two registries keep the engine open to new databases. A driver implements `connector.Driver` and registers under its kind, which makes it available to the query browser, the server console and connection management. A pipeline implements `migrate.Pipeline` and registers for a source and target kind; the application layer looks pipelines up by that pair and knows nothing else about them.

Migration jobs are leased in the metadata database with a heartbeat. Several processes can share a data directory without running a job twice, and a job whose worker dies is picked up by the next one. Scheduled backups are claimed the same way.

Besides the HTTP listener, `dedie-server` runs four background loops: the worker that executes migrations, the scheduler that takes backups, the monitor that checks connections, raises alerts and closes idle connections, and the archiver that keeps a write-ahead log receiver running for each server with point-in-time recovery on. `--no-worker` turns all four off, for a process that should only serve the API.

How each pipeline gets exactly-once delivery:

| | PostgreSQL | MySQL | MongoDB |
|---|---|---|---|
| Consistent start | replication slot created together with an exported snapshot | read transactions opened at a recorded binary log position under a brief global read lock | cluster time recorded before the copy; changes are idempotent, so overlap with the copy is harmless |
| Change stream | logical replication (`pgoutput`) | row events from the binary log | the database's change stream |
| Checkpoint | commit LSN, stored on the target in the same transaction as the data | binary log file and offset, stored the same way | resume token, stored the same way |
| Cutover marker | a logical message written to the WAL after fencing | the end of the binary log after fencing | the stream running dry |

## Development

```sh
make test               # unit tests; needs neither Node nor a database
make lint               # gofmt and go vet
cd web && npm run dev   # interface with hot reload, proxying /api to 127.0.0.1:8080
```

To compile the Go programs without building the interface, pass `-tags noui`.

The end-to-end tests need two servers of the same kind and create and drop their own databases. Each test skips itself when its variables are not set.

```sh
# PostgreSQL: the source needs wal_level = logical
export DEDIE_TEST_SOURCE=postgres://postgres@127.0.0.1:54316/postgres
export DEDIE_TEST_TARGET=postgres://postgres@127.0.0.1:54318/postgres

# MySQL: the source needs binary logging in ROW format. The cross-engine
# tests reuse these: MySQL to PostgreSQL reads DEDIE_TEST_MYSQL_SOURCE and
# DEDIE_TEST_TARGET, PostgreSQL to MySQL reads DEDIE_TEST_SOURCE and
# DEDIE_TEST_MYSQL_TARGET.
export DEDIE_TEST_MYSQL_SOURCE=mysql://root@127.0.0.1:33161
export DEDIE_TEST_MYSQL_TARGET=mysql://root@127.0.0.1:33162

# MongoDB: a replica set member; a single mongod started with --replSet is
# initiated by the test. It migrates into DEDIE_TEST_TARGET.
export DEDIE_TEST_MONGO_SOURCE=mongodb://127.0.0.1:27117

# Oracle: an account whose own schema the tests may fill and empty. The
# online tests also need supplemental logging on the database and the
# LogMiner privileges; see docs/databases.md. Oracle to PostgreSQL and
# Oracle to MySQL both read DEDIE_TEST_ORACLE_SOURCE; Oracle to Oracle goes
# from the schema of one account to the schema of another.
export DEDIE_TEST_ORACLE_SOURCE=oracle://dedie_test:secret@127.0.0.1:1521/FREEPDB1
export DEDIE_TEST_ORACLE_SOURCE_C=oracle://dedie_test_c:secret@127.0.0.1:1521/FREEPDB1
export DEDIE_TEST_ORACLE_TARGET=oracle://dedie_test_d:secret@127.0.0.1:1521/FREEPDB1

make test-integration
```

To check a desktop build without watching the window, run it with `-Ddedie.smoke=true`: it loads the interface, prints what it rendered and exits.

### Continuous integration

`.github/workflows/ci.yml` lints, runs the unit tests, runs the PostgreSQL 16 → 18 and MongoDB 8.0 → PostgreSQL 18 migration tests, builds the server and CLI for Linux, macOS and Windows, and builds the desktop application on a runner of each operating system. Pushing a tag that starts with `v` publishes everything as a GitHub release with checksums. The MySQL and Oracle migration tests are run locally for now.

### Branching

`main` holds released work. Each feature is developed on a `feature/...` branch and merged through a pull request once its tests pass.

## Configuration reference

| Setting | Meaning | Default |
|---|---|---|
| `DEDIE_DATA_DIR`, `--data` | metadata database, master key, backups | `~/.dedie` |
| `DEDIE_MASTER_KEY` | master key, base64 of 32 bytes | `master.key` in the data directory |
| `DEDIE_PG_BIN` | directory containing `pg_dump`, `pg_restore` and `pg_basebackup` | newest version found on the system |
| `DEDIE_MYSQL_BIN` | directory containing `mysqldump`, `mysql` and `mysqlbinlog` | the `PATH` |
| `DEDIE_MYSQL_CLONE_DIR` | directory a physical MySQL backup is staged in; the MySQL server must be able to create a directory there | the system's temporary directory |
| `DEDIE_PASSWORD` | password for CLI commands that would prompt | prompt |
| `DEDIE_SERVER` | for the CLI: address of the Dedie server to run commands on, with `DEDIE_TOKEN`. For the desktop application: location of the `dedie-server` program | saved sign-in; beside the jar, then `PATH` |
| `DEDIE_TOKEN` | API token for the CLI | saved by `dedie login` |
| `DEDIE_CA_CERT` | PEM file for a Dedie server with a privately signed certificate | system authorities |
| `DEDIE_CONFIG` | where the CLI keeps its saved sign-in | `dedie/cli.json` in the user's configuration directory |
| `--addr` | listen address of `dedie-server`; port 0 picks a free port | `127.0.0.1:8080` |
| `--tls-cert`, `--tls-key` | serve HTTPS | off |
| `--behind-tls-proxy` | trust `X-Forwarded-For`, mark cookies `Secure` | off |
| `--no-worker` | serve the API without executing migrations, scheduled backups or health checks | off |

What the data directory holds:

| Path | Contents |
|---|---|
| `dedie.db` | organizations, users, sessions, API token hashes, connections (passwords encrypted), jobs, schedules, saved queries, alert channels (settings encrypted), health checks, audit log |
| `master.key` | the master key, unless `DEDIE_MASTER_KEY` is set |
| `backups/` | backup files, unless another storage location is chosen |
| `wal/` | the write-ahead log archive of each server with point-in-time recovery on |
| `desktop/` | the desktop window's local preferences |
| `desktop.log` | the engine's log when run by the desktop application |

## Roadmap

1. **SQL Server** as a source and a target, and Oracle 12c and 19c in the test matrix.
2. **Two-way sync** between two databases, with conflict handling.
3. **Encrypted and off-host log archives**: the write-ahead log archive sealed like backups are, and kept in object storage.
4. **Backups of MongoDB**, and incremental MySQL backups that do not depend on the binary log being retained.
5. **Partitioned tables** for PostgreSQL, and tested upgrade paths across more MySQL versions.
6. **Single sign-on** (OIDC, SAML) and **external key management** (cloud KMS, Vault) behind the existing key interface.
7. **Privileges in detail**: per-table and per-column grants on the Database users page.
