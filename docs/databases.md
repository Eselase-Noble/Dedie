# Preparing your databases

What each kind of database needs before Dedie can migrate it: the settings on the server and the privileges of the account in the connection. Run `dedie migrate preflight <id>` (or **Preflight** in the web interface) once the connections exist. It changes nothing and names every item below that is still missing.

- [Online or offline](#online-or-offline)
- [PostgreSQL as a source](#postgresql-as-a-source)
- [MySQL as a source](#mysql-as-a-source)
- [MongoDB as a source](#mongodb-as-a-source)
- [Oracle as a source](#oracle-as-a-source)
- [The target](#the-target)

## Online or offline

| | Online migration (and continuous sync) | Offline migration |
|---|---|---|
| What happens | The source is copied, then every later change is followed until you cut over | The source is copied once, as it stood at one moment, validated, and the job ends |
| Applications | Keep working; they pause for seconds at cutover | Should not write while the copy runs, or what they write is left behind |
| Source setup | The database's change log must be readable: logical replication, the binary log or a replica set. Oracle needs nothing extra, and follows more cheaply with LogMiner set up | None beyond an account that can read the tables |
| Choose it for | Production databases that cannot stop | Development and test copies, small databases, a maintenance window, or a source you may not reconfigure |

The offline requirements are always a subset of the online ones. Each section lists them separately.

## PostgreSQL as a source

Offline: a role that can `SELECT` from the tables.

Online:

```conf
# postgresql.conf; changing wal_level needs a restart
wal_level = logical
max_replication_slots = 10      # at least one free
max_wal_senders = 10            # at least one free
```

```sql
CREATE ROLE migrator LOGIN REPLICATION PASSWORD '...';
-- The role must own the tables it publishes, or be a superuser.
```

and a line in `pg_hba.conf` that lets the Dedie machine connect to the database as that role. Every migrated table needs a primary key or a replica identity.

## MySQL as a source

Offline:

```sql
CREATE USER 'migrator'@'%' IDENTIFIED BY '...';
GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON *.* TO 'migrator'@'%';
GRANT RELOAD ON *.* TO 'migrator'@'%';   -- optional: lets the copy use several workers
```

Online, additionally:

```conf
# my.cnf
log_bin = ON
binlog_format = ROW
binlog_row_image = FULL
server_id = 1
```

```sql
GRANT RELOAD, REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'migrator'@'%';
```

Tables must be InnoDB. MariaDB is not supported.

## MongoDB as a source

Offline: an account that can read the database. A standalone server is fine.

Online: the server must be a member of a replica set (a single node started with `--replSet` and initiated counts), version 4.0 or newer, and the account must be able to open a change stream on the database (`read` on it is enough).

## Oracle as a source

Dedie connects to a **service**. On a multitenant server that is the service of the pluggable database holding the schemas, for example `FREEPDB1` or `ORCLPDB1`; `lsnrctl status` on the database host lists them. No Oracle client software is needed on the Dedie machine.

Supported sources are Oracle 12c (12.1) and newer.

### Offline

An account that can read the tables as of a past moment:

```sql
CREATE USER migrator IDENTIFIED BY "...";
GRANT CREATE SESSION TO migrator;
GRANT SELECT ANY TABLE, FLASHBACK ANY TABLE TO migrator;   -- or SELECT and FLASHBACK on each table
GRANT SELECT ANY DICTIONARY TO migrator;                   -- to read the current SCN and UNDO_RETENTION
```

The schema's own user needs none of this for its own tables, apart from a way to read the SCN (`GRANT SELECT ON V_$DATABASE` or `GRANT EXECUTE ON DBMS_FLASHBACK`).

The copy reads every table as of one system change number, using undo data. If copying a table takes longer than the database keeps undo, Oracle answers "snapshot too old" (ORA-01555). Raise the retention for the duration:

```sql
ALTER SYSTEM SET UNDO_RETENTION = 14400;   -- seconds; make it longer than the copy will take
```

### Online

After the copy, Dedie finds the rows that later transactions changed, reads each of them back by its primary key and stores it on the target. It has two ways of finding them and chooses by itself; preflight tells you which one a job will use under **Following changes**.

| | The redo stream (LogMiner) | Row versions (undo data) |
|---|---|---|
| Needs | supplemental logging, the LogMiner privileges, and redo that stays readable | nothing beyond what the copy needs |
| Cost on the source | reads only the changes | reads the primary key index of every followed table on each pass |
| Used | whenever the source is set up for it | for tables with a `JSON` or `BOOLEAN` column, and for every table when LogMiner is not set up, not allowed through the connection, or has lost the redo it still had to read |

A job that starts on the redo stream and loses it, because the source reused a redo log it had not archived, does not stop: it carries on from its last checkpoint through row versions and says so in its events. Nothing is lost by the switch. Row versions come from undo data, so the one limit is `UNDO_RETENTION`: a job paused or behind for longer than the source keeps undo cannot find what it missed and has to start again with a new copy.

For a large or busy production source the redo stream is the better choice. Three things put it in place.

**1. Supplemental logging**, switched on once by a DBA. It takes effect immediately and needs no restart:

```sql
-- in a pluggable database (19c and newer):
ALTER PLUGGABLE DATABASE ADD SUPPLEMENTAL LOG DATA;
-- in a database that is not a container, or for every pluggable database at once from the root:
ALTER DATABASE ADD SUPPLEMENTAL LOG DATA;
```

Dedie then switches on primary key logging for exactly the tables it migrates (`ALTER TABLE ... ADD SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS`) when the job starts, and off again when it finishes. That needs the account to own the tables or hold `ALTER` on them; without it the job follows row versions instead. A DBA can also switch it on for the whole database: `ALTER DATABASE ADD SUPPLEMENTAL LOG DATA (PRIMARY KEY) COLUMNS`.

**2. Privileges for the account:**

```sql
GRANT LOGMINING, SELECT ANY TRANSACTION TO migrator;
GRANT EXECUTE_CATALOG_ROLE TO migrator;      -- to call DBMS_LOGMNR
GRANT SELECT ANY DICTIONARY TO migrator;     -- V$DATABASE, V$TRANSACTION, V$LOGMNR_CONTENTS, V$LOG
```

**3. Redo that stays long enough.** In `NOARCHIVELOG` mode only the online redo logs exist, and they are reused once full. From inside a pluggable database it is stricter still: LogMiner can read only the *current* online log, so a log switch while the job still has earlier changes to read puts them out of its reach. The job then goes on through row versions, as described above. To stay on the redo stream, run the source in `ARCHIVELOG` mode and keep the archived logs until the job has read them:

```sql
-- as SYSDBA, in a maintenance window: this restarts the database
SHUTDOWN IMMEDIATE;
STARTUP MOUNT;
ALTER DATABASE ARCHIVELOG;
ALTER DATABASE OPEN;
```

Every followed table needs a primary key. A table without one is copied, and preflight warns that later changes to it are not followed.

A table that is truncated, or whose definition changes, while it is followed through row versions has all of its rows brought over again; the target table keeps the shape it was created with.

For **Oracle to Oracle**, the source account also needs `SELECT_CATALOG_ROLE` to read the definitions of objects in schemas other than its own.

### Oracle 12c

- **A database that is not a container** (the common 12c arrangement) works both offline and online. LogMiner there has to be told which redo log files to read; Dedie registers the online and archived logs that cover the range it needs, which is why the account needs to see `V$LOG`, `V$LOGFILE` and `V$ARCHIVED_LOG` (`SELECT ANY DICTIONARY` covers them).
- **A pluggable database on 12c** works offline, and online through row versions: that release runs LogMiner only from the container root, which preflight reports under "LogMiner access".
- `ALTER PLUGGABLE DATABASE ADD SUPPLEMENTAL LOG DATA` does not exist in 12c; use `ALTER DATABASE ADD SUPPLEMENTAL LOG DATA`.

The pipelines are tested end to end against Oracle 23ai. The 12c behaviour above follows Oracle's documentation for that release and the code paths it needs are exercised on 23ai, but a 12c server has not been part of the test runs; run preflight and a trial migration of a copy before relying on it.

## The target

| Target | The account needs | The database needs |
|---|---|---|
| PostgreSQL | `CREATE` on the database; ownership of what it creates | `ENCODING 'UTF8'`; none of the tables being migrated |
| MySQL | all privileges on the target database | `utf8mb4`; none of the tables being migrated |
| Oracle | to be the schema that receives the objects, with `CREATE TABLE`, `CREATE SEQUENCE`, `CREATE VIEW`, `CREATE PROCEDURE`, `CREATE TRIGGER`, `CREATE SYNONYM`, `CREATE TYPE` and a quota on its tablespace; or the `ANY` forms of those to create in other schemas | the same release as the source or newer; the receiving schemas (users) already created |

A target is never emptied without being told to. If a table or another object to be migrated already exists there, preflight lists it and the migration waits for your choice: refuse, keep it and skip it, or replace it.
