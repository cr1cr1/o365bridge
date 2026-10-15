package upstreamlog

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestPayloadRedaction(t *testing.T) {
	session := bridge.Session{AccessToken: "access-secret", RefreshToken: "refresh-secret", OWAAuthHeader: "Bearer bare-secret", OWACookies: map[string]string{"session": "cookie-secret", "X-OWA-CANARY": "canary-secret"}}
	body := []byte(`{"subject":"private mail","nested":[{"access_token":"unknown-secret","Authorization":"Bearer unknown-bearer","cookie":"unknown-cookie","refreshToken":"unknown-refresh","canary":"unknown-canary"}],"echo":"access-secret refresh-secret bare-secret cookie-secret canary-secret","url":"https://user:password-secret@example.com/path","encoded":"` + base64.StdEncoding.EncodeToString([]byte("access-secret")) + `"}`)
	p := Payload(body, int64(len(body)), false, "application/json", session)
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "private mail") {
		t.Fatalf("personal content omitted: %s", out)
	}
	for _, s := range []string{"access-secret", "refresh-secret", "bare-secret", "cookie-secret", "canary-secret", "unknown-secret", "unknown-bearer", "unknown-cookie", "unknown-refresh", "unknown-canary", "password-secret", base64.StdEncoding.EncodeToString([]byte("access-secret"))} {
		if strings.Contains(string(out), s) {
			t.Fatalf("secret %q leaked: %s", s, out)
		}
	}
}

func TestPayloadFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        []byte
		incomplete  bool
		contentType string
	}{
		{"malformed", []byte(`{"secret":"unsafe-prefix","access_token":"unknown"`), false, "application/json"},
		{"cutoff", []byte(`{"text":"` + strings.Repeat("x", CaptureLimit) + `","access_token":"cutoff-secret"}`), false, "application/json"},
		{"partial-valid", []byte(`{"text":"unsafe-prefix"}`), true, "application/json"},
		{"mime", []byte("Subject: unsafe-prefix\r\nAuthorization: Bearer unknown\r\n\r\nbody"), false, "message/rfc822"},
		{"base64", []byte(base64.StdEncoding.EncodeToString([]byte("unsafe-prefix"))), false, "text/plain"},
		{"binary", []byte{0, 1, 255, 2}, false, "application/octet-stream"},
		{"escaping-expansion", []byte(`{"text":"` + strings.Repeat("&", 10<<10) + `"}`), false, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := json.Marshal(Payload(tc.body, int64(len(tc.body)), tc.incomplete, tc.contentType, bridge.Session{}))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), `"omitted"`) || !strings.Contains(string(out), `"captured_bytes"`) || strings.Contains(string(out), "unsafe-prefix") || strings.Contains(string(out), "cutoff-secret") {
				t.Fatalf("unsafe omission: %s", out)
			}
			if len(out) > 64<<10 {
				t.Fatalf("unbounded payload: %d", len(out))
			}
		})
	}
}

func TestCredentialKeysPreservedAndEscapedSecretsRedacted(t *testing.T) {
	s := bridge.Session{AccessToken: "a+b/c=token", RefreshToken: "refresh/secret+"}
	data := []byte(`{"AccessToken":"unknown-a","Authorization":"unknown-b","Cookie":"unknown-c","echo":"` + url.QueryEscape(s.AccessToken) + ` ` + url.PathEscape(s.RefreshToken) + `"}`)
	out, _ := json.Marshal(Payload(data, int64(len(data)), false, "application/json", s))
	var record struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out, &record); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"AccessToken", "Authorization", "Cookie"} {
		if record.Data[key] != "[REDACTED]" {
			t.Fatalf("credential key %q collapsed: %s", key, out)
		}
	}
	for _, secret := range []string{url.QueryEscape(s.AccessToken), url.PathEscape(s.RefreshToken)} {
		if strings.Contains(string(out), secret) || strings.Contains(Text("GET https://example.test/a?value="+secret, s), secret) {
			t.Fatalf("escaped secret leaked: %s", out)
		}
	}
}

func TestEscapedSessionSecretsFailClosed(t *testing.T) {
	s := bridge.Session{AccessToken: "a+b/c=secret"}
	for _, encoded := range []string{"a%2bb%2fc%3dsecret", "%61%2Bb%2Fc%3Dsecret", "a%2Bb%2fc%3d%73ecret", "100% failed: a%2bb%2fc%3dsecret"} {
		text := "GET https://example.test/?value=" + encoded
		if got := Text(text, s); got == text || strings.Contains(got, encoded) {
			t.Fatalf("escaped session secret leaked: %q", got)
		}
		data := []byte(`{"echo":"` + encoded + `"}`)
		out, _ := json.Marshal(Payload(data, int64(len(data)), false, "application/json", s))
		if strings.Contains(string(out), encoded) {
			t.Fatalf("escaped payload secret leaked: %s", out)
		}
	}
}

func TestOpaqueNestedPayloadsOmittedWithoutLosingNotes(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("X-Session: access-secret\r\nAuthorization: Bearer unknown-secret\r\n\r\nbody"))
	body := []byte(`{"MimeContent":{"CharacterSet":"UTF-8","Value":"` + encoded + `"},"Attachments":[{"__type":"FileAttachment:#Exchange","Content":"` + encoded + `"},{"@odata.type":"#microsoft.graph.fileAttachment","contentBytes":"` + encoded + `"}],"Body":{"Content":"personal note","Value":"personal OWA note"}}`)
	out, _ := json.Marshal(Payload(body, int64(len(body)), false, "application/json", bridge.Session{AccessToken: "access-secret"}))
	if strings.Contains(string(out), encoded) || !strings.Contains(string(out), "OMITTED") || !strings.Contains(string(out), "personal note") || !strings.Contains(string(out), "personal OWA note") {
		t.Fatalf("opaque nested safety/notes: %s", out)
	}
}

func TestCaptureBoundedWithoutChangingWrites(t *testing.T) {
	var c Capture
	data := []byte(strings.Repeat("x", CaptureLimit+400))
	n, err := c.Write(data)
	if n != len(data) || err != nil || len(c.Data) != CaptureLimit || c.Bytes != int64(len(data)) {
		t.Fatalf("bad capture: n=%d err=%v retained=%d observed=%d", n, err, len(c.Data), c.Bytes)
	}
}

func TestURLUserinfoRedactedAcrossSchemesAndRelativeAuthorities(t *testing.T) {
	for _, address := range []string{"ftp://user:userinfo-secret@example.test/file", "dav+tls://user:userinfo-secret@example.test/file", "//user:userinfo-secret@example.test/file"} {
		if out := Text(address, bridge.Session{}); strings.Contains(out, "userinfo-secret") {
			t.Fatalf("summary userinfo leaked: %s", out)
		}
		body, _ := json.Marshal(map[string]any{"url": address})
		out, _ := json.Marshal(Payload(body, int64(len(body)), false, "application/json", bridge.Session{}))
		if strings.Contains(string(out), "userinfo-secret") {
			t.Fatalf("payload userinfo leaked: %s", out)
		}
	}
}

func TestTextSanitizesBeforeBounding(t *testing.T) {
	s := bridge.Session{AccessToken: "token-secret", OWAAuthHeader: "Bearer bare-secret"}
	for _, text := range []string{"boom token-secret bare-secret", "Authorization: Basic unknown-secret", `{"nested":{"password":"unknown-secret"}}`, `bad {"access_token":"unknown-secret`, "https://user:url-secret@example.com/path", "Bearer unknown-secret", strings.Repeat("x", 10<<10) + "token-secret"} {
		out := Text(text, s)
		for _, secret := range []string{"token-secret", "bare-secret", "unknown-secret", "url-secret"} {
			if strings.Contains(out, secret) {
				t.Fatalf("secret %q in %q", secret, out)
			}
		}
		if len(out) > 1024 {
			t.Fatalf("unbounded summary: %d", len(out))
		}
	}
}
