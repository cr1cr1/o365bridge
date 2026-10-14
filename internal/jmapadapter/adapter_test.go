package jmapadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	stdmail "net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/jmapadapter"
)

// --- fakes ---

type fakeLogin struct{}

func (fakeLogin) Login(_ context.Context, email string, ch bridge.Channel) (bridge.Session, error) {
	return bridge.Session{Email: email, Channel: ch, AccessToken: "t", TokenExpiry: time.Now().Add(time.Hour)}, nil
}
func (fakeLogin) Renew(_ context.Context, _ string, s bridge.Session) (bridge.Session, error) {
	s.TokenExpiry = time.Now().Add(time.Hour)
	return s, nil
}

type fakeFactory struct {
	provs map[string]*fakeProvider
}

func (f fakeFactory) New(email string, _ bridge.Session) bridge.Provider { return f.provs[email] }

type fakeMsg struct {
	meta bridge.MessageMeta
	raw  []byte
}

type fakeProvider struct {
	bridge.Provider
	mu      sync.Mutex
	folders []bridge.Folder
	order   map[string][]string
	msgs    map[string]map[string]*fakeMsg
	seq     int
	sent    [][]byte
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		folders: []bridge.Folder{
			{ID: "inbox", Name: "INBOX", Special: "inbox"},
			{ID: "drafts", Name: "Drafts", Special: "drafts"},
			{ID: "sent", Name: "Sent", Special: "sent"},
		},
		order: map[string][]string{},
		msgs:  map[string]map[string]*fakeMsg{},
	}
}

func makeRaw(subject, from, to, body string, date time.Time, msgID string) []byte {
	return []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		from, to, subject, date.Format(time.RFC1123Z), msgID, body))
}

func metaFromRaw(folder, id string, raw []byte, flags bridge.Flags) bridge.MessageMeta {
	m := bridge.MessageMeta{ID: id, FolderID: folder, Flags: flags, Size: int64(len(raw)), Date: time.Now()}
	if msg, err := stdmail.ReadMessage(bytes.NewReader(raw)); err == nil {
		m.Subject = msg.Header.Get("Subject")
		m.From = msg.Header.Get("From")
		if d, err := msg.Header.Date(); err == nil {
			m.Date = d
		}
		if strings.Contains(msg.Header.Get("Content-Type"), "multipart") {
			m.Attachments = true
		}
	}
	return m
}

func (p *fakeProvider) seed(folder, subject, body string, flags bridge.Flags) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	id := fmt.Sprintf("m%d", p.seq)
	raw := makeRaw(subject, "Alice <alice@example.com>", "bob@example.com", body, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "<"+id+"@example.com>")
	if p.msgs[folder] == nil {
		p.msgs[folder] = map[string]*fakeMsg{}
	}
	meta := metaFromRaw(folder, id, raw, flags)
	p.msgs[folder][id] = &fakeMsg{meta: meta, raw: raw}
	p.order[folder] = append(p.order[folder], id)
	return id
}

func (p *fakeProvider) Ping(context.Context) error { return nil }

func (p *fakeProvider) ListFolders(context.Context) ([]bridge.Folder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]bridge.Folder, len(p.folders))
	copy(out, p.folders)
	for i, f := range out {
		total, unread := 0, 0
		for _, id := range p.order[f.ID] {
			if msg, ok := p.msgs[f.ID][id]; ok {
				total++
				seen := false
				for _, fl := range msg.meta.Flags {
					if fl == `\seen` {
						seen = true
					}
				}
				if !seen {
					unread++
				}
			}
		}
		out[i].Total, out[i].Unread = total, unread
	}
	return out, nil
}

func (p *fakeProvider) CreateFolder(_ context.Context, name, parentID string) (bridge.Folder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	f := bridge.Folder{ID: fmt.Sprintf("f%d", p.seq), Name: name, ParentID: parentID}
	p.folders = append(p.folders, f)
	return f, nil
}
func (p *fakeProvider) DeleteFolder(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, f := range p.folders {
		if f.ID == id {
			p.folders = append(p.folders[:i], p.folders[i+1:]...)
		}
	}
	return nil
}
func (p *fakeProvider) RenameFolder(_ context.Context, id, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.folders {
		if p.folders[i].ID == id {
			p.folders[i].Name = name
		}
	}
	return nil
}

func (p *fakeProvider) ListMessages(_ context.Context, folderID string, offset, limit int) ([]bridge.MessageMeta, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := p.order[folderID]
	total := len(ids)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	var out []bridge.MessageMeta
	for _, id := range ids[offset:end] {
		out = append(out, p.msgs[folderID][id].meta)
	}
	return out, total, nil
}

func (p *fakeProvider) GetMessage(_ context.Context, folderID, id string) (bridge.MessageMeta, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.msgs[folderID][id]
	if !ok {
		return bridge.MessageMeta{}, fmt.Errorf("not found")
	}
	return m.meta, nil
}
func (p *fakeProvider) GetMessageMIME(_ context.Context, folderID, id string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.msgs[folderID][id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return append([]byte(nil), m.raw...), nil
}
func (p *fakeProvider) SetFlags(_ context.Context, folderID string, ids []string, add, remove bridge.Flags) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		m := p.msgs[folderID][id]
		if m == nil {
			continue
		}
		set := map[string]bool{}
		for _, f := range m.meta.Flags {
			set[f] = true
		}
		for _, f := range add {
			set[f] = true
		}
		for _, f := range remove {
			delete(set, f)
		}
		m.meta.Flags = nil
		for f := range set {
			m.meta.Flags = append(m.meta.Flags, f)
		}
	}
	return nil
}
func (p *fakeProvider) CopyMessages(_ context.Context, folderID string, ids []string, dest string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.msgs[dest] == nil {
		p.msgs[dest] = map[string]*fakeMsg{}
	}
	for _, id := range ids {
		src := p.msgs[folderID][id]
		if src == nil {
			return fmt.Errorf("not found")
		}
		p.seq++
		nid := fmt.Sprintf("m%d", p.seq)
		nm := *src
		nm.meta.ID = nid
		nm.meta.FolderID = dest
		nm.raw = append([]byte(nil), src.raw...)
		p.msgs[dest][nid] = &nm
		p.order[dest] = append(p.order[dest], nid)
	}
	return nil
}
func (p *fakeProvider) DeleteMessages(_ context.Context, folderID string, ids []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		delete(p.msgs[folderID], id)
		for i, v := range p.order[folderID] {
			if v == id {
				p.order[folderID] = append(p.order[folderID][:i], p.order[folderID][i+1:]...)
				break
			}
		}
	}
	return nil
}
func (p *fakeProvider) AppendMessage(_ context.Context, folderID string, mime []byte, flags bridge.Flags) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	id := fmt.Sprintf("m%d", p.seq)
	if p.msgs[folderID] == nil {
		p.msgs[folderID] = map[string]*fakeMsg{}
	}
	meta := metaFromRaw(folderID, id, mime, flags)
	p.msgs[folderID][id] = &fakeMsg{meta: meta, raw: append([]byte(nil), mime...)}
	p.order[folderID] = append(p.order[folderID], id)
	return id, nil
}
func (p *fakeProvider) SendMessage(_ context.Context, mime []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, append([]byte(nil), mime...))
	return nil
}

// --- HTTP helpers ---

const pw = "secret"

func setup(t *testing.T, provs map[string]*fakeProvider) http.Handler {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: fakeLogin{}, Factory: fakeFactory{provs: provs}})
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := jmapadapter.NewEngine(reg, pw, zerolog.Nop())
	return eng.Handler()
}

func doReq(t *testing.T, h http.Handler, method, path, user, pass string, body []byte, ct string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type invocation struct {
	Name string
	Args json.RawMessage
}

func callJMAP(t *testing.T, h http.Handler, user string, calls ...[3]any) []json.RawMessage {
	t.Helper()
	var mcs []any
	for _, c := range calls {
		args, _ := json.Marshal(c[1])
		mcs = append(mcs, []any{c[0], json.RawMessage(args), c[2]})
	}
	body, _ := json.Marshal(map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": mcs,
	})
	rec := doReq(t, h, "POST", "/jmap/", user, pw, body, "application/json")
	if rec.Code != 200 {
		t.Fatalf("api status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v %s", err, rec.Body.String())
	}
	var out []json.RawMessage
	for _, mr := range resp.MethodResponses {
		if len(mr) >= 2 {
			out = append(out, mr[1])
		}
	}
	return out
}

func TestSessionAndAuth(t *testing.T) {
	pa := newFakeProvider()
	pa.seed("inbox", "Hello A", "body a", bridge.Flags{`\seen`})
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})

	rec := doReq(t, h, "GET", "/jmap/session", "a@example.com", "wrong", nil, "")
	if rec.Code != 401 {
		t.Fatalf("wrong password: got %d", rec.Code)
	}
	rec = doReq(t, h, "GET", "/jmap/session", "a@example.com", pw, nil, "")
	if rec.Code != 200 {
		t.Fatalf("session: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "urn:ietf:params:jmap:mail") || !strings.Contains(body, "a@example.com") {
		t.Fatalf("session missing mail/account: %s", body)
	}
}

func TestMailboxEmailQueryGet(t *testing.T) {
	pa := newFakeProvider()
	pa.seed("inbox", "Hello A", "the quick brown fox", bridge.Flags{`\seen`})
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})
	acct := "a@example.com"

	res := callJMAP(t, h, acct, [3]any{"Mailbox/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})
	var mb struct {
		List []struct {
			ID   string  `json:"id"`
			Name string  `json:"name"`
			Role *string `json:"role"`
		} `json:"list"`
	}
	if err := json.Unmarshal(res[0], &mb); err != nil {
		t.Fatal(err)
	}
	foundInbox := false
	for _, m := range mb.List {
		if m.ID == "inbox" && m.Role != nil && *m.Role == "inbox" {
			foundInbox = true
		}
	}
	if !foundInbox {
		t.Fatalf("INBOX role not found: %s", res[0])
	}

	res = callJMAP(t, h, acct, [3]any{"Email/query", map[string]any{"accountId": acct, "filter": map[string]any{"inMailbox": "inbox"}}, "c1"})
	var q struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(res[0], &q); err != nil || len(q.IDs) != 1 {
		t.Fatalf("query: %v %s", err, res[0])
	}
	emailID := q.IDs[0]

	res = callJMAP(t, h, acct, [3]any{"Email/get", map[string]any{"accountId": acct, "ids": []string{emailID}}, "c1"})
	s := string(res[0])
	if !strings.Contains(s, "Hello A") || !strings.Contains(s, "$seen") || !strings.Contains(s, "the quick brown fox") {
		t.Fatalf("Email/get missing subject/keyword/body: %s", s)
	}
}

func TestCreateDraftAndSubmit(t *testing.T) {
	pa := newFakeProvider()
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})
	acct := "a@example.com"

	res := callJMAP(t, h, acct, [3]any{"Email/set", map[string]any{
		"accountId": acct,
		"create": map[string]any{"draft1": map[string]any{
			"mailboxIds": map[string]bool{"drafts": true},
			"keywords":   map[string]bool{"$draft": true},
			"from":       []map[string]string{{"email": "a@example.com"}},
			"to":         []map[string]string{{"email": "bob@example.com"}},
			"subject":    "Draft Subject",
			"textBody":   []map[string]string{{"partId": "t1"}},
			"bodyValues": map[string]any{"t1": map[string]string{"value": "draft body"}},
		}},
	}, "c1"})
	var setResp struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]any `json:"notCreated"`
	}
	if err := json.Unmarshal(res[0], &setResp); err != nil || len(setResp.Created) != 1 {
		t.Fatalf("create draft: %v %s", err, res[0])
	}
	emailID := setResp.Created["draft1"].ID

	// Fake provider should have the draft
	if len(pa.order["drafts"]) != 1 {
		t.Fatalf("draft not appended")
	}

	res = callJMAP(t, h, acct, [3]any{"EmailSubmission/set", map[string]any{
		"accountId": acct,
		"create":    map[string]any{"sub1": map[string]any{"emailId": emailID, "identityId": acct}},
	}, "c1"})
	if !strings.Contains(string(res[0]), "created") || strings.Contains(string(res[0]), "notCreated") && strings.Contains(string(res[0]), `"sub1"`) {
		// check created present
	}
	var subResp struct {
		Created    map[string]any `json:"created"`
		NotCreated map[string]any `json:"notCreated"`
	}
	if err := json.Unmarshal(res[0], &subResp); err != nil || len(subResp.Created) != 1 {
		t.Fatalf("submission: %v %s", err, res[0])
	}
	if len(pa.sent) != 1 || !strings.Contains(string(pa.sent[0]), "Draft Subject") {
		t.Fatalf("sent MIME wrong: %v", pa.sent)
	}
}

func TestChangesAndBogusState(t *testing.T) {
	pa := newFakeProvider()
	pa.seed("inbox", "First", "one", nil)
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})
	acct := "a@example.com"

	// Initial list observes first message
	res := callJMAP(t, h, acct, [3]any{"Email/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})
	var getResp struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(res[0], &getResp)
	initialState := getResp.State

	pa.seed("inbox", "Second", "two", nil)
	// List again to observe second
	callJMAP(t, h, acct, [3]any{"Email/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})

	res = callJMAP(t, h, acct, [3]any{"Email/changes", map[string]any{"accountId": acct, "sinceState": initialState}, "c1"})
	s := string(res[0])
	if !strings.Contains(s, "created") || strings.Contains(s, `"created":[]`) {
		t.Fatalf("changes should list created: %s (initial %s)", s, initialState)
	}

	res = callJMAP(t, h, acct, [3]any{"Email/changes", map[string]any{"accountId": acct, "sinceState": "bogus"}, "c1"})
	// Changes error is a method error invocation: name will be "error" - our helper returns args only.
	// Re-do raw to check type
	body, _ := json.Marshal(map[string]any{"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}, "methodCalls": []any{[]any{"Email/changes", map[string]any{"accountId": acct, "sinceState": "bogus"}, "c1"}}})
	rec := doReq(t, h, "POST", "/jmap/", acct, pw, body, "application/json")
	if !strings.Contains(rec.Body.String(), "cannotCalculateChanges") {
		t.Fatalf("bogus state should be cannotCalculateChanges: %s", rec.Body.String())
	}
	_ = res
}

func TestIsolationTwoAccounts(t *testing.T) {
	pa := newFakeProvider()
	pa.seed("inbox", "Secret A", "aaa", nil)
	pb := newFakeProvider()
	pb.seed("inbox", "Secret B", "bbb", nil)
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa, "b@example.com": pb})

	res := callJMAP(t, h, "b@example.com", [3]any{"Email/query", map[string]any{"accountId": "b@example.com", "filter": map[string]any{"inMailbox": "inbox"}}, "c1"})
	var q struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(res[0], &q)
	if len(q.IDs) != 1 {
		t.Fatalf("B query ids %v", q.IDs)
	}
	res = callJMAP(t, h, "b@example.com", [3]any{"Email/get", map[string]any{"accountId": "b@example.com", "ids": q.IDs}, "c1"})
	if strings.Contains(string(res[0]), "Secret A") {
		t.Fatalf("isolation leak: %s", res[0])
	}
	if !strings.Contains(string(res[0]), "Secret B") {
		t.Fatalf("B should see own: %s", res[0])
	}
}

func TestBlobUploadImportAndDownload(t *testing.T) {
	pa := newFakeProvider()
	pa.seed("inbox", "Seeded", "seed body", nil)
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})
	acct := "a@example.com"

	raw := makeRaw("Uploaded Subject", "x@example.com", "a@example.com", "uploaded body", time.Now(), "<up1@example.com>")
	req := httptest.NewRequest("POST", "/jmap/upload/"+acct, bytes.NewReader(raw))
	req.SetBasicAuth(acct, pw)
	req.Header.Set("Content-Type", "message/rfc822")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("upload %d %s", rec.Code, rec.Body.String())
	}
	var up struct {
		BlobID string `json:"blobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil || up.BlobID == "" {
		t.Fatalf("upload resp %v %s", err, rec.Body.String())
	}

	res := callJMAP(t, h, acct, [3]any{"Email/import", map[string]any{
		"accountId": acct,
		"emails":    map[string]any{"imp1": map[string]any{"blobId": up.BlobID, "mailboxIds": map[string]bool{"inbox": true}, "keywords": map[string]bool{}}},
	}, "c1"})
	if !strings.Contains(string(res[0]), "created") {
		t.Fatalf("import: %s", res[0])
	}
	// Verify imported message exists with subject via query/get all
	res = callJMAP(t, h, acct, [3]any{"Email/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})
	if !strings.Contains(string(res[0]), "Uploaded Subject") {
		t.Fatalf("imported not listed: %s", res[0])
	}

	// Download email blob: get an email id first
	res = callJMAP(t, h, acct, [3]any{"Email/query", map[string]any{"accountId": acct, "filter": map[string]any{"inMailbox": "inbox"}}, "c1"})
	var q struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(res[0], &q)
	if len(q.IDs) == 0 {
		t.Fatal("no ids")
	}
	dl := doReq(t, h, "GET", "/jmap/download/"+acct+"/"+q.IDs[0]+"/file.eml", acct, pw, nil, "")
	if dl.Code != 200 || !strings.Contains(dl.Body.String(), "Subject:") {
		t.Fatalf("download %d %s", dl.Code, dl.Body.String())
	}
	_ = io.Discard
}
