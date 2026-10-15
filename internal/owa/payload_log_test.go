package owa

import (
	"bytes"
	"context"
	"encoding/base64"
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

func traceOWA(t *testing.T, response string, status int, level zerolog.Level) (*provider, *owaLogBuffer) {
	t.Helper()
	buf := new(owaLogBuffer)
	lg := zerolog.New(buf).Level(level)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"Body":{"Subject":"private request"}}` {
			t.Errorf("request changed: %s", b)
		}
		if r.Header.Get("Authorization") != "Bearer session-bearer-secret" || r.Header.Get("Cookie") != "X-OWA-CANARY=session-canary-secret; session=session-cookie-secret" {
			t.Errorf("auth changed: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	p := Factory{BaseURL: srv.URL + "/owa/", Logger: &lg}.New("user@example.com", bridge.Session{AccessToken: "session-access-secret", RefreshToken: "session-refresh-secret", OWAAuthHeader: "Bearer session-bearer-secret", OWACookies: map[string]string{"X-OWA-CANARY": "session-canary-secret", "session": "session-cookie-secret"}}).(*provider)
	return p, buf
}

func owaTraceRecords(t *testing.T, out string) []map[string]any {
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

func TestOWAPayloadTraceHTTPPairs(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
	}{
		{"success", `{"Body":{"Subject":"private response"}}`, 200, false},
		{"native", `{"Body":{"ResponseClass":"Error","ResponseCode":"ErrorInvalidRequest","MessageText":"boom"}}`, 200, true},
		{"http", `{"error":"boom"}`, 503, true},
		{"malformed", `{"access_token":"unknown-malformed-secret"`, 200, true},
		{"empty", ``, 204, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := traceOWA(t, tc.body, tc.status, zerolog.TraceLevel)
			_, err := p.call(context.Background(), "TestAction", map[string]any{"Body": map[string]any{"Subject": "private request"}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			records := owaTraceRecords(t, buf.String())
			if len(records) != 2 {
				t.Fatalf("want trace pair, got %d: %s", len(records), buf.String())
			}
			for i, r := range records {
				direction := []string{"request", "response"}[i]
				if r["channel"] != "owa" || r["account"] != "user@example.com" || r["action"] != "TestAction" || r["direction"] != direction || r["payload"] == nil || r["request_id"] == nil {
					t.Fatalf("bad trace: %#v", r)
				}
			}
			if records[0]["request_id"] != records[1]["request_id"] || records[1]["status"] != float64(tc.status) {
				t.Fatalf("bad correlation/status: %#v", records)
			}
			if !strings.Contains(buf.String(), "private request") {
				t.Fatal("request payload absent")
			}
			if tc.name == "success" && !strings.Contains(buf.String(), "private response") {
				t.Fatal("response payload absent")
			}
			if strings.Contains(buf.String(), "unknown-malformed-secret") {
				t.Fatal("malformed payload leaked")
			}
		})
	}
}

func TestOWAPayloadDebugAndNilDisabled(t *testing.T) {
	for _, nilLogger := range []bool{false, true} {
		p, buf := traceOWA(t, `{"Body":{"Subject":"private response"}}`, 200, zerolog.DebugLevel)
		if nilLogger {
			p.log = zerolog.Nop()
		}
		if _, err := p.call(context.Background(), "TestAction", map[string]any{"Body": map[string]any{"Subject": "private request"}}); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if strings.Contains(out, "payload") || strings.Contains(out, "private") {
			t.Fatalf("disabled payload logging: %s", out)
		}
		if nilLogger && out != "" {
			t.Fatalf("nil logger output: %s", out)
		}
	}
}

func TestOWAPayloadSecretsAndBounds(t *testing.T) {
	for _, body := range []string{
		`{"Body":{"ResponseClass":"Error","MessageText":"session-bearer-secret session-access-secret session-refresh-secret session-canary-secret session-cookie-secret","nested":[{"Access_Token":"unknown-access-secret","Authorization":"Bearer unknown-auth-secret","Cookie":"unknown-cookie-secret","X-OWA-CANARY":"unknown-canary-secret","refreshToken":"unknown-refresh-secret"}],"url":"https://username:password-secret@example.com/a"}}`,
		`{"Body":{"text":"` + strings.Repeat("x", 70<<10) + `","access_token":"cutoff-secret"}}`,
	} {
		p, buf := traceOWA(t, body, 200, zerolog.TraceLevel)
		_, _ = p.call(context.Background(), "TestAction", map[string]any{"Body": map[string]any{"Subject": "private request"}})
		out := buf.String()
		for _, secret := range []string{"session-bearer-secret", "session-access-secret", "session-refresh-secret", "session-canary-secret", "session-cookie-secret", "unknown-access-secret", "unknown-auth-secret", "unknown-cookie-secret", "unknown-canary-secret", "unknown-refresh-secret", "password-secret", "cutoff-secret"} {
			if strings.Contains(out, secret) {
				t.Fatalf("secret %q leaked: %.1000s", secret, out)
			}
		}
		if len(owaTraceRecords(t, out)) != 2 {
			t.Fatalf("trace pair absent: %.1000s", out)
		}
		for _, line := range strings.Split(out, "\n") {
			if len(line) > 64<<10 {
				t.Fatalf("unbounded record: %d", len(line))
			}
		}
	}
}

func TestOWAPayloadPartialReadTrace(t *testing.T) {
	var buf owaLogBuffer
	lg := zerolog.New(&buf).Level(zerolog.TraceLevel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "200")
		_, _ = io.WriteString(w, `{"Body":{"access_token":"partial-secret"`)
	}))
	defer srv.Close()
	p := Factory{BaseURL: srv.URL, Logger: &lg}.New("user@example.com", bridge.Session{}).(*provider)
	_, err := p.call(context.Background(), "Test", map[string]any{})
	if err == nil || !bridge.IsTransient(err) {
		t.Fatalf("partial-read behavior changed: %v", err)
	}
	records := owaTraceRecords(t, buf.String())
	if len(records) != 2 || strings.Contains(buf.String(), "partial-secret") {
		t.Fatalf("unsafe partial trace: %s", buf.String())
	}
}

func TestOWAPayloadRequestSecretsAndOpaqueFields(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("X-Session: request-session-secret\r\n\r\nprivate body"))
	payload := map[string]any{"Body": map[string]any{"Subject": "private request subject", "nested": []any{map[string]any{"access_token": "unknown-request-secret", "Authorization": "Bearer unknown-request-auth"}}, "echo": "request-session-secret", "MimeContent": map[string]any{"Value": encoded}}}
	want, _ := json.Marshal(payload)
	var buf owaLogBuffer
	lg := zerolog.New(&buf).Level(zerolog.TraceLevel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, want) {
			t.Errorf("request bytes changed: %s", got)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	p := Factory{BaseURL: srv.URL, Logger: &lg}.New("user@example.com", bridge.Session{AccessToken: "request-session-secret"}).(*provider)
	if _, err := p.call(context.Background(), "CreateItem", payload); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, secret := range []string{"request-session-secret", "unknown-request-secret", "unknown-request-auth", encoded} {
		if strings.Contains(out, secret) {
			t.Fatalf("request credential leaked: %s", out)
		}
	}
	if len(owaTraceRecords(t, out)) != 2 || !strings.Contains(out, "private request subject") || !strings.Contains(out, "OMITTED: opaque MIME/file content") {
		t.Fatalf("request diagnostics missing: %s", out)
	}
	after, _ := json.Marshal(payload)
	if !bytes.Equal(after, want) {
		t.Fatal("logging mutated request map")
	}
}

func TestOWAPayloadNativeMIMEIsOpaqueAndWireUnchanged(t *testing.T) {
	raw := []byte("X-Session: session-access-secret\r\nAuthorization: Bearer unknown-native-secret\r\n\r\nprivate body")
	encoded := base64.StdEncoding.EncodeToString(raw)
	var buf owaLogBuffer
	lg := zerolog.New(&buf).Level(zerolog.TraceLevel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Action") != "GetItem" {
			t.Errorf("action changed: %s", r.Header.Get("Action"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(itemResp(map[string]any{"MimeContent": map[string]any{"CharacterSet": "UTF-8", "Value": encoded}}))
	}))
	defer srv.Close()
	p := Factory{BaseURL: srv.URL, Logger: &lg}.New("user@example.com", bridge.Session{AccessToken: "session-access-secret"})
	got, err := p.GetMessageMIME(context.Background(), "inbox", "message")
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("native MIME bytes changed: %q %v", got, err)
	}
	out := buf.String()
	if len(owaTraceRecords(t, out)) != 2 || !strings.Contains(out, "OMITTED: opaque MIME/file content") || strings.Contains(out, encoded) || strings.Contains(out, "session-access-secret") || strings.Contains(out, "unknown-native-secret") {
		t.Fatalf("unsafe native MIME trace: %s", out)
	}
}

func TestOWAPayloadStreamingReadAndLimitParity(t *testing.T) {
	for _, tc := range []struct {
		name, action  string
		contentLength int64
		reader        func() io.Reader
		wantErr       bool
	}{
		{"streaming", "Test", -1, func() io.Reader {
			return io.MultiReader(strings.NewReader(`{"Body":{}}`), io.LimitReader(attachmentPaddingReader{}, 1<<20))
		}, false},
		{"content-length", "GetAttachment", maxTaskAttachmentResponseBytes + 1, func() io.Reader { return strings.NewReader(`{"Body":{}}`) }, true},
		{"bounded", "GetAttachment", -1, func() io.Reader {
			return io.MultiReader(strings.NewReader(`{"Body":{"padding":"`), io.LimitReader(attachmentPaddingReader{}, maxTaskAttachmentResponseBytes+4096), strings.NewReader(`"}}`))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var baselineBytes int64
			var baselineReads int
			for i, mode := range []string{"nil", "debug", "trace"} {
				var buf owaLogBuffer
				var logger *zerolog.Logger
				if mode != "nil" {
					lg := zerolog.New(&buf).Level(zerolog.DebugLevel)
					if mode == "trace" {
						lg = lg.Level(zerolog.TraceLevel)
					}
					logger = &lg
				}
				body := &attachmentCountingReader{Reader: tc.reader()}
				p := Factory{BaseURL: "https://owa.invalid/", Logger: logger, HTTP: &http.Client{Transport: attachmentTestTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, ContentLength: tc.contentLength, Body: io.NopCloser(body), Header: make(http.Header)}, nil
				})}}.New("user@example.com", bridge.Session{}).(*provider)
				_, err := p.call(context.Background(), tc.action, map[string]any{})
				if (err != nil) != tc.wantErr {
					t.Fatalf("%s error changed: %v", mode, err)
				}
				if i == 0 {
					baselineBytes, baselineReads = body.bytes, body.reads
				} else if body.bytes != baselineBytes || body.reads != baselineReads {
					t.Fatalf("trace changed reads: %s bytes=%d reads=%d baseline=%d/%d", mode, body.bytes, body.reads, baselineBytes, baselineReads)
				}
				if tc.name == "streaming" && body.bytes >= 1<<20 {
					t.Fatalf("response drained: %d", body.bytes)
				}
				if tc.name == "content-length" && body.reads != 0 {
					t.Fatalf("oversize body read: %d", body.reads)
				}
				if tc.name == "bounded" && (body.bytes != maxTaskAttachmentResponseBytes+1 || !strings.Contains(err.Error(), "exceeds size limit")) {
					t.Fatalf("wire limit changed: bytes=%d err=%v", body.bytes, err)
				}
				if mode == "trace" {
					records := owaTraceRecords(t, buf.String())
					if len(records) != 2 || records[1]["payload"].(map[string]any)["captured_bytes"] != float64(body.bytes) {
						t.Fatalf("capture reads changed: %s", buf.String())
					}
				}
			}
		})
	}
}

func TestOWAPayloadConcurrentCorrelation(t *testing.T) {
	var buf owaLogBuffer
	lg := zerolog.New(&buf).Level(zerolog.TraceLevel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer srv.Close()
	p := Factory{BaseURL: srv.URL, Logger: &lg}.New("user@example.com", bridge.Session{}).(*provider)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := p.call(context.Background(), "Test", map[string]any{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	records := owaTraceRecords(t, buf.String())
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

func TestOWAPayloadNetworkFailureTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	var buf owaLogBuffer
	lg := zerolog.New(&buf).Level(zerolog.TraceLevel)
	p := Factory{BaseURL: srv.URL, Logger: &lg}.New("user@example.com", bridge.Session{}).(*provider)
	if _, err := p.call(context.Background(), "Test", map[string]any{}); err == nil || !bridge.IsTransient(err) {
		t.Fatalf("network error changed: %v", err)
	}
	records := owaTraceRecords(t, buf.String())
	if len(records) != 2 || records[1]["status"] != float64(0) {
		t.Fatalf("missing network trace: %s", buf.String())
	}
}
