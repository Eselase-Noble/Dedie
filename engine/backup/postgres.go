package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"dedie/engine/connector"
)

// pgCommand prepares a client program with connection settings passed through
// the environment. The password never appears in the argument list, where
// other local users could read it from the process table.
func pgCommand(ctx context.Context, path string, cfg connector.Config, args ...string) (*exec.Cmd, func(), error) {
	cmd := exec.CommandContext(ctx, path, args...)
	sslmode := cfg.SSLMode
	if sslmode == "" {
		sslmode = "prefer"
	}
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"PGHOST=" + cfg.Host,
		"PGPORT=" + strconv.Itoa(cfg.Port),
		"PGUSER=" + cfg.Username,
		"PGPASSWORD=" + cfg.Password,
		"PGDATABASE=" + cfg.Database,
		"PGSSLMODE=" + sslmode,
		"PGCONNECT_TIMEOUT=15",
		"PGAPPNAME=dedie",
	}
	cleanup := func() {}
	if cfg.SSLRootCert != "" {
		f, err := os.CreateTemp("", "dedie-ca-*.pem")
		if err != nil {
			return nil, nil, err
		}
		if _, err := f.WriteString(cfg.SSLRootCert); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, nil, err
		}
		f.Close()
		cmd.Env = append(cmd.Env, "PGSSLROOTCERT="+f.Name())
		cleanup = func() { os.Remove(f.Name()) }
	}
	return cmd, cleanup, nil
}

// dumpPostgres writes a custom-format pg_dump archive of the database to w.
// serverMajor is the server's major version.
func dumpPostgres(ctx context.Context, cfg connector.Config, serverMajor int, w io.Writer) error {
	path, err := findTool("pg_dump", serverMajor)
	if err != nil {
		return err
	}
	cmd, cleanup, err := pgCommand(ctx, path, cfg, "--format=custom", "--no-password", "--compress=6")
	if err != nil {
		return err
	}
	defer cleanup()
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("pg_dump failed: %s", tail(&stderr))
	}
	return nil
}

// restorePostgres applies an archive read from r to the database. It runs in
// a single transaction: a restore that fails changes nothing. The returned
// text is pg_restore's own output.
func restorePostgres(ctx context.Context, cfg connector.Config, r io.Reader, opts RestoreOptions) (string, error) {
	path, err := findTool("pg_restore", 0)
	if err != nil {
		return "", err
	}
	args := []string{"--no-password", "--single-transaction", "--exit-on-error", "--dbname=" + cfg.Database}
	if opts.Clean {
		args = append(args, "--clean", "--if-exists")
	}
	if !opts.KeepOwners {
		args = append(args, "--no-owner", "--no-privileges")
	}
	cmd, cleanup, err := pgCommand(ctx, path, cfg, args...)
	if err != nil {
		return "", err
	}
	defer cleanup()
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stderr = r, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return tail(&stderr), ctx.Err()
		}
		return tail(&stderr), fmt.Errorf("pg_restore failed: %s", tail(&stderr))
	}
	return tail(&stderr), nil
}
