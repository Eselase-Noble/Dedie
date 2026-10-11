package httpapi

import (
	"log"
	"net/http"

	"dedie/engine/app"
	"dedie/engine/connector"
	"dedie/engine/store"
)

type sessionInfo struct {
	User         store.User       `json:"user"`
	Organization string           `json:"organization"`
	Permissions  []app.Permission `json:"permissions"`
	Kinds        []string         `json:"kinds"`
	// MustEnrollTOTP sends the user to two-factor setup before anything else.
	MustEnrollTOTP bool `json:"must_enroll_totp"`
	// Token is set when the request was authenticated by an API token.
	Token string `json:"token,omitempty"`
}

func (s *Server) sessionInfo(r *http.Request, actor app.Actor) sessionInfo {
	org, _ := s.app.Organization(r.Context(), actor)
	return sessionInfo{User: actor.User, Organization: org.Name, Permissions: actor.Permissions(), Kinds: connector.Kinds(),
		MustEnrollTOTP: actor.MustEnroll, Token: actor.Token}
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	need, err := s.app.NeedsSetup(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needs_setup": need})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Organization string `json:"organization"`
		Name         string `json:"name"`
		Email        string `json:"email"`
		Password     string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, err := s.app.Setup(r.Context(), in.Organization, in.Name, in.Email, in.Password); err != nil {
		fail(w, err)
		return
	}
	s.startSession(w, r, app.LoginInput{Email: in.Email, Password: in.Password})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in app.LoginInput
	if !decode(w, r, &in) {
		return
	}
	if !s.logins.allow(s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many sign-in attempts; wait a minute and try again")
		return
	}
	s.startSession(w, r, in)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, in app.LoginInput) {
	in.IP, in.UserAgent = s.clientIP(r), r.UserAgent()
	res, err := s.app.Login(r.Context(), in)
	if err != nil {
		// The audit log only knows accounts that exist; the server log also
		// records attempts for addresses that do not, which is what an
		// administrator needs when someone reports that they cannot sign in.
		log.Printf("sign-in failed for %q from %s: %v", in.Email, in.IP, err)
		fail(w, err)
		return
	}
	s.setSession(w, res.Token, res.TTL)
	writeJSON(w, http.StatusOK, s.sessionInfo(r, app.Actor{User: res.User, IP: in.IP, MustEnroll: res.MustEnroll}))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	s.app.Logout(r.Context(), actor, sessionToken(r))
	s.setSession(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	writeJSON(w, http.StatusOK, s.sessionInfo(r, actor))
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	users, err := s.app.ListUsers(r.Context(), actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, err := s.app.CreateUser(r.Context(), actor, in.Name, in.Email, in.Password, in.Role)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, actor app.Actor) {
	var in app.UserUpdate
	if !decode(w, r, &in) {
		return
	}
	u, err := s.app.UpdateUser(r.Context(), actor, r.PathValue("id"), in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}
