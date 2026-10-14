package jmapadapter_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/jmapadapter"
)

// mimeCountingProvider wraps the shared fakeProvider and counts
// GetMessageMIME calls, so the query test can prove the Email/query
// path never fetches MIME.
type mimeCountingProvider struct {
	*fakeProvider
	mu        sync.Mutex
	mimeCalls int
}

func (p *mimeCountingProvider) GetMessageMIME(ctx context.Context, folderID, id string) ([]byte, error) {
	p.mu.Lock()
	p.mimeCalls++
	p.mu.Unlock()
	return p.fakeProvider.GetMessageMIME(ctx, folderID, id)
}

func (p *mimeCountingProvider) mimeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mimeCalls
}

type queryFactory struct{ prov bridge.Provider }

func (f queryFactory) New(string, bridge.Session) bridge.Provider { return f.prov }

func querySetup(t *testing.T, prov bridge.Provider) http.Handler {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: fakeLogin{}, Factory: queryFactory{prov: prov}})
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := jmapadapter.NewEngine(reg, pw, zerolog.Nop())
	return eng.Handler()
}

func queryEmailID(folder, upstream string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(folder + "\x00" + upstream))
}

func TestEmailQueryDoesNotFetchMIME(t *testing.T) {
	base := newFakeProvider()
	gamma := base.seed("inbox", "Gamma", "body gamma", nil)
	alpha := base.seed("inbox", "Alpha", "body alpha", nil)
	beta := base.seed("inbox", "Beta", "body beta", nil)
	base.seed("sent", "Alpha Other", "body other", nil) // must be filtered out by inMailbox
	prov := &mimeCountingProvider{fakeProvider: base}
	h := querySetup(t, prov)
	acct := "a@example.com"

	res := callJMAP(t, h, acct, [3]any{"Email/query", map[string]any{
		"accountId":      acct,
		"filter":         map[string]any{"inMailbox": "inbox"},
		"sort":           []any{map[string]any{"property": "subject", "isAscending": true}},
		"limit":          2,
		"calculateTotal": true,
	}, "c1"})
	var q struct {
		AccountID           string   `json:"accountId"`
		QueryState          string   `json:"queryState"`
		CanCalculateChanges bool     `json:"canCalculateChanges"`
		Position            int      `json:"position"`
		Total               *int     `json:"total"`
		IDs                 []string `json:"ids"`
	}
	if err := json.Unmarshal(res[0], &q); err != nil {
		t.Fatalf("decode query response: %v %s", err, res[0])
	}
	want := []string{queryEmailID("inbox", alpha), queryEmailID("inbox", beta)}
	if len(q.IDs) != len(want) || q.IDs[0] != want[0] || q.IDs[1] != want[1] {
		t.Fatalf("query ids = %v, want %v (sorted by subject, limited; gamma=%s)", q.IDs, want, gamma)
	}
	if q.Total == nil || *q.Total != 3 {
		t.Fatalf("query total = %v, want 3 (%s)", q.Total, res[0])
	}
	if q.Position != 0 || !q.CanCalculateChanges || q.QueryState == "" || q.AccountID != acct {
		t.Fatalf("query response fields wrong: %s", res[0])
	}
	if n := prov.mimeCount(); n != 0 {
		t.Fatalf("Email/query fetched MIME %d times, want 0", n)
	}

	// Sanity: Email/get afterwards still returns the full body (and
	// does fetch MIME, proving the counter works).
	res = callJMAP(t, h, acct, [3]any{"Email/get", map[string]any{
		"accountId": acct,
		"ids":       []string{queryEmailID("inbox", gamma)},
	}, "c1"})
	if s := string(res[0]); !strings.Contains(s, "Gamma") || !strings.Contains(s, "body gamma") {
		t.Fatalf("Email/get after query missing subject/body: %s", s)
	}
	if n := prov.mimeCount(); n == 0 {
		t.Fatalf("Email/get should have fetched MIME, counter still 0")
	}
}
