package httpapi

import (
	"net/http"

	"dedie/engine/app"
	"dedie/engine/connector"
)

func (s *Server) connectionStatuses(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	statuses, err := s.app.ConnectionStatuses(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statuses)
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Name          string `json:"name"`
		AddConnection bool   `json:"add_connection"`
	}
	if !decode(w, r, &in) {
		return
	}
	conn, err := s.app.CreateDatabase(r.Context(), actor, r.PathValue("id"), in.Name, in.AddConnection)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "connection": conn})
}

func (s *Server) createSchema(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.app.CreateSchema(r.Context(), actor, r.PathValue("id"), in.Name); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Server) createTable(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in connector.TableDef
	if !decode(w, r, &in) {
		return
	}
	if err := s.app.CreateTable(r.Context(), actor, r.PathValue("id"), in); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}
