package oraora

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	go_ora "github.com/sijms/go-ora/v2"

	"dedie/engine/connector/oracle"
)

// definition is one statement the source wrote about one of its objects.
type definition struct {
	// Verb is CREATE, ALTER or COMMENT. An object can come as more than
	// one statement: a trigger is created and then enabled or disabled.
	Verb string
	// Name is the object's own name, and Base the table or view it
	// belongs to when it is a constraint, an index, a trigger or a comment.
	Name, Base string
	Text       string
}

// Marks that separate the statements, and the fields of each, in the text
// the block below hands back. Neither occurs in DDL.
const (
	stmtMark  = "\x01"
	fieldMark = "\x02"
)

// What to ask DBMS_METADATA for. The names are its object types; each is
// selected either by the schema that owns the objects or, for objects
// that belong to a table, by the schema of that table.
const (
	bySchema = "SCHEMA"
	byBase   = "BASE_OBJECT_SCHEMA"
)

// transforms make a definition portable to another server and release:
// no storage, tablespace or other segment attributes, so the target's
// defaults apply; constraints and foreign keys left out of CREATE TABLE,
// because they are applied in later phases from their own statements; no
// SQL terminators.
var transforms = []string{"SQLTERMINATOR", "SEGMENT_ATTRIBUTES", "STORAGE", "TABLESPACE", "CONSTRAINTS", "REF_CONSTRAINTS"}

// fetchBlock builds the PL/SQL that reads every object of one type in
// one schema and returns their definitions as a single CLOB, made
// portable on the way (see transforms). When the objects go to a schema
// of another name, DBMS_METADATA rewrites the schema in every place it
// generates one.
//
// A transform parameter that does not apply to the object type, or that
// the source's release does not know, raises an error which is ignored:
// the list is the same for all types.
func fetchBlock(objectType, filter, owner, remapTo string) string {
	var b strings.Builder
	b.WriteString(`DECLARE
  h NUMBER; th NUMBER; ddls sys.ku$_ddls; buf CLOB; hdr VARCHAR2(4000);
  FUNCTION item(p sys.ku$_parsed_items, n VARCHAR2) RETURN VARCHAR2 IS
  BEGIN
    IF p IS NOT NULL THEN
      FOR i IN 1 .. p.COUNT LOOP
        IF p(i).item = n THEN RETURN p(i).value; END IF;
      END LOOP;
    END IF;
    RETURN NULL;
  END;
  PROCEDURE opt(n VARCHAR2, v BOOLEAN) IS
  BEGIN
    DBMS_METADATA.SET_TRANSFORM_PARAM(th, n, v);
  EXCEPTION WHEN OTHERS THEN NULL;
  END;
BEGIN
  DBMS_LOB.CREATETEMPORARY(buf, TRUE);
  h := DBMS_METADATA.OPEN(` + literal(objectType) + `);
  DBMS_METADATA.SET_FILTER(h, ` + literal(filter) + `, ` + literal(owner) + `);
  DBMS_METADATA.SET_PARSE_ITEM(h, 'VERB');
  DBMS_METADATA.SET_PARSE_ITEM(h, 'NAME');
  DBMS_METADATA.SET_PARSE_ITEM(h, 'BASE_OBJECT_NAME');
`)
	if remapTo != "" && remapTo != owner {
		b.WriteString(`  th := DBMS_METADATA.ADD_TRANSFORM(h, 'MODIFY');
  DBMS_METADATA.SET_REMAP_PARAM(th, 'REMAP_SCHEMA', ` + literal(owner) + `, ` + literal(remapTo) + `);
`)
	}
	b.WriteString(`  th := DBMS_METADATA.ADD_TRANSFORM(h, 'DDL');
`)
	for _, name := range transforms {
		b.WriteString("  opt('" + name + "', FALSE);\n")
	}
	b.WriteString(`  LOOP
    ddls := DBMS_METADATA.FETCH_DDL(h);
    EXIT WHEN ddls IS NULL;
    FOR i IN 1 .. ddls.COUNT LOOP
      hdr := CHR(1) || item(ddls(i).parsedItems, 'VERB') || CHR(2) || item(ddls(i).parsedItems, 'NAME') || CHR(2) ||
             item(ddls(i).parsedItems, 'BASE_OBJECT_NAME') || CHR(2);
      DBMS_LOB.WRITEAPPEND(buf, LENGTH(hdr), hdr);
      IF ddls(i).ddlText IS NOT NULL AND DBMS_LOB.GETLENGTH(ddls(i).ddlText) > 0 THEN
        DBMS_LOB.APPEND(buf, ddls(i).ddlText);
      END IF;
    END LOOP;
  END LOOP;
  DBMS_METADATA.CLOSE(h);
  :1 := buf;
END;`)
	return b.String()
}

// fetchDefinitions reads the definitions of every object of one type in
// one of the source's schemas.
func fetchDefinitions(ctx context.Context, src oracle.Queryer, objectType, filter, owner, remapTo string) ([]definition, error) {
	var out go_ora.Clob
	if _, err := src.ExecContext(ctx, fetchBlock(objectType, filter, owner, remapTo), go_ora.Out{Dest: &out}); err != nil {
		return nil, fmt.Errorf("read the definitions of %s objects in %s: %w", strings.ToLower(strings.ReplaceAll(objectType, "_", " ")), owner, definitionError(err))
	}
	return parseDefinitions(out.String), nil
}

// parseDefinitions takes apart what fetchBlock returned.
func parseDefinitions(text string) []definition {
	var defs []definition
	for _, part := range strings.Split(text, stmtMark) {
		fields := strings.SplitN(part, fieldMark, 4)
		if len(fields) != 4 {
			continue
		}
		defs = append(defs, definition{Verb: fields[0], Name: fields[1], Base: fields[2], Text: fields[3]})
	}
	return defs
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// tableCache remembers table definitions between the preflight and the
// runs of a job in one process. Reading a table's definition is by far
// the slowest question a job asks the source, seconds for a single table
// on some servers, and the answer cannot change without the table's
// LAST_DDL_TIME changing with it, which is part of the key along with
// the table's object number.
var tableCache = struct {
	sync.Mutex
	ddl map[string]string
}{ddl: map[string]string{}}

// tableCacheLimit bounds the cache; past it everything is forgotten.
const tableCacheLimit = 20000

// tableReader reads CREATE TABLE statements on one session of the
// source, which carries the transforms for as long as it is open.
type tableReader struct {
	conn *sql.Conn
	// origin tells one source database from another in the cache.
	origin string
}

func newTableReader(ctx context.Context, src *sql.DB, origin string) (*tableReader, error) {
	conn, err := src.Conn(ctx)
	if err != nil {
		return nil, oracle.Friendly(err)
	}
	// As in fetchBlock, a parameter this release does not know is skipped.
	var b strings.Builder
	b.WriteString("BEGIN\n")
	for _, name := range transforms {
		b.WriteString("  BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, '" + name + "', FALSE); EXCEPTION WHEN OTHERS THEN NULL; END;\n")
	}
	b.WriteString("END;")
	if _, err := conn.ExecContext(ctx, b.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("prepare to read table definitions: %w", oracle.Friendly(err))
	}
	return &tableReader{conn: conn, origin: origin}, nil
}

// close puts the session's transforms back before it returns to the pool.
func (r *tableReader) close(ctx context.Context) {
	r.conn.ExecContext(context.WithoutCancel(ctx), `BEGIN DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM, 'DEFAULT'); END;`)
	r.conn.Close()
}

// read returns the CREATE TABLE statement of one table, named as the
// source names it. stamp identifies the table and its last change.
func (r *tableReader) read(ctx context.Context, owner, name, stamp string) (string, error) {
	key := strings.Join([]string{r.origin, owner, name, stamp}, "\x00")
	tableCache.Lock()
	text, ok := tableCache.ddl[key]
	tableCache.Unlock()
	if ok {
		return text, nil
	}
	if err := r.conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL('TABLE', :1, :2) FROM dual`, name, owner).Scan(&text); err != nil {
		return "", fmt.Errorf("read the definition of table %s.%s: %w", owner, name, definitionError(err))
	}
	tableCache.Lock()
	if len(tableCache.ddl) >= tableCacheLimit {
		tableCache.ddl = map[string]string{}
	}
	tableCache.ddl[key] = text
	tableCache.Unlock()
	return text, nil
}

// definitionError adds, to the error DBMS_METADATA gives for an object it
// may not show, what the account is missing.
func definitionError(err error) error {
	err = oracle.Friendly(err)
	if strings.Contains(err.Error(), "ORA-31603") {
		return fmt.Errorf("%w. To read the definitions of objects in another schema the source account needs SELECT_CATALOG_ROLE", err)
	}
	return err
}
