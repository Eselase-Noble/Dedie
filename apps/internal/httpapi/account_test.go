package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"

	"dedie/engine/app"
	"dedie/engine/secure"
)

func TestAccountSecurity(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := httptest.NewServer(New(a, Options{}))
	defer srv.Close()
	expect := func(what string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: status %d, want %d", what, got, want)
		}
	}
	const password = "a-long-enough-password"
	owner := newClient(t, srv.URL)
	owner.do("POST", "/api/v1/setup", map[string]string{"organization": "Acme", "name": "Ada", "email": "ada@example.com", "password": password}, nil)
	status, body := owner.do("POST", "/api/v1/users", map[string]string{"name": "Ana", "email": "ana@example.com", "password": password, "role": "analyst"}, nil)
	expect("create analyst", status, 201)
	analystID := body["id"].(string)

	// --- two-factor sign-in ---
	status, body = owner.do("POST", "/api/v1/auth/totp/begin", nil, nil)
	expect("begin two-factor", status, 200)
	secret := body["secret"].(string)
	status, _ = owner.do("POST", "/api/v1/auth/totp/confirm", map[string]string{"code": "000000"}, nil)
	expect("confirm with a wrong code", status, 400)
	code, _ := secure.TOTPCode(secret, time.Now())
	status, body = owner.do("POST", "/api/v1/auth/totp/confirm", map[string]string{"code": code}, nil)
	expect("confirm two-factor", status, 200)
	recovery := body["recovery_codes"].([]any)
	if len(recovery) != 8 {
		t.Fatalf("got %d recovery codes, want 8", len(recovery))
	}

	second := newClient(t, srv.URL)
	status, body = second.do("POST", "/api/v1/auth/login", map[string]string{"email": "ada@example.com", "password": password}, nil)
	expect("password without a code", status, 401)
	if body["code"] != "totp_required" {
		t.Errorf("expected totp_required, got %v", body)
	}
	status, _ = second.do("POST", "/api/v1/auth/login", map[string]string{"email": "ada@example.com", "password": password, "code": "123456"}, nil)
	expect("wrong code", status, 401)
	status, _ = second.do("POST", "/api/v1/auth/login", map[string]string{"email": "ada@example.com", "password": password, "code": code}, nil)
	expect("password and code", status, 200)
	third := newClient(t, srv.URL)
	status, _ = third.do("POST", "/api/v1/auth/login", map[string]string{"email": "ada@example.com", "password": password, "code": recovery[0].(string)}, nil)
	expect("recovery code", status, 200)
	fourth := newClient(t, srv.URL)
	status, _ = fourth.do("POST", "/api/v1/auth/login", map[string]string{"email": "ada@example.com", "password": password, "code": recovery[0].(string)}, nil)
	expect("recovery code used twice", status, 401)

	// --- sessions ---
	status, _ = second.do("GET", "/api/v1/auth/sessions", nil, nil)
	expect("list sessions", status, 200)
	status, _ = second.do("POST", "/api/v1/auth/password", map[string]string{"current": "wrong", "new": "a-different-long-password"}, nil)
	expect("password change with the wrong current password", status, 403)
	status, _ = second.do("POST", "/api/v1/auth/password", map[string]string{"current": password, "new": "a-different-long-password"}, nil)
	expect("password change", status, 200)
	status, _ = third.do("GET", "/api/v1/auth/me", nil, nil)
	expect("other session after a password change", status, 401)
	status, _ = second.do("GET", "/api/v1/auth/me", nil, nil)
	expect("current session after a password change", status, 200)
	owner = second

	// --- API tokens ---
	status, _ = owner.do("POST", "/api/v1/auth/tokens", map[string]any{"name": "ci", "role": "owner"}, nil)
	expect("owner token", status, 201)
	analyst := newClient(t, srv.URL)
	analyst.do("POST", "/api/v1/auth/login", map[string]string{"email": "ana@example.com", "password": password}, nil)
	status, _ = analyst.do("POST", "/api/v1/auth/tokens", map[string]any{"name": "too much", "role": "admin"}, nil)
	expect("token above the user's own role", status, 403)
	status, body = analyst.do("POST", "/api/v1/auth/tokens", map[string]any{"name": "reports", "role": "viewer", "days": 30}, nil)
	expect("viewer token", status, 201)
	token := body["token"].(string)
	bearer := map[string]string{"Authorization": "Bearer " + token, csrfHeader: ""}
	api := newClient(t, srv.URL)
	status, body = api.do("GET", "/api/v1/auth/me", nil, bearer)
	expect("token request", status, 200)
	if role := body["user"].(map[string]any)["role"]; role != "viewer" {
		t.Errorf("token acts as %v, want viewer", role)
	}
	status, _ = api.do("POST", "/api/v1/auth/tokens", map[string]any{"name": "more"}, bearer)
	expect("token creating a token", status, 403)
	status, _ = api.do("GET", "/api/v1/auth/me", nil, map[string]string{"Authorization": "Bearer dedie_pat_wrong"})
	expect("unknown token", status, 401)

	// --- connection permissions ---
	conn := map[string]any{"name": "payroll", "kind": "postgres", "host": "127.0.0.1", "database": "d", "username": "u", "password": "p"}
	status, body = owner.do("POST", "/api/v1/connections", conn, nil)
	expect("create connection", status, 201)
	connID := body["id"].(string)
	status, _ = analyst.do("GET", "/api/v1/connections/"+connID, nil, nil)
	expect("open connection", status, 200)
	status, _ = analyst.do("PUT", "/api/v1/connections/"+connID+"/access", map[string]any{"restricted": true, "user_ids": []string{}}, nil)
	expect("analyst changes access", status, 403)
	status, _ = owner.do("PUT", "/api/v1/connections/"+connID+"/access", map[string]any{"restricted": true, "user_ids": []string{}}, nil)
	expect("restrict connection", status, 200)
	status, _ = analyst.do("GET", "/api/v1/connections/"+connID, nil, nil)
	expect("restricted connection without a grant", status, 404)
	status, _ = owner.do("PUT", "/api/v1/connections/"+connID+"/access", map[string]any{"restricted": true, "user_ids": []string{analystID}}, nil)
	expect("grant connection", status, 200)
	status, _ = analyst.do("GET", "/api/v1/connections/"+connID, nil, nil)
	expect("restricted connection with a grant", status, 200)

	// --- organization policy ---
	status, _ = analyst.do("PUT", "/api/v1/auth/policy", map[string]any{"password_min_length": 8, "session_hours": 1}, nil)
	expect("analyst changes policy", status, 403)
	status, _ = owner.do("PUT", "/api/v1/auth/policy", map[string]any{"password_min_length": 16, "password_require_mixed": true, "session_hours": 8, "require_totp": true}, nil)
	expect("set policy", status, 200)
	status, _ = owner.do("POST", "/api/v1/users", map[string]string{"name": "Bo", "email": "bo@example.com", "password": password, "role": "viewer"}, nil)
	expect("password that breaks the policy", status, 400)
	status, body = analyst.do("GET", "/api/v1/connections", nil, nil)
	expect("user without two-factor under a policy that requires it", status, 403)
	if body["code"] != "totp_enrollment_required" {
		t.Errorf("expected totp_enrollment_required, got %v", body)
	}
	status, _ = analyst.do("POST", "/api/v1/auth/totp/begin", nil, nil)
	expect("enrolment stays reachable", status, 200)

	status, _ = owner.do("DELETE", "/api/v1/auth/tokens/nope", nil, nil)
	expect("revoke unknown token", status, 404)
}
