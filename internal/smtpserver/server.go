// Package smtpserver is a downstream SMTP submission server: local
// clients authenticate with an account email + the bridge instance
// password, and submitted MIME is handed to the account's upstream
// provider via bridge.Registry.Call (auth renewal/fallback inside).
package smtpserver

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"sync"

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
	remote  string
	from    string
	hasFrom bool
	rcpts   []string
	quit    bool
}

func (sess *session) resetTxn() { sess.from, sess.hasFrom, sess.rcpts = "", false, nil }

func (s *Server) handle(c net.Conn) {
	r := bufio.NewReader(c)
	sess := &session{remote: c.RemoteAddr().String()}
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
		switch cmd {
		case "EHLO":
			sess.resetTxn()
			writeLine(c, "250-"+hostname+" greets "+argOr(arg, "client"))
			writeLine(c, "250-AUTH PLAIN LOGIN")
			writeLine(c, fmt.Sprintf("250-SIZE %d", maxSize))
			writeLine(c, "250 8BITMIME")
		case "HELO":
			sess.resetTxn()
			writeLine(c, "250 "+hostname+" greets "+argOr(arg, "client"))
		case "AUTH":
			s.handleAuth(c, r, sess, arg)
		case "MAIL":
			if !sess.authed {
				s.logger.Warn().Str("protocol", "smtp").Str("op", "mail").Str("remote", sess.remote).Msg("smtp MAIL before authentication")
				writeLine(c, "503 5.5.1 Authentication required")
				continue
			}
			addr, ok := parsePath(arg, "FROM:")
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
			// Plain v1 by design: no TLS listener is offered.
			writeLine(c, "454 4.7.0 TLS not available")
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
