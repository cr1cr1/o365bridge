package rodlogin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/rs/zerolog"
)

func TestOWACapturePredicate(t *testing.T) {
	cases := []struct {
		rawurl  string
		headers map[string]string
		want    bool
	}{
		{"https://outlook.office.com/owa/service.svc", map[string]string{"Authorization": "Bearer tok"}, true},
		{"https://outlook.cloud.microsoft/mail/0/inbox/", map[string]string{"authorization": "Bearer tok"}, true},
		{"https://outlook.office365.com/owa/", map[string]string{"Authorization": "Bearer tok"}, true},
		{"https://outlook.office.com/owa/", map[string]string{}, false},
		{"https://outlook.office.com/owa/", map[string]string{"Authorization": "  "}, false},
		{"https://login.microsoftonline.com/common/oauth2/v2.0/token", map[string]string{"Authorization": "Bearer tok"}, false},
		{"https://graph.microsoft.com/v1.0/me", map[string]string{"Authorization": "Bearer tok"}, false},
		{"https://example.com/", map[string]string{"Authorization": "Bearer tok"}, false},
	}
	for _, c := range cases {
		hdr, host, ok := captureFromRequest(c.rawurl, c.headers)
		if ok != c.want {
			t.Errorf("%s: ok=%v want %v", c.rawurl, ok, c.want)
			continue
		}
		if ok && (hdr == "" || host == "") {
			t.Errorf("%s: hdr/host empty %q %q", c.rawurl, hdr, host)
		}
	}
	// Verbatim capture.
	hdr, host, ok := captureFromRequest("https://outlook.office.com/api/v2.0/me", map[string]string{"Authorization": "Bearer XYZ"})
	if !ok || hdr != "Bearer XYZ" || host != "outlook.office.com" {
		t.Errorf("verbatim: %q %q %v", hdr, host, ok)
	}
}

func TestOWAHeaderFromTokenResponse(t *testing.T) {
	hdr, ok := owaHeaderFromTokenBody([]byte(`{"access_token":"tok123","token_type":"Bearer"}`))
	if !ok || hdr != "Bearer tok123" {
		t.Errorf("hdr=%q ok=%v", hdr, ok)
	}
	if _, ok := owaHeaderFromTokenBody([]byte(`{"error":"invalid_grant"}`)); ok {
		t.Errorf("error body should not capture")
	}
	if _, ok := owaHeaderFromTokenBody([]byte(`not json`)); ok {
		t.Errorf("garbage should not capture")
	}
	// Token-endpoint predicate: only token POSTs count.
	if !isTokenEndpointURL("https://login.microsoftonline.com/organizations/oauth2/v2.0/token") {
		t.Errorf("token endpoint not recognised")
	}
	if isTokenEndpointURL("https://outlook.office.com/owa/service.svc") {
		t.Errorf("owa service misclassified as token endpoint")
	}
}

func TestCookiesToSession(t *testing.T) {
	cookies := []cookieKV{
		{Name: "X-OWA-CANARY", Value: "canary1", Domain: "outlook.office.com"},
		{Name: "cadata", Value: "cd1", Domain: ".outlook.office.com"},
		{Name: "Other", Value: "o1", Domain: "outlook.cloud.microsoft"},
		{Name: "Unrelated", Value: "u1", Domain: "login.microsoftonline.com"},
		{Name: "Empty", Value: "", Domain: "outlook.office.com"},
	}
	sess := buildOWASession("a@b.c", "Bearer tok", "outlook.office.com", cookies)
	if sess.OWAAuthHeader != "Bearer tok" || sess.OWAHost != "outlook.office.com" || sess.Channel != bridge.ChannelOWA {
		t.Errorf("sess = %+v", sess)
	}
	if sess.OWACookies["X-OWA-CANARY"] != "canary1" {
		t.Errorf("canary missing: %v", sess.OWACookies)
	}
	if sess.OWACookies["cadata"] != "cd1" || sess.OWACookies["Other"] != "o1" {
		t.Errorf("cookies = %v", sess.OWACookies)
	}
	if _, ok := sess.OWACookies["Unrelated"]; ok {
		t.Errorf("unrelated host cookie included: %v", sess.OWACookies)
	}
	if _, ok := sess.OWACookies["Empty"]; ok {
		t.Errorf("empty cookie included")
	}
}

func TestManagerDispatchWithSeams(t *testing.T) {
	m := New(t.TempDir(), "cid", zerolog.Nop())
	// Graph login via stubbed browser step.
	m.graphCodeFn = func(_ context.Context, _ string, _ string, _ string) (string, error) {
		return "code-from-browser", nil
	}
	m.tokenURL = tokenServer(t, 200, map[string]any{"access_token": "gat", "refresh_token": "grt", "expires_in": 3600}, nil).URL
	sess, err := m.Login(context.Background(), "a@b.c", bridge.ChannelGraph)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessToken != "gat" || sess.Channel != bridge.ChannelGraph {
		t.Errorf("graph sess = %+v", sess)
	}

	// OWA login via stubbed browser step.
	m.owaCaptureFn = func(_ context.Context, _ owaCaptureArgs) (owaCaptureResult, error) {
		return owaCaptureResult{
			Header: "Bearer owa-tok", Host: "outlook.office.com",
			Cookies: []cookieKV{{Name: "X-OWA-CANARY", Value: "cv", Domain: "outlook.office.com"}},
		}, nil
	}
	sess, err = m.Login(context.Background(), "a@b.c", bridge.ChannelOWA)
	if err != nil {
		t.Fatal(err)
	}
	if sess.OWAAuthHeader != "Bearer owa-tok" || sess.OWACookies["X-OWA-CANARY"] != "cv" {
		t.Errorf("owa sess = %+v", sess)
	}

	// OWA renew: stub receives headless=true.
	var gotHeadless bool
	m.owaCaptureFn = func(_ context.Context, a owaCaptureArgs) (owaCaptureResult, error) {
		gotHeadless = a.Headless
		return owaCaptureResult{}, errOWANotCaptured
	}
	if _, err := m.Renew(context.Background(), "a@b.c", bridge.Session{Channel: bridge.ChannelOWA, OWAHost: "outlook.office.com"}); !bridge.IsAuth(err) {
		t.Errorf("owa renew no capture: %v want auth", err)
	}
	if !gotHeadless {
		t.Errorf("owa renew not headless")
	}
	// Graph renew dispatches on refresh token even without channel.
	sess, err = m.Renew(context.Background(), "a@b.c", bridge.Session{RefreshToken: "rt"})
	if err != nil || sess.AccessToken != "gat" {
		t.Errorf("graph renew dispatch: %+v %v", sess, err)
	}
	// Unknown channel.
	if _, err := m.Login(context.Background(), "a@b.c", bridge.Channel("bogus")); err == nil {
		t.Errorf("bogus channel should error")
	}
}

func TestProfileDir(t *testing.T) {
	m := New(t.TempDir(), "cid", zerolog.Nop())
	dir, err := m.profileDir("User@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dir, "accounts/user@example.com/rod-profile") {
		t.Errorf("profile dir = %s", dir)
	}
}

func TestOWARenewTimeoutFieldDefault(t *testing.T) {
	m := New(t.TempDir(), "cid", zerolog.Nop())
	if m.OWARenewTimeout != 30*time.Second {
		t.Errorf("default renew timeout = %v", m.OWARenewTimeout)
	}
}
