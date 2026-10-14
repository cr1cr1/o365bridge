// Package ws implements JMAP over WebSocket (RFC 8887) on top of the
// server.Engine, using only the standard library: the RFC 6455
// handshake and framing are implemented directly on a hijacked
// net/http connection.
//
// Usage: mount New(engine) at any path, e.g.
//
//	mux.Handle("/jmap/ws/", ws.New(engine))
//
// The host should advertise this endpoint in the Session object as the
// RFC 8887 "webSocketUrl" property. Note: the jmap.Session type in the
// root package has no WebSocketURL field, so this package cannot set it;
// that advertisement remains the host's (or a future root-type change's)
// job. The RFC 8887 capability URI is urn:ietf:params:jmap:websocket
// (jmap.WebSocketURI), which the host may register on the engine.
//
// Protocol (RFC 8887): after the WebSocket handshake (subprotocol
// "jmap"), client and server exchange JSON text messages discriminated
// by "@type". A client sends {"@type":"Request", ...} (a JMAP request
// object) and receives {"@type":"Response", ...}; the server pushes
// {"@type":"StateChange", ...} objects once push is enabled on the
// socket (see the shortcut note on WebSocketPushSubscription handling
// in conn.handleMessage).
package ws

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

const (
	// maxMessageSize is the largest reassembled client message accepted.
	maxMessageSize = 10 << 20 // 10 MiB

	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

const (
	closeNormal      = 1000
	closeProtocol    = 1002
	closeUnsupported = 1003
	closeTooLarge    = 1009
)

// Handler returns an http.Handler serving JMAP over WebSocket for e.
// It is an alias for New.
func Handler(e *server.Engine) http.Handler { return New(e) }

// New returns a path-agnostic http.Handler that authenticates the
// upgrade request with e.AuthenticateRequest (401 before upgrade on
// failure) and then speaks RFC 8887 on the hijacked connection.
func New(e *server.Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(e, w, r)
	})
}

func serve(e *server.Engine, w http.ResponseWriter, r *http.Request) {
	if e == nil {
		http.Error(w, "no engine", http.StatusInternalServerError)
		return
	}
	p, err := e.AuthenticateRequest(r.Context(), r)
	if err != nil || p == nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "websocket handshake requires GET", http.StatusMethodNotAllowed)
		return
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") ||
		!headerHasToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "not a websocket upgrade request", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return
	}

	var resp strings.Builder
	resp.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	resp.WriteString("Upgrade: websocket\r\n")
	resp.WriteString("Connection: Upgrade\r\n")
	resp.WriteString("Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n")
	// RFC 8887 §3.1: the WebSocket subprotocol used is "jmap"; echo it
	// when the client offered it.
	if headerHasToken(r.Header, "Sec-WebSocket-Protocol", "jmap") {
		resp.WriteString("Sec-WebSocket-Protocol: jmap\r\n")
	}
	resp.WriteString("\r\n")
	if _, err := rw.WriteString(resp.String()); err != nil {
		nc.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		nc.Close()
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{
		e:          e,
		p:          p,
		nc:         nc,
		br:         rw.Reader,
		out:        make(chan frame, 32),
		done:       make(chan struct{}),
		writerDone: make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
		pushCtx:    nil,
	}
	c.wg.Add(1)
	go c.writer()
	c.readLoop() // blocks until the connection ends
	cancel()
	c.shutdown()
	c.wg.Wait()
}

// acceptKey computes Sec-WebSocket-Accept (RFC 6455 §1.3).
func acceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// --- frames ---

type frame struct {
	op      byte
	payload []byte
}

type conn struct {
	e  *server.Engine
	p  *server.Principal
	nc net.Conn
	br *bufio.Reader

	out        chan frame
	done       chan struct{}
	writerDone chan struct{}
	once       sync.Once
	wg         sync.WaitGroup

	ctx    context.Context
	cancel context.CancelFunc

	// push state, only touched by the read goroutine.
	pushCtx     context.Context
	pushCancel  context.CancelFunc
	pushCancels []func()
}

func (c *conn) shutdown() {
	c.once.Do(func() {
		close(c.done)
		_ = c.nc.Close()
	})
	if c.pushCancel != nil {
		c.pushCancel()
	}
	for _, f := range c.pushCancels {
		f()
	}
}

// enqueue queues a frame for the writer goroutine; false if closing.
func (c *conn) enqueue(op byte, payload []byte) bool {
	select {
	case c.out <- frame{op: op, payload: payload}:
		return true
	case <-c.done:
		return false
	}
}

// sendClose queues a Close frame with code and drops the connection
// once the writer has flushed it.
func (c *conn) sendClose(code int, reason string) {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], reason)
	// Best effort: if the queue is stuck, just drop the connection.
	select {
	case c.out <- frame{op: opClose, payload: payload}:
	default:
		c.shutdown()
	}
}

func (c *conn) writer() {
	defer c.wg.Done()
	defer close(c.writerDone)
	for {
		select {
		case <-c.done:
			return
		case f := <-c.out:
			// Bound each frame write so a non-reading client cannot
			// wedge the writer (and, via a full out queue, every
			// enqueue caller) forever. On error/timeout shutdown
			// closes the connection, unblocking enqueue via done.
			_ = c.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := writeFrame(c.nc, f.op, f.payload); err != nil {
				c.shutdown()
				return
			}
			if f.op == opClose {
				c.shutdown()
				return
			}
		}
	}
}

// writeFrame writes one unmasked FIN frame (server->client).
func writeFrame(w io.Writer, op byte, payload []byte) error {
	var hdr [10]byte
	hdr[0] = 0x80 | op
	n := len(payload)
	var hlen int
	switch {
	case n < 126:
		hdr[1] = byte(n)
		hlen = 2
	case n <= 0xFFFF:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
		hlen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
		hlen = 10
	}
	if _, err := w.Write(hdr[:hlen]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

var errTooLarge = errors.New("ws: message too large")

// readFrame reads one frame. Client frames MUST be masked (RFC 6455
// §5.1); an unmasked client frame is a protocol error.
func (c *conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = hdr[0]&0x80 != 0
	op = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(b[:])
	}
	if length > maxMessageSize {
		// Consuming the payload is pointless; caller closes with 1009.
		return false, 0, nil, errTooLarge
	}
	if !masked {
		return false, 0, nil, errors.New("ws: unmasked client frame")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, op, payload, nil
}

// readLoop reads and handles messages until error or Close. It is the
// sole reader and sole mutator of push state.
func (c *conn) readLoop() {
	var msg []byte
	var msgOp byte
	fragmented := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			if errors.Is(err, errTooLarge) {
				c.sendClose(closeTooLarge, "message too large")
			} else if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				// Protocol errors (e.g. unmasked frame) get a 1002; a
				// vanished peer needs no reply — either way we exit.
				c.sendClose(closeProtocol, "")
			}
			// Give the writer a brief chance to flush the Close frame.
			c.flushClose()
			return
		}
		switch op {
		case opPing:
			c.enqueue(opPong, payload)
			continue
		case opPong:
			continue
		case opClose:
			// Reply with a Close frame (echoing payload) and exit.
			c.enqueue(opClose, payload)
			c.flushClose()
			return
		case opBinary:
			c.sendClose(closeUnsupported, "binary frames not supported")
			c.flushClose()
			return
		case opText:
			if fragmented {
				c.sendClose(closeProtocol, "new message before final fragment")
				c.flushClose()
				return
			}
			if fin {
				c.handleMessage(payload)
				continue
			}
			fragmented = true
			msgOp = opText
			msg = append(msg[:0], payload...)
		case opContinuation:
			if !fragmented {
				c.sendClose(closeProtocol, "unexpected continuation frame")
				c.flushClose()
				return
			}
			if len(msg)+len(payload) > maxMessageSize {
				c.sendClose(closeTooLarge, "message too large")
				c.flushClose()
				return
			}
			msg = append(msg, payload...)
			_ = msgOp
			if fin {
				fragmented = false
				c.handleMessage(msg)
				msg = nil
			}
		default:
			c.sendClose(closeProtocol, "unknown opcode")
			c.flushClose()
			return
		}
		if len(msg) > maxMessageSize {
			c.sendClose(closeTooLarge, "message too large")
			c.flushClose()
			return
		}
	}
}

// flushClose waits briefly for the writer to emit a queued Close frame
// (the writer drops the connection right after writing it).
func (c *conn) flushClose() {
	select {
	case <-c.writerDone:
	case <-time.After(500 * time.Millisecond):
	}
}

// --- RFC 8887 message handling ---

type wireRequest struct {
	Type string `json:"@type"`
	jmap.Request
}

type wireResponse struct {
	Type            string              `json:"@type"`
	MethodResponses []*jmap.Invocation  `json:"methodResponses"`
	CreatedIDs      map[jmap.ID]jmap.ID `json:"createdIds,omitempty"`
	SessionState    string              `json:"sessionState"`
}

type wireProblem struct {
	Type   string `json:"@type"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Error type URN when this mirrors an RFC 8620 request problem.
	ProblemType string `json:"type,omitempty"`
}

func (c *conn) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.enqueue(opText, data)
}

func (c *conn) sendProblem(status int, detail string) {
	c.sendJSON(wireProblem{Type: "Problem", Status: status, Detail: detail})
}

func (c *conn) handleMessage(data []byte) {
	var head struct {
		Type string `json:"@type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		c.sendProblem(http.StatusBadRequest, "message is not JSON")
		return
	}
	switch head.Type {
	case "Request":
		var wr wireRequest
		if err := json.Unmarshal(data, &wr); err != nil {
			c.sendProblem(http.StatusBadRequest, "invalid Request object")
			return
		}
		req := wr.Request
		resp := c.e.Process(c.ctx, c.p, &req)
		if resp == nil {
			// shortcut: request-level problems (unknown capability,
			// limits, not-a-request) collapse to a generic Problem
			// over WebSocket rather than echoing the exact RFC 8620
			// §3.6.1 problem type URN.
			c.sendProblem(http.StatusBadRequest, "invalid JMAP request")
			return
		}
		c.sendJSON(wireResponse{
			Type:            "Response",
			MethodResponses: resp.MethodResponses,
			CreatedIDs:      resp.CreatedIDs,
			SessionState:    resp.SessionState,
		})
	case "WebSocketPushSubscription":
		// shortcut: RFC 8887 §4 push enablement — we accept and ignore
		// all fields of the WebSocketPushSubscription object and treat
		// it as "enable StateChange pushes for every account of the
		// principal on this socket". Pushes are OFF until this message
		// arrives. (Per-account/type filtering fields, if any were
		// sent, are not honoured.)
		c.enablePush()
	default:
		// shortcut: unknown @type gets a simple Problem object rather
		// than a fully-typed RFC 8887 problem detail.
		c.sendProblem(http.StatusBadRequest, fmt.Sprintf("unknown @type %q", head.Type))
	}
}

// enablePush subscribes to StateChanges for every account of the
// principal and forwards them to this socket. Idempotent.
func (c *conn) enablePush() {
	if c.pushCtx != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.pushCtx = ctx
	c.pushCancel = cancel
	for accountID := range c.p.Accounts {
		ch, unsub := c.e.SubscribeStateChanges(ctx, accountID)
		c.pushCancels = append(c.pushCancels, unsub)
		c.wg.Add(1)
		go func(ch <-chan server.StateChange) {
			defer c.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-c.done:
					return
				case sc, ok := <-ch:
					if !ok {
						return
					}
					data, err := json.Marshal(sc) // {"@type":"StateChange","changed":{...}}
					if err != nil {
						continue
					}
					if !c.enqueue(opText, data) {
						return
					}
				}
			}
		}(ch)
	}
}
