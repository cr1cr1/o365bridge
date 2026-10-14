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

// fakeMDNSender records the exact MDN it was asked to send.
type fakeMDNSender struct {
	gotAccount jmap.ID
	got        mail.MDN
	serr       *jmap.SetError
}

func (f *fakeMDNSender) SendMDN(_ context.Context, account jmap.ID, mdn mail.MDN) (mail.MDN, *jmap.SetError) {
	f.gotAccount, f.got = account, mdn
	if f.serr != nil {
		return mail.MDN{}, f.serr
	}
	mdn.ID = "mdn1"
	return mdn, nil
}

type phase3Backend struct {
	mailboxes *datatype.Memory[mail.Mailbox]
	emails    *datatype.Memory[mail.Email]
	sender    *fakeMDNSender
}

func (b *phase3Backend) Mailboxes() datatype.Store[mail.Mailbox] { return b.mailboxes }
func (b *phase3Backend) Emails() datatype.Store[mail.Email]      { return b.emails }
func (b *phase3Backend) Threads(_ context.Context, _ jmap.ID, _ []jmap.ID) ([]mail.Thread, []jmap.ID, string, error) {
	return nil, nil, "1", nil
}
func (b *phase3Backend) MDNSender() mail.MDNSender { return b.sender }
func (b *phase3Backend) SMimeCapable() bool        { return true }

func newPhase3Server(t *testing.T, sender *fakeMDNSender) *httptest.Server {
	t.Helper()
	b := &phase3Backend{
		mailboxes: datatype.NewMemory[mail.Mailbox](
			func(m mail.Mailbox) jmap.ID { return m.ID },
			func(m *mail.Mailbox, id jmap.ID) { m.ID = id }),
		emails: datatype.NewMemory[mail.Email](
			func(e mail.Email) jmap.ID { return e.ID },
			func(e *mail.Email, id jmap.ID) { e.ID = id }),
		sender: sender,
	}
	e := server.New(fakeAuth{}, server.Options{})
	mail.Register(e, b)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func phase3Call(t *testing.T, srv *httptest.Server, methodCalls string) [][3]json.RawMessage {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:mdn","urn:ietf:params:jmap:smime"],"methodCalls":%s}`, methodCalls)
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

func sessionCaps(t *testing.T, srv *httptest.Server) map[jmap.URI]json.RawMessage {
	t.Helper()
	resp, err := http.Get(srv.URL + "/jmap/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session HTTP status = %d", resp.StatusCode)
	}
	var s jmap.Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s.Capabilities
}

func TestMDNSendSuccess(t *testing.T) {
	sender := &fakeMDNSender{}
	srv := newPhase3Server(t, sender)
	resps := phase3Call(t, srv, `[["MDN/send",{"accountId":"acct1","identityId":"id1","emailId":"e1","subject":"Read: Hello","textBody":"Displayed","reportingUA":"MUA 1.0","disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"},"includeOriginalMessage":true},"s"]]`)
	if nameOf(resps[0]) == "error" {
		t.Fatalf("MDN/send error: %s", resps[0][1])
	}
	args := argsOf(t, resps[0])
	if string(args["accountId"]) != `"acct1"` {
		t.Fatalf("accountId = %s", args["accountId"])
	}
	var got struct {
		MDN mail.MDN `json:"mdn"`
	}
	if err := json.Unmarshal(resps[0][1], &got); err != nil {
		t.Fatal(err)
	}
	if got.MDN.ID != "mdn1" || got.MDN.EmailID != "e1" || got.MDN.IdentityID != "id1" {
		t.Fatalf("returned mdn = %+v", got.MDN)
	}
	// Fake received the exact disposition and payload.
	if sender.gotAccount != "acct1" {
		t.Fatalf("sender account = %q", sender.gotAccount)
	}
	wantDisp := mail.MDNDisposition{ActionMode: "manual-action", SendingMode: "mdn-sent-manually", Type: "displayed"}
	if sender.got.Disposition != wantDisp {
		t.Fatalf("disposition = %+v, want %+v", sender.got.Disposition, wantDisp)
	}
	if sender.got.EmailID != "e1" || sender.got.IdentityID != "id1" || !sender.got.IncludeOriginalMessage ||
		sender.got.Subject == nil || *sender.got.Subject != "Read: Hello" ||
		sender.got.TextBody == nil || *sender.got.TextBody != "Displayed" ||
		sender.got.ReportingUA == nil || *sender.got.ReportingUA != "MUA 1.0" {
		t.Fatalf("sender got = %+v", sender.got)
	}
	if got.MDN.Disposition != wantDisp {
		t.Fatalf("returned disposition = %+v", got.MDN.Disposition)
	}
}

func TestMDNSendError(t *testing.T) {
	sender := &fakeMDNSender{serr: &jmap.SetError{Type: "notFound", Description: "no such email"}}
	srv := newPhase3Server(t, sender)
	resps := phase3Call(t, srv, `[["MDN/send",{"accountId":"acct1","emailId":"nope","disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}},"s"]]`)
	if nameOf(resps[0]) != "error" {
		t.Fatalf("name = %s, want error (%s)", nameOf(resps[0]), resps[0][1])
	}
	args := argsOf(t, resps[0])
	if string(args["type"]) != `"notFound"` {
		t.Fatalf("error type = %s", args["type"])
	}
}

func TestEmailSMimeJSONRoundTrip(t *testing.T) {
	status := mail.SignedValid
	verified := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	em := mail.Email{
		ID: "e1", BlobID: "b1", ThreadID: "t1",
		SMimeStatus: &status, SMimeErrors: []string{"warn"}, SMimeVerifiedAt: &verified,
	}
	raw, err := json.Marshal(em)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"smimeStatus":"signed/valid"`, `"smimeErrors":["warn"]`, `"smimeVerifiedAt":"2024-05-01T12:00:00Z"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("email JSON %s missing %s", raw, want)
		}
	}
	var back mail.Email
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.SMimeStatus == nil || *back.SMimeStatus != mail.SignedValid ||
		len(back.SMimeErrors) != 1 || back.SMimeErrors[0] != "warn" ||
		back.SMimeVerifiedAt == nil || !back.SMimeVerifiedAt.Equal(verified) {
		t.Fatalf("round trip = %+v", back)
	}

	// Omitted entirely when nil (non-S/MIME backends).
	raw, err = json.Marshal(mail.Email{ID: "e2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"smimeStatus", "smimeErrors", "smimeVerifiedAt"} {
		if strings.Contains(string(raw), absent) {
			t.Fatalf("email JSON %s should omit %s", raw, absent)
		}
	}
}

func TestSessionAdvertisesMDNAndSMimeOnlyWhenCapable(t *testing.T) {
	// Capable backend: both capabilities advertised.
	srv := newPhase3Server(t, &fakeMDNSender{})
	caps := sessionCaps(t, srv)
	if _, ok := caps[jmap.MDNURI]; !ok {
		t.Fatalf("capable session missing mdn: %v", caps)
	}
	if _, ok := caps[jmap.URI("urn:ietf:params:jmap:smime")]; !ok {
		t.Fatalf("capable session missing smime: %v", caps)
	}

	// Plain backend (existing harness): neither advertised.
	plainSrv, _ := seed(t)
	caps = sessionCaps(t, plainSrv)
	if _, ok := caps[jmap.MDNURI]; ok {
		t.Fatalf("plain session should not advertise mdn: %v", caps)
	}
	if _, ok := caps[jmap.URI("urn:ietf:params:jmap:smime")]; ok {
		t.Fatalf("plain session should not advertise smime: %v", caps)
	}
}
