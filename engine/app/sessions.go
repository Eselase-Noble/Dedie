package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"dedie/engine/connector"
)

// Opening a connection to a database is the slow part of most requests:
// a TCP handshake, usually a TLS handshake, and an authentication
// exchange, each a round trip or several to a server that may be far
// away. A page of the interface makes many requests, so sessions are not
// closed when a request is done with them but kept for the next one.
//
// A kept session must be indistinguishable from a new one. One that only
// read is. One that ran statements a person typed may have had its role,
// search path or settings changed, or been left inside a transaction; it
// is reset where the database can do that completely and closed where it
// cannot. Sessions are kept per connection and per set of connection
// details, so a changed password or host is never served by an old one.
const (
	// sessionIdleMax is how long an unused session is kept. It is short
	// on purpose: the point is to serve the requests of one burst of
	// work, not to hold connections on someone's server.
	sessionIdleMax = 90 * time.Second
	// sessionsKept bounds the idle sessions per connection.
	sessionsKept = 3
	// sessionTrusted is how recently a session must have been used to be
	// handed out without first asking the server whether it is still
	// there.
	sessionTrusted = 5 * time.Second
)

type idleSession struct {
	sess  connector.Session
	since time.Time
}

// sessionPool keeps idle sessions by connection.
type sessionPool struct {
	mu      sync.Mutex
	idle    map[string][]idleSession
	closed  bool
	sweeper bool
}

// sessionKey names the sessions that may stand in for one another: same
// saved connection, same details.
func sessionKey(connID string, cfg connector.Config) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%#v", cfg)))
	return connID + ":" + hex.EncodeToString(sum[:])
}

// take hands out an idle session for the key, or nil.
func (p *sessionPool) take(ctx context.Context, key string) connector.Session {
	for {
		p.mu.Lock()
		list := p.idle[key]
		if len(list) == 0 {
			p.mu.Unlock()
			return nil
		}
		// The most recently used first: it is the likeliest to be alive,
		// and the others then age out.
		got := list[len(list)-1]
		p.idle[key] = list[:len(list)-1]
		p.mu.Unlock()

		age := time.Since(got.since)
		if age < sessionIdleMax && (age < sessionTrusted || got.sess.(connector.Reusable).Alive(ctx)) {
			return got.sess
		}
		got.sess.Close(context.WithoutCancel(ctx))
		if ctx.Err() != nil {
			return nil
		}
	}
}

// put keeps a session for later, or reports false when it has no room.
func (p *sessionPool) put(key string, sess connector.Session) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || len(p.idle[key]) >= sessionsKept {
		return false
	}
	if p.idle == nil {
		p.idle = map[string][]idleSession{}
	}
	p.idle[key] = append(p.idle[key], idleSession{sess, time.Now()})
	if !p.sweeper {
		p.sweeper = true
		go p.sweep()
	}
	return true
}

// sweep closes sessions nobody came back for, and stops when none are left.
func (p *sessionPool) sweep() {
	for {
		time.Sleep(sessionIdleMax / 3)
		var stale []connector.Session
		p.mu.Lock()
		for key, list := range p.idle {
			kept := list[:0]
			for _, s := range list {
				if time.Since(s.since) >= sessionIdleMax {
					stale = append(stale, s.sess)
				} else {
					kept = append(kept, s)
				}
			}
			if len(kept) == 0 {
				delete(p.idle, key)
			} else {
				p.idle[key] = kept
			}
		}
		done := len(p.idle) == 0
		if done {
			p.sweeper = false
		}
		p.mu.Unlock()
		for _, s := range stale {
			s.Close(context.Background())
		}
		if done {
			return
		}
	}
}

// drop closes the idle sessions of one saved connection, whatever their
// details: it was edited or deleted.
func (p *sessionPool) drop(connID string) {
	var gone []connector.Session
	p.mu.Lock()
	for key, list := range p.idle {
		if len(key) > len(connID) && key[:len(connID)+1] == connID+":" {
			for _, s := range list {
				gone = append(gone, s.sess)
			}
			delete(p.idle, key)
		}
	}
	p.mu.Unlock()
	for _, s := range gone {
		s.Close(context.Background())
	}
}

// close closes every idle session and keeps no more.
func (p *sessionPool) close() {
	var gone []connector.Session
	p.mu.Lock()
	p.closed = true
	for _, list := range p.idle {
		for _, s := range list {
			gone = append(gone, s.sess)
		}
	}
	p.idle = nil
	p.mu.Unlock()
	for _, s := range gone {
		s.Close(context.Background())
	}
}

// kept is a session on loan from the pool. Closing it gives it back.
type kept struct {
	connector.Session
	pool *sessionPool
	key  string
	// changed is set once a statement was run that may have altered the
	// session itself, and broken once a call ended in a way that leaves
	// the connection's state unknown.
	changed, broken bool
	once            sync.Once
}

func (k *kept) Query(ctx context.Context, text string, opts connector.QueryOptions) ([]connector.ResultSet, error) {
	if !opts.ReadOnly {
		k.changed = true
	}
	res, err := k.Session.Query(ctx, text, opts)
	if err != nil && ctx.Err() != nil {
		// Cancelled or timed out halfway through.
		k.broken = true
	}
	return res, err
}

func (k *kept) Close(ctx context.Context) error {
	var err error
	k.once.Do(func() {
		ctx = context.WithoutCancel(ctx)
		reusable := k.Session.(connector.Reusable)
		if !k.broken && (!k.changed || reusable.Reset(ctx)) && k.pool.put(k.key, k.Session) {
			return
		}
		err = k.Session.Close(ctx)
	})
	return err
}

// session returns a session for a saved connection: one kept from an
// earlier request when there is one, a new one otherwise.
func (a *App) session(ctx context.Context, connID string, cfg connector.Config, drv connector.Driver) (connector.Session, error) {
	key := sessionKey(connID, cfg)
	if sess := a.sessions.take(ctx, key); sess != nil {
		return &kept{Session: sess, pool: &a.sessions, key: key}, nil
	}
	sess, err := drv.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, ok := sess.(connector.Reusable); !ok {
		return sess, nil
	}
	return &kept{Session: sess, pool: &a.sessions, key: key}, nil
}
