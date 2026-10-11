// Package backup creates and restores logical PostgreSQL backups by driving
// the server's own pg_dump and pg_restore. The engine pipes their output
// through authenticated encryption, so a backup never touches disk in clear
// text.
package backup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

var versionRe = regexp.MustCompile(`\(PostgreSQL\) (\d+)`)

type tool struct {
	path  string
	major int
}

// findTool locates the newest installed copy of a PostgreSQL client program
// whose major version is at least minMajor. pg_dump refuses to dump a server
// newer than itself, and pg_restore cannot read archives from a newer pg_dump,
// so the newest available binary is always the safe choice.
//
// DEDIE_PG_BIN, when set, names the only directory to look in.
func findTool(name string, minMajor int) (string, error) {
	var candidates []string
	if dir := os.Getenv("DEDIE_PG_BIN"); dir != "" {
		candidates = []string{filepath.Join(dir, name)}
	} else {
		for _, pattern := range []string{
			"/usr/lib/postgresql/*/bin/", "/usr/pgsql-*/bin/", "/opt/homebrew/opt/postgresql@*/bin/",
			"/usr/local/opt/postgresql@*/bin/", "/usr/local/pgsql/bin/",
		} {
			found, _ := filepath.Glob(pattern + name)
			candidates = append(candidates, found...)
		}
		if p, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, p)
		}
	}
	var tools []tool
	for _, c := range candidates {
		out, err := exec.Command(c, "--version").Output()
		if err != nil {
			continue
		}
		if m := versionRe.FindSubmatch(out); m != nil {
			major, _ := strconv.Atoi(string(m[1]))
			tools = append(tools, tool{c, major})
		}
	}
	if len(tools) == 0 {
		return "", fmt.Errorf("%s was not found; install the PostgreSQL client tools or set DEDIE_PG_BIN", name)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].major > tools[j].major })
	if tools[0].major < minMajor {
		return "", fmt.Errorf("the newest %s installed is version %d, but version %d or newer is required", name, tools[0].major, minMajor)
	}
	return tools[0].path, nil
}
