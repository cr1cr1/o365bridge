package ws_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/server"
	"github.com/cr1cr1/o365bridge/jmap/ws"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, r *http.Request) (*server.Principal, error) {
	if r.Header.Get("Authorization") != "Bearer good" {
		return nil, fmt.Errorf("bad credentials")
	}
	return &server.Principal{
		Username:        "user@example.com",
		Accounts:        map[jmap.ID]*jmap.Account{"acct1": {Name: "User", IsPersonal: true}},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.CoreURI: "acct1"},
	}, nil
}

func newServer(t *testing.T) (*server.Engine, *httptest.Server) {
	t.Helper()
	e := server.New(fakeAuth{}, server.Options{})
	mux := http.NewServeMux()
	mux.Handle("/jmap/ws/", ws.New(e))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return e, srv
}

// --- hand-rolled WebSocket client (client->server frames masked) ---

type client struct {
	t      *testing.T
	conn   net.Conn
	br     *bufio.Reader
	status int
	header http.Header
}

func dial(t *testing.T, srv *httptest.Server, auth bool, protocols string) *client {
	t.Helper()
	addr := strings.TrimPrefix(srv.URL, "http://")
	nc, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	if err := nc.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	var b strings.Builder
	b.WriteString("GET /jmap/ws/ HTTP/1.1\r\nHost: " + addr + "\r\n")
	b.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n")
	if protocols != "" {
		b.WriteString("Sec-WebSocket-Protocol: " + protocols + "\r\n")
	}
	if auth {
		b.WriteString("Authorization: Bearer good\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(nc, b.String()); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(nc)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var status int
	if _, err := fmt.Sscanf(statusLine, "HTTP/1.1 %d", &status); err != nil {
		t.Fatalf("status line %q: %v", statusLine, err)
	}
	hdr := http.Header{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	// Verify accept key when upgraded.
	if status == 101 {
		h := sha1.New()
		io.WriteString(h, key+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
		want := base64.StdEncoding.EncodeToString(h.Sum(nil))
		if got := hdr.Get("Sec-Websocket-Accept"); got != want {
			t.Fatalf("accept = %q, want %q", got, want)
		}
	}
	return &client{t: t, conn: nc, br: br, status: status, header: hdr}
}

func (c *client) writeFrame(op byte, payload []byte) {
	c.t.Helper()
	_ = c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	var hdr []byte
	hdr = append(hdr, 0x80|op)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(hdr); err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.conn.Write(masked); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) writeText(s string) { c.writeFrame(0x1, []byte(s)) }

func (c *client) readFrame() (byte, []byte) {
	c.t.Helper()
	_ = c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		c.t.Fatalf("read frame header: %v", err)
	}
	op := hdr[0] & 0x0F
	if hdr[1]&0x80 != 0 {
		c.t.Fatal("server frame must be unmasked")
	}
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			c.t.Fatal(err)
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			c.t.Fatal(err)
		}
		length = binary.BigEndian.Uint64(b[:])
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		c.t.Fatalf("read frame payload: %v", err)
	}
	return op, payload
}

// readText returns the next text message, failing on other opcodes.
func (c *client) readText() string {
	c.t.Helper()
	op, payload := c.readFrame()
	if op != 0x1 {
		c.t.Fatalf("expected text frame, got opcode %d payload %q", op, payload)
	}
	return string(payload)
}

// tryReadText waits up to timeout for a text frame and reports whether
// one arrived (used to assert the absence of pushes).
func (c *client) tryReadText(timeout time.Duration) (string, bool) {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	defer c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return "", false
	}
	op := hdr[0] & 0x0F
	length := uint64(hdr[1] & 0x7F)
	if length == 126 {
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return "", false
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return "", false
	}
	if op != 0x1 {
		return "", false
	}
	return string(payload), true
}

func TestHandshakeSubprotocolAndEcho(t *testing.T) {
	_, srv := newServer(t)
	cl := dial(t, srv, true, "jmap, other")
	if cl.status != 101 {
		t.Fatalf("status %d", cl.status)
	}
	if got := cl.header.Get("Sec-Websocket-Protocol"); got != "jmap" {
		t.Fatalf("subprotocol %q, want jmap", got)
	}
	// RFC 8887 §4.3.2/§4.3.3: the Request may carry "id", echoed as
	// "requestId" on the Response.
	cl.writeText(`{"@type":"Request","id":"req-7","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"helloWorld":"hi"},"0"]]}`)
	got := cl.readText()
	// Decode manually: methodResponses are arrays.
	var decoded struct {
		Type            string              `json:"@type"`
		RequestID       string              `json:"requestId"`
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
		SessionState    string              `json:"sessionState"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode %q: %v", got, err)
	}
	if decoded.Type != "Response" {
		t.Fatalf("@type %q (%s)", decoded.Type, got)
	}
	if len(decoded.MethodResponses) != 1 {
		t.Fatalf("methodResponses %s", got)
	}
	var name, callID string
	var args map[string]any
	_ = json.Unmarshal(decoded.MethodResponses[0][0], &name)
	_ = json.Unmarshal(decoded.MethodResponses[0][1], &args)
	_ = json.Unmarshal(decoded.MethodResponses[0][2], &callID)
	if name != "Core/echo" || callID != "0" || args["helloWorld"] != "hi" {
		t.Fatalf("echo response %s", got)
	}
	if decoded.SessionState == "" {
		t.Fatalf("empty sessionState in %s", got)
	}
	if decoded.RequestID != "req-7" {
		t.Fatalf("requestId = %q in %s, want req-7", decoded.RequestID, got)
	}
}

func TestBadAuthNoUpgrade(t *testing.T) {
	_, srv := newServer(t)
	cl := dial(t, srv, false, "jmap")
	if cl.status == 101 {
		t.Fatal("upgraded despite bad auth")
	}
	if cl.status != 401 {
		t.Fatalf("status %d, want 401", cl.status)
	}
}

func TestPingPong(t *testing.T) {
	_, srv := newServer(t)
	cl := dial(t, srv, true, "")
	if cl.status != 101 {
		t.Fatalf("status %d", cl.status)
	}
	cl.writeFrame(0x9, []byte("hi"))
	op, payload := cl.readFrame()
	if op != 0xA || string(payload) != "hi" {
		t.Fatalf("pong op=%d payload=%q", op, payload)
	}
}

// RFC 8887 §4.3.5.2/§4.3.5.3: push is enabled per connection with a
// WebSocketPushEnable object honouring dataTypes, and disabled with
// WebSocketPushDisable. Pushes are StateChange objects (§4.3.5.1).
func TestPushEnableDataTypesAndDisable(t *testing.T) {
	e, srv := newServer(t)
	cl := dial(t, srv, true, "jmap")
	if cl.status != 101 {
		t.Fatalf("status %d", cl.status)
	}
	// Enable pushes for Email only.
	cl.writeText(`{"@type":"WebSocketPushEnable","dataTypes":["Email"]}`)
	// Publish a Mailbox-only change: it must NOT reach the client.
	e.Publish(server.StateChange{AccountID: "acct1", Changed: map[string]map[string]string{"acct1": {"Mailbox": "s41"}}})
	// Synchronise on the socket: once the echo arrives, the enable has
	// been processed and the Mailbox push (if wrongly sent) is queued
	// ahead of the echo.
	cl.writeText(`{"@type":"Request","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"sync"]]}`)
	if got := cl.readText(); !strings.Contains(got, `"sync"`) {
		t.Fatalf("sync response = %s", got)
	}
	// Now an Email change must arrive, as a StateChange object.
	e.Publish(server.StateChange{AccountID: "acct1", Changed: map[string]map[string]string{"acct1": {"Email": "s42"}}})
	got := cl.readText()
	var sc struct {
		Type    string                       `json:"@type"`
		Changed map[string]map[string]string `json:"changed"`
	}
	if err := json.Unmarshal([]byte(got), &sc); err != nil {
		t.Fatalf("decode push %q: %v", got, err)
	}
	if sc.Type != "StateChange" || sc.Changed["acct1"]["Email"] != "s42" {
		t.Fatalf("push %s", got)
	}

	// Disable: subsequent changes must not reach the client. Sync on
	// the socket first so the disable is known to have been handled
	// before publishing again.
	cl.writeText(`{"@type":"WebSocketPushDisable"}`)
	cl.writeText(`{"@type":"Request","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"sync-disable"]]}`)
	if got := cl.readText(); !strings.Contains(got, `"sync-disable"`) {
		t.Fatalf("expected sync-disable response, got %s", got)
	}
	e.Publish(server.StateChange{AccountID: "acct1", Changed: map[string]map[string]string{"acct1": {"Email": "s43"}}})
	cl.writeText(`{"@type":"Request","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"sync2"]]}`)
	if got := cl.readText(); !strings.Contains(got, `"sync2"`) {
		t.Fatalf("after disable, expected sync2 response first, got %s", got)
	}
	if extra, ok := cl.tryReadText(300 * time.Millisecond); ok {
		t.Fatalf("unexpected frame after disable: %s", extra)
	}
}

// RFC 8887 §4.3.4: request-level errors are RequestError objects.
func TestRequestErrorTyped(t *testing.T) {
	_, srv := newServer(t)
	cl := dial(t, srv, true, "jmap")
	if cl.status != 101 {
		t.Fatalf("status %d", cl.status)
	}
	cl.writeText(`{"@type":"Request","id":"r9","using":["urn:ietf:params:jmap:bogus"],"methodCalls":[["Core/echo",{},"0"]]}`)
	got := cl.readText()
	var pe struct {
		Type      string `json:"@type"`
		Status    int    `json:"status"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal([]byte(got), &pe); err != nil {
		t.Fatalf("decode %q: %v", got, err)
	}
	if pe.Type != "RequestError" || pe.Status != 400 || pe.RequestID != "r9" {
		t.Fatalf("request-level error = %s, want RequestError 400 with requestId r9", got)
	}
}
