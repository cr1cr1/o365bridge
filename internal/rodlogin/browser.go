package rodlogin

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// launchBrowser starts Chrome/Chromium with the account profile and
// always returns a kill function via the launcher (callers must close
// the browser and Kill the launcher on every exit path).
func (m *Manager) launchBrowser(profileDir string, headless bool) (*rod.Browser, *launcher.Launcher, error) {
	l := launcher.New().
		UserDataDir(profileDir).
		Headless(headless).
		Leakless(true)
	if m.BrowserPath != "" {
		l.Bin(m.BrowserPath)
	}
	if os.Geteuid() == 0 { // containers/CI often run as root
		l.NoSandbox(true)
	}
	controlURL, err := l.Launch()
	if err != nil {
		return nil, nil, bridge.Wrap(bridge.KindTransient, "rodlogin: launch browser failed", err)
	}
	b := rod.New().ControlURL(controlURL)
	if err := b.Connect(); err != nil {
		l.Kill()
		return nil, nil, bridge.Wrap(bridge.KindTransient, "rodlogin: connect to browser failed", err)
	}
	return b, l, nil
}

func closeBrowser(b *rod.Browser, l *launcher.Launcher) {
	if b != nil {
		_ = b.Close()
	}
	if l != nil {
		l.Kill()
	}
}

// browserGone reports whether the browser/tab set has disappeared
// (the user closed the window).
func browserGone(b *rod.Browser) bool {
	pages, err := b.Pages()
	return err != nil || len(pages) == 0
}

func userCancelled(channel string, ctxErr error) error {
	return fmt.Errorf("user cancelled %s login: %w", channel, ctxErr)
}

func windowClosed(channel string) error {
	return fmt.Errorf("user cancelled %s login: browser window closed by user", channel)
}

// runGraphBrowser drives a headed browser through the Microsoft
// authorize page and returns the authorization code from the redirect
// to the native-client URI. Navigation to the redirect URI is
// intercepted (and answered with an empty 200) so the code is read
// from the request itself; page-URL polling is the fallback.
func (m *Manager) runGraphBrowser(ctx context.Context, profileDir, authURL, expectedState string) (string, error) {
	browser, l, err := m.launchBrowser(profileDir, false)
	if err != nil {
		return "", err
	}
	defer closeBrowser(browser, l)

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return "", bridge.Wrap(bridge.KindTransient, "rodlogin: open login page failed", err)
	}
	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)
	send := func(r result) {
		select {
		case resCh <- r:
		default:
		}
	}

	router := page.HijackRequests()
	if err := router.Add(redirectURI+"*", proto.NetworkResourceTypeDocument, func(h *rod.Hijack) {
		code, err := extractRedirectCode(h.Request.URL().String(), expectedState)
		if err != nil && err != errNotRedirect {
			send(result{err: err})
			return
		}
		if code != "" {
			send(result{code: code})
		}
		// No ContinueRequest: rod answers an empty 200, stopping
		// navigation at the redirect URI.
	}); err == nil {
		go router.Run()
		defer func() { _ = router.Stop() }()
	}

	if err := page.Navigate(authURL); err != nil {
		// Navigation errors are expected when the redirect is
		// intercepted; the poll loop decides the outcome.
		m.logger.Debug().Err(err).Msg("graph login navigation returned early")
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case r := <-resCh:
			return r.code, r.err
		case <-ctx.Done():
			return "", userCancelled("graph", ctx.Err())
		case <-ticker.C:
			if browserGone(browser) {
				// One last chance: a code may have raced the close.
				select {
				case r := <-resCh:
					return r.code, r.err
				default:
				}
				return "", windowClosed("graph")
			}
			if info, err := page.Info(); err == nil {
				code, err := extractRedirectCode(info.URL, expectedState)
				if err == nil && code != "" {
					return code, nil
				}
				if err != nil && err != errNotRedirect {
					return "", err
				}
			}
		}
	}
}

// runOWABrowser opens OWA in the account profile and waits for
// authenticated traffic: an Authorization header on a request to an
// OWA host, or an access_token in a token-endpoint response (the reference
// checkAuthHeader / filterResponseData). Interactive attempts end only
// on capture, ctx cancellation, or the window being closed; silent
// attempts are bounded by the caller's ctx deadline and return
// errOWANotCaptured.
func (m *Manager) runOWABrowser(ctx context.Context, args owaCaptureArgs) (owaCaptureResult, error) {
	browser, l, err := m.launchBrowser(args.ProfileDir, args.Headless)
	if err != nil {
		return owaCaptureResult{}, err
	}
	defer closeBrowser(browser, l)

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return owaCaptureResult{}, bridge.Wrap(bridge.KindTransient, "rodlogin: open OWA page failed", err)
	}

	captured := make(chan owaCaptureResult, 1)
	send := func(r owaCaptureResult) {
		select {
		case captured <- r:
		default:
		}
	}

	// Token-response watching needs request bookkeeping.
	tokenReqs := map[proto.NetworkRequestID]bool{}

	waitEvents := page.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			headers := map[string]string{}
			for name, v := range e.Request.Headers {
				headers[name] = v.String()
			}
			if header, host, ok := captureFromRequest(e.Request.URL, headers); ok {
				send(owaCaptureResult{Header: header, Host: host})
				return
			}
			if isTokenEndpointURL(e.Request.URL) && isTokenFormForOWA(e.Request.PostData) {
				tokenReqs[e.RequestID] = true
			}
		},
		func(e *proto.NetworkLoadingFinished) {
			if !tokenReqs[e.RequestID] {
				return
			}
			delete(tokenReqs, e.RequestID)
			body, err := proto.NetworkGetResponseBody{RequestID: e.RequestID}.Call(page)
			if err != nil {
				return
			}
			if header, ok := owaHeaderFromTokenBody([]byte(body.Body)); ok {
				// The token is for the OWA host the app is talking
				// to; report the start host unless we learn better
				// from a later request header.
				send(owaCaptureResult{Header: header, Host: hostOf(args.StartURL)})
			}
		},
	)
	go waitEvents()

	if err := page.Navigate(args.StartURL); err != nil {
		m.logger.Debug().Err(err).Msg("owa navigation returned early")
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case r := <-captured:
			cookies, host := m.collectCookies(browser, r.Host)
			r.Cookies = cookies
			if host != "" {
				r.Host = host
			}
			return r, nil
		case <-ctx.Done():
			if args.Headless {
				return owaCaptureResult{}, errOWANotCaptured
			}
			return owaCaptureResult{}, userCancelled("owa", ctx.Err())
		case <-ticker.C:
			if !args.Headless && browserGone(browser) {
				select {
				case r := <-captured:
					cookies, host := m.collectCookies(browser, r.Host)
					r.Cookies = cookies
					if host != "" {
						r.Host = host
					}
					return r, nil
				default:
				}
				return owaCaptureResult{}, windowClosedOWA()
			}
			if args.Headless && browserGone(browser) {
				return owaCaptureResult{}, errOWANotCaptured
			}
		}
	}
}

func windowClosedOWA() error {
	// OWA login cancellation is auth-class per the LoginManager
	// contract (OWA is the last fallback channel), and the message
	// must mention user cancellation.
	return bridge.AuthError("user cancelled owa login: browser window closed by user")
}

// collectCookies reads every cookie for the OWA hosts. host is the
// captured host; if a canary-bearing host differs, it wins.
func (m *Manager) collectCookies(browser *rod.Browser, capturedHost string) ([]cookieKV, string) {
	protoCookies, err := browser.GetCookies()
	if err != nil {
		return nil, capturedHost
	}
	out := make([]cookieKV, 0, len(protoCookies))
	host := capturedHost
	for _, c := range protoCookies {
		if !isOWAHost(c.Domain) {
			continue
		}
		out = append(out, cookieKV{Name: c.Name, Value: c.Value, Domain: c.Domain})
		if isCanaryCookie(c.Name) && host == "" {
			host = strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		}
		if isCanaryCookie(c.Name) {
			host = strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		}
	}
	return out, host
}

func isCanaryCookie(name string) bool {
	n := strings.ToLower(name)
	return n == "x-owa-canary" || strings.HasPrefix(n, "cadata")
}

func hostOf(rawURL string) string {
	// Avoid importing net/url twice-named helpers in owa.go.
	return parseHost(rawURL)
}
