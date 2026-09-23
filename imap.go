package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

// imapMailbox talks IMAP + SMTP. One process = one command, so the connection
// is opened per call and closed when the call returns.
type imapMailbox struct {
	a Account
}

func newIMAP(a Account) (Mailbox, error) {
	return imapMailbox{a: a}, nil
}

// msgID is a parsed message id: "<uidvalidity>:<uid>:<folder>" (folder last,
// may contain ':').
type msgID struct {
	validity uint32
	uid      uint32
	folder   string
}

func parseID(id string) (msgID, error) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 {
		return msgID{}, fmt.Errorf("imap: invalid message id %q", id)
	}
	v, err1 := strconv.ParseUint(parts[0], 10, 32)
	u, err2 := strconv.ParseUint(parts[1], 10, 32)
	if err1 != nil || err2 != nil || parts[2] == "" {
		return msgID{}, fmt.Errorf("imap: invalid message id %q", id)
	}
	return msgID{validity: uint32(v), uid: uint32(u), folder: parts[2]}, nil
}

func formatID(validity uint32, uid imap.UID, folder string) string {
	return fmt.Sprintf("%d:%d:%s", validity, uid, folder)
}

// dial connects, logs in and returns the client. Logout is the caller's job.
func (m imapMailbox) dial() (*imapclient.Client, error) {
	pw, err := m.a.secret()
	if err != nil {
		return nil, err
	}
	var c *imapclient.Client
	if m.a.Insecure {
		c, err = imapclient.DialInsecure(m.a.IMAP, nil)
	} else {
		c, err = imapclient.DialTLS(m.a.IMAP, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("imap dial %s: %w", m.a.IMAP, err)
	}
	if err := c.Login(m.a.User, pw).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("imap login %s: %w", m.a.User, err)
	}
	return c, nil
}

// selectFolder SELECTs a folder and fails if the mailbox changed underneath us.
func selectFolder(c *imapclient.Client, folder string, validity uint32) error {
	data, err := c.Select(folder, nil).Wait()
	if err != nil {
		return fmt.Errorf("imap select %s: %w", folder, err)
	}
	if data.UIDValidity != validity {
		return fmt.Errorf("imap select %s: uid validity changed (was %d, now %d); re-run list", folder, validity, data.UIDValidity)
	}
	return nil
}

func (m imapMailbox) List(opts ListOpts) ([]Msg, error) {
	folder := opts.Folder
	if folder == "" {
		folder = "INBOX"
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	c, err := m.dial()
	if err != nil {
		return nil, err
	}
	defer func() { c.Logout().Wait() }()

	data, err := c.Select(folder, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap select %s: %w", folder, err)
	}
	validity := data.UIDValidity

	criteria := &imap.SearchCriteria{}
	if opts.Unread {
		criteria.NotFlag = append(criteria.NotFlag, imap.FlagSeen)
	}
	if !opts.Since.IsZero() {
		criteria.Since = opts.Since
	}
	sd, err := c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap search %s: %w", folder, err)
	}
	uids, _ := sd.All.(imap.UIDSet)
	nums, ok := uids.Nums()
	if !ok {
		return nil, fmt.Errorf("imap search %s: unexpected result", folder)
	}
	// Newest first, capped at limit.
	if len(nums) > limit {
		nums = nums[len(nums)-limit:]
	}
	for i, j := 0, len(nums)-1; i < j; i, j = i+1, j-1 {
		nums[i], nums[j] = nums[j], nums[i]
	}
	if len(nums) == 0 {
		return []Msg{}, nil
	}

	var set imap.UIDSet
	for _, u := range nums {
		set.AddNum(u)
	}
	fetch := c.Fetch(set, &imap.FetchOptions{
		Envelope:      true,
		Flags:         true,
		InternalDate:  true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{{Peek: true, Part: []int{1}}},
	})
	buffs, err := fetch.Collect()
	if err != nil {
		return nil, fmt.Errorf("imap fetch %s: %w", folder, err)
	}
	// The server may return fetch results in mailbox order, not set order.
	byUID := make(map[imap.UID]*imapclient.FetchMessageBuffer, len(buffs))
	for i := range buffs {
		byUID[buffs[i].UID] = buffs[i]
	}

	out := make([]Msg, 0, len(buffs))
	for _, u := range nums {
		buf := byUID[u]
		if buf == nil {
			continue
		}
		env := buf.Envelope
		if env == nil {
			continue
		}
		msg := Msg{
			ID:      formatID(validity, buf.UID, folder),
			Subject: env.Subject,
			Date:    env.Date,
		}
		if len(env.From) > 0 {
			msg.From = env.From[0].Addr()
		} else if len(env.Sender) > 0 {
			msg.From = env.Sender[0].Addr()
		}
		for _, a := range env.To {
			msg.To = append(msg.To, a.Addr())
		}
		msg.Unread = !hasFlag(buf.Flags, imap.FlagSeen)
		msg.HasAttachments = hasAttachments(buf.BodyStructure)
		// Part 1 is the plain text body for typical messages; decode its
		// transfer encoding and collapse whitespace for the snippet.
		if len(buf.BodySection) > 0 {
			enc := ""
			if sp, ok := partOne(buf.BodyStructure).(*imap.BodyStructureSinglePart); ok {
				enc = sp.Encoding
			}
			msg.Snippet = snippet(string(decodeTransfer(buf.BodySection[0].Bytes, enc)), 200)
		}
		out = append(out, msg)
	}
	return out, nil
}

func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, f := range flags {
		if strings.EqualFold(string(f), string(want)) {
			return true
		}
	}
	return false
}

// partOne returns the body structure child for part 1 (nil if absent).
func partOne(bs imap.BodyStructure) imap.BodyStructure {
	switch bs := bs.(type) {
	case *imap.BodyStructureSinglePart:
		return bs
	case *imap.BodyStructureMultiPart:
		if len(bs.Children) > 0 {
			return bs.Children[0]
		}
	}
	return nil
}

// hasAttachments reports whether any part looks like an attachment.
func hasAttachments(bs imap.BodyStructure) bool {
	if bs == nil {
		return false
	}
	found := false
	bs.Walk(func(_ []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		disp := sp.Disposition()
		if disp != nil && strings.EqualFold(disp.Value, "attachment") {
			found = true
			return false
		}
		if disp != nil && disp.Params["filename"] != "" {
			found = true
			return false
		}
		if _, ok := sp.Params["name"]; ok && !strings.HasPrefix(sp.MediaType(), "text/") {
			found = true
			return false
		}
		return true
	})
	return found
}

func decodeTransfer(b []byte, enc string) []byte {
	switch strings.ToLower(enc) {
	case "base64":
		clean := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, string(b))
		out, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return b
		}
		return out
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		if err != nil {
			return b
		}
		return out
	}
	return b
}

func snippet(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		r = r[:max]
	}
	return string(r)
}

// rawAttachment is an Attachment plus its content, before it is saved to disk.
type rawAttachment struct {
	Attachment
	data []byte
}

// parseMIME parses a raw RFC 5322 message via go-message/mail.
func parseMIME(raw []byte) (hdr mail.Header, headers map[string][]string, text, html string, atts []rawAttachment, err error) {
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return mail.Header{}, nil, "", "", nil, fmt.Errorf("parse message: %w", err)
	}
	headers = map[string][]string{}
	for fields := r.Header.Fields(); fields.Next(); {
		k := fields.Key()
		v, _ := fields.Text()
		headers[k] = append(headers[k], v)
	}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return mail.Header{}, nil, "", "", nil, fmt.Errorf("parse message part: %w", err)
		}
		body, err := io.ReadAll(p.Body)
		if err != nil {
			return mail.Header{}, nil, "", "", nil, fmt.Errorf("read message part: %w", err)
		}
		switch h := p.Header.(type) {
		case *mail.AttachmentHeader:
			ct, _, _ := h.ContentType()
			name, _ := h.Filename()
			atts = append(atts, rawAttachment{
				Attachment: Attachment{Name: name, Size: int64(len(body)), ContentType: ct},
				data:       body,
			})
		default:
			ct, _, _ := p.Header.(*mail.InlineHeader).ContentType()
			if strings.HasPrefix(ct, "text/html") {
				html = strings.TrimSpace(html + "\n" + string(body))
			} else {
				text = strings.TrimSpace(text + "\n" + string(body))
			}
		}
	}
	return r.Header, headers, text, html, atts, nil
}

// fetchMessage returns the raw message and its flags for an id, without
// marking it read.
func (m imapMailbox) fetchMessage(id string) ([]byte, []imap.Flag, error) {
	mi, err := parseID(id)
	if err != nil {
		return nil, nil, err
	}
	c, err := m.dial()
	if err != nil {
		return nil, nil, err
	}
	defer func() { c.Logout().Wait() }()
	if err := selectFolder(c, mi.folder, mi.validity); err != nil {
		return nil, nil, err
	}
	var set imap.UIDSet
	set.AddNum(imap.UID(mi.uid))
	sec := &imap.FetchItemBodySection{Peek: true}
	buffs, err := c.Fetch(set, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{sec},
		Flags:       true,
	}).Collect()
	if err != nil {
		return nil, nil, fmt.Errorf("imap fetch %d in %s: %w", mi.uid, mi.folder, err)
	}
	if len(buffs) == 0 || len(buffs[0].BodySection) == 0 {
		return nil, nil, fmt.Errorf("imap fetch %d in %s: message not found", mi.uid, mi.folder)
	}
	return buffs[0].BodySection[0].Bytes, buffs[0].Flags, nil
}

func (m imapMailbox) Get(id, saveDir string) (*Full, error) {
	raw, flags, err := m.fetchMessage(id)
	if err != nil {
		return nil, err
	}
	hdr, headers, text, html, atts, err := parseMIME(raw)
	if err != nil {
		return nil, err
	}
	if saveDir != "" {
		for i := range atts {
			p, err := saveAttachment(saveDir, atts[i].Name, atts[i].data)
			if err != nil {
				return nil, err
			}
			atts[i].Path = p
		}
	}
	full := &Full{
		Headers:     headers,
		Text:        text,
		HTML:        html,
		Attachments: make([]Attachment, len(atts)),
	}
	for i, a := range atts {
		full.Attachments[i] = a.Attachment
	}
	full.Msg = Msg{ID: id, Unread: !hasFlag(flags, imap.FlagSeen), HasAttachments: len(atts) > 0}
	if subj, err := hdr.Subject(); err == nil {
		full.Msg.Subject = subj
	}
	if from, err := hdr.AddressList("From"); err == nil && len(from) > 0 {
		full.Msg.From = from[0].Address
	}
	if to, err := hdr.AddressList("To"); err == nil {
		full.Msg.To = addrsToStrings(to)
	}
	if d, err := hdr.Date(); err == nil {
		full.Msg.Date = d
	}
	return full, nil
}

func (m imapMailbox) Mark(id string, read bool) error {
	mi, err := parseID(id)
	if err != nil {
		return err
	}
	c, err := m.dial()
	if err != nil {
		return err
	}
	defer func() { c.Logout().Wait() }()
	if err := selectFolder(c, mi.folder, mi.validity); err != nil {
		return err
	}
	var set imap.UIDSet
	set.AddNum(imap.UID(mi.uid))
	store := &imap.StoreFlags{Silent: true, Flags: []imap.Flag{imap.FlagSeen}}
	if read {
		store.Op = imap.StoreFlagsAdd
	} else {
		store.Op = imap.StoreFlagsDel
	}
	if err := c.Store(set, store, nil).Close(); err != nil {
		return fmt.Errorf("imap store %s: %w", mi.folder, err)
	}
	return nil
}

func (m imapMailbox) Move(id, folder string) error {
	mi, err := parseID(id)
	if err != nil {
		return err
	}
	c, err := m.dial()
	if err != nil {
		return err
	}
	defer func() { c.Logout().Wait() }()
	if err := selectFolder(c, mi.folder, mi.validity); err != nil {
		return err
	}
	var set imap.UIDSet
	set.AddNum(imap.UID(mi.uid))
	if _, err := c.Move(set, folder).Wait(); err != nil {
		return fmt.Errorf("imap move %s -> %s: %w", mi.folder, folder, err)
	}
	return nil
}

func (m imapMailbox) Send(o Outgoing) error {
	pw, err := m.a.secret()
	if err != nil {
		return err
	}

	var (
		subject   = o.Subject
		inReplyTo string
		refs      []string
	)
	if o.ReplyTo != "" {
		raw, _, err := m.fetchMessage(o.ReplyTo)
		if err != nil {
			return err
		}
		r, err := mail.CreateReader(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("parse reply target: %w", err)
		}
		id, _ := r.Header.MessageID()
		inReplyTo = id
		refs, _ = r.Header.MsgIDList("References")
		if id != "" {
			refs = append(refs, id)
		}
		if subject == "" {
			subject, _ = r.Header.Subject()
		}
		if !strings.HasPrefix(strings.ToLower(subject), "re:") && subject != "" {
			subject = "Re: " + subject
		}
		if len(o.To) == 0 {
			if addrs, err := r.Header.AddressList("Reply-To"); err == nil && len(addrs) > 0 {
				o.To = addrsToStrings(addrs)
			} else if addrs, err := r.Header.AddressList("From"); err == nil {
				o.To = addrsToStrings(addrs)
			}
		}
	}
	if subject == "" {
		subject = "(no subject)"
	}

	var buf bytes.Buffer
	h := mail.Header{}
	h.SetDate(time.Now())
	h.SetSubject(subject)
	h.SetAddressList("From", []*netmail.Address{{Address: m.a.User}})
	setAddressList(&h, "To", o.To)
	setAddressList(&h, "Cc", o.Cc)
	domain := m.a.User
	if i := strings.LastIndex(domain, "@"); i >= 0 {
		domain = domain[i+1:]
	}
	h.SetMessageID(randHex(12) + "@" + domain)
	if inReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{inReplyTo})
		h.SetMsgIDList("References", refs)
	}

	w, err := mail.CreateWriter(&buf, h)
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}
	iw, err := w.CreateInline()
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}
	ih := mail.InlineHeader{}
	if o.HTML {
		ih.SetContentType("text/html", map[string]string{"charset": "utf-8"})
	} else {
		ih.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	}
	bw, err := iw.CreatePart(ih)
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}
	if _, err := bw.Write([]byte(o.Body)); err != nil {
		return fmt.Errorf("build message: %w", err)
	}
	if err := bw.Close(); err != nil {
		return fmt.Errorf("build message: %w", err)
	}
	if err := iw.Close(); err != nil {
		return fmt.Errorf("build message: %w", err)
	}
	for _, f := range o.Attach {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read attachment %s: %w", f, err)
		}
		ah := mail.AttachmentHeader{}
		ct := mime.TypeByExtension(filepath.Ext(f))
		if ct == "" {
			ct = "application/octet-stream"
		}
		ah.SetContentType(ct, nil)
		ah.SetFilename(filepath.Base(f))
		aw, err := w.CreateAttachment(ah)
		if err != nil {
			return fmt.Errorf("build message: %w", err)
		}
		if _, err := aw.Write(data); err != nil {
			return fmt.Errorf("build message: %w", err)
		}
		if err := aw.Close(); err != nil {
			return fmt.Errorf("build message: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("build message: %w", err)
	}

	// SMTP: port 465 = implicit TLS, otherwise STARTTLS unless insecure.
	hostName, _, splitErr := net.SplitHostPort(m.a.SMTP)
	if splitErr != nil {
		return fmt.Errorf("smtp dial %s: %w", m.a.SMTP, splitErr)
	}
	tlsCfg := &tls.Config{ServerName: hostName}
	var cli *gosmtp.Client
	switch {
	case strings.HasSuffix(m.a.SMTP, ":465"):
		cli, err = gosmtp.DialTLS(m.a.SMTP, tlsCfg)
	case m.a.Insecure:
		cli, err = gosmtp.Dial(m.a.SMTP)
	default:
		cli, err = gosmtp.DialStartTLS(m.a.SMTP, tlsCfg)
	}
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", m.a.SMTP, err)
	}
	defer cli.Close()

	if err := cli.Auth(sasl.NewPlainClient("", m.a.User, pw)); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	if err := cli.Mail(m.a.User, nil); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	for _, rcpt := range append(append([]string{}, o.To...), o.Cc...) {
		if err := cli.Rcpt(rcpt, nil); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", rcpt, err)
		}
	}
	dc, err := cli.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := dc.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if err := dc.Close(); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	// The server accepted the mail at DATA close; a QUIT failure must not make
	// the agent retry and send it twice.
	_ = cli.Quit()

	// File the same bytes in the Sent folder, marked as seen. A failure here
	// must not fail the command: the mail was already sent, and returning an
	// error would make the agent retry and send it twice.
	if m.a.SentFolder != "" {
		if err := func() error {
			c, err := m.dial()
			if err != nil {
				return err
			}
			defer func() { c.Logout().Wait() }()
			sec := c.Append(m.a.SentFolder, int64(buf.Len()), &imap.AppendOptions{
				Flags: []imap.Flag{imap.FlagSeen},
			})
			if _, err := sec.Write(buf.Bytes()); err != nil {
				return err
			}
			if err := sec.Close(); err != nil {
				return err
			}
			_, err = sec.Wait()
			return err
		}(); err != nil {
			w, _ := json.Marshal(map[string]string{
				"warning": fmt.Sprintf("sent, but saving to %s failed: %v", m.a.SentFolder, err),
			})
			fmt.Fprintln(os.Stderr, string(w))
		}
	}
	return nil
}

func addrsToStrings(addrs []*netmail.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Address)
	}
	return out
}

func setAddressList(h *mail.Header, key string, addrs []string) {
	if len(addrs) == 0 {
		return
	}
	l := make([]*netmail.Address, 0, len(addrs))
	for _, s := range addrs {
		l = append(l, &netmail.Address{Address: s})
	}
	h.SetAddressList(key, l)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand should never fail
	}
	return hex.EncodeToString(b)
}
