package graph_test

// Upstream HTTP operations are logged with their duration (debug
// level), per account and channel, so slow or flaky Microsoft calls
// are attributable in the bridge log.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/graph"
)

// durBuffer is a goroutine-safe log sink (provider calls may run on
// other goroutines).
type durBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *durBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *durBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestGraphUpstreamRequestLoggedWithDuration(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		jsonOut(w, 200, `{"value":[]}`)
	})
	var buf durBuffer
	lg := zerolog.New(&buf)
	factory := graph.Factory{BaseURL: f.srv.URL, Logger: &lg}
	p := factory.New("user@example.com", bridge.Session{
		Email: "user@example.com", Channel: bridge.ChannelGraph,
		AccessToken: "tok", TokenExpiry: time.Now().Add(time.Hour),
	})
	if _, err := p.ListFolders(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`"level":"debug"`, `"channel":"graph"`, `"op":"http"`,
		`"method":"GET"`, `"account":"user@example.com"`,
		`"status":200`, `"duration":"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("upstream log missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tok") {
		t.Fatalf("token leaked into log: %s", out)
	}
}

// A failed upstream call (non-2xx) is a warning and still carries the
// duration, so flaky upstreams are attributable without debug level.
func TestGraphUpstreamFailureLoggedAsWarningWithDuration(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":{"code":"Server Error"}}`)
	})
	var buf durBuffer
	lg := zerolog.New(&buf)
	factory := graph.Factory{BaseURL: f.srv.URL, Logger: &lg}
	p := factory.New("user@example.com", bridge.Session{AccessToken: "tok"})
	if _, err := p.ListFolders(context.Background()); err == nil {
		t.Fatal("want error")
	}
	out := buf.String()
	for _, want := range []string{`"level":"warn"`, `"status":502`, `"duration":"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("upstream failure log missing %s:\n%s", want, out)
		}
	}
}
