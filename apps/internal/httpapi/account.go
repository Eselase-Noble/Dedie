package httpapi

import (
	"net/http"

	"dedie/engine/app"
)

// --- the signed-in user's own account ---

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.app.ChangePassword(r.Context(), actor, in.Current, in.New); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListSessions(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.RevokeSession(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) beginTOTP(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	e, err := s.app.BeginTOTP(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) confirmTOTP(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	codes, err := s.app.ConfirmTOTP(r.Context(), actor, in.Code)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"recovery_codes": codes})
}

func (s *Server) disableTOTP(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.app.DisableTOTP(r.Context(), actor, in.Password); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- API tokens ---

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	list, err := s.app.ListTokens(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.TokenInput
	if !decode(w, r, &in) {
		return
	}
	t, err := s.app.CreateToken(r.Context(), actor, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	if err := s.app.RevokeToken(r.Context(), actor, r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- organization security policy ---

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	writeJSON(w, http.StatusOK, s.app.GetSecurityPolicy(r.Context(), actor))
}

func (s *Server) setPolicy(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.SecurityPolicy
	if !decode(w, r, &in) {
		return
	}
	p, err := s.app.SetSecurityPolicy(r.Context(), actor, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// --- who may use a connection ---

func (s *Server) getConnectionAccess(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	acc, err := s.app.GetConnectionAccess(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

func (s *Server) setConnectionAccess(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.ConnectionAccess
	if !decode(w, r, &in) {
		return
	}
	if err := s.app.SetConnectionAccess(r.Context(), actor, r.PathValue("id"), in); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
