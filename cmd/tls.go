package cmd

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
)

// tlsConfig validates downstream encryption settings before any listener
// or account state is opened. Nil preserves the existing plaintext mode.
// It intentionally leaves cipher suites and the maximum TLS version to Go.
func (s *ServeCmd) tlsConfig() (*tls.Config, error) {
	// Direct callers of ServeCmd (as opposed to Kong) also get the defaults.
	if s.ImapTLSMode == "" {
		s.ImapTLSMode = "starttls"
	}
	if s.SmtpTLSMode == "" {
		s.SmtpTLSMode = "starttls"
	}
	if s.TLSMinVersion == "" {
		s.TLSMinVersion = "1.3"
	}
	for _, setting := range []struct{ flag, value string }{
		{"--imap-tls-mode", s.ImapTLSMode},
		{"--smtp-tls-mode", s.SmtpTLSMode},
	} {
		if setting.value != "tls" && setting.value != "starttls" {
			return nil, fmt.Errorf("%s must be tls or starttls, got %q", setting.flag, setting.value)
		}
	}
	var minVersion uint16
	switch s.TLSMinVersion {
	case "1.2":
		minVersion = tls.VersionTLS12
	case "1.3":
		minVersion = tls.VersionTLS13
	default:
		return nil, fmt.Errorf("--tls-min-version must be 1.2 or 1.3, got %q", s.TLSMinVersion)
	}
	if !s.TLSEnabled {
		return nil, nil
	}
	if s.TLSCert == "" || s.TLSKey == "" {
		return nil, fmt.Errorf("--tls-enabled requires both --tls-cert and --tls-key (PEM files)")
	}
	cert, err := tls.LoadX509KeyPair(s.TLSCert, s.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("load downstream TLS certificate/key: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
	}, nil
}

// serveHTTP uses net/http's TLS path so Request.TLS is set correctly and
// HTTP-specific ALPN negotiation does not mutate the IMAP/SMTP config.
// A nil config preserves HTTP. Empty certificate paths use the already
// validated, startup-loaded keypair; there is no implicit file reload.
func serveHTTP(srv *http.Server, cfg *tls.Config) func(net.Listener) error {
	if cfg == nil {
		return srv.Serve
	}
	srv.TLSConfig = cfg.Clone()
	return func(ln net.Listener) error { return srv.ServeTLS(ln, "", "") }
}
