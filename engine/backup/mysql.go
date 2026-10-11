package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"dedie/engine/connector"
)

var mysqlSSLModes = map[string]string{
	"disable": "DISABLED", "allow": "PREFERRED", "prefer": "PREFERRED", "": "PREFERRED",
	"require": "REQUIRED", "verify-ca": "VERIFY_CA", "verify-full": "VERIFY_IDENTITY",
}

// mysqlOptionFile writes connection settings to a private temporary option
// file. The client programs read the password from it, which keeps it out of
// both the argument list and the environment. The caller removes the files.
func mysqlOptionFile(cfg connector.Config) (path string, cleanup func(), err error) {
	quote := func(v string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[client]\nhost=%s\nport=%d\nuser=%s\npassword=%s\nssl-mode=%s\nprotocol=TCP\n",
		quote(cfg.Host), port, quote(cfg.Username), quote(cfg.Password), mysqlSSLModes[cfg.SSLMode])

	var files []string
	cleanup = func() {
		for _, f := range files {
			os.Remove(f)
		}
	}
	write := func(pattern, content string) (string, error) {
		f, err := os.CreateTemp("", pattern) // created with mode 0600
		if err != nil {
			return "", err
		}
		files = append(files, f.Name())
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			return "", err
		}
		return f.Name(), f.Close()
	}
	if cfg.SSLRootCert != "" {
		ca, err := write("dedie-ca-*.pem", cfg.SSLRootCert)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		fmt.Fprintf(&b, "ssl-ca=%s\n", quote(ca))
	}
	path, err = write("dedie-my-*.cnf", b.String())
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func mysqlTool(name string) (string, error) {
	if dir := os.Getenv("DEDIE_MYSQL_BIN"); dir != "" {
		return dir + "/" + name, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s was not found; install the MySQL client tools or set DEDIE_MYSQL_BIN", name)
	}
	return path, nil
}

// positionRe finds the binary log position mysqldump writes at the top of
// a dump taken with --source-data (or --master-data on older versions).
var positionRe = regexp.MustCompile(`(?:MASTER|SOURCE)_LOG_FILE='([^']+)', (?:MASTER|SOURCE)_LOG_POS=(\d+)`)

// sniffer passes a dump through unchanged while keeping its beginning,
// where the binary log position is written.
type sniffer struct {
	w    io.Writer
	head []byte
}

func (s *sniffer) Write(p []byte) (int, error) {
	if room := 64<<10 - len(s.head); room > 0 {
		s.head = append(s.head, p[:min(room, len(p))]...)
	}
	return s.w.Write(p)
}

// dumpMySQL writes a gzip-compressed mysqldump to w. The dump is taken in
// one transaction, so InnoDB tables are consistent with each other without
// blocking writers. A full or schema backup includes routines, triggers and
// events.
func dumpMySQL(ctx context.Context, cfg connector.Config, opts Options, w io.Writer) (Meta, error) {
	tool, err := mysqlTool("mysqldump")
	if err != nil {
		return Meta{}, err
	}
	options, cleanup, err := mysqlOptionFile(cfg)
	if err != nil {
		return Meta{}, err
	}
	defer cleanup()
	// --defaults-file must come first; it also stops the tool from reading
	// option files that belong to whoever runs this process.
	args := []string{"--defaults-file=" + options, "--single-transaction", "--hex-blob", "--set-gtid-purged=OFF",
		"--no-tablespaces", "--default-character-set=utf8mb4"}
	switch opts.Type {
	case TypeSchema:
		args = append(args, "--no-data", "--routines", "--triggers", "--events")
	case TypeData:
		args = append(args, "--no-create-info", "--skip-triggers")
	case TypeTables:
		args = append(args, "--triggers")
	default:
		args = append(args, "--routines", "--triggers", "--events")
	}
	if opts.RecordPosition {
		// Written as a comment in the dump, at the exact point the
		// dump's transaction began. The option was renamed in 8.0.26.
		flag := "--master-data=2"
		if help, err := exec.CommandContext(ctx, tool, "--help").Output(); err == nil && bytes.Contains(help, []byte("--source-data")) {
			flag = "--source-data=2"
		}
		args = append(args, flag)
	}
	// "--" ends the options, so a table name can never be read as one.
	args = append(args, "--", cfg.Database)
	if opts.Type == TypeTables {
		for _, t := range opts.Tables {
			// Accept database.table as the catalog shows it.
			if _, name, ok := strings.Cut(t, "."); ok {
				t = name
			}
			args = append(args, t)
		}
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	zw := gzip.NewWriter(w)
	sniff := &sniffer{w: zw}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = sniff, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Meta{}, ctx.Err()
		}
		return Meta{}, fmt.Errorf("mysqldump failed: %s", tail(&stderr))
	}
	var meta Meta
	if m := positionRe.FindSubmatch(sniff.head); m != nil {
		meta.Position = string(m[1]) + ":" + string(m[2])
	}
	return meta, zw.Close()
}

// mysqlRows runs a statement with the mysql client and returns its rows
// as tab-separated fields.
func mysqlRows(ctx context.Context, options, stmt string) ([][]string, error) {
	tool, err := mysqlTool("mysql")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, tool, "--defaults-file="+options, "--batch", "--skip-column-names", "-e", stmt)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s", tail(&stderr))
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows, nil
}

// binlogBackup writes, as gzip-compressed SQL, every change the database
// received between a binary log position and now. Applied on top of the
// backup that ended at that position, it brings a restore up to the
// moment this backup was taken.
func binlogBackup(ctx context.Context, cfg connector.Config, from string, w io.Writer) (Meta, error) {
	startFile, startPos, ok := strings.Cut(from, ":")
	if !ok || startFile == "" {
		return Meta{}, errors.New("the backup this one builds on did not record a binary log position; take a new full backup")
	}
	tool, err := mysqlTool("mysqlbinlog")
	if err != nil {
		return Meta{}, err
	}
	options, cleanup, err := mysqlOptionFile(cfg)
	if err != nil {
		return Meta{}, err
	}
	defer cleanup()

	// Where the log ends now is where this backup ends. The statement was
	// renamed in MySQL 8.2.
	status, err := mysqlRows(ctx, options, "SHOW BINARY LOG STATUS")
	if err != nil {
		if status, err = mysqlRows(ctx, options, "SHOW MASTER STATUS"); err != nil {
			return Meta{}, fmt.Errorf("could not read the binary log position (the account needs REPLICATION CLIENT): %w", err)
		}
	}
	if len(status) == 0 || len(status[0]) < 2 {
		return Meta{}, errors.New("binary logging is off on this server, so there is nothing to build an incremental backup from")
	}
	endFile, endPos := status[0][0], status[0][1]

	logs, err := mysqlRows(ctx, options, "SHOW BINARY LOGS")
	if err != nil {
		return Meta{}, fmt.Errorf("could not list the binary logs: %w", err)
	}
	var files []string
	for _, row := range logs {
		name := row[0]
		if name >= startFile && name <= endFile {
			files = append(files, name)
		}
	}
	if len(files) == 0 || files[0] != startFile {
		return Meta{}, fmt.Errorf("the server no longer has binary log %s, which this backup needs to start from; take a new full backup", startFile)
	}

	// --start-position applies to the first file and --stop-position to
	// the last. GTIDs are left out so the events can be applied to a
	// server whether or not it uses them.
	args := []string{"--defaults-file=" + options, "--read-from-remote-server", "--skip-gtids", "--database=" + cfg.Database,
		"--start-position=" + startPos, "--stop-position=" + endPos}
	args = append(args, files...)
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	zw := gzip.NewWriter(w)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = zw, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Meta{}, ctx.Err()
		}
		return Meta{}, fmt.Errorf("mysqlbinlog failed (the account needs REPLICATION SLAVE): %s", tail(&stderr))
	}
	return Meta{Position: endFile + ":" + endPos}, zw.Close()
}

// restoreMySQL replays a dump into the database. Unlike a PostgreSQL restore
// it cannot be made atomic, because MySQL commits at every DDL statement: a
// restore that fails part-way leaves the tables it had already replaced.
func restoreMySQL(ctx context.Context, cfg connector.Config, r io.Reader) (string, error) {
	tool, err := mysqlTool("mysql")
	if err != nil {
		return "", err
	}
	options, cleanup, err := mysqlOptionFile(cfg)
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, tool, "--defaults-file="+options, "--default-character-set=utf8mb4", cfg.Database)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stderr = r, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return tail(&stderr), ctx.Err()
		}
		return tail(&stderr), fmt.Errorf("mysql restore failed: %s", tail(&stderr))
	}
	return tail(&stderr), nil
}
