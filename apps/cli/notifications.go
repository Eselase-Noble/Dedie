package main

import (
	"flag"
	"fmt"
)

// notifications lists what has happened that the signed-in user should
// know about, newest first, with what they have not read yet marked.
func (c *cli) notifications(args []string) error {
	fs := flag.NewFlagSet("notifications", flag.ExitOnError)
	unreadOnly := fs.Bool("unread", false, "show only what you have not read")
	readAll := fs.Bool("read-all", false, "mark everything as read")
	limit := fs.Int("n", 30, "number of notifications")
	fs.Parse(args)
	unread, items, err := c.be.ListNotifications(*unreadOnly, *limit)
	if err != nil {
		return err
	}
	if *readAll {
		if err := c.be.ReadAllNotifications(); err != nil {
			return err
		}
		switch unread {
		case 0:
			fmt.Println("Nothing was unread.")
		case 1:
			fmt.Println("Marked 1 notification as read.")
		default:
			fmt.Printf("Marked %d notifications as read.\n", unread)
		}
		return nil
	}
	if len(items) == 0 {
		if *unreadOnly {
			fmt.Println("No unread notifications.")
		} else {
			fmt.Println("No notifications.")
		}
		return nil
	}
	var rows [][]string
	for _, n := range items {
		mark := " "
		if !n.Read {
			mark = "*"
		}
		text := n.Title
		if n.Body != "" {
			text += " - " + n.Body
		}
		rows = append(rows, []string{mark, n.CreatedAt.Local().Format("2006-01-02 15:04"), n.Level, text})
	}
	table(" \tTIME\tLEVEL\tNOTIFICATION", rows)
	if unread > 0 {
		fmt.Printf("\n%d unread, marked *. `dedie notifications --read-all` marks them read.\n", unread)
	}
	return nil
}
