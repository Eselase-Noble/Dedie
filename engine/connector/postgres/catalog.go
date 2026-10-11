package postgres

import (
	"context"

	"dedie/engine/connector"
)

const userSchemas = `n.nspname NOT IN ('pg_catalog','information_schema','pg_toast') AND n.nspname NOT LIKE 'pg\_temp\_%' AND n.nspname NOT LIKE 'pg\_toast\_temp\_%'`

// Catalog loads the schema tree for the query browser in a handful of
// catalog queries, independent of how many tables exist.
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

	rows, err := s.conn.Query(ctx, `SELECT n.nspname, pg_get_userbyid(n.nspowner) FROM pg_namespace n WHERE `+userSchemas+` ORDER BY 1`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var name, owner string
		if err := rows.Scan(&name, &owner); err != nil {
			return nil, err
		}
		get(name).Owner = owner
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	type relKey struct{ schema, name string }
	rels := map[relKey]*connector.Table{}
	var relOrder []relKey
	relKind := map[relKey]string{}
	rows, err = s.conn.Query(ctx, `SELECT n.nspname, c.relname, c.relkind::text, greatest(c.reltuples,0)::bigint,
			CASE WHEN c.relkind IN ('r','m','p') THEN pg_total_relation_size(c.oid) ELSE 0 END,
			pg_get_userbyid(c.relowner)
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p','v','m') AND `+userSchemas+` ORDER BY 1,2`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var k relKey
		var kind string
		t := &connector.Table{Columns: []connector.TableColumn{}, Indexes: []string{}}
		if err := rows.Scan(&k.schema, &k.name, &kind, &t.EstRows, &t.Size, &t.Owner); err != nil {
			return nil, err
		}
		t.Name = k.name
		rels[k], relKind[k] = t, kind
		relOrder = append(relOrder, k)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	rows, err = s.conn.Query(ctx, `SELECT n.nspname, c.relname, a.attname, format_type(a.atttypid,a.atttypmod), NOT a.attnotnull,
			coalesce(pg_get_expr(d.adbin,d.adrelid),''),
			EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid=c.oid AND i.indisprimary AND a.attnum = ANY(i.indkey))
		FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
		WHERE a.attnum>0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m') AND `+userSchemas+`
		ORDER BY 1,2,a.attnum`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var k relKey
		var col connector.TableColumn
		if err := rows.Scan(&k.schema, &k.name, &col.Name, &col.Type, &col.Nullable, &col.Default, &col.PrimaryKey); err != nil {
			return nil, err
		}
		if t, ok := rels[k]; ok {
			t.Columns = append(t.Columns, col)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	rows, err = s.conn.Query(ctx, `SELECT schemaname, tablename, indexdef FROM pg_indexes
		WHERE schemaname NOT IN ('pg_catalog','information_schema','pg_toast') ORDER BY 1,2,indexname`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var k relKey
		var def string
		if err := rows.Scan(&k.schema, &k.name, &def); err != nil {
			return nil, err
		}
		if t, ok := rels[k]; ok {
			t.Indexes = append(t.Indexes, def)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	for _, k := range relOrder {
		sc := get(k.schema)
		if relKind[k] == "v" || relKind[k] == "m" {
			sc.Views = append(sc.Views, *rels[k])
		} else {
			sc.Tables = append(sc.Tables, *rels[k])
		}
	}

	rows, err = s.conn.Query(ctx, `SELECT n.nspname, p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE `+userSchemas+` AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=p.oid AND d.deptype='e')
		ORDER BY 1,2 LIMIT 5000`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, err
		}
		sc := get(schema)
		sc.Functions = append(sc.Functions, name)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	rows, err = s.conn.Query(ctx, `SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind='S' AND `+userSchemas+` ORDER BY 1,2`)
	if err != nil {
		return nil, friendly(err)
	}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, err
		}
		sc := get(schema)
		sc.Sequences = append(sc.Sequences, name)
	}
	if err := rows.Err(); err != nil {
		return nil, friendly(err)
	}

	cat := &connector.Catalog{Schemas: []connector.Schema{}}
	for _, name := range order {
		cat.Schemas = append(cat.Schemas, *schemas[name])
	}
	return cat, nil
}
