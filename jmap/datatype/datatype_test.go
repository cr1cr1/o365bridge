package datatype_test

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
)

type widget struct {
	ID   jmap.ID `json:"id"`
	Name string  `json:"name"`
	Size int     `json:"size"`
}

const widgetCap = jmap.URI("https://example.com/jmap/widgets")

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, _ *http.Request) (*server.Principal, error) {
	return &server.Principal{
		Username:        "user@example.com",
		Accounts:        map[jmap.ID]*jmap.Account{"acct1": {Name: "User", IsPersonal: true}},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.CoreURI: "acct1"},
	}, nil
}

func widgetMatch(obj widget, filter json.RawMessage) (bool, error) {
	var f struct {
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(filter, &f); err != nil {
		return false, err
	}
	if f.Name != nil {
		return obj.Name == *f.Name, nil
	}
	return true, nil
}

func widgetCompare(a, b widget, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		return strings.Compare(a.Name, b.Name), true
	case "size":
		switch {
		case a.Size < b.Size:
			return -1, true
		case a.Size > b.Size:
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := datatype.NewMemory[widget](
		func(w widget) jmap.ID { return w.ID },
		func(w *widget, id jmap.ID) { w.ID = id },
	)
	e := server.New(fakeAuth{}, server.Options{})
	datatype.Register[widget](e, datatype.Options[widget]{
		Name:       "Widget",
		Capability: widgetCap,
		Store:      store,
		IDOf:       func(w widget) jmap.ID { return w.ID },
		Match:      widgetMatch,
		Compare:    widgetCompare,
	})
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// call performs one API request with the given methodCalls JSON
// array literal and returns the decoded methodResponses as
// [name, args, callId] triples.
func call(t *testing.T, srv *httptest.Server, methodCalls string) (responses [][3]json.RawMessage, createdIDs map[string]string) {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core",%q],"methodCalls":%s}`,
		string(widgetCap), methodCalls)
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
		CreatedIDs      map[string]string    `json:"createdIds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.MethodResponses, decoded.CreatedIDs
}

func argsOf(t *testing.T, resp [3]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var name string
	if err := json.Unmarshal(resp[0], &name); err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(resp[1], &m); err != nil {
		t.Fatalf("response %s args not an object: %v", name, err)
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

func TestWidgetLifecycleOverHTTP(t *testing.T) {
	srv := newServer(t)

	// Capture pre-mutation state via a get (empty account: state "0").
	resps, _ := call(t, srv, `[["Widget/get",{"accountId":"acct1","ids":null},"g0"]]`)
	preState := string(argsOf(t, resps[0])["state"])
	if preState != `"0"` {
		t.Fatalf("pre-mutation state = %s, want \"0\"", preState)
	}

	// Create two widgets.
	resps, createdIDs := call(t, srv, `[["Widget/set",{"accountId":"acct1","create":{"c1":{"name":"alpha","size":3},"c2":{"name":"beta","size":1}}},"s1"]]`)
	args := argsOf(t, resps[0])
	var created map[string]widget
	if err := json.Unmarshal(args["created"], &created); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created["c1"].Name != "alpha" || created["c2"].Name != "beta" {
		t.Fatalf("created = %s", args["created"])
	}
	id1, id2 := string(created["c1"].ID), string(created["c2"].ID)
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("created ids = %q, %q", id1, id2)
	}
	// Deterministic sequential ids (sorted creation ids).
	if id1 != "1" || id2 != "2" {
		t.Fatalf("created ids = %q, %q, want 1, 2", id1, id2)
	}
	if createdIDs["c1"] != id1 || createdIDs["c2"] != id2 {
		t.Fatalf("response createdIds = %v, want c1=%s c2=%s", createdIDs, id1, id2)
	}
	if string(args["notCreated"]) != "null" {
		t.Fatalf("notCreated = %s, want null", args["notCreated"])
	}

	// Get by ids, with properties projection (unknown prop dropped).
	resps, _ = call(t, srv, fmt.Sprintf(
		`[["Widget/get",{"accountId":"acct1","ids":[%q,%q,"nope"],"properties":["name","bogus"]},"g1"]]`, id1, id2))
	args = argsOf(t, resps[0])
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(args["list"], &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("get list = %s", args["list"])
	}
	if string(list[0]["name"]) != `"alpha"` || string(list[0]["id"]) != `"1"` {
		t.Fatalf("projected[0] = %v", list[0])
	}
	if _, ok := list[0]["size"]; ok {
		t.Fatalf("size should be projected out: %v", list[0])
	}
	var notFound []string
	if err := json.Unmarshal(args["notFound"], &notFound); err != nil || len(notFound) != 1 || notFound[0] != "nope" {
		t.Fatalf("notFound = %s (err %v)", args["notFound"], err)
	}

	// Query with filter, then get via "#ids" result reference.
	resps, _ = call(t, srv, `[
		["Widget/query",{"accountId":"acct1","filter":{"name":"beta"},"calculateTotal":true},"q1"],
		["Widget/get",{"accountId":"acct1","#ids":{"resultOf":"q1","name":"Widget/query","path":"/ids"}},"g2"]
	]`)
	qargs := argsOf(t, resps[0])
	var qids []string
	if err := json.Unmarshal(qargs["ids"], &qids); err != nil || len(qids) != 1 || qids[0] != id2 {
		t.Fatalf("query ids = %s, want [%s]", qargs["ids"], id2)
	}
	if string(qargs["total"]) != "1" || string(qargs["canCalculateChanges"]) != "true" {
		t.Fatalf("query total/canCalculateChanges = %s / %s", qargs["total"], qargs["canCalculateChanges"])
	}
	gargs := argsOf(t, resps[1])
	var glist []widget
	if err := json.Unmarshal(gargs["list"], &glist); err != nil || len(glist) != 1 || glist[0].Name != "beta" {
		t.Fatalf("referenced get list = %s", gargs["list"])
	}

	// Anchor query: anchor at beta, offset -1 => alpha.
	resps, _ = call(t, srv, fmt.Sprintf(
		`[["Widget/query",{"accountId":"acct1","sort":[{"property":"name"}],"anchor":%q,"anchorOffset":-1},"q2"]]`, id2))
	qargs = argsOf(t, resps[0])
	qids = nil
	if err := json.Unmarshal(qargs["ids"], &qids); err != nil || len(qids) != 2 || qids[0] != id1 {
		t.Fatalf("anchor query ids = %s", qargs["ids"])
	}
	if string(qargs["position"]) != "0" {
		t.Fatalf("anchor query position = %s", qargs["position"])
	}

	// Unknown sort property => unsupportedSort method error.
	resps, _ = call(t, srv, `[["Widget/query",{"accountId":"acct1","sort":[{"property":"bogus"}]},"q3"]]`)
	if nameOf(t, resps[0]) != "error" {
		t.Fatalf("unknown sort response name = %s", nameOf(t, resps[0]))
	}
	if got := string(argsOf(t, resps[0])["type"]); got != `"unsupportedSort"` {
		t.Fatalf("unknown sort error type = %s", got)
	}

	// Wrong ifInState => stateMismatch.
	resps, _ = call(t, srv, `[["Widget/set",{"accountId":"acct1","ifInState":"999","create":{"cx":{"name":"x","size":9}}},"s2"]]`)
	if nameOf(t, resps[0]) != "error" || string(argsOf(t, resps[0])["type"]) != `"stateMismatch"` {
		t.Fatalf("ifInState response = %s %s", resps[0][0], resps[0][1])
	}

	// Current state for the following update.
	resps, _ = call(t, srv, `[["Widget/get",{"accountId":"acct1","ids":null},"g3"]]`)
	curStateRaw := argsOf(t, resps[0])["state"]
	var curState string
	if err := json.Unmarshal(curStateRaw, &curState); err != nil {
		t.Fatal(err)
	}

	// Update via plain + pointer patches, destroy the other widget.
	resps, _ = call(t, srv, fmt.Sprintf(
		`[["Widget/set",{"accountId":"acct1","ifInState":%q,"update":{%q:{"name":"alpha2"}},"destroy":[%q]},"s3"]]`,
		curState, id1, id2))
	args = argsOf(t, resps[0])
	var updated map[string]json.RawMessage
	if err := json.Unmarshal(args["updated"], &updated); err != nil || updated[id1] == nil {
		t.Fatalf("updated = %s", args["updated"])
	}
	var destroyed []string
	if err := json.Unmarshal(args["destroyed"], &destroyed); err != nil || len(destroyed) != 1 || destroyed[0] != id2 {
		t.Fatalf("destroyed = %s", args["destroyed"])
	}
	if string(args["oldState"]) != string(curStateRaw) {
		t.Fatalf("oldState = %s, want %s", args["oldState"], curStateRaw)
	}

	// Pointer-path patch on the survivor.
	resps, _ = call(t, srv, fmt.Sprintf(
		`[["Widget/set",{"accountId":"acct1","update":{%q:{"size":42}}},"s4"]]`, id1))
	if string(argsOf(t, resps[0])["notUpdated"]) != "null" {
		t.Fatalf("pointer patch notUpdated = %s", argsOf(t, resps[0])["notUpdated"])
	}
	resps, _ = call(t, srv, fmt.Sprintf(`[["Widget/get",{"accountId":"acct1","ids":[%q]},"g4"]]`, id1))
	var final []widget
	if err := json.Unmarshal(argsOf(t, resps[0])["list"], &final); err != nil || len(final) != 1 {
		t.Fatalf("final get = %s", argsOf(t, resps[0])["list"])
	}
	if final[0].Name != "alpha2" || final[0].Size != 42 {
		t.Fatalf("final widget = %+v", final[0])
	}

	// Update of a missing record => notUpdated notFound.
	resps, _ = call(t, srv, `[["Widget/set",{"accountId":"acct1","update":{"ghost":{"name":"g"}}},"s5"]]`)
	var notUpdated map[string]struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(argsOf(t, resps[0])["notUpdated"], &notUpdated); err != nil || notUpdated["ghost"].Type != "notFound" {
		t.Fatalf("notUpdated = %s", argsOf(t, resps[0])["notUpdated"])
	}

	// Changes since the pre-mutation state.
	resps, _ = call(t, srv, `[["Widget/changes",{"accountId":"acct1","sinceState":"0"},"ch1"]]`)
	args = argsOf(t, resps[0])
	var chCreated, chUpdated, chDestroyed []string
	_ = json.Unmarshal(args["created"], &chCreated)
	_ = json.Unmarshal(args["updated"], &chUpdated)
	_ = json.Unmarshal(args["destroyed"], &chDestroyed)
	if len(chCreated) != 2 || len(chUpdated) != 2 || len(chDestroyed) != 1 || chDestroyed[0] != id2 {
		t.Fatalf("changes created=%v updated=%v destroyed=%v", chCreated, chUpdated, chDestroyed)
	}
	if string(args["hasMoreChanges"]) != "false" {
		t.Fatalf("hasMoreChanges = %s", args["hasMoreChanges"])
	}

	// Bogus state => cannotCalculateChanges.
	resps, _ = call(t, srv, `[["Widget/changes",{"accountId":"acct1","sinceState":"bogus"},"ch2"]]`)
	if nameOf(t, resps[0]) != "error" || string(argsOf(t, resps[0])["type"]) != `"cannotCalculateChanges"` {
		t.Fatalf("bogus changes response = %s %s", resps[0][0], resps[0][1])
	}

	// queryChanges: Memory has no QueryChangesStore => cannotCalculateChanges.
	resps, _ = call(t, srv, `[["Widget/queryChanges",{"accountId":"acct1","sinceQueryState":"0"},"qc1"]]`)
	if nameOf(t, resps[0]) != "error" || string(argsOf(t, resps[0])["type"]) != `"cannotCalculateChanges"` {
		t.Fatalf("queryChanges response = %s %s", resps[0][0], resps[0][1])
	}
}

func TestQueryUnknownFilterAndPaging(t *testing.T) {
	srv := newServer(t)
	call(t, srv, `[["Widget/set",{"accountId":"acct1","create":{"a":{"name":"a","size":1},"b":{"name":"b","size":2},"c":{"name":"c","size":3}}},"s1"]]`)

	// Descending sort + position/limit paging.
	resps, _ := call(t, srv, `[["Widget/query",{"accountId":"acct1","sort":[{"property":"size","isAscending":false}],"position":1,"limit":1},"q1"]]`)
	args := argsOf(t, resps[0])
	var ids []string
	if err := json.Unmarshal(args["ids"], &ids); err != nil || len(ids) != 1 || ids[0] != "2" {
		t.Fatalf("paged ids = %s, want [2]", args["ids"])
	}
	if string(args["total"]) != "null" {
		t.Fatalf("total without calculateTotal = %s, want null", args["total"])
	}

	// Negative position counts from the end.
	resps, _ = call(t, srv, `[["Widget/query",{"accountId":"acct1","sort":[{"property":"size"}],"position":-1},"q2"]]`)
	ids = nil
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil || len(ids) != 1 || ids[0] != "3" {
		t.Fatalf("negative-position ids = %v", ids)
	}

	// Anchor that does not exist => anchorNotFound.
	resps, _ = call(t, srv, `[["Widget/query",{"accountId":"acct1","anchor":"ghost"},"q3"]]`)
	if nameOf(t, resps[0]) != "error" || string(argsOf(t, resps[0])["type"]) != `"anchorNotFound"` {
		t.Fatalf("anchor response = %s %s", resps[0][0], resps[0][1])
	}
}

func TestApplyPatch(t *testing.T) {
	type nested struct {
		ID   jmap.ID           `json:"id"`
		Name string            `json:"name"`
		Meta map[string]string `json:"meta"`
		Tags []string          `json:"tags"`
	}

	// Nested pointer path set + creation of a new nested object.
	out, err := datatype.ApplyPatch(nested{ID: "1", Name: "a", Meta: map[string]string{"x": "1"}},
		jmap.PatchObject{"meta/y": "2", "tags/0": "t0"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Meta["x"] != "1" || out.Meta["y"] != "2" || len(out.Tags) != 1 || out.Tags[0] != "t0" {
		t.Fatalf("patched = %+v", out)
	}

	// Null deletes a key (top-level and nested).
	out, err = datatype.ApplyPatch(out, jmap.PatchObject{"name": nil, "meta/x": nil})
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != "" {
		t.Fatalf("name not deleted/reset: %+v", out)
	}
	if _, ok := out.Meta["x"]; ok {
		t.Fatalf("meta/x not deleted: %+v", out)
	}
	if out.Meta["y"] != "2" {
		t.Fatalf("meta/y lost: %+v", out)
	}

	// Plain property patch.
	wout, err := datatype.ApplyPatch[widget](widget{ID: "7", Name: "n", Size: 1},
		jmap.PatchObject{"size": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if wout.Size != 5 || wout.Name != "n" {
		t.Fatalf("widget patched = %+v", wout)
	}

	// Descending into a scalar is an error.
	if _, err := datatype.ApplyPatch[widget](widget{ID: "7", Name: "n"},
		jmap.PatchObject{"name/sub": "x"}); err == nil {
		t.Fatal("expected error patching into a scalar")
	}
}
