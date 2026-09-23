package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// stringSlice accumulates repeated and comma-separated flag values.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usageFail("no subcommand")
	}
	var err error
	switch os.Args[1] {
	case "accounts":
		err = cmdAccounts(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "get":
		err = cmdGet(reorderArgs(os.Args[2:]))
	case "send":
		err = cmdSend(os.Args[2:])
	case "mark":
		err = cmdMark(reorderArgs(os.Args[2:]))
	case "move":
		err = cmdMove(reorderArgs(os.Args[2:]))
	default:
		usageFail("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		var uerr *usageError
		if asUsage(err, &uerr) {
			usageFail("%v", err)
		}
		fail(1, "%v", err)
	}
}

type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }

func asUsage(err error, uerr **usageError) bool {
	if e, ok := err.(*usageError); ok {
		*uerr = e
		return true
	}
	return false
}

func fail(code int, format string, args ...any) {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{fmt.Sprintf(format, args...)})
	fmt.Fprintln(os.Stderr, string(b))
	os.Exit(code)
}

func usageFail(format string, args ...any) { fail(2, format, args...) }

func printJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fail(1, "marshal output: %v", err)
	}
	fmt.Println(string(b))
}

// reorderArgs moves positional args to the front so flag.Parse sees flags
// after the positional id (e.g. "get 42 --account x").
func reorderArgs(args []string) []string {
	boolFlags := map[string]bool{"unread": true, "read": true, "html": true}
	var pos, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			rest = append(rest, a)
			name := strings.TrimLeft(a, "-")
			if !boolFlags[name] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(pos, rest...)
}

// selectAccount loads the config and picks the named account, or the only one.
func selectAccount(path, name string) (Account, error) {
	accounts, err := loadConfig(path)
	if err != nil {
		return Account{}, err
	}
	if name == "" {
		if len(accounts) != 1 {
			return Account{}, &usageError{fmt.Errorf("--account is required when multiple accounts are configured")}
		}
		return accounts[0], nil
	}
	for _, a := range accounts {
		if a.Name == name {
			return a, nil
		}
	}
	return Account{}, &usageError{fmt.Errorf("unknown account %q", name)}
}

func open(a Account) (Mailbox, error) {
	switch a.Type {
	case "imap":
		return newIMAP(a)
	case "graph":
		return newGraph(a)
	default:
		return nil, fmt.Errorf("account %q: unknown type %q", a.Name, a.Type)
	}
}

func cmdAccounts(args []string) error {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file")
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	accounts, err := loadConfig(configPath(*cfgPath))
	if err != nil {
		return err
	}
	type acct struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Address     string `json:"address"`
		Description string `json:"description"`
	}
	out := make([]acct, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, acct{a.Name, a.Type, a.Address(), a.Description})
	}
	printJSON(out)
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file")
	account := fs.String("account", "", "account name")
	folder := fs.String("folder", "", "folder (default inbox)")
	unread := fs.Bool("unread", false, "only unread messages")
	since := fs.String("since", "", "only messages since YYYY-MM-DD")
	limit := fs.Int("limit", 50, "maximum number of messages")
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	a, err := selectAccount(configPath(*cfgPath), *account)
	if err != nil {
		return err
	}
	opts := ListOpts{Folder: *folder, Unread: *unread, Limit: *limit}
	if *since != "" {
		t, err := time.Parse("2006-01-02", *since)
		if err != nil {
			return &usageError{fmt.Errorf("invalid --since %q: want YYYY-MM-DD", *since)}
		}
		opts.Since = t
	}
	mb, err := open(a)
	if err != nil {
		return err
	}
	msgs, err := mb.List(opts)
	if err != nil {
		return err
	}
	if msgs == nil {
		msgs = []Msg{}
	}
	printJSON(msgs)
	return nil
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file")
	account := fs.String("account", "", "account name")
	saveDir := fs.String("save-attachments", "", "directory to save attachments to")
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	if fs.NArg() != 1 {
		return &usageError{fmt.Errorf("usage: mailctl get <id> [--account NAME] [--save-attachments DIR]")}
	}
	a, err := selectAccount(configPath(*cfgPath), *account)
	if err != nil {
		return err
	}
	mb, err := open(a)
	if err != nil {
		return err
	}
	full, err := mb.Get(fs.Arg(0), *saveDir)
	if err != nil {
		return err
	}
	printJSON(full)
	return nil
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file")
	account := fs.String("account", "", "account name")
	var to, cc, attach stringSlice
	fs.Var(&to, "to", "recipient (repeatable or comma-separated)")
	fs.Var(&cc, "cc", "cc recipient (repeatable or comma-separated)")
	subject := fs.String("subject", "", "subject")
	bodyFile := fs.String("body-file", "", "file to read body from (default: stdin if not a terminal)")
	html := fs.Bool("html", false, "body is HTML")
	fs.Var(&attach, "attach", "file to attach (repeatable)")
	replyTo := fs.String("reply-to", "", "message id to reply to")
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	if len(to) == 0 && *replyTo == "" {
		return &usageError{fmt.Errorf("--to is required unless --reply-to is set")}
	}
	if *subject == "" && *replyTo == "" {
		return &usageError{fmt.Errorf("--subject is required unless --reply-to is set")}
	}
	body := ""
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}
		body = string(b)
	} else if stat, err := os.Stdin.Stat(); err == nil && stat.Mode()&os.ModeCharDevice == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		body = string(b)
	}
	a, err := selectAccount(configPath(*cfgPath), *account)
	if err != nil {
		return err
	}
	mb, err := open(a)
	if err != nil {
		return err
	}
	if err := mb.Send(Outgoing{
		To: to, Cc: cc, Subject: *subject, Body: body,
		HTML: *html, Attach: attach, ReplyTo: *replyTo,
	}); err != nil {
		return err
	}
	printJSON(map[string]bool{"ok": true})
	return nil
}

func cmdMark(args []string) error {
	fs := flag.NewFlagSet("mark", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file")
	account := fs.String("account", "", "account name")
	read := fs.Bool("read", false, "mark as read")
	unread := fs.Bool("unread", false, "mark as unread")
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	if *read == *unread {
		return &usageError{fmt.Errorf("exactly one of --read or --unread is required")}
	}
	if fs.NArg() != 1 {
		return &usageError{fmt.Errorf("usage: mailctl mark <id> --read|--unread [--account NAME]")}
	}
	a, err := selectAccount(configPath(*cfgPath), *account)
	if err != nil {
		return err
	}
	mb, err := open(a)
	if err != nil {
		return err
	}
	if err := mb.Mark(fs.Arg(0), *read); err != nil {
		return err
	}
	printJSON(map[string]bool{"ok": true})
	return nil
}

func cmdMove(args []string) error {
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file")
	account := fs.String("account", "", "account name")
	to := fs.String("to", "", "destination folder")
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	if *to == "" {
		return &usageError{fmt.Errorf("--to FOLDER is required")}
	}
	if fs.NArg() != 1 {
		return &usageError{fmt.Errorf("usage: mailctl move <id> --to FOLDER [--account NAME]")}
	}
	a, err := selectAccount(configPath(*cfgPath), *account)
	if err != nil {
		return err
	}
	mb, err := open(a)
	if err != nil {
		return err
	}
	if err := mb.Move(fs.Arg(0), *to); err != nil {
		return err
	}
	printJSON(map[string]bool{"ok": true})
	return nil
}
