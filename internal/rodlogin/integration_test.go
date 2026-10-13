package rodlogin

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/rs/zerolog"
)

// TestBrowserIntegration is the owner's manual gate: it launches a
// real browser. Skipped unless O365BRIDGE_ROD_IT=1. With
// O365BRIDGE_ROD_IT_GRAPH_EMAIL / _OWA_EMAIL set it performs a full
// interactive login for that channel; otherwise it only verifies a
// headless browser can launch and close cleanly.
func TestBrowserIntegration(t *testing.T) {
	if os.Getenv("O365BRIDGE_ROD_IT") != "1" {
		t.Skip("set O365BRIDGE_ROD_IT=1 to run the real-browser gate")
	}
	m := New(t.TempDir(), os.Getenv("O365BRIDGE_GRAPH_CLIENT_ID"), zerolog.New(os.Stderr))
	if p := os.Getenv("O365BRIDGE_ROD_BROWSER"); p != "" {
		m.BrowserPath = p
	}
	if email := os.Getenv("O365BRIDGE_ROD_IT_GRAPH_EMAIL"); email != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		sess, err := m.Login(ctx, email, bridge.ChannelGraph)
		if err != nil {
			t.Fatal(err)
		}
		if sess.AccessToken == "" {
			t.Fatal("graph login returned no access token")
		}
		return
	}
	if email := os.Getenv("O365BRIDGE_ROD_IT_OWA_EMAIL"); email != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		sess, err := m.Login(ctx, email, bridge.ChannelOWA)
		if err != nil {
			t.Fatal(err)
		}
		if sess.OWAAuthHeader == "" {
			t.Fatal("owa login returned no auth header")
		}
		return
	}
	// Launch-only smoke: a headless capture attempt against a blank
	// profile must end as errOWANotCaptured, not a launch failure.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	profile, err := m.profileDir("smoke@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.runOWABrowser(ctx, owaCaptureArgs{ProfileDir: profile, StartURL: owaStartURL, Headless: true}); err != errOWANotCaptured {
		t.Fatalf("smoke capture err = %v, want errOWANotCaptured", err)
	}
}
