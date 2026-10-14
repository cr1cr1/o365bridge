// Package smtpserver is a downstream SMTP submission server: local
// clients authenticate with an account email + the bridge instance
// password, and submitted MIME is handed to the account's upstream
// provider via bridge.Registry.Call (auth renewal/fallback inside).
package smtpserver

import (
	"bufio"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

const (
	hostname = "o365bridge"
	maxSize  = 35882577 // advertised SIZE; also the enforced cap (~35MB)
	maxLine  = 8192
	greeting = "220 " + hostname + " ESMTP o365bridge ready"
	queueFmt = "250 2.0.0 OK queued as %s"
)

// Server serves SMTP submission on a supplied listener.
type Server struct {
	// Configure before Serve. Nil TLSConfig preserves plaintext behavior;
	// otherwise StartTLS selects upgrade mode instead of implicit TLS.
	TLSConfig *tls.Config
	StartTLS  bool
	// TLSHandshakeTimeout defaults to 10 seconds when non-positive.
	TLSHandshakeTimeout time.Duration

	reg      *bridge.Registry
	password string
	logger   zerolog.Logger

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New creates a Server. instancePassword is the shared local password;
// the SMTP username selects the account (its email).
func New(reg *bridge.Registry, instancePassword string, logger zerolog.Logger) *Server {
	return &Server{reg: reg, password: instancePassword, logger: logger, conns: map[net.Conn]struct{}{}}
}

// Serve accepts connections until Close (or a listener error).
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	s.ln = l
	if s.closed {
		s.mu.Unlock()
		_ = l.Close()
		return net.ErrClosed
	}
	s.mu.Unlock()
	for {
		c, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			s.wg.Wait()
			return nil
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.handle(c)
		}()
	}
}

// Close stops listening and drops live connections.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	var err error
	if ln != nil {
		err = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

func (s *Server) checkPassword(pw string) bool {
	return subtle.ConstantTimeCompare([]byte(pw), []byte(s.password)) == 1
}

type session struct {
	authed  bool
	email   string // authenticated account email (normalised)
	hello   string // "EHLO", "HELO", or "" if neither was sent
	remote  string
	from    string
	hasFrom bool
	rcpts   []string
	quit    bool
}

func (sess *session) resetTxn() { sess.from, sess.hasFrom, sess.rcpts = "", false, nil }

func (s *Server) handshakeTLS(c net.Conn) (*tls.Conn, error) {
	timeout := s.TLSHandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	tc := tls.Server(c, s.TLSConfig)
	if err := tc.Handshake(); err != nil {
		return nil, err
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return tc, nil
}

func (s *Server) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	if s.TLSConfig != nil && !s.StartTLS {
		tc, err := s.handshakeTLS(c)
		if err != nil {
			s.logger.Warn().Err(err).Str("protocol", "smtp").Str("op", "tls").Str("remote", c.RemoteAddr().String()).Msg("smtp TLS handshake failed")
			return
		}
		c = tc
	}
	requireTLS := s.TLSConfig != nil && s.StartTLS
	upgraded := false
	r := bufio.NewReader(c)
	sess := &session{remote: c.RemoteAddr().String()}
	s.logger.Info().Str("protocol", "smtp").Str("op", "connect").Str("remote", sess.remote).Msg("smtp connection")
	defer func() {
		s.logger.Info().Str("protocol", "smtp").Str("op", "disconnect").Str("remote", sess.remote).
			Str("account", sess.email).Bool("authed", sess.authed).Msg("smtp session end")
	}()
	writeLine(c, greeting)
	for {
		line, err := readLine(r)
		if err != nil {
			return
		}
		if len(line) > maxLine {
			writeLine(c, "500 5.5.2 Line too long")
			continue
		}
		cmd, arg := splitCmd(line)
		// Every command verb is visible per session (never its
		// argument: AUTH carries credentials).
		s.logger.Info().Str("protocol", "smtp").Str("op", "command").Str("command", cmd).
			Str("remote", sess.remote).Str("account", sess.email).Bool("authed", sess.authed).
			Msg("smtp command")
		if upgraded && sess.hello != "EHLO" {
			switch cmd {
			case "AUTH", "MAIL", "RCPT", "DATA":
				writeLine(c, "503 5.5.1 Send EHLO after STARTTLS first")
				continue
			}
		}
		switch cmd {
		case "EHLO":
			sess.resetTxn()
			sess.hello = cmd
			writeLine(c, "250-"+hostname+" greets "+argOr(arg, "client"))
			if requireTLS {
				writeLine(c, "250-STARTTLS")
			} else {
				writeLine(c, "250-AUTH PLAIN LOGIN")
			}
			writeLine(c, fmt.Sprintf("250-SIZE %d", maxSize))
			writeLine(c, "250 8BITMIME")
		case "HELO":
			sess.resetTxn()
			sess.hello = cmd
			writeLine(c, "250 "+hostname+" greets "+argOr(arg, "client"))
		case "AUTH":
			if requireTLS {
				writeLine(c, "530 5.7.0 Must issue STARTTLS first")
				continue
			}
			s.handleAuth(c, r, sess, arg)
		case "MAIL":
			if requireTLS {
				writeLine(c, "530 5.7.0 Must issue STARTTLS first")
				continue
			}
			if !sess.authed {
				from, _ := parsePath(arg, "FROM:")
				// hello/auth_advertised pinpoint why a client
				// reached MAIL unauthenticated: HELO (or no
				// greeting) means it never saw the AUTH
				// advertisement; EHLO means it saw AUTH yet
				// its effective auth setting said "none".
				s.logger.Warn().Str("protocol", "smtp").Str("op", "mail").Str("remote", sess.remote).
					Str("from", from).Str("hello", argOr(sess.hello, "none")).
					Bool("auth_advertised", sess.hello == "EHLO").
					Msg("smtp MAIL before authentication")
				// RFC 4954 §5: 530 5.7.0 is the standard
				// "authentication required" reply — the signal
				// an MTA uses to authenticate (prompting the
				// user for a password) and then retry. A 503
				// bad-sequence reply would be treated as fatal.
				writeLine(c, "530 5.7.0 Authentication required")
				continue
			}
			addr, ok := parsePathAllow(arg, "FROM:", true)
			if !ok {
				writeLine(c, "501 5.1.7 Bad sender address syntax")
				continue
			}
			if sz, has := mailSizeParam(arg); has && sz > maxSize {
				writeLine(c, "552 5.3.4 Message size exceeds fixed maximum message size")
				continue
			}
			sess.from, sess.hasFrom, sess.rcpts = addr, true, nil
			writeLine(c, "250 2.1.0 OK")
		case "RCPT":
			if !sess.hasFrom {
				writeLine(c, "503 5.5.1 Need MAIL command first")
				continue
			}
			addr, ok := parsePath(arg, "TO:")
			if !ok {
				writeLine(c, "501 5.1.3 Bad recipient address syntax")
				continue
			}
			sess.rcpts = append(sess.rcpts, addr)
			writeLine(c, "250 2.1.5 OK")
		case "DATA":
			if !sess.hasFrom || len(sess.rcpts) == 0 {
				writeLine(c, "503 5.5.1 Need MAIL and RCPT first")
				continue
			}
			s.handleData(c, r, sess)
		case "RSET":
			sess.resetTxn()
			writeLine(c, "250 2.0.0 OK")
		case "NOOP":
			writeLine(c, "250 2.0.0 OK")
		case "VRFY":
			// Never confirm/deny account existence in detail.
			writeLine(c, "252 2.5.2 Cannot VRFY user, send some mail and see")
		case "STARTTLS":
			if s.TLSConfig == nil {
				s.logger.Warn().Str("protocol", "smtp").Str("op", "starttls").Str("remote", sess.remote).
					Str("account", sess.email).Msg("smtp STARTTLS requested but not offered (plain v1)")
				writeLine(c, "454 4.7.0 TLS not available")
				continue
			}
			if arg != "" {
				writeLine(c, "501 5.5.4 STARTTLS takes no arguments")
				continue
			}
			if !requireTLS || sess.hello != "EHLO" || sess.authed || sess.hasFrom {
				writeLine(c, "503 5.5.1 STARTTLS not permitted in current state")
				continue
			}
			// Never carry plaintext read-ahead across the encryption boundary.
			if r.Buffered() != 0 {
				s.logger.Warn().Str("protocol", "smtp").Str("op", "starttls").Str("remote", sess.remote).Msg("smtp STARTTLS rejected buffered plaintext")
				return
			}
			// Writes are unbuffered, so this reply reaches the client before TLS.
			if _, err := fmt.Fprint(c, "220 2.0.0 Ready to start TLS\r\n"); err != nil {
				return
			}
			tc, err := s.handshakeTLS(c)
			if err != nil {
				s.logger.Warn().Err(err).Str("protocol", "smtp").Str("op", "tls").Str("remote", sess.remote).Msg("smtp TLS handshake failed")
				return
			}
			c, r = tc, bufio.NewReader(tc)
			sess = &session{remote: sess.remote}
			requireTLS, upgraded = false, true
		case "QUIT":
			writeLine(c, "221 2.0.0 "+hostname+" closing connection")
			return
		case "":
			writeLine(c, "500 5.5.2 Empty command")
		default:
			writeLine(c, "502 5.5.2 Command not recognised")
		}
		if sess.quit {
			return
		}
	}
}
