package quota_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/quota"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, _ *http.Request) (*server.Principal, error) {
	return &server.Principal{
		Username:        "user@example.com",
		Accounts:        map[jmap.ID]*jmap.Account{"acct1": {Name: "User", IsPersonal: true}},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.CoreURI: "acct1"},
	}, nil
}

func call(t *testing.T, srv *httptest.Server, methodCalls string) [][3]json.RawMessage {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core",%q],"methodCalls":%s}`, string(jmap.QuotasURI), methodCalls)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/jmap/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d", resp.StatusCode)
	}
	var decoded struct {
		MethodResponses [][3]json.RawMessage `json:"methodResponses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.MethodResponses
}

func argsOf(t *testing.T, resp [3]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(resp[1], &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func nameOf(t *testing.T, resp [3]json.RawMessage) string {
	t.Helper()
	var name string
	if err := json.Unmarshal(resp[0], &name); err != nil {
		t.Fatal(err)
	}
	return name
}

func seed(t *testing.T, store datatype.Store[quota.Quota], qs ...quota.Quota) {
	t.Helper()
	for _, q := range qs {
		if _, serr := store.Create(context.Background(), "acct1", q); serr != nil {
			t.Fatalf("seed %s: %v", q.ID, serr)
		}
	}
}

func newMemoryServer(t *testing.T) *httptest.Server {
	t.Helper()
	limit := uint64(1000)
	store := datatype.NewMemory[quota.Quota](
		func(q quota.Quota) jmap.ID { return q.ID },
		func(q *quota.Quota, id jmap.ID) { q.ID = id },
	)
	seed(t, store,
		quota.Quota{ID: "q-octets", ResourceType: "octets", Used: 500, HardLimit: &limit, Scope: "account", Name: "storage", Types: []string{string(jmap.MailURI)}},
		quota.Quota{ID: "q-messages", ResourceType: "messages", Used: 42, Scope: "account", Name: "message-count"},
	)
	e := server.New(fakeAuth{}, server.Options{})
	quota.Register(e, store)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestQuotaGetAndQueryOverHTTP(t *testing.T) {
	srv := newMemoryServer(t)

	// Get all: hardLimit null for the unlimited quota.
	resps := call(t, srv, `[["Quota/get",{"accountId":"acct1","ids":null},"g0"]]`)
	args := argsOf(t, resps[0])
	var list []quota.Quota
	if err := json.Unmarshal(args["list"], &list); err != nil || len(list) != 2 {
		t.Fatalf("get list = %s (err %v)", args["list"], err)
	}
	if list[0].HardLimit == nil || *list[0].HardLimit != 1000 || list[1].HardLimit != nil {
		t.Fatalf("hardLimits = %v / %v", list[0].HardLimit, list[1].HardLimit)
	}
	if !strings.Contains(string(args["list"]), `"hardLimit":null`) {
		t.Fatalf("unlimited quota should marshal hardLimit null: %s", args["list"])
	}

	// Query by resourceType.
	resps = call(t, srv, `[["Quota/query",{"accountId":"acct1","filter":{"resourceType":"octets"},"calculateTotal":true},"q1"]]`)
	args = argsOf(t, resps[0])
	var ids []string
	if err := json.Unmarshal(args["ids"], &ids); err != nil || len(ids) != 1 || ids[0] != "q-octets" {
		t.Fatalf("query by resourceType ids = %s", args["ids"])
	}
	if string(args["total"]) != "1" {
		t.Fatalf("total = %s", args["total"])
	}

	// Query by scope+name (ANDed conditions), and sort by used desc.
	resps = call(t, srv, `[["Quota/query",{"accountId":"acct1","filter":{"scope":"account","name":"message-count"}},"q2"]]`)
	ids = nil
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil || len(ids) != 1 || ids[0] != "q-messages" {
		t.Fatalf("query by scope/name ids = %v", ids)
	}
	resps = call(t, srv, `[["Quota/query",{"accountId":"acct1","sort":[{"property":"used","isAscending":false}]},"q3"]]`)
	ids = nil
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil || len(ids) != 2 || ids[0] != "q-octets" {
		t.Fatalf("query sorted by used desc ids = %v", ids)
	}
}

// fakeSource is a quota.Source with fixed quotas/state.
type fakeSource struct {
	quotas []quota.Quota
	state  string
}

func (s fakeSource) Quotas(_ context.Context, _ jmap.ID) ([]quota.Quota, error) {
	return s.quotas, nil
}
func (s fakeSource) State(_ context.Context, _ jmap.ID) (string, error) { return s.state, nil }

func TestComputedStoreReadOnly(t *testing.T) {
	e := server.New(fakeAuth{}, server.Options{})
	quota.Register(e, quota.NewComputedStore(fakeSource{
		state:  "s1",
		quotas: []quota.Quota{{ID: "q1", ResourceType: "octets", Used: 7, Scope: "global", Name: "all-storage"}},
	}))
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)

	// Reads work; accountId is filled from the call account.
	resps := call(t, srv, `[["Quota/get",{"accountId":"acct1","ids":["q1","ghost"]},"g1"]]`)
	args := argsOf(t, resps[0])
	var list []quota.Quota
	if err := json.Unmarshal(args["list"], &list); err != nil || len(list) != 1 || list[0].Used != 7 {
		t.Fatalf("computed get list = %s", args["list"])
	}
	if list[0].AccountID != "acct1" {
		t.Fatalf("computed quota accountId = %q", list[0].AccountID)
	}
	var notFound []string
	if err := json.Unmarshal(args["notFound"], &notFound); err != nil || len(notFound) != 1 || notFound[0] != "ghost" {
		t.Fatalf("notFound = %s", args["notFound"])
	}
	resps = call(t, srv, `[["Quota/query",{"accountId":"acct1","filter":{"resourceType":"messages"}},"q1"]]`)
	var ids []string
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil || len(ids) != 0 {
		t.Fatalf("computed query ids = %v", ids)
	}

	// /set create is forbidden per-record.
	resps = call(t, srv, `[["Quota/set",{"accountId":"acct1","create":{"c1":{"resourceType":"octets","used":0,"hardLimit":null,"scope":"account","name":"x"}}},"s1"]]`)
	args = argsOf(t, resps[0])
	var notCreated map[string]struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(args["notCreated"], &notCreated); err != nil || notCreated["c1"].Type != "forbidden" {
		t.Fatalf("notCreated = %s", args["notCreated"])
	}
	if string(args["created"]) != "null" {
		t.Fatalf("created = %s, want null", args["created"])
	}

	// Changes at the current state: empty. Older state: cannot calculate.
	resps = call(t, srv, `[["Quota/changes",{"accountId":"acct1","sinceState":"s1"},"ch1"]]`)
	args = argsOf(t, resps[0])
	if string(args["created"]) != "[]" || string(args["newState"]) != `"s1"` {
		t.Fatalf("computed changes = %s", resps[0][1])
	}
	resps = call(t, srv, `[["Quota/changes",{"accountId":"acct1","sinceState":"old"},"ch2"]]`)
	if nameOf(t, resps[0]) != "error" || string(argsOf(t, resps[0])["type"]) != `"cannotCalculateChanges"` {
		t.Fatalf("stale changes response = %s %s", resps[0][0], resps[0][1])
	}
}
