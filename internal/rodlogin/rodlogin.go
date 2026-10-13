// Package rodlogin implements bridge.LoginManager by driving a real
// browser (go-rod) against Microsoft's login pages.
//
// Graph uses the OAuth2 authorization-code flow with PKCE in a headed
// browser; renewal is a silent refresh_token POST (no browser). OWA
// captures the web app's own Authorization header / token response from
// network traffic in a headed browser (login) or the same profile
// headless (renewal).
//
// The browser profile lives at <stateDir>/accounts/<email>/rod-profile
// (via bridge.NewStore(stateDir).AccountDir(email)), so deleting an
// account directory also resets its browser login. The Store is
// constructed internally from the state dir passed to New.
package rodlogin

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/rs/zerolog"
)

// Manager implements bridge.LoginManager with a Rod-driven browser.
type Manager struct {
	// BrowserPath is the Chrome/Chromium binary to launch; empty means
	// rod's auto-discovery (system browser or rod-managed download).
	BrowserPath string
	// OWARenewTimeout bounds a silent (headless) OWA renewal attempt.
	OWARenewTimeout time.Duration

	stateDir      string
	graphClientID string
	logger        zerolog.Logger

	tokenURL   string
	httpClient *http.Client
	now        func() time.Time

	// Test seams: the browser-driving steps. Unit tests replace these
	// so no Chromium is needed.
	graphCodeFn  func(ctx context.Context, profileDir, authURL, expectedState string) (string, error)
	owaCaptureFn func(ctx context.Context, args owaCaptureArgs) (owaCaptureResult, error)
}

var _ bridge.LoginManager = (*Manager)(nil)

// New creates a Manager. stateDir is the bridge state directory (the
// same one handed to bridge.NewStore); graphClientID may be "" to
// disable Graph (Login then fails AuthError immediately for Graph).
func New(stateDir string, graphClientID string, logger zerolog.Logger) *Manager {
	m := &Manager{
		stateDir:        stateDir,
		graphClientID:   graphClientID,
		logger:          logger,
		OWARenewTimeout: 30 * time.Second,
		tokenURL:        defaultTokenURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		now:             time.Now,
	}
	m.graphCodeFn = m.runGraphBrowser
	m.owaCaptureFn = m.runOWABrowser
	return m
}

// profileDir returns <accountDir>/rod-profile for email.
func (m *Manager) profileDir(email string) (string, error) {
	store, err := bridge.NewStore(m.stateDir)
	if err != nil {
		return "", fmt.Errorf("rodlogin: open store: %w", err)
	}
	dir, err := store.AccountDir(email)
	if err != nil {
		return "", fmt.Errorf("rodlogin: account dir: %w", err)
	}
	return filepath.Join(dir, "rod-profile"), nil
}

// Login runs an interactive (headed) login for the channel. It blocks
// until success, an upstream error, or user cancellation (context
// cancellation or the browser window being closed; the returned error
// message distinguishes the window-closed case).
func (m *Manager) Login(ctx context.Context, email string, ch bridge.Channel) (bridge.Session, error) {
	email = bridge.NormalizeEmail(email)
	switch ch {
	case bridge.ChannelGraph:
		return m.loginGraph(ctx, email)
	case bridge.ChannelOWA:
		return m.loginOWA(ctx, email)
	default:
		return bridge.Session{}, fmt.Errorf("rodlogin: unknown channel %q", string(ch))
	}
}

// Renew silently renews a session. It never shows UI: Graph uses the
// refresh-token grant, OWA reopens the stored browser profile
// headless. Auth-class failures are bridge.AuthError so the registry
// falls back to interactive re-login; network/5xx failures are
// bridge.TransientError.
func (m *Manager) Renew(ctx context.Context, email string, s bridge.Session) (bridge.Session, error) {
	email = bridge.NormalizeEmail(email)
	ch := s.Channel
	if ch == "" {
		if s.RefreshToken != "" {
			ch = bridge.ChannelGraph
		} else {
			ch = bridge.ChannelOWA
		}
	}
	switch ch {
	case bridge.ChannelGraph:
		sess, err := m.renewGraph(ctx, email, s)
		if err != nil {
			if bridge.IsAuth(err) {
				m.logger.Info().Str("account", email).Msg("silent renewal failed: re-login needed")
			}
			return bridge.Session{}, err
		}
		return sess, nil
	case bridge.ChannelOWA:
		sess, err := m.renewOWA(ctx, email, s)
		if err != nil {
			if bridge.IsAuth(err) {
				m.logger.Info().Str("account", email).Msg("silent renewal failed: re-login needed")
			}
			return bridge.Session{}, err
		}
		return sess, nil
	default:
		return bridge.Session{}, bridge.AuthError(fmt.Sprintf("rodlogin: unknown channel %q", string(ch)))
	}
}

func (m *Manager) loginOWA(ctx context.Context, email string) (bridge.Session, error) {
	profile, err := m.profileDir(email)
	if err != nil {
		return bridge.Session{}, err
	}
	m.logger.Info().Str("account", email).Str("channel", "owa").Msg("login window opened for " + email + " (owa)")
	res, err := m.owaCaptureFn(ctx, owaCaptureArgs{
		ProfileDir: profile,
		StartURL:   owaStartURL,
		Headless:   false,
	})
	if err != nil {
		return bridge.Session{}, err
	}
	sess := buildOWASession(email, res.Header, res.Host, res.Cookies)
	m.logger.Info().Str("account", email).Msg("login completed")
	return sess, nil
}

func (m *Manager) renewOWA(ctx context.Context, email string, s bridge.Session) (bridge.Session, error) {
	profile, err := m.profileDir(email)
	if err != nil {
		return bridge.Session{}, err
	}
	start := owaStartURL
	if s.OWAHost != "" {
		start = "https://" + s.OWAHost + "/mail/"
	}
	timeout := m.OWARenewTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := m.owaCaptureFn(rctx, owaCaptureArgs{
		ProfileDir: profile,
		StartURL:   start,
		Headless:   true,
	})
	if err != nil {
		if ctx.Err() != nil {
			return bridge.Session{}, ctx.Err()
		}
		// Nothing captured before the deadline / login page: the
		// stored OWA session is dead.
		return bridge.Session{}, bridge.AuthError("owa session expired")
	}
	return buildOWASession(email, res.Header, res.Host, res.Cookies), nil
}
