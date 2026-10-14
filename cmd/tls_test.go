package cmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
)

// All TLS fixtures are generated in temporary directories; no keys are
// committed or fetched and no live Microsoft services are contacted.
func testTLSFiles(t *testing.T) (certPath, keyPath string, roots *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("generated certificate did not load")
	}
	return certPath, keyPath, roots
}

func parseTLSFlags(t *testing.T, args ...string) ServeCmd {
	t.Helper()
	var cli struct {
		Serve ServeCmd `cmd:""`
	}
	parser, err := kong.New(&cli, kong.DefaultEnvars("OB"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Parse(append([]string{"serve"}, args...)); err != nil {
		t.Fatal(err)
	}
	return cli.Serve
}

func TestTLSDefaultsAndCertificateLoading(t *testing.T) {
	cmd := parseTLSFlags(t)
	if cmd.TLSEnabled || cmd.TLSMinVersion != "1.3" || cmd.ImapTLSMode != "starttls" || cmd.SmtpTLSMode != "starttls" {
		t.Fatalf("incorrect TLS defaults: %+v", cmd)
	}
	cfg, err := cmd.tlsConfig()
	if err != nil || cfg != nil {
		t.Fatalf("plaintext defaults require no certificate: config=%v error=%v", cfg, err)
	}
	cert, key, _ := testTLSFiles(t)
	cmd = parseTLSFlags(t, "--tls-enabled", "--tls-cert", cert, "--tls-key", key)
	cfg, err = cmd.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || len(cfg.Certificates) != 1 || cfg.MinVersion != tls.VersionTLS13 || cfg.MaxVersion != 0 {
		t.Fatalf("PEM pair or minimum version not applied: %+v", cfg)
	}
	if cfg.ClientAuth != tls.NoClientCert || cfg.InsecureSkipVerify {
		t.Fatal("no client-certificate or verification bypass is in scope")
	}
}

func TestTLSConfigurationValidation(t *testing.T) {
	cert, key, _ := testTLSFiles(t)
	_, otherKey, _ := testTLSFiles(t)
	badPEM := filepath.Join(t.TempDir(), "not-pem")
	if err := os.WriteFile(badPEM, []byte("not a certificate or private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		cmd  ServeCmd
		want string
	}{
		{"missing pair", ServeCmd{TLSEnabled: true}, "--tls-cert"},
		{"missing cert", ServeCmd{TLSEnabled: true, TLSKey: key}, "--tls-cert"},
		{"missing key", ServeCmd{TLSEnabled: true, TLSCert: cert}, "--tls-key"},
		{"nonexistent cert", ServeCmd{TLSEnabled: true, TLSCert: cert + ".missing", TLSKey: key}, "load downstream TLS"},
		{"nonexistent key", ServeCmd{TLSEnabled: true, TLSCert: cert, TLSKey: key + ".missing"}, "load downstream TLS"},
		{"malformed cert", ServeCmd{TLSEnabled: true, TLSCert: badPEM, TLSKey: key}, "load downstream TLS"},
		{"malformed key", ServeCmd{TLSEnabled: true, TLSCert: cert, TLSKey: badPEM}, "load downstream TLS"},
		{"mismatched key", ServeCmd{TLSEnabled: true, TLSCert: cert, TLSKey: otherKey}, "load downstream TLS"},
		{"TLS1.0", ServeCmd{TLSMinVersion: "1.0"}, "--tls-min-version"},
		{"TLS1.1", ServeCmd{TLSMinVersion: "1.1"}, "--tls-min-version"},
		{"unknown min version", ServeCmd{TLSMinVersion: "latest"}, "--tls-min-version"},
		{"unknown IMAP mode", ServeCmd{ImapTLSMode: "auto"}, "--imap-tls-mode"},
		{"unknown SMTP mode", ServeCmd{SmtpTLSMode: "plain"}, "--smtp-tls-mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := tc.cmd.tlsConfig()
			if err == nil || cfg != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("config=%v error=%v, want error mentioning %s", cfg, err, tc.want)
			}
		})
	}
	cmd := ServeCmd{TLSEnabled: false, TLSCert: "does-not-exist", TLSKey: "does-not-exist"}
	if cfg, err := cmd.tlsConfig(); cfg != nil || err != nil {
		t.Fatalf("disabled TLS must not open PEM files: config=%v error=%v", cfg, err)
	}
	cmd = ServeCmd{TLSEnabled: true, TLSCert: cert, TLSKey: key, TLSMinVersion: "1.2"}
	if cfg, err := cmd.tlsConfig(); err != nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("explicit TLS1.2 minimum: config=%v error=%v", cfg, err)
	}
}

func TestTLSFlagsEnvironment(t *testing.T) {
	cert, key, _ := testTLSFiles(t)
	for name, value := range map[string]string{
		"OB_TLS_ENABLED": "true", "OB_TLS_CERT": cert, "OB_TLS_KEY": key,
		"OB_TLS_MIN_VERSION": "1.2", "OB_IMAP_TLS_MODE": "tls", "OB_SMTP_TLS_MODE": "starttls",
	} {
		t.Setenv(name, value)
	}
	cmd := parseTLSFlags(t)
	if !cmd.TLSEnabled || cmd.TLSCert != cert || cmd.TLSKey != key || cmd.TLSMinVersion != "1.2" || cmd.ImapTLSMode != "tls" || cmd.SmtpTLSMode != "starttls" {
		t.Fatalf("TLS environment flags not applied: %+v", cmd)
	}
	cmd = parseTLSFlags(t, "--tls-min-version", "1.3", "--smtp-tls-mode", "tls")
	if cmd.TLSMinVersion != "1.3" || cmd.SmtpTLSMode != "tls" {
		t.Fatalf("explicit flags must override environment: %+v", cmd)
	}
}

func TestInvalidTLSFailsBeforeListenersOrAccountState(t *testing.T) {
	// Deliberately occupy a configured port: validation must report the
	// missing PEM pair before trying to bind, and before creating state.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	state := filepath.Join(t.TempDir(), "state-must-not-be-created")
	cmd := ServeCmd{
		Password: "test", TLSEnabled: true, Interface: "127.0.0.1",
		ImapPort: ln.Addr().(*net.TCPAddr).Port, StateDir: state,
	}
	err = cmd.Run(&Deps{Version: "test"})
	var exit *ExitError
	if err == nil || !strings.Contains(err.Error(), "--tls-cert") || !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("must fail fast with a TLS config usage error, got %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("invalid TLS created account state, err=%v", err)
	}
}
