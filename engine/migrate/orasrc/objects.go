package orasrc

import (
	"context"
	"regexp"
	"strings"

	"dedie/engine/connector/oracle"
)

// Sequence is a source sequence. The numbers are kept as Oracle prints
// them, because its default bounds are wider than a 64-bit integer.
type Sequence struct {
	Owner, Name         string
	Min, Max, Increment string
	// Cache is the number of values kept in memory; empty or "0" means
	// the sequence does not cache.
	Cache string
	// Last is LAST_NUMBER as it stood when the catalog was loaded: at or
	// beyond every value handed out, cached ones included.
	Last  string
	Cycle bool
}

// Index is a secondary index, including the unique indexes behind unique
// constraints. The primary key's index is not listed: the key is part of
// the table.
type Index struct {
	Owner, Table string // the table it is on
	Name         string
	// Type is Oracle's INDEX_TYPE: NORMAL, NORMAL/REV, BITMAP,
	// FUNCTION-BASED NORMAL, DOMAIN and so on.
	Type   string
	Unique bool
	// Columns are the indexed columns in order. A name that is not a
	// column of the table stands for an expression: descending columns
	// are stored as expressions too.
	Columns []string
}

// Check is an enabled check constraint, with its condition as written.
type Check struct {
	Owner, Table string
	Name         string
	Condition    string
}

// ForeignKey is an enabled foreign key of a table in scope. The table it
// refers to may lie outside the migration; Catalog.Table tells.
type ForeignKey struct {
	Owner, Table string
	Name         string
	Columns      []string
	RefOwner     string
	RefTable     string
	RefColumns   []string
	// DeleteRule is NO ACTION, CASCADE or SET NULL.
	DeleteRule string
	Deferrable bool
	// Deferred says a deferrable constraint is checked at commit unless
	// a transaction asks otherwise.
	Deferred bool
}

var notNullRe = regexp.MustCompile(`^"[^"]+" IS NOT NULL$`)

func (c *Catalog) loadSequences(ctx context.Context, src oracle.Queryer, in string) error {
	// Sequences named ISEQ$$ belong to identity columns.
	rows, err := query(ctx, src, `SELECT sequence_owner, sequence_name, TO_CHAR(min_value), TO_CHAR(max_value), TO_CHAR(increment_by),
			cycle_flag, TO_CHAR(cache_size), TO_CHAR(last_number)
		FROM all_sequences WHERE sequence_owner `+in+` AND sequence_name NOT LIKE 'ISEQ$$%' ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		c.Sequences = append(c.Sequences, Sequence{Owner: r[0], Name: r[1], Min: r[2], Max: r[3], Increment: r[4],
			Cycle: r[5] == "Y", Cache: r[6], Last: r[7]})
	}
	return nil
}

// SequencePositions reads where every sequence of the schemas in scope
// stands now, keyed by OWNER.NAME; those behind identity columns are
// included. A target is moved to these numbers when a migration ends, so
// that it never hands out one the source already used.
func SequencePositions(ctx context.Context, src oracle.Queryer, c *Catalog) (map[string]string, error) {
	// LAST_NUMBER is at or beyond every value handed out, cached ones included.
	rows, err := query(ctx, src, `SELECT sequence_owner, sequence_name, TO_CHAR(last_number) FROM all_sequences
		WHERE sequence_owner IN (`+c.ownerList()+`)`)
	if err != nil {
		return nil, err
	}
	next := map[string]string{}
	for _, row := range rows {
		next[row[0]+"."+row[1]] = row[2]
	}
	return next, nil
}

// loadIndexes lists secondary indexes, including the unique indexes
// behind unique constraints. The primary key's index comes with the table.
func (c *Catalog) loadIndexes(ctx context.Context, src oracle.Queryer, in string) error {
	rows, err := query(ctx, src, `SELECT i.table_owner, i.table_name, i.index_name, i.index_type, i.uniqueness,
			LISTAGG(c.column_name, CHR(10)) WITHIN GROUP (ORDER BY c.column_position)
		FROM all_indexes i JOIN all_ind_columns c ON c.index_owner = i.owner AND c.index_name = i.index_name
		WHERE i.table_owner `+in+` AND i.index_type NOT IN ('LOB', 'IOT - TOP')
		  AND NOT EXISTS (SELECT 1 FROM all_constraints k WHERE k.owner = i.table_owner AND k.table_name = i.table_name
		                  AND k.constraint_type = 'P' AND k.index_name = i.index_name)
		GROUP BY i.table_owner, i.table_name, i.index_name, i.index_type, i.uniqueness ORDER BY 1, 2, 3`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if c.Table(r[0], r[1]) == nil {
			continue
		}
		c.Indexes = append(c.Indexes, Index{Owner: r[0], Table: r[1], Name: r[2], Type: r[3], Unique: r[4] == "UNIQUE",
			Columns: strings.Split(r[5], "\n")})
	}
	return nil
}

// loadChecks lists check constraints as they are written.
func (c *Catalog) loadChecks(ctx context.Context, src oracle.Queryer, in string) error {
	rows, err := query(ctx, src, `SELECT owner, table_name, constraint_name, search_condition_vc FROM all_constraints
		WHERE owner `+in+` AND constraint_type = 'C' AND status = 'ENABLED' ORDER BY 1, 2, 3`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		cond := strings.TrimSpace(r[3])
		// NOT NULL is stored as a check constraint and is already part of
		// the column definition.
		if c.Table(r[0], r[1]) == nil || cond == "" || notNullRe.MatchString(cond) {
			continue
		}
		c.Checks = append(c.Checks, Check{Owner: r[0], Table: r[1], Name: r[2], Condition: cond})
	}
	return nil
}

// loadForeignKeys lists the enabled foreign keys of the tables in scope,
// with their ON DELETE rule.
func (c *Catalog) loadForeignKeys(ctx context.Context, src oracle.Queryer, in string) error {
	rows, err := query(ctx, src, `SELECT k.owner, k.table_name, k.constraint_name, k.delete_rule, k.deferrable, k.deferred, r.owner, r.table_name,
			(SELECT LISTAGG(c.column_name, CHR(10)) WITHIN GROUP (ORDER BY c.position) FROM all_cons_columns c
			  WHERE c.owner = k.owner AND c.constraint_name = k.constraint_name),
			(SELECT LISTAGG(c.column_name, CHR(10)) WITHIN GROUP (ORDER BY c.position) FROM all_cons_columns c
			  WHERE c.owner = r.owner AND c.constraint_name = r.constraint_name)
		FROM all_constraints k JOIN all_constraints r ON r.owner = k.r_owner AND r.constraint_name = k.r_constraint_name
		WHERE k.owner `+in+` AND k.constraint_type = 'R' AND k.status = 'ENABLED' ORDER BY 1, 2, 3`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if c.Table(r[0], r[1]) == nil {
			continue
		}
		c.ForeignKeys = append(c.ForeignKeys, ForeignKey{Owner: r[0], Table: r[1], Name: r[2], DeleteRule: r[3],
			Deferrable: r[4] == "DEFERRABLE", Deferred: r[5] == "DEFERRED", RefOwner: r[6], RefTable: r[7],
			Columns: strings.Split(r[8], "\n"), RefColumns: strings.Split(r[9], "\n")})
	}
	return nil
}
