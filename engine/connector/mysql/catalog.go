package mysql

import (
	"context"

	"dedie/engine/connector"
)

const userSchemas = `NOT IN ('mysql','sys','information_schema','performance_schema')`

// Catalog loads the object tree for the query browser. A MySQL database is
// presented as a schema, which is what it is in MySQL's own terms.
func (s *session) Catalog(ctx context.Context) (*connector.Catalog, error) {
	schemas := map[string]*connector.Schema{}
	var order []string
	get := func(name string) *connector.Schema {
		if sc, ok := schemas[name]; ok {
			return sc
		}
		sc := &connector.Schema{Name: name, Tables: []connector.Table{}, Views: []connector.Table{}, Functions: []string{}, Sequences: []string{}}
		schemas[name] = sc
		order = append(order, name)
		return sc
	}

	rows, err := s.conn.QueryContext(ctx, `SELECT schema_name FROM information_schema.schemata WHERE schema_name `+userSchemas+` ORDER BY 1`)
	if err != nil {
		return nil, Friendly(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		get(name)
	}
	rows.Close()

	type relKey struct{ schema, name string }
	rels := map[relKey]*connector.Table{}
	isView := map[relKey]bool{}
	var relOrder []relKey
	rows, err = s.conn.QueryContext(ctx, `SELECT table_schema, table_name, table_type, COALESCE(table_rows, 0),
			COALESCE(data_length, 0) + COALESCE(index_length, 0)
		FROM information_schema.tables WHERE table_schema `+userSchemas+` ORDER BY 1, 2`)
	if err != nil {
		return nil, Friendly(err)
	}
	for rows.Next() {
		var k relKey
		var kind string
		t := &connector.Table{Columns: []connector.TableColumn{}, Indexes: []string{}}
		if err := rows.Scan(&k.schema, &k.name, &kind, &t.EstRows, &t.Size); err != nil {
			rows.Close()
			return nil, err
		}
		t.Name = k.name
		rels[k], isView[k] = t, kind == "VIEW"
		relOrder = append(relOrder, k)
	}
	rows.Close()

	rows, err = s.conn.QueryContext(ctx, `SELECT table_schema, table_name, column_name, column_type, is_nullable = 'YES',
			COALESCE(column_default, ''), column_key = 'PRI'
		FROM information_schema.columns WHERE table_schema `+userSchemas+` ORDER BY 1, 2, ordinal_position`)
	if err != nil {
		return nil, Friendly(err)
	}
	for rows.Next() {
		var k relKey
		var col connector.TableColumn
		if err := rows.Scan(&k.schema, &k.name, &col.Name, &col.Type, &col.Nullable, &col.Default, &col.PrimaryKey); err != nil {
			rows.Close()
			return nil, err
		}
		if t, ok := rels[k]; ok {
			t.Columns = append(t.Columns, col)
		}
	}
	rows.Close()

	rows, err = s.conn.QueryContext(ctx, `SELECT table_schema, table_name,
			CONCAT(IF(MAX(non_unique) = 0, 'UNIQUE ', ''), index_name, ' (', GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ', '), ')')
		FROM information_schema.statistics WHERE table_schema `+userSchemas+`
		GROUP BY table_schema, table_name, index_name ORDER BY 1, 2, 3`)
	if err != nil {
		return nil, Friendly(err)
	}
	for rows.Next() {
		var k relKey
		var def string
		if err := rows.Scan(&k.schema, &k.name, &def); err != nil {
			rows.Close()
			return nil, err
		}
		if t, ok := rels[k]; ok {
			t.Indexes = append(t.Indexes, def)
		}
	}
	rows.Close()

	for _, k := range relOrder {
		sc := get(k.schema)
		if isView[k] {
			sc.Views = append(sc.Views, *rels[k])
		} else {
			sc.Tables = append(sc.Tables, *rels[k])
		}
	}

	rows, err = s.conn.QueryContext(ctx, `SELECT routine_schema, CONCAT(routine_name, ' (', LOWER(routine_type), ')')
		FROM information_schema.routines WHERE routine_schema `+userSchemas+` ORDER BY 1, 2`)
	if err != nil {
		return nil, Friendly(err)
	}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			rows.Close()
			return nil, err
		}
		sc := get(schema)
		sc.Functions = append(sc.Functions, name)
	}
	rows.Close()

	cat := &connector.Catalog{Schemas: []connector.Schema{}}
	for _, name := range order {
		cat.Schemas = append(cat.Schemas, *schemas[name])
	}
	return cat, nil
}
