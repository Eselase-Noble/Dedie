package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dedie/engine/connector"
)

// userSchemas selects the schemas that belong to the database's users
// rather than to Oracle itself.
const userSchemas = `IN (SELECT username FROM all_users WHERE oracle_maintained = 'N')`

// TypeName renders a column's type the way it is written in DDL.
func TypeName(dataType string, charLength, precision, scale sql.NullInt64, dataLength int64) string {
	switch {
	case dataType == "NUMBER" && precision.Valid && scale.Valid && scale.Int64 != 0:
		return fmt.Sprintf("NUMBER(%d,%d)", precision.Int64, scale.Int64)
	case dataType == "NUMBER" && precision.Valid:
		return fmt.Sprintf("NUMBER(%d)", precision.Int64)
	case dataType == "FLOAT" && precision.Valid:
		return fmt.Sprintf("FLOAT(%d)", precision.Int64)
	case dataType == "VARCHAR2" || dataType == "NVARCHAR2" || dataType == "CHAR" || dataType == "NCHAR":
		return fmt.Sprintf("%s(%d)", dataType, charLength.Int64)
	case dataType == "RAW":
		return fmt.Sprintf("RAW(%d)", dataLength)
	}
	return dataType
}

// Catalog loads the object tree for the query browser. An Oracle schema
// is a user; the ones Oracle maintains for itself are left out.
func (s *session) Catalog(ctx context.Context) (*connector.Catalog, error) {
	schemas := map[string]*connector.Schema{}
	var order []string
	get := func(name string) *connector.Schema {
		if sc, ok := schemas[name]; ok {
			return sc
		}
		sc := &connector.Schema{Name: name, Owner: name, Tables: []connector.Table{}, Views: []connector.Table{}, Functions: []string{}, Sequences: []string{}}
		schemas[name] = sc
		order = append(order, name)
		return sc
	}
	// each runs a query and hands every row to fn.
	each := func(query string, dest []any, fn func()) error {
		rows, err := s.conn.QueryContext(ctx, query)
		if err != nil {
			return Friendly(err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			fn()
		}
		return Friendly(rows.Err())
	}

	var name string
	if err := each(`SELECT username FROM all_users WHERE oracle_maintained = 'N' ORDER BY 1`, []any{&name}, func() { get(name) }); err != nil {
		return nil, err
	}

	type relKey struct{ schema, name string }
	rels := map[relKey]*connector.Table{}
	isView := map[relKey]bool{}
	var relOrder []relKey
	var k relKey
	var rows, blocks sql.NullInt64
	add := func(view bool) {
		rels[k] = &connector.Table{Name: k.name, Owner: k.schema, EstRows: rows.Int64, Size: blocks.Int64 * 8192,
			Columns: []connector.TableColumn{}, Indexes: []string{}}
		isView[k] = view
		relOrder = append(relOrder, k)
	}
	err := each(`SELECT owner, table_name, num_rows, blocks FROM all_tables
		WHERE owner `+userSchemas+` AND nested = 'NO' AND secondary = 'N' AND dropped = 'NO' AND (iot_type IS NULL OR iot_type = 'IOT')
		ORDER BY 1, 2`, []any{&k.schema, &k.name, &rows, &blocks}, func() { add(false) })
	if err != nil {
		return nil, err
	}
	rows, blocks = sql.NullInt64{}, sql.NullInt64{}
	if err := each(`SELECT owner, view_name FROM all_views WHERE owner `+userSchemas+` ORDER BY 1, 2`, []any{&k.schema, &k.name}, func() { add(true) }); err != nil {
		return nil, err
	}

	var col connector.TableColumn
	var dataType, nullable string
	var charLen, precision, scale sql.NullInt64
	var dataLen int64
	var def sql.NullString
	var isPK int
	err = each(`SELECT c.owner, c.table_name, c.column_name, c.data_type, c.char_length, c.data_precision, c.data_scale, c.data_length,
			c.nullable, c.data_default,
			(SELECT COUNT(*) FROM all_constraints k JOIN all_cons_columns kc ON kc.owner = k.owner AND kc.constraint_name = k.constraint_name
			  WHERE k.owner = c.owner AND k.table_name = c.table_name AND k.constraint_type = 'P' AND kc.column_name = c.column_name)
		FROM all_tab_columns c WHERE c.owner `+userSchemas+` ORDER BY c.owner, c.table_name, c.column_id`,
		[]any{&k.schema, &k.name, &col.Name, &dataType, &charLen, &precision, &scale, &dataLen, &nullable, &def, &isPK}, func() {
			if t, ok := rels[k]; ok {
				col.Type = TypeName(dataType, charLen, precision, scale, dataLen)
				col.Nullable, col.PrimaryKey, col.Default = nullable == "Y", isPK > 0, strings.TrimSpace(def.String)
				t.Columns = append(t.Columns, col)
			}
		})
	if err != nil {
		return nil, err
	}

	var text string
	err = each(`SELECT i.table_owner, i.table_name,
			DECODE(i.uniqueness, 'UNIQUE', 'UNIQUE ', '') || i.index_name || ' (' ||
			LISTAGG(c.column_name, ', ') WITHIN GROUP (ORDER BY c.column_position) || ')'
		FROM all_indexes i JOIN all_ind_columns c ON c.index_owner = i.owner AND c.index_name = i.index_name
		WHERE i.table_owner `+userSchemas+` AND i.index_type <> 'LOB'
		GROUP BY i.table_owner, i.table_name, i.index_name, i.uniqueness ORDER BY 1, 2, 3`,
		[]any{&k.schema, &k.name, &text}, func() {
			if t, ok := rels[k]; ok {
				t.Indexes = append(t.Indexes, text)
			}
		})
	if err != nil {
		return nil, err
	}

	for _, key := range relOrder {
		sc := get(key.schema)
		if isView[key] {
			sc.Views = append(sc.Views, *rels[key])
		} else {
			sc.Tables = append(sc.Tables, *rels[key])
		}
	}

	var schema string
	err = each(`SELECT owner, object_name || ' (' || LOWER(object_type) || ')' FROM all_objects
		WHERE owner `+userSchemas+` AND object_type IN ('PROCEDURE','FUNCTION','PACKAGE') ORDER BY 1, 2`,
		[]any{&schema, &text}, func() { sc := get(schema); sc.Functions = append(sc.Functions, text) })
	if err != nil {
		return nil, err
	}
	err = each(`SELECT sequence_owner, sequence_name FROM all_sequences
		WHERE sequence_owner `+userSchemas+` AND sequence_name NOT LIKE 'ISEQ$$%' ORDER BY 1, 2`,
		[]any{&schema, &text}, func() { sc := get(schema); sc.Sequences = append(sc.Sequences, text) })
	if err != nil {
		return nil, err
	}

	cat := &connector.Catalog{Schemas: []connector.Schema{}}
	for _, n := range order {
		cat.Schemas = append(cat.Schemas, *schemas[n])
	}
	return cat, nil
}
