package main

import (
	"flag"
	"fmt"
	"strconv"
)

func (c *cli) initOrg(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	org := fs.String("org", "", "organization name")
	name := fs.String("name", "", "your name")
	email := fs.String("email", "", "your email address; used to sign in")
	fs.Parse(args)
	pw, err := readPassword("Password (12 characters or more): ")
	if err != nil {
		return err
	}
	u, err := c.app.Setup(c.ctx, *org, *name, *email, pw)
	if err != nil {
		return err
	}
	fmt.Printf("Created organization %q with owner %s.\n", *org, u.Email)
	return nil
}

func (c *cli) userAdd(args []string) error {
	fs := flag.NewFlagSet("user add", flag.ExitOnError)
	name := fs.String("name", "", "full name")
	email := fs.String("email", "", "email address")
	role := fs.String("role", "analyst", "owner, admin, operator, analyst or viewer")
	fs.Parse(args)
	pw, err := readPassword("Password for the new user: ")
	if err != nil {
		return err
	}
	u, err := c.be.CreateUser(*name, *email, pw, *role)
	if err != nil {
		return err
	}
	fmt.Printf("Added %s as %s.\n", u.Email, u.Role)
	return nil
}

func (c *cli) auditList(args []string) error {
	fs := flag.NewFlagSet("audit list", flag.ExitOnError)
	limit := fs.Int("n", 50, "number of entries")
	fs.Parse(args)
	entries, err := c.be.ListAudit(0, *limit)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, e := range entries {
		rows = append(rows, []string{strconv.FormatInt(e.ID, 10), e.Time.Local().Format("2006-01-02 15:04:05"), e.Actor, e.Action, e.Target, e.Detail})
	}
	table("ID\tTIME\tACTOR\tACTION\tTARGET\tDETAIL", rows)
	return nil
}

func (c *cli) auditVerify() error {
	v, err := c.be.VerifyAudit()
	if err != nil {
		return err
	}
	if !v.Intact {
		return fmt.Errorf("audit log has been altered: the chain breaks at entry %d (%d entries before it are intact)", v.BrokenAt, v.Checked)
	}
	fmt.Printf("Audit log intact: %d entries verified.\n", v.Checked)
	return nil
}
