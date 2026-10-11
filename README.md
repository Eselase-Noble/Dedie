# Dedie

A query browser, administration console and live migration engine for relational databases.

*Dedie* is Ewe for "safe": the data should arrive safely and stay that way.

Dedie moves a production database to a newer major version while the old one stays online. It copies a consistent snapshot, follows the source's replication log to stay in step, and switches over in a write pause measured in seconds. The same engine powers a SQL browser, encrypted backups with verified restore, and a server activity console, behind role-based access control and a tamper-evident audit log.

It ships as three programs that share one engine and one data directory:

| Program | What it is |
|---|---|
| `dedie-server` | Web interface, HTTP API and the background worker that runs migrations. For teams. |
| `dedie` | Command-line interface. Works directly on the data directory; needs no server. |
| `dedie-desktop` | The same interface for one person, bound to a private loopback port. |

## Status

| Capability | State |
|---|---|
| PostgreSQL → PostgreSQL online version upgrade (source 10+, any newer target) | Working, covered by an end-to-end test that migrates 16 → 18 under write load |
| Query browser, schema explorer | Working (PostgreSQL) |
| Encrypted backup, verify, restore | Working (PostgreSQL, logical backups via `pg_dump`) |
| Server console: sessions, replication slots, sizes, cancel/terminate | Working (PostgreSQL) |
| Organizations, users, roles, audit log | Working |
| MySQL → MySQL online upgrade | Planned |
| Cross-engine sync (MySQL ↔ PostgreSQL) | Planned |
| Document → relational (MongoDB → PostgreSQL) | Planned |
| Scheduled backups, point-in-time recovery | Planned |
| Single sign-on, external key management (KMS, Vault) | Planned; the key interface is already pluggable |

New databases plug in behind one interface (`engine/connector`), and new migration paths sit beside the existing one (`engine/migrate`).

## Quick start

Requirements: Go 1.26+, Node 22+ (to build the interface), and the PostgreSQL client tools (`pg_dump`, `pg_restore`) on the machine that runs Dedie, at a version at least as new as the servers you back up.

```sh
make            # builds the web interface and bin/dedie, bin/dedie-server, bin/dedie-desktop
bin/dedie-server  # http://127.0.0.1:8080
```

Open the address, create your organization and owner account, then add connections.

The server listens on loopback by default and refuses to serve plain HTTP on any other address, because sessions and database credentials would cross the network unencrypted. To expose it:

```sh
bin/dedie-server --addr 0.0.0.0:8443 --tls-cert server.crt --tls-key server.key
# or, behind a reverse proxy that terminates TLS:
bin/dedie-server --addr 127.0.0.1:8080 --behind-tls-proxy
```

### The same thing from the command line

```sh
dedie init --org "Acme" --name "Ada Lovelace" --email ada@example.com

dedie conn add --name prod-16 --host db-old.internal --db shop --user migrator --sslmode verify-full
dedie conn add --name prod-18 --host db-new.internal --db shop --user migrator --sslmode verify-full

dedie query prod-16 "select count(*) from orders"      # read-only unless --write
dedie backup create prod-16 --note "before upgrade"
```

Passwords are prompted for, or read from `DEDIE_PASSWORD`; they are never accepted as flags.

## Migrating a database

```sh
dedie migrate create --name "shop 16 to 18" --source prod-16 --target prod-18 --fence
dedie migrate preflight <id>     # changes nothing; lists everything that would block or weaken the run
dedie migrate run <id>           # or `dedie migrate start <id>` to let a running dedie-server execute it
dedie migrate status <id>
dedie migrate cutover <id>       # when lag is near zero and you are ready to switch
```

What happens, phase by phase:

1. **Schema.** Types, sequences, functions and tables are recreated on the target from PostgreSQL's own definitions. A publication is created on the source.
2. **Snapshot.** A replication slot is created and exports a snapshot in the same step. Every table is copied in parallel from that snapshot, so all tables reflect one instant and the change stream begins exactly where the copy ends.
3. **Indexing.** Secondary indexes, foreign keys and views are built after the bulk load.
4. **Streaming.** Each source transaction is applied to the target as one transaction, together with its position in a checkpoint table. If the process dies, it resumes from that checkpoint without losing or repeating a change. The job can stay in this phase for as long as you like.
5. **Cutover.** With `--fence`, the source database is made read-only and its clients are disconnected. The stream is drained to a marker written after that point, sequences are synchronized, every table is compared between source and target (row count plus checksum), triggers are created, and the replication slot and publication are removed.

Point your application at the target once cutover completes. Applications are unable to write only during step 5.

If validation finds a difference, cutover is cancelled, the source is made writable again and streaming continues. Nothing is switched on a mismatch.

### Requirements on the source

- `wal_level = logical` (needs a server restart if it is not already set)
- a user with the `REPLICATION` attribute that owns the tables, or a superuser
- a free replication slot and WAL sender
- a primary key or replica identity on every migrated table

Preflight checks all of these.

### What is not migrated

Preflight reports these so nothing is dropped silently:

- roles, ownership and `GRANT`s
- row-level security policies, rewrite rules, event triggers, user-defined aggregates
- large objects and foreign tables
- partitioned tables, inheritance children and unlogged tables (skipped, with a warning)
- schema changes made on the source while a migration is running; logical replication does not carry DDL, so freeze the schema until cutover

### Operational notes

- A paused or failed job keeps its replication slot, and the source keeps WAL for it. Resume or abort jobs you are not going to finish; the server console highlights slots that retain WAL.
- Aborting removes the slot and publication from the source and leaves the copied tables on the target for inspection.
- With `--fence` the source stays read-only after a successful cutover, which prevents stray writes to the old database. To go back: `ALTER DATABASE <name> RESET default_transaction_read_only;`
- Fencing stops well-behaved applications. It is not a security control: a session can still override the setting.
- Checksum validation reads every row on both sides during the write pause. For very large databases choose `--validation count`.

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

**Queries.** The browser runs statements in a read-only transaction unless the user has write permission, the connection is not marked read-only, and they switch writes on for that run. This is a guard rail. The real boundary is the database role a connection uses: give each connection the least privilege it needs.

**Audit log.** Sign-ins, every executed statement, and every change to connections, users, migrations and backups are recorded. Each entry includes a hash of the previous one, so an edited or deleted entry breaks the chain; `dedie audit verify` or the Audit page reports where. Removing entries from the end of the log is not detectable from the log alone, so export it regularly if you need that guarantee.

**Transport.** New connections default to `sslmode=require`; `verify-full` with your CA certificate is recommended, and preflight warns when a migration would run over an unencrypted connection. The web server sets a content security policy, forbids framing, and rejects state-changing requests that lack its custom header or come from another origin.

**Local programs.** `dedie` and `dedie-desktop` act with owner rights, because anyone who can read the data directory and master key already has that access. Protect the directory with file permissions; it is created with mode 0700.

## Backups

Backups are logical (`pg_dump` custom format), encrypted before they reach disk, and stored under `backups/` in the data directory with a SHA-256 of the encrypted file.

```sh
dedie backup create prod-18 --note "nightly"
dedie backup list
dedie backup verify <id>          # checksum matches and the whole archive decrypts
dedie backup restore <id> --target staging --clean --confirm shop_staging
```

A restore verifies the file first, runs in a single transaction so a failure changes nothing, and requires the target database name as confirmation. Ownership and privileges are left out unless `--keep-owners` is given, since the target server often has different roles.

## Architecture

```
apps/
  server/            dedie-server: HTTP listener, worker, graceful shutdown
  cli/               dedie: one file per command group
  desktop/           dedie-desktop: loopback listener and window launcher
  internal/httpapi/  routes, middleware and handlers shared by server and desktop
engine/
  app/               use cases: authorization, auditing, orchestration
  connector/         driver contract and registry
    postgres/        connection, query execution, catalog, administration
  migrate/
    pgpg/            PostgreSQL → PostgreSQL pipeline, one file per phase
  backup/            pg_dump / pg_restore drivers
  secure/            envelope and stream encryption, password hashing, tokens
  store/             metadata database (SQLite), one file per entity
web/                 Next.js interface, exported statically and embedded in the binaries
```

Every front end calls `engine/app` and nothing beneath it, so a permission check or audit entry cannot be bypassed by using a different interface.

Migration jobs are leased in the metadata database with a heartbeat. Several processes can share a data directory without running a job twice, and a job whose worker dies is picked up by the next one.

## Development

```sh
make test               # unit tests; needs neither Node nor a database
make lint               # gofmt and go vet
cd web && npm run dev   # interface with hot reload, proxying /api to 127.0.0.1:8080
```

The end-to-end test needs two PostgreSQL servers, the source with `wal_level = logical`. It creates and drops its own databases:

```sh
export DEDIE_TEST_SOURCE=postgres://postgres@127.0.0.1:54316/postgres
export DEDIE_TEST_TARGET=postgres://postgres@127.0.0.1:54318/postgres
make test-integration
```

To compile without building the interface, pass `-tags noui`.

### Continuous integration

`.github/workflows/ci.yml` lints, runs the unit tests, runs the end-to-end migration against PostgreSQL 16 and 18, and builds binaries for Linux, macOS and Windows. Pushing a tag that starts with `v` publishes them as a GitHub release with checksums.

## Configuration reference

| Setting | Meaning | Default |
|---|---|---|
| `DEDIE_DATA_DIR`, `--data` | metadata database, master key, backups | `~/.dedie` |
| `DEDIE_MASTER_KEY` | master key, base64 of 32 bytes | `master.key` in the data directory |
| `DEDIE_PG_BIN` | directory containing `pg_dump` and `pg_restore` | newest version found on the system |
| `DEDIE_PASSWORD` | password for CLI commands that would prompt | prompt |
| `--addr` | listen address of `dedie-server` | `127.0.0.1:8080` |
| `--tls-cert`, `--tls-key` | serve HTTPS | off |
| `--behind-tls-proxy` | trust `X-Forwarded-For`, mark cookies `Secure` | off |
| `--no-worker` | serve the API without executing migrations | off |
