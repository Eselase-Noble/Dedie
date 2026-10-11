# Setting up Dedie

This guide takes a new installation from nothing to a server your team can sign in to: installing the programs, starting the server, putting it behind TLS, running it as a service, and looking after its data directory. Preparing the databases themselves is covered in [Preparing your databases](databases.md).

- [What you install](#what-you-install)
- [Requirements](#requirements)
- [Install from a release](#install-from-a-release)
- [Build from source](#build-from-source)
- [First start](#first-start)
- [Serving it to a team](#serving-it-to-a-team)
- [Running as a service](#running-as-a-service)
- [The command line on other machines](#the-command-line-on-other-machines)
- [The desktop application](#the-desktop-application)
- [The data directory and the master key](#the-data-directory-and-the-master-key)
- [Backing up Dedie itself](#backing-up-dedie-itself)
- [Upgrading](#upgrading)
- [Troubleshooting](#troubleshooting)

## What you install

| Program | Install it when |
|---|---|
| `dedie-server` | a team will use Dedie. It serves the web interface and the HTTP API and runs the migrations, scheduled backups and health checks |
| `dedie` | you want to drive a Dedie server from a terminal or from CI, or to set an installation up on the server host |
| Dedie Desktop | one person will use Dedie on their own machine. It contains its own copy of the engine and needs no server |

One machine runs `dedie-server`. It must be able to reach every database it manages over the network; the databases do not need to reach it, and nothing is installed on them.

## Requirements

To run:

- Linux, macOS or Windows on x86-64 or ARM64. The server and the command line are single files with no dependencies.
- For **backups** only, the database's own client tools on the machine that runs Dedie: `pg_dump`, `pg_restore` and `pg_basebackup` at least as new as the PostgreSQL servers you back up, and `mysqldump`, `mysql` and `mysqlbinlog` for MySQL. Migrations, the query browser and monitoring need none of them.
- Nothing extra for Oracle or MongoDB: Dedie speaks their protocols itself, so no Oracle client or Instant Client is installed.
- Java 17 or newer, for the desktop application only.

To build from source: Go 1.26 or newer and Node 22 or newer; JDK 17 and Maven for the desktop application.

## Install from a release

Download the archive for your platform from the [releases page](https://github.com/Eselase-Noble/Dedie/releases) and check it against `SHA256SUMS`:

```sh
curl -LO https://github.com/Eselase-Noble/Dedie/releases/latest/download/dedie-linux-amd64.tar.gz
curl -LO https://github.com/Eselase-Noble/Dedie/releases/latest/download/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS

sudo tar -C /usr/local/bin -xzf dedie-linux-amd64.tar.gz    # dedie and dedie-server
dedie-server --help
```

Archives are named `dedie-<os>-<arch>.tar.gz` (`linux`, `darwin`, `windows`; `amd64`, `arm64`) and `dedie-desktop-<os>.tar.gz`.

## Build from source

```sh
git clone https://github.com/Eselase-Noble/Dedie.git
cd Dedie
make            # the web interface, bin/dedie-server and bin/dedie
make desktop    # optional: bin/desktop, the desktop application
```

## First start

```sh
dedie-server
```

Open http://127.0.0.1:8080. On a new installation the sign-in page offers **Create account**: that creates your organization and its owner. From there:

1. **Connections → Add connection** for each database. Passwords are encrypted before they are stored and are never shown again. Use **Test** before saving.
2. **Dedie users** to invite the rest of the team, each with a role (owner, admin, operator, analyst or viewer).
3. **Account → Two-factor sign-in** for yourself, and **Security policy** if everyone should be required to use it.

The same can be done without a browser, on the server host:

```sh
dedie init --org "Acme" --name "Ada Lovelace" --email ada@example.com    # asks for a password
dedie user add --name "Kofi Mensah" --email kofi@example.com --role operator
```

## Serving it to a team

The server listens on the local machine only, and refuses to serve plain HTTP on any other address: sessions and database credentials would cross the network unencrypted. There are two ways to open it up.

**With its own certificate:**

```sh
dedie-server --addr 0.0.0.0:8443 --tls-cert /etc/dedie/server.crt --tls-key /etc/dedie/server.key
```

**Behind a reverse proxy that terminates TLS**, which is the usual arrangement:

```sh
dedie-server --addr 127.0.0.1:8080 --behind-tls-proxy
```

```nginx
server {
    listen 443 ssl;
    server_name dedie.example.com;
    ssl_certificate     /etc/letsencrypt/live/dedie.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/dedie.example.com/privkey.pem;

    client_max_body_size 0;          # backup uploads and downloads are large
    proxy_read_timeout 3600s;        # so are long-running queries and exports

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

`--behind-tls-proxy` makes the server trust `X-Forwarded-For` for the audit log and mark its cookies `Secure`. Use it only when a proxy really is in front; otherwise a client could forge its address.

Firewall the machine so that only the proxy port is reachable, and allow it outbound to your database servers.

## Running as a service

On Linux with systemd, give Dedie a user and a directory of its own:

```sh
sudo useradd --system --home /var/lib/dedie --create-home --shell /usr/sbin/nologin dedie
```

`/etc/systemd/system/dedie.service`:

```ini
[Unit]
Description=Dedie server
After=network-online.target
Wants=network-online.target

[Service]
User=dedie
Group=dedie
Environment=DEDIE_DATA_DIR=/var/lib/dedie
ExecStart=/usr/local/bin/dedie-server --addr 127.0.0.1:8080 --behind-tls-proxy
Restart=on-failure
RestartSec=5
# A running migration is handed back to the queue on shutdown and resumes
# on the next start; give it time to stop cleanly.
TimeoutStopSec=60
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now dedie
journalctl -u dedie -f
```

To use the command line against that installation on the server host, run it as the same user with the same data directory:

```sh
sudo -u dedie DEDIE_DATA_DIR=/var/lib/dedie dedie --local conn list
```

Health for a load balancer or an uptime check is at `/healthz`; Prometheus metrics are at `/metrics` (with an API token).

## The command line on other machines

Install `dedie` on your own machine and sign it in to the server. Everything then runs on the server, with your role:

```sh
dedie login https://dedie.example.com
dedie conn list
dedie migrate list
```

`dedie login` stores an API token, not your password. In CI, set `DEDIE_SERVER` and `DEDIE_TOKEN` (a token created on the Account page) instead of signing in.

## The desktop application

```sh
tar -xzf dedie-desktop-linux.tar.gz
java -jar dedie-desktop/dedie-desktop.jar
```

It starts its own engine on a port nothing outside the machine can reach and keeps its data in the same kind of data directory (`~/.dedie`). It is the whole product for one person; it is not a client for a Dedie server.

## The data directory and the master key

Everything Dedie knows lives in one directory: `~/.dedie` by default, or wherever `--data` or `DEDIE_DATA_DIR` points.

| Path | Contents |
|---|---|
| `dedie.db` | organizations, users, connections (passwords encrypted), jobs, schedules, saved queries, alert channels, the audit log |
| `master.key` | the key that protects every stored password and every encrypted backup |
| `backups/` | backup files, unless another storage location is chosen |
| `wal/` | write-ahead log archives for point-in-time recovery |

The master key is created on first start. Without it the stored passwords and the encrypted backups cannot be read by anyone, including you. Two rules follow:

- **Keep a copy of it somewhere other than the server**, with the same care as a root password.
- **Do not keep it beside off-site copies of your backups.** Whoever holds both can read them.

To keep the key out of the data directory altogether, supply it from your secret store as `DEDIE_MASTER_KEY` (base64 of 32 bytes; `openssl rand -base64 32` makes one) before the first start.

## Backing up Dedie itself

Stop the server, or accept a copy taken while it is quiet, and copy the data directory:

```sh
sudo systemctl stop dedie
sudo tar -C /var/lib -czf dedie-state-$(date +%F).tar.gz dedie
sudo systemctl start dedie
```

Restoring is the reverse: unpack the directory, make sure the master key is the one it was written with, and start the server. `dedie audit verify` confirms that the audit log arrived intact.

## Upgrading

1. Read the release notes.
2. Copy the data directory (above).
3. Replace the `dedie-server` and `dedie` programs and restart the service.

The server brings its own database up to date when it starts. Running migrations and syncs are handed back to the queue on shutdown and continue from their checkpoints afterwards. Keep `dedie` on other machines at the same version as the server.

## Troubleshooting

| Symptom | Cause and remedy |
|---|---|
| The server exits with a message about plain HTTP | It was asked to listen beyond the local machine without TLS. Add `--tls-cert` and `--tls-key`, or put it behind a proxy with `--behind-tls-proxy` |
| Sign-in works locally but not through the proxy | The proxy is not sending `X-Forwarded-Proto https`, or the server was started without `--behind-tls-proxy`, so its `Secure` cookie is refused |
| "connection refused" or a timeout when testing a connection | The Dedie machine cannot reach the database: check the firewall, the database's listen address and, for PostgreSQL, `pg_hba.conf` |
| A connection test fails on TLS | The server's certificate is not signed by a public authority: choose `verify-ca` or `verify-full` and paste the authority's certificate, or use `require` to encrypt without verifying. Oracle listeners take TLS on a separate port; use `disable` for the usual 1521 |
| Oracle: ORA-12514 | The **service name** is wrong. It is the name of the pluggable database's service (`lsnrctl status` lists them), not the instance SID |
| Backups fail with "pg_dump: not found" or a version complaint | Install client tools at least as new as the server, or point `DEDIE_PG_BIN` at the directory that holds them |
| Stored passwords cannot be decrypted after moving the installation | The data directory was copied without its `master.key`, or `DEDIE_MASTER_KEY` differs from the one it was written with |
| Nobody can sign in | On the server host, `dedie --local user add --role owner` creates a new owner; the command line there needs no sign-in |
