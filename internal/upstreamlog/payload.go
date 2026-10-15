// Package upstreamlog formats diagnostics only; it never changes wire data.
package upstreamlog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

const CaptureLimit = 64 << 10
const outputLimit = 48 << 10

var sequence atomic.Uint64

func NextID() uint64 { return sequence.Add(1) }

// Capture retains a bounded prefix and counts only bytes the caller already read.
// Its Write never interferes with a TeeReader's original read/error behavior.
type Capture struct {
	Data  []byte
	Bytes int64
}

func (c *Capture) Write(p []byte) (int, error) {
	c.Bytes += int64(len(p))
	n := min(len(p), CaptureLimit-len(c.Data))
	c.Data = append(c.Data, p[:n]...)
	return len(p), nil
}

type payload struct {
	CapturedBytes int64           `json:"captured_bytes"`
	Format        string          `json:"format"`
	Data          json.RawMessage `json:"data,omitempty"`
	Omitted       string          `json:"omitted,omitempty"`
	Truncated     bool            `json:"truncated,omitempty"`
}

// Payload fails closed for incomplete/oversized JSON and opaque MIME/base64 or
// binary bodies: a prefix cannot be safely redacted and encoding is not secrecy.
// captured counts observed bytes, not necessarily the full upstream body.
func Payload(data []byte, captured int64, incomplete bool, contentType string, session bridge.Session) any {
	p := payload{CapturedBytes: captured, Format: "json"}
	if incomplete || captured > CaptureLimit || len(data) > CaptureLimit {
		p.Omitted, p.Truncated = "incomplete or capture limit exceeded", true
		return p
	}
	if len(bytes.TrimSpace(data)) == 0 {
		p.Format = "empty"
		return p
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		p.Format, p.Omitted = "opaque", "non-JSON or malformed payload"
		return p
	}
	// A streaming decoder may have read trailing data; do not expose an unsafe prefix.
	if err := dec.Decode(new(any)); err != io.EOF {
		p.Omitted = "non-JSON or malformed payload"
		return p
	}
	// MIME/base64/binary endpoints may contain JSON-looking bytes. Treat explicit
	// non-JSON content types as opaque rather than trusting their apparent shape.
	if contentType != "" && !strings.Contains(strings.ToLower(contentType), "json") {
		p.Format, p.Omitted = "opaque", "non-JSON payload"
		return p
	}
	redactor := newRedactor(session)
	safe, err := json.Marshal(redactor.value(value))
	if err != nil || len(safe) > outputLimit {
		p.Omitted, p.Truncated = "formatted payload limit exceeded", true
		return p
	}
	p.Data = safe
	return p
}

var userinfo = regexp.MustCompile(`((?:[A-Za-z][A-Za-z0-9+.-]*:)?//)[^\s/]+@`)
var bearer = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;"'<>]+`)
var credentialMarker = regexp.MustCompile(`(?i)authorization|cookie|canary|access[_ -]?token|refresh[_ -]?token|owa[_ -]?auth[_ -]?header|password|client[_ -]?secret`)

type redactor struct{ secrets []string }

func newRedactor(s bridge.Session) redactor {
	values := []string{s.AccessToken, s.RefreshToken, s.OWAAuthHeader}
	if fields := strings.Fields(s.OWAAuthHeader); len(fields) == 2 {
		values = append(values, fields[1]) // The bare OWA bearer token is also a secret.
	}
	for _, value := range s.OWACookies {
		values = append(values, value)
	}
	var secrets []string
	for _, value := range values {
		if value == "" {
			continue
		}
		secrets = append(secrets, value, base64.StdEncoding.EncodeToString([]byte(value)), base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	// Replace overlapping values longest first, before any output is shortened.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return redactor{secrets: secrets}
}

func (r redactor) scrub(s string) string {
	// URL escaping is case-insensitive and may encode even unreserved bytes.
	// Omit a credential-bearing escaped field instead of guessing its spelling.
	if strings.Contains(s, "%") {
		for _, unescape := range []func(string) (string, error){url.PathUnescape, url.QueryUnescape} {
			decoded, err := unescape(s)
			if err != nil {
				return "[OMITTED: unsafe percent-escaped text]"
			}
			if decoded == s {
				continue
			}
			if credentialMarker.MatchString(decoded) || userinfo.MatchString(decoded) || bearer.MatchString(decoded) {
				return "[OMITTED: encoded credential-bearing text]"
			}
			for _, secret := range r.secrets {
				if strings.Contains(decoded, secret) {
					return "[OMITTED: encoded session value]"
				}
			}
		}
	}
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	s = userinfo.ReplaceAllString(s, "${1}[REDACTED]@")
	return bearer.ReplaceAllString(s, "Bearer [REDACTED]")
}

func (r redactor) text(s string) string {
	s = r.scrub(s)
	// Free text is not a credential grammar. Omit suspicious strings rather than
	// trying to salvage arbitrary echoed header lines or malformed credential JSON.
	if credentialMarker.MatchString(s) {
		return "[OMITTED: credential-bearing text]"
	}
	return s
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(key))
	return strings.Contains(key, "token") || strings.Contains(key, "authorization") || strings.Contains(key, "cookie") || strings.Contains(key, "canary") || strings.Contains(key, "password") || strings.Contains(key, "secret") || key == "owaauthheader"
}

func (r redactor) value(value any) any {
	switch v := value.(type) {
	case map[string]any:
		fileAttachment := false
		for key, item := range v {
			if key == "__type" || key == "@odata.type" {
				if typ, ok := item.(string); ok && strings.Contains(strings.ToLower(typ), "fileattachment") {
					fileAttachment = true
				}
			}
		}
		safe := make(map[string]any, len(v))
		for key, item := range v {
			// Encoded opaque file/MIME data can hide credentials inside a larger blob.
			// Do not decode file data merely for logging, or omit ordinary note content.
			if strings.EqualFold(key, "MimeContent") || strings.EqualFold(key, "contentBytes") || fileAttachment && strings.EqualFold(key, "Content") {
				safe[r.scrub(key)] = "[OMITTED: opaque MIME/file content]"
			} else if sensitiveKey(key) {
				safe[r.scrub(key)] = "[REDACTED]"
			} else {
				safe[r.scrub(key)] = r.value(item)
			}
		}
		return safe
	case []any:
		for i, item := range v {
			v[i] = r.value(item)
		}
		return v
	case string:
		return r.text(v)
	default:
		return v
	}
}

// Text keeps existing summaries/metadata useful but bounded and credential-safe.
// Payloads belong exclusively in trace records, never in these short fields.
func Text(text string, session bridge.Session) string {
	if len(text) > CaptureLimit {
		return "[OMITTED: text limit exceeded]"
	}
	r := newRedactor(session)
	var value any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if dec.Decode(&value) == nil && dec.Decode(new(any)) == io.EOF {
		safe, err := json.Marshal(r.value(value))
		if err == nil {
			text = string(safe)
		} else {
			return "[OMITTED: unsafe text]"
		}
	} else {
		text = r.text(text)
	}
	if len(text) > 512 {
		return text[:512] + "[TRUNCATED]"
	}
	return text
}
