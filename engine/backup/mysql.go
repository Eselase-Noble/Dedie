package backup

import (
	"bytes"
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

// dumpMySQL writes a mysqldump of the database to w. The dump is taken in
// one transaction, so InnoDB tables are consistent with each other without
// blocking writers, and it includes routines, triggers and events.
func dumpMySQL(ctx context.Context, cfg connector.Config, w io.Writer) error {
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
	cmd := exec.CommandContext(ctx, tool, "--defaults-file="+options, "--single-transaction", "--routines", "--triggers",
		"--events", "--hex-blob", "--set-gtid-purged=OFF", "--no-tablespaces", "--default-character-set=utf8mb4", cfg.Database)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("mysqldump failed: %s", tail(&stderr))
	}
	return nil
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
