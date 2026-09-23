package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

const testUser = "user@example.org"

// msgPlain is a simple text message; msgAttach carries a base64 attachment.
const msgPlain = "From: Alice <alice@example.org>\r\n" +
	"To: " + testUser + "\r\n" +
	"Subject: Hello there\r\n" +
	"Date: Mon, 01 Jan 2024 10:00:00 +0000\r\n" +
	"Message-ID: <m1@example.org>\r\n" +
	"\r\n" +
	"This is the plain text body of message one.\r\n"

const msgAttach = "From: Bob <bob@example.org>\r\n" +
	"To: " + testUser + "\r\n" +
	"Subject: Report\r\n" +
	"Date: Mon, 01 Jan 2024 11:00:00 +0000\r\n" +
	"Message-ID: <m2@example.org>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
	"\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Please find the report attached.\r\n" +
	"--BOUND\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--BOUND--\r\n"

// captureBackend is a go-smtp backend that records the last message.
type captureBackend struct {
	mu    sync.Mutex
	from  string
	rcpts []string
	data  []byte
}

func (b *captureBackend) NewSession(*gosmtp.Conn) (gosmtp.Session, error) { return b, nil }
func (b *captureBackend) Mail(from string, _ *gosmtp.MailOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.from = from
	return nil
}
func (b *captureBackend) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rcpts = append(b.rcpts, to)
	return nil
}
func (b *captureBackend) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = data
	return err
}
func (b *captureBackend) Reset()                   {}
func (b *captureBackend) Logout() error            { return nil }
func (b *captureBackend) AuthMechanisms() []string { return []string{"PLAIN"} }
func (b *captureBackend) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if username == testUser && password == "pass" {
			return nil
		}
		return errors.New("invalid credentials")
	}), nil
}

type testEnv struct {
	account Account
	smtp    *captureBackend
}

func startTest(t *testing.T) testEnv {
	t.Helper()

	// IMAP: in-memory server, plaintext.
	memSrv := imapmemserver.New()
	memSrv.AddUser(imapmemserver.NewUser(testUser, "pass"))
	imapSrv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memSrv.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	imapLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go imapSrv.Serve(imapLn)
	t.Cleanup(func() { imapSrv.Close() })

	// SMTP: capture backend, plaintext.
	be := &captureBackend{}
	smtpSrv := gosmtp.NewServer(be)
	smtpSrv.AllowInsecureAuth = true
	smtpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go smtpSrv.Serve(smtpLn)
	t.Cleanup(func() { smtpSrv.Close() })

	// Provision mailboxes and seed messages through a real client.
	c, err := imapclient.DialInsecure(imapLn.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(testUser, "pass").Wait(); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"INBOX", "Sent", "Archive"} {
		if err := c.Create(folder, nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{msgPlain, msgAttach} {
		appendMsg(t, c, "INBOX", raw, false)
	}
	// A copy of the plain message in Sent, so Send's APPEND is distinguishable.
	appendMsg(t, c, "Sent", msgPlain, true)
	if err := c.Logout().Wait(); err != nil {
		t.Fatal(err)
	}

	return testEnv{
		account: Account{
			Name: "test", Type: "imap",
			IMAP:       imapLn.Addr().String(),
			SMTP:       smtpLn.Addr().String(),
			User:       testUser,
			Password:   "pass",
			SentFolder: "Sent",
			Insecure:   true,
		},
		smtp: be,
	}
}

func appendMsg(t *testing.T, c *imapclient.Client, folder, raw string, seen bool) {
	t.Helper()
	var flags []imap.Flag
	if seen {
		flags = append(flags, imap.FlagSeen)
	}
	ac := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: flags})
	if _, err := ac.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestList(t *testing.T) {
	env := startTest(t)
	mb, err := newIMAP(env.account)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := mb.List(ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d: %+v", len(msgs), msgs)
	}
	// Newest first.
	if msgs[0].Subject != "Report" || msgs[1].Subject != "Hello there" {
		t.Errorf("wrong order: %q, %q", msgs[0].Subject, msgs[1].Subject)
	}
	if msgs[0].From != "bob@example.org" {
		t.Errorf("from = %q", msgs[0].From)
	}
	if !msgs[0].HasAttachments {
		t.Error("Report should have attachments")
	}
	if msgs[1].HasAttachments {
		t.Error("Hello there should not have attachments")
	}
	if !msgs[0].Unread || !msgs[1].Unread {
		t.Error("both messages should be unread")
	}
	if !strings.Contains(msgs[1].Snippet, "plain text body") {
		t.Errorf("snippet = %q", msgs[1].Snippet)
	}
	if !strings.HasPrefix(msgs[0].ID, "1:") || !strings.HasSuffix(msgs[0].ID, ":INBOX") {
		t.Errorf("id format: %q", msgs[0].ID)
	}

	// Unread filter: both are unread now, so still 2.
	unread, err := mb.List(ListOpts{Unread: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 2 {
		t.Fatalf("unread: want 2, got %d", len(unread))
	}
}

func TestGetAndMark(t *testing.T) {
	env := startTest(t)
	mb, err := newIMAP(env.account)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := mb.List(ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	full, err := mb.Get(msgs[0].ID, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.Text, "Please find the report attached.") {
		t.Errorf("text = %q", full.Text)
	}
	if len(full.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(full.Attachments))
	}
	att := full.Attachments[0]
	if att.Name != "report.pdf" {
		t.Errorf("attachment name = %q", att.Name)
	}
	if att.ContentType != "application/pdf" {
		t.Errorf("content type = %q", att.ContentType)
	}
	if att.Path == "" {
		t.Fatal("attachment path not set")
	}
	data, err := os.ReadFile(att.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-1.4")) {
		t.Errorf("attachment not decoded: %q", data[:20])
	}

	// Get must not mark the message as read.
	unread, err := mb.List(ListOpts{Unread: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 2 {
		t.Fatalf("get marked read: %d unread left", len(unread))
	}

	// Mark read; the unread list shrinks to 1.
	if err := mb.Mark(msgs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	unread, err = mb.List(ListOpts{Unread: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 1 || unread[0].Subject != "Hello there" {
		t.Fatalf("after mark: %+v", unread)
	}
	// And unread again.
	if err := mb.Mark(msgs[0].ID, false); err != nil {
		t.Fatal(err)
	}
	unread, err = mb.List(ListOpts{Unread: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 2 {
		t.Fatalf("after unmark: %d unread", len(unread))
	}
}

func TestMove(t *testing.T) {
	env := startTest(t)
	mb, err := newIMAP(env.account)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := mb.List(ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	id := msgs[1].ID // "Hello there"
	if err := mb.Move(id, "Archive"); err != nil {
		t.Fatal(err)
	}
	inInbox, err := mb.List(ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(inInbox) != 1 || inInbox[0].Subject != "Report" {
		t.Fatalf("inbox after move: %+v", inInbox)
	}
	archived, err := mb.List(ListOpts{Folder: "Archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || !strings.HasSuffix(archived[0].ID, ":Archive") || archived[0].Subject != "Hello there" {
		t.Fatalf("archive after move: %+v", archived)
	}
}

func TestSend(t *testing.T) {
	env := startTest(t)
	mb, err := newIMAP(env.account)
	if err != nil {
		t.Fatal(err)
	}
	err = mb.Send(Outgoing{
		To:      []string{"dest@example.net"},
		Cc:      []string{"cc@example.net"},
		Subject: "Test mail",
		Body:    "hi from the test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Captured by the SMTP server.
	if got := env.smtp.data; !bytes.Contains(got, []byte("hi from the test")) {
		t.Errorf("smtp data = %q", got)
	}
	if env.smtp.from != testUser {
		t.Errorf("smtp from = %q", env.smtp.from)
	}
	want := []string{"dest@example.net", "cc@example.net"}
	if strings.Join(env.smtp.rcpts, ",") != strings.Join(want, ",") {
		t.Errorf("rcpts = %v", env.smtp.rcpts)
	}
	if !strings.Contains(strings.ToLower(string(env.smtp.data)), "message-id: <") {
		t.Errorf("no Message-ID in %q", env.smtp.data)
	}
	// Appended to Sent, marked as seen.
	sent, err := mb.List(ListOpts{Folder: "Sent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("sent folder: %+v", sent)
	}
	if sent[0].Unread {
		t.Error("appended message should be \\Seen")
	}
	if sent[0].Subject != "Test mail" {
		t.Errorf("sent subject = %q", sent[0].Subject)
	}
}

func TestSendReply(t *testing.T) {
	env := startTest(t)
	mb, err := newIMAP(env.account)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := mb.List(ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// Reply to "Report" without --to: To should default to the original From.
	err = mb.Send(Outgoing{ReplyTo: msgs[0].ID, Body: "here you go"})
	if err != nil {
		t.Fatal(err)
	}
	data := env.smtp.data
	if !strings.Contains(string(data), "In-Reply-To: <m2@example.org>") {
		t.Errorf("In-Reply-To missing: %q", data)
	}
	if !strings.Contains(string(data), "References: <m2@example.org>") {
		t.Errorf("References missing: %q", data)
	}
	if !strings.Contains(string(data), "Subject: Re: Report") {
		t.Errorf("subject not Re-prefixed: %q", data)
	}
	if !bytes.Contains(data, []byte("To: <bob@example.org>")) {
		t.Errorf("To not defaulted to original From: %q", data)
	}
}

// Compile-time interface check plus a small validation of id round-tripping
// for folders containing ':'.
func TestIDRoundTrip(t *testing.T) {
	id := formatID(7, 42, "Sub/Folder:with:colons")
	mi, err := parseID(id)
	if err != nil {
		t.Fatal(err)
	}
	if mi.validity != 7 || mi.uid != 42 || mi.folder != "Sub/Folder:with:colons" {
		t.Fatalf("round trip: %+v", mi)
	}
	if _, err := parseID("nonsense"); err == nil {
		t.Fatal("expected error for malformed id")
	}
}
