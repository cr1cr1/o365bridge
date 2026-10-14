package jmap_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cr1cr1/o365bridge/jmap"
)

// assertSemanticJSONEqual compares two JSON documents by value, not bytes,
// because Go map marshaling does not preserve key order.
func assertSemanticJSONEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not valid JSON: %v\n%s", err, got)
	}
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("JSON mismatch\nwant: %s\ngot:  %s", want, got)
	}
}

func roundTrip[T any](t *testing.T, input string, v *T) []byte {
	t.Helper()
	if err := json.Unmarshal([]byte(input), v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertSemanticJSONEqual(t, []byte(input), out)
	return out
}

// RFC 8620 §2.1 example Session object, reconstructed faithfully.
// The RFC rendering contains three editorial artefacts that cannot be
// transcribed literally: a missing comma after the "urn:ietf:params:jmap:mail"
// capability, "..." elisions inside accountCapabilities, and the field name
// "maxConcurrentRequest" (singular) where §2's normative text defines
// "maxConcurrentRequests" (plural). The elided contacts capability is an
// empty object here; the download/eventSource URLs are joined without the
// line-break introduced by RFC page formatting.
const sessionGolden = `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000,
      "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000,
      "maxConcurrentRequests": 8,
      "maxCallsInRequest": 32,
      "maxObjectsInGet": 256,
      "maxObjectsInSet": 128,
      "collationAlgorithms": ["i;ascii-numeric", "i;ascii-casemap", "i;unicode-casemap"]
    },
    "urn:ietf:params:jmap:mail": {},
    "urn:ietf:params:jmap:contacts": {},
    "https://example.com/apis/foobar": {"maxFoosFinangled": 42}
  },
  "accounts": {
    "A13824": {
      "name": "john@example.com",
      "isPersonal": true,
      "isReadOnly": false,
      "accountCapabilities": {
        "urn:ietf:params:jmap:mail": {"maxMailboxesPerEmail": null, "maxMailboxDepth": 10},
        "urn:ietf:params:jmap:contacts": {}
      }
    },
    "A97813": {
      "name": "jane@example.com",
      "isPersonal": false,
      "isReadOnly": true,
      "accountCapabilities": {
        "urn:ietf:params:jmap:mail": {"maxMailboxesPerEmail": 1, "maxMailboxDepth": 10}
      }
    }
  },
  "primaryAccounts": {
    "urn:ietf:params:jmap:mail": "A13824",
    "urn:ietf:params:jmap:contacts": "A13824"
  },
  "username": "john@example.com",
  "apiUrl": "https://jmap.example.com/api/",
  "downloadUrl": "https://jmap.example.com/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "https://jmap.example.com/upload/{accountId}/",
  "eventSourceUrl": "https://jmap.example.com/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
  "state": "75128aab4b1b"
}`

func TestSessionRoundTrip_RFC8620Example(t *testing.T) {
	var s jmap.Session
	roundTrip(t, sessionGolden, &s)

	if s.Username != "john@example.com" || s.State != "75128aab4b1b" {
		t.Fatalf("username/state = %q/%q", s.Username, s.State)
	}
	if s.PrimaryAccounts[jmap.MailURI] != "A13824" {
		t.Fatalf("primary mail account = %q", s.PrimaryAccounts[jmap.MailURI])
	}
	acct := s.Accounts["A97813"]
	if acct == nil || !acct.IsReadOnly || acct.IsPersonal || acct.Name != "jane@example.com" {
		t.Fatalf("shared account = %+v", acct)
	}
	if _, ok := s.Capabilities["https://example.com/apis/foobar"]; !ok {
		t.Fatalf("vendor capability lost: %v", s.Capabilities)
	}
	core, ok, err := s.CoreCapability()
	if err != nil || !ok {
		t.Fatalf("CoreCapability() ok=%v err=%v", ok, err)
	}
	if core.MaxSizeUpload != 50000000 || core.MaxConcurrentRequests != 8 ||
		core.MaxCallsInRequest != 32 || core.MaxObjectsInGet != 256 || core.MaxObjectsInSet != 128 {
		t.Fatalf("core capability = %+v", core)
	}
	if len(core.CollationAlgorithms) != 3 || core.CollationAlgorithms[2] != "i;unicode-casemap" {
		t.Fatalf("collations = %v", core.CollationAlgorithms)
	}
}

func TestInvocationIsThreeElementArray(t *testing.T) {
	inv, err := jmap.NewInvocation("Foo/get", map[string]any{"accountId": "A1"}, "c1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if want := `["Foo/get",{"accountId":"A1"},"c1"]`; string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}

	var back jmap.Invocation
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != "Foo/get" || back.CallID != "c1" || string(back.Args) != `{"accountId":"A1"}` {
		t.Fatalf("round trip = %+v args=%s", back, back.Args)
	}
	var args struct {
		AccountID string `json:"accountId"`
	}
	if err := back.DecodeArgs(&args); err != nil || args.AccountID != "A1" {
		t.Fatalf("DecodeArgs = %+v err=%v", args, err)
	}

	for _, bad := range []string{`["Foo/get",{}]`, `["Foo/get",{},"c1","extra"]`, `[42,{},"c1"]`, `"Foo/get"`} {
		var v jmap.Invocation
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want error", bad)
		}
	}
}

// RFC 8620 §3.3.1 example request.
func TestRequestRoundTrip_RFC8620Example(t *testing.T) {
	const input = `{
	  "using": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"],
	  "methodCalls": [
	    ["method1", {"arg1": "arg1data", "arg2": "arg2data"}, "c1"],
	    ["method2", {"arg1": "arg1data"}, "c2"],
	    ["method3", {}, "c3"]
	  ]
	}`
	var req jmap.Request
	roundTrip(t, input, &req)
	if len(req.Using) != 2 || req.Using[0] != jmap.CoreURI {
		t.Fatalf("using = %v", req.Using)
	}
	if len(req.MethodCalls) != 3 || req.MethodCalls[2].Name != "method3" || req.MethodCalls[0].CallID != "c1" {
		t.Fatalf("methodCalls = %+v", req.MethodCalls)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	const input = `{
	  "methodResponses": [["method1", {"arg1": 3}, "c1"], ["error", {"type": "unknownMethod"}, "c3"]],
	  "createdIds": {"c1": "A1"},
	  "sessionState": "75128aab4b1b"
	}`
	var resp jmap.Response
	roundTrip(t, input, &resp)
	if resp.SessionState != "75128aab4b1b" || resp.CreatedIDs["c1"] != "A1" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestMethodErrorJSONAndErrorInterface(t *testing.T) {
	err := jmap.NewInvalidArguments("bad ids")
	var asError error = err // must satisfy the Go error interface
	if asError.Error() == "" || err.Type != "invalidArguments" {
		t.Fatalf("error = %v type=%q", asError, err.Type)
	}
	got, mErr := json.Marshal(err)
	if mErr != nil {
		t.Fatal(mErr)
	}
	if want := `{"description":"bad ids","type":"invalidArguments"}`; string(got) != want {
		// Key order is not pinned; compare semantically.
		assertSemanticJSONEqual(t, []byte(want), got)
	}

	// Unknown extension fields survive a round trip.
	var decoded jmap.MethodError
	roundTrip(t, `{"type":"serverFail","description":"boom","retryAfter":30}`, &decoded)
	if decoded.Extra["retryAfter"] == nil || string(decoded.Extra["retryAfter"]) != "30" {
		t.Fatalf("extra lost: %+v", decoded)
	}

	cases := map[string]*jmap.MethodError{
		"invalidArguments":            jmap.NewInvalidArguments(""),
		"invalidResultReference":      jmap.NewInvalidResultReference(""),
		"forbidden":                   jmap.NewForbidden(""),
		"accountNotFound":             jmap.NewAccountNotFound(""),
		"accountNotSupportedByMethod": jmap.NewAccountNotSupportedByMethod(""),
		"accountReadOnly":             jmap.NewAccountReadOnly(""),
		"requestTooLarge":             jmap.NewRequestTooLarge(""),
		"stateMismatch":               jmap.NewStateMismatch(""),
		"unknownMethod":               jmap.NewUnknownMethod(""),
		"serverUnavailable":           jmap.NewServerUnavailable(""),
		"serverFail":                  jmap.NewServerFail(""),
		"serverPartialFail":           jmap.NewServerPartialFail(""),
		"cannotCalculateChanges":      jmap.NewCannotCalculateChanges(""),
		"anchorNotFound":              jmap.NewAnchorNotFound(""),
		"unsupportedFilter":           jmap.NewUnsupportedFilter(""),
		"unsupportedSort":             jmap.NewUnsupportedSort(""),
		"tooManyChanges":              jmap.NewTooManyChanges(""),
	}
	for want, e := range cases {
		if e.Type != want {
			t.Errorf("constructor type = %q, want %q", e.Type, want)
		}
	}
	// Request-level problem types (§3.6.1) are a different vocabulary used
	// with the urn:ietf:params:jmap:error: prefix, not MethodError types.
	if jmap.ProblemUnknownCapability != "urn:ietf:params:jmap:error:unknownCapability" {
		t.Fatalf("problem type = %q", jmap.ProblemUnknownCapability)
	}
}

func TestResultReferenceShape(t *testing.T) {
	var ref jmap.ResultReference
	roundTrip(t, `{"resultOf":"t1","name":"Email/get","path":"/list/*/threadId"}`, &ref)
	if ref.ResultOf != "t1" || ref.Name != "Email/get" || ref.Path != "/list/*/threadId" {
		t.Fatalf("ref = %+v", ref)
	}
}

type dummy struct {
	ID    jmap.ID `json:"id"`
	Title string  `json:"title"`
}

func TestStandardMethodTypesRoundTrip(t *testing.T) {
	var getArgs jmap.GetArgs
	roundTrip(t, `{"accountId":"A1","ids":["a","b"],"properties":["title"]}`, &getArgs)
	var getArgsNull jmap.GetArgs
	roundTrip(t, `{"accountId":"A1","ids":null,"properties":null}`, &getArgsNull)

	var getResp jmap.GetResponse[dummy]
	roundTrip(t, `{"accountId":"A1","state":"s1","list":[{"id":"a","title":"x"}],"notFound":["z"]}`, &getResp)
	if getResp.List[0].Title != "x" || getResp.NotFound[0] != "z" {
		t.Fatalf("getResp = %+v", getResp)
	}

	setArgsJSON := `{"accountId":"A1","ifInState":"s1","create":{"c1":{"id":"","title":"new"}},"update":{"a":{"title":"renamed","keywords/music":null}},"destroy":["d1"]}`
	var setArgs jmap.SetArgs[dummy]
	roundTrip(t, setArgsJSON, &setArgs)
	if setArgs.Create["c1"].Title != "new" || setArgs.Update["a"]["title"] != "renamed" {
		t.Fatalf("setArgs = %+v", setArgs)
	}

	var setResp jmap.SetResponse[dummy]
	roundTrip(t, `{"accountId":"A1","oldState":"s1","newState":"s2","created":{"c1":{"id":"a","title":"new"}},"updated":{"a":null},"destroyed":["d1"],"notCreated":null,"notUpdated":{"b":{"type":"notFound","description":"gone"}},"notDestroyed":null}`, &setResp)
	if setResp.NotUpdated["b"].Type != "notFound" {
		t.Fatalf("setResp = %+v", setResp)
	}

	var changes jmap.ChangesResponse
	roundTrip(t, `{"accountId":"A1","oldState":"s1","newState":"s2","hasMoreChanges":true,"created":["a"],"updated":["b"],"destroyed":["c"]}`, &changes)
	var changesArgs jmap.ChangesArgs
	roundTrip(t, `{"accountId":"A1","sinceState":"s1","maxChanges":50}`, &changesArgs)

	var query jmap.QueryResponse
	roundTrip(t, `{"accountId":"A1","queryState":"q1","canCalculateChanges":true,"position":0,"ids":["a","b"],"total":2}`, &query)
	var queryArgs jmap.QueryArgs
	roundTrip(t, `{"accountId":"A1","filter":{"hasKeyword":"music"},"sort":[{"property":"title","isAscending":false}],"position":0,"anchor":null,"anchorOffset":0,"limit":10,"calculateTotal":true}`, &queryArgs)
	if queryArgs.Sort[0].IsAscending == nil || *queryArgs.Sort[0].IsAscending {
		t.Fatalf("sort = %+v", queryArgs.Sort[0])
	}

	var qcArgs jmap.QueryChangesArgs
	roundTrip(t, `{"accountId":"A1","filter":null,"sort":null,"sinceQueryState":"q1","maxChanges":null,"upToId":"a","calculateTotal":false}`, &qcArgs)
	var qcResp jmap.QueryChangesResponse
	roundTrip(t, `{"accountId":"A1","oldQueryState":"q1","newQueryState":"q2","removed":["b"],"added":[{"id":"c","index":0}]}`, &qcResp)
	if qcResp.Added[0].ID != "c" || qcResp.Added[0].Index != 0 {
		t.Fatalf("qcResp = %+v", qcResp)
	}
}

func TestIDValidity(t *testing.T) {
	for _, id := range []jmap.ID{"A13824", "a-1_B"} {
		if !id.Valid() {
			t.Errorf("ID %q should be valid", id)
		}
	}
	for _, id := range []jmap.ID{"", "has space", "a/b", "a=b"} {
		if id.Valid() {
			t.Errorf("ID %q should be invalid", id)
		}
	}
}

func TestSetErrorShape(t *testing.T) {
	var e jmap.SetError
	roundTrip(t, `{"type":"invalidProperties","description":"bad","properties":["title"]}`, &e)
	var exists jmap.SetError
	roundTrip(t, `{"type":"alreadyExists","existingId":"A9"}`, &exists)
	if exists.ExistingID == nil || *exists.ExistingID != "A9" {
		t.Fatalf("existingId = %v", exists.ExistingID)
	}
}
