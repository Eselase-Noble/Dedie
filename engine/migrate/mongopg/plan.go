package mongopg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"dedie/engine/connector/mongodb"
	"dedie/engine/migrate"
)

// Collection is one collection to migrate.
type Collection struct {
	Name    string
	EstDocs int64
}

// scope decides which collections a job covers.
type scope struct {
	only    map[string]bool
	exclude map[string]bool
}

func newScope(cfg migrate.Config) scope {
	// A collection may be written "database.collection", the way the
	// schema tree shows it; only the collection part matters here.
	set := func(names []string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			if _, coll, ok := strings.Cut(n, "."); ok {
				n = coll
			}
			m[n] = true
		}
		return m
	}
	return scope{only: set(cfg.Tables), exclude: set(cfg.ExcludeTables)}
}

func (s scope) includes(name string) bool {
	if strings.HasPrefix(name, "system.") || s.exclude[name] {
		return false
	}
	return len(s.only) == 0 || s.only[name]
}

// collections lists the collections in scope, leaving out views, which
// hold no documents of their own.
func collections(ctx context.Context, db *mongo.Database, sc scope) ([]Collection, error) {
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return nil, mongodb.Friendly(err)
	}
	var out []Collection
	found := map[string]bool{}
	for _, spec := range specs {
		if spec.Type == "view" || !sc.includes(spec.Name) {
			continue
		}
		n, _ := db.Collection(spec.Name).EstimatedDocumentCount(ctx)
		out = append(out, Collection{Name: spec.Name, EstDocs: n})
		found[spec.Name] = true
	}
	for name := range sc.only {
		if !found[name] {
			return nil, fmt.Errorf("collection %s does not exist in the source database", name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// checkNames rejects collection names PostgreSQL cannot hold as table names.
func checkNames(list []Collection) error {
	for _, c := range list {
		if len(c.Name) > 63 {
			return fmt.Errorf("collection name %q is longer than the 63 bytes PostgreSQL allows for a table name", c.Name)
		}
		if strings.ContainsRune(c.Name, 0) {
			return errors.New("a collection name contains a null character")
		}
	}
	return nil
}

// row turns a document into the values stored for it: its key as text, the
// whole document as JSON, and one value per lifted column. clean reports
// whether the JSON had to be altered to be storable, and dropped how many
// field values did not fit their column.
func row(doc bson.Raw, cols []column) (values []any, clean bool, dropped int, err error) {
	key, err := doc.LookupErr("_id")
	if err != nil {
		return nil, false, 0, errors.New("a document has no _id")
	}
	js, err := mongodb.DocumentJSON(doc)
	if err != nil {
		return nil, false, 0, err
	}
	// jsonb cannot hold the null character; MongoDB strings can.
	clean = true
	if strings.Contains(js, `\u0000`) {
		js, clean = strings.ReplaceAll(js, `\u0000`, ""), false
	}
	values = make([]any, 0, 2+len(cols))
	values = append(values, keyText(key), js)
	for _, c := range cols {
		v, ok := c.value(doc)
		if !ok {
			dropped++
		}
		values = append(values, v)
	}
	return values, clean, dropped, nil
}

// keyText is the text form of a document key.
func keyText(key bson.RawValue) string {
	if t := mongodb.Text(key); t != nil {
		return strings.ReplaceAll(*t, "\x00", "")
	}
	return "null"
}
