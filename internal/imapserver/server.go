package imapserver

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

const capability = "IMAP4rev1 IDLE MOVE UIDPLUS NAMESPACE UNSELECT"

type Server struct {
	// Configure TLS before Serve. Nil preserves plaintext; StartTLS selects upgrade mode.
	TLSConfig *tls.Config
	StartTLS  bool
	// TLSHandshakeTimeout bounds handshakes. Default 10s.
	TLSHandshakeTimeout time.Duration

	// IdlePollInterval is the IDLE/NOOP poll period. Default 10s.
	IdlePollInterval time.Duration

	// SentDedupeRetryDelay spaces the Sent-copy dedupe lookup retries.
	// Clients append milliseconds after the send, while the upstream
	// listing can lag briefly. Default 400ms.
	SentDedupeRetryDelay time.Duration

	reg      *bridge.Registry
	password string
	logger   zerolog.Logger

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	idmaps   map[string]*bridge.IDMap
	closed   bool
	wg       sync.WaitGroup
}

func New(reg *bridge.Registry, instancePassword string, logger zerolog.Logger) *Server {
	return &Server{
		reg: reg, password: instancePassword, logger: logger,
		IdlePollInterval:     10 * time.Second,
		SentDedupeRetryDelay: 400 * time.Millisecond,
		conns:                map[net.Conn]struct{}{},
		idmaps:               map[string]*bridge.IDMap{},
	}
}

func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	s.listener = l
	if s.closed {
		s.mu.Unlock()
		_ = l.Close()
		return nil
	}
	s.mu.Unlock()
	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				_ = conn.Close()
				_ = recover()
			}()
			s.handle(conn)
		}()
	}
}

func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.listener
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

func (s *Server) getIDMap(email string) (*bridge.IDMap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.idmaps[email]; ok {
		return m, nil
	}
	m, err := s.reg.Store().IDMap(email)
	if err != nil {
		return nil, err
	}
	s.idmaps[email] = m
	return m, nil
}

func (s *Server) pollInterval() time.Duration {
	if s.IdlePollInterval <= 0 {
		return 10 * time.Second
	}
	return s.IdlePollInterval
}

// ---------- session ----------

type entry struct {
	meta bridge.MessageMeta
	uid  uint32
}

type selected struct {
	name     string // display name as selected
	folder   bridge.Folder
	readonly bool
	msgs     []entry
}

type session struct {
	srv     *Server
	conn    net.Conn
	secure  bool
	r       *bufio.Reader
	wmu     sync.Mutex
	w       *bufio.Writer
	email   string
	remote  string
	cmd     string // current command, for log context
	idmap   *bridge.IDMap
	sel     *selected
	deleted map[uint32]bool // uids flagged \Deleted by this session
}

func (ss *session) writeLine(line string) error {
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	_, err := ss.w.WriteString(line + "\r\n")
	if err != nil {
		return err
	}
	return ss.w.Flush()
}

func (ss *session) printf(format string, args ...any) error {
	return ss.writeLine(fmt.Sprintf(format, args...))
}

func (ss *session) tagged(tag, status, text string) error {
	return ss.printf("%s %s %s", tag, status, sanitize(text))
}

// no reports an upstream failure to the client AND leaves a
// warning in the bridge log, per account and operation (these
// failures used to be invisible above debug).
func (ss *session) no(tag string, err error) error {
	ss.srv.logger.Warn().Err(err).Str("protocol", "imap").Str("op", strings.ToLower(ss.cmd)).
		Str("account", ss.email).Msg("imap operation failed")
	return ss.tagged(tag, "NO", err.Error())
}

// logAuth records an IMAP auth attempt per account: info on
// success, warning on failure. Passwords are never logged.
func (ss *session) logAuth(user string, ok bool) {
	ev := ss.srv.logger.Info()
	if !ok {
		ev = ss.srv.logger.Warn()
	}
	ev.Str("protocol", "imap").Str("op", "auth").
		Str("account", bridge.NormalizeEmail(user)).Bool("ok", ok).Msg("imap authentication")
}

func (ss *session) call(op func(context.Context, bridge.Provider) error) error {
	// A suspended upstream answers reads with success and zero items;
	// fail commands with the recorded reason instead of letting the
	// client see silently empty folders.
	if reason, degraded := ss.srv.reg.DegradedReason(ss.email); degraded {
		return fmt.Errorf("bridge: %s", reason)
	}
	return ss.srv.reg.Call(context.Background(), ss.email, op)
}

func (s *Server) handle(conn net.Conn) {
	ss := &session{srv: s, conn: conn, remote: conn.RemoteAddr().String(), r: bufio.NewReader(conn), w: bufio.NewWriter(conn), deleted: map[uint32]bool{}}
	defer func() { _ = ss.conn.Close() }()
	defer func() { _ = recover() }()
	s.logger.Info().Str("protocol", "imap").Str("op", "connect").Str("remote", ss.remote).Msg("imap connection")
	if s.TLSConfig != nil && !s.StartTLS && !ss.handshakeTLS() {
		return
	}
	_ = ss.printf("* OK [CAPABILITY %s] o365bridge ready", ss.capabilities())
	for {
		line, lits, err := ss.readCommand()
		if err != nil {
			return
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !ss.dispatch(line, lits) {
			return
		}
	}
}

func (ss *session) requiresTLS() bool {
	return ss.srv.TLSConfig != nil && ss.srv.StartTLS && !ss.secure
}

func (ss *session) capabilities() string {
	if ss.requiresTLS() {
		return capability + " STARTTLS LOGINDISABLED"
	}
	if ss.secure {
		return capability + " AUTH=PLAIN"
	}
	return capability
}

func (ss *session) cmdStartTLS(tag, rest string) bool {
	if strings.TrimSpace(rest) != "" {
		_ = ss.tagged(tag, "BAD", "STARTTLS takes no arguments")
		return true
	}
	if !ss.requiresTLS() || ss.email != "" {
		_ = ss.tagged(tag, "BAD", "STARTTLS unavailable in this state")
		return true
	}
	// Never carry plaintext read-ahead across the TLS boundary.
	if ss.r.Buffered() != 0 {
		_ = ss.tagged(tag, "BAD", "STARTTLS cannot be pipelined")
		return false
	}
	if err := ss.tagged(tag, "OK", "Begin TLS negotiation"); err != nil {
		return false
	}
	return ss.handshakeTLS()
}

func (ss *session) handshakeTLS() bool {
	timeout := ss.srv.TLSHandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	conn := tls.Server(ss.conn, ss.srv.TLSConfig)
	err := conn.SetDeadline(time.Now().Add(timeout))
	if err == nil {
		err = conn.Handshake()
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		ss.srv.logger.Warn().Err(err).Str("protocol", "imap").Str("op", "tls").
			Str("remote", ss.remote).Msg("imap TLS handshake failed")
		return false
	}
	ss.conn = conn
	ss.r = bufio.NewReader(conn)
	ss.w = bufio.NewWriter(conn)
	ss.secure = true
	return true
}

// readCommand reads one logical IMAP command, expanding literals.
// A synchronizing literal ({n} without '+') requires the server to
// send a continuation request before the client sends the data —
// without it, real clients (Thunderbird APPEND) wait forever while
// the server waits for bytes: a deadlock. Non-synchronizing
// literals ({n+}) need no continuation.
func (ss *session) readCommand() (string, [][]byte, error) {
	r, w := ss.r, ss.w
	var full strings.Builder
	var lits [][]byte
	for {
		seg, err := r.ReadString('\n')
		if err != nil {
			if len(seg) == 0 && full.Len() == 0 {
				return "", nil, err
			}
			if err == io.EOF && full.Len() > 0 {
				// fall through with what we have
			} else if err != nil {
				return "", nil, err
			}
		}
		trimmed := strings.TrimRight(seg, "\r\n")
		if full.Len() == 0 {
			_, rest := splitFirst(trimmed)
			cmd, _ := splitFirst(rest)
			cmdUpper := strings.ToUpper(cmd)
			// Match dispatch's normalization before soliciting any literal data.
			if cmdUpper == "STARTTLS" || (ss.requiresTLS() && (cmdUpper == "LOGIN" || cmdUpper == "AUTHENTICATE")) {
				return trimmed, nil, nil
			}
		}
		if idx, n, sync, ok := literalSuffix(trimmed); ok {
			if sync {
				if _, err := w.WriteString("+ Ready for literal data\r\n"); err != nil {
					return "", nil, err
				}
				if err := w.Flush(); err != nil {
					return "", nil, err
				}
			}
			lit := make([]byte, n)
			if _, err := io.ReadFull(r, lit); err != nil {
				return "", nil, err
			}
			lits = append(lits, lit)
			full.WriteString(trimmed[:idx])
			full.WriteByte(0)
			full.WriteString(strconv.Itoa(len(lits) - 1))
			full.WriteByte(0)
			// The literal is the last argument of the only
			// literal-taking command we support (APPEND): the
			// command is complete. Only read a remainder if the
			// client already pipelined one (e.g. a trailing CRLF);
			// otherwise we would block waiting for bytes a real
			// client never sends.
			if err == io.EOF || r.Buffered() == 0 {
				break
			}
			continue
		}
		full.WriteString(trimmed)
		break
	}
	return full.String(), lits, nil
}

// literalSuffix parses a trailing {n} / {n+} literal marker;
// sync reports whether it is a synchronizing literal ({n}, which
// requires a continuation request).
func literalSuffix(s string) (idx, n int, sync bool, ok bool) {
	if !strings.HasSuffix(s, "}") {
		return 0, 0, false, false
	}
	i := strings.LastIndex(s, "{")
	if i < 0 {
		return 0, 0, false, false
	}
	inner := s[i+1 : len(s)-1]
	sync = !strings.HasSuffix(inner, "+")
	inner = strings.TrimSuffix(inner, "+")
	if inner == "" {
		return 0, 0, false, false
	}
	for _, c := range inner {
		if c < '0' || c > '9' {
			return 0, 0, false, false
		}
	}
	v, err := strconv.Atoi(inner)
	if err != nil {
		return 0, 0, false, false
	}
	return i, v, sync, true
}

// dispatch returns false when the connection should close.
func (ss *session) dispatch(line string, lits [][]byte) bool {
	tag, rest := splitFirst(line)
	if tag == "" {
		return true
	}
	cmd, rest := splitFirst(rest)
	cmdUpper := strings.ToUpper(cmd)
	// compound UID command
	uidMode := false
	if cmdUpper == "UID" {
		sub, r2 := splitFirst(rest)
		cmdUpper = "UID " + strings.ToUpper(sub)
		uidMode = true
		rest = r2
	}
	ss.cmd = cmdUpper
	ev := ss.srv.logger.Trace().Str("protocol", "imap").Str("op", strings.ToLower(cmdUpper)).
		Str("account", ss.email).Str("remote", ss.remote).Str("tag", tag).
		Str("cmd", cmdUpper).Bool("auth", ss.email != "").Bool("secure", ss.secure)
	if ss.sel != nil {
		ev = ev.Str("mailbox", ss.sel.name).Bool("readonly", ss.sel.readonly)
	}
	ev.Msg("imap command")

	if ss.requiresTLS() && (cmdUpper == "LOGIN" || cmdUpper == "AUTHENTICATE") {
		_ = ss.tagged(tag, "NO", "[PRIVACYREQUIRED] TLS is required before authentication")
		// A literal's unread payload cannot safely be treated as the next command.
		_, _, _, literal := literalSuffix(rest)
		return !literal
	}

	switch cmdUpper {
	case "STARTTLS":
		return ss.cmdStartTLS(tag, rest)
	case "CAPABILITY":
		_ = ss.printf("* CAPABILITY %s", ss.capabilities())
		_ = ss.tagged(tag, "OK", "CAPABILITY completed")
	case "LOGOUT":
		_ = ss.printf("* BYE o365bridge logging out")
		_ = ss.tagged(tag, "OK", "LOGOUT completed")
		return false
	case "LOGIN":
		ss.cmdLogin(tag, rest, lits)
	case "AUTHENTICATE":
		return ss.cmdAuthenticate(tag, rest, lits)
	case "NOOP":
		if ss.sel != nil {
			_ = ss.pollChanges()
		}
		_ = ss.tagged(tag, "OK", "NOOP completed")
	default:
		if ss.email == "" {
			ss.srv.logger.Warn().Str("protocol", "imap").Str("op", strings.ToLower(cmdUpper)).
				Str("remote", ss.remote).Msg("imap command before authentication")
			_ = ss.tagged(tag, "NO", "not authenticated")
			return true
		}
		switch cmdUpper {
		case "NAMESPACE":
			_ = ss.printf(`* NAMESPACE (("" "/")) NIL NIL`)
			_ = ss.tagged(tag, "OK", "NAMESPACE completed")
		case "IDLE":
			return ss.cmdIdle(tag)
		case "LIST", "LSUB":
			ss.cmdList(tag, cmdUpper, rest, lits)
		case "SELECT", "EXAMINE":
			ss.cmdSelect(tag, rest, lits, cmdUpper == "EXAMINE")
		case "UNSELECT":
			ss.sel = nil
			ss.deleted = map[uint32]bool{}
			_ = ss.tagged(tag, "OK", "UNSELECT completed")
		case "STATUS":
			ss.cmdStatus(tag, rest, lits)
		case "CREATE":
			ss.cmdCreate(tag, rest, lits)
		case "DELETE":
			ss.cmdDelete(tag, rest, lits)
		case "RENAME":
			ss.cmdRename(tag, rest, lits)
		case "SUBSCRIBE", "UNSUBSCRIBE":
			_ = ss.tagged(tag, "OK", cmdUpper+" completed")
		case "APPEND":
			ss.cmdAppend(tag, rest, lits)
		case "CHECK":
			if ss.sel != nil {
				_ = ss.pollChanges()
			}
			_ = ss.tagged(tag, "OK", "CHECK completed")
		case "CLOSE":
			ss.cmdClose(tag)
		case "EXPUNGE", "UID EXPUNGE":
			ss.cmdExpunge(tag)
		case "SEARCH", "UID SEARCH":
			ss.cmdSearch(tag, rest, uidMode)
		case "FETCH", "UID FETCH":
			ss.cmdFetch(tag, rest, lits, uidMode)
		case "STORE", "UID STORE":
			ss.cmdStore(tag, rest, lits, uidMode)
		case "COPY", "UID COPY":
			ss.cmdCopyMove(tag, rest, lits, uidMode, false)
		case "MOVE", "UID MOVE":
			ss.cmdCopyMove(tag, rest, lits, uidMode, true)
		default:
			_ = ss.tagged(tag, "BAD", "unknown command")
		}
	}
	return true
}

func splitFirst(s string) (first, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

func (ss *session) checkPassword(user, pass string) bool {
	got := []byte(pass)
	want := []byte(ss.srv.password)
	ok := subtle.ConstantTimeCompare(got, want) == 1 && len(got) == len(want)
	return ok && strings.TrimSpace(user) != ""
}

func (ss *session) setAuth(user string) error {
	ss.email = bridge.NormalizeEmail(user)
	m, err := ss.srv.getIDMap(ss.email)
	if err != nil {
		return err
	}
	ss.idmap = m
	return nil
}

func (ss *session) cmdLogin(tag, rest string, lits [][]byte) {
	p := &parser{s: rest, lits: lits}
	user, err1 := p.astring()
	pass, err2 := p.astring()
	if err1 != nil || err2 != nil {
		_ = ss.tagged(tag, "BAD", "LOGIN requires user and password")
		return
	}
	if !ss.checkPassword(user, pass) {
		ss.logAuth(user, false)
		_ = ss.tagged(tag, "NO", "authentication failed")
		return
	}
	if err := ss.setAuth(user); err != nil {
		_ = ss.no(tag, err)
		return
	}
	ss.logAuth(user, true)
	_ = ss.printf("%s OK [CAPABILITY %s] LOGIN completed", tag, ss.capabilities())
}

func (ss *session) cmdAuthenticate(tag, rest string, lits [][]byte) bool {
	p := &parser{s: rest, lits: lits}
	mech, err := p.astring()
	if err != nil || !strings.EqualFold(mech, "PLAIN") {
		_ = ss.tagged(tag, "NO", "unsupported authentication mechanism")
		return true
	}
	var encoded string
	if tok, err := p.astring(); err == nil && tok != "" {
		encoded = tok
	} else {
		if err := ss.writeLine("+ "); err != nil {
			return false
		}
		line, err := ss.r.ReadString('\n')
		if err != nil {
			return false
		}
		encoded = strings.TrimSpace(line)
		if encoded == "*" {
			_ = ss.tagged(tag, "NO", "authentication cancelled")
			return true
		}
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// tolerate missing padding
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
		if err != nil {
			_ = ss.tagged(tag, "BAD", "invalid base64")
			return true
		}
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		_ = ss.tagged(tag, "BAD", "invalid PLAIN payload")
		return true
	}
	user, pass := parts[1], parts[2]
	if user == "" {
		user = parts[0]
	}
	if !ss.checkPassword(user, pass) {
		ss.logAuth(user, false)
		_ = ss.tagged(tag, "NO", "authentication failed")
		return true
	}
	if err := ss.setAuth(user); err != nil {
		_ = ss.no(tag, err)
		return true
	}
	ss.logAuth(user, true)
	_ = ss.printf("%s OK [CAPABILITY %s] AUTHENTICATE completed", tag, ss.capabilities())
	return true
}

// ---------- folders ----------

func displayName(f bridge.Folder, all []bridge.Folder) string {
	if f.Special == "inbox" {
		return "INBOX"
	}
	name := f.Name
	if f.ParentID != "" {
		for _, pf := range all {
			if pf.ID == f.ParentID {
				pd := displayName(pf, all)
				if strings.Contains(name, "/") {
					return name
				}
				return pd + "/" + name
			}
		}
	}
	return name
}

func (ss *session) listFolders() ([]bridge.Folder, error) {
	var out []bridge.Folder
	err := ss.call(func(ctx context.Context, p bridge.Provider) error {
		f, err := p.ListFolders(ctx)
		if err != nil {
			return err
		}
		out = f
		return nil
	})
	return out, err
}

func (ss *session) resolveFolder(name string) (bridge.Folder, string, error) {
	folders, err := ss.listFolders()
	if err != nil {
		return bridge.Folder{}, "", err
	}
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, "INBOX") {
		for _, f := range folders {
			if f.Special == "inbox" {
				return f, "INBOX", nil
			}
		}
	}
	for _, f := range folders {
		d := displayName(f, folders)
		if strings.EqualFold(d, name) || strings.EqualFold(f.Name, name) {
			return f, d, nil
		}
	}
	return bridge.Folder{}, "", fmt.Errorf("mailbox does not exist: %s", name)
}

func (ss *session) cmdList(tag, cmd, rest string, lits [][]byte) {
	p := &parser{s: rest, lits: lits}
	ref, err1 := p.astring()
	pat, err2 := p.astring()
	if err1 != nil || err2 != nil {
		_ = ss.tagged(tag, "BAD", cmd+" requires reference and pattern")
		return
	}
	if pat == "" {
		_ = ss.printf(`* %s (\Noselect) "/" ""`, cmd)
		_ = ss.tagged(tag, "OK", cmd+" completed")
		return
	}
	folders, err := ss.listFolders()
	if err != nil {
		_ = ss.no(tag, err)
		return
	}
	pattern := pat
	if ref != "" {
		if strings.HasSuffix(ref, "/") {
			pattern = ref + pat
		} else {
			pattern = ref + pat
		}
	}
	type item struct {
		folder  bridge.Folder
		display string
	}
	var items []item
	for _, f := range folders {
		d := displayName(f, folders)
		items = append(items, item{f, d})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].display < items[j].display })
	for _, it := range items {
		if !imapMatch(pattern, it.display) {
			continue
		}
		hasChild := false
		for _, o := range items {
			if o.folder.ParentID == it.folder.ID {
				hasChild = true
			}
		}
		attrs := []string{}
		switch it.folder.Special {
		case "inbox":
			attrs = append(attrs, "\\Inbox")
		case "sent":
			attrs = append(attrs, "\\Sent")
		case "drafts":
			attrs = append(attrs, "\\Drafts")
		case "trash":
			attrs = append(attrs, "\\Trash")
		case "junk":
			attrs = append(attrs, "\\Junk")
		case "archive":
			attrs = append(attrs, "\\Archive")
		}
		if hasChild {
			attrs = append(attrs, "\\HasChildren")
		} else {
			attrs = append(attrs, "\\HasNoChildren")
		}
		_ = ss.printf("* %s (%s) \"/\" %s", cmd, strings.Join(attrs, " "), quote(it.display))
	}
	_ = ss.tagged(tag, "OK", cmd+" completed")
}

func (ss *session) listEntries(folderID string) ([]entry, error) {
	var metas []bridge.MessageMeta
	err := ss.call(func(ctx context.Context, p bridge.Provider) error {
		m, _, err := p.ListMessages(ctx, folderID, 0, 100000)
		if err != nil {
			return err
		}
		metas = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(metas, func(i, j int) bool {
		if metas[i].Date.Equal(metas[j].Date) {
			return metas[i].ID < metas[j].ID
		}
		return metas[i].Date.Before(metas[j].Date)
	})
	if ss.idmap == nil {
		m, err := ss.srv.getIDMap(ss.email)
		if err != nil {
			return nil, err
		}
		ss.idmap = m
	}
	out := make([]entry, 0, len(metas))
	for _, m := range metas {
		uid, err := ss.idmap.UID(folderID, m.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, entry{meta: m, uid: uid})
	}
	return out, nil
}

func nextUID(entries []entry) uint32 {
	var max uint32
	for _, e := range entries {
		if e.uid > max {
			max = e.uid
		}
	}
	return max + 1
}

func unseenCount(entries []entry) int {
	n := 0
	for _, e := range entries {
		if !hasBridgeFlag([]string(e.meta.Flags), "\\seen") {
			n++
		}
	}
	return n
}

func (ss *session) refreshSelected() error {
	if ss.sel == nil {
		return fmt.Errorf("no mailbox selected")
	}
	entries, err := ss.listEntries(ss.sel.folder.ID)
	if err != nil {
		return err
	}
	ss.sel.msgs = entries
	// prune deleted set
	live := map[uint32]bool{}
	for _, e := range entries {
		live[e.uid] = true
	}
	for uid := range ss.deleted {
		if !live[uid] {
			delete(ss.deleted, uid)
		}
	}
	return nil
}

func (ss *session) cmdSelect(tag, rest string, lits [][]byte, examine bool) {
	p := &parser{s: rest, lits: lits}
	name, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "SELECT requires mailbox")
		return
	}
	folder, display, err := ss.resolveFolder(name)
	if err != nil {
		_ = ss.tagged(tag, "NO", sanitize(err.Error()))
		return
	}
	ss.sel = &selected{name: display, folder: folder, readonly: examine}
	ss.deleted = map[uint32]bool{}
	if err := ss.refreshSelected(); err != nil {
		ss.sel = nil
		_ = ss.no(tag, err)
		return
	}
	validity := ss.idmap.UIDValidity(folder.ID)
	_ = ss.printf(`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft \Recent)`)
	_ = ss.printf(`* OK [PERMANENTFLAGS (\Answered \Flagged \Deleted \Seen \Draft)] Permanent flags`)
	_ = ss.printf("* %d EXISTS", len(ss.sel.msgs))
	_ = ss.printf("* 0 RECENT")
	_ = ss.printf("* OK [UIDVALIDITY %d] UIDs valid", validity)
	_ = ss.printf("* OK [UIDNEXT %d] Predicted next UID", nextUID(ss.sel.msgs))
	_ = ss.printf("* OK [UNSEEN %d] Unseen messages", unseenCount(ss.sel.msgs))
	mode := "READ-WRITE"
	verb := "SELECT"
	if examine {
		mode = "READ-ONLY"
		verb = "EXAMINE"
	}
	ss.srv.logger.Info().Str("protocol", "imap").Str("op", "select").Str("account", ss.email).
		Str("folder", display).Str("folder_id", folder.ID).Int("messages", len(ss.sel.msgs)).Msg("imap select")
	_ = ss.printf("%s OK [%s] %s completed", tag, mode, verb)
}

func (ss *session) cmdStatus(tag, rest string, lits [][]byte) {
	p := &parser{s: rest, lits: lits}
	name, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "STATUS requires mailbox")
		return
	}
	itemsStr := extractParen(p.rest())
	folder, display, err := ss.resolveFolder(name)
	if err != nil {
		_ = ss.tagged(tag, "NO", sanitize(err.Error()))
		return
	}
	entries, err := ss.listEntries(folder.ID)
	if err != nil {
		_ = ss.no(tag, err)
		return
	}
	validity := ss.idmap.UIDValidity(folder.ID)
	var parts []string
	for _, it := range strings.Fields(itemsStr) {
		switch strings.ToUpper(it) {
		case "MESSAGES":
			parts = append(parts, fmt.Sprintf("MESSAGES %d", len(entries)))
		case "UNSEEN":
			parts = append(parts, fmt.Sprintf("UNSEEN %d", unseenCount(entries)))
		case "UIDNEXT":
			parts = append(parts, fmt.Sprintf("UIDNEXT %d", nextUID(entries)))
		case "UIDVALIDITY":
			parts = append(parts, fmt.Sprintf("UIDVALIDITY %d", validity))
		case "RECENT":
			parts = append(parts, "RECENT 0")
		}
	}
	_ = ss.printf("* STATUS %s (%s)", quote(display), strings.Join(parts, " "))
	_ = ss.tagged(tag, "OK", "STATUS completed")
}

func (ss *session) cmdCreate(tag, rest string, lits [][]byte) {
	p := &parser{s: rest, lits: lits}
	name, err := p.astring()
	if err != nil || strings.Trim(name, "/") == "" {
		_ = ss.tagged(tag, "BAD", "CREATE requires mailbox name")
		return
	}
	name = strings.TrimSuffix(name, "/")
	leaf := name
	parentID := ""
	if i := strings.LastIndex(name, "/"); i >= 0 {
		parentName, l := name[:i], name[i+1:]
		leaf = l
		if pf, _, err := ss.resolveFolder(parentName); err == nil {
			parentID = pf.ID
		}
	}
	err = ss.call(func(ctx context.Context, p bridge.Provider) error {
		_, err := p.CreateFolder(ctx, leaf, parentID)
		return err
	})
	if err != nil {
		_ = ss.no(tag, err)
		return
	}
	_ = ss.tagged(tag, "OK", "CREATE completed")
}

func (ss *session) cmdDelete(tag, rest string, lits [][]byte) {
	p := &parser{s: rest, lits: lits}
	name, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "DELETE requires mailbox")
		return
	}
	folder, _, err := ss.resolveFolder(name)
	if err != nil {
		_ = ss.tagged(tag, "NO", sanitize(err.Error()))
		return
	}
	err = ss.call(func(ctx context.Context, p bridge.Provider) error {
		return p.DeleteFolder(ctx, folder.ID)
	})
	if err != nil {
		_ = ss.no(tag, err)
		return
	}
	if ss.idmap != nil {
		_ = ss.idmap.Forget(folder.ID)
	}
	if ss.sel != nil && ss.sel.folder.ID == folder.ID {
		ss.sel = nil
	}
	_ = ss.tagged(tag, "OK", "DELETE completed")
}

func (ss *session) cmdRename(tag, rest string, lits [][]byte) {
	p := &parser{s: rest, lits: lits}
	oldName, err1 := p.astring()
	newName, err2 := p.astring()
	if err1 != nil || err2 != nil {
		_ = ss.tagged(tag, "BAD", "RENAME requires old and new names")
		return
	}
	folder, _, err := ss.resolveFolder(oldName)
	if err != nil {
		_ = ss.tagged(tag, "NO", sanitize(err.Error()))
		return
	}
	leaf := newName
	if i := strings.LastIndex(newName, "/"); i >= 0 {
		leaf = newName[i+1:]
	}
	err = ss.call(func(ctx context.Context, p bridge.Provider) error {
		return p.RenameFolder(ctx, folder.ID, leaf)
	})
	if err != nil {
		_ = ss.no(tag, err)
		return
	}
	if ss.sel != nil && ss.sel.folder.ID == folder.ID {
		ss.sel.name = newName
	}
	_ = ss.tagged(tag, "OK", "RENAME completed")
}

func (ss *session) cmdAppend(tag, rest string, lits [][]byte) {
	p := &parser{s: rest, lits: lits}
	name, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "APPEND requires mailbox")
		return
	}
	var flags bridge.Flags
	// optional flag list
	if p.peek() == '(' {
		inner, err := p.paren()
		if err != nil {
			_ = ss.tagged(tag, "BAD", "bad flag list")
			return
		}
		for _, f := range parseFlagList("(" + inner + ")") {
			flags = append(flags, bridge.Flags{toBridgeFlag(f)}...)
		}
	}
	// optional date-time (quoted or atom)
	if p.peek() == '"' {
		if _, err := p.astring(); err != nil {
			_ = ss.tagged(tag, "BAD", "bad date-time")
			return
		}
	}
	raw, ok := p.literalBytes()
	if !ok {
		// literal may be the astring fallback (small clients) — treat as bytes
		s, err := p.astring()
		if err != nil {
			_ = ss.tagged(tag, "BAD", "APPEND requires a literal message")
			return
		}
		raw = []byte(s)
	}
	folder, _, err := ss.resolveFolder(name)
	if err != nil {
		_ = ss.tagged(tag, "NO", sanitize(err.Error()))
		return
	}
	// The upstream send already stored a Sent copy (SendAndSaveCopy /
	// sendMail); a client that also "places a copy in Sent" (the
	// Thunderbird default) would create a duplicate. Compare the
	// appended Message-ID against the newest page of the folder — the
	// send copy is the newest item — and skip storing when it is
	// already there. Gmail-like semantics: deleting the visible copy
	// deletes the sent mail itself. Clients append within milliseconds
	// of the send, while the upstream listing may lag or hiccup, so
	// the lookup retries briefly before giving up and storing.
	if folder.Special == "sent" {
		if mid := bridge.HeaderMessageID(raw); mid != "" {
			found := false
			var checkErr error
			for attempt := 0; attempt < 3 && !found; attempt++ {
				if attempt > 0 {
					time.Sleep(ss.srv.SentDedupeRetryDelay)
				}
				checkErr = ss.call(func(ctx context.Context, p bridge.Provider) error {
					metas, _, err := p.ListMessages(ctx, folder.ID, 0, 50)
					if err != nil {
						return err
					}
					for _, m := range metas {
						if m.MessageID == mid {
							found = true
							break
						}
					}
					return nil
				})
			}
			if checkErr != nil {
				// The check is best-effort: when the folder cannot be
				// listed, store the copy as before rather than fail.
				ss.srv.logger.Warn().Err(checkErr).Str("protocol", "imap").Str("op", "append").
					Str("account", ss.email).Str("folder", name).Msg("sent copy check failed, storing append normally")
			} else if found {
				ss.srv.logger.Info().Str("protocol", "imap").Str("op", "append").Str("account", ss.email).
					Str("folder", name).Str("folder_id", folder.ID).Msg("imap append deduped sent copy")
				_ = ss.tagged(tag, "OK", "APPEND completed (sent copy already stored)")
				if ss.sel != nil && ss.sel.folder.ID == folder.ID {
					_ = ss.refreshSelected()
				}
				return
			}
		}
	}
	var newID string
	err = ss.call(func(ctx context.Context, p bridge.Provider) error {
		id, err := p.AppendMessage(ctx, folder.ID, raw, flags)
		if err != nil {
			return err
		}
		newID = id
		return nil
	})
	if err != nil {
		_ = ss.no(tag, err)
		return
	}
	ss.srv.logger.Info().Str("protocol", "imap").Str("op", "append").Str("account", ss.email).
		Str("folder", name).Str("folder_id", folder.ID).Str("item", newID).Int("size", len(raw)).Msg("imap append")
	validity := ss.idmap.UIDValidity(folder.ID)
	uid, uerr := ss.idmap.UID(folder.ID, newID)
	if uerr == nil && newID != "" {
		_ = ss.printf("%s OK [APPENDUID %d %d] APPEND completed", tag, validity, uid)
	} else {
		_ = ss.tagged(tag, "OK", "APPEND completed")
	}
	if ss.sel != nil && ss.sel.folder.ID == folder.ID {
		_ = ss.refreshSelected()
	}
}

func (ss *session) doExpunge(silent bool) error {
	if ss.sel == nil {
		return fmt.Errorf("no mailbox selected")
	}
	if len(ss.deleted) == 0 {
		return nil
	}
	// collect indices with deleted uids
	var idxs []int
	var ids []string
	for i, e := range ss.sel.msgs {
		if ss.deleted[e.uid] {
			idxs = append(idxs, i)
			ids = append(ids, e.meta.ID)
		}
	}
	if len(ids) == 0 {
		ss.deleted = map[uint32]bool{}
		return nil
	}
	folderID := ss.sel.folder.ID
	err := ss.call(func(ctx context.Context, p bridge.Provider) error {
		return p.DeleteMessages(ctx, folderID, ids)
	})
	if err != nil {
		return err
	}
	removed := map[int]bool{}
	for _, i := range idxs {
		removed[i] = true
	}
	// emit descending so lower seqs stay valid
	if !silent {
		for k := len(idxs) - 1; k >= 0; k-- {
			_ = ss.printf("* %d EXPUNGE", idxs[k]+1)
		}
	}
	var kept []entry
	for i, e := range ss.sel.msgs {
		if !removed[i] {
			kept = append(kept, e)
		}
	}
	ss.sel.msgs = kept
	for _, i := range idxs {
		_ = i
	}
	ss.deleted = map[uint32]bool{}
	return nil
}

func (ss *session) cmdExpunge(tag string) {
	if ss.sel == nil {
		_ = ss.tagged(tag, "NO", "no mailbox selected")
		return
	}
	if ss.sel.readonly {
		_ = ss.tagged(tag, "NO", "mailbox is read-only")
		return
	}
	if err := ss.doExpunge(false); err != nil {
		_ = ss.no(tag, err)
		return
	}
	_ = ss.tagged(tag, "OK", "EXPUNGE completed")
}

func (ss *session) cmdClose(tag string) {
	if ss.sel == nil {
		_ = ss.tagged(tag, "NO", "no mailbox selected")
		return
	}
	if !ss.sel.readonly {
		if err := ss.doExpunge(true); err != nil {
			_ = ss.no(tag, err)
			return
		}
	}
	ss.sel = nil
	ss.deleted = map[uint32]bool{}
	_ = ss.tagged(tag, "OK", "CLOSE completed")
}

// pollChanges refreshes the selected folder and emits untagged diffs.
func (ss *session) pollChanges() error {
	if ss.sel == nil {
		return nil
	}
	old := append([]entry(nil), ss.sel.msgs...)
	if err := ss.refreshSelected(); err != nil {
		return err
	}
	cur := ss.sel.msgs
	oldByID := map[string]entry{}
	for _, e := range old {
		oldByID[e.meta.ID] = e
	}
	curIDs := map[string]bool{}
	for _, e := range cur {
		curIDs[e.meta.ID] = true
	}
	// expunges (descending old seq)
	for i := len(old) - 1; i >= 0; i-- {
		if !curIDs[old[i].meta.ID] {
			_ = ss.printf("* %d EXPUNGE", i+1)
		}
	}
	if len(cur) != len(old) {
		// EXISTS reflects new total (also after expunges)
		hasNew := false
		for _, e := range cur {
			if _, ok := oldByID[e.meta.ID]; !ok {
				hasNew = true
			}
		}
		if hasNew || len(cur) > len(old) {
			_ = ss.printf("* %d EXISTS", len(cur))
		}
	}
	// flag updates
	for i, e := range cur {
		if o, ok := oldByID[e.meta.ID]; ok {
			if formatFlags([]string(o.meta.Flags)) != formatFlags([]string(e.meta.Flags)) {
				_ = ss.printf("* %d FETCH (FLAGS %s)", i+1, formatFlags([]string(e.meta.Flags)))
			}
		}
	}
	return nil
}

func (ss *session) cmdIdle(tag string) bool {
	if ss.sel == nil {
		_ = ss.tagged(tag, "NO", "no mailbox selected")
		return true
	}
	if err := ss.writeLine("+ idling"); err != nil {
		return false
	}
	type lineRes struct {
		line string
		err  error
	}
	lineCh := make(chan lineRes, 1)
	go func() {
		line, err := ss.r.ReadString('\n')
		lineCh <- lineRes{line, err}
	}()
	ticker := time.NewTicker(ss.srv.pollInterval())
	defer ticker.Stop()
	for {
		select {
		case res := <-lineCh:
			if res.err != nil {
				return false
			}
			if strings.EqualFold(strings.TrimSpace(res.line), "DONE") {
				_ = ss.tagged(tag, "OK", "IDLE terminated")
				return true
			}
			_ = ss.tagged(tag, "BAD", "expected DONE")
			return true
		case <-ticker.C:
			if err := ss.pollChanges(); err != nil {
				ss.srv.logger.Debug().Err(err).Msg("idle poll")
			}
		}
	}
}
