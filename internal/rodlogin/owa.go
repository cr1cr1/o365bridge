package rodlogin

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

const owaStartURL = "https://outlook.cloud.microsoft/mail/"

// owaHosts are the hosts whose authenticated traffic carries the OWA
// session.
var owaHosts = map[string]bool{
	"outlook.office.com":      true,
	"outlook.cloud.microsoft": true,
	"outlook.office365.com":   true,
}

// errOWANotCaptured means a silent OWA attempt saw no authenticated
// traffic before its deadline.
var errOWANotCaptured = errors.New("rodlogin: no authenticated OWA traffic captured")

// cookieKV is a name/value/domain cookie triple (the seam between the
// browser layer, which deals in rod proto cookies, and the pure
// session-mapping logic).
type cookieKV struct {
	Name   string
	Value  string
	Domain string
}

type owaCaptureArgs struct {
	ProfileDir string
	StartURL   string
	Headless   bool
}

type owaCaptureResult struct {
	Header  string // verbatim Authorization header, e.g. "Bearer ..."
	Host    string // host that served the authenticated traffic
	Cookies []cookieKV
}

// isOWAHost reports whether host (with an optional leading dot, as
// cookie domains carry) is an OWA host.
func isOWAHost(host string) bool {
	return owaHosts[strings.TrimPrefix(strings.ToLower(host), ".")]
}

// captureFromRequest decides whether an outgoing request counts as
// authenticated OWA traffic: an Authorization header (any case) on a
// request to an OWA host. Returns the header verbatim and the host.
func captureFromRequest(rawURL string, headers map[string]string) (header, host string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !isOWAHost(u.Hostname()) {
		return "", "", false
	}
	for name, value := range headers {
		if strings.EqualFold(name, "Authorization") && strings.TrimSpace(value) != "" {
			return value, u.Hostname(), true
		}
	}
	return "", "", false
}

// isTokenEndpointURL reports whether rawURL is a Microsoft token
// endpoint (whose JSON responses may carry an access_token).
func isTokenEndpointURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "login.microsoftonline.com") &&
		strings.HasSuffix(u.Path, "/oauth2/v2.0/token")
}

// isTokenFormForOWA mirrors the web client's token-form gate: a token
// request counts when it is an authorization_code grant or asks for
// the OWA scope.
func isTokenFormForOWA(postData string) bool {
	v, err := url.ParseQuery(postData)
	if err != nil {
		return strings.Contains(postData, "https://outlook.office.com/.default")
	}
	if v.Get("grant_type") == "authorization_code" {
		return true
	}
	for _, s := range strings.Fields(v.Get("scope")) {
		if s == "https://outlook.office.com/.default" {
			return true
		}
	}
	return false
}

// owaHeaderFromTokenBody converts a token-endpoint JSON response body
// into the OWA Authorization header ("Bearer <access_token>").
func owaHeaderFromTokenBody(body []byte) (string, bool) {
	var p struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.AccessToken == "" {
		return "", false
	}
	return "Bearer " + p.AccessToken, true
}

func parseHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// buildOWASession maps a captured header plus browser cookies into a
// Session. Every cookie belonging to an OWA host is stored (keyed by
// name) so the canary (X-OWA-CANARY / cadata family) is present when
// the browser had it.
func buildOWASession(email, header, host string, cookies []cookieKV) bridge.Session {
	out := map[string]string{}
	for _, c := range cookies {
		if c.Name == "" || c.Value == "" || !isOWAHost(c.Domain) {
			continue
		}
		out[c.Name] = c.Value
	}
	return bridge.Session{
		Email:         bridge.NormalizeEmail(email),
		Channel:       bridge.ChannelOWA,
		OWAAuthHeader: header,
		OWACookies:    out,
		OWAHost:       host,
	}
}
