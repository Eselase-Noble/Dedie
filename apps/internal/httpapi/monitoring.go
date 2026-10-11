package httpapi

import (
	"net/http"
	"strconv"

	"dedie/engine/app"
)

// healthz answers load balancers and uptime checks. It needs no sign-in
// and says nothing beyond whether the service is working.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Healthy(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// metrics serves the Prometheus text format. Scrapers authenticate with an
// API token: `authorization: {credentials: <token>}` in a scrape config.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if !actor.Can(app.PermServerAdmin) {
		fail(w, app.ErrForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.app.WriteMetrics(r.Context(), actor, w); err != nil {
		fail(w, err)
	}
}

func (s *Server) monitoring(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	m, err := s.app.GetMonitoring(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) listAlertEvents(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.app.ListAlertEvents(r.Context(), actor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) listAlertChannels(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListAlertChannels(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) saveAlertChannel(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.ChannelInput
	if !decode(w, r, &in) {
		return
	}
	ch, err := s.app.SaveAlertChannel(r.Context(), actor, r.PathValue("id"), in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) deleteAlertChannel(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.DeleteAlertChannel(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) testAlertChannel(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.TestAlertChannel(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
