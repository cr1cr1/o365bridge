package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/rs/zerolog"
)

type payloadBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *payloadBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *payloadBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

func graphTraceRecords(t *testing.T, out string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("invalid log: %v: %s", err, line)
		}
		if r["level"] == "trace" {
			records = append(records, r)
		}
	}
	return records
}

func loggedGraph(t *testing.T, handler http.HandlerFunc, level zerolog.Level) (*provider, *payloadBuffer) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	buf := new(payloadBuffer)
	lg := zerolog.New(buf).Level(level)
	p := Factory{BaseURL: srv.URL, Logger: &lg}.New("user@example.com", bridge.Session{AccessToken: "session-access-secret", RefreshToken: "session-refresh-secret", OWAAuthHeader: "Bearer session-bearer-secret", OWACookies: map[string]string{"cookie": "session-cookie-secret", "X-OWA-CANARY": "session-canary-secret"}}).(*provider)
	return p, buf
}

func TestGraphPayloadHTTPPairs(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType string
		status                  int
		wantErr                 bool
	}{
		{"success", `{"subject":"private response"}`, "application/json", 200, false},
		{"empty", "", "", 204, false},
		{"native-http", `{"error":{"code":"ErrorAccessDenied","message":"boom"}}`, "application/json", 403, true},
		{"malformed", `{"access_token":"unknown-malformed-secret"`, "application/json", 200, false},
		{"malformed-http", `{"access_token":"unknown-malformed-secret"`, "application/json", 502, true},
		{"mime", "Subject: private MIME\r\nAuthorization: Bearer unknown-mime-secret\r\n\r\nbody", "message/rfc822", 200, false},
		{"binary", string([]byte{0, 255, 1, 2}), "application/octet-stream", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := loggedGraph(t, func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				if string(data) != `{"subject":"private request"}` || r.Header.Get("Authorization") != "Bearer session-access-secret" || r.Header.Get("X-Custom") != "unchanged" || r.Method != "POST" || r.URL.Path != "/v1.0/me/test" {
					t.Errorf("request changed: %s %v %s", r.URL, r.Header, data)
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}, zerolog.TraceLevel)
			got, err := p.doBytesLimit(context.Background(), "POST", "/me/test", []byte(`{"subject":"private request"}`), "application/json", map[string]string{"X-Custom": "unchanged"}, 0)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if !tc.wantErr && string(got) != tc.body {
				t.Fatalf("response changed: %q", got)
			}
			records := graphTraceRecords(t, buf.String())
			if len(records) != 2 {
				t.Fatalf("want trace pair, got %d: %s", len(records), buf.String())
			}
			for i, r := range records {
				if r["channel"] != "graph" || r["account"] != "user@example.com" || r["method"] != "POST" || r["resource"] != "/me/test" || r["direction"] != []string{"request", "response"}[i] || r["request_id"] == nil || r["payload"] == nil {
					t.Fatalf("bad trace: %#v", r)
				}
			}
			if records[0]["request_id"] != records[1]["request_id"] || records[1]["status"] != float64(tc.status) {
				t.Fatalf("bad correlation/status: %#v", records)
			}
			out := buf.String()
			if !strings.Contains(out, "private request") {
				t.Fatal("request payload absent")
			}
			if tc.name == "success" && !strings.Contains(out, "private response") {
				t.Fatal("response payload absent")
			}
			for _, secret := range []string{"unknown-malformed-secret", "unknown-mime-secret", "session-access-secret"} {
				if strings.Contains(out, secret) {
					t.Fatalf("credential %q leaked: %s", secret, out)
				}
			}
		})
	}
}

func TestGraphPayloadSecretsAndBounds(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"ErrorAccessDenied","message":"session-access-secret session-refresh-secret session-bearer-secret session-cookie-secret session-canary-secret"},"nested":[{"access_token":"unknown-access-secret","refreshToken":"unknown-refresh-secret","Authorization":"Bearer unknown-auth-secret","Cookie":"unknown-cookie-secret","canary":"unknown-canary-secret"}],"url":"https://user:password-secret@example.com/"}`,
		`{"text":"` + strings.Repeat("x", 70<<10) + `","access_token":"cutoff-secret"}`,
	} {
		p, buf := loggedGraph(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			_, _ = io.WriteString(w, body)
		}, zerolog.TraceLevel)
		_, _ = p.doBytesLimit(context.Background(), "GET", "/me/test", nil, "", nil, 0)
		out := buf.String()
		for _, secret := range []string{"session-access-secret", "session-refresh-secret", "session-bearer-secret", "session-cookie-secret", "session-canary-secret", "unknown-access-secret", "unknown-refresh-secret", "unknown-auth-secret", "unknown-cookie-secret", "unknown-canary-secret", "password-secret", "cutoff-secret"} {
			if strings.Contains(out, secret) {
				t.Fatalf("secret %q leaked: %.1000s", secret, out)
			}
		}
		if len(graphTraceRecords(t, out)) != 2 {
			t.Fatalf("missing traces: %.1000s", out)
		}
		for _, line := range strings.Split(out, "\n") {
			if len(line) > 64<<10 {
				t.Fatalf("unbounded record: %d", len(line))
			}
		}
	}
}

func TestGraphPayloadDebugAndNilDisabled(t *testing.T) {
	for _, nilLogger := range []bool{false, true} {
		p, buf := loggedGraph(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"subject":"private response"}`)
		}, zerolog.DebugLevel)
		if nilLogger {
			p.log = zerolog.Nop()
		}
		if _, err := p.doBytesLimit(context.Background(), "POST", "/me/test", []byte(`{"subject":"private request"}`), "application/json", nil, 0); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if strings.Contains(out, "payload") || strings.Contains(out, "private") {
			t.Fatalf("disabled payload log: %s", out)
		}
		if nilLogger && out != "" {
			t.Fatalf("nil logger output: %s", out)
		}
	}
}

func TestGraphPayloadPartialAndDownloadLimit(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		length, limit int
		transient     bool
	}{
		{"partial", `{"access_token":"partial-secret"`, 200, 0, true},
		{"limit", `{"access_token":"limit-secret"}`, 0, 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := loggedGraph(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.length > 0 {
					w.Header().Set("Content-Length", "200")
				}
				_, _ = io.WriteString(w, tc.body)
			}, zerolog.TraceLevel)
			data, err := p.doBytesLimit(context.Background(), "GET", "/me/test", nil, "", nil, int64(tc.limit))
			if err == nil || data != nil {
				t.Fatalf("error/bytes changed: %q %v", data, err)
			}
			if tc.transient && !bridge.IsTransient(err) {
				t.Fatalf("classification changed: %v", err)
			}
			if tc.name == "limit" && !strings.Contains(err.Error(), "exceeds download limit") {
				t.Fatalf("limit changed: %v", err)
			}
			out := buf.String()
			records := graphTraceRecords(t, out)
			if len(records) != 2 || strings.Contains(out, "partial-secret") || strings.Contains(out, "limit-secret") {
				t.Fatalf("unsafe partial trace: %s", out)
			}
			captured := records[1]["payload"].(map[string]any)["captured_bytes"]
			if tc.limit > 0 && captured != float64(tc.limit+1) {
				t.Fatalf("limit reader consumed extra bytes: %v", captured)
			}
		})
	}
}

func TestGraphPayloadMIMERequestUnchanged(t *testing.T) {
	body := []byte("U3ViamVjdDogcHJpdmF0ZQ0KDQpib2R5")
	p, buf := loggedGraph(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) || r.Header.Get("Content-Type") != "text/plain" {
			t.Errorf("MIME changed: %q", got)
		}
		w.WriteHeader(202)
	}, zerolog.TraceLevel)
	if _, err := p.doBytesLimit(context.Background(), "POST", "/me/sendMail", body, "text/plain", nil, 0); err != nil {
		t.Fatal(err)
	}
	records := graphTraceRecords(t, buf.String())
	if len(records) != 2 || !strings.Contains(buf.String(), "omitted") || strings.Contains(buf.String(), string(body)) {
		t.Fatalf("unsafe MIME trace: %s", buf.String())
	}
}

func TestGraphPayloadNetworkAndCancellationTrace(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		p, buf := loggedGraph(t, func(w http.ResponseWriter, r *http.Request) {}, zerolog.TraceLevel)
		p.baseURL = "http://127.0.0.1:1"
		ctx := context.Background()
		if cancel {
			c, stop := context.WithCancel(ctx)
			stop()
			ctx = c
		}
		_, err := p.doBytesLimit(ctx, "GET", "/me/test", nil, "", nil, 0)
		if err == nil || cancel && err != context.Canceled || !cancel && !bridge.IsTransient(err) {
			t.Fatalf("network classification changed: %v", err)
		}
		records := graphTraceRecords(t, buf.String())
		if len(records) != 2 || records[1]["status"] != float64(0) {
			t.Fatalf("missing network trace: %s", buf.String())
		}
	}
}

func TestGraphPayloadConcurrentCorrelation(t *testing.T) {
	p, buf := loggedGraph(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{}`) }, zerolog.TraceLevel)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := p.doBytesLimit(context.Background(), "GET", "/me/test", nil, "", nil, 0); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	records := graphTraceRecords(t, buf.String())
	ids := map[any]map[any]bool{}
	for _, r := range records {
		id := r["request_id"]
		if ids[id] == nil {
			ids[id] = map[any]bool{}
		}
		ids[id][r["direction"]] = true
	}
	if len(records) != 16 || len(ids) != 8 {
		t.Fatalf("bad concurrent correlation: %#v", records)
	}
	for _, pair := range ids {
		if !pair["request"] || !pair["response"] {
			t.Fatalf("unpaired trace: %#v", ids)
		}
	}
}
