package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookIsSigned(t *testing.T) {
	var body, signature string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, signature = string(b), r.Header.Get("X-Dedie-Signature")
	}))
	defer srv.Close()
	cfg := Config{URL: srv.URL, Secret: "s3cret"}
	if err := cfg.Validate(KindWebhook); err != nil {
		t.Fatal(err)
	}
	if err := Send(context.Background(), KindWebhook, cfg, Message{Type: "backup.failed", Subject: "s", Text: "m", Time: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(body))
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); signature != want {
		t.Errorf("signature %q, want %q", signature, want)
	}
	if !strings.Contains(body, `"type":"backup.failed"`) {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestFailuresAndRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := Send(context.Background(), KindSlack, Config{URL: srv.URL}, Message{}); err == nil {
		t.Error("a 500 answer was treated as delivered")
	}
	if err := Send(context.Background(), KindSlack, Config{URL: srv.URL + "/moved"}, Message{}); err == nil {
		t.Error("a redirect was followed or treated as delivered")
	}
}

func TestValidateAndHeaders(t *testing.T) {
	for kind, cfg := range map[string]Config{
		KindWebhook: {URL: "ftp://example.com"},
		KindSlack:   {URL: "not a url"},
		KindEmail:   {SMTPHost: "smtp.example.com", From: "a@example.com"},
	} {
		if err := cfg.Validate(kind); err == nil {
			t.Errorf("%s: invalid settings were accepted", kind)
		}
	}
	cfg := Config{SMTPHost: "smtp.example.com", From: "a@example.com", To: []string{"b@example.com"}}
	if err := cfg.Validate(KindEmail); err != nil || cfg.SMTPPort != 587 {
		t.Errorf("valid email settings: %v, port %d", err, cfg.SMTPPort)
	}
	// Alert text must not be able to add mail headers.
	mail := string(mailBody(cfg, Message{Subject: "x\r\nBcc: evil@example.com", Text: "body", Time: time.Unix(0, 0)}))
	if strings.Contains(strings.SplitN(mail, "\r\n\r\n", 2)[0], "\r\nBcc:") {
		t.Errorf("header injection: %q", mail)
	}
}
