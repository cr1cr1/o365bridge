package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// --- fakes ---

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, r *http.Request) (*server.Principal, error) {
	if r.Header.Get("Authorization") != "Bearer good" {
		return nil, fmt.Errorf("bad credentials")
	}
	return &server.Principal{
		Username: "user@example.com",
		Accounts: map[jmap.ID]*jmap.Account{
			"acct1": {Name: "User", IsPersonal: true},
			"acct2": {Name: "Second"},
		},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.CoreURI: "acct1"},
	}, nil
}

type fakeBlobs struct {
	mu sync.Mutex
	m  map[string][]byte
	ct map[string]string
	n  int
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{m: map[string][]byte{}, ct: map[string]string{}} }

func (f *fakeBlobs) Upload(_ context.Context, acct jmap.ID, ct string, data []byte) (jmap.ID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := jmap.ID(fmt.Sprintf("blob%d", f.n))
	f.m[string(acct)+"/"+string(id)] = append([]byte(nil), data...)
	f.ct[string(acct)+"/"+string(id)] = ct
	return id, nil
}

func (f *fakeBlobs) Download(_ context.Context, acct, blob jmap.ID) (string, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := string(acct) + "/" + string(blob)
	data, ok := f.m[k]
	if !ok {
		return "", nil, server.ErrNotFound
	}
	return f.ct[k], data, nil
}

func (f *fakeBlobs) Copy(_ context.Context, from, to, blob jmap.ID) (jmap.ID, error) {
	ct, data, err := f.Download(context.Background(), from, blob)
	if err != nil {
		return "", err
	}
	return f.Upload(context.Background(), to, ct, data)
}

type fakeSubs struct {
	mu sync.Mutex
	m  map[jmap.ID][]*server.PushSubscription
}

func newFakeSubs() *fakeSubs { return &fakeSubs{m: map[jmap.ID][]*server.PushSubscription{}} }

func (f *fakeSubs) List(_ context.Context, acct jmap.ID) ([]*server.PushSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*server.PushSubscription(nil), f.m[acct]...), nil
}

func (f *fakeSubs) Upsert(_ context.Context, acct jmap.ID, sub *server.PushSubscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.m[acct] {
		if s.ID == sub.ID {
			cp := *sub
			f.m[acct][i] = &cp
			return nil
		}
	}
	cp := *sub
	f.m[acct] = append(f.m[acct], &cp)
	return nil
}

func (f *fakeSubs) Delete(_ context.Context, acct, id jmap.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.m[acct] {
		if s.ID == id {
			f.m[acct] = append(f.m[acct][:i], f.m[acct][i+1:]...)
			return nil
		}
	}
	return server.ErrNotFound
}

// --- helpers ---

func newEngine(t *testing.T, opts server.Options) (*server.Engine, *httptest.Server) {
	t.Helper()
	if opts.Blobs == nil && opts.Subscriptions == nil {
		// most tests want both; individual tests may override by
		// constructing their own engine instead
	}
	e := server.New(fakeAuth{}, opts)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return e, srv
}

func doReq(t *testing.T, method, url, ct string, body []byte, auth bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer good")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

type apiResp struct {
	MethodResponses []struct {
		Name   string
		Args   json.RawMessage
		CallID string
	} `json:"-"`
	raw struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
		CreatedIDs      map[string]string   `json:"createdIds"`
		SessionState    string              `json:"sessionState"`
	}
}

func postAPI(t *testing.T, srv *httptest.Server, reqBody any) (int, *apiResp, []byte) {
	t.Helper()
	var body []byte
	switch v := reqBody.(type) {
	case []byte:
		body = v
	case string:
		body = []byte(v)
	default:
		body, _ = json.Marshal(v)
	}
	resp := doReq(t, "POST", srv.URL+"/jmap/", "application/json", body, true)
	data, _ := io.ReadAll(resp.Body)
	out := &apiResp{}
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(data, &out.raw); err != nil {
			t.Fatalf("decode response: %v (%s)", err, data)
		}
		for _, mr := range out.raw.MethodResponses {
			var name, callID string
			_ = json.Unmarshal(mr[0], &name)
			_ = json.Unmarshal(mr[2], &callID)
			out.MethodResponses = append(out.MethodResponses, struct {
				Name   string
				Args   json.RawMessage
				CallID string
			}{name, mr[1], callID})
		}
	}
	return resp.StatusCode, out, data
}

func call(name string, args map[string]any, id string) []any {
	return []any{name, args, id}
}

// --- tests ---

func TestWellKnownAndSession(t *testing.T) {
	e, srv := newEngine(t, server.Options{BaseURL: "http://example.test", Blobs: newFakeBlobs(), Subscriptions: newFakeSubs()})
	e.RegisterCapability("urn:example:test", map[string]any{"x": 1}, map[string]any{})

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", srv.URL+"/.well-known/jmap", nil)
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/jmap/session" {
		t.Fatalf("well-known: %d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
	}

	r := doReq(t, "GET", srv.URL+"/jmap/session", "", nil, true)
	data, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 {
		t.Fatalf("session status %d: %s", r.StatusCode, data)
	}
	var s jmap.Session
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Username != "user@example.com" {
		t.Errorf("username %q", s.Username)
	}
	if _, ok := s.Capabilities[jmap.CoreURI]; !ok {
		t.Error("core capability missing")
	}
	if _, ok := s.Capabilities["urn:example:test"]; !ok {
		t.Error("registered capability missing")
	}
	if _, ok := s.Capabilities[jmap.MailURI]; ok {
		t.Error("unregistered mail capability advertised")
	}
	if s.APIURL != "http://example.test/jmap/" {
		t.Errorf("apiUrl %q", s.APIURL)
	}
	if !strings.Contains(s.UploadURL, "{accountId}") || !strings.Contains(s.DownloadURL, "{blobId}") {
		t.Errorf("templated urls: upload=%q download=%q", s.UploadURL, s.DownloadURL)
	}
	if !strings.Contains(s.EventSourceURL, "{types}") {
		t.Errorf("eventSourceUrl %q", s.EventSourceURL)
	}
	if s.Accounts["acct1"] == nil || s.PrimaryAccounts[jmap.CoreURI] != "acct1" {
		t.Errorf("accounts/primary: %v %v", s.Accounts, s.PrimaryAccounts)
	}
	core, ok, err := s.CoreCapability()
	if err != nil || !ok || core.MaxCallsInRequest == 0 {
		t.Errorf("core cap: %+v ok=%v err=%v", core, ok, err)
	}
}

func TestUnauthorized(t *testing.T) {
	_, srv := newEngine(t, server.Options{})
	r := doReq(t, "GET", srv.URL+"/jmap/session", "", nil, false)
	io.ReadAll(r.Body)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session without auth: %d", r.StatusCode)
	}
	r2 := doReq(t, "POST", srv.URL+"/jmap/", "application/json", []byte(`{"using":[],"methodCalls":[]}`), false)
	io.ReadAll(r2.Body)
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api without auth: %d", r2.StatusCode)
	}
}

func TestAPIEchoUnknownAndCapability(t *testing.T) {
	_, srv := newEngine(t, server.Options{})
	status, resp, _ := postAPI(t, srv, map[string]any{
		"using": []string{string(jmap.CoreURI)},
		"methodCalls": []any{
			call("Core/echo", map[string]any{"helloWorld": "hi"}, "0"),
			call("Nope/missing", map[string]any{}, "1"),
		},
	})
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	var echo map[string]any
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &echo); err != nil || echo["helloWorld"] != "hi" {
		t.Fatalf("echo: %s err=%v", resp.MethodResponses[0].Args, err)
	}
	var me jmap.MethodError
	if err := json.Unmarshal(resp.MethodResponses[1].Args, &me); err != nil || me.Type != jmap.ErrorUnknownMethod {
		t.Fatalf("unknown method: %s err=%v", resp.MethodResponses[1].Args, err)
	}
	if resp.MethodResponses[1].Name != "error" {
		t.Fatalf("error invocation name %q", resp.MethodResponses[1].Name)
	}

	// Method whose capability was not put in using.
	e2, srv2 := newEngine(t, server.Options{})
	e2.RegisterCapability("urn:example:test", map[string]any{}, nil)
	e2.RegisterMethod("Test/thing", "urn:example:test", func(_ context.Context, c *server.Call) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	_, resp2, _ := postAPI(t, srv2, map[string]any{
		"using":       []string{string(jmap.CoreURI)},
		"methodCalls": []any{call("Test/thing", map[string]any{}, "0")},
	})
	var me2 jmap.MethodError
	_ = json.Unmarshal(resp2.MethodResponses[0].Args, &me2)
	if me2.Type != "unknownCapability" {
		t.Fatalf("capability method error: %s", resp2.MethodResponses[0].Args)
	}
}

func TestResultReferences(t *testing.T) {
	e, srv := newEngine(t, server.Options{})
	e.RegisterMethod("Test/create", jmap.CoreURI, func(_ context.Context, c *server.Call) (any, error) {
		c.SetCreated("c1", "srv1")
		return map[string]any{"ids": []string{"x1", "x2"}, "list": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}, nil
	})
	var gotArgs json.RawMessage
	e.RegisterMethod("Test/get", jmap.CoreURI, func(_ context.Context, c *server.Call) (any, error) {
		gotArgs = append(json.RawMessage(nil), c.Args...)
		return map[string]any{"ok": true}, nil
	})

	status, resp, raw := postAPI(t, srv, map[string]any{
		"using": []string{string(jmap.CoreURI)},
		"methodCalls": []any{
			call("Test/create", map[string]any{}, "0"),
			[]any{"Test/get", map[string]any{"#ids": map[string]any{"resultOf": "0", "name": "Test/create", "path": "/ids"}}, "1"},
			[]any{"Test/get", map[string]any{"#bad": map[string]any{"resultOf": "9", "name": "Test/create", "path": "/ids"}}, "2"},
			[]any{"Test/get", map[string]any{"#first": map[string]any{"resultOf": "0", "name": "Test/create", "path": "/list/*/id"}}, "3"},
		},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	var got struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(gotArgs, &got); err != nil {
		// gotArgs is from last successful call (wildcard); check below via separate unmarshal
		_ = err
	}
	// The wildcard call was last; its args must hold first=["a","b"].
	var wild struct {
		First []string `json:"first"`
	}
	if err := json.Unmarshal(gotArgs, &wild); err != nil || len(wild.First) != 2 || wild.First[0] != "a" {
		t.Fatalf("wildcard args: %s err=%v", gotArgs, err)
	}
	// createdIds propagated to response.
	if resp.raw.CreatedIDs["c1"] != "srv1" {
		t.Fatalf("createdIds: %v", resp.raw.CreatedIDs)
	}
	// Invalid reference -> invalidResultReference, others proceeded (4 responses).
	if len(resp.MethodResponses) != 4 {
		t.Fatalf("responses: %d", len(resp.MethodResponses))
	}
	var me jmap.MethodError
	_ = json.Unmarshal(resp.MethodResponses[2].Args, &me)
	if me.Type != jmap.ErrorInvalidResultReference {
		t.Fatalf("invalid ref error: %s", resp.MethodResponses[2].Args)
	}
	if resp.MethodResponses[3].Name != "Test/get" {
		t.Fatalf("call after invalid ref not processed: %s", resp.MethodResponses[3].Args)
	}
}

func TestRequestLevelProblems(t *testing.T) {
	_, srv := newEngine(t, server.Options{})

	status, _, data := postAPI(t, srv, "{not json")
	var prob jmap.Problem
	_ = json.Unmarshal(data, &prob)
	if status != 400 || prob.Type != jmap.ProblemNotJSON {
		t.Fatalf("notJSON: %d %s", status, data)
	}

	status, _, data = postAPI(t, srv, map[string]any{"foo": 1})
	_ = json.Unmarshal(data, &prob)
	if status != 400 || prob.Type != jmap.ProblemNotRequest {
		t.Fatalf("notRequest: %d %s", status, data)
	}

	status, _, data = postAPI(t, srv, map[string]any{
		"using":       []string{"urn:example:unknown"},
		"methodCalls": []any{},
	})
	_ = json.Unmarshal(data, &prob)
	if status != 400 || prob.Type != jmap.ProblemUnknownCapability {
		t.Fatalf("unknownCapability: %d %s", status, data)
	}
}

func TestBlobRoundTripAndCopy(t *testing.T) {
	_, srv := newEngine(t, server.Options{Blobs: newFakeBlobs()})

	r := doReq(t, "POST", srv.URL+"/jmap/upload/acct1", "text/plain", []byte("hello blob"), true)
	data, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.StatusCode, data)
	}
	var up struct {
		AccountID string `json:"accountId"`
		BlobID    string `json:"blobId"`
		Type      string `json:"type"`
		Size      int    `json:"size"`
	}
	if err := json.Unmarshal(data, &up); err != nil || up.BlobID == "" || up.Size != 10 || up.Type != "text/plain" {
		t.Fatalf("upload resp: %s err=%v", data, err)
	}

	r = doReq(t, "GET", srv.URL+"/jmap/download/acct1/"+up.BlobID+"/greeting.txt", "", nil, true)
	data, _ = io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(data) != "hello blob" || r.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("download: %d %q ct=%q", r.StatusCode, data, r.Header.Get("Content-Type"))
	}

	r = doReq(t, "GET", srv.URL+"/jmap/download/acct1/nope/x.txt", "", nil, true)
	io.ReadAll(r.Body)
	if r.StatusCode != 404 {
		t.Fatalf("missing blob: %d", r.StatusCode)
	}

	_, resp, _ := postAPI(t, srv, map[string]any{
		"using": []string{string(jmap.CoreURI)},
		"methodCalls": []any{
			call("Blob/copy", map[string]any{"fromAccountId": "acct1", "toAccountId": "acct2", "blobIds": []string{up.BlobID, "missing"}}, "0"),
		},
	})
	var copyResp struct {
		Copied    map[string]string         `json:"copied"`
		NotCopied map[string]*jmap.SetError `json:"notCopied"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &copyResp); err != nil {
		t.Fatal(err)
	}
	newID := copyResp.Copied[up.BlobID]
	if newID == "" || copyResp.NotCopied["missing"] == nil || copyResp.NotCopied["missing"].Type != "blobNotFound" {
		t.Fatalf("copy resp: %s", resp.MethodResponses[0].Args)
	}
	r = doReq(t, "GET", srv.URL+"/jmap/download/acct2/"+newID+"/g.txt", "", nil, true)
	data, _ = io.ReadAll(r.Body)
	if string(data) != "hello blob" {
		t.Fatalf("copied download: %q", data)
	}
}

func TestEventSourceReceivesPublish(t *testing.T) {
	e, srv := newEngine(t, server.Options{})
	req, _ := http.NewRequest("GET", srv.URL+"/jmap/eventsource/?ping=30", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	reader := bufio.NewReader(resp.Body)
	// Initial comment proves the handler is subscribed.
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, ":") {
		t.Fatalf("initial line %q err=%v", line, err)
	}
	e.Publish(server.StateChange{AccountID: "acct1", Changed: map[string]map[string]string{"acct1": {"Mailbox": "s42"}}})

	deadline := time.Now().Add(5 * time.Second)
	var dataLine string
	for time.Now().Before(deadline) {
		l, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(l, "event:") && !strings.Contains(l, "state") {
			t.Fatalf("event line %q", l)
		}
		if strings.HasPrefix(l, "data:") {
			dataLine = strings.TrimSpace(strings.TrimPrefix(l, "data:"))
			break
		}
	}
	if dataLine == "" {
		t.Fatal("no SSE data received")
	}
	var sc struct {
		Type    string                       `json:"@type"`
		Changed map[string]map[string]string `json:"changed"`
	}
	if err := json.Unmarshal([]byte(dataLine), &sc); err != nil || sc.Type != "StateChange" || sc.Changed["acct1"]["Mailbox"] != "s42" {
		t.Fatalf("sse payload %q err=%v", dataLine, err)
	}
}

func TestPushSubscriptionSetVerifyAndPublish(t *testing.T) {
	received := make(chan map[string]any, 10)
	pushSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		received <- m
		w.WriteHeader(201)
	}))
	defer pushSrv.Close()

	subs := newFakeSubs()
	e, srv := newEngine(t, server.Options{Subscriptions: subs})

	_, resp, raw := postAPI(t, srv, map[string]any{
		"using": []string{string(jmap.CoreURI)},
		"methodCalls": []any{
			call("PushSubscription/set", map[string]any{
				"create": map[string]any{"c1": map[string]any{"deviceClientId": "dev1", "url": pushSrv.URL}},
			}, "0"),
		},
	})
	var setResp struct {
		Created map[string]*server.PushSubscription `json:"created"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &setResp); err != nil || setResp.Created["c1"] == nil || setResp.Created["c1"].ID == "" {
		t.Fatalf("set resp: %s err=%v", raw, err)
	}
	if resp.raw.CreatedIDs["c1"] != string(setResp.Created["c1"].ID) {
		t.Fatalf("createdIds from set: %v", resp.raw.CreatedIDs)
	}

	select {
	case m := <-received:
		if m["@type"] != "PushVerification" || m["pushSubscriptionId"] != string(setResp.Created["c1"].ID) || m["verificationCode"] == "" || m["verificationCode"] == nil {
			t.Fatalf("verification payload: %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no verification POST")
	}

	// get returns it.
	_, resp, _ = postAPI(t, srv, map[string]any{
		"using":       []string{string(jmap.CoreURI)},
		"methodCalls": []any{call("PushSubscription/get", map[string]any{"ids": nil}, "0")},
	})
	var getResp struct {
		List  []*server.PushSubscription `json:"list"`
		State string                     `json:"state"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &getResp); err != nil || len(getResp.List) != 1 {
		t.Fatalf("get resp: %s err=%v", resp.MethodResponses[0].Args, err)
	}

	e.Publish(server.StateChange{AccountID: "acct1", Changed: map[string]map[string]string{"acct1": {"Email": "s9"}}})
	select {
	case m := <-received:
		if m["@type"] != "StateChange" {
			t.Fatalf("publish payload: %v", m)
		}
		changed, _ := m["changed"].(map[string]any)
		acct, _ := changed["acct1"].(map[string]any)
		if acct["Email"] != "s9" {
			t.Fatalf("publish changed: %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no StateChange POST from Publish")
	}
}
