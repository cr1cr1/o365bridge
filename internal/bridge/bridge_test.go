package bridge_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// --- fakes ---

type fakeLogin struct {
	mu            sync.Mutex
	logins        []bridge.Channel
	loginsByEmail map[string][]bridge.Channel
	renews        int
	renewsByEmail map[string]int
	loginErr      map[bridge.Channel]error
	renewErr      error
}

func (f *fakeLogin) Login(_ context.Context, email string, ch bridge.Channel) (bridge.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins = append(f.logins, ch)
	if f.loginsByEmail == nil {
		f.loginsByEmail = map[string][]bridge.Channel{}
	}
	f.loginsByEmail[email] = append(f.loginsByEmail[email], ch)
	if err := f.loginErr[ch]; err != nil {
		return bridge.Session{}, err
	}
	return bridge.Session{Email: email, Channel: ch, AccessToken: "tok-" + email, TokenExpiry: time.Now().Add(time.Hour)}, nil
}

func (f *fakeLogin) Renew(_ context.Context, email string, s bridge.Session) (bridge.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews++
	if f.renewsByEmail == nil {
		f.renewsByEmail = map[string]int{}
	}
	f.renewsByEmail[email]++
	if f.renewErr != nil {
		return bridge.Session{}, f.renewErr
	}
	s.AccessToken = "tok2"
	s.TokenExpiry = time.Now().Add(time.Hour)
	return s, nil
}

func (f *fakeLogin) loginCount(email string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.loginsByEmail[email])
}

func (f *fakeLogin) renewCount(email string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewsByEmail[email]
}

type fakeProvider struct {
	bridge.Provider // embedded nil; tests override the methods they use

	pingErr error
	opErrs  []error // returned in order by Ping-like op via Do
	doCalls int
}

func (p *fakeProvider) Ping(context.Context) error { return p.pingErr }

type factoryCall struct {
	email   string
	channel bridge.Channel
	token   string
}

type fakeFactory struct {
	mu        sync.Mutex
	channels  []bridge.Channel
	providers map[bridge.Channel]*fakeProvider
	byEmail   map[string]*fakeProvider
	made      []factoryCall
}

func (f *fakeFactory) New(email string, s bridge.Session) bridge.Provider {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = append(f.channels, s.Channel)
	f.made = append(f.made, factoryCall{email: email, channel: s.Channel, token: s.AccessToken})
	if p := f.byEmail[email]; p != nil {
		return p
	}
	if p := f.providers[s.Channel]; p != nil {
		return p
	}
	return &fakeProvider{}
}

func (f *fakeFactory) madeCalls() []factoryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]factoryCall(nil), f.made...)
}

func newRegistry(t *testing.T, graphID string, login *fakeLogin, fac *fakeFactory) *bridge.Registry {
	t.Helper()
	r, err := bridge.NewRegistry(bridge.Config{
		StateDir:      t.TempDir(),
		GraphClientID: graphID,
		Login:         login,
		Factory:       fac,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// --- acceptance scenarios from the design ---

// Given no Graph client ID configured, when a client first presents an
// unknown email, then the bridge logs in via OWA only and serves it.
func TestFirstUseWithoutGraphClientIDGoesStraightToOWA(t *testing.T) {
	login := &fakeLogin{}
	fac := &fakeFactory{}
	r := newRegistry(t, "", login, fac)

	p, err := r.Provider(context.Background(), "User@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("nil provider")
	}
	if len(login.logins) != 1 || login.logins[0] != bridge.ChannelOWA {
		t.Fatalf("logins = %v, want [owa]", login.logins)
	}
	// Second use: session cache, no new login.
	if _, err := r.Provider(context.Background(), "user@example.com"); err != nil {
		t.Fatal(err)
	}
	if len(login.logins) != 1 {
		t.Fatalf("logins after cache hit = %v, want still 1", login.logins)
	}
}

// Given a Graph client ID, when the Graph probe fails with a
// non-transient auth error, then the bridge falls back to OWA
// automatically and persists OWA as the channel.
func TestGraphAuthFailureFallsBackToOWA(t *testing.T) {
	login := &fakeLogin{}
	fac := &fakeFactory{providers: map[bridge.Channel]*fakeProvider{
		bridge.ChannelGraph: {pingErr: bridge.AuthError("consent denied")},
	}}
	r := newRegistry(t, "client-id", login, fac)

	if _, err := r.Provider(context.Background(), "a@corp.example"); err != nil {
		t.Fatal(err)
	}
	if len(login.logins) != 2 || login.logins[0] != bridge.ChannelGraph || login.logins[1] != bridge.ChannelOWA {
		t.Fatalf("logins = %v, want [graph owa]", login.logins)
	}
	acct, err := r.Account("a@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Channel != bridge.ChannelOWA {
		t.Fatalf("channel = %v, want owa", acct.Channel)
	}
}

// Given a Graph client ID, when the Graph probe fails transiently,
// then the bridge must NOT fall back to OWA and must not persist.
func TestGraphTransientFailureDoesNotFallBack(t *testing.T) {
	login := &fakeLogin{}
	fac := &fakeFactory{providers: map[bridge.Channel]*fakeProvider{
		bridge.ChannelGraph: {pingErr: bridge.TransientError("network down")},
	}}
	r := newRegistry(t, "client-id", login, fac)

	if _, err := r.Provider(context.Background(), "a@corp.example"); err == nil {
		t.Fatal("want error")
	}
	if len(login.logins) != 1 {
		t.Fatalf("logins = %v, want only graph attempt", login.logins)
	}
	if _, err := r.Account("a@corp.example"); !errors.Is(err, bridge.ErrUnknownAccount) {
		t.Fatalf("account err = %v, want ErrUnknownAccount", err)
	}
}

// Given a working Graph account, when an operation later fails with
// an auth error and silent renewal also fails, then Call falls back
// to OWA and retries the operation without surfacing an error.
func TestCallFallsBackToOWAOnLaterAuthFailure(t *testing.T) {
	login := &fakeLogin{renewErr: bridge.AuthError("refresh revoked")}
	graphP := &fakeProvider{}
	owaP := &fakeProvider{}
	fac := &fakeFactory{providers: map[bridge.Channel]*fakeProvider{
		bridge.ChannelGraph: graphP, bridge.ChannelOWA: owaP,
	}}
	r := newRegistry(t, "client-id", login, fac)
	if _, err := r.Provider(context.Background(), "a@corp.example"); err != nil {
		t.Fatal(err)
	}

	opCalls := map[bridge.Provider]int{}
	err := r.Call(context.Background(), "a@corp.example", func(_ context.Context, p bridge.Provider) error {
		opCalls[p]++
		if p == bridge.Provider(graphP) {
			return bridge.AuthError("token dead")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if opCalls[graphP] != 1 || opCalls[owaP] != 1 {
		t.Fatalf("op calls = %v, want graph once then owa once", opCalls)
	}
	acct, _ := r.Account("a@corp.example")
	if acct.Channel != bridge.ChannelOWA {
		t.Fatalf("channel after fallback = %v, want owa", acct.Channel)
	}
}

// Concurrent operations that all hit an auth failure must share
// ONE recovery (renewal / fallback login), not one per connection —
// otherwise several login windows would open for the same account.
func TestConcurrentCallAuthRecoverySingleFlights(t *testing.T) {
	login := &fakeLogin{renewErr: bridge.AuthError("refresh revoked")}
	graphP := &fakeProvider{}
	owaP := &fakeProvider{}
	fac := &fakeFactory{providers: map[bridge.Channel]*fakeProvider{
		bridge.ChannelGraph: graphP, bridge.ChannelOWA: owaP,
	}}
	r := newRegistry(t, "client-id", login, fac)
	if _, err := r.Provider(context.Background(), "a@corp.example"); err != nil {
		t.Fatal(err)
	}
	login.mu.Lock()
	login.logins = nil
	login.mu.Unlock()

	// Barrier: every goroutine must be inside the failing operation
	// before any of them returns the auth error, so all eight truly
	// recover concurrently.
	var arrived sync.WaitGroup
	arrived.Add(8)
	release := make(chan struct{})
	go func() { arrived.Wait(); close(release) }()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			errs[i] = r.Call(context.Background(), "a@corp.example", func(_ context.Context, p bridge.Provider) error {
				if p == bridge.Provider(graphP) {
					if first {
						first = false
						arrived.Done()
						<-release
					}
					return bridge.AuthError("token dead")
				}
				return nil
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	owaLogins := 0
	for _, ch := range login.logins {
		if ch == bridge.ChannelOWA {
			owaLogins++
		}
	}
	if owaLogins != 1 {
		t.Fatalf("OWA fallback logins = %d (all logins %v), want exactly 1", owaLogins, login.logins)
	}
}

// Two IDMap handles for one account (e.g. two IMAP connections)
// must share allocations: distinct upstream ids get distinct UIDs
// and neither handle overwrites the other's mappings.
func TestIDMapHandlesShareAllocations(t *testing.T) {
	dir := t.TempDir()
	st, _ := bridge.NewStore(dir)
	h1, err := st.IDMap("a@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := st.IDMap("a@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	u1, err := h1.UID("INBOX", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := h2.UID("INBOX", "msg-2")
	if err != nil {
		t.Fatal(err)
	}
	if u1 == u2 {
		t.Fatalf("handles allocated the same UID %d for different messages", u1)
	}
	// A fresh store sees both mappings.
	st2, _ := bridge.NewStore(dir)
	m, _ := st2.IDMap("a@corp.example")
	if id, ok := m.UpstreamID("INBOX", u1); !ok || id != "msg-1" {
		t.Fatalf("u1 lookup = %q,%v", id, ok)
	}
	if id, ok := m.UpstreamID("INBOX", u2); !ok || id != "msg-2" {
		t.Fatalf("u2 lookup = %q,%v", id, ok)
	}
}

// Concurrent first connections for the same email share one login.
func TestConcurrentFirstUseSingleFlightsLogin(t *testing.T) {
	login := &fakeLogin{}
	r := newRegistry(t, "", login, &fakeFactory{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Provider(context.Background(), "a@corp.example")
		}()
	}
	wg.Wait()
	if len(login.logins) != 1 {
		t.Fatalf("logins = %v, want exactly 1 (single-flight)", login.logins)
	}
}

// Per-account state is isolated, owner-only, and hand-deletable.
func TestStorePerAccountIsolationAndPerms(t *testing.T) {
	dir := t.TempDir()
	st, err := bridge.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"a@corp.example", "b@corp.example"} {
		if err := st.SaveAccount(bridge.AccountState{
			Email:   email,
			Session: bridge.Session{Email: email, Channel: bridge.ChannelOWA, AccessToken: "x"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	aDir := filepath.Join(dir, "accounts", "a@corp.example")
	info, err := os.Stat(aDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("account dir perm = %o, want 700", info.Mode().Perm())
	}
	finfo, err := os.Stat(filepath.Join(aDir, "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	if finfo.Mode().Perm() != 0o600 {
		t.Fatalf("account.json perm = %o, want 600", finfo.Mode().Perm())
	}
	if err := st.DeleteAccount("a@corp.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadAccount("a@corp.example"); !errors.Is(err, bridge.ErrUnknownAccount) {
		t.Fatalf("deleted account load err = %v", err)
	}
	if _, err := st.LoadAccount("b@corp.example"); err != nil {
		t.Fatalf("other account affected by delete: %v", err)
	}
}

// A hostile or mistyped username must not escape the accounts
// directory: path separators in the email are neutralised in the
// on-disk directory name, and save/load still round-trip.
func TestStoreAccountDirCannotEscape(t *testing.T) {
	dir := t.TempDir()
	st, err := bridge.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	evil := "../evil@corp.example"
	if err := st.SaveAccount(bridge.AccountState{
		Email:   evil,
		Session: bridge.Session{Email: evil, Channel: bridge.ChannelOWA},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadAccount(evil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != evil {
		t.Fatalf("round trip email = %q", got.Email)
	}
	// Nothing may have been created outside <root>/accounts.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "accounts" {
			t.Fatalf("escaped state dir: found %q at store root", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts", ".._evil@corp.example", "account.json")); err != nil {
		t.Fatalf("sanitised account file missing: %v", err)
	}
}

// Two accounts used at the same time: each gets its own login
// (exactly one, single-flighted per account), its own provider
// built from its own session token, its own persisted state and
// its own ID map. Nothing crosses over.
func TestTwoAccountsConcurrentIsolation(t *testing.T) {
	const (
		emailA = "a@corp.example"
		emailB = "b@corp.example"
	)
	login := &fakeLogin{}
	aP, bP := &fakeProvider{}, &fakeProvider{}
	fac := &fakeFactory{byEmail: map[string]*fakeProvider{emailA: aP, emailB: bP}}
	r := newRegistry(t, "", login, fac)

	// Concurrent first use of both accounts, repeatedly.
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			email := emailA
			if i%2 == 1 {
				email = emailB
			}
			p, err := r.Provider(context.Background(), email)
			if err != nil {
				errs[i] = err
				return
			}
			want := bridge.Provider(aP)
			if email == emailB {
				want = bridge.Provider(bP)
			}
			if p != want {
				errs[i] = errors.New("provider for " + email + " is the other account's provider")
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := login.loginCount(emailA); got != 1 {
		t.Fatalf("logins for %s = %d, want exactly 1", emailA, got)
	}
	if got := login.loginCount(emailB); got != 1 {
		t.Fatalf("logins for %s = %d, want exactly 1", emailB, got)
	}
	// Every provider ever built carried its own account's session.
	for _, c := range fac.madeCalls() {
		if c.token != "tok-"+c.email {
			t.Fatalf("factory built provider for %s with token %q (another account's session?)", c.email, c.token)
		}
	}

	// ID maps are per account: A's message is invisible to B, and
	// B allocating the same upstream ID does not disturb A.
	st := r.Store()
	mapA, err := st.IDMap(emailA)
	if err != nil {
		t.Fatal(err)
	}
	mapB, err := st.IDMap(emailB)
	if err != nil {
		t.Fatal(err)
	}
	uidA, err := mapA.UID("INBOX", "shared-upstream-id")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mapB.UpstreamID("INBOX", uidA); ok {
		t.Fatal("account B's ID map can see account A's message")
	}
	if _, err := mapB.UID("INBOX", "shared-upstream-id"); err != nil {
		t.Fatal(err)
	}
	if id, ok := mapA.UpstreamID("INBOX", uidA); !ok || id != "shared-upstream-id" {
		t.Fatalf("account A lookup after B allocated = %q,%v", id, ok)
	}

	// Deleting one account's state leaves the other fully usable.
	if err := st.DeleteAccount(emailA); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Account(emailA); !errors.Is(err, bridge.ErrUnknownAccount) {
		t.Fatalf("deleted account err = %v, want ErrUnknownAccount", err)
	}
	if _, err := r.Account(emailB); err != nil {
		t.Fatalf("account B affected by deleting A: %v", err)
	}
	if _, err := r.Provider(context.Background(), emailB); err != nil {
		t.Fatalf("account B provider after deleting A: %v", err)
	}
	if got := login.loginCount(emailB); got != 1 {
		t.Fatalf("account B had to log in again after A was deleted: logins = %d", got)
	}
}

// An auth failure on account A triggers renewal/fallback for A
// only: account B is not renewed, not re-logged-in, and its
// operations are never invoked for A's recovery.
func TestTwoAccountsRecoveryIsolation(t *testing.T) {
	const (
		emailA = "a@corp.example"
		emailB = "b@corp.example"
	)
	login := &fakeLogin{renewErr: bridge.AuthError("refresh revoked")}
	aP, bP := &fakeProvider{}, &fakeProvider{}
	fac := &fakeFactory{byEmail: map[string]*fakeProvider{emailA: aP, emailB: bP}}
	r := newRegistry(t, "", login, fac)
	for _, email := range []string{emailA, emailB} {
		if _, err := r.Provider(context.Background(), email); err != nil {
			t.Fatal(err)
		}
	}

	aCalls, bCalls := 0, 0
	err := r.Call(context.Background(), emailA, func(_ context.Context, p bridge.Provider) error {
		switch p {
		case bridge.Provider(aP):
			aCalls++
			if aCalls == 1 {
				return bridge.AuthError("token dead")
			}
			return nil
		case bridge.Provider(bP):
			bCalls++
			return nil
		}
		return errors.New("unexpected provider")
	})
	if err != nil {
		t.Fatal(err)
	}
	if aCalls != 2 {
		t.Fatalf("account A op calls = %d, want 2 (fail then retry after recovery)", aCalls)
	}
	if bCalls != 0 {
		t.Fatalf("account A's operation ran on account B's provider %d times", bCalls)
	}
	if got := login.renewCount(emailA); got != 1 {
		t.Fatalf("renewals for A = %d, want 1", got)
	}
	if got := login.renewCount(emailB); got != 0 {
		t.Fatalf("account B was renewed because A failed: renewals = %d", got)
	}
	if got := login.loginCount(emailB); got != 1 {
		t.Fatalf("account B logins = %d, want still 1 (its initial login only)", got)
	}
	if got := login.loginCount(emailA); got != 2 {
		t.Fatalf("account A logins = %d, want 2 (initial + fallback re-login)", got)
	}
}

// IMAP UIDs are stable per (folder, upstream ID) across restarts and
// UIDVALIDITY never changes for an existing folder.
func TestIDMapStableAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	st1, _ := bridge.NewStore(dir)
	m1, err := st1.IDMap("a@corp.example")
	if err != nil {
		t.Fatal(err)
	}
	uid1, err := m1.UID("INBOX", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	uid2, err := m1.UID("INBOX", "msg-2")
	if err != nil {
		t.Fatal(err)
	}
	otherFolder, err := m1.UID("Sent", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if uid1 == uid2 || uid1 == 0 {
		t.Fatalf("uids not distinct/nonzero: %d %d", uid1, uid2)
	}
	// UIDs are folder-local in IMAP: the same upstream id in another
	// folder lives in that folder's own UID space (values may
	// coincide numerically; reverse lookups are per folder).
	if otherFolder == 0 {
		t.Fatal("other folder uid must be allocated")
	}
	if up, ok := m1.UpstreamID("Sent", otherFolder); !ok || up != "msg-1" {
		t.Fatalf("Sent reverse lookup = %q,%v want msg-1,true", up, ok)
	}
	vv := m1.UIDValidity("INBOX")

	st2, _ := bridge.NewStore(dir)
	m2, _ := st2.IDMap("a@corp.example")
	again, err := m2.UID("INBOX", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if again != uid1 || m2.UIDValidity("INBOX") != vv {
		t.Fatalf("after restart uid=%d (want %d) validity=%d (want %d)", again, uid1, m2.UIDValidity("INBOX"), vv)
	}
	// Reverse lookup round-trips (needed for UID FETCH/STORE).
	up, ok := m2.UpstreamID("INBOX", uid2)
	if !ok || up != "msg-2" {
		t.Fatalf("reverse lookup = %q,%v want msg-2,true", up, ok)
	}
	_ = otherFolder
}
