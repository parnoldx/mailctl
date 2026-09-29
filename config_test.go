package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string // substring of expected error, "" = success
		check   func(t *testing.T, accts []Account)
	}{
		{
			name: "valid imap with defaults",
			toml: `
[accounts.work]
type = "imap"
imap = "imap.example.com:993"
smtp = "smtp.example.com:587"
user = "me@example.com"
password = "hunter2"
`,
			check: func(t *testing.T, accts []Account) {
				if len(accts) != 1 || accts[0].Name != "work" {
					t.Fatalf("got %+v", accts)
				}
				if accts[0].SentFolder != "Sent" {
					t.Errorf("SentFolder = %q, want Sent", accts[0].SentFolder)
				}
				if accts[0].Address() != "me@example.com" {
					t.Errorf("Address = %q", accts[0].Address())
				}
			},
		},
		{
			name: "imap missing smtp",
			toml: `
[accounts.work]
type = "imap"
imap = "imap.example.com:993"
user = "me@example.com"
`,
			wantErr: "missing smtp",
		},
		{
			name: "graph missing tenant",
			toml: `
[accounts.support]
type = "graph"
client_id = "id"
mailbox = "support@example.com"
`,
			wantErr: "missing tenant",
		},
		{
			name: "unknown type",
			toml: `
[accounts.x]
type = "nntp"
`,
			wantErr: "unknown type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			accts, err := loadConfig(writeConfig(t, tt.toml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, accts)
		})
	}
}

func TestSecretOrder(t *testing.T) {
	base := `
[accounts.work]
type = "imap"
imap = "imap.example.com:993"
smtp = "smtp.example.com:587"
user = "me@example.com"
`
	graph := `
[accounts.support]
type = "graph"
tenant = "t"
client_id = "cid"
mailbox = "support@example.com"
`
	t.Setenv("MAILCTL_WORK_PASSWORD", "from-env")
	t.Setenv("MAILCTL_SUPPORT_CLIENT_SECRET", "from-env")

	// env beats cmd beats literal
	accts, err := loadConfig(writeConfig(t, base+`password_cmd = "echo from-cmd"
password = "literal"`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := accts[0].secret(); got != "from-env" {
		t.Errorf("secret = %q, want from-env", got)
	}

	// cmd beats literal, trailing newline trimmed, cmd sets env var unset
	os.Unsetenv("MAILCTL_WORK_PASSWORD")
	accts, err = loadConfig(writeConfig(t, base+`password_cmd = "echo from-cmd"
password = "literal"`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := accts[0].secret(); got != "from-cmd" {
		t.Errorf("secret = %q, want from-cmd", got)
	}

	// literal fallback
	accts, err = loadConfig(writeConfig(t, base+`password = "literal"`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := accts[0].secret(); got != "literal" {
		t.Errorf("secret = %q, want literal", got)
	}

	// graph env var
	accts, err = loadConfig(writeConfig(t, graph+`client_secret_cmd = "echo from-cmd"
client_secret = "literal"`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := accts[0].secret(); got != "from-env" {
		t.Errorf("graph secret = %q, want from-env", got)
	}

	// cmd failure includes stderr
	os.Unsetenv("MAILCTL_SUPPORT_CLIENT_SECRET")
	accts, err = loadConfig(writeConfig(t, graph+`client_secret_cmd = "echo boom >&2; exit 1"`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accts[0].secret(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want stderr boom", err)
	}

	// nothing set → error
	accts, err = loadConfig(writeConfig(t, graph))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accts[0].secret(); err == nil {
		t.Error("want error when no secret source")
	}
}
