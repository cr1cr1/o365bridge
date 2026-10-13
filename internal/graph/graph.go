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

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

const defaultBaseURL = "https://graph.microsoft.com"

// Factory builds Graph providers. Zero value is usable.
type Factory struct {
	BaseURL string
	HTTP    *http.Client
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
	return &provider{email: email, session: s, baseURL: strings.TrimRight(base, "/"), http: client}
}

var _ bridge.Factory = Factory{}
var _ bridge.Factory = (*Factory)(nil)

type provider struct {
	email   string
	session bridge.Session
	baseURL string
	http    *http.Client
}

type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

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
	resp, err := p.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, bridge.Wrap(bridge.KindTransient, "graph: network error", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, bridge.Wrap(bridge.KindTransient, "graph: read response", err)
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
		return nil, bridge.Wrap(bridge.ClassifyHTTP(resp.StatusCode, code), detail, fmt.Errorf("%s", cause))
	}
	return data, nil
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

func toGraphDT(t time.Time, tz string) dateTimeTZ {
	loc := time.UTC
	name := "UTC"
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc, name = l, tz
		}
	}
	return dateTimeTZ{DateTime: t.In(loc).Format("2006-01-02T15:04:05.0000000"), TimeZone: name}
}

func fromGraphDT(d dateTimeTZ) time.Time {
	if d.DateTime == "" {
		return time.Time{}
	}
	loc := time.UTC
	if d.TimeZone != "" {
		if l, err := time.LoadLocation(d.TimeZone); err == nil {
			loc = l
		}
	}
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
