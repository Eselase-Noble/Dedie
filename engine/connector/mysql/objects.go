package mysql

import (
	"context"
	"errors"
	"strings"

	"dedie/engine/connector"
)

func (s *session) CreateDatabase(ctx context.Context, name string) error {
	if err := connector.CheckName("database", name); err != nil {
		return err
	}
	_, err := s.conn.ExecContext(ctx, "CREATE DATABASE "+Quote(name)+" CHARACTER SET utf8mb4")
	return Friendly(err)
}

// CreateSchema is not offered for MySQL, where a schema and a database are
// the same thing.
func (s *session) CreateSchema(ctx context.Context, name string) error {
	return errors.New("in MySQL a schema is a database; create a database instead")
}

func (s *session) CreateTable(ctx context.Context, def connector.TableDef) error {
	if err := def.Validate(); err != nil {
		return err
	}
	var cols, pk []string
	for _, c := range def.Columns {
		col := Quote(c.Name) + " " + strings.TrimSpace(c.Type)
		if !c.Nullable || c.PrimaryKey {
			col += " NOT NULL"
		}
		if c.Default != "" {
			col += " DEFAULT " + connector.DefaultSQL(c.Default)
		}
		cols = append(cols, col)
		if c.PrimaryKey {
			pk = append(pk, Quote(c.Name))
		}
	}
	if len(pk) > 0 {
		cols = append(cols, "PRIMARY KEY ("+strings.Join(pk, ", ")+")")
	}
	name := Quote(def.Name)
	if def.Schema != "" {
		name = Quote(def.Schema) + "." + name
	}
	_, err := s.conn.ExecContext(ctx, "CREATE TABLE "+name+" (\n  "+strings.Join(cols, ",\n  ")+"\n) ENGINE=InnoDB")
	return Friendly(err)
}

// ServerObjects lists the databases on the server. MySQL objects have no
// owner: access is decided by privileges alone, so no roles are offered.
func (s *session) ServerObjects(ctx context.Context) (*connector.ServerObjects, error) {
	out := &connector.ServerObjects{Databases: []connector.Database{}, Roles: []string{}}
	rows, err := s.conn.QueryContext(ctx, `SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('mysql','sys','information_schema','performance_schema') ORDER BY 1`)
	if err != nil {
		return nil, Friendly(err)
	}
	defer rows.Close()
	for rows.Next() {
		var d connector.Database
		if err := rows.Scan(&d.Name); err != nil {
			return nil, err
		}
		out.Databases = append(out.Databases, d)
	}
	return out, Friendly(rows.Err())
}

func (s *session) SetOwner(ctx context.Context, c connector.OwnerChange) error {
	return errors.New("MySQL objects have no owner; access is controlled with GRANT and REVOKE")
}
