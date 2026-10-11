// Package notify delivers alerts to the places an organization watches: a
// webhook, a Slack-compatible chat hook, or email.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Channel kinds.
const (
	KindWebhook = "webhook"
	KindSlack   = "slack"
	KindEmail   = "email"
)

// Config holds the settings of a channel. Which fields apply depends on
// its kind.
type Config struct {
	// URL is the address a webhook or Slack message is posted to.
	URL string `json:"url,omitempty"`
	// Secret, when set, signs webhook bodies: the X-Dedie-Signature header
	// carries "sha256=" and the hex HMAC-SHA256 of the body.
	Secret string `json:"secret,omitempty"`

	SMTPHost string `json:"smtp_host,omitempty"`
	SMTPPort int    `json:"smtp_port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	From     string `json:"from,omitempty"`
	// To lists the recipients.
	To []string `json:"to,omitempty"`
}

// Message is one alert.
type Message struct {
	Type         string    `json:"type"`
	Subject      string    `json:"subject"`
	Text         string    `json:"message"`
	Organization string    `json:"organization"`
	Time         time.Time `json:"time"`
}

// Validate checks a channel's settings and fills in defaults.
func (c *Config) Validate(kind string) error {
	switch kind {
	case KindWebhook, KindSlack:
		u, err := url.Parse(strings.TrimSpace(c.URL))
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("enter the full address to post to, starting with https://")
		}
		c.URL = u.String()
	case KindEmail:
		c.SMTPHost = strings.TrimSpace(c.SMTPHost)
		if c.SMTPHost == "" {
			return errors.New("enter the SMTP server")
		}
		if c.SMTPPort == 0 {
			c.SMTPPort = 587
		}
		if _, err := mail.ParseAddress(c.From); err != nil {
			return errors.New("enter a valid From address")
		}
		if len(c.To) == 0 {
			return errors.New("enter at least one recipient")
		}
		for _, to := range c.To {
			if _, err := mail.ParseAddress(to); err != nil {
				return fmt.Errorf("%q is not a valid email address", to)
			}
		}
	default:
		return fmt.Errorf("unknown channel kind %q", kind)
	}
	return nil
}

// Summary describes where a channel delivers, without credentials.
func (c Config) Summary(kind string) string {
	switch kind {
	case KindEmail:
		return strings.Join(c.To, ", ") + " via " + c.SMTPHost
	default:
		// Chat hooks carry their credential in the path, so only the
		// host is shown.
		if u, err := url.Parse(c.URL); err == nil {
			return u.Host
		}
	}
	return ""
}

// Send delivers a message through a channel.
func Send(ctx context.Context, kind string, cfg Config, msg Message) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	switch kind {
	case KindWebhook:
		body, _ := json.Marshal(msg)
		headers := map[string]string{"X-Dedie-Event": msg.Type}
		if cfg.Secret != "" {
			mac := hmac.New(sha256.New, []byte(cfg.Secret))
			mac.Write(body)
			headers["X-Dedie-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		return post(ctx, cfg.URL, body, headers)
	case KindSlack:
		body, _ := json.Marshal(map[string]string{"text": "*" + msg.Subject + "*\n" + msg.Text})
		return post(ctx, cfg.URL, body, nil)
	case KindEmail:
		return sendMail(ctx, cfg, msg)
	}
	return fmt.Errorf("unknown channel kind %q", kind)
}

func post(ctx context.Context, address string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dedie")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Redirects are not followed: an alert goes where it was configured to
	// go and nowhere else.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		// The address may embed a credential; report the cause without it.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return urlErr.Err
		}
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the endpoint answered %s", resp.Status)
	}
	return nil
}

// sendMail submits a message over SMTP. Port 465 is spoken with TLS from
// the first byte; any other port must offer STARTTLS, so that neither the
// credentials nor the alert travel in clear text.
func sendMail(ctx context.Context, cfg Config, msg Message) error {
	addr := net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(cfg.SMTPPort))
	tlsConfig := &tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}
	dialer := &net.Dialer{}
	var conn net.Conn
	var err error
	if cfg.SMTPPort == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()
	if cfg.SMTPPort != 465 {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("the mail server does not offer STARTTLS; use port 465 or a server that encrypts")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)); err != nil {
			return err
		}
	}
	from, _ := mail.ParseAddress(cfg.From)
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	for _, to := range cfg.To {
		addr, _ := mail.ParseAddress(to)
		if err := client.Rcpt(addr.Address); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(mailBody(cfg, msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// mailBody renders the message. Header values have line breaks removed so
// that alert text can never add headers of its own.
func mailBody(cfg Config, msg Message) []byte {
	clean := strings.NewReplacer("\r", " ", "\n", " ")
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", clean.Replace(cfg.From))
	fmt.Fprintf(&b, "To: %s\r\n", clean.Replace(strings.Join(cfg.To, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", clean.Replace("[Dedie] "+msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", msg.Time.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	body := msg.Text + "\n\nOrganization: " + msg.Organization + "\nEvent: " + msg.Type + "\nTime: " + msg.Time.Format(time.RFC1123)
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.Bytes()
}
