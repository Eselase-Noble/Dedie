package httpapi

import (
	"net/http"

	"dedie/engine/app"
	"dedie/engine/connector"
)

// --- users and connections of a database server ---

func (s *Server) databaseAccounts(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	out, err := s.app.DatabaseAccounts(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) saveDatabaseAccount(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in connector.AccountChange
	if !decode(w, r, &in) {
		return
	}
	if err := s.app.SaveDatabaseAccount(r.Context(), actor, r.PathValue("id"), in); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) dropDatabaseAccount(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	err := s.app.DropDatabaseAccount(r.Context(), actor, r.PathValue("id"), r.PathValue("user"), r.URL.Query().Get("host"), r.URL.Query().Get("hand_over") == "1")
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setConnectionLimit(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in connector.LimitChange
	if !decode(w, r, &in) {
		return
	}
	if err := s.app.SetConnectionLimit(r.Context(), actor, r.PathValue("id"), in); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) terminateSessions(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in connector.SessionFilter
	if !decode(w, r, &in) {
		return
	}
	n, err := s.app.TerminateSessions(r.Context(), actor, r.PathValue("id"), in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sessions": n})
}
