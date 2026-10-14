package mail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/mail"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// backend2 extends the fake backend with the optional Phase 2a-2 contracts.
type backend2 struct {
	backend
	identities  *datatype.Memory[mail.Identity]
	submissions *datatype.Memory[mail.EmailSubmission]
	vac         *fakeVacation
}

func (b *backend2) Identities() datatype.Store[mail.Identity]         { return b.identities }
func (b *backend2) Submissions() datatype.Store[mail.EmailSubmission] { return b.submissions }
func (b *backend2) VacationStore() mail.VacationStore                 { return b.vac }
func (b *backend2) SnippetSource() mail.SnippetSource                 { return b }
func (b *backend2) EmailImporter() mail.Importer                      { return b }
func (b *backend2) EmailCopier() mail.Copier                          { return b }
func (b *backend2) EmailParser() mail.Parser                          { return b }

func (b *backend2) Snippets(_ context.Context, account jmap.ID, ids []jmap.ID) ([]mail.SearchSnippet, []jmap.ID, error) {
	var out []mail.SearchSnippet
	var notFound []jmap.ID
	for _, id := range ids {
		found, _, err := b.emails.Get(context.Background(), account, []jmap.ID{id})
		if err != nil {
			return nil, nil, err
		}
		if len(found) == 0 {
			notFound = append(notFound, id)
			continue
		}
		subj, prev := found[0].Subject, found[0].Preview
		out = append(out, mail.SearchSnippet{EmailID: id, Subject: &subj, Preview: &prev})
	}
	return out, notFound, nil
}

func (b *backend2) ImportEmail(ctx context.Context, account jmap.ID, blobID jmap.ID, mailboxIDs map[jmap.ID]bool, keywords map[string]bool, receivedAt *time.Time) (mail.Email, *jmap.SetError) {
	if blobID == "bad" {
		return mail.Email{}, &jmap.SetError{Type: "invalidProperties", Description: "bad blob"}
	}
	rt := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	if receivedAt != nil {
		rt = *receivedAt
	}
	return b.emails.Create(ctx, account, mail.Email{BlobID: blobID, ThreadID: "t1", MailboxIDs: mailboxIDs, Keywords: keywords, ReceivedAt: rt, Subject: "Imported"})
}

func (b *backend2) CopyEmail(ctx context.Context, fromAccount, toAccount jmap.ID, id jmap.ID) (mail.Email, *jmap.SetError) {
	found, _, err := b.emails.Get(ctx, fromAccount, []jmap.ID{id})
	if err != nil {
		return mail.Email{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	if len(found) == 0 {
		return mail.Email{}, &jmap.SetError{Type: "notFound"}
	}
	cp := found[0]
	cp.ID = ""
	return b.emails.Create(ctx, toAccount, cp)
}

func (b *backend2) ParseEmail(ctx context.Context, account jmap.ID, blobID jmap.ID) (mail.Email, error) {
	if blobID == "bad" {
		return mail.Email{}, fmt.Errorf("cannot parse")
	}
	all, _, err := b.emails.Get(ctx, account, nil)
	if err != nil {
		return mail.Email{}, err
	}
	for _, e := range all {
		if e.BlobID == blobID {
			return e, nil
		}
	}
	return mail.Email{}, fmt.Errorf("unknown blob")
}

type fakeVacation struct {
	cur   mail.VacationResponse
	state int
}

func (f *fakeVacation) Vacation(_ context.Context, _ jmap.ID) (mail.VacationResponse, string, error) {
	return f.cur, strconv.Itoa(f.state), nil
}
func (f *fakeVacation) SetVacation(_ context.Context, _ jmap.ID, v mail.VacationResponse, ifInState *string) (string, string, error) {
	old := strconv.Itoa(f.state)
	if ifInState != nil && *ifInState != old {
		return "", "", jmap.NewStateMismatch("mismatch")
	}
	f.cur = v
	f.state++
	return old, strconv.Itoa(f.state), nil
}

func seed2(t *testing.T) (*httptest.Server, *backend2) {
	t.Helper()
	b := &backend2{
		backend: backend{
			mailboxes: datatype.NewMemory[mail.Mailbox](func(m mail.Mailbox) jmap.ID { return m.ID }, func(m *mail.Mailbox, id jmap.ID) { m.ID = id }),
			emails:    datatype.NewMemory[mail.Email](func(e mail.Email) jmap.ID { return e.ID }, func(e *mail.Email, id jmap.ID) { e.ID = id }),
			threads:   []mail.Thread{{ID: "t1", EmailIDs: []jmap.ID{"e1"}}},
		},
		identities:  datatype.NewMemory[mail.Identity](func(v mail.Identity) jmap.ID { return v.ID }, func(v *mail.Identity, id jmap.ID) { v.ID = id }),
		submissions: datatype.NewMemory[mail.EmailSubmission](func(v mail.EmailSubmission) jmap.ID { return v.ID }, func(v *mail.EmailSubmission, id jmap.ID) { v.ID = id }),
		vac:         &fakeVacation{cur: mail.VacationResponse{ID: "singleton"}},
	}
	ctx := context.Background()
	if _, serr := b.emails.Create(ctx, "acct1", mail.Email{ID: "e1", BlobID: "b1", ThreadID: "t1", MailboxIDs: map[jmap.ID]bool{"mb-inbox": true}, Keywords: map[string]bool{}, Subject: "Hello", Preview: "prev text", ReceivedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}); serr != nil {
		t.Fatal(serr)
	}
	e := server.New(fakeAuth{}, server.Options{})
	mail.Register(e, b)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv, b
}

func TestIdentitySetGetQuery(t *testing.T) {
	srv, _ := seed2(t)
	resps := call(t, srv, `[["Identity/set",{"accountId":"acct1","create":{"c1":{"name":"Alice Work","email":"alice@example.com","mayDelete":true}}},"s"]]`)
	var created map[string]mail.Identity
	if err := json.Unmarshal(argsOf(t, resps[0])["created"], &created); err != nil || created["c1"].ID == "" {
		t.Fatalf("identity created = %s err %v", argsOf(t, resps[0])["created"], err)
	}
	id := string(created["c1"].ID)
	resps = call(t, srv, fmt.Sprintf(`[["Identity/get",{"accountId":"acct1","ids":[%q]},"g"]]`, id))
	var list []mail.Identity
	_ = json.Unmarshal(argsOf(t, resps[0])["list"], &list)
	if len(list) != 1 || list[0].Email != "alice@example.com" || !list[0].MayDelete {
		t.Fatalf("identity get = %s", argsOf(t, resps[0])["list"])
	}
	if !strings.Contains(string(argsOf(t, resps[0])["list"]), `"textSignature":""`) {
		t.Fatalf("identity JSON = %s", argsOf(t, resps[0])["list"])
	}
	ids := queryIDs(t, srv, "Identity/query", `"accountId":"acct1","filter":{"email":"ALICE@"}`)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("identity query ids = %v", ids)
	}
	ids = queryIDs(t, srv, "Identity/query", `"accountId":"acct1","filter":{"name":"work"},"sort":[{"property":"name"}]`)
	if len(ids) != 1 {
		t.Fatalf("identity name query ids = %v", ids)
	}
}

func TestEmailSubmissionOnSuccessDestroy(t *testing.T) {
	srv, _ := seed2(t)
	resps := call(t, srv, `[["EmailSubmission/set",{"accountId":"acct1","create":{"s1":{"identityId":"id1","emailId":"e1","threadId":"t1","envelope":{"mailFrom":{"email":"alice@example.com","parameters":{}},"rcptTo":[{"email":"bob@example.com","parameters":{"NOTIFY":"SUCCESS"}}]},"undoStatus":"pending"}},"onSuccessDestroyEmail":{"s1":"e1"}},"s"]]`)
	args := argsOf(t, resps[0])
	var created map[string]mail.EmailSubmission
	if err := json.Unmarshal(args["created"], &created); err != nil || created["s1"].ID == "" {
		t.Fatalf("submission created = %s err %v", args["created"], err)
	}
	if created["s1"].Envelope == nil || created["s1"].Envelope.RcptTo[0].Parameters["NOTIFY"] == nil || *created["s1"].Envelope.RcptTo[0].Parameters["NOTIFY"] != "SUCCESS" {
		t.Fatalf("envelope = %s", args["created"])
	}
	subID := string(created["s1"].ID)
	// Submission visible via get; email destroyed by onSuccess.
	resps = call(t, srv, fmt.Sprintf(`[["EmailSubmission/get",{"accountId":"acct1","ids":[%q]},"g"],["Email/get",{"accountId":"acct1","ids":["e1"]},"g2"]]`, subID))
	if nameOf(resps[0]) != "EmailSubmission/get" {
		t.Fatalf("resp0 = %s", resps[0][0])
	}
	var subs []mail.EmailSubmission
	_ = json.Unmarshal(argsOf(t, resps[0])["list"], &subs)
	if len(subs) != 1 || subs[0].EmailID != "e1" {
		t.Fatalf("submission get = %s", argsOf(t, resps[0])["list"])
	}
	var notFound []string
	_ = json.Unmarshal(argsOf(t, resps[1])["notFound"], &notFound)
	if len(notFound) != 1 || notFound[0] != "e1" {
		t.Fatalf("email should be destroyed, notFound = %s list=%s", argsOf(t, resps[1])["notFound"], argsOf(t, resps[1])["list"])
	}
	// onSuccess for a failed/unknown creation does nothing (no panic, e1 already gone check via query filter)
	ids := queryIDs(t, srv, "EmailSubmission/query", `"accountId":"acct1","filter":{"undoStatus":"pending"},"sort":[{"property":"sendAt"}]`)
	if len(ids) != 1 || ids[0] != subID {
		t.Fatalf("submission query ids = %v", ids)
	}
}

func TestVacationGetSetAndMismatch(t *testing.T) {
	srv, _ := seed2(t)
	resps := call(t, srv, `[["VacationResponse/get",{"accountId":"acct1","ids":null},"g"]]`)
	args := argsOf(t, resps[0])
	if !strings.Contains(string(args["list"]), `"id":"singleton"`) || !strings.Contains(string(args["list"]), `"isEnabled":false`) {
		t.Fatalf("vacation get = %s", args["list"])
	}
	var state string
	_ = json.Unmarshal(args["state"], &state)
	resps = call(t, srv, fmt.Sprintf(`[["VacationResponse/set",{"accountId":"acct1","ifInState":%q,"update":{"singleton":{"isEnabled":true,"subject":"Away","textBody":"out"}}},"s"]]`, state))
	args = argsOf(t, resps[0])
	if string(args["updated"]) != `{"singleton":null}` {
		t.Fatalf("vacation updated = %s notUpdated=%s", args["updated"], args["notUpdated"])
	}
	resps = call(t, srv, `[["VacationResponse/get",{"accountId":"acct1"},"g"]]`)
	if !strings.Contains(string(argsOf(t, resps[0])["list"]), `"subject":"Away"`) || !strings.Contains(string(argsOf(t, resps[0])["list"]), `"isEnabled":true`) {
		t.Fatalf("vacation after set = %s", argsOf(t, resps[0])["list"])
	}
	resps = call(t, srv, `[["VacationResponse/set",{"accountId":"acct1","ifInState":"bogus","update":{"singleton":{"isEnabled":false}}},"s"]]`)
	if nameOf(resps[0]) != "error" || string(argsOf(t, resps[0])["type"]) != `"stateMismatch"` {
		t.Fatalf("vacation mismatch resp = %s %s", resps[0][0], resps[0][1])
	}
}

func TestSearchSnippetImportCopyParse(t *testing.T) {
	srv, _ := seed2(t)
	resps := call(t, srv, `[["SearchSnippet/get",{"accountId":"acct1","emailIds":["e1","nope"]},"g"]]`)
	args := argsOf(t, resps[0])
	var list []mail.SearchSnippet
	_ = json.Unmarshal(args["list"], &list)
	if len(list) != 1 || list[0].Subject == nil || *list[0].Subject != "Hello" || list[0].Preview == nil || *list[0].Preview != "prev text" {
		t.Fatalf("snippets = %s", args["list"])
	}
	var nf []string
	_ = json.Unmarshal(args["notFound"], &nf)
	if len(nf) != 1 || nf[0] != "nope" {
		t.Fatalf("snippet notFound = %s", args["notFound"])
	}

	// Import: success + notCreated.
	resps = call(t, srv, `[["Email/import",{"accountId":"acct1","emails":{"i1":{"blobId":"b-new","mailboxIds":{"mb-inbox":true}},"i2":{"blobId":"bad","mailboxIds":{"mb-inbox":true}}}},"s"]]`)
	args = argsOf(t, resps[0])
	var created map[string]mail.Email
	_ = json.Unmarshal(args["created"], &created)
	if _, ok := created["i1"]; !ok || created["i1"].ID == "" || created["i1"].BlobID != "b-new" {
		t.Fatalf("import created = %s", args["created"])
	}
	if !strings.Contains(string(args["notCreated"]), `"i2"`) || !strings.Contains(string(args["notCreated"]), `"invalidProperties"`) {
		t.Fatalf("import notCreated = %s", args["notCreated"])
	}
	if string(args["oldState"]) != "null" {
		t.Fatalf("import oldState = %s", args["oldState"])
	}
	importedID := string(created["i1"].ID)

	// Copy within account.
	resps = call(t, srv, `[["Email/copy",{"fromAccountId":"acct1","accountId":"acct1","emailIds":["e1","nope"]},"s"]]`)
	args = argsOf(t, resps[0])
	var copied map[string]mail.Email
	_ = json.Unmarshal(args["copied"], &copied)
	if copied["e1"].ID == "" || copied["e1"].ID == "e1" {
		t.Fatalf("copied = %s", args["copied"])
	}
	if !strings.Contains(string(args["notCopied"]), `"nope"`) {
		t.Fatalf("notCopied = %s", args["notCopied"])
	}
	_ = importedID

	// Parse: parsed + notParsable, properties projection keeps blobId.
	resps = call(t, srv, `[["Email/parse",{"accountId":"acct1","blobIds":["b1","bad"],"properties":["subject"]},"s"]]`)
	args = argsOf(t, resps[0])
	if !strings.Contains(string(args["parsed"]), `"subject":"Hello"`) || !strings.Contains(string(args["parsed"]), `"blobId":"b1"`) {
		t.Fatalf("parsed = %s", args["parsed"])
	}
	if strings.Contains(string(args["parsed"]), `"preview"`) {
		t.Fatalf("parsed should be projected: %s", args["parsed"])
	}
	if !strings.Contains(string(args["notParsable"]), `"bad"`) || !strings.Contains(string(args["notParsable"]), `"notParsable"`) {
		t.Fatalf("notParsable = %s", args["notParsable"])
	}
}
