package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Account is one configured mailbox.
type Account struct {
	Name        string `toml:"-"`
	Type        string `toml:"type"` // "imap" | "graph"
	Description string `toml:"description"`
	// imap
	// imap
	IMAP        string `toml:"imap"`
	SMTP        string `toml:"smtp"`
	User        string `toml:"user"`
	Password    string `toml:"password"`
	PasswordCmd string `toml:"password_cmd"`
	SentFolder  string `toml:"sent_folder"`
	// graph
	Tenant          string `toml:"tenant"`
	ClientID        string `toml:"client_id"`
	ClientSecret    string `toml:"client_secret"`
	ClientSecretCmd string `toml:"client_secret_cmd"`
	Mailbox         string `toml:"mailbox"`
	Insecure        bool   `toml:"-"` // tests only: plaintext IMAP/SMTP, never settable from config
}

type config struct {
	Accounts map[string]Account `toml:"accounts"`
}

// configPath resolves the config file location: --config, $MAILCTL_CONFIG, then XDG.
func configPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if p := os.Getenv("MAILCTL_CONFIG"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "mailctl", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mailctl", "config.toml")
}

// loadConfig reads and validates the config file, returning accounts with names set.
func loadConfig(path string) ([]Account, error) {
	var cfg config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var names []string
	for name := range cfg.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Account, 0, len(cfg.Accounts))
	for _, name := range names {
		a := cfg.Accounts[name]
		a.Name = name
		if err := a.validate(); err != nil {
			return nil, err
		}
		if a.SentFolder == "" {
			a.SentFolder = "Sent"
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("config: no [accounts.*] sections in %s", path)
	}
	return out, nil
}

func (a Account) validate() error {
	switch a.Type {
	case "imap":
		for _, f := range []struct{ name, val string }{{"imap", a.IMAP}, {"smtp", a.SMTP}, {"user", a.User}} {
			if f.val == "" {
				return fmt.Errorf("account %q: missing %s", a.Name, f.name)
			}
		}
	case "graph":
		for _, f := range []struct{ name, val string }{{"tenant", a.Tenant}, {"client_id", a.ClientID}, {"mailbox", a.Mailbox}} {
			if f.val == "" {
				return fmt.Errorf("account %q: missing %s", a.Name, f.name)
			}
		}
	default:
		return fmt.Errorf("account %q: unknown type %q (want \"imap\" or \"graph\")", a.Name, a.Type)
	}
	return nil
}

// secret resolves the account's secret: env var, then *_cmd, then the literal field.
func (a Account) secret() (string, error) {
	envName := strings.ToUpper(strings.ReplaceAll(a.Name, "-", "_"))
	var envVar, cmd, literal, field string
	if a.Type == "imap" {
		envVar, cmd, literal, field = "MAILCTL_"+envName+"_PASSWORD", a.PasswordCmd, a.Password, "password"
	} else {
		envVar, cmd, literal, field = "MAILCTL_"+envName+"_CLIENT_SECRET", a.ClientSecretCmd, a.ClientSecret, "client_secret"
	}
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}
	if cmd != "" {
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return "", fmt.Errorf("account %q: %s_cmd failed: %v: %s", a.Name, field, err, ee.Stderr)
			}
			return "", fmt.Errorf("account %q: %s_cmd failed: %v", a.Name, field, err)
		}
		return strings.TrimRight(string(out), "\n"), nil
	}
	if literal != "" {
		return literal, nil
	}
	return "", fmt.Errorf("account %q: no %s available: set %s, %s_cmd, or the literal field", a.Name, field, envVar, field)
}

// Address returns the account's address: user for imap, mailbox for graph.
func (a Account) Address() string {
	if a.Type == "imap" {
		return a.User
	}
	return a.Mailbox
}
