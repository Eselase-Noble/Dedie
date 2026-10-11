package oraora

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"dedie/engine/connector/oracle"
	"dedie/engine/migrate/orasrc"
)

// definitions is everything read from the source about its objects
// beyond what the catalog holds: the statements that create them, as the
// source writes them, and the few facts needed to put those in order.
type definitions struct {
	// tables holds each table's CREATE TABLE statement by OWNER.NAME, with
	// the schema as the source has it.
	tables map[string]string
	// temporary lists each schema's global temporary tables. They hold no
	// rows to copy, so the catalog leaves them out; their definition is
	// carried over all the same.
	temporary map[string][]string
	// objects holds the other definitions by schema and DBMS_METADATA
	// object type, already written for the schema that receives them.
	objects map[string]map[string][]definition
	// indexTable gives, by OWNER.INDEX, the table of each index that was
	// named by a person. Oracle's own names for the indexes it makes
	// behind constraints and large objects mean nothing on another server.
	indexTable map[string]string
	// identities lists the identity columns: owner, table, column,
	// ALWAYS or BY DEFAULT, the sequence behind it, and YES when a NULL
	// is replaced as well.
	identities [][]string
	// left is what was found that is not carried over.
	left []string
}

// queryText runs a query and returns its rows as text; NULL comes back
// as an empty string.
func queryText(ctx context.Context, db oracle.Queryer, text string, args ...any) ([][]string, error) {
	rows, err := db.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, oracle.Friendly(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		raw := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, oracle.Friendly(err)
		}
		row := make([]string, len(cols))
		for i, v := range raw {
			row[i] = v.String
		}
		out = append(out, row)
	}
	return out, oracle.Friendly(rows.Err())
}

// inParallel runs the jobs on a few goroutines and returns the first error.
func inParallel(workers int, jobs []func() error) error {
	queue := make(chan func() error)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range queue {
				if err := job(); err != nil {
					once.Do(func() { firstErr = err })
				}
			}
		}()
	}
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	wg.Wait()
	return firstErr
}

// definitionReaders is how many sessions read definitions at once.
const definitionReaders = 4

// readDefinitions asks the source for the definition of everything in
// scope. schemas says where each source schema goes, so that the
// definitions come back written for it. A table-level migration takes the
// tables with their indexes, constraints and triggers, and the sequences
// their defaults may call; views, stored code and synonyms are objects
// in their own right and are left where they are.
func readDefinitions(ctx context.Context, src *sql.DB, origin string, cat *orasrc.Catalog, schemas map[string]string, tableScope bool) (*definitions, error) {
	d := &definitions{tables: map[string]string{}, temporary: map[string][]string{}, objects: map[string]map[string][]definition{}, indexTable: map[string]string{}}
	quoted := make([]string, len(cat.Owners))
	for i, o := range cat.Owners {
		quoted[i] = literal(o)
		d.objects[o] = map[string][]definition{}
	}
	in := "IN (" + strings.Join(quoted, ", ") + ")"

	// Nothing below uses a dictionary column newer than Oracle 12.1.
	rows, err := queryText(ctx, src, `SELECT table_owner, index_name, table_name FROM all_indexes
		WHERE table_owner `+in+` AND generated = 'N' AND index_type NOT IN ('LOB', 'IOT - TOP')`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		d.indexTable[r[0]+"."+r[1]] = r[2]
	}
	if d.identities, err = queryText(ctx, src, `SELECT i.owner, i.table_name, i.column_name, i.generation_type, i.sequence_name, c.default_on_null
		FROM all_tab_identity_cols i JOIN all_tab_columns c ON c.owner = i.owner AND c.table_name = i.table_name AND c.column_name = i.column_name
		WHERE i.owner `+in+` ORDER BY 1, 2, 3`); err != nil {
		return nil, err
	}
	if !tableScope {
		rows, err := queryText(ctx, src, `SELECT owner, table_name FROM all_tables
			WHERE owner `+in+` AND temporary = 'Y' AND nested = 'NO' AND secondary = 'N' AND dropped = 'NO' ORDER BY 1, 2`)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			d.temporary[r[0]] = append(d.temporary[r[0]], r[1])
		}
	}
	d.findLeft(ctx, src, in)

	// A table's definition is read again only when the table has changed,
	// or is another table that took the name.
	stamps := map[string]string{}
	rows, err = queryText(ctx, src, `SELECT owner, object_name, object_id || '/' || TO_CHAR(last_ddl_time, 'YYYYMMDDHH24MISS') FROM all_objects
		WHERE owner `+in+` AND object_type = 'TABLE'`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		stamps[r[0]+"."+r[1]] = r[2]
	}
	type name struct{ owner, table string }
	var names []name
	for _, t := range cat.Tables {
		names = append(names, name{t.Owner, t.Name})
	}
	for owner, list := range d.temporary {
		for _, table := range list {
			names = append(names, name{owner, table})
		}
	}

	var mu sync.Mutex
	var jobs []func() error
	// Each reader keeps a session of its own and takes tables as they come.
	next := make(chan name, len(names))
	for _, n := range names {
		next <- n
	}
	close(next)
	for i := 0; i < min(definitionReaders, len(names)); i++ {
		jobs = append(jobs, func() error {
			reader, err := newTableReader(ctx, src, origin)
			if err != nil {
				return err
			}
			defer reader.close(ctx)
			for n := range next {
				text, err := reader.read(ctx, n.owner, n.table, stamps[n.owner+"."+n.table])
				if err != nil {
					return err
				}
				mu.Lock()
				d.tables[n.owner+"."+n.table] = text
				mu.Unlock()
			}
			return nil
		})
	}
	types := [][2]string{{"SEQUENCE", bySchema}, {"CONSTRAINT", byBase}, {"REF_CONSTRAINT", byBase}, {"INDEX", byBase}, {"COMMENT", byBase}, {"TRIGGER", bySchema}}
	if !tableScope {
		for _, t := range codeTypes {
			types = append(types, [2]string{t, bySchema})
		}
	}
	for _, owner := range cat.Owners {
		for _, t := range types {
			jobs = append(jobs, func() error {
				defs, err := fetchDefinitions(ctx, src, t[0], t[1], owner, schemas[owner])
				if err != nil {
					return err
				}
				mu.Lock()
				d.objects[owner][t[0]] = defs
				mu.Unlock()
				return nil
			})
		}
	}
	if err := inParallel(definitionReaders*2, jobs); err != nil {
		return nil, err
	}
	return d, nil
}

// findLeft counts the things in the schemas that are not carried over,
// so that preflight can name them. A view the account may not read
// counts as nothing found.
func (d *definitions) findLeft(ctx context.Context, src *sql.DB, in string) {
	for _, c := range []struct{ what, query string }{
		{"materialized view(s)", `SELECT COUNT(*) FROM all_mviews WHERE owner ` + in},
		{"external table(s)", `SELECT COUNT(*) FROM all_external_tables WHERE owner ` + in},
		{"database link(s)", `SELECT COUNT(*) FROM all_db_links WHERE owner ` + in},
		{"scheduler job(s)", `SELECT COUNT(*) FROM all_scheduler_jobs WHERE owner ` + in},
		{"queue(s)", `SELECT COUNT(*) FROM all_queues WHERE owner ` + in + ` AND queue_type = 'NORMAL_QUEUE'`},
	} {
		var n int
		if err := src.QueryRowContext(ctx, c.query).Scan(&n); err == nil && n > 0 {
			d.left = append(d.left, fmt.Sprintf("%d %s", n, c.what))
		}
	}
}
