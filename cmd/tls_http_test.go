package cmd

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/davserver"
	"github.com/cr1cr1/o365bridge/internal/jmapadapter"
	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/rs/zerolog"
)

// Discovery, DAV OPTIONS and Core/echo are transport tests: neither
// browser login nor Microsoft API calls should ever be needed.
type noTLSLogin struct{}

func (noTLSLogin) Login(context.Context, string, bridge.Channel) (bridge.Session, error) {
	return bridge.Session{}, errors.New("unexpected upstream login in TLS test")
}
func (noTLSLogin) Renew(context.Context, string, bridge.Session) (bridge.Session, error) {
	return bridge.Session{}, errors.New("unexpected upstream renewal in TLS test")
}

type noTLSProvider struct{ bridge.Provider }

func testTLSRegistry(t *testing.T) *bridge.Registry {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{
		StateDir: t.TempDir(), Login: noTLSLogin{},
		Factory: bridge.FactoryFunc(func(string, bridge.Session) bridge.Provider { return &noTLSProvider{} }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Store().SaveAccount(bridge.AccountState{
		Email:   "tls@example.com",
		Session: bridge.Session{Email: "tls@example.com", Channel: bridge.ChannelOWA, TokenExpiry: time.Now().Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func startHTTPListener(t *testing.T, handler http.Handler, cfg *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- serveHTTP(srv, cfg)(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				t.Errorf("serve HTTP: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("HTTP listener failed to shut down")
		}
	})
	return ln.Addr().String()
}

func trustedHTTPClient(t *testing.T, roots *x509.CertPool, max uint16) *http.Client {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, MinVersion: tls.VersionTLS12, MaxVersion: max,
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Transport: transport, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestHTTPSDAVAndJMAPWithMinimumTLS(t *testing.T) {
	cert, key, roots := testTLSFiles(t)
	command := ServeCmd{TLSEnabled: true, TLSCert: cert, TLSKey: key}
	cfg, err := command.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	reg := testTLSRegistry(t)
	client := trustedHTTPClient(t, roots, 0)
	for _, endpoint := range []struct {
		name    string
		handler http.Handler
		path    string
		method  string
	}{
		{"dav", davserver.New(reg, "secret", zerolog.Nop()), "/caldav/", http.MethodOptions},
		{"jmap", jmapadapter.NewHandler(reg, "secret", zerolog.Nop()), "/.well-known/jmap", http.MethodGet},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			addr := startHTTPListener(t, endpoint.handler, cfg)
			req, _ := http.NewRequest(endpoint.method, "https://"+addr+endpoint.path, nil)
			req.SetBasicAuth("tls@example.com", "secret")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
				t.Fatalf("%s HTTPS: status=%d TLS=%+v", endpoint.name, resp.StatusCode, resp.TLS)
			}
			if endpoint.name == "jmap" {
				var s jmap.Session
				if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
					t.Fatal(err)
				}
				for _, u := range []string{s.APIURL, s.UploadURL, s.DownloadURL, s.EventSourceURL} {
					if !strings.HasPrefix(u, "https://"+addr+"/") {
						t.Errorf("HTTPS discovery URL: %q", u)
					}
				}
				var cap struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(s.Capabilities[jmap.WebSocketURI], &cap); err != nil {
					t.Fatal(err)
				}
				want := "wss://" + addr + "/jmap/ws/"
				if cap.URL != want || s.WebSocketURL != want {
					t.Errorf("WSS discovery URL: capability=%q session=%q want=%q", cap.URL, s.WebSocketURL, want)
				}
			}
			// Real TLS clients, not an InsecureSkipVerify bypass.
			untrusted := trustedHTTPClient(t, x509.NewCertPool(), 0)
			if bad, err := untrusted.Do(req); err == nil {
				bad.Body.Close()
				t.Error("untrusted certificate accepted")
			}
			oldTLS := trustedHTTPClient(t, roots, tls.VersionTLS12)
			if old, err := oldTLS.Do(req); err == nil {
				old.Body.Close()
				t.Error("TLS1.2 accepted despite TLS1.3 minimum")
			}
			plain := trustedHTTPClient(t, roots, 0)
			plainReq, _ := http.NewRequest(http.MethodGet, "http://"+addr+endpoint.path, nil)
			if p, err := plain.Do(plainReq); err == nil {
				p.Body.Close()
				if p.StatusCode != http.StatusBadRequest {
					t.Errorf("plaintext reached HTTPS listener: status=%d", p.StatusCode)
				}
			}
		})
	}
	if len(cfg.NextProtos) != 0 {
		t.Fatalf("HTTP ALPN leaked into shared IMAP/SMTP TLS config: %v", cfg.NextProtos)
	}
}

func TestHTTPSAllowsExplicitTLS12Minimum(t *testing.T) {
	cert, key, roots := testTLSFiles(t)
	command := ServeCmd{TLSEnabled: true, TLSCert: cert, TLSKey: key, TLSMinVersion: "1.2"}
	cfg, err := command.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	addr := startHTTPListener(t, h, cfg)
	client := trustedHTTPClient(t, roots, tls.VersionTLS12)
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS12 || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("explicit TLS1.2 minimum: status=%d TLS=%v", resp.StatusCode, resp.TLS)
	}
}

func TestTLSDisabledKeepsHTTP(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	addr := startHTTPListener(t, h, nil)
	client := trustedHTTPClient(t, nil, 0)
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("plaintext regression: status=%d TLS=%v", resp.StatusCode, resp.TLS)
	}
}

func TestJMAPWebSocketUpgradeOverTLS(t *testing.T) {
	cert, key, roots := testTLSFiles(t)
	command := ServeCmd{TLSEnabled: true, TLSCert: cert, TLSKey: key}
	cfg, err := command.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	addr := startHTTPListener(t, jmapadapter.NewHandler(testTLSRegistry(t), "secret", zerolog.Nop()), cfg)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("tls@example.com:secret"))
	_, err = fmt.Fprintf(conn, "GET /jmap/ws/ HTTP/1.1\r\nHost: %s\r\nAuthorization: Basic %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: jmap\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", addr, auth)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Protocol") != "jmap" {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("TLS WebSocket upgrade: status=%d body=%s", resp.StatusCode, body)
	}
}
