package smtpserver_test

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/smtpserver"
	"github.com/rs/zerolog"
)

func smtpTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS13},
		&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}
}

func startSMTPTransport(t *testing.T, config *tls.Config, startTLS bool, timeout time.Duration, logger zerolog.Logger) (string, *smtpserver.Server) {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: fakeLogin{}, Factory: fakeFactory{p: &fakeProvider{}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := smtpserver.New(reg, pw, logger)
	if config != nil {
		srv.TLSConfig = config.Clone()
	}
	srv.StartTLS = startTLS
	srv.TLSHandshakeTimeout = timeout
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() {
		_ = srv.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return l.Addr().String(), srv
}

func smtpRawDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSMTPSTARTTLSPrivacy(t *testing.T) {
	serverConfig, _ := smtpTLSConfigs(t)
	logs := &lockedBuffer{}
	addr, _ := startSMTPTransport(t, serverConfig, true, 0, zerolog.New(logs))
	cl := dial(t, addr)
	defer cl.c.Close()
	if got := cl.cmd(t, "EHLO client"); !strings.Contains(got, "STARTTLS") || strings.Contains(got, "AUTH") {
		t.Errorf("plaintext EHLO = %q, want STARTTLS without AUTH", got)
	}
	for _, command := range []string{"AUTH PLAIN", "AUTH PLAIN invalid-base64!", "AUTH PLAIN " + authPlain(acct, pw), "AUTH LOGIN", "AUTH LOGIN invalid-base64!", "MAIL FROM:<user@example.com>"} {
		if got := cl.cmd(t, "%s", command); got != "530 5.7.0 Must issue STARTTLS first" {
			t.Fatalf("%s = %q, want encryption-required 530 without challenge", strings.Fields(command)[0], got)
		}
	}
	if out := logs.String(); strings.Contains(out, `"op":"auth"`) || strings.Contains(out, `"op":"send"`) {
		t.Fatalf("plaintext credentials reached authentication/upstream: %s", out)
	}
}

func smtpUpgrade(t *testing.T, cl *client, config *tls.Config) {
	t.Helper()
	if got := cl.cmd(t, "STARTTLS"); got != "220 2.0.0 Ready to start TLS" {
		t.Fatalf("STARTTLS = %q", got)
	}
	c := tls.Client(cl.c, config)
	if err := c.Handshake(); err != nil {
		t.Fatalf("STARTTLS handshake: %v", err)
	}
	cl.c, cl.r = c, bufio.NewReader(c)
}

func TestSMTPSTARTTLSUpgrade(t *testing.T) {
	for _, mechanism := range []string{"PLAIN", "LOGIN"} {
		t.Run(mechanism, func(t *testing.T) {
			serverConfig, clientConfig := smtpTLSConfigs(t)
			addr, _ := startSMTPTransport(t, serverConfig, true, 0, zerolog.Nop())
			cl := dial(t, addr)
			defer cl.c.Close()
			cl.cmd(t, "EHLO before")
			smtpUpgrade(t, cl, clientConfig)
			for _, cmd := range []string{"AUTH PLAIN " + authPlain(acct, pw), "MAIL FROM:<user@example.com>", "RCPT TO:<recipient@example.com>", "DATA"} {
				if got := cl.cmd(t, "%s", cmd); !strings.HasPrefix(got, "503 ") {
					t.Fatalf("before new EHLO: %s = %q", cmd, got)
				}
			}
			cl.cmd(t, "HELO not-enough")
			for _, cmd := range []string{"AUTH LOGIN", "MAIL FROM:<user@example.com>"} {
				if got := cl.cmd(t, "%s", cmd); !strings.HasPrefix(got, "503 ") {
					t.Fatalf("HELO must not replace EHLO: %s = %q", cmd, got)
				}
			}
			if got := cl.cmd(t, "EHLO after"); !strings.Contains(got, "AUTH PLAIN LOGIN") || strings.Contains(got, "STARTTLS") {
				t.Fatalf("encrypted EHLO = %q", got)
			}
			if got := cl.cmd(t, "RCPT TO:<recipient@example.com>"); !strings.HasPrefix(got, "503 ") {
				t.Fatalf("transaction state = %q", got)
			}
			if mechanism == "PLAIN" {
				if got := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw)); !strings.HasPrefix(got, "235 ") {
					t.Fatalf("AUTH = %q", got)
				}
			} else {
				if got := cl.cmd(t, "AUTH LOGIN"); got != "334 VXNlcm5hbWU6" {
					t.Fatalf("LOGIN challenge = %q", got)
				}
				if got := cl.cmd(t, "dXNlckBleGFtcGxlLmNvbQ=="); got != "334 UGFzc3dvcmQ6" {
					t.Fatalf("password challenge = %q", got)
				}
				if got := cl.cmd(t, "aW5zdGFuY2Utc2VjcmV0"); !strings.HasPrefix(got, "235 ") {
					t.Fatalf("LOGIN = %q", got)
				}
			}
			if got := cl.cmd(t, "MAIL FROM:<user@example.com>"); !strings.HasPrefix(got, "250 ") {
				t.Fatalf("MAIL = %q", got)
			}
			if got := cl.cmd(t, "RCPT TO:<recipient@example.com>"); !strings.HasPrefix(got, "250 ") {
				t.Fatalf("RCPT = %q", got)
			}
			if got := cl.cmd(t, "DATA"); !strings.HasPrefix(got, "354 ") {
				t.Fatalf("DATA = %q", got)
			}
			if got := cl.cmd(t, "Subject: encrypted\r\n\r\nmessage\r\n."); !strings.HasPrefix(got, "250 2.0.0 OK queued as ") {
				t.Fatalf("delivery = %q", got)
			}
		})
	}
}

func TestSMTPSTARTTLSCommandStates(t *testing.T) {
	serverConfig, clientConfig := smtpTLSConfigs(t)
	addr, _ := startSMTPTransport(t, serverConfig, true, 0, zerolog.Nop())
	cl := dial(t, addr)
	defer cl.c.Close()
	for _, tc := range []struct{ command, code string }{
		{"STARTTLS extra", "501 "}, {"STARTTLS", "503 "}, {"HELO client", "250 "}, {"STARTTLS", "503 "}, {"EHLO client", "250-"}, {"STARTTLS extra", "501 "},
	} {
		if got := cl.cmd(t, "%s", tc.command); !strings.HasPrefix(got, tc.code) {
			t.Fatalf("%s = %q, want %s", tc.command, got, tc.code)
		}
	}
	smtpUpgrade(t, cl, clientConfig)
	if got := cl.cmd(t, "STARTTLS"); !strings.HasPrefix(got, "503 ") {
		t.Fatalf("repeated STARTTLS = %q", got)
	}
	cl.cmd(t, "EHLO client")
	if got := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw)); !strings.HasPrefix(got, "235 ") {
		t.Fatalf("state-test authentication = %q", got)
	}
	if got := cl.cmd(t, "STARTTLS"); !strings.HasPrefix(got, "503 ") {
		t.Fatalf("authenticated STARTTLS = %q", got)
	}
	if got := cl.cmd(t, "MAIL FROM:<user@example.com>"); !strings.HasPrefix(got, "250 ") {
		t.Fatalf("state-test MAIL = %q", got)
	}
	if got := cl.cmd(t, "STARTTLS"); !strings.HasPrefix(got, "503 ") {
		t.Fatalf("transaction STARTTLS = %q", got)
	}
}

func TestSMTPSTARTTLSBufferedPlaintextRejected(t *testing.T) {
	serverConfig, _ := smtpTLSConfigs(t)
	logs := &lockedBuffer{}
	addr, _ := startSMTPTransport(t, serverConfig, true, 0, zerolog.New(logs))
	cl := dial(t, addr)
	defer cl.c.Close()
	cl.cmd(t, "EHLO client")
	if _, err := cl.c.Write([]byte("STARTTLS\r\nAUTH PLAIN " + authPlain(acct, pw) + "\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = cl.c.SetReadDeadline(time.Now().Add(time.Second))
	if line, err := cl.r.ReadString('\n'); err == nil {
		t.Fatalf("buffered plaintext suffix accepted: %q", line)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("server did not close buffered plaintext suffix: %v", err)
	}
	if out := logs.String(); strings.Contains(out, `"op":"auth"`) {
		t.Fatalf("plaintext suffix reached authentication: %s", out)
	}
}

func TestSMTPTLSHandshakeTimeout(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "starttls"}[startTLS], func(t *testing.T) {
			serverConfig, _ := smtpTLSConfigs(t)
			logs := &lockedBuffer{}
			addr, _ := startSMTPTransport(t, serverConfig, startTLS, 40*time.Millisecond, zerolog.New(logs))
			c := smtpRawDial(t, addr)
			if startTLS {
				cl := &client{c: c, r: bufio.NewReader(c)}
				cl.readReply(t)
				cl.cmd(t, "EHLO client")
				if got := cl.cmd(t, "STARTTLS"); got != "220 2.0.0 Ready to start TLS" {
					t.Fatalf("STARTTLS = %q", got)
				}
			}
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			var b [1]byte
			if n, err := c.Read(b[:]); n != 0 || err == nil {
				t.Fatalf("stalled TLS did not close: bytes=%q err=%v", b[:n], err)
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("server did not enforce handshake timeout: %v", err)
			}
			if out := logs.String(); !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, "TLS handshake failed") {
				t.Fatalf("missing handshake failure warning: %s", out)
			}
		})
	}
}

// Hold a real accepted socket across Close to expose accept/shutdown races.
type smtpHandoffListener struct {
	net.Listener
	accepted chan struct{}
	release  chan struct{}
}

func (l *smtpHandoffListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		close(l.accepted)
		<-l.release
	}
	return c, err
}

func TestSMTPTLSCloseDuringAcceptHandoff(t *testing.T) {
	serverConfig, _ := smtpTLSConfigs(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gated := &smtpHandoffListener{Listener: l, accepted: make(chan struct{}), release: make(chan struct{})}
	srv := smtpserver.New(nil, pw, zerolog.Nop())
	srv.TLSConfig = serverConfig.Clone()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(gated) }()
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(gated.release) })
		_ = srv.Close()
	})
	c := smtpRawDial(t, l.Addr().String())
	select {
	case <-gated.accepted:
	case <-time.After(time.Second):
		t.Fatal("accept did not arrive")
	}
	_ = srv.Close()
	release.Do(func() { close(gated.release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		_ = c.Close()
		<-done
		t.Fatal("connection accepted across Close entered an untracked TLS handshake")
	}
}

func TestSMTPTLSCertificateAndMinimumVersion(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			min     uint16
			trusted bool
			max     uint16
			wantOK  bool
		}{
			{"trusted", tls.VersionTLS13, true, 0, true},
			{"untrusted", tls.VersionTLS13, false, 0, false},
			{"tls12_rejected_by_min13", tls.VersionTLS13, true, tls.VersionTLS12, false},
			{"tls12_allowed_by_min12", tls.VersionTLS12, true, tls.VersionTLS12, true},
		} {
			t.Run(map[bool]string{false: "implicit", true: "starttls"}[startTLS]+"/"+tc.name, func(t *testing.T) {
				serverConfig, clientConfig := smtpTLSConfigs(t)
				serverConfig.MinVersion = tc.min
				clientConfig.MaxVersion = tc.max
				if !tc.trusted {
					clientConfig.RootCAs = x509.NewCertPool()
				}
				addr, _ := startSMTPTransport(t, serverConfig, startTLS, 0, zerolog.Nop())
				c := smtpRawDial(t, addr)
				if startTLS {
					cl := &client{c: c, r: bufio.NewReader(c)}
					cl.readReply(t)
					cl.cmd(t, "EHLO client")
					if got := cl.cmd(t, "STARTTLS"); got != "220 2.0.0 Ready to start TLS" {
						t.Fatalf("STARTTLS = %q", got)
					}
				}
				tlsConn := tls.Client(c, clientConfig)
				err := tlsConn.Handshake()
				if (err == nil) != tc.wantOK {
					t.Fatalf("TLS handshake = %v, want success=%v", err, tc.wantOK)
				}
				if err != nil {
					return
				}
				if tc.max == tls.VersionTLS12 && tlsConn.ConnectionState().Version != tls.VersionTLS12 {
					t.Fatal("configured TLS1.2 was not negotiated")
				}
				cl := &client{c: tlsConn, r: bufio.NewReader(tlsConn)}
				if !startTLS {
					cl.readReply(t)
				}
				if got := cl.cmd(t, "EHLO encrypted"); !strings.Contains(got, "AUTH PLAIN LOGIN") || strings.Contains(got, "STARTTLS") {
					t.Fatalf("EHLO = %q", got)
				}
			})
		}
	}
}

func TestSMTPTLSHandshakeFailureClosesWithoutDowngrade(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "starttls"}[startTLS], func(t *testing.T) {
			serverConfig, _ := smtpTLSConfigs(t)
			logs := &lockedBuffer{}
			addr, _ := startSMTPTransport(t, serverConfig, startTLS, 0, zerolog.New(logs))
			c := smtpRawDial(t, addr)
			if startTLS {
				cl := &client{c: c, r: bufio.NewReader(c)}
				cl.readReply(t)
				cl.cmd(t, "EHLO client")
				if got := cl.cmd(t, "STARTTLS"); got != "220 2.0.0 Ready to start TLS" {
					t.Fatalf("STARTTLS = %q", got)
				}
			}
			if _, err := c.Write([]byte("AUTH PLAIN " + authPlain(acct, pw) + "\r\nEHLO downgrade\r\n")); err != nil {
				t.Fatal(err)
			}
			var b [4096]byte
			if n, err := c.Read(b[:]); n != 0 || err == nil {
				t.Fatalf("plaintext after failed handshake: %q (%v)", b[:n], err)
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("failed TLS did not close: %v", err)
			}
			if out := logs.String(); !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, "TLS handshake failed") || strings.Contains(out, `"op":"auth"`) {
				t.Fatalf("handshake failure logging = %s", out)
			}
		})
	}
}

func TestSMTPTLSCloseDropsConnections(t *testing.T) {
	for _, mode := range []string{"implicit_handshake", "starttls_handshake", "implicit_active", "starttls_active"} {
		t.Run(mode, func(t *testing.T) {
			serverConfig, clientConfig := smtpTLSConfigs(t)
			startTLS := strings.HasPrefix(mode, "starttls")
			addr, srv := startSMTPTransport(t, serverConfig, startTLS, 0, zerolog.Nop())
			c := smtpRawDial(t, addr)
			cl := &client{c: c, r: bufio.NewReader(c)}
			if startTLS {
				cl.readReply(t)
				cl.cmd(t, "EHLO client")
				if got := cl.cmd(t, "STARTTLS"); got != "220 2.0.0 Ready to start TLS" {
					t.Fatalf("STARTTLS = %q", got)
				}
			}
			if strings.HasSuffix(mode, "active") {
				tc := tls.Client(c, clientConfig)
				if err := tc.Handshake(); err != nil {
					t.Fatal(err)
				}
				cl.c, cl.r = tc, bufio.NewReader(tc)
				if !startTLS {
					cl.readReply(t)
				}
				cl.cmd(t, "EHLO client")
			}
			closed := make(chan error, 1)
			go func() { closed <- srv.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Errorf("Close: %v", err)
				}
			case <-time.After(time.Second):
				_ = c.Close()
				t.Fatal("Close hung on TLS connection")
			}
			var b [1]byte
			if n, err := cl.c.Read(b[:]); n != 0 || err == nil {
				t.Fatalf("Close left connection active: %q (%v)", b[:n], err)
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("connection did not close: %v", err)
			}
		})
	}
}

func TestSMTPNilTLSConfigPreservesPlainMode(t *testing.T) {
	addr, _ := startSMTPTransport(t, nil, true, 0, zerolog.Nop())
	cl := dial(t, addr)
	defer cl.c.Close()
	if got := cl.cmd(t, "EHLO client"); !strings.Contains(got, "AUTH PLAIN LOGIN") || strings.Contains(got, "STARTTLS") {
		t.Fatalf("plain EHLO = %q", got)
	}
	for _, command := range []string{"STARTTLS", "STARTTLS extra"} {
		if got := cl.cmd(t, "%s", command); got != "454 4.7.0 TLS not available" {
			t.Fatalf("nil-config %s = %q", command, got)
		}
	}
	if got := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw)); !strings.HasPrefix(got, "235 ") {
		t.Fatalf("plain AUTH = %q", got)
	}
	if got := cl.cmd(t, "MAIL FROM:<user@example.com>"); !strings.HasPrefix(got, "250 ") {
		t.Fatalf("plain MAIL = %q", got)
	}
}

func TestSMTPTLSClearsHandshakeDeadline(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "starttls"}[startTLS], func(t *testing.T) {
			serverConfig, clientConfig := smtpTLSConfigs(t)
			addr, _ := startSMTPTransport(t, serverConfig, startTLS, 200*time.Millisecond, zerolog.Nop())
			cl := &client{c: smtpRawDial(t, addr)}
			cl.r = bufio.NewReader(cl.c)
			if startTLS {
				cl.readReply(t)
				cl.cmd(t, "EHLO client")
				smtpUpgrade(t, cl, clientConfig)
			} else {
				tc := tls.Client(cl.c, clientConfig)
				if err := tc.Handshake(); err != nil {
					t.Fatal(err)
				}
				cl.c, cl.r = tc, bufio.NewReader(tc)
				cl.readReply(t)
			}
			<-time.After(250 * time.Millisecond)
			if got := cl.cmd(t, "EHLO after-handshake-deadline"); !strings.Contains(got, "AUTH PLAIN LOGIN") {
				t.Fatalf("deadline not cleared: %q", got)
			}
		})
	}
}

// Record server writes beginning at the first TLS handshake record, excluding
// the plaintext SMTP greeting/capabilities/STARTTLS reply in upgrade mode.
type smtpTLSWireConn struct {
	net.Conn
	mu        sync.Mutex
	wire      bytes.Buffer
	recording bool
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *smtpTLSWireConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	if n > 0 && p[0] == 22 {
		c.recording = true
	}
	if c.recording {
		_, _ = c.wire.Write(p[:n])
	}
	c.mu.Unlock()
	return n, err
}

func (c *smtpTLSWireConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() { close(c.closed) })
	return err
}

type smtpTLSWireListener struct {
	net.Listener
	accepted chan *smtpTLSWireConn
}

func (l *smtpTLSWireListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	recorded := &smtpTLSWireConn{Conn: c, closed: make(chan struct{})}
	l.accepted <- recorded
	return recorded, nil
}

func TestSMTPTLSQuitSendsCloseNotify(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "starttls"}[startTLS], func(t *testing.T) {
			serverConfig, clientConfig := smtpTLSConfigs(t)
			// TLS1.2 keeps the record content type visible even after encryption.
			serverConfig.MinVersion = tls.VersionTLS12
			clientConfig.MaxVersion = tls.VersionTLS12
			srv := smtpserver.New(nil, pw, zerolog.Nop())
			srv.TLSConfig, srv.StartTLS = serverConfig.Clone(), startTLS
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			recordedListener := &smtpTLSWireListener{Listener: l, accepted: make(chan *smtpTLSWireConn, 1)}
			done := make(chan error, 1)
			go func() { done <- srv.Serve(recordedListener) }()
			t.Cleanup(func() {
				_ = srv.Close()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("Serve: %v", err)
					}
				case <-time.After(time.Second):
					t.Error("Serve did not stop")
				}
			})
			c := smtpRawDial(t, l.Addr().String())
			var recorded *smtpTLSWireConn
			select {
			case recorded = <-recordedListener.accepted:
			case <-time.After(time.Second):
				t.Fatal("recording listener did not accept")
			}
			cl := &client{c: c, r: bufio.NewReader(c)}
			if startTLS {
				cl.readReply(t)
				cl.cmd(t, "EHLO client")
				smtpUpgrade(t, cl, clientConfig)
			} else {
				tc := tls.Client(c, clientConfig)
				if err := tc.Handshake(); err != nil {
					t.Fatal(err)
				}
				cl.c, cl.r = tc, bufio.NewReader(tc)
				cl.readReply(t)
			}
			cl.cmd(t, "EHLO encrypted")
			if got := cl.cmd(t, "QUIT"); got != "221 2.0.0 o365bridge closing connection" {
				t.Fatalf("QUIT = %q", got)
			}
			select {
			case <-recorded.closed:
			case <-time.After(time.Second):
				t.Fatal("QUIT did not close socket")
			}
			recorded.mu.Lock()
			wire := append([]byte(nil), recorded.wire.Bytes()...)
			recorded.mu.Unlock()
			var types []byte
			for len(wire) > 0 {
				if len(wire) < 5 {
					t.Fatalf("incomplete TLS record header: %x", wire)
				}
				n := 5 + (int(wire[3]) << 8) + int(wire[4])
				if n > len(wire) {
					t.Fatalf("incomplete TLS record: need %d bytes, have %d", n, len(wire))
				}
				types = append(types, wire[0])
				wire = wire[n:]
			}
			if len(types) == 0 || types[len(types)-1] != 21 {
				t.Fatalf("QUIT TLS record types = %v, want final Alert (21) carrying close_notify", types)
			}
		})
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	serverConfig, clientConfig := smtpTLSConfigs(t)
	addr, _ := startSMTPTransport(t, serverConfig, false, 0, zerolog.Nop())
	c := tls.Client(smtpRawDial(t, addr), clientConfig)
	if err := c.Handshake(); err != nil {
		t.Fatalf("trusted implicit TLS handshake: %v", err)
	}
	cl := &client{c: c, r: bufio.NewReader(c)}
	if got := cl.readReply(t); got != "220 o365bridge ESMTP o365bridge ready" {
		t.Fatalf("greeting = %q", got)
	}
	if got := cl.cmd(t, "EHLO client"); !strings.Contains(got, "AUTH PLAIN LOGIN") || strings.Contains(got, "STARTTLS") {
		t.Fatalf("implicit TLS EHLO = %q", got)
	}
	if got := cl.cmd(t, "AUTH PLAIN %s", authPlain(acct, pw)); !strings.HasPrefix(got, "235 ") {
		t.Fatalf("AUTH = %q", got)
	}
}
