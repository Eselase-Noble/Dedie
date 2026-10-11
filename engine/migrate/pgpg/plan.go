package pgpg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Stmt is one DDL statement to run on the target.
type Stmt struct {
	Kind string
	Name string
	SQL  string
	// Soft statements may fail without failing the job; the failure is
	// reported to the operator. Views, functions and triggers are soft
	// because they can depend on objects outside the migration's scope.
	Soft bool
	err  error
}

const kindForeignKey = "foreign key"

// Table is a source table that will be copied and replicated.
type Table struct {
	OID      uint32
	Schema   string
	Name     string
	Cols     []string // columns to copy; generated columns are excluded
	EstRows  int64
	HasFloat bool
}

func (t Table) QName() string { return qname(t.Schema, t.Name) }

// Skipped is a table the pipeline cannot replicate, with the reason.
type Skipped struct {
	Schema   string
	Name     string
	Reason   string
	Blocking bool
}

// Plan is everything derived from the source catalog: which tables move and
// the DDL to run before the snapshot, after it, and at cutover.
type Plan struct {
	VersionNum int
	Schemas    []string
	Tables     []Table
	Skipped    []Skipped
	Pre        []Stmt
	Post       []Stmt
	Final      []Stmt
}

const userSchemaFilter = `n.nspname NOT IN ('pg_catalog','information_schema','pg_toast','` + metaSchema + `') AND n.nspname NOT LIKE 'pg\_temp\_%' AND n.nspname NOT LIKE 'pg\_toast\_temp\_%'`

// notExtension excludes objects that belong to an extension; those are
// recreated by CREATE EXTENSION on the target.
func notExtension(class, oidExpr string) string {
	return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend x WHERE x.classid='pg_catalog.%s'::regclass AND x.objid=%s AND x.deptype='e')`, class, oidExpr)
}

// BuildPlan reads the source catalog. The DDL comes from PostgreSQL's own
// deparse functions (pg_get_constraintdef, pg_get_indexdef, ...), with an
// empty search_path so every name in it is schema-qualified.
func BuildPlan(ctx context.Context, src *pgx.Conn, cfg Config) (*Plan, error) {
	p := &Plan{}
	if _, err := src.Exec(ctx, `SELECT pg_catalog.set_config('search_path', '', false)`); err != nil {
		return nil, err
	}
	if err := src.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&p.VersionNum); err != nil {
		return nil, err
	}

	var only any
	if len(cfg.Schemas) > 0 {
		only = cfg.Schemas
	}
	if err := collect(ctx, src, &p.Schemas, `SELECT n.nspname FROM pg_catalog.pg_namespace n
		WHERE `+userSchemaFilter+` AND ($1::text[] IS NULL OR n.nspname = ANY($1)) ORDER BY 1`, only); err != nil {
		return nil, err
	}
	if len(p.Schemas) == 0 {
		return nil, fmt.Errorf("no schemas selected for migration")
	}

	if err := p.loadTables(ctx, src, cfg); err != nil {
		return nil, fmt.Errorf("read tables: %w", err)
	}
	oids := make([]uint32, len(p.Tables))
	for i, t := range p.Tables {
		oids[i] = t.OID
	}

	for _, s := range p.Schemas {
		p.Pre = append(p.Pre, Stmt{Kind: "schema", Name: s, SQL: "CREATE SCHEMA IF NOT EXISTS " + qident(s)})
	}
	steps := []func() error{
		func() error { return p.loadExtensions(ctx, src) },
		func() error { return p.loadTypes(ctx, src) },
		func() error { return p.loadSequences(ctx, src) },
		func() error { return p.loadFunctions(ctx, src) },
		func() error { return p.loadTableDDL(ctx, src, oids) },
		func() error { return p.loadConstraints(ctx, src, oids) },
		func() error { return p.loadSequenceOwners(ctx, src, oids) },
		func() error { return p.loadIndexes(ctx, src, oids) },
		func() error { return p.loadViews(ctx, src) },
		func() error { return p.loadTriggers(ctx, src, oids) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, fmt.Errorf("read source schema: %w", err)
		}
	}
	return p, nil
}

func collect(ctx context.Context, conn *pgx.Conn, dst *[]string, sql string, args ...any) error {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	*dst = out
	return err
}

// stmts runs a query returning (name, ddl) pairs and appends them to dst.
func stmts(ctx context.Context, conn *pgx.Conn, dst *[]Stmt, kind string, soft bool, sql string, args ...any) error {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return err
		}
		*dst = append(*dst, Stmt{Kind: kind, Name: name, SQL: ddl, Soft: soft})
	}
	return rows.Err()
}

func (p *Plan) loadTables(ctx context.Context, src *pgx.Conn, cfg Config) error {
	excluded := map[string]bool{}
	for _, e := range cfg.ExcludeTables {
		excluded[strings.TrimSpace(e)] = true
	}
	rows, err := src.Query(ctx, `SELECT c.oid, n.nspname, c.relname, c.relkind::text, c.relpersistence::text, c.relreplident::text,
			greatest(c.reltuples,0)::bigint,
			EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid=c.oid AND i.indisprimary),
			EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indrelid=c.oid AND i.indisreplident),
			EXISTS (SELECT 1 FROM pg_catalog.pg_inherits h WHERE h.inhrelid=c.oid),
			EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid=a.atttypid
				WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped AND t.typname IN ('float4','float8','_float4','_float8'))
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p') AND n.nspname = ANY($1) AND `+notExtension("pg_class", "c.oid")+`
		ORDER BY 2,3`, p.Schemas)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t Table
		var kind, persistence, replident string
		var hasPK, hasReplIdx, isChild bool
		if err := rows.Scan(&t.OID, &t.Schema, &t.Name, &kind, &persistence, &replident, &t.EstRows, &hasPK, &hasReplIdx, &isChild, &t.HasFloat); err != nil {
			return err
		}
		if excluded[t.Schema+"."+t.Name] {
			continue
		}
		skip := func(reason string, blocking bool) {
			p.Skipped = append(p.Skipped, Skipped{Schema: t.Schema, Name: t.Name, Reason: reason, Blocking: blocking})
		}
		hasKey := replident == "f" || (replident == "d" && hasPK) || (replident == "i" && hasReplIdx)
		switch {
		case kind == "p":
			skip("partitioned tables are not supported yet", false)
		case isChild:
			skip("partitions and inheritance children are not supported yet", false)
		case persistence != "p":
			skip("unlogged and temporary tables are not written to the replication log", false)
		case !hasKey:
			// Publishing a table without a replica identity makes UPDATE and
			// DELETE fail on the source, so this must stop the migration.
			skip("no primary key or replica identity; add one, or exclude the table", true)
		default:
			p.Tables = append(p.Tables, t)
		}
	}
	return rows.Err()
}

func (p *Plan) loadExtensions(ctx context.Context, src *pgx.Conn) error {
	return stmts(ctx, src, &p.Pre, "extension", false, `SELECT e.extname,
			format('CREATE EXTENSION IF NOT EXISTS %I', e.extname) ||
			CASE WHEN n.nspname = ANY($1) THEN format(' WITH SCHEMA %I', n.nspname) ELSE '' END
		FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace
		WHERE e.extname <> 'plpgsql' ORDER BY 1`, p.Schemas)
}

func (p *Plan) loadTypes(ctx context.Context, src *pgx.Conn) error {
	notExt := notExtension("pg_type", "t.oid")
	if err := stmts(ctx, src, &p.Pre, "type", true, `SELECT format('%I.%I', n.nspname, t.typname),
			format('CREATE TYPE %I.%I AS ENUM (%s)', n.nspname, t.typname,
				coalesce((SELECT string_agg(quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder) FROM pg_catalog.pg_enum e WHERE e.enumtypid=t.oid), ''))
		FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
		WHERE t.typtype='e' AND n.nspname = ANY($1) AND `+notExt+` ORDER BY 1`, p.Schemas); err != nil {
		return err
	}
	if err := stmts(ctx, src, &p.Pre, "type", true, `SELECT format('%I.%I', n.nspname, t.typname),
			format('CREATE TYPE %I.%I AS (%s)', n.nspname, t.typname,
				(SELECT string_agg(format('%I %s', a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)), ', ' ORDER BY a.attnum)
				 FROM pg_catalog.pg_attribute a WHERE a.attrelid=t.typrelid AND a.attnum>0 AND NOT a.attisdropped))
		FROM pg_catalog.pg_type t JOIN pg_catalog.pg_class c ON c.oid=t.typrelid AND c.relkind='c'
		JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
		WHERE n.nspname = ANY($1) AND `+notExt+` ORDER BY 1`, p.Schemas); err != nil {
		return err
	}
	return stmts(ctx, src, &p.Pre, "domain", true, `SELECT format('%I.%I', n.nspname, t.typname),
			format('CREATE DOMAIN %I.%I AS %s', n.nspname, t.typname, pg_catalog.format_type(t.typbasetype, t.typtypmod)) ||
			CASE WHEN t.typdefault IS NOT NULL THEN ' DEFAULT ' || t.typdefault ELSE '' END ||
			CASE WHEN t.typnotnull THEN ' NOT NULL' ELSE '' END ||
			coalesce((SELECT string_agg(' CONSTRAINT ' || quote_ident(c.conname) || ' ' || pg_catalog.pg_get_constraintdef(c.oid), '')
				FROM pg_catalog.pg_constraint c WHERE c.contypid=t.oid AND c.contype='c'), '')
		FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
		WHERE t.typtype='d' AND n.nspname = ANY($1) AND `+notExt+` ORDER BY 1`, p.Schemas)
}

// loadSequences covers standalone and serial sequences. Identity sequences
// are created implicitly with their column.
func (p *Plan) loadSequences(ctx context.Context, src *pgx.Conn) error {
	return stmts(ctx, src, &p.Pre, "sequence", false, `SELECT format('%I.%I', s.schemaname, s.sequencename),
			format('CREATE SEQUENCE %I.%I AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s%s',
				s.schemaname, s.sequencename, s.data_type::text, s.increment_by, s.min_value, s.max_value, s.start_value, s.cache_size,
				CASE WHEN s.cycle THEN ' CYCLE' ELSE '' END)
		FROM pg_catalog.pg_sequences s
		WHERE s.schemaname = ANY($1) AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend x
			WHERE x.classid='pg_catalog.pg_class'::regclass AND x.objid=format('%I.%I', s.schemaname, s.sequencename)::regclass AND x.deptype IN ('i','e'))
		ORDER BY 1`, p.Schemas)
}

func (p *Plan) loadSequenceOwners(ctx context.Context, src *pgx.Conn, oids []uint32) error {
	return stmts(ctx, src, &p.Pre, "sequence owner", true, `SELECT format('%I.%I', sn.nspname, s.relname),
			format('ALTER SEQUENCE %I.%I OWNED BY %I.%I.%I', sn.nspname, s.relname, tn.nspname, t.relname, a.attname)
		FROM pg_catalog.pg_depend d
		JOIN pg_catalog.pg_class s ON s.oid=d.objid AND s.relkind='S'
		JOIN pg_catalog.pg_namespace sn ON sn.oid=s.relnamespace
		JOIN pg_catalog.pg_class t ON t.oid=d.refobjid
		JOIN pg_catalog.pg_namespace tn ON tn.oid=t.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid=t.oid AND a.attnum=d.refobjsubid
		WHERE d.classid='pg_catalog.pg_class'::regclass AND d.refclassid='pg_catalog.pg_class'::regclass
			AND d.deptype='a' AND t.oid = ANY($1) ORDER BY 1`, oids)
}

func (p *Plan) loadFunctions(ctx context.Context, src *pgx.Conn) error {
	plain := "p.prokind IN ('f','p')"
	if p.VersionNum < 110000 {
		plain = "NOT p.proisagg AND NOT p.proiswindow"
	}
	return stmts(ctx, src, &p.Pre, "function", true, `SELECT format('%I.%I(%s)', n.nspname, p.proname, pg_catalog.pg_get_function_identity_arguments(p.oid)),
			pg_catalog.pg_get_functiondef(p.oid)
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname = ANY($1) AND `+plain+` AND `+notExtension("pg_proc", "p.oid")+` ORDER BY 1`, p.Schemas)
}

// loadTableDDL builds CREATE TABLE statements column by column and records
// which columns the snapshot copies.
func (p *Plan) loadTableDDL(ctx context.Context, src *pgx.Conn, oids []uint32) error {
	generated := "''"
	if p.VersionNum >= 120000 {
		generated = "a.attgenerated::text"
	}
	rows, err := src.Query(ctx, `SELECT a.attrelid, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
			coalesce(pg_catalog.pg_get_expr(d.adbin, d.adrelid), ''), a.attidentity::text, `+generated+`,
			CASE WHEN a.attcollation <> 0 AND a.attcollation <> t.typcollation THEN
				(SELECT format('%I.%I', cn.nspname, co.collname) FROM pg_catalog.pg_collation co
				 JOIN pg_catalog.pg_namespace cn ON cn.oid=co.collnamespace WHERE co.oid=a.attcollation)
			ELSE '' END
		FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid=a.atttypid
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
		WHERE a.attrelid = ANY($1) AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attrelid, a.attnum`, oids)
	if err != nil {
		return err
	}
	defer rows.Close()
	defs := map[uint32][]string{}
	cols := map[uint32][]string{}
	always := map[uint32][]string{}
	for rows.Next() {
		var oid uint32
		var name, typ, def, identity, gen, collation string
		var notNull bool
		if err := rows.Scan(&oid, &name, &typ, &notNull, &def, &identity, &gen, &collation); err != nil {
			return err
		}
		col := qident(name) + " " + typ
		if collation != "" {
			col += " COLLATE " + collation
		}
		switch {
		case gen == "s":
			col += " GENERATED ALWAYS AS (" + def + ") STORED"
		case gen == "v":
			col += " GENERATED ALWAYS AS (" + def + ") VIRTUAL"
		case identity == "a":
			// A GENERATED ALWAYS column rejects the explicit values that
			// replicated updates carry, so it is created BY DEFAULT and
			// switched back at cutover.
			col += " GENERATED BY DEFAULT AS IDENTITY"
			always[oid] = append(always[oid], name)
		case identity == "d":
			col += " GENERATED BY DEFAULT AS IDENTITY"
		case def != "":
			col += " DEFAULT " + def
		}
		if notNull {
			col += " NOT NULL"
		}
		defs[oid] = append(defs[oid], col)
		if gen == "" {
			cols[oid] = append(cols[oid], name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range p.Tables {
		t := &p.Tables[i]
		t.Cols = cols[t.OID]
		p.Pre = append(p.Pre, Stmt{Kind: "table", Name: t.Schema + "." + t.Name,
			SQL: "CREATE TABLE " + t.QName() + " (\n  " + strings.Join(defs[t.OID], ",\n  ") + "\n)"})
		for _, col := range always[t.OID] {
			p.Final = append(p.Final, Stmt{Kind: "identity", Name: t.Schema + "." + t.Name + "." + col, Soft: true,
				SQL: "ALTER TABLE " + t.QName() + " ALTER COLUMN " + qident(col) + " SET GENERATED ALWAYS"})
		}
	}
	return nil
}

// loadConstraints puts primary keys, unique and check constraints before the
// snapshot (the stream needs keys to find rows) and foreign keys and
// exclusion constraints after it, when all referenced rows exist.
func (p *Plan) loadConstraints(ctx context.Context, src *pgx.Conn, oids []uint32) error {
	rows, err := src.Query(ctx, `SELECT c.contype::text, format('%I.%I', n.nspname, r.relname), c.conname, pg_catalog.pg_get_constraintdef(c.oid)
		FROM pg_catalog.pg_constraint c JOIN pg_catalog.pg_class r ON r.oid=c.conrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=r.relnamespace
		WHERE c.conrelid = ANY($1) AND c.contype IN ('p','u','c','f','x')
		ORDER BY array_position(ARRAY['p','u','c','x','f'], c.contype::text), 2, 3`, oids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var typ, table, name, def string
		if err := rows.Scan(&typ, &table, &name, &def); err != nil {
			return err
		}
		s := Stmt{Kind: "constraint", Name: table + " " + name,
			SQL: "ALTER TABLE ONLY " + table + " ADD CONSTRAINT " + qident(name) + " " + def}
		switch typ {
		case "p", "u":
			p.Pre = append(p.Pre, s)
		case "c":
			s.Soft = true
			p.Pre = append(p.Pre, s)
		case "f":
			s.Kind, s.Soft = kindForeignKey, true
			p.Post = append(p.Post, s)
		default:
			s.Soft = true
			p.Post = append(p.Post, s)
		}
	}
	return rows.Err()
}

func (p *Plan) loadIndexes(ctx context.Context, src *pgx.Conn, oids []uint32) error {
	return stmts(ctx, src, &p.Post, "index", true, `SELECT format('%I.%I', n.nspname, ic.relname), pg_catalog.pg_get_indexdef(i.indexrelid)
		FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid=i.indexrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=ic.relnamespace
		WHERE i.indrelid = ANY($1) AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint c WHERE c.conindid=i.indexrelid)
		ORDER BY 1`, oids)
}

func (p *Plan) loadViews(ctx context.Context, src *pgx.Conn) error {
	rows, err := src.Query(ctx, `SELECT c.relkind::text, format('%I.%I', n.nspname, c.relname), pg_catalog.pg_get_viewdef(c.oid)
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('v','m') AND n.nspname = ANY($1) AND `+notExtension("pg_class", "c.oid")+` ORDER BY 2`, p.Schemas)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name, def string
		if err := rows.Scan(&kind, &name, &def); err != nil {
			return err
		}
		def = strings.TrimSuffix(strings.TrimSpace(def), ";")
		if kind == "m" {
			p.Post = append(p.Post, Stmt{Kind: "materialized view", Name: name, Soft: true,
				SQL: "CREATE MATERIALIZED VIEW " + name + " AS " + def + " WITH NO DATA"})
			p.Final = append(p.Final, Stmt{Kind: "refresh", Name: name, Soft: true, SQL: "REFRESH MATERIALIZED VIEW " + name})
		} else {
			p.Post = append(p.Post, Stmt{Kind: "view", Name: name, Soft: true, SQL: "CREATE VIEW " + name + " AS " + def})
		}
	}
	return rows.Err()
}

// loadTriggers defers triggers to cutover. Created earlier, they would fire
// for every replicated row and repeat side effects the source already ran.
func (p *Plan) loadTriggers(ctx context.Context, src *pgx.Conn, oids []uint32) error {
	return stmts(ctx, src, &p.Final, "trigger", true, `SELECT format('%I on %I.%I', t.tgname, n.nspname, c.relname), pg_catalog.pg_get_triggerdef(t.oid)
		FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE NOT t.tgisinternal AND t.tgrelid = ANY($1) ORDER BY 1`, oids)
}
