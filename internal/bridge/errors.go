// Package bridge is the deep module at the centre of o365bridge: the
// domain types every downstream protocol and upstream provider share,
// the provider contract, per-account session state, and the registry
// that owns account discovery, channel probing and auth fallback.
//
// Exported surface is the contract; everything else in this module
// may be refactored as long as the acceptance tests pass.
package bridge

import (
	"errors"
	"fmt"
)

// ErrUnknownAccount is returned when no state exists for an email.
var ErrUnknownAccount = errors.New("bridge: unknown account")

// Kind classifies upstream failures. The classification is load
// bearing: only non-transient auth errors may trigger channel
// fallback or interactive re-login; transient errors never do.
type Kind int

const (
	KindOther Kind = iota
	// KindTransient covers network, DNS, timeouts, throttling (429)
	// and upstream 5xx. Retry with backoff; never fall back.
	KindTransient
	// KindAuth covers non-transient client/auth failures: consent
	// denied or revoked, invalid client/scope/grant, tenant-disabled
	// API, persistent 401/403 on a fresh token, dead OWA session.
	KindAuth
)

// Error is an upstream failure carrying a Kind.
type Error struct {
	Kind Kind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// TransientError builds a KindTransient error.
func TransientError(msg string) error { return &Error{Kind: KindTransient, Msg: msg} }

// AuthError builds a KindAuth error.
func AuthError(msg string) error { return &Error{Kind: KindAuth, Msg: msg} }

// Wrap classifies err under kind, keeping it unwrappable.
func Wrap(kind Kind, msg string, err error) error {
	return &Error{Kind: kind, Msg: msg, Err: err}
}

// KindOf extracts the Kind of err, KindOther if unclassified.
func KindOf(err error) Kind {
	var be *Error
	if errors.As(err, &be) {
		return be.Kind
	}
	return KindOther
}

// IsAuth reports whether err is a non-transient auth failure.
func IsAuth(err error) bool { return KindOf(err) == KindAuth }

// IsTransient reports whether err is transient.
func IsTransient(err error) bool { return KindOf(err) == KindTransient }

// ClassifyHTTP maps an upstream HTTP status (and Graph-style error
// code, may be "") onto a Kind.
func ClassifyHTTP(status int, _ string) Kind {
	switch {
	case status == 401 || status == 403:
		return KindAuth
	case status == 429 || status >= 500:
		return KindTransient
	case status >= 400:
		// Non-transient client errors (bad scope, consent, tenant
		// policy) are auth-class for fallback purposes.
		return KindAuth
	default:
		return KindOther
	}
}
