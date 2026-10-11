package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"dedie/engine/connector"
)

func (s *session) CreateDatabase(ctx context.Context, name string) error {
	if err := connector.CheckName("database", name); err != nil {
		return err
	}
	// UTF8 holds any text; template0 allows it whatever the server default is.
	_, err := s.conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" ENCODING 'UTF8' TEMPLATE template0")
	return friendly(err)
}

func (s *session) CreateSchema(ctx context.Context, name string) error {
	if err := connector.CheckName("schema", name); err != nil {
		return err
	}
	_, err := s.conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize())
	return friendly(err)
}

func (s *session) CreateTable(ctx context.Context, def connector.TableDef) error {
	if err := def.Validate(); err != nil {
		return err
	}
	schema := def.Schema
	if schema == "" {
		schema = "public"
	}
	var cols, pk []string
	for _, c := range def.Columns {
		col := pgx.Identifier{c.Name}.Sanitize() + " " + strings.TrimSpace(c.Type)
		if c.Default != "" {
			col += " DEFAULT " + connector.DefaultSQL(c.Default)
		}
		if !c.Nullable || c.PrimaryKey {
			col += " NOT NULL"
		}
		cols = append(cols, col)
		if c.PrimaryKey {
			pk = append(pk, pgx.Identifier{c.Name}.Sanitize())
		}
	}
	if len(pk) > 0 {
		cols = append(cols, "PRIMARY KEY ("+strings.Join(pk, ", ")+")")
	}
	_, err := s.conn.Exec(ctx, "CREATE TABLE "+pgx.Identifier{schema, def.Name}.Sanitize()+" (\n  "+strings.Join(cols, ",\n  ")+"\n)")
	return friendly(err)
}
