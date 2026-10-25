package jmapadapter_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/jmapadapter"
)

// --- tracking provider: wraps fakeProvider, records move/copy/delete,
// can fail MIME per upstream id ---

type moveCall struct{ src, dst string }

type trackProvider struct {
	*fakeProvider
	mu          sync.Mutex
	moves       []moveCall
	copies      int
	deletes     int
	failMIMEFor map[string]bool
}

func newTrackProvider() *trackProvider {
	return &trackProvider{fakeProvider: newFakeProvider(), failMIMEFor: map[string]bool{}}
}

func (p *trackProvider) MoveMessages(_ context.Context, src string, ids []string, dst string) error {
	p.mu.Lock()
	p.moves = append(p.moves, moveCall{src, dst})
	p.mu.Unlock()
	p.fakeProvider.mu.Lock()
	defer p.fakeProvider.mu.Unlock()
	if p.msgs[dst] == nil {
		p.msgs[dst] = map[string]*fakeMsg{}
	}
	for _, id := range ids {
		m, ok := p.msgs[src][id]
		if !ok {
			return fmt.Errorf("not found")
		}
		nm := *m
		nm.meta.FolderID = dst
		p.msgs[dst][id] = &nm
		delete(p.msgs[src], id)
		for i, v := range p.order[src] {
			if v == id {
				p.order[src] = append(p.order[src][:i], p.order[src][i+1:]...)
				break
			}
		}
		p.order[dst] = append(p.order[dst], id)
	}
	return nil
}

func (p *trackProvider) CopyMessages(ctx context.Context, src string, ids []string, dst string) error {
	p.mu.Lock()
	p.copies++
	p.mu.Unlock()
	return p.fakeProvider.CopyMessages(ctx, src, ids, dst)
}

func (p *trackProvider) DeleteMessages(ctx context.Context, folder string, ids []string) error {
	p.mu.Lock()
	p.deletes++
	p.mu.Unlock()
	return p.fakeProvider.DeleteMessages(ctx, folder, ids)
}

func (p *trackProvider) GetMessageMIME(ctx context.Context, folder, id string) ([]byte, error) {
	p.mu.Lock()
	fail := p.failMIMEFor[id]
	p.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("mime unavailable")
	}
	return p.fakeProvider.GetMessageMIME(ctx, folder, id)
}

func (p *trackProvider) setFailMIME(id string, fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failMIMEFor[id] = fail
}

func (p *trackProvider) counts() (moves []moveCall, copies, deletes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]moveCall(nil), p.moves...), p.copies, p.deletes
}

type singleFactory struct{ p bridge.Provider }

func (f singleFactory) New(string, bridge.Session) bridge.Provider { return f.p }

func setupFix(t *testing.T, p bridge.Provider) http.Handler {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: fakeLogin{}, Factory: singleFactory{p}})
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := jmapadapter.NewEngine(reg, pw, zerolog.Nop())
	return eng.Handler()
}

type setErrBody struct {
	Type       string   `json:"type"`
	Properties []string `json:"properties"`
}

func queryIDs(t *testing.T, h http.Handler, acct string) []string {
	t.Helper()
	res := callJMAP(t, h, acct, [3]any{"Email/query", map[string]any{"accountId": acct, "filter": map[string]any{"inMailbox": "inbox"}}, "c1"})
	var q struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(res[0], &q); err != nil {
		t.Fatal(err)
	}
	return q.IDs
}

// Fix 2: journal paging within a multi-record version, plus legacy states.
func TestJournalPagingMultiRecordVersion(t *testing.T) {
	reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: fakeLogin{}, Factory: fakeFactory{provs: map[string]*fakeProvider{}}})
	if err != nil {
		t.Fatal(err)
	}
	j := jmapadapter.NewJournal(reg, zerolog.Nop())
	acct := "a@example.com"
	if !j.Observe(acct, []jmapadapter.Witness{{ID: "e1", Folder: "inbox", Hash: "h1"}, {ID: "e2", Folder: "inbox", Hash: "h2"}}, false) {
		t.Fatal("observe should record a change")
	}
	// Old plain state "0" (caught up at version 0) still works.
	c, _, _, _, hm, err := j.Changes(acct, "0", 10)
	if err != nil || hm || len(c) != 2 {
		t.Fatalf("legacy plain state: c=%v hm=%v err=%v", c, hm, err)
	}
	// Page with maxChanges=1 across two calls: both IDs exactly once.
	c1, _, _, s1, hm1, err := j.Changes(acct, "0:0", 1)
	if err != nil || !hm1 || len(c1) != 1 || s1 == "0:0" {
		t.Fatalf("page1: c=%v state=%q hm=%v err=%v", c1, s1, hm1, err)
	}
	c2, _, _, s2, hm2, err := j.Changes(acct, s1, 1)
	if err != nil || hm2 || len(c2) != 1 {
		t.Fatalf("page2: c=%v state=%q hm=%v err=%v", c2, s2, hm2, err)
	}
	seen := map[string]int{}
	seen[string(c1[0])]++
	seen[string(c2[0])]++
	if len(seen) != 2 || seen["e1"] != 1 || seen["e2"] != 1 {
		t.Fatalf("paged ids not exactly once: %v (c1=%v c2=%v)", seen, c1, c2)
	}
	// Caught-up: State round-trips with no further changes; plain "1"
	// (caught up at version 1) also yields nothing.
	if st := j.State(acct); st != s2 {
		t.Fatalf("State %q != final page state %q", st, s2)
	}
	for _, since := range []string{s2, "1"} {
		c, u, d, _, hm, err := j.Changes(acct, since, 10)
		if err != nil || hm || len(c)+len(u)+len(d) != 0 {
			t.Fatalf("caught-up since %q: %v %v %v hm=%v err=%v", since, c, u, d, hm, err)
		}
	}
	// Bogus / future states cannot be calculated.
	if _, _, _, _, _, err := j.Changes(acct, "bogus", 10); err == nil {
		t.Fatal("bogus state should error")
	}
	if _, _, _, _, _, err := j.Changes(acct, "99:0", 10); err == nil {
		t.Fatal("future state should error")
	}
}

// Fix 3: single-folder mailboxIds change is a Move, not Copy+Delete.
func TestEmailSetMoveUsesMoveMessages(t *testing.T) {
	tp := newTrackProvider()
	tp.folders = append(tp.folders, bridge.Folder{ID: "archive", Name: "Archive"})
	tp.seed("inbox", "Move Me", "body", nil)
	h := setupFix(t, tp)
	acct := "a@example.com"
	ids := queryIDs(t, h, acct)
	if len(ids) != 1 {
		t.Fatalf("ids %v", ids)
	}
	res := callJMAP(t, h, acct, [3]any{"Email/set", map[string]any{
		"accountId": acct,
		"update":    map[string]any{ids[0]: map[string]any{"mailboxIds": map[string]bool{"archive": true}}},
	}, "c1"})
	var resp struct {
		Updated    map[string]any `json:"updated"`
		NotUpdated map[string]any `json:"notUpdated"`
	}
	if err := json.Unmarshal(res[0], &resp); err != nil || len(resp.NotUpdated) != 0 {
		t.Fatalf("move set: %v %s", err, res[0])
	}
	moves, copies, deletes := tp.counts()
	if len(moves) != 1 || moves[0].src != "inbox" || moves[0].dst != "archive" {
		t.Fatalf("moves = %v", moves)
	}
	if deletes != 0 || copies != 0 {
		t.Fatalf("copies=%d deletes=%d, want 0/0", copies, deletes)
	}
	if len(tp.msgs["inbox"]) != 0 || len(tp.msgs["archive"]) != 1 {
		t.Fatalf("folders: inbox=%d archive=%d", len(tp.msgs["inbox"]), len(tp.msgs["archive"]))
	}
}

// Fix 5: moving an email into Sent does not duplicate when the send
// already stored its copy (same stateless rule as IMAP APPEND).
func TestEmailSetMoveIntoSentDedupes(t *testing.T) {
	jmapadapter.SentDedupeRetryDelay = time.Millisecond
	tp := newTrackProvider()
	upID := tp.seed("drafts", "Send Me", "body", nil)
	// The SendAndSaveCopy copy upstream: same Message-ID, in Sent.
	tp.mu.Lock()
	tp.fakeProvider.mu.Lock()
	raw := tp.msgs["drafts"][upID].raw
	meta := metaFromRaw("sent", "sentcopy", raw, nil)
	if tp.msgs["sent"] == nil {
		tp.msgs["sent"] = map[string]*fakeMsg{}
	}
	tp.msgs["sent"]["sentcopy"] = &fakeMsg{meta: meta, raw: raw}
	tp.order["sent"] = append(tp.order["sent"], "sentcopy")
	tp.fakeProvider.mu.Unlock()
	tp.mu.Unlock()

	h := setupFix(t, tp)
	acct := "a@example.com"
	res := callJMAP(t, h, acct, [3]any{"Email/query", map[string]any{"accountId": acct, "filter": map[string]any{"inMailbox": "drafts"}}, "c0"})
	var q struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(res[0], &q); err != nil || len(q.IDs) != 1 {
		t.Fatalf("draft query: %v ids=%v err=%v", res[0], q.IDs, err)
	}
	draftID := q.IDs[0]
	rawID, _ := base64.RawURLEncoding.DecodeString(draftID)
	if _, up, ok := strings.Cut(string(rawID), "\x00"); !ok || up != upID {
		t.Fatalf("draft id %s does not map to %s", draftID, upID)
	}
	res = callJMAP(t, h, acct, [3]any{"Email/set", map[string]any{
		"accountId": acct,
		"update":    map[string]any{draftID: map[string]any{"mailboxIds": map[string]bool{"sent": true}}},
	}, "c1"})
	var resp struct {
		NotUpdated map[string]setErrBody `json:"notUpdated"`
	}
	if err := json.Unmarshal(res[0], &resp); err != nil || len(resp.NotUpdated) != 0 {
		t.Fatalf("move set: %v %s", err, res[0])
	}
	moves, _, deletes := tp.counts()
	if len(moves) != 0 {
		t.Fatalf("move into Sent went upstream: %v", moves)
	}
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1", deletes)
	}
	tp.fakeProvider.mu.Lock()
	defer tp.fakeProvider.mu.Unlock()
	if len(tp.msgs["drafts"]) != 0 || len(tp.msgs["sent"]) != 1 {
		t.Fatalf("folders: drafts=%d sent=%d", len(tp.msgs["drafts"]), len(tp.msgs["sent"]))
	}
}

// Fix 4: mixed immutable + keyword patch fails atomically.
func TestEmailSetImmutableMixedPatchRejected(t *testing.T) {
	tp := newTrackProvider()
	upID := tp.seed("inbox", "Original", "body", nil)
	h := setupFix(t, tp)
	acct := "a@example.com"
	ids := queryIDs(t, h, acct)
	res := callJMAP(t, h, acct, [3]any{"Email/set", map[string]any{
		"accountId": acct,
		"update":    map[string]any{ids[0]: map[string]any{"subject": "Changed", "keywords": map[string]bool{"$seen": true}}},
	}, "c1"})
	var resp struct {
		NotUpdated map[string]setErrBody `json:"notUpdated"`
	}
	if err := json.Unmarshal(res[0], &resp); err != nil {
		t.Fatal(err)
	}
	se, ok := resp.NotUpdated[ids[0]]
	if !ok || se.Type != "invalidProperties" || len(se.Properties) != 1 || se.Properties[0] != "subject" {
		t.Fatalf("notUpdated = %s", res[0])
	}
	// Flags unchanged upstream (keyword part must not have been applied).
	for _, f := range tp.msgs["inbox"][upID].meta.Flags {
		if f == `\seen` {
			t.Fatalf("flags were changed despite rejection: %v", tp.msgs["inbox"][upID].meta.Flags)
		}
	}
	if tp.msgs["inbox"][upID].meta.Subject != "Original" {
		t.Fatal("subject changed upstream")
	}
}

// Fix 5: future sendAt (and bad identity) rejected without sending.
func TestSubmissionFutureSendAtRejected(t *testing.T) {
	tp := newTrackProvider()
	tp.seed("inbox", "To Send", "body", nil)
	h := setupFix(t, tp)
	acct := "a@example.com"
	ids := queryIDs(t, h, acct)
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)

	mk := func(extra map[string]any) map[string]setErrBody {
		create := map[string]any{"emailId": ids[0], "identityId": acct}
		for k, v := range extra {
			create[k] = v
		}
		res := callJMAP(t, h, acct, [3]any{"EmailSubmission/set", map[string]any{
			"accountId": acct, "create": map[string]any{"s1": create},
		}, "c1"})
		var resp struct {
			NotCreated map[string]setErrBody `json:"notCreated"`
		}
		if err := json.Unmarshal(res[0], &resp); err != nil {
			t.Fatal(err)
		}
		return resp.NotCreated
	}
	if se, ok := mk(map[string]any{"sendAt": future})["s1"]; !ok || se.Type != "invalidProperties" || len(se.Properties) != 1 || se.Properties[0] != "sendAt" {
		t.Fatalf("future sendAt notCreated wrong")
	}
	if se, ok := mk(map[string]any{"identityId": "other@example.com"})["s1"]; !ok || se.Type != "invalidProperties" || len(se.Properties) != 1 || se.Properties[0] != "identityId" {
		t.Fatalf("bad identity notCreated wrong")
	}
	if len(tp.sent) != 0 {
		t.Fatalf("SendMessage called %d times, want 0", len(tp.sent))
	}
}

// Fixes 1+6: a message whose MIME fails during a full listing is not
// reported destroyed by a later Email/changes.
func TestUnreadableMessageNotDestroyed(t *testing.T) {
	tp := newTrackProvider()
	tp.seed("inbox", "Good", "one", nil)
	badUp := tp.seed("inbox", "Flaky", "two", nil)
	h := setupFix(t, tp)
	acct := "a@example.com"

	getAll := func() (ids []string, state string) {
		res := callJMAP(t, h, acct, [3]any{"Email/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})
		var gr struct {
			State string `json:"state"`
			List  []struct {
				ID string `json:"id"`
			} `json:"list"`
		}
		if err := json.Unmarshal(res[0], &gr); err != nil {
			t.Fatal(err)
		}
		for _, e := range gr.List {
			ids = append(ids, e.ID)
		}
		return ids, gr.State
	}

	ids, state := getAll()
	if len(ids) != 2 {
		t.Fatalf("initial list %v", ids)
	}
	// Identify the flaky message's JMAP id by subject.
	res := callJMAP(t, h, acct, [3]any{"Email/get", map[string]any{"accountId": acct, "ids": ids}, "c1"})
	var gr struct {
		List []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
		} `json:"list"`
	}
	_ = json.Unmarshal(res[0], &gr)
	var flakyID string
	for _, e := range gr.List {
		if e.Subject == "Flaky" {
			flakyID = e.ID
		}
	}
	if flakyID == "" {
		t.Fatalf("flaky id not found: %s", res[0])
	}

	tp.setFailMIME(badUp, true)
	if got, _ := getAll(); len(got) != 1 {
		t.Fatalf("list with failure should return 1, got %v", got)
	}
	tp.setFailMIME(badUp, false)
	if got, _ := getAll(); len(got) != 2 {
		t.Fatalf("recovered list %v", got)
	}

	res = callJMAP(t, h, acct, [3]any{"Email/changes", map[string]any{"accountId": acct, "sinceState": state}, "c1"})
	var ch struct {
		Destroyed []string `json:"destroyed"`
	}
	if err := json.Unmarshal(res[0], &ch); err != nil {
		t.Fatal(err)
	}
	for _, d := range ch.Destroyed {
		if d == flakyID {
			t.Fatalf("flaky message falsely destroyed: %s", res[0])
		}
	}
}
