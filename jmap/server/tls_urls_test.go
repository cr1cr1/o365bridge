package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// Exercise discovery over a real HTTP or HTTPS connection. URLs must
// select the actual transport, including the RFC 8887 capability URL.
func TestSessionTransportURLs(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "http"
		if secure {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			e := server.New(fakeAuth{}, server.Options{})
			e.RegisterCapability(jmap.WebSocketURI, map[string]any{"url": "", "supportsPush": true}, nil)
			var srv *httptest.Server
			if secure {
				srv = httptest.NewTLSServer(e.Handler())
			} else {
				srv = httptest.NewServer(e.Handler())
			}
			defer srv.Close()
			for _, path := range []string{"/.well-known/jmap", "/jmap/session"} {
				req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer good")
				client := srv.Client()
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				var s jmap.Session
				err = json.NewDecoder(resp.Body).Decode(&s)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Location") != "" {
					t.Fatalf("direct discovery at %s: status=%d error=%v", path, resp.StatusCode, err)
				}
				for _, u := range []string{s.APIURL, s.DownloadURL, s.UploadURL, s.EventSourceURL} {
					if !strings.HasPrefix(u, srv.URL+"/") {
						t.Errorf("expected URL under %s, got %q", srv.URL, u)
					}
				}
				wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
				wantWS := wsBase + "/jmap/ws/"
				if s.WebSocketURL != wantWS {
					t.Errorf("webSocketUrl = %q, want %q", s.WebSocketURL, wantWS)
				}
				var cap struct {
					URL          string `json:"url"`
					SupportsPush bool   `json:"supportsPush"`
				}
				if err := json.Unmarshal(s.Capabilities[jmap.WebSocketURI], &cap); err != nil {
					t.Fatal(err)
				}
				if cap.URL != wantWS || !cap.SupportsPush {
					t.Errorf("WebSocket capability %+v, want url %q and supportsPush preserved", cap, wantWS)
				}
			}
		})
	}
}
