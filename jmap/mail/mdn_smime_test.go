package mail_test

// Tests for MDN (RFC 9007) and S/MIME (RFC 9219), written from the
// normative text: MDN/send is a batch method over "Id[MDN]" with
// "sent"/"notSent" responses and mandatory "$mdnsent" enforcement
// (§2.1, §3.1); MDN/parse parses blobs (§2.2); the S/MIME capability
// is "urn:ietf:params:jmap:smimeverify" (§3) with the §4 status
// vocabulary.

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	mdnCap   = jmap.MDNURI // "urn:ietf:params:jmap:mdn"
	smimeCap = jmap.URI("urn:ietf:params:jmap:smimeverify")
)

func strPtr(s string) *string { return &s }

// fakeMDNSender records the exact MDN it was asked to send and fills
// in the server-set fields, as a real backend would (RFC 9007 §2.1:
// originalRecipient/originalMessageId are server-generated).
type fakeMDNSender struct {
	gotAccount  jmap.ID
	gotIdentity jmap.ID
	got         mail.MDN
	serr        *jmap.SetError
}

func (f *fakeMDNSender) SendMDN(_ context.Context, account, identity jmap.ID, mdn mail.MDN) (mail.MDN, *jmap.SetError) {
	f.gotAccount, f.gotIdentity, f.got = account, identity, mdn
	if f.serr != nil {
		return mail.MDN{}, f.serr
	}
	mdn.MdnGateway = strPtr("gateway.example.com")
	mdn.OriginalMessageID = strPtr("<orig@example.org>")
	if mdn.FinalRecipient == nil {
		mdn.FinalRecipient = strPtr("rfc822; john@example.com")
	}
	return mdn, nil
}

// fakeMDNParser decides per blob how to answer MDN/parse.
type fakeMDNParser struct {
	got []jmap.ID
	fn  func(blobID jmap.ID) (mail.MDN, error)
}

func (f *fakeMDNParser) ParseMDN(_ context.Context, _ jmap.ID, blobID jmap.ID) (mail.MDN, error) {
	f.got = append(f.got, blobID)
	return f.fn(blobID)
}

type phase3Backend struct {
	mailboxes *datatype.Memory[mail.Mailbox]
	emails    *datatype.Memory[mail.Email]
	sender    *fakeMDNSender
	parser    *fakeMDNParser
}

func (b *phase3Backend) Mailboxes() datatype.Store[mail.Mailbox] { return b.mailboxes }
func (b *phase3Backend) Emails() datatype.Store[mail.Email]      { return b.emails }
func (b *phase3Backend) Threads(_ context.Context, _ jmap.ID, _ []jmap.ID) ([]mail.Thread, []jmap.ID, string, error) {
	return nil, nil, "1", nil
}
func (b *phase3Backend) MDNSender() mail.MDNSender { return b.sender }
func (b *phase3Backend) MDNParser() mail.MDNParser { return b.parser }
func (b *phase3Backend) SMimeCapable() bool        { return true }

func newPhase3Server(t *testing.T, sender *fakeMDNSender, parser *fakeMDNParser) (*httptest.Server, *phase3Backend) {
	t.Helper()
	b := &phase3Backend{
		mailboxes: datatype.NewMemory[mail.Mailbox](
			func(m mail.Mailbox) jmap.ID { return m.ID },
			func(m *mail.Mailbox, id jmap.ID) { m.ID = id }),
		emails: datatype.NewMemory[mail.Email](
			func(e mail.Email) jmap.ID { return e.ID },
			func(e *mail.Email, id jmap.ID) { e.ID = id }),
		sender: sender,
		parser: parser,
	}
	e := server.New(fakeAuth{}, server.Options{})
	mail.Register(e, b)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv, b
}

func phase3Call(t *testing.T, srv *httptest.Server, methodCalls string) [][3]json.RawMessage {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail",%q,%q],"methodCalls":%s}`, mdnCap, smimeCap, methodCalls)
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

func seedEmail(t *testing.T, b *phase3Backend, id string, keywords map[string]bool) {
	t.Helper()
	if _, serr := b.emails.Create(context.Background(), "acct1", mail.Email{
		ID: jmap.ID(id), BlobID: jmap.ID("b-" + id), ThreadID: "t1",
		Subject: "Hello", Keywords: keywords,
	}); serr != nil {
		t.Fatalf("seed email: %v", serr)
	}
}

// RFC 9007 §2.1/§3.1: MDN/send takes "send": Id[MDN] plus
// onSuccessUpdateEmail, and answers with sent/notSent maps; the
// server fills in the server-set fields.
func TestMDNSendBatchPerRFC9007(t *testing.T) {
	sender := &fakeMDNSender{}
	srv, b := newPhase3Server(t, sender, nil)
	seedEmail(t, b, "e1", nil)

	resps := phase3Call(t, srv, `[["MDN/send",{
		"accountId":"acct1",
		"identityId":"id1",
		"send":{"k1":{"forEmailId":"e1","subject":"Read receipt for: Hello","textBody":"Displayed.","reportingUA":"MUA 1.0","disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}},
		"onSuccessUpdateEmail":{"#k1":{"keywords/$mdnsent":true}}
	},"s"]]`)
	if len(resps) != 1 {
		t.Fatalf("responses = %d", len(resps))
	}
	if nameOf(resps[0]) != "MDN/send" {
		t.Fatalf("response name = %s args = %s", nameOf(resps[0]), resps[0][1])
	}
	args := argsOf(t, resps[0])
	if string(args["accountId"]) != `"acct1"` {
		t.Fatalf("accountId = %s", args["accountId"])
	}
	var sent map[string]mail.MDN
	if err := json.Unmarshal(args["sent"], &sent); err != nil {
		t.Fatalf("sent = %s", args["sent"])
	}
	m, ok := sent["k1"]
	if !ok {
		t.Fatalf("sent = %s, want k1", args["sent"])
	}
	if m.ForEmailID != "e1" || m.FinalRecipient == nil || *m.FinalRecipient != "rfc822; john@example.com" ||
		m.OriginalMessageID == nil || *m.OriginalMessageID != "<orig@example.org>" ||
		m.Disposition.Type != "displayed" {
		t.Fatalf("sent[k1] = %+v", m)
	}
	if args["notSent"] != nil && string(args["notSent"]) != "null" {
		t.Fatalf("notSent = %s, want null", args["notSent"])
	}
	// The fake received the payload and identity.
	if sender.gotIdentity != "id1" || sender.got.ForEmailID != "e1" ||
		sender.got.Subject == nil || *sender.got.Subject != "Read receipt for: Hello" {
		t.Fatalf("sender got = %+v identity = %q", sender.got, sender.gotIdentity)
	}
	// onSuccessUpdateEmail applied: the email now carries $mdnsent.
	found, nf, err := b.emails.Get(context.Background(), "acct1", []jmap.ID{"e1"})
	if err != nil || len(nf) != 0 || len(found) != 1 {
		t.Fatalf("get after send: %v %v", nf, err)
	}
	if !found[0].Keywords["$mdnsent"] {
		t.Fatalf("email keywords after send = %v, want $mdnsent", found[0].Keywords)
	}
}

// RFC 9007 §2.1: "the server MUST reject an MDN/send that does not
// result in setting the keyword $mdnsent".
func TestMDNSendRequiresMdnsentUpdate(t *testing.T) {
	sender := &fakeMDNSender{}
	srv, b := newPhase3Server(t, sender, nil)
	seedEmail(t, b, "e1", nil)

	resps := phase3Call(t, srv, `[["MDN/send",{
		"accountId":"acct1","identityId":"id1",
		"send":{"k1":{"forEmailId":"e1","disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}}
	},"s"]]`)
	if nameOf(resps[0]) != "error" {
		t.Fatalf("name = %s args = %s, want error", nameOf(resps[0]), resps[0][1])
	}
	args := argsOf(t, resps[0])
	if string(args["type"]) != `"invalidArguments"` {
		t.Fatalf("error type = %s, want invalidArguments", args["type"])
	}
	if sender.gotAccount != "" {
		t.Fatal("sender was called despite missing $mdnsent update")
	}
}

// RFC 9007 §2.1: mdnAlreadySent when the message already carries
// $mdnsent; sender errors are per-record in notSent.
func TestMDNSendAlreadySentAndSenderError(t *testing.T) {
	sender := &fakeMDNSender{}
	srv, b := newPhase3Server(t, sender, nil)
	seedEmail(t, b, "e1", map[string]bool{"$mdnsent": true})
	seedEmail(t, b, "e2", nil)

	errSender := &fakeMDNSender{serr: &jmap.SetError{Type: "notFound", Description: "no Disposition-Notification-To"}}
	srv2, b2 := newPhase3Server(t, errSender, nil)
	seedEmail(t, b2, "e9", nil)

	resps := phase3Call(t, srv, `[["MDN/send",{
		"accountId":"acct1","identityId":"id1",
		"send":{"k1":{"forEmailId":"e1","disposition":{"actionMode":"automatic-action","sendingMode":"mdn-sent-automatically","type":"dispatched"}}},
		"onSuccessUpdateEmail":{"#k1":{"keywords/$mdnsent":true}}
	},"a"]]`)
	args := argsOf(t, resps[0])
	var notSent map[string]*jmap.SetError
	if err := json.Unmarshal(args["notSent"], &notSent); err != nil {
		t.Fatalf("notSent = %s", args["notSent"])
	}
	if notSent["k1"] == nil || notSent["k1"].Type != "mdnAlreadySent" {
		t.Fatalf("notSent = %s, want mdnAlreadySent", args["notSent"])
	}

	resps = phase3Call(t, srv2, `[["MDN/send",{
		"accountId":"acct1","identityId":"id1",
		"send":{"k9":{"forEmailId":"e9","disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}},
		"onSuccessUpdateEmail":{"#k9":{"keywords/$mdnsent":true}}
	},"b"]]`)
	args = argsOf(t, resps[0])
	if err := json.Unmarshal(args["notSent"], &notSent); err != nil {
		t.Fatalf("notSent = %s", args["notSent"])
	}
	if notSent["k9"] == nil || notSent["k9"].Type != "notFound" {
		t.Fatalf("notSent = %s, want notFound", args["notSent"])
	}
	if args["sent"] != nil && string(args["sent"]) != "null" {
		t.Fatalf("sent = %s, want null", args["sent"])
	}
}

// RFC 9007 §2.2: MDN/parse answers parsed/notParsable/notFound.
func TestMDNParse(t *testing.T) {
	parser := &fakeMDNParser{fn: func(blobID jmap.ID) (mail.MDN, error) {
		switch blobID {
		case "good":
			return mail.MDN{
				ForEmailID:  "e1",
				Disposition: mail.MDNDisposition{ActionMode: "automatic-action", SendingMode: "mdn-sent-automatically", Type: "dispatched"},
			}, nil
		case "junk":
			return mail.MDN{}, mail.ErrMDNNotParsable
		default:
			return mail.MDN{}, mail.ErrMDNNotFound
		}
	}}
	srv, _ := newPhase3Server(t, &fakeMDNSender{}, parser)
	resps := phase3Call(t, srv, `[["MDN/parse",{"accountId":"acct1","blobIds":["good","junk","gone"]},"p"]]`)
	if nameOf(resps[0]) != "MDN/parse" {
		t.Fatalf("name = %s args = %s", nameOf(resps[0]), resps[0][1])
	}
	args := argsOf(t, resps[0])
	var parsed map[string]mail.MDN
	if err := json.Unmarshal(args["parsed"], &parsed); err != nil {
		t.Fatalf("parsed = %s", args["parsed"])
	}
	if parsed["good"].ForEmailID != "e1" || parsed["good"].Disposition.Type != "dispatched" {
		t.Fatalf("parsed = %s", args["parsed"])
	}
	var notParsable, notFound []string
	if err := json.Unmarshal(args["notParsable"], &notParsable); err != nil || len(notParsable) != 1 || notParsable[0] != "junk" {
		t.Fatalf("notParsable = %s err = %v", args["notParsable"], err)
	}
	if err := json.Unmarshal(args["notFound"], &notFound); err != nil || len(notFound) != 1 || notFound[0] != "gone" {
		t.Fatalf("notFound = %s err = %v", args["notFound"], err)
	}
}

// RFC 9219 §3: the capability is urn:ietf:params:jmap:smimeverify,
// empty object, and only for capable backends.
func TestSessionAdvertisesMDNAndSMimeOnlyWhenCapable(t *testing.T) {
	srv, _ := newPhase3Server(t, &fakeMDNSender{}, nil)
	caps := sessionCaps(t, srv)
	if _, ok := caps[mdnCap]; !ok {
		t.Fatalf("capable session missing mdn: %v", caps)
	}
	if _, ok := caps[smimeCap]; !ok {
		t.Fatalf("capable session missing smimeverify: %v", caps)
	}

	plainSrv, _ := seed(t)
	caps = sessionCaps(t, plainSrv)
	if _, ok := caps[mdnCap]; ok {
		t.Fatalf("plain session should not advertise mdn: %v", caps)
	}
	if _, ok := caps[smimeCap]; ok {
		t.Fatalf("plain session should not advertise smimeverify: %v", caps)
	}
}

// RFC 9219 §4: the smime* response properties and their vocabulary.
func TestEmailSMimeJSONRoundTrip(t *testing.T) {
	status := mail.SMimeSignedVerified
	atDelivery := mail.SMimeSignedFailed
	verified := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	em := mail.Email{
		ID: "e1", BlobID: "b1", ThreadID: "t1",
		SMimeStatus:           &status,
		SMimeStatusAtDelivery: &atDelivery,
		SMimeErrors:           []string{"expired certificate"},
		SMimeVerifiedAt:       &verified,
	}
	raw, err := json.Marshal(em)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"smimeStatus":"signed/verified"`,
		`"smimeStatusAtDelivery":"signed/failed"`,
		`"smimeErrors":["expired certificate"]`,
		`"smimeVerifiedAt":"2024-05-01T12:00:00Z"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("email JSON %s missing %s", raw, want)
		}
	}
	var back mail.Email
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.SMimeStatus == nil || *back.SMimeStatus != mail.SMimeSignedVerified ||
		back.SMimeStatusAtDelivery == nil || *back.SMimeStatusAtDelivery != mail.SMimeSignedFailed ||
		len(back.SMimeErrors) != 1 || back.SMimeVerifiedAt == nil || !back.SMimeVerifiedAt.Equal(verified) {
		t.Fatalf("round trip = %+v", back)
	}

	// Omitted entirely when nil (non-S/MIME backends).
	raw, err = json.Marshal(mail.Email{ID: "e2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"smimeStatus", "smimeStatusAtDelivery", "smimeErrors", "smimeVerifiedAt"} {
		if strings.Contains(string(raw), absent) {
			t.Fatalf("email JSON %s should omit %s", raw, absent)
		}
	}
}

// RFC 9219 §4 status vocabulary (values are backend-supplied; the
// constants must match the RFC strings).
func TestSMimeStatusVocabulary(t *testing.T) {
	for _, tc := range []struct {
		c    mail.SMimeStatus
		want string
	}{
		{mail.SMimeUnknown, "unknown"},
		{mail.SMimeSigned, "signed"},
		{mail.SMimeSignedVerified, "signed/verified"},
		{mail.SMimeSignedFailed, "signed/failed"},
		{mail.SMimeEncryptedSignedVerified, "encrypted+signed/verified"},
		{mail.SMimeEncryptedSignedFailed, "encrypted+signed/failed"},
	} {
		if string(tc.c) != tc.want {
			t.Fatalf("constant %q != RFC value %q", tc.c, tc.want)
		}
	}
	_ = errors.New // keep errors imported for future negative tests
}
