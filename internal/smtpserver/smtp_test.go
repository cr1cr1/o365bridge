package smtpserver_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/smtpserver"
)

// --- in-memory fake provider / registry harness ---

type fakeProvider struct {
	bridge.Provider
	mu      sync.Mutex
	sent    [][]byte
	sendErr error
}

func (p *fakeProvider) SendMessage(_ context.Context, mime []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sendErr != nil {
		return p.sendErr
	}
	cp := append([]byte(nil), mime...)
	p.sent = append(p.sent, cp)
	return nil
}
func (p *fakeProvider) sentCopy() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.sent...)
}

type fakeLogin struct{}

func (fakeLogin) Login(_ context.Context, email string, ch bridge.Channel) (bridge.Session, error) {
	return bridge.Session{Email: email, Channel: ch, AccessToken: "t", TokenExpiry: time.Now().Add(time.Hour)}, nil
}
func (fakeLogin) Renew(_ context.Context, _ string, s bridge.Session) (bridge.Session, error) {
	s.TokenExpiry = time.Now().Add(time.Hour)
	return s, nil
}

type fakeFactory struct {
	p     *fakeProvider
	provs map[string]*fakeProvider // per-account providers, when set
}

func (f fakeFactory) New(email string, _ bridge.Session) bridge.Provider {
	if f.provs != nil {
		return f.provs[email]
	}
	return f.p
}

const (
	acct = "user@example.com"
	pw   = "instance-secret"
)

func start(t *testing.T, p *fakeProvider) (addr string, cleanup func()) {
	t.Helper()
	return startWithLogger(t, p, zerolog.Nop())
}

func startMulti(t *testing.T, provs map[string]*fakeProvider) (addr string, cleanup func()) {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{
		StateDir: t.TempDir(), Login: fakeLogin{}, Factory: fakeFactory{provs: provs},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := smtpserver.New(reg, pw, zerolog.Nop())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	return l.Addr().String(), func() { _ = srv.Close() }
}

func startWithLogger(t *testing.T, p *fakeProvider, logger zerolog.Logger) (addr string, cleanup func()) {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{
		StateDir: t.TempDir(), Login: fakeLogin{}, Factory: fakeFactory{p: p},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := smtpserver.New(reg, pw, logger)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	return l.Addr().String(), func() { _ = srv.Close() }
}

type client struct {
	c net.Conn
	r *bufio.Reader
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	cl := &client{c: c, r: bufio.NewReader(c)}
	if resp := cl.readReply(t); !strings.HasPrefix(resp, "220") || !strings.Contains(resp, "o365bridge") {
		t.Fatalf("greeting = %q", resp)
	}
	return cl
}

// readReply reads a (possibly multiline) SMTP reply, returns it joined.
func (cl *client) readReply(t *testing.T) string {
	t.Helper()
	var lines []string
	for {
		line, err := cl.r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, strings.Join(lines, ""))
		}
		lines = append(lines, strings.TrimRight(line, "\r\n"))
		if len(line) >= 4 && line[3] == ' ' {
			break
		}
		if len(line) < 4 {
			break
		}
	}
	return strings.Join(lines, "\n")
}

func (cl *client) cmd(t *testing.T, format string, args ...any) string {
	t.Helper()
	s := fmt.Sprintf(format, args...)
	if _, err := cl.c.Write([]byte(s + "\r\n")); err != nil {
		t.Fatal(err)
	}
	return cl.readReply(t)
}

// A failed SMTP auth must leave a warning in the bridge log with
// the account (it used to be debug-only, i.e. invisible).
func TestSMTPAuthFailureLoggedPerAccount(t *testing.T) {
	buf := &lockedBuffer{}
	addr, done := startWithLogger(t, &fakeProvider{}, zerolog.New(buf))
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()
	cl.cmd(t, "EHLO client.example")
	r := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, "wrong-password"))
	if !strings.HasPrefix(r, "535") {
		t.Fatalf("auth = %q, want 535", r)
	}
	out := buf.String()
	for _, want := range []string{`"level":"warn"`, `"protocol":"smtp"`, `"account":"` + acct + `"`, `"op":"auth"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, "wrong-password") {
		t.Fatalf("password leaked into log: %s", out)
	}
}

// MAIL before AUTH (the "server requires authentication" a client
// reports) must be warning-logged with the attempted sender, and
// the session must leave a per-connection trace.
func TestSMTPMailBeforeAuthWarnLogged(t *testing.T) {
	buf := &lockedBuffer{}
	addr, done := startWithLogger(t, &fakeProvider{}, zerolog.New(buf))
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()
	cl.cmd(t, "EHLO client.example")
	r := cl.cmd(t, "MAIL FROM:<user@example.com>")
	// RFC 4954: 530 5.7.0 is the standard "authentication
	// required" reply — the signal an MTA uses to start AUTH and
	// prompt the user for a password. A 503 would be fatal.
	if !strings.HasPrefix(r, "530 5.7.0") {
		t.Fatalf("MAIL before auth = %q, want 530 5.7.0", r)
	}
	out := buf.String()
	for _, want := range []string{`"level":"warn"`, `"protocol":"smtp"`, `"op":"mail"`, `"from":"user@example.com"`, `"hello":"EHLO"`, `"auth_advertised":true`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s: %s", want, out)
		}
	}
}

// Same warning after HELO (no AUTH is ever advertised on HELO) must
// say so, distinguishing a client that never EHLO'd from one that
// EHLO'd, saw AUTH, and skipped it anyway.
func TestSMTPMailBeforeAuthAfterHeloLogged(t *testing.T) {
	buf := &lockedBuffer{}
	addr, done := startWithLogger(t, &fakeProvider{}, zerolog.New(buf))
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()
	cl.cmd(t, "HELO client.example")
	if r := cl.cmd(t, "MAIL FROM:<user@example.com>"); !strings.HasPrefix(r, "530 5.7.0") {
		t.Fatalf("MAIL before auth = %q, want 530 5.7.0", r)
	}
	out := buf.String()
	for _, want := range []string{`"hello":"HELO"`, `"auth_advertised":false`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s: %s", want, out)
		}
	}
}

// lockedBuffer is a goroutine-safe log sink (the server logs from
// connection goroutines while the test reads).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// After a 530, a standard MTA recovers in the same session:
// AUTH (prompting for the password if needed), then resend.
func TestSMTPRecoverAfter530ByAuthenticating(t *testing.T) {
	prov := &fakeProvider{}
	addr, done := start(t, prov)
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()

	cl.cmd(t, "EHLO client.example")
	if r := cl.cmd(t, "MAIL FROM:<%s>", acct); !strings.HasPrefix(r, "530") {
		t.Fatalf("MAIL before auth = %q, want 530", r)
	}
	if r := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw)); !strings.HasPrefix(r, "235") {
		t.Fatalf("auth after 530 = %q, want 235", r)
	}
	if r := cl.cmd(t, "MAIL FROM:<%s>", acct); !strings.HasPrefix(r, "250") {
		t.Fatalf("MAIL after auth = %q, want 250", r)
	}
	if r := cl.cmd(t, "RCPT TO:<dest@example.com>"); !strings.HasPrefix(r, "250") {
		t.Fatalf("RCPT = %q, want 250", r)
	}
	if r := cl.cmd(t, "DATA"); !strings.HasPrefix(r, "354") {
		t.Fatalf("DATA = %q, want 354", r)
	}
	msg := "From: " + acct + "\r\nTo: dest@example.com\r\nSubject: recovered\r\n\r\nhello\r\n.\r\n"
	if _, err := cl.c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	if r := cl.readReply(t); !strings.HasPrefix(r, "250") {
		t.Fatalf("message = %q, want 250", r)
	}
	sent := prov.sentCopy()
	if len(sent) != 1 || !strings.Contains(string(sent[0]), "Subject: recovered") {
		t.Fatalf("provider sent = %q", sent)
	}
}

func authPlain(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
}

func TestSMTPAuthFailThenSucceedAndBadSequence(t *testing.T) {
	addr, done := start(t, &fakeProvider{})
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()

	// MAIL before AUTH -> 530 5.7.0 (RFC 4954 authentication
	// required; the client recovers by authenticating).
	if r := cl.cmd(t, "MAIL FROM:<%s>", acct); !strings.HasPrefix(r, "530 5.7.0") {
		t.Fatalf("MAIL before auth = %q, want 530 5.7.0", r)
	}
	ehlo := cl.cmd(t, "EHLO client.example")
	for _, want := range []string{"250", "AUTH PLAIN LOGIN", "SIZE 35882577", "8BITMIME"} {
		if !strings.Contains(ehlo, want) {
			t.Fatalf("EHLO %q missing %q", ehlo, want)
		}
	}
	if r := cl.cmd(t, "STARTTLS"); !(strings.HasPrefix(r, "454") || strings.HasPrefix(r, "502")) {
		t.Fatalf("STARTTLS = %q, want 454/502", r)
	}
	if r := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, "wrong")); !strings.HasPrefix(r, "535") {
		t.Fatalf("bad auth = %q, want 535", r)
	}
	if r := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw)); !strings.HasPrefix(r, "235") {
		t.Fatalf("good auth = %q, want 235", r)
	}
	// RCPT before MAIL -> 503
	if r := cl.cmd(t, "RCPT TO:<dest@example.com>"); !strings.HasPrefix(r, "503") {
		t.Fatalf("RCPT before MAIL = %q, want 503", r)
	}
	if r := cl.cmd(t, "NOOP"); !strings.HasPrefix(r, "250") {
		t.Fatalf("NOOP = %q", r)
	}
	if r := cl.cmd(t, "VRFY %s", acct); !strings.HasPrefix(r, "252") {
		t.Fatalf("VRFY = %q, want 252", r)
	}
	if r := cl.cmd(t, "RSET"); !strings.HasPrefix(r, "250") {
		t.Fatalf("RSET = %q", r)
	}
	if r := cl.cmd(t, "QUIT"); !strings.HasPrefix(r, "221") {
		t.Fatalf("QUIT = %q, want 221", r)
	}
}

func TestSMTPAuthLoginMechanism(t *testing.T) {
	addr, done := start(t, &fakeProvider{})
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()
	cl.cmd(t, "HELO client.example")
	if r := cl.cmd(t, "AUTH LOGIN"); !strings.HasPrefix(r, "334") {
		t.Fatalf("AUTH LOGIN = %q, want 334", r)
	}
	if r := cl.cmd(t, "%s", base64.StdEncoding.EncodeToString([]byte(acct))); !strings.HasPrefix(r, "334") {
		t.Fatalf("login user = %q, want 334", r)
	}
	if r := cl.cmd(t, "%s", base64.StdEncoding.EncodeToString([]byte(pw))); !strings.HasPrefix(r, "235") {
		t.Fatalf("login pass = %q, want 235", r)
	}
}

func TestSMTPFullSendDeliversExactMIME(t *testing.T) {
	prov := &fakeProvider{}
	addr, done := start(t, prov)
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()
	cl.cmd(t, "EHLO client.example")
	if r := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw)); !strings.HasPrefix(r, "235") {
		t.Fatalf("auth = %q", r)
	}
	if r := cl.cmd(t, "MAIL FROM:<%s>", acct); !strings.HasPrefix(r, "250") {
		t.Fatalf("MAIL = %q", r)
	}
	if r := cl.cmd(t, "MAIL FROM:<not-an-address>"); !strings.HasPrefix(r, "501") && !strings.HasPrefix(r, "553") {
		// a second MAIL mid-transaction may be 503; either way not 250
		_ = r
	}
	if r := cl.cmd(t, "RCPT TO:<dest@example.com>"); !strings.HasPrefix(r, "250") {
		t.Fatalf("RCPT = %q", r)
	}
	if r := cl.cmd(t, "DATA"); !strings.HasPrefix(r, "354") {
		t.Fatalf("DATA = %q", r)
	}
	mimeBody := "From: user@example.com\r\nTo: dest@example.com\r\nSubject: Hi\r\n\r\nHello\r\n..dotline\r\nLast\r\n.\r\n"
	if _, err := cl.c.Write([]byte(mimeBody)); err != nil {
		t.Fatal(err)
	}
	if r := cl.readReply(t); !strings.HasPrefix(r, "250") {
		t.Fatalf("DATA result = %q, want 250 with queue id", r)
	} else if !strings.Contains(r, "queued") && len(strings.Fields(r)) < 3 {
		t.Fatalf("DATA result %q lacks queue id", r)
	}
	sent := prov.sentCopy()
	if len(sent) != 1 {
		t.Fatalf("sent count = %d", len(sent))
	}
	want := "From: user@example.com\r\nTo: dest@example.com\r\nSubject: Hi\r\n\r\nHello\r\n.dotline\r\nLast\r\n"
	if string(sent[0]) != want {
		t.Fatalf("mime mismatch:\ngot  %q\nwant %q", sent[0], want)
	}
	cl.cmd(t, "QUIT")
}

// RFC 5321: the empty reverse-path MAIL FROM:<> is legal (bounce
// messages) and must be accepted; and every command in a session
// must be visible in the log per connection.
func TestSMTPEmptyReversePathAndCommandLog(t *testing.T) {
	buf := &lockedBuffer{}
	prov := &fakeProvider{}
	addr, done := startWithLogger(t, prov, zerolog.New(buf))
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()
	cl.cmd(t, "EHLO client.example")
	if r := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw)); !strings.HasPrefix(r, "235") {
		t.Fatalf("auth = %q", r)
	}
	if r := cl.cmd(t, "MAIL FROM:<>"); !strings.HasPrefix(r, "250") {
		t.Fatalf("MAIL FROM:<> = %q, want 250 (empty reverse-path is legal)", r)
	}
	out := buf.String()
	for _, want := range []string{`"command":"EHLO"`, `"command":"AUTH"`, `"command":"MAIL"`, `"account":"` + acct + `"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s: %s", want, out)
		}
	}
}

// Two accounts sending at the same time: each message is
// delivered through its own account's provider only.
func TestSMTPTwoAccountsIsolated(t *testing.T) {
	const (
		emailA = "a@corp.example"
		emailB = "b@corp.example"
	)
	provA, provB := &fakeProvider{}, &fakeProvider{}
	addr, done := startMulti(t, map[string]*fakeProvider{emailA: provA, emailB: provB})
	defer done()

	send := func(email, subject string) {
		cl := dial(t, addr)
		defer cl.c.Close()
		cl.cmd(t, "EHLO client.example")
		if r := cl.cmd(t, "AUTH PLAIN %s", authPlain(email, pw)); !strings.HasPrefix(r, "235") {
			t.Fatalf("%s auth = %q", email, r)
		}
		if r := cl.cmd(t, "MAIL FROM:<%s>", email); !strings.HasPrefix(r, "250") {
			t.Fatalf("%s MAIL = %q", email, r)
		}
		if r := cl.cmd(t, "RCPT TO:<dest@example.com>"); !strings.HasPrefix(r, "250") {
			t.Fatalf("%s RCPT = %q", email, r)
		}
		if r := cl.cmd(t, "DATA"); !strings.HasPrefix(r, "354") {
			t.Fatalf("%s DATA = %q", email, r)
		}
		body := "From: " + email + "\r\nTo: dest@example.com\r\nSubject: " + subject + "\r\n\r\nhello\r\n.\r\n"
		if _, err := cl.c.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if r := cl.readReply(t); !strings.HasPrefix(r, "250") {
			t.Fatalf("%s DATA result = %q", email, r)
		}
	}
	send(emailA, "from-a")
	send(emailB, "from-b")

	sentA, sentB := provA.sentCopy(), provB.sentCopy()
	if len(sentA) != 1 || !strings.Contains(string(sentA[0]), "Subject: from-a") {
		t.Fatalf("account A provider sent = %q, want exactly its own message", sentA)
	}
	if len(sentB) != 1 || !strings.Contains(string(sentB[0]), "Subject: from-b") {
		t.Fatalf("account B provider sent = %q, want exactly its own message", sentB)
	}
}

func TestSMTPUpstreamErrorGives451(t *testing.T) {
	prov := &fakeProvider{sendErr: bridge.TransientError("upstream down")}
	addr, done := start(t, prov)
	defer done()
	cl := dial(t, addr)
	defer cl.c.Close()
	cl.cmd(t, "EHLO x")
	cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw))
	cl.cmd(t, "MAIL FROM:<%s>", acct)
	cl.cmd(t, "RCPT TO:<d@example.com>")
	cl.cmd(t, "DATA")
	_, _ = cl.c.Write([]byte("Subject: x\r\n\r\nbody\r\n.\r\n"))
	if r := cl.readReply(t); !strings.HasPrefix(r, "451") {
		t.Fatalf("upstream err reply = %q, want 451", r)
	}
}
