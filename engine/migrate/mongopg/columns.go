package mongopg

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"dedie/engine/connector"
	"dedie/engine/connector/mongodb"
	"dedie/engine/migrate"
)

// Types a document field can be lifted into.
var columnTypes = map[string]bool{"text": true, "bigint": true, "double precision": true, "numeric": true,
	"boolean": true, "timestamptz": true, "jsonb": true}

// column is a validated ColumnSpec.
type column struct {
	path []string
	name string
	typ  string
}

// columnsOf validates the columns asked for a collection.
func columnsOf(collection string, specs []migrate.ColumnSpec) ([]column, error) {
	var out []column
	seen := map[string]bool{"_id": true, "doc": true}
	for _, sp := range specs {
		field := strings.TrimSpace(sp.Field)
		if field == "" || strings.Contains(field, "..") || strings.HasPrefix(field, ".") || strings.HasSuffix(field, ".") {
			return nil, fmt.Errorf("%s: %q is not a field path", collection, sp.Field)
		}
		name := strings.TrimSpace(sp.Name)
		if name == "" {
			name = strings.ReplaceAll(field, ".", "_")
		}
		typ := strings.ToLower(strings.Join(strings.Fields(sp.Type), " "))
		switch {
		case !columnTypes[typ]:
			return nil, fmt.Errorf("%s.%s: type %q is not one of text, bigint, double precision, numeric, boolean, timestamptz, jsonb", collection, field, sp.Type)
		case len(name) > 63 || strings.ContainsRune(name, 0):
			return nil, fmt.Errorf("%s.%s: the column name is not valid", collection, field)
		case seen[strings.ToLower(name)]:
			return nil, fmt.Errorf("%s: column %s is used twice or is reserved (_id and doc are)", collection, name)
		}
		seen[strings.ToLower(name)] = true
		out = append(out, column{path: strings.Split(field, "."), name: name, typ: typ})
	}
	return out, nil
}

// allColumns validates the configuration for every collection.
func allColumns(cfg migrate.Config) (map[string][]column, error) {
	out := map[string][]column{}
	for coll, specs := range cfg.Columns {
		cols, err := columnsOf(coll, specs)
		if err != nil {
			return nil, err
		}
		out[coll] = cols
	}
	return out, nil
}

// value converts a field to what its column stores. A field that is
// missing, null or of a type the column cannot hold exactly becomes NULL;
// ok reports whether a present value had to be dropped that way. The
// document itself always keeps the original.
func (c column) value(doc bson.Raw) (v any, ok bool) {
	raw, err := doc.LookupErr(c.path...)
	if err != nil || raw.Type == bson.TypeNull || raw.Type == bson.TypeUndefined {
		return nil, true
	}
	switch c.typ {
	case "text":
		if t := mongodb.Text(raw); t != nil {
			return strings.ReplaceAll(*t, "\x00", ""), true
		}
	case "jsonb":
		return strings.ReplaceAll(mongodb.JSON(raw), `\u0000`, ""), true
	case "bigint":
		switch raw.Type {
		case bson.TypeInt32:
			return int64(raw.Int32()), true
		case bson.TypeInt64:
			return raw.Int64(), true
		case bson.TypeDouble:
			// Only a whole number is the same number as a bigint.
			if f := raw.Double(); f == float64(int64(f)) && f > -9e18 && f < 9e18 {
				return int64(f), true
			}
		}
	case "double precision":
		switch raw.Type {
		case bson.TypeDouble:
			return raw.Double(), true
		case bson.TypeInt32:
			return float64(raw.Int32()), true
		case bson.TypeInt64:
			return float64(raw.Int64()), true
		}
	case "numeric":
		switch raw.Type {
		case bson.TypeInt32:
			return strconv.FormatInt(int64(raw.Int32()), 10), true
		case bson.TypeInt64:
			return strconv.FormatInt(raw.Int64(), 10), true
		case bson.TypeDouble:
			return strconv.FormatFloat(raw.Double(), 'f', -1, 64), true
		case bson.TypeDecimal128:
			if s := raw.Decimal128().String(); s != "NaN" && !strings.Contains(s, "Infinity") {
				return s, true
			}
		}
	case "boolean":
		if raw.Type == bson.TypeBoolean {
			return raw.Boolean(), true
		}
	case "timestamptz":
		if raw.Type == bson.TypeDateTime {
			return time.UnixMilli(raw.DateTime()).UTC(), true
		}
	}
	return nil, false
}

// cast is the cast applied to a parameter of this column's type. Values
// are sent as text for the two types whose Go form is a string.
func (c column) cast() string {
	if c.typ == "numeric" || c.typ == "jsonb" {
		return "::" + c.typ
	}
	return ""
}

func quoteColumn(name string) string { return pgx.Identifier{name}.Sanitize() }

// createTable is the statement that creates a collection's table: the key,
// the whole document, and a column for each lifted field.
func createTable(table string, cols []column) string {
	ddl := `CREATE TABLE IF NOT EXISTS ` + qname(table) + ` (_id text PRIMARY KEY, doc jsonb NOT NULL`
	for _, c := range cols {
		ddl += ", " + quoteColumn(c.name) + " " + c.typ
	}
	return ddl + ")"
}

// tableDDL creates a collection's table. Columns added to the configuration
// after the table exists are added to it.
func tableDDL(table string, cols []column) string {
	ddl := createTable(table, cols)
	for _, c := range cols {
		ddl += `; ALTER TABLE ` + qname(table) + ` ADD COLUMN IF NOT EXISTS ` + quoteColumn(c.name) + " " + c.typ
	}
	return ddl
}

// upsertSQL stores a document and its lifted fields, replacing the row of
// the same key.
func upsertSQL(table string, cols []column) string {
	names, params, sets := `_id, doc`, `$1, $2`, `doc = excluded.doc`
	for i, c := range cols {
		q := quoteColumn(c.name)
		names += ", " + q
		params += fmt.Sprintf(", $%d%s", i+3, c.cast())
		sets += ", " + q + " = excluded." + q
	}
	return `INSERT INTO ` + qname(table) + ` (` + names + `) VALUES (` + params + `) ON CONFLICT (_id) DO UPDATE SET ` + sets
}

// scalarType is the column type a BSON type maps to exactly, or "" when
// it has none (objects and arrays stay in the document).
func scalarType(t bson.Type) string {
	switch t {
	case bson.TypeString, bson.TypeObjectID:
		return "text"
	case bson.TypeInt32, bson.TypeInt64:
		return "bigint"
	case bson.TypeDouble:
		return "double precision"
	case bson.TypeDecimal128:
		return "numeric"
	case bson.TypeBoolean:
		return "boolean"
	case bson.TypeDateTime:
		return "timestamptz"
	}
	return ""
}

// Suggest samples each collection in scope and proposes columns for the
// top-level fields whose values share one simple type in at least nine
// documents out of ten that have the field. The proposal is a starting
// point for a person to review, not a decision.
func Suggest(ctx context.Context, srcCfg connector.Config, cfg migrate.Config) (map[string][]migrate.ColumnSpec, error) {
	client, err := mongodb.Connect(ctx, srcCfg)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(context.WithoutCancel(ctx))
	db := client.Database(srcCfg.Database)
	list, err := collections(ctx, db, newScope(cfg))
	if err != nil {
		return nil, err
	}
	out := map[string][]migrate.ColumnSpec{}
	for _, c := range list {
		specs, err := suggestFor(ctx, db.Collection(c.Name))
		if err != nil {
			return nil, err
		}
		if len(specs) > 0 {
			out[c.Name] = specs
		}
	}
	return out, nil
}

func suggestFor(ctx context.Context, coll *mongo.Collection) ([]migrate.ColumnSpec, error) {
	cur, err := coll.Find(ctx, bson.D{}, options.Find().SetLimit(500))
	if err != nil {
		return nil, mongodb.Friendly(err)
	}
	defer cur.Close(context.WithoutCancel(ctx))
	type tally struct {
		present int
		types   map[string]int
		order   int
	}
	fields := map[string]*tally{}
	for cur.Next(ctx) {
		elems, _ := cur.Current.Elements()
		for _, e := range elems {
			key := e.Key()
			if key == "_id" {
				continue
			}
			t := fields[key]
			if t == nil {
				t = &tally{types: map[string]int{}, order: len(fields)}
				fields[key] = t
			}
			v := e.Value()
			if v.Type == bson.TypeNull {
				continue
			}
			t.present++
			t.types[scalarType(v.Type)]++
		}
	}
	if err := cur.Err(); err != nil {
		return nil, mongodb.Friendly(err)
	}
	var names []string
	for name := range fields {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return fields[names[i]].order < fields[names[j]].order })
	var out []migrate.ColumnSpec
	for _, name := range names {
		t := fields[name]
		for typ, n := range t.types {
			// Whole numbers and fractions in one field fit a double.
			if typ == "bigint" && t.types["double precision"] > 0 {
				continue
			}
			if typ == "double precision" {
				n += t.types["bigint"]
			}
			if typ != "" && t.present > 0 && n*10 >= t.present*9 && len(name) <= 63 {
				out = append(out, migrate.ColumnSpec{Field: name, Type: typ})
			}
		}
	}
	return out, nil
}
