package mongopg

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"dedie/engine/connector/mongodb"
	"dedie/engine/migrate"
)

// digest is a count of keys and a checksum that does not depend on the
// order they are read in: the two databases sort keys differently.
type digest struct {
	count int64
	sum   [sha256.Size]byte
}

func (d *digest) add(key string) {
	d.count++
	h := sha256.Sum256([]byte(key))
	for i := range d.sum {
		d.sum[i] ^= h[i]
	}
}

// validate compares each collection with its table: the number of
// documents, and unless mode is "count" also the set of keys. It returns
// how many differ. The documents' contents are not compared.
func validate(ctx context.Context, db *mongo.Database, dst *pgx.Conn, list []Collection, mode string, rep migrate.Reporter) (int, error) {
	if mode == "none" {
		return 0, nil
	}
	mismatches := 0
	for _, c := range list {
		var src, tgt digest
		if mode == "count" {
			n, err := db.Collection(c.Name).CountDocuments(ctx, bson.D{})
			if err != nil {
				return 0, mongodb.Friendly(err)
			}
			src.count = n
			if err := dst.QueryRow(ctx, `SELECT count(*) FROM `+qname(c.Name)).Scan(&tgt.count); err != nil {
				return 0, err
			}
		} else {
			cur, err := db.Collection(c.Name).Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
			if err != nil {
				return 0, mongodb.Friendly(err)
			}
			for cur.Next(ctx) {
				if key, err := cur.Current.LookupErr("_id"); err == nil {
					src.add(keyText(key))
				}
			}
			err = cur.Err()
			cur.Close(context.WithoutCancel(ctx))
			if err != nil {
				return 0, mongodb.Friendly(err)
			}
			rows, err := dst.Query(ctx, `SELECT _id FROM `+qname(c.Name))
			if err != nil {
				return 0, err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return 0, err
				}
				tgt.add(id)
			}
			if err := rows.Err(); err != nil {
				return 0, err
			}
		}
		result := "match"
		if mode == "count" {
			result = "count match"
		}
		if src != tgt {
			result = "mismatch"
			mismatches++
			rep.Event("error", fmt.Sprintf("%s differs: %d documents in the source, %d rows in the target", c.Name, src.count, tgt.count))
		}
		rep.UpdateTable(targetSchema, c.Name, map[string]any{"src_count": src.count, "dst_count": tgt.count, "validation": result})
	}
	return mismatches, nil
}
