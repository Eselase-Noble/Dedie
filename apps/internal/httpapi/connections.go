package httpapi

import (
	"net/http"
	"strconv"

	"dedie/engine/app"
)

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListConnections(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getConnection(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	c, err := s.app.GetConnection(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) createConnection(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	s.saveConnection(w, r, actor, "", http.StatusCreated)
}

func (s *Server) updateConnection(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	s.saveConnection(w, r, actor, r.PathValue("id"), http.StatusOK)
}

func (s *Server) saveConnection(w http.ResponseWriter, r *http.Request, actor app.Actor, id string, status int) {
	var in app.ConnectionInput
	if !decode(w, r, &in) {
		return
	}
	c, err := s.app.SaveConnection(r.Context(), actor, id, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, status, c)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.DeleteConnection(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) testConnection(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	info, err := s.app.TestConnection(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) testConnectionInput(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.ConnectionInput
	if !decode(w, r, &in) {
		return
	}
	info, err := s.app.TestInput(r.Context(), actor, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	cat, err := s.app.Catalog(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

func (s *Server) query(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.QueryRequest
	if !decode(w, r, &in) {
		return
	}
	resp, err := s.app.Query(r.Context(), actor, r.PathValue("id"), in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	ov, err := s.app.ServerOverview(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *Server) cancelBackend(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	pid, err := strconv.ParseInt(r.PathValue("pid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	terminate := r.URL.Query().Get("terminate") == "1"
	if err := s.app.CancelBackend(r.Context(), actor, r.PathValue("id"), pid, terminate); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
