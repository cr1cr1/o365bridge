package owa

// Upstream OWA service.svc operations are logged with their duration
// (debug level), per account and action; failures as warnings so a
// flaky tenant is attributable without debug level.

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type owaLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *owaLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *owaLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLoggedFake(t *testing.T, respond func(string, map[string]any) any, logger *zerolog.Logger) bridge.Provider {
	t.Helper()
	f := &fake{t: t, respond: respond}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return Factory{BaseURL: srv.URL + "/owa/", Logger: logger}.New("user@example.com", bridge.Session{
		Email: "user@example.com", OWAAuthHeader: "Bearer token123",
		OWACookies: map[string]string{"X-OWA-CANARY": "canary"},
	})
}

func TestOWAUpstreamRequestLoggedWithDuration(t *testing.T) {
	var buf owaLogBuffer
	lg := zerolog.New(&buf)
	p := newLoggedFake(t, func(string, map[string]any) any {
		return okBody(map[string]any{"RootFolder": map[string]any{
			"ParentFolder": map[string]any{"FolderId": map[string]any{"Id": "root"}},
		}})
	}, &lg)
	if _, err := p.ListFolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`"level":"debug"`, `"channel":"owa"`, `"op":"http"`,
		`"method":"POST"`, `"action":"FindFolder"`,
		`"account":"user@example.com"`, `"status":200`, `"duration":"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("upstream log missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "token123") {
		t.Fatalf("auth header leaked into log: %s", out)
	}
}

func TestOWAUpstreamFailureLoggedAsWarningWithDuration(t *testing.T) {
	var buf owaLogBuffer
	lg := zerolog.New(&buf)
	p := newLoggedFake(t, func(string, map[string]any) any {
		return map[string]any{"ResponseClass": "Error", "MessageText": "boom"}
	}, &lg)
	if _, err := p.ListFolders(context.Background()); err == nil {
		t.Fatal("want error")
	}
	out := buf.String()
	for _, want := range []string{`"level":"warn"`, `"duration":"`, "boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("upstream failure log missing %s:\n%s", want, out)
		}
	}
}
