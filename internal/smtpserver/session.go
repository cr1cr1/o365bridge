package smtpserver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func writeLine(c net.Conn, s string) {
	_, _ = io.WriteString(c, s+"\r\n")
}

// readLine reads one CRLF (or LF) terminated line, without terminator.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func splitCmd(line string) (cmd, arg string) {
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return strings.ToUpper(line[:i]), strings.TrimSpace(line[i+1:])
	}
	return strings.ToUpper(line), ""
}

// mailSizeParam extracts a SIZE=<n> ESMTP parameter, if present.
func mailSizeParam(arg string) (int64, bool) {
	for _, f := range strings.Fields(arg) {
		if len(f) > 5 && strings.EqualFold(f[:5], "SIZE=") {
			n, err := strconv.ParseInt(f[5:], 10, 64)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func argOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// parsePath parses "FROM:<a@b> [params]" / "TO:<a@b>".
func parsePath(arg, prefix string) (string, bool) {
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", false
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	if !strings.HasPrefix(rest, "<") {
		return "", false
	}
	end := strings.Index(rest, ">")
	if end < 0 {
		return "", false
	}
	addr := rest[1:end]
	// Reject params smuggled inside brackets / empty / no @ / spaces.
	if addr == "" || strings.ContainsAny(addr, " \t<>") || !strings.Contains(addr, "@") {
		return "", false
	}
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", false
	}
	return addr, true
}

func (s *Server) authenticate(sess *session, user, pass string) bool {
	user = bridge.NormalizeEmail(user)
	if user == "" || !strings.Contains(user, "@") || !s.checkPassword(pass) {
		return false
	}
	sess.authed, sess.email = true, user
	return true
}

// logAuth records an SMTP auth attempt per account at human
// levels: success is info, failure is a warning (failures used to
// be debug-only, so a client complaining about authentication left
// no trace in the default log). Passwords are never logged.
func (s *Server) logAuth(sess *session, mech, user string, ok bool) {
	ev := s.logger.Info()
	if !ok {
		ev = s.logger.Warn()
	}
	ev.Str("protocol", "smtp").Str("op", "auth").Str("mech", mech).
		Str("account", bridge.NormalizeEmail(user)).Str("remote", sess.remote).
		Bool("ok", ok).Msg("smtp authentication")
}

func (s *Server) handleAuth(c net.Conn, r *bufio.Reader, sess *session, arg string) {
	if sess.authed {
		writeLine(c, "503 5.5.1 Already authenticated")
		return
	}
	mech, rest := splitCmd(arg)
	switch mech {
	case "PLAIN":
		raw := rest
		if raw == "" {
			writeLine(c, "334 ")
			l, err := readLine(r)
			if err != nil {
				sess.quit = true
				return
			}
			raw = l
		}
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
		if err != nil {
			writeLine(c, "501 5.5.4 Malformed authentication data")
			return
		}
		parts := strings.Split(string(dec), "\x00")
		var user, pass string
		switch len(parts) {
		case 3: // authzid NUL username NUL password
			user, pass = parts[1], parts[2]
		case 2: // username NUL password (no authzid)
			user, pass = parts[0], parts[1]
		default:
			s.logAuth(sess, mech, "", false)
			writeLine(c, "501 5.5.4 Malformed authentication data")
			return
		}
		// Password is never logged.
		if s.authenticate(sess, user, pass) {
			s.logAuth(sess, mech, user, true)
			writeLine(c, "235 2.7.0 Authentication successful")
		} else {
			s.logAuth(sess, mech, user, false)
			writeLine(c, "535 5.7.8 Authentication credentials invalid")
		}
	case "LOGIN":
		userRaw := rest
		if userRaw == "" {
			writeLine(c, "334 VXNlcm5hbWU6") // "Username:"
			l, err := readLine(r)
			if err != nil {
				sess.quit = true
				return
			}
			userRaw = l
		}
		user, err := base64.StdEncoding.DecodeString(strings.TrimSpace(userRaw))
		if err != nil {
			writeLine(c, "501 5.5.4 Malformed authentication data")
			return
		}
		writeLine(c, "334 UGFzc3dvcmQ6") // "Password:"
		l, err := readLine(r)
		if err != nil {
			sess.quit = true
			return
		}
		pass, err := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
		if err != nil {
			writeLine(c, "501 5.5.4 Malformed authentication data")
			return
		}
		if s.authenticate(sess, string(user), string(pass)) {
			s.logAuth(sess, mech, string(user), true)
			writeLine(c, "235 2.7.0 Authentication successful")
		} else {
			s.logAuth(sess, mech, string(user), false)
			writeLine(c, "535 5.7.8 Authentication credentials invalid")
		}
	case "":
		writeLine(c, "501 5.5.4 Authentication mechanism required")
	default:
		writeLine(c, "504 5.5.4 Unrecognised authentication type")
	}
}

func (s *Server) handleData(c net.Conn, r *bufio.Reader, sess *session) {
	writeLine(c, "354 End data with <CR><LF>.<CR><LF>")
	var buf strings.Builder
	tooBig := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			sess.quit = true
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			break
		}
		// Dot-unstuffing: a leading ".." loses one dot.
		trimmed = strings.TrimPrefix(trimmed, ".")
		if !tooBig {
			if buf.Len()+len(trimmed)+2 > maxSize {
				tooBig = true
			} else {
				buf.WriteString(trimmed)
				buf.WriteString("\r\n")
			}
		}
	}
	from, rcpts, email := sess.from, sess.rcpts, sess.email
	sess.resetTxn()
	if tooBig {
		writeLine(c, fmt.Sprintf("552 5.3.4 Message exceeds maximum size %d", maxSize))
		return
	}
	mime := []byte(buf.String())
	// Upstream access only via Registry.Call; body never logged.
	err := s.reg.Call(context.Background(), email, func(ctx context.Context, p bridge.Provider) error {
		return p.SendMessage(ctx, mime)
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("protocol", "smtp").Str("op", "send").Str("account", email).Int("size", len(mime)).Msg("smtp send failed upstream")
		writeLine(c, "451 4.3.0 Temporary failure, please try again later")
		return
	}
	id := queueID()
	s.logger.Info().Str("protocol", "smtp").Str("op", "send").Str("account", email).Str("queue", id).Int("size", len(mime)).Int("rcpts", len(rcpts)).Str("from", from).Msg("smtp message accepted")
	writeLine(c, fmt.Sprintf(queueFmt, id))
}

func queueID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "queue"
	}
	return hex.EncodeToString(b[:])
}
