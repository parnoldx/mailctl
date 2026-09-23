package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var graphBase = "https://graph.microsoft.com/v1.0"
var tokenURL = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"

// graph implements Mailbox via Microsoft Graph, app-only (client credentials).
type graph struct {
	acc  Account
	tok  string
	http *http.Client
}

func newGraph(a Account) (Mailbox, error) {
	secret, err := a.secret()
	if err != nil {
		return nil, err
	}
	tok, err := fetchToken(a.Tenant, a.ClientID, secret)
	if err != nil {
		return nil, fmt.Errorf("graph auth: %w", err)
	}
	return &graph{acc: a, tok: tok, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

func fetchToken(tenant, clientID, secret string) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", secret)
	form.Set("scope", "https://graph.microsoft.com/.default")
	form.Set("grant_type", "client_credentials")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Post(
		fmt.Sprintf(tokenURL, url.PathEscape(tenant)),
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", graphErr(resp.Status, data)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("token endpoint: missing access_token")
	}
	return tr.AccessToken, nil
}

// userPath prefixes a Graph path with /users/{mailbox}.
func (g *graph) userPath(p string) string {
	return "/users/" + url.PathEscape(g.acc.Mailbox) + p
}

// do issues an authenticated Graph request. Extra args are sent as Prefer headers.
func (g *graph) do(method, path string, body, out any, prefer ...string) error {
	hdr := make(map[string]string, len(prefer))
	for _, p := range prefer {
		hdr["Prefer"] = p // at most one Prefer per call
	}
	return g.doReq(method, graphBase+path, body, hdr, true, out)
}

// doReq performs one HTTP request; auth=false omits the bearer header (upload PUTs).
func (g *graph) doReq(method, full string, body any, hdr map[string]string, auth bool, out any) error {
	var payload []byte
	isJSON := false
	switch b := body.(type) {
	case nil:
	case []byte:
		payload = b
	default:
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
		isJSON = true
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, full, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if isJSON {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if auth {
			req.Header.Set("Authorization", "Bearer "+g.tok)
		}
		resp, err := g.http.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, full, err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(data) == 0 {
				return nil
			}
			return json.Unmarshal(data, out)
		}
		if (resp.StatusCode == 429 || resp.StatusCode == 503) && attempt == 0 {
			wait := 5 * time.Second
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra >= 0 {
				wait = time.Duration(ra) * time.Second
			}
			time.Sleep(wait)
			continue
		}
		return graphErr(resp.Status, data)
	}
}

func graphErr(status string, body []byte) error {
	var ge struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(body, &ge)
	if ge.Error.Code != "" || ge.Error.Message != "" {
		return fmt.Errorf("graph %s: %s: %s", status, ge.Error.Code, ge.Error.Message)
	}
	return fmt.Errorf("graph %s: %s", status, strings.TrimSpace(string(body)))
}

// ---- JSON wire types ----

type emailAddress struct {
	Address string `json:"address"`
}

type gRecip struct {
	EmailAddress emailAddress `json:"emailAddress"`
}

type gBody struct {
	ContentType string `json:"contentType"` // "Text" | "HTML"
	Content     string `json:"content"`
}

type gMessage struct {
	ID               string    `json:"id"`
	Subject          string    `json:"subject"`
	ReceivedDateTime time.Time `json:"receivedDateTime"`
	BodyPreview      string    `json:"bodyPreview"`
	IsRead           bool      `json:"isRead"`
	HasAttachments   bool      `json:"hasAttachments"`
	From             *struct {
		EmailAddress emailAddress `json:"emailAddress"`
	} `json:"from"`
	ToRecipients           []gRecip `json:"toRecipients"`
	Body                   *gBody   `json:"body"`
	InternetMessageHeaders []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"internetMessageHeaders"`
}

func (m *gMessage) toMsg() Msg {
	msg := Msg{
		ID: m.ID, Subject: m.Subject, Date: m.ReceivedDateTime,
		Snippet: m.BodyPreview, Unread: !m.IsRead, HasAttachments: m.HasAttachments,
	}
	if m.From != nil {
		msg.From = m.From.EmailAddress.Address
	}
	for _, r := range m.ToRecipients {
		msg.To = append(msg.To, r.EmailAddress.Address)
	}
	return msg
}

type gAttachment struct {
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	Size         int64  `json:"size"`
	ContentBytes []byte `json:"contentBytes"` // json decodes base64
}

// ---- folders ----

var wellKnownFolders = map[string]bool{
	"inbox": true, "drafts": true, "sentitems": true, "deleteditems": true,
	"archive": true, "junkemail": true, "outbox": true,
}

// folderID maps "" / well-known names / display names to a folder id path segment.
func (g *graph) folderID(name string) (string, error) {
	if name == "" {
		return "inbox", nil
	}
	low := strings.ToLower(name)
	if wellKnownFolders[low] {
		return low, nil
	}
	q := url.Values{}
	q.Set("$filter", fmt.Sprintf("displayName eq '%s'", strings.ReplaceAll(name, "'", "''")))
	q.Set("$select", "id")
	var res struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := g.do("GET", g.userPath("/mailFolders?")+q.Encode(), nil, &res); err != nil {
		return "", err
	}
	if len(res.Value) == 0 {
		return "", fmt.Errorf("folder %q not found", name)
	}
	return res.Value[0].ID, nil
}

// ---- Mailbox ----

func (g *graph) List(o ListOpts) ([]Msg, error) {
	fid, err := g.folderID(o.Folder)
	if err != nil {
		return nil, err
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	q.Set("$top", strconv.Itoa(limit))
	q.Set("$orderby", "receivedDateTime desc")
	q.Set("$select", "id,from,toRecipients,subject,receivedDateTime,bodyPreview,isRead,hasAttachments")
	var filters []string
	if o.Unread {
		filters = append(filters, "isRead eq false")
	}
	if !o.Since.IsZero() {
		filters = append(filters, "receivedDateTime ge "+o.Since.Format(time.RFC3339))
	}
	if len(filters) > 0 {
		q.Set("$filter", strings.Join(filters, " and "))
	}
	var res struct {
		Value []gMessage `json:"value"`
	}
	if err := g.do("GET", g.userPath("/mailFolders/"+url.PathEscape(fid)+"/messages?")+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	out := make([]Msg, 0, len(res.Value))
	for i := range res.Value {
		out = append(out, res.Value[i].toMsg())
	}
	return out, nil
}

func (g *graph) Get(id, saveDir string) (*Full, error) {
	q := url.Values{}
	q.Set("$select", "id,from,toRecipients,subject,receivedDateTime,bodyPreview,isRead,hasAttachments,body,internetMessageHeaders")
	msgPath := g.userPath("/messages/" + url.PathEscape(id) + "?" + q.Encode())
	var htmlMsg, textMsg gMessage
	if err := g.do("GET", msgPath, nil, &htmlMsg, `outlook.body-content-type="html"`); err != nil {
		return nil, err
	}
	if err := g.do("GET", msgPath, nil, &textMsg, `outlook.body-content-type="text"`); err != nil {
		return nil, err
	}
	full := &Full{Msg: htmlMsg.toMsg(), Headers: map[string][]string{}}
	if htmlMsg.Body != nil {
		full.HTML = htmlMsg.Body.Content
	}
	if textMsg.Body != nil {
		full.Text = textMsg.Body.Content
	}
	for _, h := range htmlMsg.InternetMessageHeaders {
		full.Headers[h.Name] = append(full.Headers[h.Name], h.Value)
	}
	var ares struct {
		Value []gAttachment `json:"value"`
	}
	if err := g.do("GET", g.userPath("/messages/"+url.PathEscape(id)+"/attachments"), nil, &ares); err != nil {
		return nil, err
	}
	for _, at := range ares.Value {
		att := Attachment{Name: at.Name, Size: at.Size, ContentType: at.ContentType}
		if len(at.ContentBytes) > 0 {
			att.Size = int64(len(at.ContentBytes))
			if saveDir != "" {
				p, err := saveAttachment(saveDir, at.Name, at.ContentBytes)
				if err != nil {
					return nil, err
				}
				att.Path = p
			}
		}
		full.Attachments = append(full.Attachments, att)
	}
	return full, nil
}

// saveAttachment writes data under dir with a sanitized basename and no path traversal.
func saveAttachment(dir, name string, data []byte) (string, error) {
	base := filepath.Base(name)
	if base == "." || base == ".." || base == string(filepath.Separator) || base == "" {
		base = "attachment"
	}
	path := filepath.Join(dir, base)
	for i := 1; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(base)
		path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(base, ext), i, ext))
	}
	return path, os.WriteFile(path, data, 0o600)
}

func recips(addrs []string) []gRecip {
	out := make([]gRecip, 0, len(addrs))
	for _, a := range addrs {
		if p, err := mail.ParseAddress(a); err == nil {
			a = p.Address
		}
		out = append(out, gRecip{EmailAddress: emailAddress{Address: a}})
	}
	return out
}

func bodyType(html bool) string {
	if html {
		return "HTML"
	}
	return "Text"
}

func (g *graph) Send(o Outgoing) error {
	var draft gMessage
	if o.ReplyTo == "" {
		req := gNewMessage{
			Subject: o.Subject, Body: gBody{ContentType: bodyType(o.HTML), Content: o.Body},
			ToRecipients: recips(o.To), CcRecipients: recips(o.Cc),
		}
		if err := g.do("POST", g.userPath("/messages"), req, &draft); err != nil {
			return err
		}
	} else {
		if err := g.do("POST", g.userPath("/messages/"+url.PathEscape(o.ReplyTo)+"/createReply"), nil, &draft); err != nil {
			return err
		}
	}
	fail := func(err error) error {
		g.cleanupDraft(draft.ID)
		return err
	}
	if o.ReplyTo != "" {
		// Prepend the reply text above the quoted original in the draft body.
		existing, ct := "", "Text"
		if draft.Body != nil {
			existing, ct = draft.Body.Content, draft.Body.ContentType
		}
		sep := "\n"
		if ct == "HTML" {
			sep = "<br>"
		}
		patch := gPatchMessage{Body: &gBody{ContentType: ct, Content: o.Body + sep + existing}}
		if len(o.To) > 0 {
			patch.ToRecipients = recips(o.To)
		}
		if len(o.Cc) > 0 {
			patch.CcRecipients = recips(o.Cc)
		}
		if o.Subject != "" {
			patch.Subject = &o.Subject
		}
		if err := g.do("PATCH", g.userPath("/messages/"+url.PathEscape(draft.ID)), patch, nil); err != nil {
			return fail(err)
		}
	}
	if err := g.sendAttachments(draft.ID, o.Attach); err != nil {
		return fail(err)
	}
	if err := g.do("POST", g.userPath("/messages/"+url.PathEscape(draft.ID)+"/send"), nil, nil); err != nil {
		return fail(err)
	}
	return nil
}

func (g *graph) cleanupDraft(id string) {
	_ = g.do("DELETE", g.userPath("/messages/"+url.PathEscape(id)), nil, nil)
}

type gNewMessage struct {
	Subject      string   `json:"subject"`
	Body         gBody    `json:"body"`
	ToRecipients []gRecip `json:"toRecipients,omitempty"`
	CcRecipients []gRecip `json:"ccRecipients,omitempty"`
}

type gPatchMessage struct {
	Subject      *string  `json:"subject,omitempty"`
	Body         *gBody   `json:"body,omitempty"`
	ToRecipients []gRecip `json:"toRecipients,omitempty"`
	CcRecipients []gRecip `json:"ccRecipients,omitempty"`
}

// sendAttachments attaches files to a draft: small inline, large via upload session.
func (g *graph) sendAttachments(draftID string, files []string) error {
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		name := filepath.Base(f)
		ct := mime.TypeByExtension(filepath.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		if len(data) <= 3*1024*1024 {
			body := map[string]any{
				"@odata.type":  "#microsoft.graph.fileAttachment",
				"name":         name,
				"contentType":  ct,
				"contentBytes": base64.StdEncoding.EncodeToString(data),
			}
			if err := g.do("POST", g.userPath("/messages/"+url.PathEscape(draftID)+"/attachments"), body, nil); err != nil {
				return err
			}
			continue
		}
		var sess struct {
			UploadURL string `json:"uploadUrl"`
		}
		req := map[string]any{"AttachmentItem": map[string]any{
			"attachmentType": "file", "name": name, "contentType": ct, "size": len(data),
		}}
		if err := g.do("POST", g.userPath("/messages/"+url.PathEscape(draftID)+"/attachments/createUploadSession"), req, &sess); err != nil {
			return err
		}
		// ponytail: single-threaded chunk loop; parallelize if uploads of 100+ MB matter
		const chunk = 320 * 1024 * 12 // 3.75 MiB: multiple of 320 KiB, <= 4 MiB
		for start := 0; start < len(data); start += chunk {
			end := min(start+chunk, len(data))
			hdr := map[string]string{
				"Content-Range": fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)),
				"Content-Type":  "application/octet-stream",
			}
			if err := g.doReq("PUT", sess.UploadURL, data[start:end], hdr, false, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *graph) Mark(id string, read bool) error {
	return g.do("PATCH", g.userPath("/messages/"+url.PathEscape(id)), map[string]bool{"isRead": read}, nil)
}

func (g *graph) Move(id, folder string) error {
	dst, err := g.folderID(folder)
	if err != nil {
		return err
	}
	return g.do("POST", g.userPath("/messages/"+url.PathEscape(id)+"/move"), map[string]string{"destinationId": dst}, nil)
}
