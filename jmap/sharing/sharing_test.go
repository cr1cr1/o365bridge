package sharing_test

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
	"github.com/cr1cr1/o365bridge/jmap/server"
	"github.com/cr1cr1/o365bridge/jmap/sharing"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, _ *http.Request) (*server.Principal, error) {
	return &server.Principal{
		Username:        "user@example.com",
		Accounts:        map[jmap.ID]*jmap.Account{"acct1": {Name: "User", IsPersonal: true}},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.CoreURI: "acct1"},
	}, nil
}

func strPtr(s string) *string { return &s }

func newServer(t *testing.T) (*httptest.Server, datatype.Store[sharing.Principal]) {
	t.Helper()
	store := datatype.NewMemory[sharing.Principal](
		func(p sharing.Principal) jmap.ID { return p.ID },
		func(p *sharing.Principal, id jmap.ID) { p.ID = id },
	)
	for _, p := range []sharing.Principal{
		{ID: "alice", Type: "individual", Name: strPtr("Alice Adams"), Email: strPtr("alice@example.com"), TimeZone: strPtr("Europe/London")},
		{ID: "team", Type: "group", Name: strPtr("Team Calendar"), Email: strPtr("team@example.com")},
		{ID: "alicia", Type: "individual", Name: strPtr("Alicia Keys"), Email: strPtr("alicia@example.com")},
	} {
		if _, serr := store.Create(context.Background(), "acct1", p); serr != nil {
			t.Fatalf("seed %s: %v", p.ID, serr)
		}
	}
	e := server.New(fakeAuth{}, server.Options{})
	sharing.Register(e, store)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv, store
}

func call(t *testing.T, srv *httptest.Server, methodCalls string) [][3]json.RawMessage {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core",%q],"methodCalls":%s}`, string(jmap.SharingURI), methodCalls)
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

func queryIDs(t *testing.T, srv *httptest.Server, args string) []string {
	t.Helper()
	resps := call(t, srv, fmt.Sprintf(`[["Principal/query",%s,"q"]]`, args))
	var ids []string
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestPrincipalGetQueryChangesOverHTTP(t *testing.T) {
	srv, _ := newServer(t)

	// Get by id: capabilities render as {} (not null), nullable
	// fields render as null where unset.
	resps := call(t, srv, `[["Principal/get",{"accountId":"acct1","ids":["alice","ghost"]},"g1"]]`)
	args := argsOf(t, resps[0])
	var list []sharing.Principal
	if err := json.Unmarshal(args["list"], &list); err != nil || len(list) != 1 {
		t.Fatalf("get list = %s (err %v)", args["list"], err)
	}
	if list[0].Name == nil || *list[0].Name != "Alice Adams" || list[0].Type != "individual" {
		t.Fatalf("principal = %+v", list[0])
	}
	if !strings.Contains(string(args["list"]), `"capabilities":{}`) {
		t.Fatalf("capabilities should be {}: %s", args["list"])
	}
	var notFound []string
	if err := json.Unmarshal(args["notFound"], &notFound); err != nil || len(notFound) != 1 || notFound[0] != "ghost" {
		t.Fatalf("notFound = %s", args["notFound"])
	}

	// Query by type.
	if ids := queryIDs(t, srv, `{"accountId":"acct1","filter":{"type":"group"}}`); len(ids) != 1 || ids[0] != "team" {
		t.Fatalf("query by type ids = %v", ids)
	}
	// Query by name: case-insensitive substring ("ali" hits Alice+Alicia).
	if ids := queryIDs(t, srv, `{"accountId":"acct1","filter":{"name":"ALI"},"sort":[{"property":"name"}]}`); len(ids) != 2 || ids[0] != "alice" || ids[1] != "alicia" {
		t.Fatalf("query by name ids = %v", ids)
	}
	// Query by email.
	if ids := queryIDs(t, srv, `{"accountId":"acct1","filter":{"email":"team@example.com"}}`); len(ids) != 1 || ids[0] != "team" {
		t.Fatalf("query by email ids = %v", ids)
	}

	// Changes via Memory journal since the initial state.
	resps = call(t, srv, `[["Principal/changes",{"accountId":"acct1","sinceState":"0"},"ch1"]]`)
	args = argsOf(t, resps[0])
	var created []string
	if err := json.Unmarshal(args["created"], &created); err != nil || len(created) != 3 {
		t.Fatalf("changes created = %s", args["created"])
	}
	if string(args["hasMoreChanges"]) != "false" {
		t.Fatalf("hasMoreChanges = %s", args["hasMoreChanges"])
	}
}

func TestPrincipalAndSharedWithJSON(t *testing.T) {
	// A zero Principal marshals capabilities as {} and nullable
	// fields as null.
	raw, err := json.Marshal(sharing.Principal{ID: "x", Type: "virtual"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"capabilities":{}`, `"name":null`, `"email":null`, `"timeZone":null`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("principal JSON %s missing %s", raw, want)
		}
	}

	// SharedWith wire shape: principalId -> right -> true.
	raw, err = json.Marshal(sharing.SharedWith{"alice": {"mayRead": true}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"alice":{"mayRead":true}}` {
		t.Fatalf("SharedWith JSON = %s", raw)
	}
}
