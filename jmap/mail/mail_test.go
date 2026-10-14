package mail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/mail"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, _ *http.Request) (*server.Principal, error) {
	return &server.Principal{
		Username:        "user@example.com",
		Accounts:        map[jmap.ID]*jmap.Account{"acct1": {Name: "User", IsPersonal: true}},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.CoreURI: "acct1", jmap.MailURI: "acct1"},
	}, nil
}

type backend struct {
	mailboxes *datatype.Memory[mail.Mailbox]
	emails    *datatype.Memory[mail.Email]
	threads   []mail.Thread
}

func (b *backend) Mailboxes() datatype.Store[mail.Mailbox] { return b.mailboxes }
func (b *backend) Emails() datatype.Store[mail.Email]      { return b.emails }

func (b *backend) Threads(_ context.Context, _ jmap.ID, ids []jmap.ID) ([]mail.Thread, []jmap.ID, string, error) {
	if ids == nil {
		return b.threads, nil, "1", nil
	}
	var found []mail.Thread
	var notFound []jmap.ID
	for _, id := range ids {
		ok := false
		for _, th := range b.threads {
			if th.ID == id {
				found = append(found, th)
				ok = true
				break
			}
		}
		if !ok {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, "1", nil
}

func strptr(s string) *string { return &s }

func seed(t *testing.T) (*httptest.Server, *backend) {
	t.Helper()
	b := &backend{
		mailboxes: datatype.NewMemory[mail.Mailbox](
			func(m mail.Mailbox) jmap.ID { return m.ID },
			func(m *mail.Mailbox, id jmap.ID) { m.ID = id }),
		emails: datatype.NewMemory[mail.Email](
			func(e mail.Email) jmap.ID { return e.ID },
			func(e *mail.Email, id jmap.ID) { e.ID = id }),
		threads: []mail.Thread{
			{ID: "t1", EmailIDs: []jmap.ID{"e1", "e2"}},
			{ID: "t2", EmailIDs: []jmap.ID{"e3"}},
		},
	}
	ctx := context.Background()
	rights := &mail.MailboxRights{MayReadItems: true, MayAddItems: true, MayRemoveItems: true, MaySetSeen: true, MaySetKeywords: true, MayCreateChild: true, MayRename: true, MayDelete: true}
	for _, m := range []mail.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: strptr("inbox"), SortOrder: 0, TotalEmails: 2, UnreadEmails: 1, MyRights: rights, IsSubscribed: true},
		{ID: "mb-archive", Name: "Archive", Role: strptr("archive"), SortOrder: 10, TotalEmails: 1, MyRights: rights, IsSubscribed: true},
	} {
		if _, serr := b.mailboxes.Create(ctx, "acct1", m); serr != nil {
			t.Fatal(serr)
		}
	}
	alice := []mail.EmailAddress{{Name: "Alice", Email: "alice@example.com"}}
	bob := []mail.EmailAddress{{Name: "Bob", Email: "bob@example.com"}}
	me := []mail.EmailAddress{{Name: "Me", Email: "user@example.com"}}
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	for _, e := range []mail.Email{
		{ID: "e1", BlobID: "b1", ThreadID: "t1", MailboxIDs: map[jmap.ID]bool{"mb-inbox": true},
			Keywords: map[string]bool{"$seen": true}, Size: 100, ReceivedAt: base.Add(48 * time.Hour),
			MessageID: []string{"<m1@example.com>"}, From: alice, To: me, Subject: "Hello World", Preview: "hello preview text"},
		{ID: "e2", BlobID: "b2", ThreadID: "t1", MailboxIDs: map[jmap.ID]bool{"mb-inbox": true},
			Keywords: map[string]bool{}, Size: 500, ReceivedAt: base.Add(24 * time.Hour),
			MessageID: []string{"<m2@example.com>"}, From: bob, To: me, Subject: "Invoice due", Preview: "please pay now", HasAttachment: true},
		{ID: "e3", BlobID: "b3", ThreadID: "t2", MailboxIDs: map[jmap.ID]bool{"mb-archive": true},
			Keywords: map[string]bool{"$seen": true, "$flagged": true}, Size: 300, ReceivedAt: base,
			MessageID: []string{"<m3@example.com>"}, From: alice, To: me, Subject: "Hello again", Preview: "another hello"},
	} {
		if _, serr := b.emails.Create(ctx, "acct1", e); serr != nil {
			t.Fatal(serr)
		}
	}
	e := server.New(fakeAuth{}, server.Options{})
	mail.Register(e, b)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv, b
}

func call(t *testing.T, srv *httptest.Server, methodCalls string) [][3]json.RawMessage {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":%s}`, methodCalls)
	resp, err := http.Post(srv.URL+"/jmap/", "application/json", strings.NewReader(body))
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
		t.Fatalf("args not object: %v (%s)", err, resp[1])
	}
	return m
}

func nameOf(resp [3]json.RawMessage) string {
	var n string
	_ = json.Unmarshal(resp[0], &n)
	return n
}

func queryIDs(t *testing.T, srv *httptest.Server, method, args string) []string {
	t.Helper()
	resps := call(t, srv, fmt.Sprintf(`[[%q,{%s},"q"]]`, method, args))
	if nameOf(resps[0]) == "error" {
		t.Fatalf("%s error: %s", method, resps[0][1])
	}
	var ids []string
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestMailboxQueryRoleFilter(t *testing.T) {
	srv, _ := seed(t)
	ids := queryIDs(t, srv, "Mailbox/query", `"accountId":"acct1","filter":{"role":"inbox"}`)
	if len(ids) != 1 || ids[0] != "mb-inbox" {
		t.Fatalf("role=inbox ids = %v", ids)
	}
	// Operator recursion + exact name.
	ids = queryIDs(t, srv, "Mailbox/query", `"accountId":"acct1","filter":{"operator":"AND","conditions":[{"hasAnyRole":true},{"name":"Archive"}]}`)
	if len(ids) != 1 || ids[0] != "mb-archive" {
		t.Fatalf("AND ids = %v", ids)
	}
	// Unknown condition => invalidArguments.
	resps := call(t, srv, `[["Mailbox/query",{"accountId":"acct1","filter":{"bogus":1}},"q"]]`)
	if nameOf(resps[0]) != "error" || string(argsOf(t, resps[0])["type"]) != `"invalidArguments"` {
		t.Fatalf("unknown filter resp = %s %s", resps[0][0], resps[0][1])
	}
	// Mailbox/get JSON shape.
	resps = call(t, srv, `[["Mailbox/get",{"accountId":"acct1","ids":["mb-inbox"]},"g"]]`)
	var list []mail.Mailbox
	if err := json.Unmarshal(argsOf(t, resps[0])["list"], &list); err != nil || len(list) != 1 {
		t.Fatalf("mailbox get = %s err %v", argsOf(t, resps[0])["list"], err)
	}
	if list[0].Name != "Inbox" || list[0].Role == nil || *list[0].Role != "inbox" || list[0].ParentID != nil || list[0].MyRights == nil || !list[0].MyRights.MayReadItems {
		t.Fatalf("mailbox = %+v", list[0])
	}
	if !strings.Contains(string(argsOf(t, resps[0])["list"]), `"parentId":null`) ||
		!strings.Contains(string(argsOf(t, resps[0])["list"]), `"totalEmails":2`) {
		t.Fatalf("mailbox JSON = %s", argsOf(t, resps[0])["list"])
	}
}

func TestEmailQueryFilters(t *testing.T) {
	srv, _ := seed(t)
	ids := queryIDs(t, srv, "Email/query", `"accountId":"acct1","filter":{"inMailbox":"mb-inbox","hasKeyword":"$seen"}`)
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("inMailbox+hasKeyword ids = %v", ids)
	}
	ids = queryIDs(t, srv, "Email/query", `"accountId":"acct1","filter":{"text":"HELLO"}`)
	if len(ids) != 2 { // e1, e3 (subject/preview, case-insensitive)
		t.Fatalf("text=HELLO ids = %v", ids)
	}
	ids = queryIDs(t, srv, "Email/query", `"accountId":"acct1","filter":{"operator":"OR","conditions":[{"from":"bob@"},{"subject":"again"}]}`)
	if len(ids) != 2 {
		t.Fatalf("OR from/subject ids = %v", ids)
	}
	ids = queryIDs(t, srv, "Email/query", `"accountId":"acct1","filter":{"header":["Message-ID","m2@"]}`)
	if len(ids) != 1 || ids[0] != "e2" {
		t.Fatalf("header ids = %v", ids)
	}
	// Other headers never match (documented shortcut).
	ids = queryIDs(t, srv, "Email/query", `"accountId":"acct1","filter":{"header":["X-Custom","x"]}`)
	if len(ids) != 0 {
		t.Fatalf("unknown header ids = %v", ids)
	}
}

func TestEmailQuerySortSizeDescLimit(t *testing.T) {
	srv, _ := seed(t)
	ids := queryIDs(t, srv, "Email/query", `"accountId":"acct1","sort":[{"property":"size","isAscending":false}],"limit":2`)
	if len(ids) != 2 || ids[0] != "e2" || ids[1] != "e3" {
		t.Fatalf("size desc limit ids = %v", ids)
	}
	ids = queryIDs(t, srv, "Email/query", `"accountId":"acct1","sort":[{"property":"receivedAt"}]`)
	if len(ids) != 3 || ids[0] != "e3" || ids[2] != "e1" {
		t.Fatalf("receivedAt asc ids = %v", ids)
	}
}

func TestEmailGetPropertiesProjection(t *testing.T) {
	srv, _ := seed(t)
	resps := call(t, srv, `[["Email/get",{"accountId":"acct1","ids":["e1"],"properties":["subject","keywords"]},"g"]]`)
	args := argsOf(t, resps[0])
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(args["list"], &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %s", args["list"])
	}
	if string(list[0]["subject"]) != `"Hello World"` || string(list[0]["id"]) != `"e1"` {
		t.Fatalf("projected = %v", list[0])
	}
	var kw map[string]bool
	if err := json.Unmarshal(list[0]["keywords"], &kw); err != nil || !kw["$seen"] {
		t.Fatalf("keywords = %s", list[0]["keywords"])
	}
	if _, ok := list[0]["preview"]; ok {
		t.Fatalf("preview should be projected out: %v", list[0])
	}
}

func TestEmailSetUpdateKeywordsPatch(t *testing.T) {
	srv, _ := seed(t)
	resps := call(t, srv, `[["Email/set",{"accountId":"acct1","update":{"e2":{"keywords/$seen":true,"keywords/$flagged":true}}},"s"]]`)
	args := argsOf(t, resps[0])
	if string(args["notUpdated"]) != "null" {
		t.Fatalf("notUpdated = %s", args["notUpdated"])
	}
	resps = call(t, srv, `[["Email/get",{"accountId":"acct1","ids":["e2"],"properties":["keywords"]},"g"]]`)
	var list []mail.Email
	if err := json.Unmarshal(argsOf(t, resps[0])["list"], &list); err != nil || len(list) != 1 {
		t.Fatalf("get = %s", argsOf(t, resps[0])["list"])
	}
	if !list[0].Keywords["$seen"] || !list[0].Keywords["$flagged"] {
		t.Fatalf("keywords after patch = %v", list[0].Keywords)
	}
}

func TestMailboxSetCreateRenameDestroy(t *testing.T) {
	srv, _ := seed(t)
	resps := call(t, srv, `[["Mailbox/set",{"accountId":"acct1","create":{"c1":{"name":"Projects","parentId":null,"role":null}}},"s"]]`)
	args := argsOf(t, resps[0])
	var created map[string]mail.Mailbox
	if err := json.Unmarshal(args["created"], &created); err != nil || created["c1"].ID == "" {
		t.Fatalf("created = %s err %v", args["created"], err)
	}
	id := string(created["c1"].ID)

	resps = call(t, srv, fmt.Sprintf(`[["Mailbox/set",{"accountId":"acct1","update":{%q:{"name":"Projects 2026"}}},"s"]]`, id))
	if string(argsOf(t, resps[0])["notUpdated"]) != "null" {
		t.Fatalf("rename notUpdated = %s", argsOf(t, resps[0])["notUpdated"])
	}
	ids := queryIDs(t, srv, "Mailbox/query", `"accountId":"acct1","filter":{"name":"Projects 2026"}`)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("renamed query ids = %v", ids)
	}

	resps = call(t, srv, fmt.Sprintf(`[["Mailbox/set",{"accountId":"acct1","destroy":[%q]},"s"]]`, id))
	var destroyed []string
	if err := json.Unmarshal(argsOf(t, resps[0])["destroyed"], &destroyed); err != nil || len(destroyed) != 1 {
		t.Fatalf("destroyed = %s", argsOf(t, resps[0])["destroyed"])
	}
	resps = call(t, srv, fmt.Sprintf(`[["Mailbox/get",{"accountId":"acct1","ids":[%q]},"g"]]`, id))
	var notFound []string
	_ = json.Unmarshal(argsOf(t, resps[0])["notFound"], &notFound)
	if len(notFound) != 1 || notFound[0] != id {
		t.Fatalf("notFound after destroy = %s", argsOf(t, resps[0])["notFound"])
	}
}

func TestThreadGet(t *testing.T) {
	srv, _ := seed(t)
	resps := call(t, srv, `[["Thread/get",{"accountId":"acct1","ids":["t1","nope"]},"g"]]`)
	args := argsOf(t, resps[0])
	var list []mail.Thread
	if err := json.Unmarshal(args["list"], &list); err != nil || len(list) != 1 {
		t.Fatalf("thread list = %s", args["list"])
	}
	if list[0].ID != "t1" || len(list[0].EmailIDs) != 2 || list[0].EmailIDs[0] != "e1" || list[0].EmailIDs[1] != "e2" {
		t.Fatalf("thread = %+v", list[0])
	}
	if !strings.Contains(string(args["list"]), `"emailIds":["e1","e2"]`) {
		t.Fatalf("thread JSON = %s", args["list"])
	}
	var notFound []string
	_ = json.Unmarshal(args["notFound"], &notFound)
	if len(notFound) != 1 || notFound[0] != "nope" {
		t.Fatalf("thread notFound = %s", args["notFound"])
	}
	if string(args["state"]) != `"1"` {
		t.Fatalf("thread state = %s", args["state"])
	}
}

func TestEmailChangesViaMemoryJournal(t *testing.T) {
	srv, _ := seed(t)
	resps := call(t, srv, `[["Email/get",{"accountId":"acct1","ids":null},"g"]]`)
	var state string
	if err := json.Unmarshal(argsOf(t, resps[0])["state"], &state); err != nil {
		t.Fatal(err)
	}
	// Create a new email after that state.
	resps = call(t, srv, `[["Email/set",{"accountId":"acct1","create":{"c9":{"blobId":"b9","threadId":"t2","mailboxIds":{"mb-inbox":true},"keywords":{},"size":42,"receivedAt":"2024-01-04T10:00:00Z","subject":"Fresh","preview":"fresh"}}},"s"]]`)
	args := argsOf(t, resps[0])
	var created map[string]mail.Email
	if err := json.Unmarshal(args["created"], &created); err != nil || created["c9"].ID == "" {
		t.Fatalf("created = %s", args["created"])
	}
	newID := string(created["c9"].ID)

	resps = call(t, srv, fmt.Sprintf(`[["Email/changes",{"accountId":"acct1","sinceState":%q},"ch"]]`, state))
	args = argsOf(t, resps[0])
	var chCreated []string
	_ = json.Unmarshal(args["created"], &chCreated)
	if len(chCreated) != 1 || chCreated[0] != newID {
		t.Fatalf("changes created = %s, want [%s]", args["created"], newID)
	}
	if string(args["oldState"]) != fmt.Sprintf("%q", state) {
		t.Fatalf("oldState = %s", args["oldState"])
	}
}
