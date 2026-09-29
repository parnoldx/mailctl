package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test stand-in values for the build-time defaults.
const (
	testTenant   = "example.com"
	testClientID = "00000000-0000-0000-0000-000000000000"
)

// setupLogin starts a fake authorize+token+Graph server, points loginBase,
// tokenURL, graphBase and openBrowser at it, and redirects config into a temp
// dir. The openBrowser stub plays the browser: it fetches the authorize URL,
// which answers 302 onto the loopback redirect, which feeds cmdLogin's code.
func setupLogin(t *testing.T, tokenForm *url.Values) {
	t.Helper()
	oldTenant, oldClientID := defaultTenant, defaultClientID
	defaultTenant, defaultClientID = testTenant, testClientID
	t.Cleanup(func() { defaultTenant, defaultClientID = oldTenant, oldClientID })

	mux := http.NewServeMux()
	mux.HandleFunc("/example.com/oauth2/v2.0/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != testClientID || q.Get("code_challenge") == "" ||
			q.Get("state") == "" || q.Get("code_challenge_method") != "S256" ||
			!strings.HasPrefix(q.Get("redirect_uri"), "http://localhost:") {
			t.Errorf("authorize request missing PKCE/state/redirect: %v", q)
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=AUTHCODE&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/example.com/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if tokenForm != nil {
			*tokenForm = r.PostForm
		}
		writeJSON(t, w, 200, map[string]any{"access_token": "AT0K", "refresh_token": "RT0K", "expires_in": 3600})
	})
	mux.HandleFunc("/v1.0/me", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer AT0K" {
			t.Errorf("Authorization = %q, want Bearer AT0K", got)
		}
		writeJSON(t, w, 200, map[string]string{"mail": "pa@example.com", "userPrincipalName": "pa@example.com"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldLogin, oldTok, oldGraph, oldOpen := loginBase, tokenURL, graphBase, openBrowser
	loginBase, tokenURL, graphBase = srv.URL, srv.URL+"/%s/oauth2/v2.0/token", srv.URL+"/v1.0"
	t.Cleanup(func() { loginBase, tokenURL, graphBase, openBrowser = oldLogin, oldTok, oldGraph, oldOpen })

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	openBrowser = func(target string) error {
		go func() { http.Get(target) }() //nolint: the simulated browser; errors surface in the handlers
		return nil
	}
}

func TestLoginCreatesAccount(t *testing.T) {
	var tokenForm url.Values
	setupLogin(t, &tokenForm)

	if err := cmdLogin(nil); err != nil {
		t.Fatalf("cmdLogin: %v", err)
	}

	if tokenForm.Get("grant_type") != "authorization_code" || tokenForm.Get("code") != "AUTHCODE" ||
		tokenForm.Get("code_verifier") == "" || !strings.HasPrefix(tokenForm.Get("redirect_uri"), "http://localhost:") {
		t.Fatalf("token form wrong: %v", tokenForm)
	}

	cfg := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "mailctl", "config.toml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	want := fmt.Sprintf("[accounts.%q]", "pa")
	if !strings.Contains(string(data), want) ||
		!strings.Contains(string(data), `client_id = "`+testClientID+`"`) ||
		!strings.Contains(string(data), `mailbox = "pa@example.com"`) ||
		!strings.Contains(string(data), "token_file = '") {
		t.Fatalf("config.toml missing the account:\n%s", data)
	}

	tokenFile := filepath.Join(filepath.Dir(cfg), "graph-pa.token.json")
	tb, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var tok struct {
		Refresh string `json:"refresh_token"`
	}
	if err := json.Unmarshal(tb, &tok); err != nil || tok.Refresh != "RT0K" {
		t.Fatalf("token file wrong: %s (%v)", tb, err)
	}
}

func TestLoginRefreshesExistingAccount(t *testing.T) {
	setupLogin(t, nil)
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "mailctl")
	tokenPath := filepath.Join(cfgDir, "graph-pa.token.json")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"),
		[]byte("[accounts.pa]\ntype = \"graph\"\ntenant = \"example.com\"\nclient_id = \""+testClientID+"\"\nmailbox = \"pa@example.com\"\ntoken_file = '"+tokenPath+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"access_token":"old","refresh_token":"old","expiry":"2000-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cmdLogin([]string{"--account", "pa"}); err != nil {
		t.Fatalf("cmdLogin: %v", err)
	}

	var tok struct {
		Refresh string `json:"refresh_token"`
	}
	tb, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if err := json.Unmarshal(tb, &tok); err != nil || tok.Refresh != "RT0K" {
		t.Fatalf("token file not refreshed: %s (%v)", tb, err)
	}
	data, _ := os.ReadFile(filepath.Join(cfgDir, "config.toml"))
	if strings.Count(string(data), "[accounts.pa]") != 1 {
		t.Fatalf("config.toml duplicated the section:\n%s", data)
	}
}

func TestAccountNameOf(t *testing.T) {
	for in, want := range map[string]string{
		"pa@example.com":    "pa",
		"Hans.Peter@x.de":   "hans-peter",
		"weird!name@a.b.c":  "weird-name",
		"keep_underscore-a": "keep_underscore-a",
	} {
		if got := accountNameOf(in); got != want {
			t.Errorf("accountNameOf(%q) = %q, want %q", in, got, want)
		}
	}
}
