package imapserver_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/imapserver"
)

func imapTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
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
		&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
}

func startConfiguredIMAP(t *testing.T, logger zerolog.Logger, configure func(*imapserver.Server)) (*imapserver.Server, net.Listener) {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: fakeLogin{}, Factory: fakeFactory{prov: newFakeProvider()}})
	if err != nil {
		t.Fatal(err)
	}
	srv := imapserver.New(reg, testPassword, logger)
	configure(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveIMAP(t, srv, ln)
	return srv, ln
}

func serveIMAP(t *testing.T, srv *imapserver.Server, ln net.Listener) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		closed := make(chan error, 1)
		go func() { closed <- srv.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				t.Errorf("Close: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Close did not return")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not return")
		}
	})
}

func rawIMAPConn(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

type imapRecordingConn struct {
	net.Conn
	writes    lockedBuffer
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *imapRecordingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		_, _ = c.writes.Write(p[:n])
	}
	return n, err
}

func (c *imapRecordingConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() { close(c.closed) })
	return err
}

type imapRecordingListener struct {
	net.Listener
	accepted chan *imapRecordingConn
}

func (l *imapRecordingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	recorded := &imapRecordingConn{Conn: conn, closed: make(chan struct{})}
	l.accepted <- recorded
	return recorded, nil
}

func TestIMAPTLSLogoutSendsCloseNotify(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("starttls=%t", startTLS), func(t *testing.T) {
			serverConfig, clientConfig := imapTLSConfigs(t)
			serverConfig.MinVersion = tls.VersionTLS12
			clientConfig.MinVersion, clientConfig.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
			srv := imapserver.New(nil, testPassword, zerolog.Nop())
			srv.TLSConfig, srv.StartTLS = serverConfig.Clone(), startTLS
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			recorder := &imapRecordingListener{Listener: ln, accepted: make(chan *imapRecordingConn, 1)}
			serveIMAP(t, srv, recorder)
			conn := rawIMAPConn(t, ln)
			var recorded *imapRecordingConn
			select {
			case recorded = <-recorder.accepted:
			case <-time.After(time.Second):
				t.Fatal("connection was not accepted")
			}
			c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
			if startTLS {
				_ = c.readLine()
				upgradeIMAPClient(t, c, clientConfig)
			} else {
				secure := tls.Client(conn, clientConfig)
				if err := secure.Handshake(); err != nil {
					t.Fatal(err)
				}
				c.conn, c.r = secure, bufio.NewReader(secure)
				_ = c.readLine()
			}
			if reply := joined(c.cmd("noop", "NOOP")); !strings.Contains(reply, "noop OK") {
				t.Fatalf("NOOP: %s", reply)
			}
			if reply := joined(c.cmd("logout", "LOGOUT")); !strings.Contains(reply, "* BYE") || !strings.Contains(reply, "logout OK") {
				t.Fatalf("LOGOUT: %s", reply)
			}
			_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := c.r.ReadByte(); err != io.EOF {
				t.Fatalf("logout EOF: %v", err)
			}
			select {
			case <-recorded.closed:
			case <-time.After(time.Second):
				t.Fatal("logout did not close transport")
			}
			wire := []byte(recorded.writes.String())
			// Ignore any plaintext greeting/STARTTLS reply before the first handshake record.
			offset := bytes.IndexByte(wire, 22)
			if offset < 0 {
				t.Fatal("missing TLS handshake records")
			}
			var recordTypes []byte
			for offset < len(wire) {
				if len(wire)-offset < 5 || wire[offset+1] != 3 {
					t.Fatalf("invalid TLS record header at %d", offset)
				}
				length := int(wire[offset+3])<<8 | int(wire[offset+4])
				if offset+5+length > len(wire) {
					t.Fatalf("truncated TLS record at %d", offset)
				}
				recordTypes = append(recordTypes, wire[offset])
				offset += 5 + length
			}
			if last := recordTypes[len(recordTypes)-1]; last != 21 {
				t.Fatalf("graceful TLS logout must end with Alert close_notify (21), got %d; record types=%v", last, recordTypes)
			}
		})
	}
}

func TestIMAPImplicitTLSBeforeGreeting(t *testing.T) {
	serverConfig, clientConfig := imapTLSConfigs(t)
	_, ln := startConfiguredIMAP(t, zerolog.Nop(), func(s *imapserver.Server) {
		s.TLSConfig = serverConfig.Clone()
	})
	conn := tls.Client(rawIMAPConn(t, ln), clientConfig)
	if err := conn.Handshake(); err != nil {
		t.Fatalf("TLS handshake before greeting: %v", err)
	}
	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	if greeting := c.readLine(); !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("greeting = %q", greeting)
	}
	c.login("a")
}

func plaintextIMAPClient(t *testing.T, ln net.Listener) (*client, string) {
	t.Helper()
	conn := rawIMAPConn(t, ln)
	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	return c, c.readLine()
}

func upgradeIMAPClient(t *testing.T, c *client, config *tls.Config) {
	t.Helper()
	if _, err := fmt.Fprint(c.conn, "tls STARTTLS\r\n"); err != nil {
		t.Fatal(err)
	}
	if reply := c.readLine(); !strings.HasPrefix(reply, "tls OK ") {
		t.Fatalf("STARTTLS reply = %q", reply)
	}
	conn := tls.Client(c.conn, config.Clone())
	if err := conn.Handshake(); err != nil {
		t.Fatalf("STARTTLS handshake: %v", err)
	}
	c.conn, c.r = conn, bufio.NewReader(conn)
}

type imapLoginCounter struct {
	fakeLogin
	calls atomic.Int32
}

func (l *imapLoginCounter) Login(ctx context.Context, email string, channel bridge.Channel) (bridge.Session, error) {
	l.calls.Add(1)
	return l.fakeLogin.Login(ctx, email, channel)
}

func TestIMAPSTARTTLSDoesNotAccessUpstreamBeforeUpgrade(t *testing.T) {
	serverConfig, clientConfig := imapTLSConfigs(t)
	login := &imapLoginCounter{}
	var providerCalls atomic.Int32
	provider := newFakeProvider()
	reg, err := bridge.NewRegistry(bridge.Config{
		StateDir: t.TempDir(), Login: login,
		Factory: bridge.FactoryFunc(func(_ string, _ bridge.Session) bridge.Provider {
			providerCalls.Add(1)
			return provider
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := imapserver.New(reg, testPassword, zerolog.Nop())
	srv.TLSConfig, srv.StartTLS = serverConfig.Clone(), true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveIMAP(t, srv, ln)
	c, _ := plaintextIMAPClient(t, ln)
	for i, command := range []string{"LOGIN " + testEmail + " " + testPassword, "AUTHENTICATE PLAIN", "AUTHENTICATE PLAIN !!!"} {
		tag := fmt.Sprintf("gate%d", i)
		if reply := joined(c.cmd(tag, command)); !strings.Contains(reply, tag+" NO [PRIVACYREQUIRED]") {
			t.Fatalf("credential gate: %s", reply)
		}
	}
	if reply := joined(c.cmd("select", "SELECT INBOX")); !strings.Contains(reply, "select NO") {
		t.Fatalf("plaintext session authenticated: %s", reply)
	}
	if login.calls.Load() != 0 || providerCalls.Load() != 0 {
		t.Fatalf("upstream before TLS: logins=%d providers=%d", login.calls.Load(), providerCalls.Load())
	}
	upgradeIMAPClient(t, c, clientConfig)
	c.login("auth")
	if reply := joined(c.cmd("list", "LIST \"\" \"*\"")); !strings.Contains(reply, "INBOX") || !strings.Contains(reply, "list OK") {
		t.Fatalf("upstream after TLS: %s", reply)
	}
	if login.calls.Load() == 0 || providerCalls.Load() == 0 {
		t.Fatal("encrypted upstream operation did not exercise login/provider boundaries")
	}
}

func TestIMAPSTARTTLSUnicodeCredentialsNeverSolicitLiteral(t *testing.T) {
	serverConfig, _ := imapTLSConfigs(t)
	_, ln := startConfiguredIMAP(t, zerolog.Nop(), func(s *imapserver.Server) {
		s.TLSConfig, s.StartTLS = serverConfig.Clone(), true
	})
	for _, command := range []string{
		"LOGıN " + testEmail + " {15}",
		"AUTHENTıCATE PLAIN {15}",
	} {
		t.Run(command, func(t *testing.T) {
			c, _ := plaintextIMAPClient(t, ln)
			if _, err := fmt.Fprintf(c.conn, "a %s\r\n", command); err != nil {
				t.Fatal(err)
			}
			if reply := c.readLine(); !strings.HasPrefix(reply, "a NO [PRIVACYREQUIRED]") {
				t.Fatalf("Unicode credential verb must not solicit plaintext literal: %q", reply)
			}
		})
	}
}

func TestIMAPSTARTTLSCredentialsRequirePrivacy(t *testing.T) {
	serverConfig, _ := imapTLSConfigs(t)
	logs := &lockedBuffer{}
	_, ln := startConfiguredIMAP(t, zerolog.New(logs), func(s *imapserver.Server) {
		s.TLSConfig, s.StartTLS = serverConfig.Clone(), true
	})
	for _, command := range []string{
		"LOGIN " + testEmail + " " + testPassword,
		"LOGIN", "LOGIN {16}", "LOGIN " + testEmail + " {15}",
		"LOGIN {999999999+}", "AUTHENTICATE PLAIN",
		"AUTHENTICATE PLAIN !!!not-base64!!!", "AUTHENTICATE PLAIN {16}",
	} {
		t.Run(command, func(t *testing.T) {
			c, greeting := plaintextIMAPClient(t, ln)
			if !strings.Contains(greeting, "STARTTLS") || !strings.Contains(greeting, "LOGINDISABLED") || strings.Contains(greeting, "AUTH=PLAIN") {
				t.Fatalf("pre-upgrade capabilities: %s", greeting)
			}
			if _, err := fmt.Fprintf(c.conn, "a %s\r\n", command); err != nil {
				t.Fatal(err)
			}
			if reply := c.readLine(); !strings.HasPrefix(reply, "a NO [PRIVACYREQUIRED]") {
				t.Fatalf("plaintext credentials must be rejected without a challenge: %q", reply)
			}
		})
	}
	if out := logs.String(); strings.Contains(out, `"op":"auth"`) || strings.Contains(out, testEmail) || strings.Contains(out, testPassword) {
		t.Fatalf("plaintext credentials were inspected/logged: %s", out)
	}
}

func TestIMAPSTARTTLSUpgradeEnablesAuthentication(t *testing.T) {
	for _, mechanism := range []string{"LOGIN", "AUTHENTICATE PLAIN", "AUTHENTICATE challenge"} {
		t.Run(mechanism, func(t *testing.T) {
			serverConfig, clientConfig := imapTLSConfigs(t)
			_, ln := startConfiguredIMAP(t, zerolog.Nop(), func(s *imapserver.Server) {
				s.TLSConfig, s.StartTLS = serverConfig.Clone(), true
			})
			c, _ := plaintextIMAPClient(t, ln)
			before := joined(c.cmd("pre", "CAPABILITY"))
			if !strings.Contains(before, "STARTTLS") || !strings.Contains(before, "LOGINDISABLED") || strings.Contains(before, "AUTH=PLAIN") {
				t.Fatalf("plaintext CAPABILITY: %s", before)
			}
			upgradeIMAPClient(t, c, clientConfig)
			after := joined(c.cmd("post", "CAPABILITY"))
			if !strings.Contains(after, "AUTH=PLAIN") || strings.Contains(after, "STARTTLS") || strings.Contains(after, "LOGINDISABLED") {
				t.Fatalf("encrypted CAPABILITY: %s", after)
			}
			if reply := joined(c.cmd("repeat", "STARTTLS")); !strings.Contains(reply, "repeat BAD") {
				t.Fatalf("repeated STARTTLS: %s", reply)
			}
			payload := base64.StdEncoding.EncodeToString([]byte("\x00" + testEmail + "\x00" + testPassword))
			switch mechanism {
			case "LOGIN":
				c.login("auth")
			case "AUTHENTICATE PLAIN":
				if reply := joined(c.cmd("auth", "AUTHENTICATE PLAIN "+payload)); !strings.Contains(reply, "auth OK") {
					t.Fatalf("AUTHENTICATE: %s", reply)
				}
			default:
				if _, err := fmt.Fprint(c.conn, "auth AUTHENTICATE PLAIN\r\n"); err != nil {
					t.Fatal(err)
				}
				if reply := c.readLine(); !strings.HasPrefix(reply, "+") {
					t.Fatalf("challenge: %q", reply)
				}
				if _, err := fmt.Fprintf(c.conn, "%s\r\n", payload); err != nil {
					t.Fatal(err)
				}
				if reply := joined(c.readUntil("auth")); !strings.Contains(reply, "auth OK") {
					t.Fatalf("challenge authentication: %s", reply)
				}
			}
			if reply := joined(c.cmd("authenticated", "STARTTLS")); !strings.Contains(reply, "authenticated BAD") {
				t.Fatalf("authenticated STARTTLS: %s", reply)
			}
			if reply := joined(c.cmd("list", "LIST \"\" \"*\"")); !strings.Contains(reply, "INBOX") || !strings.Contains(reply, "list OK") {
				t.Fatalf("upstream after encrypted auth: %s", reply)
			}
		})
	}
}

func TestIMAPSTARTTLSPipelinedPlaintextRejected(t *testing.T) {
	serverConfig, _ := imapTLSConfigs(t)
	logs := &lockedBuffer{}
	_, ln := startConfiguredIMAP(t, zerolog.New(logs), func(s *imapserver.Server) {
		s.TLSConfig, s.StartTLS = serverConfig.Clone(), true
	})
	c, _ := plaintextIMAPClient(t, ln)
	if _, err := fmt.Fprintf(c.conn, "tls STARTTLS\r\na LOGIN %s %s\r\n", testEmail, testPassword); err != nil {
		t.Fatal(err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(time.Second))
	for {
		line, err := c.r.ReadString('\n')
		if strings.Contains(line, "a OK") {
			t.Fatalf("buffered plaintext authenticated: %q", line)
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("pipelined plaintext left connection open: %v", err)
			}
			break
		}
	}
	if out := logs.String(); strings.Contains(out, `"op":"auth"`) {
		t.Fatalf("buffered credentials processed: %s", out)
	}
}

type imapAcceptGate struct {
	net.Listener
	accepted chan net.Conn
	release  chan struct{}
}

func (g *imapAcceptGate) Accept() (net.Conn, error) {
	conn, err := g.Listener.Accept()
	if err != nil {
		return nil, err
	}
	g.accepted <- conn
	<-g.release
	return conn, nil
}

func TestIMAPCloseRejectsConnectionAcceptedDuringShutdown(t *testing.T) {
	serverConfig, _ := imapTLSConfigs(t)
	srv := imapserver.New(nil, testPassword, zerolog.Nop())
	srv.TLSConfig = serverConfig.Clone()
	srv.TLSHandshakeTimeout = 5 * time.Second
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gate := &imapAcceptGate{Listener: ln, accepted: make(chan net.Conn, 1), release: make(chan struct{})}
	serveIMAP(t, srv, gate)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate.release) }) })
	conn := rawIMAPConn(t, ln)
	select {
	case accepted := <-gate.accepted:
		t.Cleanup(func() { _ = accepted.Close() })
	case <-time.After(time.Second):
		t.Fatal("connection was not accepted")
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(gate.release) })
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if n, err := conn.Read(buf[:]); n != 0 || err == nil {
		t.Fatalf("accepted connection survived shutdown: n=%d err=%v", n, err)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("accepted connection was not closed: %v", err)
	}
}

func TestIMAPTLSHandshakeTimeoutClosesAndWarns(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("starttls=%t", startTLS), func(t *testing.T) {
			serverConfig, _ := imapTLSConfigs(t)
			logs := &lockedBuffer{}
			_, ln := startConfiguredIMAP(t, zerolog.New(logs), func(s *imapserver.Server) {
				s.TLSConfig, s.StartTLS = serverConfig.Clone(), startTLS
				s.TLSHandshakeTimeout = 50 * time.Millisecond
			})
			conn := rawIMAPConn(t, ln)
			if startTLS {
				c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
				_ = c.readLine()
				if _, err := fmt.Fprint(conn, "tls STARTTLS\r\n"); err != nil {
					t.Fatal(err)
				}
				if reply := c.readLine(); !strings.HasPrefix(reply, "tls OK ") {
					t.Fatalf("STARTTLS: %q", reply)
				}
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			var buf [256]byte
			if n, err := conn.Read(buf[:]); n != 0 || err == nil {
				t.Fatalf("stalled handshake emitted plaintext or stayed open: n=%d err=%v bytes=%q", n, err, buf[:n])
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("handshake deadline was not applied: %v", err)
			}
			out := logs.String()
			for _, want := range []string{`"level":"warn"`, `"protocol":"imap"`, `"op":"tls"`} {
				if !strings.Contains(out, want) {
					t.Fatalf("handshake warning missing %s: %s", want, out)
				}
			}
		})
	}
}

func TestIMAPTLSHandshakeFailureNeverDowngrades(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("starttls=%t", startTLS), func(t *testing.T) {
			serverConfig, _ := imapTLSConfigs(t)
			logs := &lockedBuffer{}
			_, ln := startConfiguredIMAP(t, zerolog.New(logs), func(s *imapserver.Server) {
				s.TLSConfig, s.StartTLS = serverConfig.Clone(), startTLS
			})
			conn := rawIMAPConn(t, ln)
			if startTLS {
				c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
				_ = c.readLine()
				if _, err := fmt.Fprint(conn, "tls STARTTLS\r\n"); err != nil {
					t.Fatal(err)
				}
				if reply := c.readLine(); !strings.HasPrefix(reply, "tls OK ") {
					t.Fatalf("STARTTLS: %q", reply)
				}
			}
			if _, err := fmt.Fprintf(conn, "a NOOP\r\nb LOGIN %s %s\r\n", testEmail, testPassword); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			var buf [256]byte
			if n, err := conn.Read(buf[:]); n != 0 || err == nil {
				t.Fatalf("failed handshake resumed plaintext: n=%d err=%v bytes=%q", n, err, buf[:n])
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("failed handshake did not close: %v", err)
			}
			if out := logs.String(); !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, `"op":"tls"`) || strings.Contains(out, `"op":"auth"`) {
				t.Fatalf("handshake failure log: %s", out)
			}
		})
	}
}

func TestIMAPTLSCloseHandshakingAndUpgradedConnections(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		for _, complete := range []bool{false, true} {
			t.Run(fmt.Sprintf("starttls=%t/complete=%t", startTLS, complete), func(t *testing.T) {
				serverConfig, clientConfig := imapTLSConfigs(t)
				logs := &lockedBuffer{}
				srv, ln := startConfiguredIMAP(t, zerolog.New(logs), func(s *imapserver.Server) {
					s.TLSConfig, s.StartTLS = serverConfig.Clone(), startTLS
					s.TLSHandshakeTimeout = 5 * time.Second
				})
				conn := rawIMAPConn(t, ln)
				if startTLS {
					c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
					_ = c.readLine()
					if _, err := fmt.Fprint(conn, "tls STARTTLS\r\n"); err != nil {
						t.Fatal(err)
					}
					if reply := c.readLine(); !strings.HasPrefix(reply, "tls OK ") {
						t.Fatalf("STARTTLS: %q", reply)
					}
				} else if !complete {
					// The connection log marks entry into the implicit handshake path.
					until := time.Now().Add(time.Second)
					for !strings.Contains(logs.String(), `"op":"connect"`) {
						if time.Now().After(until) {
							t.Fatal("server did not enter connection handler")
						}
						time.Sleep(time.Millisecond)
					}
				}
				if complete {
					secure := tls.Client(conn, clientConfig)
					if err := secure.Handshake(); err != nil {
						t.Fatal(err)
					}
					conn = secure
					c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
					if !startTLS {
						_ = c.readLine()
					}
					if reply := joined(c.cmd("noop", "NOOP")); !strings.Contains(reply, "noop OK") {
						t.Fatalf("upgraded socket: %s", reply)
					}
				}
				closed := make(chan error, 1)
				go func() { closed <- srv.Close() }()
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("Close blocked on handshaking/upgraded socket")
				}
				var buf [1]byte
				if n, err := conn.Read(buf[:]); n != 0 || err == nil {
					t.Fatalf("socket remains open after Close: n=%d err=%v", n, err)
				}
			})
		}
	}
}

func TestIMAPTLSVersionAndCertificatePolicy(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		for _, policy := range []string{"trusted", "untrusted", "wrong-name", "reject-tls12", "permit-tls12"} {
			t.Run(fmt.Sprintf("starttls=%t/%s", startTLS, policy), func(t *testing.T) {
				serverConfig, clientConfig := imapTLSConfigs(t)
				switch policy {
				case "untrusted":
					clientConfig.RootCAs = x509.NewCertPool()
				case "wrong-name":
					clientConfig.ServerName = "wrong.example"
				case "reject-tls12", "permit-tls12":
					clientConfig.MinVersion, clientConfig.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
					if policy == "permit-tls12" {
						serverConfig.MinVersion = tls.VersionTLS12
					}
				}
				_, ln := startConfiguredIMAP(t, zerolog.Nop(), func(s *imapserver.Server) { s.TLSConfig, s.StartTLS = serverConfig.Clone(), startTLS })
				conn := rawIMAPConn(t, ln)
				if startTLS {
					c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
					_ = c.readLine()
					if _, err := fmt.Fprint(conn, "tls STARTTLS\r\n"); err != nil {
						t.Fatal(err)
					}
					if reply := c.readLine(); !strings.HasPrefix(reply, "tls OK ") {
						t.Fatalf("STARTTLS: %q", reply)
					}
				}
				secure := tls.Client(conn, clientConfig)
				err := secure.Handshake()
				wantSuccess := policy == "trusted" || policy == "permit-tls12"
				if (err == nil) != wantSuccess {
					t.Fatalf("TLS policy: success=%t want=%t err=%v", err == nil, wantSuccess, err)
				}
				if wantSuccess {
					c := &client{t: t, conn: secure, r: bufio.NewReader(secure)}
					if !startTLS {
						_ = c.readLine()
					}
					c.login("auth")
				}
			})
		}
	}
}

func TestIMAPTLSHandshakeDeadlineClearedAfterUpgrade(t *testing.T) {
	for _, startTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("starttls=%t", startTLS), func(t *testing.T) {
			serverConfig, clientConfig := imapTLSConfigs(t)
			_, ln := startConfiguredIMAP(t, zerolog.Nop(), func(s *imapserver.Server) {
				s.TLSConfig, s.StartTLS = serverConfig.Clone(), startTLS
				s.TLSHandshakeTimeout = 200 * time.Millisecond
			})
			var c *client
			if startTLS {
				c, _ = plaintextIMAPClient(t, ln)
				upgradeIMAPClient(t, c, clientConfig)
			} else {
				conn := tls.Client(rawIMAPConn(t, ln), clientConfig)
				if err := conn.Handshake(); err != nil {
					t.Fatal(err)
				}
				c = &client{t: t, conn: conn, r: bufio.NewReader(conn)}
				_ = c.readLine()
			}
			time.Sleep(300 * time.Millisecond)
			if reply := joined(c.cmd("noop", "NOOP")); !strings.Contains(reply, "noop OK") {
				t.Fatalf("handshake deadline leaked into protocol session: %s", reply)
			}
			c.login("auth")
		})
	}
}

func TestIMAPSTARTTLSUnavailableAndMalformed(t *testing.T) {
	for _, mode := range []string{"plain", "starttls", "implicit"} {
		t.Run(mode, func(t *testing.T) {
			serverConfig, clientConfig := imapTLSConfigs(t)
			_, ln := startConfiguredIMAP(t, zerolog.Nop(), func(s *imapserver.Server) {
				if mode != "plain" {
					s.TLSConfig = serverConfig.Clone()
				}
				s.StartTLS = mode != "implicit"
			})
			var c *client
			if mode == "implicit" {
				conn := tls.Client(rawIMAPConn(t, ln), clientConfig)
				if err := conn.Handshake(); err != nil {
					t.Fatal(err)
				}
				c = &client{t: t, conn: conn, r: bufio.NewReader(conn)}
				_ = c.readLine()
			} else {
				c, _ = plaintextIMAPClient(t, ln)
			}
			for _, command := range []string{"STARTTLS unexpected", "STARTTLS {1}", "STARTTLS {1+}"} {
				if reply := joined(c.cmd("bad", command)); !strings.Contains(reply, "bad BAD") {
					t.Fatalf("malformed STARTTLS: %s", reply)
				}
			}
			if mode != "starttls" {
				if reply := joined(c.cmd("unavailable", "STARTTLS")); !strings.Contains(reply, "unavailable BAD") {
					t.Fatalf("unavailable STARTTLS: %s", reply)
				}
			}
			if reply := joined(c.cmd("noop", "NOOP")); !strings.Contains(reply, "noop OK") {
				t.Fatalf("protocol after rejected upgrade: %s", reply)
			}
			if mode == "plain" {
				c.login("auth") // StartTLS without a TLSConfig must still preserve plaintext.
				if reply := joined(c.cmd("authenticated", "STARTTLS")); !strings.Contains(reply, "authenticated BAD") {
					t.Fatalf("authenticated unavailable STARTTLS: %s", reply)
				}
			}
		})
	}
}
