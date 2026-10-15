// Package graph implements bridge.Provider against Microsoft Graph v1.0.
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/upstreamlog"
)

const defaultBaseURL = "https://graph.microsoft.com"

// Factory builds Graph providers. Zero value is usable.
type Factory struct {
	BaseURL string
	HTTP    *http.Client
	// Logger receives one debug line per upstream Graph call
	// (method, resource, status, duration), trace-only sanitized
	// payload records, and a warning on failure. Nil disables logging.
	Logger *zerolog.Logger
}

// New implements bridge.Factory.
func (f Factory) New(email string, s bridge.Session) bridge.Provider {
	base := f.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	client := f.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	log := zerolog.Nop()
	if f.Logger != nil {
		log = *f.Logger
	}
	return &provider{email: email, session: s, baseURL: strings.TrimRight(base, "/"), http: client, log: log}
}

var _ bridge.Factory = Factory{}
var _ bridge.Factory = (*Factory)(nil)

type provider struct {
	email   string
	session bridge.Session
	baseURL string
	http    *http.Client
	log     zerolog.Logger
}

type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type graphHTTPError struct {
	statusCode int
	message    string
}

func (e *graphHTTPError) Error() string { return e.message }

// do performs a JSON request. resource is "/me/..." (v1.0 is prepended) or
// an absolute URL (pagination links). Returns raw body on 2xx.
func (p *provider) do(ctx context.Context, method, resource string, body any, extraHeaders map[string]string) ([]byte, error) {
	var payload []byte
	contentType := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, bridge.Wrap(bridge.KindOther, "graph: marshal request", err)
		}
		payload, contentType = data, "application/json"
	}
	return p.doBytes(ctx, method, resource, payload, contentType, extraHeaders)
}

// doBytes performs a request with a pre-encoded payload (raw-MIME endpoints
// take base64 text/plain instead of JSON).
func (p *provider) doBytes(ctx context.Context, method, resource string, payload []byte, contentType string, extraHeaders map[string]string) ([]byte, error) {
	return p.doBytesLimit(ctx, method, resource, payload, contentType, extraHeaders, 0)
}

// doBytesLimit shares authentication and error handling with normal Graph calls.
func (p *provider) doBytesLimit(ctx context.Context, method, resource string, payload []byte, contentType string, extraHeaders map[string]string, maxBytes int64) ([]byte, error) {
	if p.session.AccessToken == "" {
		return nil, bridge.AuthError("graph: missing access token")
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	u := resource
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = p.baseURL + "/v1.0" + resource
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, bridge.Wrap(bridge.KindOther, "graph: build request", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.session.AccessToken)
	// Immutable ids survive moves; large pages reduce round-trips.
	req.Header.Set("Prefer", `odata.maxpagesize=500, IdType="ImmutableId"`)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	trace := p.log.Trace().Enabled()
	var requestID uint64
	if trace {
		requestID = upstreamlog.NextID()
		p.logPayload(method, resource, requestID, "request", 0, payload, contentType, false)
	}
	start := time.Now()
	resp, err := p.http.Do(req)
	if err != nil {
		if trace {
			p.logPayload(method, resource, requestID, "response", 0, nil, "", true)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		p.logUpstream(zerolog.WarnLevel, method, resource, 0, time.Since(start), err.Error())
		return nil, bridge.Wrap(bridge.KindTransient, "graph: network error", err)
	}
	defer resp.Body.Close()
	var responseBody io.Reader = resp.Body
	if maxBytes > 0 {
		responseBody = io.LimitReader(resp.Body, maxBytes+1)
	}
	data, err := io.ReadAll(responseBody)
	dur := time.Since(start)
	if trace {
		incomplete := err != nil || maxBytes > 0 && int64(len(data)) > maxBytes
		p.logPayload(method, resource, requestID, "response", resp.StatusCode, data, resp.Header.Get("Content-Type"), incomplete)
	}
	if err != nil {
		p.logUpstream(zerolog.WarnLevel, method, resource, resp.StatusCode, dur, "read response: "+err.Error())
		return nil, bridge.Wrap(bridge.KindTransient, "graph: read response", err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		p.logUpstream(zerolog.WarnLevel, method, resource, resp.StatusCode, dur, "response exceeds download limit")
		return nil, bridge.Wrap(bridge.KindOther, "graph: task attachment exceeds download limit", nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code, msg := "", ""
		var ge graphError
		if json.Unmarshal(data, &ge) == nil {
			code, msg = ge.Error.Code, ge.Error.Message
		}
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		detail := fmt.Sprintf("graph: %s %s: status %d", method, resource, resp.StatusCode)
		cause := msg
		if code != "" {
			cause = "code=" + code + ": " + msg
		}
		p.logUpstream(zerolog.WarnLevel, method, resource, resp.StatusCode, dur, cause)
		return nil, bridge.Wrap(bridge.ClassifyHTTP(resp.StatusCode, code), detail, &graphHTTPError{statusCode: resp.StatusCode, message: cause})
	}
	p.logUpstream(zerolog.DebugLevel, method, resource, resp.StatusCode, dur, "")
	return data, nil
}

// logUpstream emits one line per upstream Graph round trip: duration,
// method, resource and status per account; failures as warnings.
// Credentials are redacted; payload diagnostics have separate trace records.
func (p *provider) logUpstream(level zerolog.Level, method, resource string, status int, dur time.Duration, errMsg string) {
	ev := p.log.WithLevel(level)
	if !ev.Enabled() {
		return
	}
	ev.Str("channel", "graph").Str("op", "http").Str("method", method).
		Str("resource", upstreamlog.Text(resource, p.session)).Str("account", upstreamlog.Text(p.email, p.session)).Int("status", status).
		Str("duration", dur.String())
	if errMsg != "" {
		ev.Str("error", upstreamlog.Text(errMsg, p.session)).Msg("graph upstream request failed")
		return
	}
	ev.Msg("graph upstream request")
}

// logPayload is called only when trace is enabled, using already-buffered bytes.
func (p *provider) logPayload(method, resource string, requestID uint64, direction string, status int, data []byte, contentType string, incomplete bool) {
	p.log.Trace().Str("channel", "graph").Str("op", "http").Str("method", method).
		Str("resource", upstreamlog.Text(resource, p.session)).Str("account", upstreamlog.Text(p.email, p.session)).
		Uint64("request_id", requestID).Str("direction", direction).Int("status", status).
		Interface("payload", upstreamlog.Payload(data, int64(len(data)), incomplete, contentType, p.session)).
		Msg("graph upstream payload")
}

func (p *provider) doJSON(ctx context.Context, method, resource string, body, out any) error {
	return p.doJSONH(ctx, method, resource, body, out, nil)
}

func (p *provider) doJSONH(ctx context.Context, method, resource string, body, out any, headers map[string]string) error {
	data, err := p.do(ctx, method, resource, body, headers)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return bridge.Wrap(bridge.KindOther, "graph: decode response", err)
	}
	return nil
}

type page[T any] struct {
	Count    *int   `json:"@odata.count"`
	Value    []T    `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

const maxPages = 100

// getAll follows @odata.nextLink until exhausted or limit items collected
// (limit <= 0 means unbounded). Returns items and @odata.count if seen.
func getAll[T any](ctx context.Context, p *provider, resource string, limit int, headers map[string]string) ([]T, int, error) {
	var all []T
	total := -1
	next := resource
	for pageNum := 0; next != "" && pageNum < maxPages; pageNum++ {
		data, err := p.do(ctx, http.MethodGet, next, nil, headers)
		if err != nil {
			return nil, 0, err
		}
		var pg page[T]
		if err := json.Unmarshal(data, &pg); err != nil {
			return nil, 0, bridge.Wrap(bridge.KindOther, "graph: decode page", err)
		}
		if pg.Count != nil {
			total = *pg.Count
		}
		all = append(all, pg.Value...)
		if limit > 0 && len(all) >= limit {
			all = all[:limit]
			break
		}
		next = pg.NextLink
	}
	if total < 0 {
		total = len(all)
	}
	return all, total, nil
}

func esc(s string) string { return url.PathEscape(s) }

// --- Graph JSON helpers ---

type dateTimeTZ struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type emailAddress struct {
	Address string `json:"address,omitempty"`
	Name    string `json:"name,omitempty"`
}
type emailAddressWrapper struct {
	EmailAddress emailAddress `json:"emailAddress"`
}

// zoneLocation resolves an IANA or Windows zone name to a Go
// location (Graph returns Windows names for Outlook-created items;
// loading those as IANA silently fell back to UTC before).
func zoneLocation(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	if l, err := time.LoadLocation(bridge.FromWindowsZone(name)); err == nil {
		return l
	}
	if l, err := time.LoadLocation(name); err == nil {
		return l
	}
	return time.UTC
}

func toGraphDT(t time.Time, tz string) dateTimeTZ {
	loc := zoneLocation(tz)
	name := "UTC"
	if tz != "" {
		// Send the Windows name: Graph accepts it for every zone,
		// including ones whose IANA name it would reject.
		name = bridge.ToWindowsZone(tz)
		if name == tz && loc == time.UTC && tz != "UTC" {
			name = "UTC"
		}
	}
	return dateTimeTZ{DateTime: t.In(loc).Format("2006-01-02T15:04:05.0000000"), TimeZone: name}
}

func fromGraphDT(d dateTimeTZ) time.Time {
	if d.DateTime == "" {
		return time.Time{}
	}
	loc := zoneLocation(d.TimeZone)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, d.DateTime, loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

func parseGraphTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999Z07:00", "2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func addrList(ws []emailAddressWrapper) []string {
	var out []string
	for _, w := range ws {
		if w.EmailAddress.Address != "" {
			out = append(out, w.EmailAddress.Address)
		}
	}
	return out
}
