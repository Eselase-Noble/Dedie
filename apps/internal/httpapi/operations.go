package httpapi

import (
	"net/http"
	"strconv"

	"dedie/engine/app"
)

// --- migrations ---

func (s *Server) listMigrations(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListMigrations(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createMigration(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.MigrationInput
	if !decode(w, r, &in) {
		return
	}
	j, err := s.app.CreateMigration(r.Context(), actor, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func (s *Server) getMigration(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	d, err := s.app.GetMigration(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) preflightMigration(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	report, err := s.app.PreflightMigration(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) migrationAction(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.MigrationAction(r.Context(), actor, r.PathValue("id"), r.PathValue("action")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- backups ---

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListBackups(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		ConnectionID string `json:"connection_id"`
		Note         string `json:"note"`
	}
	if !decode(w, r, &in) {
		return
	}
	b, err := s.app.CreateBackup(r.Context(), actor, in.ConnectionID, in.Note, false)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, b)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.DeleteBackup(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) verifyBackup(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.VerifyBackup(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.RestoreInput
	if !decode(w, r, &in) {
		return
	}
	res, err := s.app.RestoreBackup(r.Context(), actor, r.PathValue("id"), in, false)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) listRestores(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListRestores(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// --- audit ---

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.app.ListAudit(r.Context(), actor, before, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	v, err := s.app.VerifyAudit(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
