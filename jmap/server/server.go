// Package server implements the HTTP engine and backend contracts of
// the JMAP library (RFC 8620): session/discovery, the API endpoint with
// result-reference resolution and createdIds bookkeeping, blob
// upload/download/copy, EventSource push, and PushSubscription delivery.
//
// The engine is storage- and credential-agnostic: a host implements the
// small backend interfaces below and registers method handlers for the
// data types it supports. Only capabilities registered with the engine
// are advertised in the Session object.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// ErrNotFound is returned by backend stores when the requested object
// (blob, subscription, ...) does not exist for the account.
var ErrNotFound = errors.New("jmap/server: not found")

// Principal is the authenticated identity a session acts for.
type Principal struct {
	Username        string
	Accounts        map[jmap.ID]*jmap.Account
	PrimaryAccounts map[jmap.URI]jmap.ID
}

// Authenticator decides who an HTTP request is. It is credential-form
// agnostic: it may inspect Authorization headers/cookies itself, or
// accept a principal pre-authenticated by host middleware (e.g. attached
// to the request context). Returning an error makes the engine reply
// 401 to every JMAP resource.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*Principal, error)
}

// BlobStore is the optional backend capability behind the upload and
// download endpoints and Blob/copy (RFC 8620 §6).
type BlobStore interface {
	Upload(ctx context.Context, accountID jmap.ID, contentType string, data []byte) (blobID jmap.ID, err error)
	Download(ctx context.Context, accountID, blobID jmap.ID) (contentType string, data []byte, err error)
	Copy(ctx context.Context, fromAccount, toAccount, blobID jmap.ID) (newID jmap.ID, err error)
}

// PushSubscription is a RFC 8620 §7.2 push subscription object. The RFC
// defines exactly the fields id, deviceClientId, url, keys, and expires;
// there is no topics field in RFC 8620 (delivery filtering is by the
// content of the StateChange payload).
type PushSubscription struct {
	ID             jmap.ID    `json:"id,omitempty"`
	DeviceClientID string     `json:"deviceClientId"`
	URL            string     `json:"url"`
	Keys           *PushKeys  `json:"keys,omitempty"`
	Expires        *time.Time `json:"expires,omitempty"`
}

// PushKeys holds the encryption keys of a PushSubscription (§7.2).
type PushKeys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// SubscriptionStore is the optional persistence for PushSubscriptions.
type SubscriptionStore interface {
	List(ctx context.Context, accountID jmap.ID) ([]*PushSubscription, error)
	Upsert(ctx context.Context, accountID jmap.ID, sub *PushSubscription) error
	Delete(ctx context.Context, accountID jmap.ID, id jmap.ID) error
}

// StateChange is a RFC 8620 §7.1 state-change notification. Changed maps
// accountId -> data-type name -> new state string, exactly as in the
// JSON form {"@type":"StateChange","changed":{...}}. AccountID is a
// convenience for the common single-account case (used by Publish to
// find subscriptions and EventSource subscribers); when Changed is nil,
// Publish/EventSource synthesise Changed from AccountID and the Changed
// handling in stateChangeChanged below.
type StateChange struct {
	AccountID jmap.ID
	Changed   map[string]map[string]string
}

// changed returns the effective RFC "changed" object.
func (sc StateChange) changed() map[string]map[string]string {
	if len(sc.Changed) > 0 {
		return sc.Changed
	}
	if sc.AccountID != "" {
		return map[string]map[string]string{string(sc.AccountID): {}}
	}
	return map[string]map[string]string{}
}

// MarshalJSON encodes {"@type":"StateChange","changed":{...}}.
func (sc StateChange) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type    string                       `json:"@type"`
		Changed map[string]map[string]string `json:"changed"`
	}{"StateChange", sc.changed()})
}

// UnmarshalJSON decodes the RFC form, deriving AccountID when exactly
// one account is present.
func (sc *StateChange) UnmarshalJSON(data []byte) error {
	var v struct {
		Changed map[string]map[string]string `json:"changed"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	sc.Changed = v.Changed
	if len(v.Changed) == 1 {
		for k := range v.Changed {
			sc.AccountID = jmap.ID(k)
		}
	}
	return nil
}

// StateChangeSource is the optional backend push source: it emits state
// changes for an account until ctx is cancelled.
type StateChangeSource interface {
	Subscribe(ctx context.Context, accountID jmap.ID) (<-chan StateChange, error)
}

// Options configures an Engine. Zero values take the documented
// defaults. Paths are Go http.ServeMux patterns (hence the {accountId}
// placeholders, which double as the RFC URL templates advertised in the
// Session object).
type Options struct {
	SessionPath     string // default "/jmap/session"
	APIPath         string // default "/jmap/"
	UploadPath      string // default "/jmap/upload/{accountId}"
	DownloadPath    string // default "/jmap/download/{accountId}/{blobId}/{name}"
	EventSourcePath string // default "/jmap/eventsource/"
	// WebSocketPath is only used to build Session.WebSocketURL when
	// the host registers the WebSocket capability and mounts a
	// WebSocket handler (e.g. jmap/ws) at this path.
	WebSocketPath string // default "/jmap/ws/"
	// BaseURL is prepended to the paths to form the absolute URLs in
	// the Session object (no trailing slash). Empty means relative.
	BaseURL string
	// Core overrides the advertised/enforced core capability limits;
	// zero fields take defaults.
	Core jmap.CoreCapability
	// SessionState is echoed as Session.State / Response.SessionState.
	// shortcut: a constant "0" by default; backends whose session shape
	// changes should supply a changing value via later phases.
	SessionState string

	Blobs         BlobStore         // optional; enables upload/download/Blob/copy
	Subscriptions SubscriptionStore // optional; enables PushSubscription/get|set and Publish delivery
	StateChanges  StateChangeSource // optional; merged with the internal Publish bus for EventSource

	// HTTPClient delivers push verification/state-change POSTs.
	// Default: a client with a 5s timeout.
	HTTPClient *http.Client
	// OnPushError, if set, receives the final error of a failed push
	// delivery (after retries). Library code otherwise stays silent.
	OnPushError func(sub *PushSubscription, err error)
}

// Call is one method invocation as handed to a MethodHandler. Args has
// already had result references resolved by the engine.
type Call struct {
	Name      string
	CallID    string
	Args      json.RawMessage
	Principal *Principal
	// Created accumulates creationId -> server id mappings made by the
	// handler (via SetCreated). The engine merges it into the response
	// createdIds and the request-level map for later result references.
	Created map[jmap.ID]jmap.ID
}

// Decode unmarshals the (resolved) arguments into v.
func (c *Call) Decode(v any) error { return json.Unmarshal(c.Args, v) }

// SetCreated records that client creation id cid now refers to serverID.
func (c *Call) SetCreated(cid, serverID jmap.ID) {
	if c.Created == nil {
		c.Created = make(map[jmap.ID]jmap.ID)
	}
	c.Created[cid] = serverID
}

// AccountID resolves the call's "accountId" argument against the
// principal: present-but-unknown yields accountNotFound, missing yields
// invalidArguments. Methods without an accountId argument (e.g.
// PushSubscription/*) should use OptionalAccountID.
func (c *Call) AccountID() (jmap.ID, error) {
	var a struct {
		AccountID *jmap.ID `json:"accountId"`
	}
	if err := c.Decode(&a); err != nil {
		return "", jmap.NewInvalidArguments(err.Error())
	}
	if a.AccountID == nil || *a.AccountID == "" {
		return "", jmap.NewInvalidArguments("missing accountId")
	}
	if _, ok := c.Principal.Accounts[*a.AccountID]; !ok {
		return "", jmap.NewAccountNotFound(string(*a.AccountID))
	}
	return *a.AccountID, nil
}

// OptionalAccountID is AccountID for methods whose accountId is absent
// or optional: a present id is validated as in AccountID; an absent id
// resolves to the principal's sole account, else its core primary
// account, else invalidArguments.
func (c *Call) OptionalAccountID() (jmap.ID, error) {
	var a struct {
		AccountID *jmap.ID `json:"accountId"`
	}
	if err := c.Decode(&a); err != nil {
		return "", jmap.NewInvalidArguments(err.Error())
	}
	if a.AccountID != nil && *a.AccountID != "" {
		if _, ok := c.Principal.Accounts[*a.AccountID]; !ok {
			return "", jmap.NewAccountNotFound(string(*a.AccountID))
		}
		return *a.AccountID, nil
	}
	if len(c.Principal.Accounts) == 1 {
		for id := range c.Principal.Accounts {
			return id, nil
		}
	}
	if id, ok := c.Principal.PrimaryAccounts[jmap.CoreURI]; ok {
		if _, ok := c.Principal.Accounts[id]; ok {
			return id, nil
		}
	}
	return "", jmap.NewInvalidArguments("missing accountId")
}

// MethodHandler handles one registered method. It returns the response
// arguments value (marshalled by the engine; json.RawMessage is used
// verbatim) or an error. A *jmap.MethodError is sent as-is; any other
// error becomes a serverFail method error.
type MethodHandler func(ctx context.Context, call *Call) (any, error)

type methodEntry struct {
	capability jmap.URI
	handler    MethodHandler
}

type capEntry struct {
	session, account any
}

// Engine is the JMAP HTTP engine. It is safe for concurrent use.
type Engine struct {
	auth Authenticator
	opts Options
	core jmap.CoreCapability

	mu      sync.RWMutex
	methods map[string]methodEntry
	caps    map[jmap.URI]capEntry

	busMu sync.Mutex
	bus   map[chan StateChange]map[jmap.ID]bool
}

// New builds an Engine with the built-in methods registered: Core/echo
// always, Blob/copy when Options.Blobs is set, and
// PushSubscription/get|set when Options.Subscriptions is set.
func New(auth Authenticator, opts Options) *Engine {
	if opts.SessionPath == "" {
		opts.SessionPath = "/jmap/session"
	}
	if opts.APIPath == "" {
		opts.APIPath = "/jmap/"
	}
	if opts.UploadPath == "" {
		opts.UploadPath = "/jmap/upload/{accountId}"
	}
	if opts.DownloadPath == "" {
		opts.DownloadPath = "/jmap/download/{accountId}/{blobId}/{name}"
	}
	if opts.EventSourcePath == "" {
		opts.EventSourcePath = "/jmap/eventsource/"
	}
	if opts.WebSocketPath == "" {
		opts.WebSocketPath = "/jmap/ws/"
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	if opts.SessionState == "" {
		opts.SessionState = "0" // shortcut: constant session state
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	core := opts.Core
	if core.MaxSizeUpload == 0 {
		core.MaxSizeUpload = 10 << 20
	}
	if core.MaxConcurrentUpload == 0 {
		core.MaxConcurrentUpload = 4
	}
	if core.MaxSizeRequest == 0 {
		core.MaxSizeRequest = 10 << 20
	}
	if core.MaxConcurrentRequests == 0 {
		core.MaxConcurrentRequests = 10
	}
	if core.MaxCallsInRequest == 0 {
		core.MaxCallsInRequest = 50
	}
	if core.MaxObjectsInGet == 0 {
		core.MaxObjectsInGet = 500
	}
	if core.MaxObjectsInSet == 0 {
		core.MaxObjectsInSet = 500
	}
	if core.CollationAlgorithms == nil {
		core.CollationAlgorithms = []string{"i;ascii-numeric"}
	}

	e := &Engine{
		auth: auth, opts: opts, core: core,
		methods: make(map[string]methodEntry),
		caps:    make(map[jmap.URI]capEntry),
		bus:     make(map[chan StateChange]map[jmap.ID]bool),
	}
	e.RegisterCapability(jmap.CoreURI, core, map[string]any{})
	e.RegisterMethod("Core/echo", jmap.CoreURI, func(_ context.Context, call *Call) (any, error) {
		// RFC 8620 §4: Core/echo returns its arguments unchanged.
		return call.Args, nil
	})
	if opts.Blobs != nil {
		e.RegisterMethod("Blob/copy", jmap.CoreURI, e.blobCopy)
	}
	if opts.Subscriptions != nil {
		e.RegisterMethod("PushSubscription/get", jmap.CoreURI, e.pushSubscriptionGet)
		e.RegisterMethod("PushSubscription/set", jmap.CoreURI, e.pushSubscriptionSet)
	}
	return e
}

// RegisterCapability advertises uri in the Session object.
// sessionCap is the server-level capability object; accountCap, if
// non-nil, is the per-account object placed in every account's
// accountCapabilities. Values are marshalled to JSON at session time.
func (e *Engine) RegisterCapability(uri jmap.URI, sessionCap, accountCap any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.caps[uri] = capEntry{session: sessionCap, account: accountCap}
}

// RegisterMethod registers h for name. capability is the capability URI
// the method requires in the request's "using" list ("" = none).
func (e *Engine) RegisterMethod(name string, capability jmap.URI, h MethodHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.methods[name] = methodEntry{capability: capability, handler: h}
}

func (e *Engine) knownCapability(uri jmap.URI) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.caps[uri]
	return ok
}

// Handler returns the engine's HTTP handler (discovery, session, API,
// upload, download, EventSource).
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	// RFC 8620 §2.2: the session resource IS /.well-known/jmap ("The
	// JMAP Session resource is then https://host/.well-known/jmap
	// (following any redirects)"). Serve the session object directly
	// instead of redirecting: some clients (himalaya) treat the
	// discovery GET as non-redirectable.
	mux.HandleFunc("GET /.well-known/jmap", e.serveSession)
	mux.HandleFunc("GET "+e.opts.SessionPath, e.serveSession)
	mux.HandleFunc("POST "+e.opts.APIPath, e.serveAPI)
	mux.HandleFunc("POST "+e.opts.UploadPath, e.serveUpload)
	mux.HandleFunc("GET "+e.opts.DownloadPath, e.serveDownload)
	mux.HandleFunc("GET "+e.opts.EventSourcePath, e.serveEventSource)
	return mux
}

// authenticate runs the Authenticator, writing a 401 and returning nil
// on failure.
func (e *Engine) authenticate(w http.ResponseWriter, r *http.Request) *Principal {
	if e.auth == nil {
		writeProblem(w, &jmap.Problem{Type: "about:blank", Status: http.StatusUnauthorized, Detail: "no authenticator configured"})
		return nil
	}
	p, err := e.AuthenticateRequest(r.Context(), r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, &jmap.Problem{Type: "about:blank", Status: http.StatusUnauthorized, Detail: "unauthorized"})
		return nil
	}
	return p
}

func clonePrincipal(p *Principal) *Principal {
	cp := *p
	return &cp
}

// wsBase converts an http(s) BaseURL to its ws(s) equivalent for
// the Session webSocketUrl; a relative BaseURL stays relative.
func wsBase(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return base
	}
}

// buildSession renders the Session object for p from the registered
// capabilities only.
func (e *Engine) buildSession(p *Principal) (*jmap.Session, error) {
	e.mu.RLock()
	caps := make(map[jmap.URI]capEntry, len(e.caps))
	for k, v := range e.caps {
		caps[k] = v
	}
	e.mu.RUnlock()

	s := &jmap.Session{
		Capabilities:    make(map[jmap.URI]json.RawMessage, len(caps)),
		Accounts:        make(map[jmap.ID]*jmap.Account, len(p.Accounts)),
		PrimaryAccounts: p.PrimaryAccounts,
		Username:        p.Username,
		APIURL:          e.opts.BaseURL + e.opts.APIPath,
		DownloadURL:     e.opts.BaseURL + e.opts.DownloadPath,
		UploadURL:       e.opts.BaseURL + e.opts.UploadPath,
		EventSourceURL:  e.eventSourceURL(),
		State:           e.opts.SessionState,
	}
	if _, ok := caps[jmap.WebSocketURI]; ok {
		// RFC 8887: advertise the WebSocket endpoint when the host
		// registered the capability (and mounts the handler).
		s.WebSocketURL = wsBase(e.opts.BaseURL) + e.opts.WebSocketPath
	}
	if s.PrimaryAccounts == nil {
		s.PrimaryAccounts = map[jmap.URI]jmap.ID{}
	}
	for uri, c := range caps {
		raw, err := marshalCap(c.session)
		if err != nil {
			return nil, fmt.Errorf("jmap/server: capability %s: %w", uri, err)
		}
		if raw != nil {
			s.Capabilities[uri] = raw
		}
	}
	for id, acct := range p.Accounts {
		cp := &jmap.Account{}
		if acct != nil {
			*cp = *acct
		}
		// Registered account capabilities win over whatever the
		// principal carried, so the session never advertises a
		// capability the engine does not know.
		ac := make(map[jmap.URI]json.RawMessage)
		for uri, c := range caps {
			if c.account == nil {
				continue
			}
			raw, err := marshalCap(c.account)
			if err != nil {
				return nil, fmt.Errorf("jmap/server: account capability %s: %w", uri, err)
			}
			if raw != nil {
				ac[uri] = raw
			}
		}
		if len(ac) > 0 {
			cp.AccountCapabilities = ac
		} else if cp.AccountCapabilities == nil {
			cp.AccountCapabilities = map[jmap.URI]json.RawMessage{}
		}
		s.Accounts[id] = cp
	}
	return s, nil
}

func marshalCap(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(v)
}

func (e *Engine) eventSourceURL() string {
	u := e.opts.BaseURL + e.opts.EventSourcePath
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	// RFC 8620 §2 template variables for the EventSource URL.
	return u + sep + "types={types}&closeafter={closeafter}&ping={ping}"
}

func (e *Engine) serveSession(w http.ResponseWriter, r *http.Request) {
	p := e.authenticate(w, r)
	if p == nil {
		return
	}
	s, err := e.buildSession(p)
	if err != nil {
		writeProblem(w, &jmap.Problem{Type: "about:blank", Status: http.StatusInternalServerError, Detail: err.Error()})
		return
	}
	// RFC 8620 §2: the session URLs are absolute. When no BaseURL is
	// configured, absolutize them from the request's Host so clients
	// discovering the server via /.well-known/jmap on any host:port
	// get usable URLs.
	if e.opts.BaseURL == "" {
		host := r.Host
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base := scheme + "://" + host
		s.APIURL = base + s.APIURL
		s.DownloadURL = base + s.DownloadURL
		s.UploadURL = base + s.UploadURL
		s.EventSourceURL = base + s.EventSourceURL
		if s.WebSocketURL != "" {
			s.WebSocketURL = wsBase(base) + s.WebSocketURL
		}
	}
	// A host may mount WebSocket without knowing its public address until
	// discovery. Fill an unset RFC 8887 capability URL from this connection,
	// preserving explicit host configuration and other capability fields.
	if raw, ok := s.Capabilities[jmap.WebSocketURI]; ok && s.WebSocketURL != "" {
		var capability map[string]json.RawMessage
		if json.Unmarshal(raw, &capability) == nil && capability != nil {
			var endpoint string
			_ = json.Unmarshal(capability["url"], &endpoint)
			if endpoint == "" {
				capability["url"], _ = json.Marshal(s.WebSocketURL)
				s.Capabilities[jmap.WebSocketURI], _ = json.Marshal(capability)
			}
		}
	}
	writeJSON(w, http.StatusOK, s)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, p *jmap.Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
