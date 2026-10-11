# Dedie

A query browser, administration console and live migration engine for PostgreSQL, MySQL and MongoDB.

*Dedie* is Ewe for "safe": the data should arrive safely and stay that way.

Website: https://eselase-noble.github.io/Dedie/ · Downloads: https://github.com/Eselase-Noble/Dedie/releases

Dedie moves a production database to a newer server version, or to a different database engine, while the old one stays online. It copies a consistent snapshot, follows the source's replication log to stay in step, and switches over in a write pause measured in seconds. The same engine powers a query browser with data editing, scheduled encrypted backups with verified restore, health monitoring with alerts, and administration of database users and connections, behind role-based access control, two-factor sign-in and a tamper-evident audit log.

## Contents

- [What it does](#what-it-does)
- [Status](#status)
- [Programs](#programs)
- [Quick start](#quick-start)
- [The web interface](#the-web-interface)
- [The command line](#the-command-line)
- [The desktop application](#the-desktop-application)
- [Migrating a database](#migrating-a-database)
- [Continuous sync](#continuous-sync)
- [Backups](#backups)
- [Monitoring and alerts](#monitoring-and-alerts)
- [Database users and connections](#database-users-and-connections)
- [Security model](#security-model)
- [HTTP API](#http-api)
- [Architecture](#architecture)
- [Development](#development)
- [Configuration reference](#configuration-reference)
- [Roadmap](#roadmap)

## What it does

**Online migration.** Upgrade PostgreSQL or MySQL to a newer version, move a database between MySQL and PostgreSQL in either direction, or move MongoDB into PostgreSQL, with the source in service throughout. A migration can cover a whole database, selected schemas, or selected tables. It can be paused and resumed, survives a crash without losing or repeating a change, and refuses to complete a cutover if source and target differ.

**Continuous sync.** The same pipelines can keep a target following its source indefinitely, for a reporting copy or a warm standby on another engine.

**Query browser.** A full-window workspace with a schema tree and any number of tabs. A query tab is a SQL editor that follows the connection's dialect, completes table and column names, formats SQL, explains and analyzes statements, and can cancel one that is running. A table tab pages through a table with a filter and, when you switch editing on, changes cells, adds rows and deletes them. Results sort, filter and export as CSV, JSON or SQL. Queries can be saved privately or shared with the organization.

**Creating and owning objects.** Create a database, a schema or a table in any database on the server, and hand databases, schemas and tables to another role.

**Backup and recovery.** Full, structure-only, data-only, selected-table and physical backups, compressed and encrypted before they reach disk. Schedules with retention take them automatically and verify each one. Restore into an existing database, into a new one, or one table at a time.

**Monitoring and alerts.** Every database is checked once a minute. Uptime, response time and migration lag are shown on one page and exported for Prometheus, and alerts go to Slack, a webhook or email.

**Database users and connections.** Create database users, give them roles or a ready-made access level, cap how many connections a user or a database may hold, see which applications are using the server's connections, and disconnect them.

**Server administration.** Live sessions with cancel and terminate, database sizes, replication slots and the WAL they retain, and the settings that matter for replication and security.

**Organization controls.** Users with five roles, two-factor sign-in, API tokens, per-connection permissions, a password policy, encrypted credential storage, and a hash-chained audit log that records every statement and every change.

## Status

| Capability | PostgreSQL | MySQL | MongoDB |
|---|---|---|---|
| Online version upgrade | Working (source 10+) | Working (source 5.6+) | — |
| Query browser and schema explorer | Working | Working | Working (commands as JSON) |
| Edit rows in the grid | Working | Working | — |
| Backups: full, structure, data, selected tables | Working | Working | — |
| Physical backup of the whole server | Working | — | — |
| Scheduled backups with retention | Working | Working | — |
| Health monitoring and alerts | Working | Working | Working |
| Database users, roles, connection limits | Working | Working | — |
| Change object owners | Working | not applicable | not applicable |
| Server console | Working | Working | Working |

| Migration or sync path | State |
|---|---|
| PostgreSQL → PostgreSQL | Working |
| MySQL → MySQL | Working |
| MySQL → PostgreSQL | Working: tables, keys, indexes and foreign keys are translated; views, routines and triggers are not |
| PostgreSQL → MySQL | Working: tables, keys, indexes and foreign keys are translated; views, functions and triggers are not |
| MongoDB → PostgreSQL | Working: each collection becomes a table holding every document as `jsonb`; indexes and validators are not translated |

All five pipelines are covered by end-to-end tests that migrate a database under write load, pause and resume mid-stream, cut over and compare the result. The PostgreSQL test runs 16 → 18; the cross-engine tests run MySQL 8.0 → PostgreSQL 18, PostgreSQL 16 → MySQL 8.0 and MongoDB 8.0 → PostgreSQL 18. MySQL versions other than 8.0 and MongoDB versions other than 8.0 have not been exercised yet.

Not built yet: two-way sync with conflict handling, point-in-time recovery, physical backups for MySQL, backups of MongoDB, single sign-on, and external key management. See the [roadmap](#roadmap).

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
| Overview | See connections, migrations and recent backups at a glance. |
| Connections | Add, edit, test and delete connections, see which servers are online, and restrict a connection to chosen people. Passwords are write-only; weak SSL modes are flagged. |
| Query browser | Browse schemas; open tables and edit their rows; write, format, explain and save SQL in tabs; sort, filter and export results; create databases, schemas and tables; change owners. |
| Migrations | Create a migration or a continuous sync at database, schema or table level; run the preflight check; watch phase, lag and per-table progress; pause, resume, cut over or abort. |
| Backups | Take a backup of any type, schedule backups with retention, verify, download, restore into an existing or a new database, and review restore history. |
| Monitoring | Uptime and response time of every database, migrations in progress, alert channels and recent alerts. |
| Server activity | Inspect sessions, sizes, replication slots and settings; cancel or terminate a session. |
| Database users | Create and change users on a database server, set connection limits, and see and disconnect who is connected. |
| Dedie users | Create users, change roles, disable accounts, reset passwords and two-factor sign-in, and set the security policy. |
| Audit log | Read the log and verify that it has not been altered. |
| Account | Change your password, turn on two-factor sign-in, review your sessions and create API tokens. |

Navigation and actions a role may not use are hidden; the server enforces the same rules regardless.

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
dedie conn list
dedie query prod-16 "select count(*) from orders"      # read-only unless --write
dedie backup create prod-16 --note "before upgrade"
dedie migrate list
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

The window has a native menu bar: **Go** jumps to any page (Ctrl/Cmd+1 to 8), **View** zooms and switches to full screen, and **Help** opens the engine's log and the data folder. Exports and backup downloads open a save dialog, and the window remembers its size, position and zoom.

The engine's log is written to `desktop.log` in the data directory. If the engine stops, the window says so and offers to start it again.

## Migrating a database

```sh
dedie migrate create --name "shop 16 to 18" --source prod-16 --target prod-18 --fence
dedie migrate preflight <id>     # changes nothing; lists everything that would block or weaken the run
dedie migrate run <id>           # or `dedie migrate start <id>` to let a running dedie-server execute it
dedie migrate status <id>
dedie migrate cutover <id>       # when lag is near zero and you are ready to switch
```

Supported pairs are PostgreSQL → PostgreSQL, MySQL → MySQL, MySQL → PostgreSQL, PostgreSQL → MySQL and MongoDB → PostgreSQL. The target must not already contain the tables being migrated.

Against a Dedie server, `dedie migrate run` starts the job on the server and follows its progress; Ctrl+C stops watching without stopping the migration.

### Levels

| Level | How to ask for it | What moves |
|---|---|---|
| Database | nothing extra | Every schema and table in the source connection's database |
| Schema | `--schemas public,billing` | Everything in the listed schemas (PostgreSQL; in MySQL a schema is the database) |
| Table | `--tables public.orders,public.order_items` | Only the listed tables, with their indexes, constraints and triggers. Views and other standalone objects are left out |

`--exclude` removes tables at any level. PostgreSQL tables are named `schema.table`; MySQL tables by name alone. Preflight fails if a listed table does not exist.

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
- Views, stored routines, triggers and events are written in MySQL's dialect and are not migrated.

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
- Views, functions, triggers and custom types are not migrated.
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

- A level is the whole database or selected collections (`--tables orders,customers`).
- Collections created while the job runs are picked up and get their own table.
- Indexes, validators and views are not translated. Dropping or renaming a collection on the source is reported and leaves the target table as it is.
- `jsonb` cannot store the null character; it is removed from the few strings that contain one, and each case is counted and reported.
- Two documents whose keys read the same as text, such as the number `5` and the string `"5"`, would share a row. Validation catches this as a count mismatch.
- The source cannot be fenced. Stop the applications that write to it before cutting over.
- Validation compares document counts and the set of keys, not document contents.
- A paused job can resume for as long as the source's oplog still reaches back to where it stopped.

### For all pipelines

- Schema changes made on the source while a migration is running are not replicated. Freeze the schema until cutover. `TRUNCATE` is replicated.
- Aborting removes what the migration created on the source and leaves the copied tables on the target for inspection.
- With `--fence` the source stays read-only after a successful cutover, which prevents stray writes to the old database.
- Fencing stops well-behaved applications. It is not a security control.
- Checksum validation reads every row on both sides during the write pause. For very large databases choose `--validation count`.

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

Backups are compressed, encrypted before they reach disk, and stored under `backups/` in the data directory with a SHA-256 of the encrypted file.

| Type | Contains | PostgreSQL | MySQL |
|---|---|---|---|
| `full` | structure and data of the database | `pg_dump` custom format | `mysqldump`, gzip |
| `schema` | structure only: tables, indexes, views, routines | yes | yes |
| `data` | rows only, for a database that already has the structure | yes | yes |
| `tables` | structure and data of the tables you choose | yes | yes |
| `physical` | a copy of the server's data files, covering every database | `pg_basebackup`, needs the `REPLICATION` attribute | not available |

```sh
dedie backup create prod-18 --note "before release"
dedie backup create prod-18 --type tables --tables public.orders,public.order_items
dedie backup create prod-18 --type physical
dedie backup list
dedie backup verify <id>          # checksum matches and the whole archive decrypts
dedie backup restore <id> --target staging --clean --confirm shop_staging
dedie backup restore <id> --target staging --new-database shop_check --confirm shop_check
dedie backup restore <id> --target staging --new-database one_table --tables public.orders --confirm one_table
dedie backup export <id> shop.dump            # the decrypted archive, for use outside Dedie
dedie backup extract <id> /var/lib/pg-recovered   # a physical backup, into an empty data directory
```

### Schedules and retention

A schedule takes a backup of one connection on a cron expression (`minute hour day month weekday`, in the server's time zone; `@hourly`, `@daily`, `@weekly` and `@monthly` also work), verifies it, and then deletes the schedule's backups that fall outside its retention rule.

```sh
dedie backup schedule add --name nightly --connection prod-18 --cron "0 2 * * *" --keep-last 14
dedie backup schedule add --name weekly-physical --connection prod-18 --type physical --cron "0 3 * * 0" --keep-days 60
dedie backup schedule list
```

- A backup is deleted as soon as it is beyond `--keep-last` or older than `--keep-days`. The newest backup is always kept, so a schedule never deletes its only good one.
- A running `dedie-server` takes the backups. After downtime it takes one backup, not one for every slot it missed.
- A failed scheduled backup raises an alert.
- Backups taken by hand are never deleted by a schedule.

### Recovery

A restore verifies the file first and requires the name of the database being written to as confirmation.

- **Into a new database.** The safest way to look inside a backup or recover one table: the database is created on the target server and restored into, and nothing existing is touched.
- **Selected tables** can be restored from a PostgreSQL backup.
- **Physical backups** are not restored into a running database. `dedie backup extract` unpacks one into an empty directory; start PostgreSQL of the same major version on that directory and the server comes back as it was when the backup was taken, with every database.

- **PostgreSQL** restores run in a single transaction, so a failure changes nothing. Ownership and privileges are left out unless `--keep-owners` is given, since the target server often has different roles.
- **MySQL** restores replace tables of the same name and cannot be rolled back: MySQL commits at every schema statement, so a restore that fails part-way leaves the tables it had already replaced.

Point-in-time recovery, which replays the write-ahead log on top of a physical backup to reach a chosen moment, is on the roadmap.

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

**Queries.** The browser runs statements in a read-only session unless the user has write permission, the connection is not marked read-only, and they switch writes on for that run. The database server enforces it. Edits made in the grid bind their values as parameters and are applied only when they touch exactly one row. This is a guard rail; the real boundary is the database account a connection uses, so give each connection the least privilege it needs.

**Audit log.** Sign-ins, every executed statement, every row edited in the grid, and every change to connections, users, database users, migrations, backups, schedules and alert channels are recorded, with the API token used where there was one. Each entry includes a hash of the previous one, so an edited or deleted entry breaks the chain; `dedie audit verify` or the Audit page reports where. Removing entries from the end of the log is not detectable from the log alone, so export it regularly if you need that guarantee.

**Transport.** New connections default to `sslmode=require`; `verify-full` with your CA certificate is recommended, and preflight warns when a migration would run over an unencrypted connection. The web server sets a content security policy, forbids framing, and rejects state-changing requests that lack its custom header or come from another origin.

**Client tools.** Backup tools receive credentials through the environment or a private temporary option file, never on the command line.

**Data directory.** Created with mode 0700; the metadata database and backups with 0600.

## HTTP API

Everything the interface and the remote CLI do goes through a JSON API under `/api/v1`. Authenticate with an API token (`Authorization: Bearer dedie_pat_…`), or with the session cookie, in which case every request that changes state must also carry the header `X-Dedie-Request: 1`. Errors are `{"error": "..."}` with a matching status, and a `code` where a client is expected to react to the condition.

| Area | Endpoints |
|---|---|
| Setup and session | `GET/POST /setup`, `POST /auth/login`, `POST /auth/logout`, `GET /auth/me` |
| Account | `POST /auth/password`, `GET /auth/sessions`, `DELETE /auth/sessions/{id}`, `POST /auth/totp/{begin,confirm,disable}`, `GET/POST /auth/tokens`, `DELETE /auth/tokens/{id}`, `GET/PUT /auth/policy` |
| Users | `GET/POST /users`, `PATCH /users/{id}` |
| Connections | `GET/POST /connections`, `GET/PUT/DELETE /connections/{id}`, `POST /connections/test`, `POST /connections/{id}/test`, `GET /connections/status`, `GET/PUT /connections/{id}/access` |
| Objects | `POST /connections/{id}/databases`, `POST /connections/{id}/schemas`, `POST /connections/{id}/tables`, `GET /connections/{id}/server-objects`, `POST /connections/{id}/owner` |
| Query | `GET /connections/{id}/catalog`, `POST /connections/{id}/query`, `POST /connections/{id}/rows`, `GET/POST /queries`, `PUT/DELETE /queries/{id}` |
| Server console | `GET /connections/{id}/overview`, `POST /connections/{id}/backends/{pid}/cancel` |
| Database users | `GET/POST /connections/{id}/accounts`, `DELETE /connections/{id}/accounts/{user}`, `POST /connections/{id}/connection-limit`, `POST /connections/{id}/sessions/terminate` |
| Migrations | `GET/POST /migrations`, `GET /migrations/{id}`, `POST /migrations/{id}/preflight`, `POST /migrations/{id}/actions/{start,pause,resume,cutover,abort,delete}` |
| Backups | `GET/POST /backups`, `DELETE /backups/{id}`, `POST /backups/{id}/verify`, `POST /backups/{id}/restore`, `GET /backups/{id}/download`, `GET /restores`, `GET/POST /schedules`, `PUT/DELETE /schedules/{id}`, `POST /schedules/{id}/run` |
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
  schedule/          cron expressions
  notify/            alert delivery: webhook, chat hook, email
  secure/            envelope and stream encryption, password hashing, tokens
  store/             metadata database (SQLite), one file per entity
web/                 Next.js interface, exported statically and embedded in the server
```

Every front end calls `engine/app` and nothing beneath it, so a permission check or audit entry cannot be bypassed by using a different interface.

Two registries keep the engine open to new databases. A driver implements `connector.Driver` and registers under its kind, which makes it available to the query browser, the server console and connection management. A pipeline implements `migrate.Pipeline` and registers for a source and target kind; the application layer looks pipelines up by that pair and knows nothing else about them.

Migration jobs are leased in the metadata database with a heartbeat. Several processes can share a data directory without running a job twice, and a job whose worker dies is picked up by the next one. Scheduled backups are claimed the same way.

Besides the HTTP listener, `dedie-server` runs three background loops: the worker that executes migrations, the scheduler that takes backups, and the monitor that checks connections and raises alerts. `--no-worker` turns all three off, for a process that should only serve the API.

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

make test-integration
```

To check a desktop build without watching the window, run it with `-Ddedie.smoke=true`: it loads the interface, prints what it rendered and exits.

### Continuous integration

`.github/workflows/ci.yml` lints, runs the unit tests, runs the PostgreSQL 16 → 18 and MongoDB 8.0 → PostgreSQL 18 migration tests, builds the server and CLI for Linux, macOS and Windows, and builds the desktop application on a runner of each operating system. Pushing a tag that starts with `v` publishes everything as a GitHub release with checksums. The MySQL migration tests are run locally for now.

### Branching

`main` holds released work. Each feature is developed on a `feature/...` branch and merged through a pull request once its tests pass.

## Configuration reference

| Setting | Meaning | Default |
|---|---|---|
| `DEDIE_DATA_DIR`, `--data` | metadata database, master key, backups | `~/.dedie` |
| `DEDIE_MASTER_KEY` | master key, base64 of 32 bytes | `master.key` in the data directory |
| `DEDIE_PG_BIN` | directory containing `pg_dump`, `pg_restore` and `pg_basebackup` | newest version found on the system |
| `DEDIE_MYSQL_BIN` | directory containing `mysqldump` and `mysql` | the `PATH` |
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
| `backups/` | encrypted backup files |
| `desktop/` | the desktop window's local preferences |
| `desktop.log` | the engine's log when run by the desktop application |

## Roadmap

1. **Point-in-time recovery** for PostgreSQL: continuous WAL archiving on top of physical backups, and recovery to a chosen moment.
2. **Two-way sync** between two databases, with conflict handling.
3. **Typed columns for MongoDB**: a reviewed mapping that lifts chosen document fields into columns beside `doc`.
4. **Backup storage outside the server**: S3-compatible object storage, and backups of MongoDB.
5. **Partitioned tables** for PostgreSQL, and tested upgrade paths across more MySQL versions.
6. **Single sign-on** (OIDC, SAML) and **external key management** (cloud KMS, Vault) behind the existing key interface.
7. **Privileges in detail**: per-table and per-column grants on the Database users page.
