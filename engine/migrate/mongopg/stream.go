package mongopg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"dedie/engine/connector/mongodb"
)

// Reasons the stream stops without an error.
const (
	stopPause   = "pause"
	stopAbort   = "abort"
	stopCutover = "cutover"
)

// batchSize is how many changes are applied in one target transaction.
const batchSize = 500

// change is one event of the stream, reduced to what is applied.
type change struct {
	Op string `bson:"operationType"`
	NS struct {
		Coll string `bson:"coll"`
	} `bson:"ns"`
	Key         bson.Raw       `bson:"documentKey"`
	Full        bson.Raw       `bson:"fullDocument"`
	ClusterTime bson.Timestamp `bson:"clusterTime"`
}

// stream applies the source's changes to the target until the operator
// pauses, aborts or cuts over. It returns why it stopped.
func (r *runner) stream(ctx context.Context) (string, error) {
	if _, err := r.dst.Exec(ctx, checkpointDDL); err != nil {
		return "", err
	}
	// The checkpoint on the target is the truth: it was committed together
	// with the changes it covers.
	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup).SetMaxAwaitTime(time.Second).SetBatchSize(batchSize)
	var token []byte
	err := r.dst.QueryRow(ctx, `SELECT resume_token FROM `+metaSchema+`.mongo_checkpoint WHERE job_id=$1`, r.jobID).Scan(&token)
	switch {
	case err == nil:
		opts.SetResumeAfter(bson.Raw(token))
	case errors.Is(err, pgx.ErrNoRows):
		opts.SetStartAtOperationTime(&bson.Timestamp{T: r.st.StartT, I: r.st.StartI})
	default:
		return "", err
	}
	cs, err := r.db.Watch(ctx, mongo.Pipeline{}, opts)
	if err != nil {
		return "", fmt.Errorf("open the change stream: %w", mongodb.Friendly(err))
	}
	defer cs.Close(context.WithoutCancel(ctx))

	// A collection created while the job was paused is in the list by
	// now but has no table yet; its documents arrive through the stream.
	known := map[string]bool{}
	for _, c := range r.list {
		if _, err := r.dst.Exec(ctx, tableDDL(c.Name)); err != nil {
			return "", err
		}
		known[c.Name] = true
	}
	var pending []change
	flush := func() error {
		if err := r.apply(ctx, pending, cs.ResumeToken(), known); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	draining, idle := false, 0
	lastTick := time.Now()
	for {
		if cs.TryNext(ctx) {
			var ev change
			if err := bson.Unmarshal(cs.Current, &ev); err != nil {
				return "", err
			}
			// The decoded event points into the stream's buffer, which
			// the next read overwrites.
			ev.Key, ev.Full = append(bson.Raw(nil), ev.Key...), append(bson.Raw(nil), ev.Full...)
			pending = append(pending, ev)
			idle = 0
			if len(pending) >= batchSize {
				if err := flush(); err != nil {
					return "", err
				}
			}
		} else {
			if err := cs.Err(); err != nil {
				return "", mongodb.Friendly(err)
			}
			if cs.ID() == 0 {
				return "", errors.New("the change stream was closed by the server: the database was dropped or renamed")
			}
			// Nothing waiting: commit what there is, which also moves
			// the checkpoint past changes outside this job's scope.
			if err := flush(); err != nil {
				return "", err
			}
			idle++
			r.st.LagSeconds = 0
		}

		if time.Since(lastTick) >= time.Second {
			lastTick = time.Now()
			r.save()
			switch r.rep.Desired() {
			case "pause":
				if err := flush(); err != nil {
					return "", err
				}
				return stopPause, nil
			case "abort":
				return stopAbort, nil
			case "cutover":
				if !draining {
					draining, idle = true, 0
					r.rep.Event("info", "cutover requested: applying the remaining changes")
				}
			}
		}
		// Two empty reads in a row, each of which waited a second at the
		// server, mean the source has nothing more to send.
		if draining && idle >= 2 {
			return stopCutover, nil
		}
	}
}

// apply writes a batch of changes and the stream position after them in
// one transaction.
func (r *runner) apply(ctx context.Context, batch []change, token bson.Raw, known map[string]bool) error {
	if len(token) == 0 {
		return nil
	}
	tx, err := r.dst.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	b := &pgx.Batch{}
	var rows int64
	var last bson.Timestamp
	for _, ev := range batch {
		name := ev.NS.Coll
		last = ev.ClusterTime
		switch ev.Op {
		case "insert", "update", "replace", "delete":
		case "drop", "rename", "dropDatabase":
			r.anomaly(fmt.Sprintf("the source ran %s on %s; the target table was left as it is", ev.Op, name))
			continue
		default:
			continue // index and other definition changes are not translated
		}
		if !r.scope.includes(name) {
			continue
		}
		if !known[name] {
			// A collection created after the job started.
			if len(name) > 63 {
				r.anomaly("skipped changes to " + name + ": the name is too long for a PostgreSQL table")
				continue
			}
			// Created at once rather than queued: the statements queued
			// after it are prepared against the table before any runs.
			if _, err := tx.Exec(ctx, tableDDL(name)); err != nil {
				return err
			}
			known[name] = true
			r.rep.Event("info", "new collection "+name+": created its table")
		}
		if ev.Op == "delete" {
			key, err := ev.Key.LookupErr("_id")
			if err != nil {
				continue
			}
			b.Queue(`DELETE FROM `+qname(name)+` WHERE _id = $1`, keyText(key))
			rows++
			continue
		}
		if len(ev.Full) == 0 {
			// The document was deleted before the server looked it up
			// for this update; its delete event follows.
			continue
		}
		id, js, clean, err := row(ev.Full)
		if err != nil {
			r.anomaly("skipped a change in " + name + ": " + err.Error())
			continue
		}
		if !clean {
			r.anomaly("removed null characters from a document in " + name + " (key " + id + "), which jsonb cannot store")
		}
		b.Queue(`INSERT INTO `+qname(name)+` (_id, doc) VALUES ($1, $2) ON CONFLICT (_id) DO UPDATE SET doc = excluded.doc`, id, js)
		rows++
	}
	b.Queue(checkpointUpsert, r.jobID, []byte(token))
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if len(batch) > 0 {
		r.st.TxApplied++
		r.st.RowsApplied += rows
		r.st.LastCommitTime = time.Now().UTC()
		r.st.AppliedLSN = time.Unix(int64(last.T), 0).UTC().Format(time.RFC3339)
		if lag := time.Now().Unix() - int64(last.T); lag > 0 {
			r.st.LagSeconds = lag
		} else {
			r.st.LagSeconds = 0
		}
	}
	return nil
}

func (r *runner) anomaly(msg string) {
	r.st.Anomalies++
	if r.st.Anomalies <= 20 {
		r.rep.Event("warn", msg)
	}
}
