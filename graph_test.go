package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testMailbox = "support@example.com" // url-escaped to support%40example.com on the wire

func writeJSON(t *testing.T, w http.ResponseWriter, code int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("write json: %v", err)
	}
}

// setupGraph starts a fake token+Graph server, overrides graphBase/tokenURL,
// and returns an authenticated *graph.
func setupGraph(t *testing.T, mux *http.ServeMux, tokenForm *url.Values) *graph {
	t.Helper()
	mux.HandleFunc("POST /token/ten", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if tokenForm != nil {
			*tokenForm = r.PostForm
		}
		writeJSON(t, w, 200, map[string]string{"access_token": "T0K", "token_type": "Bearer"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldBase, oldTok := graphBase, tokenURL
	graphBase, tokenURL = srv.URL+"/v1.0", srv.URL+"/token/%s"
	t.Cleanup(func() { graphBase, tokenURL = oldBase, oldTok })
	g, err := newGraph(Account{Name: "test", Type: "graph", Tenant: "ten", ClientID: "cid", ClientSecret: "csec", Mailbox: testMailbox})
	if err != nil {
		t.Fatalf("newGraph: %v", err)
	}
	return g.(*graph)
}

func checkBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer T0K" {
		t.Errorf("Authorization = %q, want Bearer T0K", got)
	}
}

func TestGraphTokenForm(t *testing.T) {
	var form url.Values
	setupGraph(t, http.NewServeMux(), &form)
	want := url.Values{
		"client_id":     {"cid"},
		"client_secret": {"csec"},
		"scope":         {"https://graph.microsoft.com/.default"},
		"grant_type":    {"client_credentials"},
	}
	for k, v := range want {
		if got := form.Get(k); got != v[0] {
			t.Errorf("token form %s = %q, want %q", k, got, v[0])
		}
	}
}

func TestGraphList(t *testing.T) {
	mux := http.NewServeMux()
	var gotQ url.Values
	mux.HandleFunc("GET /v1.0/users/"+url.PathEscape(testMailbox)+"/mailFolders/inbox/messages", func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotQ = r.URL.Query()
		writeJSON(t, w, 200, map[string]any{"value": []map[string]any{{
			"id": "M1", "subject": "Hi", "receivedDateTime": "2026-09-01T10:00:00Z",
			"bodyPreview": "prev", "isRead": false, "hasAttachments": true,
			"from":         map[string]any{"emailAddress": map[string]any{"address": "a@b.c"}},
			"toRecipients": []map[string]any{{"emailAddress": map[string]any{"address": "d@e.f"}}},
		}}})
	})
	g := setupGraph(t, mux, nil)
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	msgs, err := g.List(ListOpts{Unread: true, Since: since, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := gotQ.Get("$top"); got != "10" {
		t.Errorf("$top = %q", got)
	}
	if got := gotQ.Get("$orderby"); got != "receivedDateTime desc" {
		t.Errorf("$orderby = %q", got)
	}
	if got := gotQ.Get("$filter"); got != "isRead eq false and receivedDateTime ge 2026-08-01T00:00:00Z" {
		t.Errorf("$filter = %q", got)
	}
	if !strings.Contains(gotQ.Get("$select"), "bodyPreview") {
		t.Errorf("$select = %q", gotQ.Get("$select"))
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs", len(msgs))
	}
	m := msgs[0]
	if m.ID != "M1" || m.From != "a@b.c" || len(m.To) != 1 || m.To[0] != "d@e.f" ||
		m.Subject != "Hi" || m.Snippet != "prev" || !m.Unread || !m.HasAttachments {
		t.Errorf("bad msg: %+v", m)
	}
	if m.Date.UTC() != time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC) {
		t.Errorf("date = %v", m.Date)
	}
}

func TestGraphListResolvesDisplayName(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1.0/users/"+url.PathEscape(testMailbox)+"/mailFolders", func(w http.ResponseWriter, r *http.Request) {
		if f := r.URL.Query().Get("$filter"); f != "displayName eq 'Custom Stu'''" {
			t.Errorf("$filter = %q", f)
		}
		writeJSON(t, w, 200, map[string]any{"value": []map[string]any{{"id": "FID123"}}})
	})
	mux.HandleFunc("GET /v1.0/users/"+url.PathEscape(testMailbox)+"/mailFolders/FID123/messages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]any{"value": []any{}})
	})
	g := setupGraph(t, mux, nil)
	if _, err := g.List(ListOpts{Folder: "Custom Stu'"}); err != nil {
		t.Fatalf("List custom folder: %v", err)
	}
}

func TestGraphGet(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	msgPath := "GET /v1.0/users/" + url.PathEscape(testMailbox) + "/messages/M1"
	mux.HandleFunc(msgPath, func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Query().Get("$select") == "" {
			t.Error("missing $select")
		}
		content, ct := "text body", "Text"
		if strings.Contains(r.Header.Get("Prefer"), `outlook.body-content-type="html"`) {
			content, ct = "<b>html body</b>", "HTML"
		}
		writeJSON(t, w, 200, map[string]any{
			"id": "M1", "subject": "Sub", "receivedDateTime": "2026-09-01T10:00:00Z",
			"from":         map[string]any{"emailAddress": map[string]any{"address": "a@b.c"}},
			"toRecipients": []map[string]any{{"emailAddress": map[string]any{"address": "d@e.f"}}},
			"body":         map[string]any{"contentType": ct, "content": content},
			"internetMessageHeaders": []map[string]any{
				{"name": "From", "value": "a@b.c"}, {"name": "Thread-Index", "value": "xyz"},
			},
		})
	})
	mux.HandleFunc("GET /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/M1/attachments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]any{"value": []map[string]any{
			{"@odata.type": "#microsoft.graph.fileAttachment", "name": "../../x.txt",
				"contentType": "text/plain", "size": 5, "contentBytes": base64.StdEncoding.EncodeToString([]byte("hello"))},
			{"@odata.type": "#microsoft.graph.referenceAttachment", "name": "link.att", "size": 0},
		}})
	})
	g := setupGraph(t, mux, nil)
	full, err := g.Get("M1", dir)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if full.HTML != "<b>html body</b>" || full.Text != "text body" {
		t.Errorf("bodies: html=%q text=%q", full.HTML, full.Text)
	}
	if full.Headers["From"][0] != "a@b.c" || full.Headers["Thread-Index"][0] != "xyz" {
		t.Errorf("headers: %v", full.Headers)
	}
	if len(full.Attachments) != 2 {
		t.Fatalf("got %d attachments", len(full.Attachments))
	}
	a := full.Attachments[0]
	if a.Name != "../../x.txt" {
		t.Errorf("attachment name should be preserved as reported: %q", a.Name)
	}
	if a.Path == "" {
		t.Fatal("fileAttachment not saved")
	}
	if filepath.Base(a.Path) != "x.txt" {
		t.Errorf("traversal not sanitized: %q", a.Path)
	}
	b, err := os.ReadFile(a.Path)
	if err != nil || string(b) != "hello" {
		t.Errorf("saved file: %q %v", b, err)
	}
	if full.Attachments[1].Path != "" {
		t.Errorf("referenceAttachment should not be saved: %q", full.Attachments[1].Path)
	}
}

func TestGraphGetSanitizesDotDotName(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/M1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]any{"id": "M1", "body": map[string]any{"contentType": "Text", "content": "x"}})
	})
	mux.HandleFunc("GET /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/M1/attachments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]any{"value": []map[string]any{
			{"@odata.type": "#microsoft.graph.fileAttachment", "name": "..",
				"contentBytes": base64.StdEncoding.EncodeToString([]byte("data"))},
		}})
	})
	g := setupGraph(t, mux, nil)
	full, err := g.Get("M1", dir)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p := full.Attachments[0].Path; p == "" || filepath.Base(p) == ".." || !strings.HasPrefix(p, dir) {
		t.Errorf("unsafe path %q", p)
	}
}

func TestGraphSendNewSmallAttachment(t *testing.T) {
	mux := http.NewServeMux()
	var order []string
	newBody := map[string]any{}
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages", func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		order = append(order, "draft")
		if err := json.NewDecoder(r.Body).Decode(&newBody); err != nil {
			t.Errorf("decode draft: %v", err)
		}
		writeJSON(t, w, 201, map[string]any{"id": "D1"})
	})
	var attachBody map[string]any
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/D1/attachments", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "attach")
		if err := json.NewDecoder(r.Body).Decode(&attachBody); err != nil {
			t.Errorf("decode attachment: %v", err)
		}
		writeJSON(t, w, 201, map[string]any{"id": "A1"})
	})
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/D1/send", func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "send")
		w.WriteHeader(202)
	})
	g := setupGraph(t, mux, nil)
	f := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(f, []byte("file-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.Send(Outgoing{
		To: []string{"x@y.z"}, Cc: []string{"c@d.e"}, Subject: "S", Body: "B", Attach: []string{f},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.Join(order, ",") != "draft,attach,send" {
		t.Errorf("order = %v", order)
	}
	if newBody["subject"] != "S" {
		t.Errorf("subject = %v", newBody["subject"])
	}
	body := newBody["body"].(map[string]any)
	if body["contentType"] != "Text" || body["content"] != "B" {
		t.Errorf("body = %v", body)
	}
	tos := newBody["toRecipients"].([]any)
	if tos[0].(map[string]any)["emailAddress"].(map[string]any)["address"] != "x@y.z" {
		t.Errorf("toRecipients = %v", newBody["toRecipients"])
	}
	if attachBody["@odata.type"] != "#microsoft.graph.fileAttachment" || attachBody["name"] != "note.txt" {
		t.Errorf("attachment = %v", attachBody)
	}
	got, err := base64.StdEncoding.DecodeString(attachBody["contentBytes"].(string))
	if err != nil || string(got) != "file-content" {
		t.Errorf("contentBytes = %q %v", got, err)
	}
}

func TestGraphSendLargeAttachmentUploadSession(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldBase, oldTok := graphBase, tokenURL
	graphBase, tokenURL = srv.URL+"/v1.0", srv.URL+"/token/%s"
	t.Cleanup(func() { graphBase, tokenURL = oldBase, oldTok })

	big := bytes.Repeat([]byte("0123456789"), 320*1024*13) // 4.16 MB: spans two chunks
	f := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(f, big, 0o600); err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("POST /token/ten", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 200, map[string]string{"access_token": "T0K"})
	})
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 201, map[string]any{"id": "D1"})
	})
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/D1/attachments/createUploadSession", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode session req: %v", err)
		}
		item := req["AttachmentItem"].(map[string]any)
		if item["size"].(float64) != float64(len(big)) || item["name"] != "big.bin" {
			t.Errorf("AttachmentItem = %v", item)
		}
		writeJSON(t, w, 200, map[string]string{"uploadUrl": srv.URL + "/upload/s1"})
	})
	var uploaded bytes.Buffer
	var ranges []string
	mux.HandleFunc("PUT /upload/s1", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("upload PUT must not carry the Authorization header")
		}
		ranges = append(ranges, r.Header.Get("Content-Range"))
		if _, err := io.Copy(&uploaded, r.Body); err != nil {
			t.Errorf("copy chunk: %v", err)
		}
		writeJSON(t, w, 201, map[string]any{"id": "A1"})
	})
	var sendCalled bool
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/D1/send", func(w http.ResponseWriter, r *http.Request) {
		sendCalled = true
		w.WriteHeader(202)
	})
	g, err := newGraph(Account{Name: "test", Type: "graph", Tenant: "ten", ClientID: "cid", ClientSecret: "csec", Mailbox: testMailbox})
	if err != nil {
		t.Fatalf("newGraph: %v", err)
	}
	if err := g.(*graph).Send(Outgoing{To: []string{"x@y.z"}, Subject: "S", Body: "B", Attach: []string{f}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Equal(uploaded.Bytes(), big) {
		t.Errorf("uploaded %d bytes, want %d", uploaded.Len(), len(big))
	}
	if !sendCalled {
		t.Error("send not called")
	}
	if len(ranges) != 11 || ranges[0] != fmt.Sprintf("bytes 0-%d/%d", 320*1024*12-1, len(big)) {
		t.Errorf("Content-Range headers = %v", ranges)
	}
}

func TestGraphSendReply(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/MSG1/createReply", func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		writeJSON(t, w, 201, map[string]any{"id": "D1", "body": map[string]any{"contentType": "Text", "content": "quoted original\n"}})
	})
	var patch map[string]any
	mux.HandleFunc("PATCH /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/D1", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Errorf("decode patch: %v", err)
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/D1/send", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
	})
	g := setupGraph(t, mux, nil)
	if err := g.Send(Outgoing{ReplyTo: "MSG1", Body: "my reply", To: []string{"new@to.z"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	body := patch["body"].(map[string]any)
	if body["content"] != "my reply\nquoted original\n" {
		t.Errorf("patched body = %q", body["content"])
	}
	if _, ok := patch["subject"]; ok {
		t.Errorf("subject should not be overridden: %v", patch["subject"])
	}
	tos := patch["toRecipients"].([]any)
	if tos[0].(map[string]any)["emailAddress"].(map[string]any)["address"] != "new@to.z" {
		t.Errorf("toRecipients = %v", patch["toRecipients"])
	}
}

func TestGraphSendCleansUpDraftOnFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 201, map[string]any{"id": "D1"})
	})
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/D1/send", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 500, map[string]any{"error": map[string]string{"code": "X", "message": "boom"}})
	})
	var deleted bool
	mux.HandleFunc("DELETE /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/D1", func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		w.WriteHeader(204)
	})
	g := setupGraph(t, mux, nil)
	if err := g.Send(Outgoing{To: []string{"x@y.z"}, Subject: "S", Body: "B"}); err == nil {
		t.Fatal("Send should fail")
	}
	if !deleted {
		t.Error("draft not deleted after failed send")
	}
}

func TestGraphMarkMove(t *testing.T) {
	mux := http.NewServeMux()
	var isRead *bool
	mux.HandleFunc("PATCH /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/M1", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]bool
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode: %v", err)
		}
		v := b["isRead"]
		isRead = &v
		w.WriteHeader(200)
	})
	var dst string
	mux.HandleFunc("POST /v1.0/users/"+url.PathEscape(testMailbox)+"/messages/M1/move", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode: %v", err)
		}
		dst = b["destinationId"]
		w.WriteHeader(201)
	})
	g := setupGraph(t, mux, nil)
	if err := g.Mark("M1", true); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if isRead == nil || !*isRead {
		t.Errorf("isRead = %v", isRead)
	}
	if err := g.Move("M1", "archive"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if dst != "archive" {
		t.Errorf("destinationId = %q", dst)
	}
}

func TestGraphErrorMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1.0/users/"+url.PathEscape(testMailbox)+"/mailFolders/inbox/messages", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 403, map[string]any{"error": map[string]string{
			"code": "ErrorAccessDenied", "message": "Access is denied.",
		}})
	})
	g := setupGraph(t, mux, nil)
	_, err := g.List(ListOpts{})
	if err == nil {
		t.Fatal("List should fail")
	}
	for _, want := range []string{"403", "ErrorAccessDenied", "Access is denied."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestGraphRetryAfter(t *testing.T) {
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("GET /v1.0/users/"+url.PathEscape(testMailbox)+"/mailFolders/inbox/messages", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, 429, map[string]any{"error": map[string]string{"code": "TooBusy", "message": "slow down"}})
			return
		}
		writeJSON(t, w, 200, map[string]any{"value": []any{}})
	})
	g := setupGraph(t, mux, nil)
	if _, err := g.List(ListOpts{}); err != nil {
		t.Fatalf("List after retry: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}
