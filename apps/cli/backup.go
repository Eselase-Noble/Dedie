package main

import (
	"flag"
	"fmt"

	"dedie/engine/app"
)

func (c *cli) backupCreate(args []string) error {
	fs := flag.NewFlagSet("backup create", flag.ExitOnError)
	note := fs.String("note", "", "a note stored with the backup")
	conn, err := oneArg(parse(fs, args), "connection name")
	if err != nil {
		return err
	}
	fmt.Println("Backing up", conn, "...")
	b, err := c.app.CreateBackup(c.ctx, c.actor, conn, *note, true)
	if err != nil {
		return err
	}
	if b.Status != "completed" {
		return fmt.Errorf("backup failed: %s", b.Error)
	}
	fmt.Printf("Backup %s completed: %s, sha256 %s\n", b.ID, humanBytes(b.SizeBytes), b.SHA256)
	return nil
}

func (c *cli) backupList() error {
	list, err := c.app.ListBackups(c.ctx, c.actor)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, b := range list {
		rows = append(rows, []string{b.ID, b.ConnectionName, b.Database, b.Status, humanBytes(b.SizeBytes), b.CreatedAt.Local().Format("2006-01-02 15:04"), b.Note})
	}
	table("ID\tCONNECTION\tDATABASE\tSTATUS\tSIZE\tCREATED\tNOTE", rows)
	return nil
}

func (c *cli) backupVerify(args []string) error {
	id, err := oneArg(args, "backup id")
	if err != nil {
		return err
	}
	if err := c.app.VerifyBackup(c.ctx, c.actor, id); err != nil {
		return err
	}
	fmt.Println("Backup is intact and decrypts correctly.")
	return nil
}

func (c *cli) backupRestore(args []string) error {
	fs := flag.NewFlagSet("backup restore", flag.ExitOnError)
	var in app.RestoreInput
	fs.StringVar(&in.TargetID, "target", "", "connection to restore into")
	fs.BoolVar(&in.Clean, "clean", false, "drop existing objects before recreating them")
	fs.BoolVar(&in.KeepOwners, "keep-owners", false, "restore object ownership and privileges")
	fs.StringVar(&in.Confirm, "confirm", "", "the target database name, to confirm the restore")
	id, err := oneArg(parse(fs, args), "backup id")
	if err != nil {
		return err
	}
	fmt.Println("Restoring", id, "...")
	if _, err := c.app.RestoreBackup(c.ctx, c.actor, id, in, true); err != nil {
		return err
	}
	fmt.Println("Restore completed.")
	return nil
}

func (c *cli) backupRemove(args []string) error {
	id, err := oneArg(args, "backup id")
	if err != nil {
		return err
	}
	if err := c.app.DeleteBackup(c.ctx, c.actor, id); err != nil {
		return err
	}
	fmt.Println("Deleted", id)
	return nil
}
