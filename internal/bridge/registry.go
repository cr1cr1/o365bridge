package bridge

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Config wires the registry. GraphClientID empty disables Graph:
// accounts then bind to OWA directly.
type Config struct {
	StateDir      string
	GraphClientID string
	Login         LoginManager
	Factory       Factory
	Now           func() time.Time // nil = time.Now
}

// Registry owns dynamic account discovery: accounts are born when a
// client first presents an unknown email, logins are single-flight
// per account and held indefinitely, and non-transient auth failures
// fall back Graph -> OWA automatically.
type Registry struct {
	cfg   Config
	store *Store

	mu       sync.Mutex
	inflight map[string]*loginFlight

	// Sent-copy dedupe state (see sentcopy.go).
	sentMu sync.Mutex
	sent   map[string]time.Time
}

type loginFlight struct {
	done chan struct{}
	kind string // "ensure", "renew", "fallback" — flights are only
	// joinable by callers doing the same kind of work; other
	// callers wait for the flight to finish, then start their own.
	prov Provider
	err  error
}

// NewRegistry creates a registry over the session cache at
// cfg.StateDir.
func NewRegistry(cfg Config) (*Registry, error) {
	if cfg.Login == nil || cfg.Factory == nil {
		return nil, fmt.Errorf("bridge: registry needs a LoginManager and a Factory")
	}
	st, err := NewStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Registry{cfg: cfg, store: st, inflight: map[string]*loginFlight{}}, nil
}

// Store exposes the session cache (ID maps, per-account dirs).
func (r *Registry) Store() *Store { return r.store }

func (r *Registry) channels() []Channel {
	if r.cfg.GraphClientID != "" {
		return []Channel{ChannelGraph, ChannelOWA}
	}
	return []Channel{ChannelOWA}
}

// Account returns the public view of a discovered account.
func (r *Registry) Account(email string) (Account, error) {
	st, err := r.store.LoadAccount(email)
	if err != nil {
		return Account{}, err
	}
	return Account{Email: st.Email, Channel: st.Session.Channel}, nil
}

// flight runs fn as the single in-flight login/recovery of the
// given kind for an account, joins a running flight of the SAME
// kind, or waits out a flight of a different kind (its result is
// not interchangeable with ours). Waiters whose context ends
// simply stop waiting; the flight itself is undisturbed. The
// joined result reports whether the caller joined someone else's
// flight (vs. leading it).
func (r *Registry) flight(ctx context.Context, email, kind string, fn func(context.Context) (Provider, error)) (Provider, error) {
	prov, _, err := r.runFlight(ctx, email, kind, fn)
	return prov, err
}

func (r *Registry) runFlight(ctx context.Context, email, kind string, fn func(context.Context) (Provider, error)) (Provider, bool, error) {
	for {
		r.mu.Lock()
		f, ok := r.inflight[email]
		if ok && f.kind == kind {
			r.mu.Unlock()
			select {
			case <-f.done:
				return f.prov, true, f.err
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		}
		if ok {
			// Different kind of flight in progress: wait it out,
			// then loop to lead (or join) a flight of our kind.
			r.mu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		}
		f = &loginFlight{done: make(chan struct{}), kind: kind}
		r.inflight[email] = f
		r.mu.Unlock()

		f.prov, f.err = fn(ctx)
		close(f.done)

		r.mu.Lock()
		if r.inflight[email] == f {
			delete(r.inflight, email)
		}
		r.mu.Unlock()
		return f.prov, false, f.err
	}
}

// Provider returns the Provider bound to the account, discovering
// and logging in the account on first use. Concurrent calls for the
// same new account share exactly one login flight.
func (r *Registry) Provider(ctx context.Context, email string) (Provider, error) {
	email = NormalizeEmail(email)

	// Fast path: persisted, unexpired session — unless a login or
	// recovery for this account is in flight, in which case the
	// persisted session is the stale one being fixed.
	r.mu.Lock()
	_, flying := r.inflight[email]
	r.mu.Unlock()
	if !flying {
		if st, err := r.store.LoadAccount(email); err == nil && !st.Session.Expired(r.cfg.Now()) {
			return r.cfg.Factory.New(email, st.Session), nil
		}
	}
	// Loop: a caller may join a *recovery* flight (renewal/fallback
	// started by Registry.Call) whose result is not a login result
	// at all — e.g. a failed silent renewal. Joining such a failed
	// flight must not surface its error here; go round again and
	// either take the fast path, join the next flight, or lead the
	// login ourselves.
	for {
		prov, joined, err := r.runFlight(ctx, email, "ensure", func(ctx context.Context) (Provider, error) {
			return r.ensure(ctx, email)
		})
		if err == nil || !joined {
			return prov, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// ensure performs (or renews) the login for an account and persists
// the resulting session. Called under a single flight.
func (r *Registry) ensure(ctx context.Context, email string) (Provider, error) {
	if st, err := r.store.LoadAccount(email); err == nil {
		// A concurrent flight may have just refreshed this session
		// while we waited to lead: an unexpired session needs no
		// renewal at all.
		if !st.Session.Expired(r.cfg.Now()) {
			return r.cfg.Factory.New(email, st.Session), nil
		}
		// Known account with an expired session: try silent renewal
		// of the same channel.
		if sess, rerr := r.cfg.Login.Renew(ctx, email, st.Session); rerr == nil {
			st.Session = sess
			if err := r.store.SaveAccount(st); err != nil {
				return nil, err
			}
			return r.cfg.Factory.New(email, sess), nil
		} else if !IsAuth(rerr) {
			return nil, rerr
		}
		// Silent renewal impossible: interactive re-login on the
		// same channel, falling back Graph -> OWA on auth failure.
		return r.loginSequence(ctx, email, channelsFrom(st.Session.Channel, r.channels()))
	}

	// New account: probe channels in order.
	log.Info().Str("account", email).Msg("new account discovered, starting login")
	return r.loginSequence(ctx, email, r.channels())
}

func channelsFrom(start Channel, all []Channel) []Channel {
	for i, ch := range all {
		if ch == start {
			return all[i:]
		}
	}
	return []Channel{start}
}

// loginSequence tries each channel: interactive login, then (Graph
// only) a mail proof ping. Non-transient auth failures advance to
// the next channel; transient failures abort without persisting.
func (r *Registry) loginSequence(ctx context.Context, email string, channels []Channel) (Provider, error) {
	var lastErr error
	for _, ch := range channels {
		sess, err := r.cfg.Login.Login(ctx, email, ch)
		if err != nil {
			if IsAuth(err) {
				lastErr = err
				continue
			}
			return nil, err
		}
		prov := r.cfg.Factory.New(email, sess)
		if ch == ChannelGraph {
			if err := prov.Ping(ctx); err != nil {
				if IsAuth(err) {
					log.Info().Str("account", email).Msg("graph not usable for account, falling back to owa")
					lastErr = err
					continue
				}
				return nil, err
			}
		}
		sess.Email = email
		sess.Channel = ch
		if err := r.store.SaveAccount(AccountState{Email: email, Session: sess}); err != nil {
			return nil, err
		}
		log.Info().Str("account", email).Str("channel", string(ch)).Msg("account bound to channel")
		return prov, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("bridge: no channel available for %s", email)
}

// Call runs op against the account's provider. On a non-transient
// auth failure it first renews silently and retries; if renewal
// fails (or the retry still fails auth), a Graph account falls
// back to OWA automatically, while an OWA account re-logs-in
// interactively. The client operation is held throughout; it only
// fails on an actual upstream error or user cancellation.
func (r *Registry) Call(ctx context.Context, email string, op func(context.Context, Provider) error) error {
	email = NormalizeEmail(email)
	prov, err := r.Provider(ctx, email)
	if err != nil {
		return err
	}
	// Remember which session the failing operation runs against,
	// so recovery can tell "someone already replaced this session"
	// apart from "this session is still the dead one".
	base, _ := r.store.LoadAccount(email)
	err = op(ctx, prov)
	if err == nil || !IsAuth(err) {
		return err
	}

	// 1. Silent renewal (single-flighted across concurrent calls),
	// then one retry on the same channel.
	prov, err = r.flight(ctx, email, "renew", func(ctx context.Context) (Provider, error) {
		st, lerr := r.store.LoadAccount(email)
		if lerr != nil {
			return nil, lerr
		}
		sess, rerr := r.cfg.Login.Renew(ctx, email, st.Session)
		if rerr != nil {
			return nil, rerr
		}
		sess.Email, sess.Channel = email, st.Session.Channel
		if err := r.store.SaveAccount(AccountState{Email: email, Session: sess}); err != nil {
			return nil, err
		}
		return r.cfg.Factory.New(email, sess), nil
	})
	if err == nil {
		err = op(ctx, prov)
		if err == nil || !IsAuth(err) {
			return err
		}
	} else if !IsAuth(err) {
		return err
	}

	// 2. Renewal could not fix it: Graph falls back to OWA, OWA
	// re-logs-in interactively (also single-flighted). One retry.
	st, lerr := r.store.LoadAccount(email)
	if lerr != nil {
		return lerr
	}
	wasGraph := st.Session.Channel == ChannelGraph
	if wasGraph {
		log.Info().Str("account", email).Msg("graph auth failed after renewal, falling back to owa")
	}
	prov, err = r.flight(ctx, email, "fallback", func(ctx context.Context) (Provider, error) {
		// A concurrent call may have completed recovery while we
		// waited for the flight: if the session on disk is no
		// longer the session our failing operation used (channel
		// or token changed) and it is fresh, retry on it instead
		// of logging in again.
		if cur, err := r.store.LoadAccount(email); err == nil &&
			(cur.Session.Channel != base.Session.Channel ||
				cur.Session.AccessToken != base.Session.AccessToken ||
				cur.Session.RefreshToken != base.Session.RefreshToken ||
				cur.Session.OWAAuthHeader != base.Session.OWAAuthHeader) &&
			!cur.Session.Expired(r.cfg.Now()) {
			return r.cfg.Factory.New(email, cur.Session), nil
		}
		return r.loginSequence(ctx, email, []Channel{ChannelOWA})
	})
	if err != nil {
		return err
	}
	return op(ctx, prov)
}
