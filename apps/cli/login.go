package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// profile is the saved sign-in to a Dedie server.
type profile struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	// TokenID lets `dedie logout` revoke the token at the server.
	TokenID string `json:"token_id,omitempty"`
	Email   string `json:"email,omitempty"`
	// CACert is a PEM file for servers whose certificate is signed by a
	// private certificate authority.
	CACert string `json:"ca_cert,omitempty"`
}

// profilePath is where the sign-in is kept: DEDIE_CONFIG, or dedie/cli.json
// under the user's configuration directory.
func profilePath() (string, error) {
	if p := os.Getenv("DEDIE_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dedie", "cli.json"), nil
}

// loadProfile returns the server to use, if any: the environment first
// (for CI, where nothing is saved), then the saved sign-in.
func loadProfile() (*profile, error) {
	if server := os.Getenv("DEDIE_SERVER"); server != "" {
		token := os.Getenv("DEDIE_TOKEN")
		if token == "" {
			return nil, errors.New("DEDIE_SERVER is set but DEDIE_TOKEN is not; create an API token on the Account page")
		}
		return &profile{Server: server, Token: token, CACert: os.Getenv("DEDIE_CA_CERT")}, nil
	}
	path, err := profilePath()
	if err != nil {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var p profile
	if err := json.Unmarshal(b, &p); err != nil || p.Server == "" || p.Token == "" {
		return nil, nil
	}
	return &p, nil
}

func saveProfile(p profile) error {
	path, err := profilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	// The file holds a credential: readable by its owner only.
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// connect builds the remote backend for a saved sign-in.
func (c *cli) connect(p *profile) (*remote, error) {
	base, err := checkServerURL(p.Server)
	if err != nil {
		return nil, err
	}
	client, err := newHTTPClient(p.CACert)
	if err != nil {
		return nil, err
	}
	return &remote{ctx: c.ctx, base: base, token: p.Token, http: client}, nil
}

func prompt(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}

// login signs in to a Dedie server and saves an API token for later
// commands. The password is used once and never stored.
func (c *cli) login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	email := fs.String("email", "", "your email address (prompted for when omitted)")
	caCert := fs.String("ca-cert", "", "PEM file with the certificate authority of a privately signed server")
	days := fs.Int("days", 90, "days until this sign-in expires (0: never)")
	useToken := fs.Bool("with-token", false, "use an existing API token from DEDIE_TOKEN instead of signing in with a password")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return errors.New("usage: dedie login <server address>, for example: dedie login https://dedie.example.com")
	}
	base, err := checkServerURL(pos[0])
	if err != nil {
		return err
	}
	client, err := newHTTPClient(*caCert)
	if err != nil {
		return err
	}
	r := &remote{ctx: c.ctx, base: base, http: client}
	saved := profile{Server: base, CACert: *caCert}

	var me struct {
		User struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
		Organization string `json:"organization"`
	}
	if *useToken {
		if saved.Token = os.Getenv("DEDIE_TOKEN"); saved.Token == "" {
			return errors.New("set DEDIE_TOKEN to the token to save")
		}
		r.token = saved.Token
		if err := r.send("GET", "/auth/me", nil, &me); err != nil {
			return err
		}
	} else {
		if *email == "" {
			if *email, err = prompt("Email: "); err != nil {
				return err
			}
		}
		password, err := readPassword("Password: ")
		if err != nil {
			return err
		}
		creds := map[string]string{"email": *email, "password": password}
		err = r.send("POST", "/auth/login", creds, &me)
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Code == "totp_required" {
			if creds["code"], err = prompt("Authentication code: "); err != nil {
				return err
			}
			err = r.send("POST", "/auth/login", creds, &me)
		}
		if err != nil {
			return err
		}
		// Exchange the session for a token, then end the session: from
		// here on only the token exists, and it can be revoked alone.
		host, _ := os.Hostname()
		var created struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := r.send("POST", "/auth/tokens", map[string]any{"name": "CLI on " + host, "days": *days}, &created); err != nil {
			return err
		}
		r.send("POST", "/auth/logout", nil, nil)
		saved.Token, saved.TokenID = created.Token, created.ID
	}
	saved.Email = me.User.Email
	if err := saveProfile(saved); err != nil {
		return err
	}
	fmt.Printf("Signed in to %s as %s (%s, %s).\nCommands now run on that server. `dedie logout` signs out; `dedie --local <command>` uses this machine's data directory.\n",
		base, me.User.Email, me.User.Role, me.Organization)
	return nil
}

// logout revokes the saved token at the server and forgets it.
func (c *cli) logout() error {
	p, _ := loadProfile()
	path, err := profilePath()
	if err != nil {
		return err
	}
	if p == nil || os.Getenv("DEDIE_SERVER") != "" {
		return errors.New("there is no saved sign-in")
	}
	if p.TokenID != "" {
		if r, err := c.connect(p); err == nil {
			if err := r.send("DELETE", "/auth/tokens/"+seg(p.TokenID), nil, nil); err != nil {
				fmt.Fprintln(os.Stderr, "dedie: the token could not be revoked at the server:", err)
			}
		}
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Println("Signed out of", p.Server)
	return nil
}

// status says where commands run and as whom.
func (c *cli) status(forceLocal bool) error {
	p, err := loadProfile()
	if err != nil {
		return err
	}
	if p == nil || forceLocal {
		fmt.Printf("Mode:  local\nData:  %s\n", dataDir())
		if p != nil {
			fmt.Printf("Saved sign-in to %s is not used with --local.\n", p.Server)
		}
		return nil
	}
	r, err := c.connect(p)
	if err != nil {
		return err
	}
	var me struct {
		User struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
		Organization string `json:"organization"`
		Token        string `json:"token"`
	}
	if err := r.send("GET", "/auth/me", nil, &me); err != nil {
		return err
	}
	fmt.Printf("Mode:          remote\nServer:        %s\nOrganization:  %s\nSigned in as:  %s (%s)\nToken:         %s\n",
		r.base, me.Organization, me.User.Email, me.User.Role, me.Token)
	return nil
}
