package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Built-in defaults for `mailctl login` with no flags; set at build time via
// -ldflags "-X main.defaultTenant=... -X main.defaultClientID=..." (see README).
// Empty by default: a plain build asks for --tenant/--client-id.
var (
	defaultTenant   string
	defaultClientID string
)

// loginBase is where sign-in is spoken; a test points it at a fake.
var loginBase = "https://login.microsoftonline.com"

// openBrowser opens a URL on this machine, best effort: a desktop opens it
// itself; over SSH there is none and the printed line is the one to follow.
var openBrowser = func(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}

// cmdLogin signs a person in with a browser (authorization code + PKCE, the
// redirect landing on localhost) and leaves behind what the other commands
// need: the mailbox-cli-format token cache and a graph account in config.toml.
// The browser sign-in, not the device-code flow, because the security defaults
// most tenants run block device code (AADSTS530035).
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file")
	name := fs.String("account", "", "account name (default: the address's local part)")
	tenant := fs.String("tenant", defaultTenant, "Entra tenant (domain or id)")
	clientID := fs.String("client-id", defaultClientID, "Entra app (client) id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *tenant == "" || *clientID == "" {
		return errors.New("no built-in tenant/app: pass --tenant and --client-id, or build with -ldflags \"-X main.defaultTenant=... -X main.defaultClientID=...\"")
	}

	accounts, err := loadAccountsAllowMissing(configPath(*cfgPath))
	if err != nil {
		return err
	}
	var existing *Account
	for i, a := range accounts {
		if a.Name == *name {
			if a.Type != "graph" {
				return fmt.Errorf("account %q already exists as %s; pick another --account name", a.Name, a.Type)
			}
			existing = &accounts[i]
			break
		}
	}
	if existing != nil && *tenant == defaultTenant {
		*tenant = existing.Tenant // an account's own tenant stands unless asked otherwise
	}
	if existing != nil && *clientID == defaultClientID {
		*clientID = existing.ClientID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tok, err := browserLogin(ctx, *tenant, *clientID)
	if err != nil {
		return err
	}

	me, err := graphMe(ctx, tok.Access)
	if err != nil {
		return err
	}
	address := me.Mail
	if address == "" {
		address = me.UserPrincipalName
	}
	if address == "" {
		return errors.New("the signed-in account reports neither mail nor userPrincipalName")
	}

	tokenPath := ""
	if existing != nil {
		tokenPath = existing.TokenFile
	}
	if tokenPath == "" {
		n := *name
		if n == "" {
			n = accountNameOf(address)
		}
		tokenPath = filepath.Join(filepath.Dir(configPath(*cfgPath)), "graph-"+n+".token.json")
	}
	if err := saveToken(tokenPath, tok.Access, tok.Refresh, tok.Expiry); err != nil {
		return err
	}

	if existing == nil {
		n := *name
		if n == "" {
			n = accountNameOf(address)
		}
		for _, a := range accounts {
			if a.Name == n {
				return fmt.Errorf("account %q already exists in the config; pick another --account name", n)
			}
		}
		cfg := configPath(*cfgPath)
		if err := appendAccount(cfg, n, Account{
			Type: "graph", Tenant: *tenant, ClientID: *clientID,
			Mailbox: address, TokenFile: tokenPath,
		}); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  Signed in as %s, account %q added to %s\n", address, n, cfg)
	} else {
		fmt.Fprintf(os.Stderr, "  Signed in as %s, account %q refreshed\n", address, existing.Name)
	}
	printJSON(map[string]string{"ok": "true", "mailbox": address, "token_file": tokenPath})
	return nil
}

// accountNameOf turns an address into a TOML-safe account name: the local
// part, lowercased, with anything outside bare-key characters dashed.
func accountNameOf(address string) string {
	local := strings.ToLower(address)
	if i := strings.Index(local, "@"); i >= 0 {
		local = local[:i]
	}
	var b strings.Builder
	for _, r := range local {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// appendAccount adds an [accounts.NAME] section textually: config.toml is
// user-editable and a rewrite would drop comments.
func appendAccount(path, name string, a Account) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var b strings.Builder
	s := string(old)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	b.WriteString(s)
	// ponytail: %q covers normal names; a name with a quote inside would need
	// TOML escaping — flag it rather than writing a broken table.
	if strings.ContainsAny(name, `"[]`) {
		return fmt.Errorf("account name %q cannot be written to the config", name)
	}
	fmt.Fprintf(&b, "\n[accounts.%q]\n", name)
	fmt.Fprintf(&b, "type = %q\n", a.Type)
	fmt.Fprintf(&b, "tenant = %q\n", a.Tenant)
	fmt.Fprintf(&b, "client_id = %q\n", a.ClientID)
	fmt.Fprintf(&b, "mailbox = %q\n", a.Mailbox)
	fmt.Fprintf(&b, "token_file = '%s'\n", a.TokenFile)
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func loadAccountsAllowMissing(path string) ([]Account, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	return loadConfig(path)
}

// meInfo is the signed-in person's address as Graph reports it on /me.
type meInfo struct {
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// graphMe reads the signed-in person's address off /me.
func graphMe(ctx context.Context, access string) (meInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", graphBase+"/me?$select=mail,userPrincipalName", nil)
	if err != nil {
		return meInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return meInfo{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return meInfo{}, err
	}
	if resp.StatusCode/100 != 2 {
		return meInfo{}, graphErr(resp.Status, data)
	}
	var me meInfo
	if err := json.Unmarshal(data, &me); err != nil {
		return meInfo{}, err
	}
	return me, nil
}

// browserLogin runs the authorization-code flow with PKCE: an authorize URL is
// opened in the browser, the redirect lands on a one-shot localhost server,
// the code is exchanged for tokens. Scopes are mailbox-cli's, so the cache is
// interchangeable with the one delegatedToken reads.
func browserLogin(ctx context.Context, tenant, clientID string) (token, error) {
	verifier, err := pkceVerifier()
	if err != nil {
		return token{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return token{}, err
	}
	state := base64.RawURLEncoding.EncodeToString(raw[:])

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return token{}, err
	}
	defer l.Close()
	redirect := fmt.Sprintf("http://localhost:%d", l.Addr().(*net.TCPAddr).Port)

	// http://localhost is registered without a port: for native clients Entra
	// matches the loopback regardless of port, so a free one will do.
	authorize := fmt.Sprintf("%s/%s/oauth2/v2.0/authorize?%s", loginBase, url.PathEscape(tenant), url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"scope":                 {delegatedScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}.Encode())
	fmt.Fprintf(os.Stderr, "  Open this address in a browser and sign in:\n  %s\n", authorize)
	_ = openBrowser(authorize)

	code := make(chan any, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			fmt.Fprint(w, "The sign-in was refused. The details are in the terminal.")
			code <- fmt.Errorf("the sign-in was refused: %s", e)
			return
		}
		if q.Get("state") != state {
			fmt.Fprint(w, "Unknown sign-in answer.")
			code <- errors.New("the sign-in answer carries the wrong state")
			return
		}
		fmt.Fprint(w, "Signed in. You can close this tab and return to the terminal.")
		code <- q.Get("code")
	})}
	go srv.Serve(l)
	defer srv.Close()

	var authCode string
	select {
	case <-ctx.Done():
		return token{}, ctx.Err()
	case v := <-code:
		switch v := v.(type) {
		case error:
			return token{}, v
		case string:
			authCode = v
		}
	}
	if authCode == "" {
		return token{}, errors.New("the sign-in came back without a code")
	}
	return exchangeCode(ctx, tenant, clientID, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	})
}

// token is one sign-in's result: the refresh token is the sign-in, the access
// token an hour's worth of it.
type token struct {
	Access  string
	Refresh string
	Expiry  time.Time
}

// exchangeCode asks the token endpoint. A refused sign-in names the cause.
func exchangeCode(ctx context.Context, tenant, clientID string, form url.Values) (token, error) {
	form.Set("client_id", clientID)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(
		fmt.Sprintf(tokenURL, url.PathEscape(tenant)),
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return token{}, err
	}
	if resp.StatusCode/100 != 2 {
		var oe struct {
			Code        string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(data, &oe)
		if oe.Code != "" {
			return token{}, fmt.Errorf("sign-in refused: %s: %s", oe.Code, oe.Description)
		}
		return token{}, graphErr(resp.Status, data)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		Refresh     string `json:"refresh_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tr); err != nil || tr.AccessToken == "" || tr.Refresh == "" {
		return token{}, errors.New("token endpoint: missing access or refresh token")
	}
	return token{tr.AccessToken, tr.Refresh, time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)}, nil
}

// pkceVerifier is a random code verifier: enough entropy for RFC 7636 and no
// characters that need escaping.
func pkceVerifier() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
