package app

import (
	"context"
	"errors"
	"testing"

	"dedie/engine/connector"
)

// fakeSession counts what happens to it. Only the calls the pool makes
// are implemented; any other would be a nil dereference, which is the
// point.
type fakeSession struct {
	connector.Session
	id               int
	alive, resets    bool
	closed, wasReset int
}

func (f *fakeSession) Query(context.Context, string, connector.QueryOptions) ([]connector.ResultSet, error) {
	return nil, nil
}
func (f *fakeSession) Close(context.Context) error { f.closed++; return nil }
func (f *fakeSession) Alive(context.Context) bool  { return f.alive }
func (f *fakeSession) Reset(context.Context) bool  { f.wasReset++; return f.resets }

type fakeDriver struct {
	opened []*fakeSession
	resets bool
	fail   error
}

func (d *fakeDriver) Kind() string     { return "fake" }
func (d *fakeDriver) DefaultPort() int { return 1 }
func (d *fakeDriver) Open(context.Context, connector.Config) (connector.Session, error) {
	if d.fail != nil {
		return nil, d.fail
	}
	s := &fakeSession{id: len(d.opened), alive: true, resets: d.resets}
	d.opened = append(d.opened, s)
	return s, nil
}

func TestSessionsAreKept(t *testing.T) {
	ctx := context.Background()
	a := &App{}
	drv := &fakeDriver{resets: true}
	cfg := connector.Config{Host: "db", Username: "u", Password: "one"}
	open := func(cfg connector.Config) connector.Session {
		t.Helper()
		s, err := a.session(ctx, "con_1", cfg, drv)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A session that only read is used again as it is.
	s := open(cfg)
	s.Query(ctx, "SELECT 1", connector.QueryOptions{ReadOnly: true})
	s.Close(ctx)
	s.Close(ctx) // closing twice must not keep it twice
	s = open(cfg)
	if len(drv.opened) != 1 || drv.opened[0].wasReset != 0 {
		t.Fatalf("opened %d session(s), reset %d time(s); want the first one again, untouched", len(drv.opened), drv.opened[0].wasReset)
	}
	// One that ran a statement of the user's is reset first.
	s.Query(ctx, "SET ROLE other", connector.QueryOptions{})
	s.Close(ctx)
	if drv.opened[0].wasReset != 1 || drv.opened[0].closed != 0 {
		t.Errorf("after a statement that may change the session: reset %d, closed %d", drv.opened[0].wasReset, drv.opened[0].closed)
	}

	// Two at once need two; both are kept afterwards.
	one, two := open(cfg), open(cfg)
	if len(drv.opened) != 2 {
		t.Fatalf("two requests at once share %d session(s)", len(drv.opened))
	}
	one.Close(ctx)
	two.Close(ctx)

	// Other connection details never get a session opened with these.
	other := cfg
	other.Password = "two"
	s = open(other)
	if len(drv.opened) != 3 {
		t.Error("a session opened with one password served a connection that has another")
	}
	s.Close(ctx)

	// A session the server has dropped is not handed out.
	a.sessions.mu.Lock()
	for key, list := range a.sessions.idle {
		for i := range list {
			list[i].since = list[i].since.Add(-2 * sessionTrusted)
			list[i].sess.(*fakeSession).alive = false
		}
		a.sessions.idle[key] = list
	}
	a.sessions.mu.Unlock()
	s = open(cfg)
	if len(drv.opened) != 4 || drv.opened[0].closed+drv.opened[1].closed != 2 {
		t.Errorf("dead sessions: %d opened in all, %d closed", len(drv.opened), drv.opened[0].closed+drv.opened[1].closed)
	}
	s.Close(ctx)

	// Editing or deleting the connection closes what was kept for it,
	// and so does shutting down.
	a.sessions.drop("con_1")
	if drv.opened[2].closed != 1 || drv.opened[3].closed != 1 {
		t.Error("sessions of an edited connection were kept")
	}
	s = open(cfg)
	a.sessions.close()
	s.Close(ctx)
	if last := drv.opened[len(drv.opened)-1]; last.closed != 1 {
		t.Error("a session given back after shutdown was kept")
	}
}

func TestSessionThatCannotBeResetIsClosed(t *testing.T) {
	ctx := context.Background()
	a := &App{}
	drv := &fakeDriver{resets: false}
	s, err := a.session(ctx, "con_1", connector.Config{}, drv)
	if err != nil {
		t.Fatal(err)
	}
	s.Query(ctx, "USE other", connector.QueryOptions{})
	s.Close(ctx)
	if drv.opened[0].closed != 1 {
		t.Error("a session whose state could not be restored was kept")
	}
	// A cancelled statement leaves the connection in an unknown state.
	s, _ = a.session(ctx, "con_1", connector.Config{}, drv)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	drv.opened[1].Session = nil
	k := s.(*kept)
	k.Session = failing{drv.opened[1]}
	s.Query(cancelled, "SELECT 1", connector.QueryOptions{ReadOnly: true})
	s.Close(ctx)
	if drv.opened[1].closed != 1 {
		t.Error("a session whose statement was cancelled was kept")
	}
	drv.fail = errors.New("refused")
	if _, err := a.session(ctx, "con_1", connector.Config{}, drv); err == nil {
		t.Error("a failed connection was not reported")
	}
}

// failing is a session whose statements end in an error.
type failing struct{ *fakeSession }

func (failing) Query(ctx context.Context, _ string, _ connector.QueryOptions) ([]connector.ResultSet, error) {
	return nil, ctx.Err()
}
