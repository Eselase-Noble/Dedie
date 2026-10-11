package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"dedie/engine/app"
)

const (
	sessionCookie = "dedie_session"
	// csrfHeader must accompany every state-changing request. Browsers do
	// not let another site add a custom header without a CORS preflight,
	// which this server never approves.
	csrfHeader   = "X-Dedie-Request"
	maxBodyBytes = 2 << 20
)

// guard applies the protections every request gets: security headers, a body
// size limit and cross-site request checks.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if s.opts.Secure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				// A request that carries an API token does not use the
				// session cookie, so there is nothing for another site to
				// ride on and the cross-site checks do not apply.
				if bearerToken(r) == "" && (r.Header.Get(csrfHeader) == "" || !sameOrigin(r)) {
					writeError(w, http.StatusForbidden, "cross-site request rejected")
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects requests whose Origin header names another host.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) clientIP(r *http.Request) string {
	if s.opts.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			return strings.TrimSpace(strings.Split(fwd, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// bearerToken returns the API token of a request, if it carries one.
func bearerToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	return ""
}

type handler func(w http.ResponseWriter, r *http.Request, actor app.Actor)

// authed resolves the API token or session cookie to an actor before
// calling h. A request with a token is judged by the token alone.
func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var actor app.Actor
		var err error
		if token := bearerToken(r); token != "" {
			actor, err = s.app.AuthenticateToken(r.Context(), token, s.clientIP(r))
		} else {
			actor, err = s.app.Authenticate(r.Context(), sessionToken(r), s.clientIP(r))
		}
		if err != nil {
			fail(w, err)
			return
		}
		// Until two-factor sign-in is set up, only the account endpoints
		// that set it up are reachable.
		if actor.MustEnroll && !strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			fail(w, &app.Error{Status: http.StatusForbidden, Code: "totp_enrollment_required",
				Message: "your organization requires two-factor sign-in; set it up on the Account page first"})
			return
		}
		h(w, r, actor)
	}
}

func (s *Server) setSession(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.opts.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(ttl.Seconds()),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail maps an application error to a response. Unexpected errors are logged
// and reported without detail so internals do not leak to clients.
func fail(w http.ResponseWriter, err error) {
	var appErr *app.Error
	if errors.As(err, &appErr) {
		body := map[string]string{"error": appErr.Message}
		if appErr.Code != "" {
			body["code"] = appErr.Code
		}
		writeJSON(w, appErr.Status, body)
		return
	}
	log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// decode reads a JSON body into v and reports whether it succeeded.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// rateLimiter allows a number of events per key within a sliding window.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(limit, windowSeconds int) *rateLimiter {
	return &rateLimiter{limit: limit, window: time.Duration(windowSeconds) * time.Second, hits: map[string][]time.Time{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.limit {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	if len(l.hits) > 10000 {
		l.hits = map[string][]time.Time{key: l.hits[key]}
	}
	return true
}
