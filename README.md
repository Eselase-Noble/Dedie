# Dedie

A query browser, administration console and live migration engine for relational databases.

*Dedie* is Ewe for "safe": the data should arrive safely and stay that way.

Dedie moves a production database to a newer server version while the old one stays online. It copies a consistent snapshot, follows the source's replication log to stay in step, and switches over in a write pause measured in seconds. The same engine powers a SQL browser, encrypted backups with verified restore, and a server activity console, behind role-based access control and a tamper-evident audit log.

## Contents

- [What it does](#what-it-does)
- [Status](#status)
- [Programs](#programs)
- [Quick start](#quick-start)
- [The web interface](#the-web-interface)
- [The command line](#the-command-line)
- [The desktop application](#the-desktop-application)
- [Migrating a database](#migrating-a-database)
- [Backups](#backups)
- [Security model](#security-model)
- [HTTP API](#http-api)
- [Architecture](#architecture)
- [Development](#development)
- [Configuration reference](#configuration-reference)
- [Roadmap](#roadmap)

## What it does

**Online migration.** Upgrade PostgreSQL or MySQL to a newer version, or move a MySQL database to PostgreSQL, with the source in service throughout. A migration can cover a whole database, selected schemas, or selected tables. It can be paused and resumed, survives a crash without losing or repeating a change, and refuses to complete a cutover if source and target differ.

**Query browser.** A full-window workspace with a schema tree, a SQL editor and a result grid, each resizable. The editor follows the connection's SQL dialect, completes table and column names and can run just the selected text. The grid handles wide and long results: it scrolls in both directions under a pinned header, and a click on a cell shows its full value. Statements run read-only unless a permitted user explicitly allows writes.

**Creating objects.** Create a database, a schema or a table from the query browser, with a small table designer. The database server decides whether the account is allowed to.

**Backup and recovery.** Logical backups, encrypted before they reach disk, with integrity verification and restore into any connection of the same kind.

**Connection status.** The Connections page shows which servers are answering, with response time, and checks again every thirty seconds.

**Server administration.** Live sessions with cancel and terminate, database sizes, replication slots and the WAL they retain, and the settings that matter for replication and security.

**Organization controls.** Users with five roles, encrypted credential storage, a hash-chained audit log that records every statement and every change, and transport safeguards on by default.

## Status

| Capability | PostgreSQL | MySQL |
|---|---|---|
| Online version upgrade | Working (source 10+) | Working (source 5.6+) |
| Query browser and schema explorer | Working | Working |
| Encrypted backup, verify, restore | Working | Working |
| Server console | Working | Working |
| Create database, schema, table | Working | Working |

| Migration path | State |
|---|---|
| PostgreSQL → PostgreSQL | Working |
| MySQL → MySQL | Working |
| MySQL → PostgreSQL | Working: tables, keys, indexes and foreign keys are translated; views, routines and triggers are not |
| PostgreSQL → MySQL | Not built yet |

All three pipelines are covered by end-to-end tests that migrate a database under write load, pause and resume mid-stream, cut over with a read-only fence and compare the result. The PostgreSQL test runs 16 → 18. The MySQL tests use MySQL 8.0 (to MySQL 8.0, and to PostgreSQL 18); other MySQL versions have not been exercised yet.

Not built yet: PostgreSQL → MySQL, document stores to relational (MongoDB → PostgreSQL), continuous two-way sync, scheduled backups, point-in-time recovery, single sign-on, and external key management. See the [roadmap](#roadmap).

## Programs

All three share one engine and one data directory, so a migration started in one can be watched or controlled from another.

| Program | What it is | Built from |
|---|---|---|
| `dedie-server` | Web interface, HTTP API and the worker that runs migrations. For teams. | `apps/server` (Go) |
| `dedie` | Command-line interface. Works directly on the data directory; needs no server. | `apps/cli` (Go) |
| Dedie Desktop | A native window for one person on their own machine. | `apps/desktop` (JavaFX) |

## Quick start

Requirements:

- Go 1.26 or newer and Node 22 or newer, to build
- JDK 17 or newer and Maven, for the desktop application only
- the database client tools on the machine that runs Dedie, for backups: `pg_dump` and `pg_restore` (at least as new as the servers you back up), `mysqldump` and `mysql`

```sh
git clone https://github.com/Eselase-Noble/Dedie.git
cd Dedie
make                # builds the web interface, bin/dedie-server and bin/dedie
bin/dedie-server    # http://127.0.0.1:8080
```

The first screen is the sign-in page. On a new installation choose **Create account** there to set up your organization and its owner, then add connections. Further users are added by owners and admins from the Users page.

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
| Connections | Add, edit, test and delete connections, and see which servers are online. Passwords are write-only; weak SSL modes are flagged. |
| Query browser | Browse schemas, write SQL, run all of it or just the selection with Ctrl/Cmd+Enter, inspect cells, export results as CSV, and create databases, schemas and tables. |
| Migrations | Create a migration at database, schema or table level; run the preflight check; watch phase, lag and per-table progress; pause, resume, cut over or abort. |
| Backups | Create, verify, restore and delete backups; review restore history and logs. |
| Server activity | Inspect sessions, sizes, replication slots and settings; cancel or terminate a session. |
| Users | Create users, change roles, disable accounts, reset passwords. |
| Audit log | Read the log and verify that it has not been altered. |

Navigation and actions a role may not use are hidden; the server enforces the same rules regardless.

## The command line

```sh
dedie init --org "Acme" --name "Ada Lovelace" --email ada@example.com

dedie conn add --name prod-16 --host db-old.internal --db shop --user migrator --sslmode verify-full
dedie conn add --kind mysql --name orders --host mysql.internal --db orders --user migrator
dedie conn list
dedie conn test prod-16

dedie query prod-16 "select count(*) from orders"      # read-only unless --write
dedie backup create prod-16 --note "before upgrade"
dedie user add --name "Kofi Mensah" --email kofi@example.com --role operator
dedie audit verify
```

Passwords are prompted for, or read from `DEDIE_PASSWORD`; they are never accepted as flags, where they would end up in shell history and the process list. Run `dedie help` for every command.

The CLI acts with owner rights, because anyone who can read the data directory and master key already has that access. Its actions are audited under the operating-system user name.

## The desktop application

Dedie Desktop is a JavaFX application. It starts the same `dedie-server` engine on a free loopback port, which nothing outside the machine can reach, and shows the interface in its own window. There is no second implementation of anything: queries, migrations, backups and access control behave exactly as they do on a server.

```sh
make desktop
java -jar bin/desktop/dedie-desktop.jar
```

`bin/desktop` is self-contained: the application, its JavaFX libraries and the server program. Closing the window stops the engine cleanly; a running migration is handed back to the queue and resumes the next time the application starts.

The application looks for `dedie-server` beside its own jar, then in a `bin` directory above the working directory, then on the `PATH`. Set `DEDIE_SERVER` to point at it explicitly. During development, `cd apps/desktop && mvn javafx:run` starts it from the source tree.

One limitation: the embedded browser cannot save files, so CSV export from the query browser works in the web interface but not in the desktop window.

## Migrating a database

```sh
dedie migrate create --name "shop 16 to 18" --source prod-16 --target prod-18 --fence
dedie migrate preflight <id>     # changes nothing; lists everything that would block or weaken the run
dedie migrate run <id>           # or `dedie migrate start <id>` to let a running dedie-server execute it
dedie migrate status <id>
dedie migrate cutover <id>       # when lag is near zero and you are ready to switch
```

Supported pairs are PostgreSQL → PostgreSQL, MySQL → MySQL and MySQL → PostgreSQL. The target must not already contain the tables being migrated.

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

### For all pipelines

- Schema changes made on the source while a migration is running are not replicated. Freeze the schema until cutover. `TRUNCATE` is replicated.
- Aborting removes what the migration created on the source and leaves the copied tables on the target for inspection.
- With `--fence` the source stays read-only after a successful cutover, which prevents stray writes to the old database.
- Fencing stops well-behaved applications. It is not a security control.
- Checksum validation reads every row on both sides during the write pause. For very large databases choose `--validation count`.

## Backups

Backups are logical (`pg_dump` custom format, or `mysqldump`), encrypted before they reach disk, and stored under `backups/` in the data directory with a SHA-256 of the encrypted file.

```sh
dedie backup create prod-18 --note "nightly"
dedie backup list
dedie backup verify <id>          # checksum matches and the whole archive decrypts
dedie backup restore <id> --target staging --clean --confirm shop_staging
```

A restore verifies the file first and requires the target database name as confirmation.

- **PostgreSQL** restores run in a single transaction, so a failure changes nothing. Ownership and privileges are left out unless `--keep-owners` is given, since the target server often has different roles.
- **MySQL** restores replace tables of the same name and cannot be rolled back: MySQL commits at every schema statement, so a restore that fails part-way leaves the tables it had already replaced.

Backups are taken on demand. Scheduling and point-in-time recovery are on the roadmap.

## Security model

**Secrets at rest.** Database passwords are sealed with AES-256-GCM under a per-secret data key, which is itself wrapped by the master key (envelope encryption). Each ciphertext is bound to its organization and connection, so it cannot be copied onto another record. Backups are encrypted as a stream of authenticated chunks; truncation, reordering and modification are all detected on read.

**Master key.** Read from `DEDIE_MASTER_KEY` (32 bytes, base64) or from `master.key` in the data directory, created with mode 0600 on first run. Whoever holds this key can decrypt stored credentials and backups. Keep it out of backups of the data directory, and store a copy somewhere safe: without it, saved connections and backups are unrecoverable.

**Sign-in.** Passwords are hashed with Argon2id. Sessions are random 256-bit tokens kept in an `HttpOnly`, `SameSite=Strict` cookie; only their SHA-256 is stored. Five failed attempts lock an account for fifteen minutes, and sign-in is rate-limited per address. Changing a user's password, role or status ends their sessions.

**Roles.**

| Role | May |
|---|---|
| owner | everything, including managing other owners |
| admin | everything except creating or changing owners |
| operator | connections, read and write queries, migrations, backups, restores, server console |
| analyst | read-only queries, view connections and migrations |
| viewer | view connections and migrations |

An organization always keeps at least one active owner.

**Queries.** The browser runs statements in a read-only session unless the user has write permission, the connection is not marked read-only, and they switch writes on for that run. The database server enforces it. This is a guard rail; the real boundary is the database account a connection uses, so give each connection the least privilege it needs.

**Audit log.** Sign-ins, every executed statement, and every change to connections, users, migrations and backups are recorded. Each entry includes a hash of the previous one, so an edited or deleted entry breaks the chain; `dedie audit verify` or the Audit page reports where. Removing entries from the end of the log is not detectable from the log alone, so export it regularly if you need that guarantee.

**Transport.** New connections default to `sslmode=require`; `verify-full` with your CA certificate is recommended, and preflight warns when a migration would run over an unencrypted connection. The web server sets a content security policy, forbids framing, and rejects state-changing requests that lack its custom header or come from another origin.

**Client tools.** Backup tools receive credentials through the environment or a private temporary option file, never on the command line.

**Data directory.** Created with mode 0700; the metadata database and backups with 0600.

## HTTP API

Everything the interface does goes through a JSON API under `/api/v1`. Requests are authenticated by the session cookie; every request that changes state must carry the header `X-Dedie-Request: 1`. Errors are `{"error": "..."}` with a matching status.

| Area | Endpoints |
|---|---|
| Setup and session | `GET/POST /setup`, `POST /auth/login`, `POST /auth/logout`, `GET /auth/me` |
| Users | `GET/POST /users`, `PATCH /users/{id}` |
| Connections | `GET/POST /connections`, `GET/PUT/DELETE /connections/{id}`, `POST /connections/test`, `POST /connections/{id}/test`, `GET /connections/status` |
| Objects | `POST /connections/{id}/databases`, `POST /connections/{id}/schemas`, `POST /connections/{id}/tables` |
| Query | `GET /connections/{id}/catalog`, `POST /connections/{id}/query` |
| Server console | `GET /connections/{id}/overview`, `POST /connections/{id}/backends/{pid}/cancel` |
| Migrations | `GET/POST /migrations`, `GET /migrations/{id}`, `POST /migrations/{id}/preflight`, `POST /migrations/{id}/actions/{start,pause,resume,cutover,abort,delete}` |
| Backups | `GET/POST /backups`, `DELETE /backups/{id}`, `POST /backups/{id}/verify`, `POST /backups/{id}/restore`, `GET /restores` |
| Audit | `GET /audit`, `GET /audit/verify` |

## Architecture

```
apps/
  server/            dedie-server: HTTP listener, worker, graceful shutdown
  cli/               dedie: one file per command group
  desktop/           Dedie Desktop: JavaFX window that runs the server as its engine
  internal/httpapi/  routes, middleware and handlers
engine/
  app/               use cases: authorization, auditing, orchestration
  connector/         driver contract and registry
    postgres/        connection, query execution, catalog, administration
    mysql/           the same for MySQL
  migrate/           options, phases, reporting and the pipeline registry
    pgpg/            PostgreSQL → PostgreSQL, one file per phase
    mymy/            MySQL → MySQL, one file per phase; also the binary log reader other pipelines reuse
    mypg/            MySQL → PostgreSQL: schema translation, value conversion, PostgreSQL applier
  backup/            pg_dump / pg_restore and mysqldump / mysql drivers
  secure/            envelope and stream encryption, password hashing, tokens
  store/             metadata database (SQLite), one file per entity
web/                 Next.js interface, exported statically and embedded in the server
```

Every front end calls `engine/app` and nothing beneath it, so a permission check or audit entry cannot be bypassed by using a different interface.

Two registries keep the engine open to new databases. A driver implements `connector.Driver` and registers under its kind, which makes it available to the query browser, the server console and connection management. A pipeline implements `migrate.Pipeline` and registers for a source and target kind; the application layer looks pipelines up by that pair and knows nothing else about them.

Migration jobs are leased in the metadata database with a heartbeat. Several processes can share a data directory without running a job twice, and a job whose worker dies is picked up by the next one.

How each pipeline gets exactly-once delivery:

| | PostgreSQL | MySQL |
|---|---|---|
| Consistent start | replication slot created together with an exported snapshot | read transactions opened at a recorded binary log position under a brief global read lock |
| Change stream | logical replication (`pgoutput`) | row events from the binary log |
| Checkpoint | commit LSN, stored on the target in the same transaction as the data | binary log file and offset, stored the same way |
| Cutover marker | a logical message written to the WAL after fencing | the end of the binary log after fencing |

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

# MySQL: the source needs binary logging in ROW format. The MySQL to
# PostgreSQL test uses DEDIE_TEST_MYSQL_SOURCE and DEDIE_TEST_TARGET.
export DEDIE_TEST_MYSQL_SOURCE=mysql://root@127.0.0.1:33161
export DEDIE_TEST_MYSQL_TARGET=mysql://root@127.0.0.1:33162

make test-integration
```

To check a desktop build without watching the window, run it with `-Ddedie.smoke=true`: it loads the interface, prints what it rendered and exits.

### Continuous integration

`.github/workflows/ci.yml` lints, runs the unit tests, runs the PostgreSQL 16 → 18 migration test, builds the server and CLI for Linux, macOS and Windows, and builds the desktop application on a runner of each operating system. Pushing a tag that starts with `v` publishes everything as a GitHub release with checksums. The MySQL migration tests are run locally for now.

### Branching

`main` holds released work. Each feature is developed on a `feature/...` branch and merged through a pull request once its tests pass.

## Configuration reference

| Setting | Meaning | Default |
|---|---|---|
| `DEDIE_DATA_DIR`, `--data` | metadata database, master key, backups | `~/.dedie` |
| `DEDIE_MASTER_KEY` | master key, base64 of 32 bytes | `master.key` in the data directory |
| `DEDIE_PG_BIN` | directory containing `pg_dump` and `pg_restore` | newest version found on the system |
| `DEDIE_MYSQL_BIN` | directory containing `mysqldump` and `mysql` | the `PATH` |
| `DEDIE_PASSWORD` | password for CLI commands that would prompt | prompt |
| `DEDIE_SERVER` | location of `dedie-server` for the desktop application | beside the jar, then `PATH` |
| `--addr` | listen address of `dedie-server`; port 0 picks a free port | `127.0.0.1:8080` |
| `--tls-cert`, `--tls-key` | serve HTTPS | off |
| `--behind-tls-proxy` | trust `X-Forwarded-For`, mark cookies `Secure` | off |
| `--no-worker` | serve the API without executing migrations | off |

What the data directory holds:

| Path | Contents |
|---|---|
| `dedie.db` | organizations, users, sessions, connections (passwords encrypted), jobs, audit log |
| `master.key` | the master key, unless `DEDIE_MASTER_KEY` is set |
| `backups/` | encrypted backup files |
| `desktop/` | the desktop window's local preferences |

## Roadmap

1. **PostgreSQL → MySQL**, the reverse of the cross-engine path that exists today, and continuous two-way sync.
2. **Document to relational**: MongoDB → PostgreSQL, with a reviewed mapping from documents to tables.
3. **Scheduled backups and retention**, then point-in-time recovery.
4. **Partitioned tables** for PostgreSQL, and tested upgrade paths across more MySQL versions.
5. **Single sign-on** and **external key management** (cloud KMS, Vault) behind the existing key interface.
