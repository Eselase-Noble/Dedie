package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// dumpMySQL writes a gzip-compressed mysqldump to w. The dump is taken in
// one transaction, so InnoDB tables are consistent with each other without
// blocking writers. A full or schema backup includes routines, triggers and
// events.
func dumpMySQL(ctx context.Context, cfg connector.Config, opts Options, w io.Writer) error {
	tool, err := mysqlTool("mysqldump")
	if err != nil {
		return err
	}
	options, cleanup, err := mysqlOptionFile(cfg)
	if err != nil {
		return err
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
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = zw, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("mysqldump failed: %s", tail(&stderr))
	}
	return zw.Close()
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
