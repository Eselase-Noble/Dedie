package main

import (
	"fmt"
	"strconv"
	"time"
)

// overview prints what the web interface's first page shows: what is
// waiting for somebody, then every connection with its health and its
// last backup.
func (c *cli) overview() error {
	s, err := c.be.Summary()
	if err != nil {
		return err
	}
	if len(s.Attention) == 0 {
		fmt.Println("Nothing needs attention.")
	} else {
		fmt.Printf("Needs attention (%d):\n", len(s.Attention))
		for _, a := range s.Attention {
			fmt.Printf("  %-7s  %s\n", a.Level, a.Title)
			if a.Detail != "" {
				fmt.Printf("           %s\n", a.Detail)
			}
		}
	}
	if len(s.Connections) == 0 {
		return nil
	}
	fmt.Println()
	var rows [][]string
	for _, x := range s.Connections {
		health := "not checked yet"
		if x.Checked && x.Online {
			health = "online, " + strconv.FormatInt(x.LatencyMS, 10) + " ms"
		} else if x.Checked {
			health = "NOT ANSWERING"
		}
		backup := "-"
		if x.BackupsApply {
			backup = "never"
			if !x.LastBackupAt.IsZero() {
				backup = ago(time.Since(x.LastBackupAt)) + " ago"
			}
			if x.Scheduled {
				backup += ", scheduled"
			}
		}
		rows = append(rows, []string{x.Name, envLabel(x.Environment), x.Kind, health, backup})
	}
	table("CONNECTION\tENVIRONMENT\tKIND\tHEALTH\tLAST BACKUP", rows)
	return nil
}

// ago writes a length of time the way it is said: 40 minutes, 3 hours, 12 days.
func ago(d time.Duration) string {
	unit := func(n int, name string) string {
		if n == 1 {
			return "1 " + name
		}
		return strconv.Itoa(n) + " " + name + "s"
	}
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return unit(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return unit(int(d.Hours()), "hour")
	}
	return unit(int(d.Hours()/24), "day")
}
