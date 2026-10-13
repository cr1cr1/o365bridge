package rodlogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

const (
	authorizeBaseURL = "https://login.microsoftonline.com/organizations/oauth2/v2.0/authorize"
	defaultTokenURL  = "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"
	redirectURI      = "https://login.microsoftonline.com/common/oauth2/nativeclient"
	graphScope       = "offline_access https://graph.microsoft.com/.default"
)

// errNotRedirect marks a URL that is not the OAuth redirect target.
var errNotRedirect = errors.New("rodlogin: not a redirect URL")

// newPKCE returns a fresh PKCE verifier and its S256 challenge.
func newPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	return verifier, pkceChallenge(verifier), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// buildAuthorizeURL builds the Microsoft authorize URL for one attempt.
func buildAuthorizeURL(clientID, email, state, challenge string) string {
	q := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"response_mode":         {"query"},
		"scope":                 {graphScope},
		"login_hint":            {email},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return authorizeBaseURL + "?" + q.Encode()
}

// extractRedirectCode pulls the authorization code out of a navigation
// to the redirect URI, verifying state. Non-redirect URLs return
// errNotRedirect; an upstream error parameter or a state mismatch is
// an AuthError.
func extractRedirectCode(rawURL, expectedState string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", errNotRedirect
	}
	ru, _ := url.Parse(redirectURI)
	if u.Scheme != ru.Scheme || u.Host != ru.Host || u.Path != ru.Path {
		return "", errNotRedirect
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", bridge.AuthError("graph authorization failed: " + e)
	}
	if q.Get("state") != expectedState {
		return "", bridge.AuthError("graph authorization failed: state mismatch")
	}
	code := q.Get("code")
	if code == "" {
		return "", bridge.AuthError("graph authorization failed: no code in redirect")
	}
	return code, nil
}

type tokenPayload struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// parseTokenResponse turns a token-endpoint response into a Session
// (Email/Channel left to the caller) or a classified error. It never
// includes token material in error messages.
func parseTokenResponse(status int, body []byte, now time.Time) (bridge.Session, error) {
	var p tokenPayload
	_ = json.Unmarshal(body, &p) // best effort; status drives class
	if status >= 200 && status < 300 {
		if p.AccessToken == "" {
			return bridge.Session{}, bridge.AuthError("graph token endpoint returned no access token")
		}
		sess := bridge.Session{AccessToken: p.AccessToken, RefreshToken: p.RefreshToken}
		if p.ExpiresIn > 0 {
			sess.TokenExpiry = now.Add(time.Duration(p.ExpiresIn) * time.Second)
		}
		return sess, nil
	}
	if kind := classifyTokenError(status, p.Error); kind == bridge.KindTransient {
		return bridge.Session{}, bridge.TransientError(fmt.Sprintf("graph token endpoint failed: %s (HTTP %d)", errLabel(p.Error), status))
	}
	return bridge.Session{}, bridge.AuthError(fmt.Sprintf("graph token endpoint failed: %s (HTTP %d)", errLabel(p.Error), status))
}

func errLabel(code string) string {
	if code == "" {
		return "unknown error"
	}
	return code
}

// classifyTokenError maps a token-endpoint failure onto a bridge Kind:
// consent/grant/client/scope problems are auth-class (re-login may
// help); network-adjacent codes, 429 and 5xx are transient.
func classifyTokenError(status int, errCode string) bridge.Kind {
	switch errCode {
	case "temporarily_unavailable", "server_error", "slow_down":
		return bridge.KindTransient
	}
	if status == 429 || status >= 500 {
		return bridge.KindTransient
	}
	// invalid_grant, consent_required, invalid_client, invalid_scope,
	// unauthorized_client, interaction_required, and any other 4xx.
	return bridge.KindAuth
}

// doToken POSTs a grant to the token endpoint and parses the result.
func (m *Manager) doToken(ctx context.Context, form url.Values) (bridge.Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return bridge.Session{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return bridge.Session{}, ctx.Err()
		}
		return bridge.Session{}, bridge.Wrap(bridge.KindTransient, "graph token endpoint request failed", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return bridge.Session{}, bridge.Wrap(bridge.KindTransient, "graph token endpoint read failed", err)
	}
	return parseTokenResponse(resp.StatusCode, body, m.now())
}

func (m *Manager) exchangeCode(ctx context.Context, email, code, verifier string) (bridge.Session, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {m.graphClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"scope":         {graphScope},
	}
	sess, err := m.doToken(ctx, form)
	if err != nil {
		return bridge.Session{}, err
	}
	sess.Email = email
	sess.Channel = bridge.ChannelGraph
	return sess, nil
}

func (m *Manager) renewGraph(ctx context.Context, email string, s bridge.Session) (bridge.Session, error) {
	if m.graphClientID == "" {
		return bridge.Session{}, bridge.AuthError("graph renewal unavailable: no graph client ID configured")
	}
	if s.RefreshToken == "" {
		return bridge.Session{}, bridge.AuthError("graph session expired: no refresh token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {m.graphClientID},
		"refresh_token": {s.RefreshToken},
		"scope":         {graphScope},
	}
	sess, err := m.doToken(ctx, form)
	if err != nil {
		return bridge.Session{}, err
	}
	sess.Email = email
	sess.Channel = bridge.ChannelGraph
	if sess.RefreshToken == "" { // no rotation: keep the old one
		sess.RefreshToken = s.RefreshToken
	}
	return sess, nil
}

func (m *Manager) loginGraph(ctx context.Context, email string) (bridge.Session, error) {
	if m.graphClientID == "" {
		return bridge.Session{}, bridge.AuthError("graph login unavailable: no graph client ID configured")
	}
	profile, err := m.profileDir(email)
	if err != nil {
		return bridge.Session{}, err
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		return bridge.Session{}, fmt.Errorf("rodlogin: pkce: %w", err)
	}
	state, err := randomState()
	if err != nil {
		return bridge.Session{}, fmt.Errorf("rodlogin: state: %w", err)
	}
	authURL := buildAuthorizeURL(m.graphClientID, email, state, challenge)
	m.logger.Info().Str("account", email).Str("channel", "graph").Msg("login window opened for " + email + " (graph)")
	m.logger.Debug().Str("url", redactURL(authURL)).Msg("graph authorize url")
	code, err := m.graphCodeFn(ctx, profile, authURL, state)
	if err != nil {
		return bridge.Session{}, err
	}
	sess, err := m.exchangeCode(ctx, email, code, verifier)
	if err != nil {
		return bridge.Session{}, err
	}
	m.logger.Info().Str("account", email).Msg("login completed")
	return sess, nil
}

var sensitiveParams = map[string]bool{
	"code": true, "code_challenge": true, "code_verifier": true,
	"state": true, "access_token": true, "refresh_token": true,
	"id_token": true, "client_secret": true, "assertion": true,
}

// redactURL replaces sensitive query values for safe debug logging.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	q := u.Query()
	for k := range q {
		if sensitiveParams[strings.ToLower(k)] {
			q.Set(k, "redacted")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
