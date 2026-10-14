package imapserver_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/imapserver"
)

// ---------- fakes ----------

type fakeLogin struct{}

func (fakeLogin) Login(_ context.Context, email string, ch bridge.Channel) (bridge.Session, error) {
	return bridge.Session{Email: email, Channel: ch, AccessToken: "tok", TokenExpiry: time.Now().Add(time.Hour)}, nil
}
func (fakeLogin) Renew(_ context.Context, _ string, s bridge.Session) (bridge.Session, error) {
	s.TokenExpiry = time.Now().Add(time.Hour)
	return s, nil
}

type fakeMsg struct {
	meta bridge.MessageMeta
	raw  []byte
}

type flagsCall struct {
	folder string
	ids    []string
	add    bridge.Flags
	remove bridge.Flags
}

type fakeProvider struct {
	mu          sync.Mutex
	folders     []bridge.Folder
	msgs        map[string][]*fakeMsg
	nextID      int
	setFlags    []flagsCall
	deleted     [][]string
	copiedTo    []string
	copiedIDs   []string
	movedTo     []string
	movedIDs    []string
	appendedTo  []string
	createdName []string
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		folders: []bridge.Folder{
			{ID: "inbox", Name: "INBOX", Special: "inbox"},
			{ID: "sent", Name: "Sent Items", Special: "sent"},
			{ID: "archive", Name: "Archive", Special: "archive"},
		},
		msgs: map[string][]*fakeMsg{},
	}
}

func (p *fakeProvider) Ping(context.Context) error { return nil }

func (p *fakeProvider) ListFolders(context.Context) ([]bridge.Folder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bridge.Folder(nil), p.folders...), nil
}

func (p *fakeProvider) CreateFolder(_ context.Context, name, parentID string) (bridge.Folder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createdName = append(p.createdName, name)
	f := bridge.Folder{ID: "folder-" + name, Name: name, ParentID: parentID}
	p.folders = append(p.folders, f)
	return f, nil
}

func (p *fakeProvider) DeleteFolder(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, f := range p.folders {
		if f.ID == id {
			p.folders = append(p.folders[:i], p.folders[i+1:]...)
		}
	}
	delete(p.msgs, id)
	return nil
}

func (p *fakeProvider) RenameFolder(_ context.Context, id, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.folders {
		if p.folders[i].ID == id {
			p.folders[i].Name = name
		}
	}
	return nil
}

func (p *fakeProvider) ListMessages(_ context.Context, folderID string, offset, limit int) ([]bridge.MessageMeta, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := p.msgs[folderID]
	total := len(all)
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	out := []bridge.MessageMeta{}
	for _, m := range all[offset:end] {
		out = append(out, m.meta)
	}
	return out, total, nil
}

func (p *fakeProvider) GetMessage(_ context.Context, folderID, id string) (bridge.MessageMeta, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.msgs[folderID] {
		if m.meta.ID == id {
			return m.meta, nil
		}
	}
	return bridge.MessageMeta{}, fmt.Errorf("not found")
}

func (p *fakeProvider) GetMessageMIME(_ context.Context, folderID, id string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.msgs[folderID] {
		if m.meta.ID == id {
			return append([]byte(nil), m.raw...), nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (p *fakeProvider) SearchMessages(_ context.Context, folderID string, c bridge.SearchCriteria) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, m := range p.msgs[folderID] {
		if c.Text != "" && !strings.Contains(strings.ToLower(m.meta.Subject+" "+m.meta.From), strings.ToLower(c.Text)) {
			continue
		}
		if c.From != "" && !strings.Contains(strings.ToLower(m.meta.From), strings.ToLower(c.From)) {
			continue
		}
		if c.Subject != "" && !strings.Contains(strings.ToLower(m.meta.Subject), strings.ToLower(c.Subject)) {
			continue
		}
		if !c.Since.IsZero() && m.meta.Date.Before(c.Since) {
			continue
		}
		if !c.Before.IsZero() && !m.meta.Date.Before(c.Before) {
			continue
		}
		if c.Unseen && hasFlag(m.meta.Flags, "\\seen") {
			continue
		}
		if c.Flagged && !hasFlag(m.meta.Flags, "\\flagged") {
			continue
		}
		out = append(out, m.meta.ID)
	}
	return out, nil
}

func hasFlag(f bridge.Flags, want string) bool {
	for _, x := range f {
		if strings.EqualFold(x, want) {
			return true
		}
	}
	return false
}

func (p *fakeProvider) SetFlags(_ context.Context, folderID string, ids []string, add, remove bridge.Flags) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setFlags = append(p.setFlags, flagsCall{folderID, append([]string(nil), ids...), append(bridge.Flags(nil), add...), append(bridge.Flags(nil), remove...)})
	for _, m := range p.msgs[folderID] {
		for _, id := range ids {
			if m.meta.ID != id {
				continue
			}
			for _, a := range add {
				if !hasFlag(m.meta.Flags, a) {
					m.meta.Flags = append(m.meta.Flags, strings.ToLower(a))
				}
			}
			for _, r := range remove {
				var nf bridge.Flags
				for _, f := range m.meta.Flags {
					if !strings.EqualFold(f, r) {
						nf = append(nf, f)
					}
				}
				m.meta.Flags = nf
			}
		}
	}
	return nil
}

func (p *fakeProvider) MoveMessages(_ context.Context, folderID string, ids []string, dest string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.movedTo = append(p.movedTo, dest)
	p.movedIDs = append(p.movedIDs, ids...)
	var remain []*fakeMsg
	for _, m := range p.msgs[folderID] {
		moved := false
		for _, id := range ids {
			if m.meta.ID == id {
				moved = true
			}
		}
		if moved {
			cp := *m
			cp.meta.FolderID = dest
			p.msgs[dest] = append(p.msgs[dest], &cp)
		} else {
			remain = append(remain, m)
		}
	}
	p.msgs[folderID] = remain
	return nil
}

func (p *fakeProvider) CopyMessages(_ context.Context, folderID string, ids []string, dest string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.copiedTo = append(p.copiedTo, dest)
	p.copiedIDs = append(p.copiedIDs, ids...)
	for _, m := range p.msgs[folderID] {
		for _, id := range ids {
			if m.meta.ID == id {
				cp := *m
				cp.meta.FolderID = dest
				p.msgs[dest] = append(p.msgs[dest], &cp)
			}
		}
	}
	return nil
}

func (p *fakeProvider) DeleteMessages(_ context.Context, folderID string, ids []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, append([]string(nil), ids...))
	var remain []*fakeMsg
	for _, m := range p.msgs[folderID] {
		keep := true
		for _, id := range ids {
			if m.meta.ID == id {
				keep = false
			}
		}
		if keep {
			remain = append(remain, m)
		}
	}
	p.msgs[folderID] = remain
	return nil
}

func (p *fakeProvider) AppendMessage(_ context.Context, folderID string, mime []byte, flags bridge.Flags) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendedTo = append(p.appendedTo, folderID)
	p.nextID++
	id := fmt.Sprintf("appended-%d", p.nextID)
	meta := parseMeta(id, folderID, mime, flags)
	p.msgs[folderID] = append(p.msgs[folderID], &fakeMsg{meta: meta, raw: append([]byte(nil), mime...)})
	return id, nil
}

func (p *fakeProvider) SendMessage(context.Context, []byte) error { return nil }

// calendar / tasks / contacts stubs
func (p *fakeProvider) ListCalendars(context.Context) ([]bridge.Calendar, error) {
	return nil, nil
}
func (p *fakeProvider) ListEvents(context.Context, string, time.Time, time.Time) ([]bridge.Event, error) {
	return nil, nil
}
func (p *fakeProvider) CreateEvent(_ context.Context, e bridge.Event) (bridge.Event, error) {
	return e, nil
}
func (p *fakeProvider) UpdateEvent(context.Context, bridge.Event) error   { return nil }
func (p *fakeProvider) DeleteEvent(context.Context, string, string) error { return nil }
func (p *fakeProvider) RespondEvent(context.Context, string, string, string) error {
	return nil
}
func (p *fakeProvider) FreeBusy(context.Context, []string, time.Time, time.Time) (map[string][][2]time.Time, error) {
	return nil, nil
}
func (p *fakeProvider) ListTaskLists(context.Context) ([]bridge.Calendar, error) {
	return nil, nil
}
func (p *fakeProvider) ListTasks(context.Context, string) ([]bridge.Task, error) {
	return nil, nil
}
func (p *fakeProvider) CreateTask(_ context.Context, t bridge.Task) (bridge.Task, error) {
	return t, nil
}
func (p *fakeProvider) UpdateTask(context.Context, bridge.Task) error { return nil }
func (p *fakeProvider) DeleteTask(context.Context, string, string) error {
	return nil
}
func (p *fakeProvider) ListContacts(context.Context, string) ([]bridge.Contact, error) {
	return nil, nil
}
func (p *fakeProvider) CreateContact(_ context.Context, c bridge.Contact) (bridge.Contact, error) {
	return c, nil
}
func (p *fakeProvider) UpdateContact(context.Context, bridge.Contact) error { return nil }
func (p *fakeProvider) DeleteContact(context.Context, string, string) error {
	return nil
}

func parseMeta(id, folderID string, raw []byte, flags bridge.Flags) bridge.MessageMeta {
	m := bridge.MessageMeta{ID: id, FolderID: folderID, Flags: flags, Size: int64(len(raw)), Date: time.Now().UTC().Truncate(time.Second)}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "subject":
			m.Subject = v
		case "from":
			m.From = v
		case "to":
			m.To = []string{v}
		case "date":
			if t, err := time.Parse(time.RFC1123Z, v); err == nil {
				m.Date = t
			}
		}
	}
	return m
}

func (p *fakeProvider) addMessage(folderID, raw string, flags bridge.Flags, date time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	id := fmt.Sprintf("msg-%d", p.nextID)
	meta := parseMeta(id, folderID, []byte(raw), flags)
	meta.Date = date
	p.msgs[folderID] = append(p.msgs[folderID], &fakeMsg{meta: meta, raw: []byte(raw)})
	return id
}

type fakeFactory struct{ prov *fakeProvider }

func (f fakeFactory) New(string, bridge.Session) bridge.Provider { return f.prov }

// ---------- server harness ----------

const testEmail = "user@example.com"
const testPassword = "instance-secret"

func startServer(t *testing.T) (*fakeProvider, net.Listener) {
	t.Helper()
	return startServerWithLogger(t, zerolog.Nop())
}

func startServerWithLogger(t *testing.T, logger zerolog.Logger) (*fakeProvider, net.Listener) {
	t.Helper()
	prov := newFakeProvider()
	reg, err := bridge.NewRegistry(bridge.Config{
		StateDir: t.TempDir(),
		Login:    fakeLogin{},
		Factory:  fakeFactory{prov: prov},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := imapserver.New(reg, testPassword, logger)
	srv.IdlePollInterval = 50 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return prov, ln
}

type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dial(t *testing.T, ln net.Listener) *client {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	greeting := c.readLine()
	if !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("greeting = %q", greeting)
	}
	t.Cleanup(func() { conn.Close() })
	return c
}

func (c *client) readLine() string {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read line: %v (got %q)", err, line)
	}
	return strings.TrimRight(line, "\r\n")
}

// cmd sends one command and reads until the tagged response.
func (c *client) cmd(tag, line string) []string {
	c.t.Helper()
	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, line); err != nil {
		c.t.Fatal(err)
	}
	return c.readUntil(tag)
}

func (c *client) readUntil(tag string) []string {
	c.t.Helper()
	var out []string
	for {
		line := c.readLine()
		out = append(out, line)
		if strings.HasPrefix(line, tag+" ") {
			return out
		}
	}
}

func (c *client) login(tag string) {
	c.t.Helper()
	resp := c.cmd(tag, "LOGIN "+testEmail+" "+testPassword)
	joined := strings.Join(resp, "\n")
	if !strings.Contains(joined, tag+" OK") {
		c.t.Fatalf("login failed: %s", joined)
	}
}

func joined(resp []string) string { return strings.Join(resp, "\n") }

func sampleRaw(subject, body string) string {
	return "From: Alice <alice@example.com>\r\nTo: user@example.com\r\nSubject: " + subject + "\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
}

// A real client using a synchronizing literal ({n} without '+')
// waits for the server continuation before sending the literal
// bytes. The server must send "+ ..." or APPEND deadlocks.
func TestAppendSynchronizingLiteralWaitsForContinuation(t *testing.T) {
	prov, ln := startServer(t)
	c := dial(t, ln)
	c.login("a")
	raw := sampleRaw("Draft sync", "body")
	if _, err := fmt.Fprintf(c.conn, "b APPEND INBOX (\\Draft) {%d}\r\n", len(raw)); err != nil {
		t.Fatal(err)
	}
	cont := c.readLine()
	if !strings.HasPrefix(cont, "+") {
		t.Fatalf("expected continuation, got %q", cont)
	}
	if _, err := c.conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	resp := c.readUntil("b")
	if !strings.Contains(joined(resp), "b OK") {
		t.Fatalf("APPEND resp: %s", joined(resp))
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.appendedTo) != 1 {
		t.Fatalf("appendedTo = %v", prov.appendedTo)
	}
}

// Auth failures and operations must be visible in the bridge log
// per account (a failed login used to leave no trace above debug).
func TestIMAPAuthFailureAndAppendAreLoggedPerAccount(t *testing.T) {
	buf := &lockedBuffer{}
	logger := zerolog.New(buf)
	_, ln := startServerWithLogger(t, logger)
	c := dial(t, ln)
	bad := c.cmd("a", "LOGIN "+testEmail+" wrong-password")
	if !strings.Contains(joined(bad), "a NO") {
		t.Fatalf("bad login: %s", joined(bad))
	}
	out := buf.String()
	for _, want := range []string{`"level":"warn"`, `"protocol":"imap"`, `"account":"` + testEmail + `"`, `"op":"auth"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s: %s", want, out)
		}
	}
	buf.Reset()
	c.login("b")
	raw := sampleRaw("Logged draft", "x")
	if _, err := fmt.Fprintf(c.conn, "c APPEND INBOX (\\Draft) {%d}\r\n", len(raw)); err != nil {
		t.Fatal(err)
	}
	if cont := c.readLine(); !strings.HasPrefix(cont, "+") {
		t.Fatalf("continuation = %q", cont)
	}
	if _, err := c.conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if resp := c.readUntil("c"); !strings.Contains(joined(resp), "c OK") {
		t.Fatalf("APPEND: %s", joined(resp))
	}
	out = buf.String()
	for _, want := range []string{`"level":"info"`, `"protocol":"imap"`, `"account":"` + testEmail + `"`, `"op":"append"`, `"folder_id":"inbox"`, `"item":"appended-1"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s: %s", want, out)
		}
	}
}

// A command sent before authentication must be warning-logged
// (it used to return NO with no trace above debug).
func TestIMAPUnauthenticatedCommandWarnLogged(t *testing.T) {
	buf := &lockedBuffer{}
	_, ln := startServerWithLogger(t, zerolog.New(buf))
	c := dial(t, ln)
	resp := c.cmd("a", "SELECT INBOX")
	if !strings.Contains(joined(resp), "a NO") {
		t.Fatalf("unauth SELECT: %s", joined(resp))
	}
	out := buf.String()
	for _, want := range []string{`"level":"warn"`, `"protocol":"imap"`, `"op":"select"`} {
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

// ---------- tests ----------

func TestGreetingAndLoginOkBadPassword(t *testing.T) {
	_, ln := startServer(t)
	c := dial(t, ln)
	bad := c.cmd("a", "LOGIN "+testEmail+" wrong-password")
	if !strings.Contains(joined(bad), "a NO") {
		t.Fatalf("bad password should NO, got %s", joined(bad))
	}
	ok := c.cmd("b", "LOGIN "+testEmail+" "+testPassword)
	if !strings.Contains(joined(ok), "b OK") {
		t.Fatalf("good login: %s", joined(ok))
	}
	// AUTHENTICATE PLAIN (initial response)
	c2 := dial(t, ln)
	payload := "\x00" + testEmail + "\x00" + testPassword
	resp := c2.cmd("a", "AUTHENTICATE PLAIN "+base64.StdEncoding.EncodeToString([]byte(payload)))
	if !strings.Contains(joined(resp), "a OK") {
		t.Fatalf("AUTHENTICATE PLAIN: %s", joined(resp))
	}
	// AUTHENTICATE PLAIN (challenge flow)
	c3 := dial(t, ln)
	if _, err := fmt.Fprintf(c3.conn, "a AUTHENTICATE PLAIN\r\n"); err != nil {
		t.Fatal(err)
	}
	if chal := c3.readLine(); !strings.HasPrefix(chal, "+") {
		t.Fatalf("expected challenge, got %q", chal)
	}
	if _, err := fmt.Fprintf(c3.conn, "%s\r\n", base64.StdEncoding.EncodeToString([]byte(payload))); err != nil {
		t.Fatal(err)
	}
	if final := c3.readUntil("a"); !strings.Contains(joined(final), "a OK") {
		t.Fatalf("auth plain challenge flow: %s", joined(final))
	}
}

func TestListShowsFolders(t *testing.T) {
	_, ln := startServer(t)
	c := dial(t, ln)
	c.login("a")
	resp := c.cmd("b", `LIST "" "*"`)
	j := joined(resp)
	for _, want := range []string{"INBOX", "Sent Items", "Archive"} {
		if !strings.Contains(j, want) {
			t.Fatalf("LIST missing %q: %s", want, j)
		}
	}
	if !strings.Contains(j, "b OK") {
		t.Fatalf("LIST not OK: %s", j)
	}
}

func TestSelectUIDValidityStableAcrossConnections(t *testing.T) {
	prov, ln := startServer(t)
	prov.addMessage("inbox", sampleRaw("Hello", "body one"), nil, time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
	getValidity := func() string {
		c := dial(t, ln)
		c.login("a")
		resp := c.cmd("b", "SELECT INBOX")
		j := joined(resp)
		if !strings.Contains(j, "b OK [READ-WRITE]") || !strings.Contains(j, "* 1 EXISTS") {
			t.Fatalf("SELECT: %s", j)
		}
		for _, line := range resp {
			if strings.Contains(line, "UIDVALIDITY") {
				f := strings.Fields(strings.Trim(line, "[]"))
				_ = f
				// "* OK [UIDVALIDITY n] ..."
				start := strings.Index(line, "UIDVALIDITY")
				rest := strings.Fields(line[start:])
				if len(rest) >= 2 {
					return strings.Trim(rest[1], "]")
				}
			}
		}
		t.Fatalf("no UIDVALIDITY in %s", j)
		return ""
	}
	v1 := getValidity()
	v2 := getValidity()
	if v1 == "" || v1 != v2 {
		t.Fatalf("UIDVALIDITY unstable: %q vs %q", v1, v2)
	}
	if _, err := strconv.Atoi(v1); err != nil {
		t.Fatalf("UIDVALIDITY not numeric: %q", v1)
	}
}

func TestAppendThenFetchBody(t *testing.T) {
	prov, ln := startServer(t)
	c := dial(t, ln)
	c.login("a")
	raw := sampleRaw("Appended Subject", "appended body text")
	c2 := c
	if _, err := fmt.Fprintf(c2.conn, "b APPEND INBOX {%d}\r\n", len(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c2.conn, "\r\n"); err != nil {
		t.Fatal(err)
	}
	resp := c2.readUntil("b")
	if !strings.Contains(joined(resp), "b OK") {
		t.Fatalf("APPEND: %s", joined(resp))
	}
	if !strings.Contains(joined(resp), "APPENDUID") {
		t.Fatalf("APPEND should return APPENDUID: %s", joined(resp))
	}
	sel := c2.cmd("c", "SELECT INBOX")
	if !strings.Contains(joined(sel), "* 1 EXISTS") {
		t.Fatalf("after append SELECT: %s", joined(sel))
	}
	fetch := c2.cmd("d", "FETCH 1 (UID FLAGS RFC822.SIZE BODY[])")
	j := joined(fetch)
	if !strings.Contains(j, "appended body text") || !strings.Contains(j, "Appended Subject") {
		t.Fatalf("FETCH BODY[] missing bytes: %s", j)
	}
	if !strings.Contains(j, "UID ") {
		t.Fatalf("FETCH missing UID: %s", j)
	}
	_ = prov
}

func TestStoreSeenThenFetchFlags(t *testing.T) {
	prov, ln := startServer(t)
	id := prov.addMessage("inbox", sampleRaw("Flag Me", "x"), nil, time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
	c := dial(t, ln)
	c.login("a")
	c.cmd("b", "SELECT INBOX")
	store := c.cmd("c", `STORE 1 +FLAGS (\Seen)`)
	if !strings.Contains(joined(store), "c OK") {
		t.Fatalf("STORE: %s", joined(store))
	}
	fetch := c.cmd("d", "FETCH 1 (FLAGS)")
	if !strings.Contains(joined(fetch), `\Seen`) {
		t.Fatalf("FETCH FLAGS missing Seen: %s", joined(fetch))
	}
	found := false
	prov.mu.Lock()
	for _, call := range prov.setFlags {
		for _, got := range call.ids {
			if got == id && hasFlag(call.add, `\seen`) {
				found = true
			}
		}
	}
	prov.mu.Unlock()
	if !found {
		t.Fatalf("provider SetFlags not recorded for %s: %+v", id, prov.setFlags)
	}
}

func TestUIDSearchUnseen(t *testing.T) {
	prov, ln := startServer(t)
	prov.addMessage("inbox", sampleRaw("Seen One", "x"), bridge.Flags{"\\seen"}, time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
	prov.addMessage("inbox", sampleRaw("Unseen One", "x"), nil, time.Date(2006, 1, 3, 15, 4, 5, 0, time.UTC))
	c := dial(t, ln)
	c.login("a")
	c.cmd("b", "SELECT INBOX")
	// seq search: only seq 2 unseen
	resp := c.cmd("c", "SEARCH UNSEEN")
	j := joined(resp)
	if !strings.Contains(j, "* SEARCH 2") {
		t.Fatalf("SEARCH UNSEEN want '* SEARCH 2': %s", j)
	}
	uresp := c.cmd("d", "UID SEARCH UNSEEN")
	uj := joined(uresp)
	// UID of second message should be 2
	if !strings.Contains(uj, "* SEARCH 2") {
		t.Fatalf("UID SEARCH UNSEEN want '* SEARCH 2': %s", uj)
	}
}

func TestCopyAndMove(t *testing.T) {
	prov, ln := startServer(t)
	id1 := prov.addMessage("inbox", sampleRaw("CopyMe", "x"), nil, time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
	id2 := prov.addMessage("inbox", sampleRaw("MoveMe", "x"), nil, time.Date(2006, 1, 3, 15, 4, 5, 0, time.UTC))
	c := dial(t, ln)
	c.login("a")
	c.cmd("b", "SELECT INBOX")
	cp := c.cmd("c", "COPY 1 Archive")
	if !strings.Contains(joined(cp), "c OK") {
		t.Fatalf("COPY: %s", joined(cp))
	}
	mv := c.cmd("d", "UID MOVE 2 Archive")
	// UID MOVE seq set is UID; UID of MoveMe is 2 (but response varies) - check OK
	if !strings.Contains(joined(mv), "d OK") {
		t.Fatalf("MOVE: %s", joined(mv))
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.copiedTo) != 1 || prov.copiedTo[0] != "archive" || len(prov.copiedIDs) != 1 || prov.copiedIDs[0] != id1 {
		t.Fatalf("copy recorded: to=%v ids=%v want archive [%s]", prov.copiedTo, prov.copiedIDs, id1)
	}
	if len(prov.movedTo) != 1 || prov.movedTo[0] != "archive" || len(prov.movedIDs) != 1 || prov.movedIDs[0] != id2 {
		t.Fatalf("move recorded: to=%v ids=%v want archive [%s]", prov.movedTo, prov.movedIDs, id2)
	}
}

func TestIdleEmitsExists(t *testing.T) {
	prov, ln := startServer(t)
	prov.addMessage("inbox", sampleRaw("First", "x"), nil, time.Now())
	c := dial(t, ln)
	c.login("a")
	c.cmd("b", "SELECT INBOX")
	if _, err := fmt.Fprintf(c.conn, "c IDLE\r\n"); err != nil {
		t.Fatal(err)
	}
	line := c.readLine()
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("IDLE continuation = %q", line)
	}
	prov.addMessage("inbox", sampleRaw("Second", "y"), nil, time.Now())
	// read until EXISTS
	found := false
	for i := 0; i < 20; i++ {
		l := c.readLine()
		if strings.Contains(l, "EXISTS") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("IDLE did not emit EXISTS")
	}
	if _, err := fmt.Fprintf(c.conn, "DONE\r\n"); err != nil {
		t.Fatal(err)
	}
	resp := c.readUntil("c")
	if !strings.Contains(joined(resp), "c OK") {
		t.Fatalf("IDLE DONE: %s", joined(resp))
	}
}

func TestExpungeAfterStoreDeleted(t *testing.T) {
	prov, ln := startServer(t)
	id := prov.addMessage("inbox", sampleRaw("DeleteMe", "x"), nil, time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
	prov.addMessage("inbox", sampleRaw("KeepMe", "x"), nil, time.Date(2006, 1, 3, 15, 4, 5, 0, time.UTC))
	c := dial(t, ln)
	c.login("a")
	c.cmd("b", "SELECT INBOX")
	c.cmd("c", `STORE 1 +FLAGS (\Deleted)`)
	exp := c.cmd("d", "EXPUNGE")
	j := joined(exp)
	if !strings.Contains(j, "EXPUNGE") || !strings.Contains(j, "d OK") {
		t.Fatalf("EXPUNGE: %s", j)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	found := false
	for _, del := range prov.deleted {
		for _, d := range del {
			if d == id {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("provider DeleteMessages not called with %s: %v", id, prov.deleted)
	}
	// folder now has 1 message upstream
	if len(prov.msgs["inbox"]) != 1 {
		t.Fatalf("inbox len after expunge = %d", len(prov.msgs["inbox"]))
	}
}

func TestStatusAndFetchEnvelope(t *testing.T) {
	prov, ln := startServer(t)
	prov.addMessage("inbox", sampleRaw("Hello World", "body"), nil, time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
	c := dial(t, ln)
	c.login("a")
	st := c.cmd("b", `STATUS INBOX (MESSAGES UNSEEN UIDNEXT UIDVALIDITY)`)
	j := joined(st)
	if !strings.Contains(j, "MESSAGES 1") || !strings.Contains(j, "UNSEEN 1") {
		t.Fatalf("STATUS: %s", j)
	}
	c.cmd("c", "SELECT INBOX")
	f := c.cmd("d", "FETCH 1 (ENVELOPE BODYSTRUCTURE INTERNALDATE)")
	fj := joined(f)
	if !strings.Contains(fj, "Hello World") {
		t.Fatalf("ENVELOPE missing subject: %s", fj)
	}
	if !strings.Contains(strings.ToLower(fj), "text") {
		t.Fatalf("BODYSTRUCTURE: %s", fj)
	}
}
