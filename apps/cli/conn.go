package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"dedie/engine/app"
)

func (c *cli) connAdd(args []string) error {
	fs := flag.NewFlagSet("conn add", flag.ExitOnError)
	var in app.ConnectionInput
	fs.StringVar(&in.Name, "name", "", "name for this connection")
	fs.StringVar(&in.Kind, "kind", "postgres", "database kind: postgres, mysql, mongodb or oracle")
	fs.StringVar(&in.Host, "host", "", "server host")
	fs.IntVar(&in.Port, "port", 0, "server port (default: the kind's standard port)")
	fs.StringVar(&in.Database, "db", "", "database name; for Oracle, the service name (for example FREEPDB1)")
	fs.StringVar(&in.Username, "user", "", "database user")
	fs.StringVar(&in.SSLMode, "sslmode", "", "disable, allow, prefer, require, verify-ca or verify-full (default: require; disable for Oracle, whose listener takes TLS on a separate port)")
	caFile := fs.String("ssl-root-cert", "", "PEM file with the CA that signed the server certificate")
	fs.BoolVar(&in.ReadOnly, "read-only", false, "never allow writes through this connection")
	fs.Parse(args)
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		if err != nil {
			return err
		}
		in.SSLRootCert = string(pem)
	}
	pw, err := readPassword("Database password: ")
	if err != nil {
		return err
	}
	in.Password = pw
	info, err := c.be.TestInput(in)
	if err != nil {
		return err
	}
	conn, err := c.be.SaveConnection("", in)
	if err != nil {
		return err
	}
	fmt.Printf("Saved %s (%s %s, encrypted: %v).\n", conn.Name, info.Kind, info.Version, info.Encrypted)
	return nil
}

func (c *cli) connList() error {
	list, err := c.be.ListConnections()
	if err != nil {
		return err
	}
	var rows [][]string
	for _, x := range list {
		mode := "read-write"
		if x.ReadOnly {
			mode = "read-only"
		}
		rows = append(rows, []string{x.Name, x.Kind, x.Host + ":" + strconv.Itoa(x.Port), x.Database, x.Username, x.SSLMode, mode})
	}
	table("NAME\tKIND\tADDRESS\tDATABASE\tUSER\tSSL\tMODE", rows)
	return nil
}

func (c *cli) connTest(args []string) error {
	name, err := oneArg(args, "connection name")
	if err != nil {
		return err
	}
	info, err := c.be.TestConnection(name)
	if err != nil {
		return err
	}
	fmt.Printf("Connected to %s %s\n  database:  %s\n  user:      %s (superuser: %v)\n  encrypted: %v\n",
		info.Kind, info.Version, info.Database, info.User, info.Superuser, info.Encrypted)
	return nil
}

func (c *cli) connRemove(args []string) error {
	name, err := oneArg(args, "connection name")
	if err != nil {
		return err
	}
	if err := c.be.DeleteConnection(name); err != nil {
		return err
	}
	fmt.Println("Deleted", name)
	return nil
}

func (c *cli) query(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	write := fs.Bool("write", false, "allow the statement to modify data")
	maxRows := fs.Int("max-rows", 1000, "maximum rows to print per result")
	pos := parse(fs, args)
	if len(pos) != 2 {
		return fmt.Errorf("usage: dedie query [--write] <connection> <sql>")
	}
	resp, err := c.be.Query(pos[0], app.QueryRequest{SQL: pos[1], Write: *write, MaxRows: *maxRows})
	if err != nil {
		return err
	}
	for _, rs := range resp.Results {
		if len(rs.Columns) > 0 {
			header := make([]string, len(rs.Columns))
			for i, col := range rs.Columns {
				header[i] = strings.ToUpper(col.Name)
			}
			rows := make([][]string, len(rs.Rows))
			for i, r := range rs.Rows {
				rows[i] = make([]string, len(r))
				for j, v := range r {
					if v == nil {
						rows[i][j] = "NULL"
					} else {
						rows[i][j] = strings.ReplaceAll(*v, "\n", "\\n")
					}
				}
			}
			table(strings.Join(header, "\t"), rows)
		}
		note := rs.Command
		if rs.Truncated {
			note = fmt.Sprintf("first %d rows shown", len(rs.Rows))
		}
		fmt.Printf("(%s, %d ms)\n", note, resp.DurationMS)
	}
	return nil
}
