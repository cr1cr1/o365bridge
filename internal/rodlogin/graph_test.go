package rodlogin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/rs/zerolog"
)

func TestBuildAuthorizeURL(t *testing.T) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if pkceChallenge(verifier) != challenge {
		t.Fatalf("challenge %q does not match verifier", challenge)
	}
	raw := buildAuthorizeURL("client-123", "User@Example.com", "state-xyz", challenge)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://login.microsoftonline.com/organizations/oauth2/v2.0/authorize" {
		t.Errorf("base = %s", u)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":             "client-123",
		"response_type":         "code",
		"redirect_uri":          redirectURI,
		"response_mode":         "query",
		"scope":                 graphScope,
		"login_hint":            "User@Example.com",
		"state":                 "state-xyz",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestExtractRedirectCode(t *testing.T) {
	good := redirectURI + "?code=abc123&state=s1"
	code, err := extractRedirectCode(good, "s1")
	if err != nil || code != "abc123" {
		t.Fatalf("code=%q err=%v", code, err)
	}
	if _, err := extractRedirectCode(good, "other"); !bridge.IsAuth(err) {
		t.Errorf("wrong state: err=%v want auth", err)
	}
	if _, err := extractRedirectCode(redirectURI+"?error=access_denied&error_description=nope&state=s1", "s1"); !bridge.IsAuth(err) {
		t.Errorf("error param: err=%v want auth", err)
	}
	if _, err := extractRedirectCode("https://example.com/?code=x&state=s1", "s1"); err == nil {
		t.Errorf("non-redirect URL should be ignored (nil error sentinel)")
	} else if !errors.Is(err, errNotRedirect) {
		t.Errorf("non-redirect: err=%v want errNotRedirect", err)
	}
}

func TestParseTokenResponse(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sess, err := parseTokenResponse(http.StatusOK, []byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessToken != "at" || sess.RefreshToken != "rt" {
		t.Errorf("tokens = %+v", sess)
	}
	if !sess.TokenExpiry.Equal(now.Add(time.Hour)) {
		t.Errorf("expiry = %v", sess.TokenExpiry)
	}
}

func TestClassifyTokenErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   bridge.Kind
	}{
		{400, `{"error":"invalid_grant","error_description":"revoked"}`, bridge.KindAuth},
		{400, `{"error":"consent_required"}`, bridge.KindAuth},
		{401, `{"error":"invalid_client"}`, bridge.KindAuth},
		{400, `{"error":"invalid_scope"}`, bridge.KindAuth},
		{403, `{"error":"unauthorized_client"}`, bridge.KindAuth},
		{500, `{"error":"server_error"}`, bridge.KindTransient},
		{502, `garbage`, bridge.KindTransient},
		{429, `{"error":"temporarily_unavailable"}`, bridge.KindTransient},
		{400, `{"error":"temporarily_unavailable"}`, bridge.KindTransient},
		{503, ``, bridge.KindTransient},
	}
	for _, c := range cases {
		_, err := parseTokenResponse(c.status, []byte(c.body), time.Now())
		if err == nil {
			t.Errorf("status %d body %s: nil error", c.status, c.body)
			continue
		}
		if got := bridge.KindOf(err); got != c.kind {
			t.Errorf("status %d body %s: kind=%v want %v (err=%v)", c.status, c.body, got, c.kind, err)
		}
		if strings.Contains(err.Error(), "access_token") {
			t.Errorf("error leaks field names unexpectedly: %v", err)
		}
	}
	// A 200 without access_token is auth-class (malformed grant).
	if _, err := parseTokenResponse(200, []byte(`{"refresh_token":"x"}`), time.Now()); !bridge.IsAuth(err) {
		t.Errorf("200 no token: %v want auth", err)
	}
}

func tokenServer(t *testing.T, status int, payload map[string]any, seen *url.Values) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if seen != nil {
			*seen = r.Form
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
}

func TestExchangeAndRenewViaHTTPTest(t *testing.T) {
	var seen url.Values
	srv := tokenServer(t, 200, map[string]any{"access_token": "at2", "refresh_token": "rt2", "expires_in": 1800}, &seen)
	defer srv.Close()

	m := New(t.TempDir(), "cid", zerolog.Nop())
	m.tokenURL = srv.URL

	sess, err := m.exchangeCode(context.Background(), "a@b.c", "the-code", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessToken != "at2" || sess.RefreshToken != "rt2" || sess.Email != "a@b.c" || sess.Channel != bridge.ChannelGraph {
		t.Errorf("sess = %+v", sess)
	}
	if seen.Get("grant_type") != "authorization_code" || seen.Get("code") != "the-code" ||
		seen.Get("code_verifier") != "verifier-1" || seen.Get("redirect_uri") != redirectURI || seen.Get("client_id") != "cid" {
		t.Errorf("form = %v", seen)
	}

	sess, err = m.renewGraph(context.Background(), "a@b.c", bridge.Session{RefreshToken: "old-rt"})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Get("grant_type") != "refresh_token" || seen.Get("refresh_token") != "old-rt" {
		t.Errorf("renew form = %v", seen)
	}
	// Refresh token rotation: keep old one if server omits a new one.
	srv2 := tokenServer(t, 200, map[string]any{"access_token": "at3", "expires_in": 60}, nil)
	defer srv2.Close()
	m.tokenURL = srv2.URL
	sess, err = m.renewGraph(context.Background(), "a@b.c", bridge.Session{RefreshToken: "keep-me"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.RefreshToken != "keep-me" {
		t.Errorf("refresh token not preserved: %+v", sess)
	}
}

func TestRenewGraphErrorsClassify(t *testing.T) {
	srv := tokenServer(t, 400, map[string]any{"error": "invalid_grant"}, nil)
	defer srv.Close()
	m := New(t.TempDir(), "cid", zerolog.Nop())
	m.tokenURL = srv.URL
	if _, err := m.renewGraph(context.Background(), "a@b.c", bridge.Session{RefreshToken: "x"}); !bridge.IsAuth(err) {
		t.Errorf("invalid_grant renew: %v want auth", err)
	}
	if _, err := m.renewGraph(context.Background(), "a@b.c", bridge.Session{}); !bridge.IsAuth(err) {
		t.Errorf("no refresh token: %v want auth", err)
	}
	// Network failure -> transient.
	m.tokenURL = "http://127.0.0.1:1/token"
	if _, err := m.renewGraph(context.Background(), "a@b.c", bridge.Session{RefreshToken: "x"}); !bridge.IsTransient(err) {
		t.Errorf("network renew: %v want transient", err)
	}
}

func TestGraphLoginNoClientID(t *testing.T) {
	m := New(t.TempDir(), "", zerolog.Nop())
	if _, err := m.Login(context.Background(), "a@b.c", bridge.ChannelGraph); !bridge.IsAuth(err) {
		t.Errorf("empty client id login: %v want auth", err)
	}
	var _ bridge.LoginManager = m
}

func TestRedactURL(t *testing.T) {
	out := redactURL("https://x/y?code=secret&state=abc&foo=bar")
	if strings.Contains(out, "secret") || strings.Contains(out, "abc") {
		t.Errorf("redact failed: %s", out)
	}
	if !strings.Contains(out, "foo=bar") {
		t.Errorf("non-sensitive param dropped: %s", out)
	}
}
