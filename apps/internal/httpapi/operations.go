package httpapi

import (
	"io"
	"net/http"
	"strconv"

	"dedie/engine/app"
	"dedie/engine/migrate"
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

// previewMigration lists the statements the migration would run on the target.
func (s *Server) previewMigration(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.PreviewMigration(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// migrationLeftovers lists what the migration leaves behind on the source,
// with the definition of each object.
func (s *Server) migrationLeftovers(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.MigrationLeftovers(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// setOnExisting records what the migration should do about objects that
// already exist on the target.
func (s *Server) setOnExisting(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Policy string `json:"policy"`
	}
	if !decode(w, r, &in) {
		return
	}
	job, err := s.app.SetOnExisting(r.Context(), actor, r.PathValue("id"), in.Policy)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) migrationAction(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.MigrationAction(r.Context(), actor, r.PathValue("id"), r.PathValue("action")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) suggestColumns(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Tables        []string `json:"tables"`
		ExcludeTables []string `json:"exclude_tables"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := s.app.SuggestColumns(r.Context(), actor, r.PathValue("id"), migrate.Config{Tables: in.Tables, ExcludeTables: in.ExcludeTables})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
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

// --- point-in-time recovery ---

func (s *Server) listWALArchives(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.WALArchives(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) setWALArchive(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Enabled       bool `json:"enabled"`
		RetentionDays int  `json:"retention_days"`
	}
	if !decode(w, r, &in) {
		return
	}
	st, err := s.app.SetWALArchive(r.Context(), actor, r.PathValue("id"), in.Enabled, in.RetentionDays)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// --- backup locations ---

func (s *Server) listLocations(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListLocations(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default_path": s.app.DefaultLocation(), "locations": list})
}

func (s *Server) createLocation(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.LocationInput
	if !decode(w, r, &in) {
		return
	}
	l, err := s.app.CreateLocation(r.Context(), actor, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) deleteLocation(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.DeleteLocation(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
