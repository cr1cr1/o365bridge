package server

import (
	"context"
	"errors"
	"net/http"
	"sync"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// AuthenticateRequest performs credential authentication for r without
// writing any response. It returns (nil, error) on failure, including
// when no Authenticator is configured. It is the non-writing counterpart
// of the internal authenticate used by the HTTP handlers, exported so
// other transports (e.g. WebSocket, RFC 8887) can reuse the same
// credentials check before upgrading.
func (e *Engine) AuthenticateRequest(ctx context.Context, r *http.Request) (*Principal, error) {
	if e.auth == nil {
		return nil, errors.New("jmap/server: no authenticator configured")
	}
	if ctx == nil {
		ctx = r.Context()
	}
	p, err := e.auth.Authenticate(ctx, r)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("jmap/server: unauthorized")
	}
	if p.Accounts == nil {
		p = clonePrincipal(p)
		p.Accounts = map[jmap.ID]*jmap.Account{}
	}
	return p, nil
}

// Process runs the dispatch logic of the HTTP API endpoint for an
// already-authenticated principal: request-level capability and limit
// checks, sequential dispatch with result-reference resolution, and
// createdIds bookkeeping. The returned Response echoes the engine's
// session state.
//
// Process returns nil if the request has a request-level problem
// (not a request object, unknown capability, or too many calls); HTTP
// transports render that as an RFC 8620 §3.6.1 Problem, other transports
// as their own error object.
func (e *Engine) Process(ctx context.Context, p *Principal, req *jmap.Request) *jmap.Response {
	resp, _ := e.process(ctx, p, req)
	return resp
}

// process is Process plus the request-level problem detail the HTTP
// handler needs.
func (e *Engine) process(ctx context.Context, p *Principal, req *jmap.Request) (*jmap.Response, *jmap.Problem) {
	if p == nil || req == nil || req.Using == nil || req.MethodCalls == nil {
		return nil, &jmap.Problem{Type: jmap.ProblemNotRequest, Status: http.StatusBadRequest, Detail: "request body is not a JMAP request object"}
	}
	for _, u := range req.Using {
		if !e.knownCapability(u) {
			return nil, &jmap.Problem{Type: jmap.ProblemUnknownCapability, Status: http.StatusBadRequest, Detail: "unknown capability: " + string(u)}
		}
	}
	if uint64(len(req.MethodCalls)) > e.core.MaxCallsInRequest {
		return nil, &jmap.Problem{Type: jmap.ProblemLimit, Status: http.StatusBadRequest, Limit: "maxCallsInRequest"}
	}

	using := make(map[jmap.URI]bool, len(req.Using))
	for _, u := range req.Using {
		using[u] = true
	}
	created := make(map[jmap.ID]jmap.ID, len(req.CreatedIDs))
	for k, v := range req.CreatedIDs {
		created[k] = v
	}
	haveCreated := len(created) > 0

	resp := &jmap.Response{MethodResponses: []*jmap.Invocation{}, SessionState: e.opts.SessionState}
	prev := make(map[string]*jmap.Invocation) // callID -> response invocation

	for _, inv := range req.MethodCalls {
		out := e.dispatchOne(ctx, p, inv, using, prev, created)
		resp.MethodResponses = append(resp.MethodResponses, out)
		prev[inv.CallID] = out
	}
	if len(created) > 0 || haveCreated {
		resp.CreatedIDs = created
	}
	return resp, nil
}

// SubscribeStateChanges subscribes to the engine's internal Publish bus
// (the same bus EventSource uses) for a single account. If accountID is
// empty, changes for all accounts are delivered. The returned cancel
// function unsubscribes and is safe to call more than once; cancelling
// ctx also unsubscribes. Delivery is buffered but non-blocking on the
// publisher: a slow subscriber may drop changes (same semantics as the
// bus itself).
func (e *Engine) SubscribeStateChanges(ctx context.Context, accountID jmap.ID) (<-chan StateChange, func()) {
	var accounts map[jmap.ID]bool
	if accountID != "" {
		accounts = map[jmap.ID]bool{accountID: true}
	}
	ch, unsub := e.subscribeBus(accounts)
	var once sync.Once
	cancel := func() { once.Do(unsub) }
	if ctx != nil {
		go func() {
			<-ctx.Done()
			cancel()
		}()
	}
	return ch, cancel
}
