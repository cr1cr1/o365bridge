// Package owa implements bridge.Provider against Outlook Web App's
// JSON service (service.svc), mirroring the request/response shapes
// the reference OWA integration uses against service.svc.
package owa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type Factory struct {
	BaseURL string
	HTTP    *http.Client
}

func (f Factory) New(email string, s bridge.Session) bridge.Provider {
	base := f.BaseURL
	if base == "" {
		host := s.OWAHost
		if host == "" {
			host = "outlook.cloud.microsoft"
		}
		base = "https://" + host + "/owa/"
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	c := f.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	return &provider{email: email, sess: s, base: base, http: c}
}

type provider struct {
	email string
	sess  bridge.Session
	base  string
	http  *http.Client
}

// --- transport ---

func hdr() map[string]any {
	return map[string]any{"__type": "JsonRequestHeaders:#Exchange", "RequestServerVersion": "Exchange2013"}
}

// envelope builds {__type, Header, Body:{__type, ...fields}}
func envelope(reqType, bodyType string, fields map[string]any) map[string]any {
	body := map[string]any{"__type": bodyType}
	for k, v := range fields {
		body[k] = v
	}
	return map[string]any{"__type": reqType, "Header": hdr(), "Body": body}
}

func (p *provider) call(ctx context.Context, action string, payload any) (any, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"service.svc", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Action", action)
	req.Header.Set("Content-Type", "application/json")
	if p.sess.OWAAuthHeader != "" {
		req.Header.Set("Authorization", p.sess.OWAAuthHeader)
	} else if canary := p.sess.OWACookies["X-OWA-CANARY"]; canary != "" {
		req.Header.Set("X-OWA-CANARY", canary)
	} else if canary := p.sess.OWACookies["canary"]; canary != "" {
		req.Header.Set("X-OWA-CANARY", canary)
	}
	if len(p.sess.OWACookies) > 0 {
		keys := make([]string, 0, len(p.sess.OWACookies))
		for k := range p.sess.OWACookies {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+p.sess.OWACookies[k])
		}
		req.Header.Set("Cookie", strings.Join(parts, "; "))
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, bridge.Wrap(bridge.KindTransient, "owa: "+action, err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	decErr := json.NewDecoder(resp.Body).Decode(&raw)
	if resp.StatusCode == 440 {
		return nil, bridge.Wrap(bridge.KindAuth, fmt.Sprintf("owa: %s: HTTP 440 login expired", action), nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		kind := bridge.ClassifyHTTP(resp.StatusCode, "")
		return nil, bridge.Wrap(kind, fmt.Sprintf("owa: %s: HTTP %d", action, resp.StatusCode), nil)
	}
	if decErr != nil {
		return nil, bridge.Wrap(bridge.KindTransient, "owa: "+action+": decode response", decErr)
	}
	var result any = raw
	if body, ok := raw["Body"]; ok {
		result = body
	}
	if m, ok := result.(map[string]any); ok {
		if items := arr(m, "ResponseMessages", "Items"); len(items) == 1 {
			result = items[0]
		}
	}
	if m, ok := result.(map[string]any); ok {
		if mt := str(m, "MessageText"); mt != "" {
			return nil, fmt.Errorf("owa: %s: %s", action, mt)
		}
	}
	return result, nil
}

// --- tolerant JSON navigation helpers ---

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
func dig(v any, keys ...string) any {
	cur := v
	for _, k := range keys {
		m := asMap(cur)
		if m == nil {
			return nil
		}
		cur = m[k]
	}
	return cur
}
func str(v any, keys ...string) string {
	if s, ok := dig(v, keys...).(string); ok {
		return s
	}
	return ""
}
func num(v any, keys ...string) float64 {
	if n, ok := dig(v, keys...).(float64); ok {
		return n
	}
	return 0
}
func boolean(v any, keys ...string) bool {
	b, _ := dig(v, keys...).(bool)
	return b
}

// arr accepts object-or-array values.
func arr(v any, keys ...string) []any {
	switch x := dig(v, keys...).(type) {
	case []any:
		return x
	case map[string]any:
		return []any{x}
	case nil:
		return nil
	default:
		return []any{x}
	}
}
func firstItem(v any) map[string]any {
	items := arr(v, "Items")
	if len(items) == 0 {
		// ResponseMessages not unwrapped (multi-item)
		if rms := arr(v, "ResponseMessages", "Items"); len(rms) > 0 {
			if sub := arr(rms[0], "Items"); len(sub) > 0 {
				return asMap(sub[0])
			}
		}
		return nil
	}
	return asMap(items[0])
}
func allItems(v any) []map[string]any {
	var out []map[string]any
	for _, it := range arr(v, "Items") {
		if m := asMap(it); m != nil {
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, rm := range arr(v, "ResponseMessages", "Items") {
		for _, it := range arr(rm, "Items") {
			if m := asMap(it); m != nil {
				out = append(out, m)
			}
		}
	}
	return out
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04:05.9999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	// FreeBusy gives zulu-local without Z
	if t, err := time.Parse("2006-01-02T15:04:05", strings.TrimSuffix(s, "Z")); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

var distinguished = map[string]bool{
	"msgfolderroot": true, "inbox": true, "sentitems": true, "drafts": true,
	"deleteditems": true, "junkemail": true, "outbox": true, "calendar": true,
	"tasks": true, "contacts": true, "publicfoldersroot": true, "directory": true,
	"archivemsgfolderroot": true, "archive": true,
}

func folderRef(id string) map[string]any {
	if distinguished[strings.ToLower(id)] {
		return map[string]any{"__type": "DistinguishedFolderId:#Exchange", "Id": id}
	}
	return map[string]any{"__type": "FolderId:#Exchange", "Id": id}
}
func targetFolder(id string) map[string]any {
	return map[string]any{"__type": "TargetFolderId:#Exchange", "BaseFolderId": folderRef(id)}
}
func itemID(id string) map[string]any { return map[string]any{"__type": "ItemId:#Exchange", "Id": id} }
func itemIDs(ids []string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, itemID(id))
	}
	return out
}
func propURI(uri string) map[string]any {
	return map[string]any{"__type": "PropertyUri:#Exchange", "FieldURI": uri}
}
func mailbox(email, name string) map[string]any {
	return map[string]any{"Name": name, "EmailAddress": email}
}
func mailboxEmail(v any) string {
	// v may be {Mailbox:{EmailAddress}} or {EmailAddress} or attendee {Mailbox:...}
	if e := str(v, "Mailbox", "EmailAddress"); e != "" {
		return e
	}
	return str(v, "EmailAddress")
}
func recipients(v any, key string) []string {
	var out []string
	for _, r := range arr(v, key) {
		if e := mailboxEmail(r); e != "" {
			out = append(out, e)
		} else if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func notSupported(op string) error { return fmt.Errorf("owa: not supported: %s", op) }
