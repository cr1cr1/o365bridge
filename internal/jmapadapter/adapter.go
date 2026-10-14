// Package jmapadapter implements the JMAP mail Backend contracts
// (library in ./jmap) over bridge.Registry / bridge.Provider.
// It is the only main-module package that imports the library.
package jmapadapter

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/mail"
	"github.com/cr1cr1/o365bridge/jmap/server"
	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// Backend implements mail.Backend plus the optional Identity,
// Submission, Snippet, Import, Copy and Parse backends.
// Vacation is NOT implemented (no honest upstream source).
type Backend struct {
	Reg     *bridge.Registry
	Journal *Journal
	Logger  zerolog.Logger

	blobs  *blobStore
	engine *server.Engine

	mu         sync.Mutex
	subs       map[string]map[jmap.ID]*mail.EmailSubmission
	subSeq     map[string]int
	subVersion map[string]int
}

// NewEngine builds the authenticator, blob store and engine, registers
// mail methods, and wires journal push. Returns engine and backend.
func NewEngine(reg *bridge.Registry, instancePassword string, logger zerolog.Logger) (*server.Engine, *Backend) {
	b := &Backend{
		Reg: reg, Logger: logger.With().Str("protocol", "jmap").Logger(),
		subs: map[string]map[jmap.ID]*mail.EmailSubmission{}, subSeq: map[string]int{}, subVersion: map[string]int{},
	}
	b.Journal = NewJournal(reg, b.Logger)
	b.blobs = newBlobStore(b)
	auth := &authenticator{reg: reg, password: instancePassword, logger: b.Logger, backend: b}
	eng := server.New(auth, server.Options{Blobs: b.blobs})
	b.SetEngine(eng)
	mail.Register(eng, b)
	return eng, b
}

// SetEngine records the engine for Publish and wires the journal publisher.
func (b *Backend) SetEngine(e *server.Engine) {
	b.engine = e
	if b.Journal != nil {
		b.Journal.SetPublisher(func(account, state string) {
			if b.engine == nil {
				return
			}
			b.engine.Publish(server.StateChange{
				AccountID: jmap.ID(account),
				Changed:   map[string]map[string]string{account: {"Email": state}},
			})
		})
	}
}

// Handler is a convenience for the engine's HTTP handler.
func (b *Backend) Handler() http.Handler {
	if b.engine == nil {
		return nil
	}
	return b.engine.Handler()
}

func (b *Backend) Mailboxes() datatype.Store[mail.Mailbox]           { return &mailboxStore{b} }
func (b *Backend) Emails() datatype.Store[mail.Email]                { return &emailStore{b} }
func (b *Backend) Identities() datatype.Store[mail.Identity]         { return &identityStore{b} }
func (b *Backend) Submissions() datatype.Store[mail.EmailSubmission] { return &submissionStore{b} }
func (b *Backend) SnippetSource() mail.SnippetSource                 { return b }
func (b *Backend) EmailImporter() mail.Importer                      { return b }
func (b *Backend) EmailCopier() mail.Copier                          { return b }
func (b *Backend) EmailParser() mail.Parser                          { return b }

// call runs fn via Registry.Call with per-account logging (never bodies/credentials).
func (b *Backend) call(ctx context.Context, account, op string, fn func(context.Context, bridge.Provider) error) error {
	err := b.Reg.Call(ctx, account, fn)
	if err != nil {
		b.Logger.Warn().Str("account", account).Str("op", op).Err(err).Msg("jmap upstream failure")
	} else {
		b.Logger.Info().Str("account", account).Str("op", op).Msg("jmap operation")
	}
	return err
}

// --- Authenticator ---

type authenticator struct {
	reg      *bridge.Registry
	password string
	logger   zerolog.Logger
	backend  *Backend
}

func (a *authenticator) Authenticate(ctx context.Context, r *http.Request) (*server.Principal, error) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return nil, errors.New("missing basic auth")
	}
	email := bridge.NormalizeEmail(user)
	if !strings.Contains(email, "@") {
		a.logger.Warn().Str("account", email).Msg("jmap auth failure: bad username")
		return nil, errors.New("bad username")
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) != 1 {
		a.logger.Warn().Str("account", email).Msg("jmap auth failure")
		return nil, errors.New("bad password")
	}
	// Trigger hold-and-wait login.
	if _, err := a.reg.Provider(ctx, email); err != nil {
		a.logger.Warn().Str("account", email).Err(err).Msg("jmap auth failure: provider")
		return nil, err
	}
	mailCap, _ := json.Marshal(map[string]any{})
	return &server.Principal{
		Username: email,
		Accounts: map[jmap.ID]*jmap.Account{
			jmap.ID(email): {Name: email, IsPersonal: true, AccountCapabilities: map[jmap.URI]json.RawMessage{jmap.MailURI: mailCap}},
		},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.MailURI: jmap.ID(email)},
	}, nil
}

// --- IDs ---

func encodeEmailID(folderID, upstreamID string) jmap.ID {
	return jmap.ID(base64.RawURLEncoding.EncodeToString([]byte(folderID + "\x00" + upstreamID)))
}

func decodeEmailID(id jmap.ID) (folderID, upstreamID string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(string(id))
	if err != nil {
		return "", "", err
	}
	f, u, ok := strings.Cut(string(raw), "\x00")
	if !ok || f == "" || u == "" {
		return "", "", fmt.Errorf("invalid email id")
	}
	return f, u, nil
}

func partBlobID(emailID jmap.ID, partID string) jmap.ID {
	return jmap.ID("p." + string(emailID) + "." + partID)
}

// --- flag/keyword mapping ---

func flagsToKeywords(f bridge.Flags) map[string]bool {
	out := map[string]bool{}
	for _, fl := range f {
		low := strings.ToLower(fl)
		switch low {
		case `\seen`, `\flagged`, `\answered`, `\draft`:
			out["$"+low[1:]] = true
		default:
			// unknown flags pass through as-is lowercased keyword
			out[low] = true
		}
	}
	return out
}

func keywordToFlag(k string) string {
	low := strings.ToLower(k)
	switch low {
	case "$seen", "$flagged", "$answered", "$draft":
		return `\` + low[1:]
	default:
		return low
	}
}

func keywordsToFlags(kw map[string]bool) bridge.Flags {
	var out bridge.Flags
	for k, v := range kw {
		if v {
			out = append(out, keywordToFlag(k))
		}
	}
	return out
}

func strPtr(s string) *string { return &s }
