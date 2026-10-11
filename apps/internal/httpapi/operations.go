package httpapi

import (
	"io"
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
	var in app.BackupInput
	if !decode(w, r, &in) {
		return
	}
	b, err := s.app.CreateBackup(r.Context(), actor, in, false)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, b)
}

// downloadBackup streams the decrypted archive as a file.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	src, name, err := s.app.ExportBackup(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	defer src.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	io.Copy(w, src)
}

// --- backup schedules ---

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListSchedules(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.ScheduleInput
	if !decode(w, r, &in) {
		return
	}
	sc, err := s.app.SaveSchedule(r.Context(), actor, r.PathValue("id"), in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.DeleteSchedule(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.RunScheduleNow(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
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
