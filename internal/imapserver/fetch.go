package imapserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// ---------- STORE / COPY / MOVE ----------

func (ss *session) cmdStore(tag, rest string, lits [][]byte, uidMode bool) {
	if ss.sel == nil {
		_ = ss.tagged(tag, "NO", "no mailbox selected")
		return
	}
	p := &parser{s: rest, lits: lits}
	setStr, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "STORE requires sequence set")
		return
	}
	item, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "STORE requires flag item")
		return
	}
	restFlags := p.rest()
	flagStrs := parseFlagList(restFlags)
	if len(flagStrs) == 0 && strings.TrimSpace(restFlags) != "" && !strings.HasPrefix(strings.TrimSpace(restFlags), "(") {
		flagStrs = strings.Fields(restFlags)
	}
	idxs, err := parseSeqSet(setStr, ss.sel.msgs, uidMode)
	if err != nil || len(idxs) == 0 {
		_ = ss.tagged(tag, "NO", "no matching messages")
		return
	}
	upper := strings.ToUpper(item)
	silent := strings.HasSuffix(upper, ".SILENT")
	mode := strings.TrimSuffix(upper, ".SILENT") // +FLAGS, -FLAGS, FLAGS

	var flags bridge.Flags
	for _, f := range flagStrs {
		flags = append(flags, toBridgeFlag(f))
	}
	var ids []string
	for _, i := range idxs {
		ids = append(ids, ss.sel.msgs[i].meta.ID)
	}
	var add, remove bridge.Flags
	switch mode {
	case "+FLAGS":
		add = flags
	case "-FLAGS":
		remove = flags
	case "FLAGS":
		add = flags
		// remove existing flags not in new set
		want := map[string]bool{}
		for _, f := range flags {
			want[strings.ToLower(f)] = true
		}
		rm := map[string]bool{}
		for _, i := range idxs {
			for _, f := range ss.sel.msgs[i].meta.Flags {
				lf := strings.ToLower(string(f))
				if !want[lf] && !rm[lf] {
					rm[lf] = true
					remove = append(remove, bridge.Flags{f}...)
				}
			}
		}
	default:
		_ = ss.tagged(tag, "BAD", "unsupported STORE item")
		return
	}
	if ss.sel.readonly {
		_ = ss.tagged(tag, "NO", "mailbox is read-only")
		return
	}
	folderID := ss.sel.folder.ID
	if len(add) > 0 || len(remove) > 0 || mode == "FLAGS" {
		err = ss.call(func(ctx context.Context, p bridge.Provider) error {
			return p.SetFlags(ctx, folderID, ids, add, remove)
		})
		if err != nil {
			_ = ss.no(tag, err)
			return
		}
	}
	// update local view + deleted set
	for _, i := range idxs {
		e := &ss.sel.msgs[i]
		switch mode {
		case "+FLAGS":
			for _, f := range add {
				if !hasBridgeFlag([]string(e.meta.Flags), f) {
					e.meta.Flags = append(e.meta.Flags, bridge.Flags{f}...)
				}
			}
		case "-FLAGS":
			var nf bridge.Flags
			for _, f := range e.meta.Flags {
				keep := true
				for _, r := range remove {
					if strings.EqualFold(string(f), r) {
						keep = false
					}
				}
				if keep {
					nf = append(nf, f)
				}
			}
			e.meta.Flags = nf
		case "FLAGS":
			e.meta.Flags = append(bridge.Flags(nil), add...)
		}
		if hasBridgeFlag([]string(e.meta.Flags), "\\deleted") {
			ss.deleted[e.uid] = true
		} else {
			delete(ss.deleted, e.uid)
		}
	}
	if !silent {
		for _, i := range idxs {
			e := ss.sel.msgs[i]
			if uidMode {
				_ = ss.printf("* %d FETCH (FLAGS %s UID %d)", i+1, formatFlags([]string(e.meta.Flags)), e.uid)
			} else {
				_ = ss.printf("* %d FETCH (FLAGS %s)", i+1, formatFlags([]string(e.meta.Flags)))
			}
		}
	}
	ss.deleteMarkedNow(idxs)
	_ = ss.tagged(tag, "OK", "STORE completed")
}

// deleteMarkedNow makes a \Deleted mark act at once, so clients that
// only mark — without a following EXPUNGE — still get a working
// delete. Outside the trash: the marked messages move to the account's
// trash folder upstream. Inside any folder marked as trash or drafts:
// the marked messages are removed permanently upstream (deleted mail
// would only move in a circle; a discarded draft is a working copy,
// and send flows rely on its removal). In both cases the messages then
// leave the selected view with untagged EXPUNGE responses (descending,
// so sequence numbers stay valid). If the upstream call fails, the
// mark stays session-local and the next EXPUNGE retries the removal.
func (ss *session) deleteMarkedNow(idxs []int) {
	if ss.sel == nil {
		return
	}
	var markIdxs []int
	var ids []string
	for _, i := range idxs {
		if i < len(ss.sel.msgs) && hasBridgeFlag([]string(ss.sel.msgs[i].meta.Flags), "\\deleted") {
			markIdxs = append(markIdxs, i)
			ids = append(ids, ss.sel.msgs[i].meta.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	src := ss.sel.folder.ID
	inTrash := ss.sel.folder.Special == "trash" || ss.sel.folder.Special == "drafts"
	var err error
	if inTrash {
		err = ss.call(func(ctx context.Context, p bridge.Provider) error {
			return p.DeleteMessages(ctx, src, ids)
		})
	} else {
		var folders []bridge.Folder
		folders, err = ss.listFolders()
		if err == nil {
			trashID := ""
			for _, f := range folders {
				if f.Special == "trash" {
					trashID = f.ID
					break
				}
			}
			if trashID == "" {
				return
			}
			err = ss.call(func(ctx context.Context, p bridge.Provider) error {
				return p.MoveMessages(ctx, src, ids, trashID)
			})
		}
	}
	if err != nil {
		ss.srv.logger.Warn().Err(err).Str("protocol", "imap").Str("op", "store").
			Str("account", ss.email).Msg("\\Deleted action failed, keeping mark session-local")
		return
	}
	for k := len(markIdxs) - 1; k >= 0; k-- {
		_ = ss.printf("* %d EXPUNGE", markIdxs[k]+1)
	}
	removed := map[int]bool{}
	for _, i := range markIdxs {
		removed[i] = true
		delete(ss.deleted, ss.sel.msgs[i].uid)
	}
	var kept []entry
	for i, e := range ss.sel.msgs {
		if !removed[i] {
			kept = append(kept, e)
		}
	}
	ss.sel.msgs = kept
}

func (ss *session) cmdCopyMove(tag, rest string, lits [][]byte, uidMode bool, move bool) {
	if ss.sel == nil {
		_ = ss.tagged(tag, "NO", "no mailbox selected")
		return
	}
	p := &parser{s: rest, lits: lits}
	setStr, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "requires sequence set")
		return
	}
	destName, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "requires destination mailbox")
		return
	}
	idxs, err := parseSeqSet(setStr, ss.sel.msgs, uidMode)
	if err != nil || len(idxs) == 0 {
		_ = ss.tagged(tag, "NO", "no matching messages")
		return
	}
	dest, _, err := ss.resolveFolder(destName)
	if err != nil {
		_ = ss.tagged(tag, "NO", sanitize(err.Error()))
		return
	}
	var ids []string
	var srcUIDs []string
	for _, i := range idxs {
		ids = append(ids, ss.sel.msgs[i].meta.ID)
		srcUIDs = append(srcUIDs, strconv.FormatUint(uint64(ss.sel.msgs[i].uid), 10))
	}
	srcFolder := ss.sel.folder.ID
	if move {
		err = ss.call(func(ctx context.Context, p bridge.Provider) error {
			return p.MoveMessages(ctx, srcFolder, ids, dest.ID)
		})
	} else {
		err = ss.call(func(ctx context.Context, p bridge.Provider) error {
			return p.CopyMessages(ctx, srcFolder, ids, dest.ID)
		})
	}
	if err != nil {
		_ = ss.no(tag, err)
		return
	}
	// Best-effort COPYUID: if upstream preserved IDs in dest, map them.
	copyUID := ""
	if destEntries, err := ss.listEntries(dest.ID); err == nil {
		idSet := map[string]bool{}
		for _, id := range ids {
			idSet[id] = true
		}
		byID := map[string]uint32{}
		for _, e := range destEntries {
			if idSet[e.meta.ID] {
				byID[e.meta.ID] = e.uid
			}
		}
		if len(byID) == len(ids) && len(ids) > 0 {
			var dstUIDs []string
			for _, id := range ids {
				dstUIDs = append(dstUIDs, strconv.FormatUint(uint64(byID[id]), 10))
			}
			copyUID = fmt.Sprintf("[COPYUID %d %s %s] ", ss.idmap.UIDValidity(dest.ID), strings.Join(srcUIDs, ","), strings.Join(dstUIDs, ","))
		}
	}
	verb := "COPY"
	if move {
		verb = "MOVE"
		// remove from local view, expunge descending
		removed := map[int]bool{}
		for _, i := range idxs {
			removed[i] = true
		}
		for k := len(idxs) - 1; k >= 0; k-- {
			_ = ss.printf("* %d EXPUNGE", idxs[k]+1)
		}
		var kept []entry
		for i, e := range ss.sel.msgs {
			if !removed[i] {
				kept = append(kept, e)
			}
		}
		ss.sel.msgs = kept
	}
	ss.srv.logger.Info().Str("protocol", "imap").Str("op", strings.ToLower(verb)).Str("account", ss.email).
		Str("folder", destName).Int("count", len(ids)).Msg("imap " + strings.ToLower(verb))
	_ = ss.printf("%s OK %s%s completed", tag, copyUID, verb)
}

// ---------- FETCH ----------

func needsMIME(item string) bool {
	u := strings.ToUpper(item)
	if strings.HasPrefix(u, "BODY") || strings.HasPrefix(u, "RFC822") || u == "ENVELOPE" || u == "BODYSTRUCTURE" {
		return true
	}
	return false
}

func (ss *session) fetchMIME(e entry) ([]byte, error) {
	var raw []byte
	err := ss.call(func(ctx context.Context, p bridge.Provider) error {
		b, err := p.GetMessageMIME(ctx, ss.sel.folder.ID, e.meta.ID)
		if err != nil {
			return err
		}
		raw = b
		return nil
	})
	return raw, err
}

func (ss *session) cmdFetch(tag, rest string, lits [][]byte, uidMode bool) {
	if ss.sel == nil {
		_ = ss.tagged(tag, "NO", "no mailbox selected")
		return
	}
	p := &parser{s: rest, lits: lits}
	setStr, err := p.astring()
	if err != nil {
		_ = ss.tagged(tag, "BAD", "FETCH requires sequence set")
		return
	}
	itemsStr := strings.TrimSpace(p.rest())
	var items []string
	if strings.HasPrefix(itemsStr, "(") {
		items = splitTopLevel(extractParen(itemsStr))
	} else if itemsStr != "" {
		items = []string{itemsStr}
	}
	if len(items) == 0 {
		_ = ss.tagged(tag, "BAD", "FETCH requires data items")
		return
	}
	// expand macros
	var expanded []string
	for _, it := range items {
		switch strings.ToUpper(it) {
		case "ALL":
			expanded = append(expanded, "FLAGS", "INTERNALDATE", "RFC822.SIZE", "ENVELOPE")
		case "FAST":
			expanded = append(expanded, "FLAGS", "INTERNALDATE", "RFC822.SIZE")
		case "FULL":
			expanded = append(expanded, "FLAGS", "INTERNALDATE", "RFC822.SIZE", "ENVELOPE", "BODYSTRUCTURE")
		default:
			expanded = append(expanded, it)
		}
	}
	items = expanded
	// UID is implicit in UID FETCH responses in many clients; RFC requires
	// it only when requested, but adding is harmless & expected by tests.
	hasUID := false
	for _, it := range items {
		if strings.EqualFold(it, "UID") {
			hasUID = true
		}
	}
	if uidMode && !hasUID {
		items = append([]string{"UID"}, items...)
	}
	idxs, err := parseSeqSet(setStr, ss.sel.msgs, uidMode)
	if err != nil {
		_ = ss.tagged(tag, "BAD", "bad sequence set")
		return
	}
	needAny := false
	for _, it := range items {
		if needsMIME(it) {
			needAny = true
		}
	}
	_ = needAny
	for _, idx := range idxs {
		e := ss.sel.msgs[idx]
		var raw []byte
		mimeNeeded := false
		for _, it := range items {
			if needsMIME(it) {
				mimeNeeded = true
			}
		}
		if mimeNeeded {
			b, err := ss.fetchMIME(e)
			if err != nil {
				_ = ss.no(tag, err)
				return
			}
			raw = b
		}
		// Non-PEEK BODY[]-class fetches set \Seen (RFC 3501 §6.4.5);
		// BODY.PEEK / ENVELOPE / BODYSTRUCTURE do not.
		markSeen := false
		for _, it := range items {
			u := strings.ToUpper(it)
			if strings.HasPrefix(u, "BODY[") || u == "RFC822" || strings.HasPrefix(u, "RFC822.") {
				markSeen = true
			}
		}
		if markSeen && !hasBridgeFlag([]string(e.meta.Flags), "\\seen") {
			folderID, msgID := ss.sel.folder.ID, e.meta.ID
			if err := ss.call(func(ctx context.Context, p bridge.Provider) error {
				return p.SetFlags(ctx, folderID, []string{msgID}, bridge.Flags{"\\seen"}, nil)
			}); err == nil {
				e.meta.Flags = append(e.meta.Flags, "\\seen")
				ss.sel.msgs[idx] = e
			}
		}
		if err := ss.writeFetch(idx+1, e, items, raw); err != nil {
			return
		}
	}
	_ = ss.tagged(tag, "OK", "FETCH completed")
}

// writeFetch writes one * n FETCH (...) response, handling literals.
func (ss *session) writeFetch(seq int, e entry, items []string, raw []byte) error {
	type part struct {
		text    string // "NAME value" for non-literal
		name    string
		literal []byte
		isLit   bool
	}
	var parts []part
	addText := func(s string) { parts = append(parts, part{text: s}) }
	addLit := func(name string, b []byte) {
		if b == nil {
			b = []byte{}
		}
		parts = append(parts, part{name: name, literal: b, isLit: true})
	}
	for _, item := range items {
		u := strings.ToUpper(item)
		switch {
		case u == "UID":
			addText(fmt.Sprintf("UID %d", e.uid))
		case u == "FLAGS":
			addText("FLAGS " + formatFlags([]string(e.meta.Flags)))
		case u == "RFC822.SIZE":
			sz := e.meta.Size
			if sz == 0 && raw != nil {
				sz = int64(len(raw))
			}
			addText(fmt.Sprintf("RFC822.SIZE %d", sz))
		case u == "INTERNALDATE":
			d := e.meta.Date
			if d.IsZero() {
				d = time.Now()
			}
			addText("INTERNALDATE " + quote(d.Format("02-Jan-2006 15:04:05 -0700")))
		case u == "ENVELOPE":
			addText("ENVELOPE " + buildEnvelope(e, raw))
		case u == "BODYSTRUCTURE" || u == "BODY":
			addText("BODYSTRUCTURE " + buildBodyStructure(raw, e))
		case u == "RFC822":
			addLit("RFC822", raw)
		case u == "RFC822.HEADER":
			h, _ := splitHeaderBody(raw)
			addLit("RFC822.HEADER", h)
		case u == "RFC822.TEXT":
			_, body := splitHeaderBody(raw)
			addLit("RFC822.TEXT", body)
		case strings.HasPrefix(u, "BODY.PEEK[") || strings.HasPrefix(u, "BODY["):
			section := item[strings.Index(item, "["):]
			// section includes [...] and possible <partial>
			respName, data := bodySection(item, raw)
			_ = section
			addLit(respName, data)
		default:
			// unknown item: ignore
		}
	}
	// emit
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("* %d FETCH (", seq))
	for i, pt := range parts {
		if i > 0 {
			buf.WriteByte(' ')
		}
		if pt.isLit {
			buf.WriteString(fmt.Sprintf("%s {%d}\r\n", pt.name, len(pt.literal)))
			buf.Write(pt.literal)
		} else {
			buf.WriteString(pt.text)
		}
	}
	buf.WriteString(")\r\n")
	if _, err := ss.w.Write(buf.Bytes()); err != nil {
		return err
	}
	return ss.w.Flush()
}

func splitHeaderBody(raw []byte) (header, body []byte) {
	if raw == nil {
		return []byte{}, []byte{}
	}
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i+4], raw[i+4:]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i+2], raw[i+2:]
	}
	return raw, []byte{}
}

// bodySection returns the response item name and bytes for BODY[...]/BODY.PEEK[...].
func bodySection(item string, raw []byte) (string, []byte) {
	_ = strings.HasPrefix(strings.ToUpper(item), "BODY.PEEK") // PEEK only changes response name handling below
	open := strings.Index(item, "[")
	closeIdx := strings.LastIndex(item, "]")
	if open < 0 || closeIdx < open {
		return "BODY[]", raw
	}
	section := item[open+1 : closeIdx]
	upper := strings.ToUpper(strings.TrimSpace(section))
	respName := "BODY[" + section + "]"
	switch {
	case upper == "":
		return "BODY[]", raw
	case upper == "HEADER":
		h, _ := splitHeaderBody(raw)
		return respName, h
	case upper == "TEXT":
		_, b := splitHeaderBody(raw)
		return respName, b
	case strings.HasPrefix(upper, "HEADER.FIELDS.NOT"):
		fields := parenFields(section)
		h, _ := splitHeaderBody(raw)
		return "BODY[HEADER.FIELDS.NOT (" + strings.Join(fields, " ") + ")]", selectHeaders(h, fields, false)
	case strings.HasPrefix(upper, "HEADER.FIELDS"):
		fields := parenFields(section)
		h, _ := splitHeaderBody(raw)
		return "BODY[HEADER.FIELDS (" + strings.Join(fields, " ") + ")]", selectHeaders(h, fields, true)
	default:
		// numeric / MIME sections unsupported: return full for []-like, else text
		if upper == "MIME" {
			h, _ := splitHeaderBody(raw)
			return respName, h
		}
		return respName, raw
	}
}

func parenFields(section string) []string {
	i := strings.Index(section, "(")
	j := strings.LastIndex(section, ")")
	if i < 0 || j < i {
		return nil
	}
	f := strings.Fields(section[i+1 : j])
	for k := range f {
		f[k] = strings.ToUpper(f[k])
	}
	return f
}

func selectHeaders(header []byte, fields []string, include bool) []byte {
	want := map[string]bool{}
	for _, f := range fields {
		want[strings.ToLower(f)] = true
	}
	var out []string
	// header block lines, handling folded lines minimally
	lines := strings.Split(strings.ReplaceAll(string(header), "\r\n", "\n"), "\n")
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		line := cur.String()
		cur.Reset()
		name := line
		if i := strings.Index(line, ":"); i >= 0 {
			name = line[:i]
		}
		is := want[strings.ToLower(strings.TrimSpace(name))]
		if is == include {
			out = append(out, line)
		}
	}
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		if strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t") {
			cur.WriteString("\r\n")
			cur.WriteString(ln)
			continue
		}
		flush()
		cur.WriteString(ln)
	}
	flush()
	if len(out) == 0 {
		return []byte{}
	}
	return []byte(strings.Join(out, "\r\n") + "\r\n\r\n")
}

// ---------- ENVELOPE / BODYSTRUCTURE ----------

func buildEnvelope(e entry, raw []byte) string {
	var hdr mail.Header
	if raw != nil {
		if msg, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
			hdr = msg.Header
		}
	}
	get := func(k string) string {
		if hdr != nil {
			return hdr.Get(k)
		}
		return ""
	}
	date := get("Date")
	if date == "" && !e.meta.Date.IsZero() {
		date = e.meta.Date.Format(time.RFC1123Z)
	}
	subject := get("Subject")
	if subject == "" {
		subject = e.meta.Subject
	}
	// decode encoded-word subject if possible
	if dec, err := new(mime.WordDecoder).DecodeHeader(subject); err == nil {
		subject = dec
	}
	addr := func(headerName string, fallback []string) string {
		if hdr != nil {
			if v := get(headerName); v != "" {
				if s := formatAddressList(v); s != "" {
					return s
				}
			}
			return "NIL"
		}
		if len(fallback) == 0 {
			return "NIL"
		}
		var parts []string
		for _, f := range fallback {
			if s := formatAddressList(f); s != "" && s != "NIL" {
				// formatAddressList returns parenthesised list already
				// strip outer parens for merging? Keep as single.
				parts = append(parts, strings.TrimSuffix(strings.TrimPrefix(s, "("), ")"))
			}
		}
		if len(parts) == 0 {
			return "NIL"
		}
		return "(" + strings.Join(parts, " ") + ")"
	}
	fromFallback := []string{}
	if e.meta.From != "" {
		fromFallback = []string{e.meta.From}
	}
	from := addr("From", fromFallback)
	sender := addr("Sender", nil)
	if sender == "NIL" {
		sender = from
	}
	replyTo := addr("Reply-To", nil)
	if replyTo == "NIL" {
		replyTo = from
	}
	to := addr("To", e.meta.To)
	cc := addr("Cc", nil)
	bcc := addr("Bcc", nil)
	inReplyTo := get("In-Reply-To")
	msgID := get("Message-ID")
	if msgID == "" {
		msgID = get("Message-Id")
	}
	return fmt.Sprintf("(%s %s %s %s %s %s %s %s %s %s)",
		quoteOrNIL(date), quoteOrNIL(subject), from, sender, replyTo, to, cc, bcc,
		quoteOrNIL(inReplyTo), quoteOrNIL(msgID))
}

func formatAddressList(v string) string {
	list, err := mail.ParseAddressList(v)
	if err != nil {
		// single bare address / name <addr>
		if a, err2 := mail.ParseAddress(v); err2 == nil {
			list = []*mail.Address{a}
		} else {
			v = strings.TrimSpace(v)
			if v == "" {
				return "NIL"
			}
			// bare email
			return "(" + formatOneAddress("", v) + ")"
		}
	}
	if len(list) == 0 {
		return "NIL"
	}
	var parts []string
	for _, a := range list {
		parts = append(parts, formatOneAddress(a.Name, a.Address))
	}
	return "(" + strings.Join(parts, " ") + ")"
}

func formatOneAddress(name, addr string) string {
	mailbox, host := addr, ""
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		mailbox, host = addr[:i], addr[i+1:]
	}
	n := "NIL"
	if name != "" {
		n = quote(name)
	}
	return fmt.Sprintf("(%s NIL %s %s)", n, quoteOrNIL(mailbox), quoteOrNIL(host))
}

func buildBodyStructure(raw []byte, e entry) string {
	if raw == nil || len(raw) == 0 {
		sz := e.meta.Size
		return fmt.Sprintf(`("text" "plain" ("charset" "utf-8") NIL NIL "7bit" %d 1 NIL NIL NIL)`, sz)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		sz := int64(len(raw))
		lines := int64(bytes.Count(raw, []byte("\n")))
		return fmt.Sprintf(`("text" "plain" ("charset" "utf-8") NIL NIL "7bit" %d %d NIL NIL NIL)`, sz, lines)
	}
	return structureFor(msg.Header, msg.Body)
}

func structureFor(hdr map[string][]string, body io.Reader) string {
	ct := ""
	if v, ok := hdr["Content-Type"]; ok && len(v) > 0 {
		ct = v[0]
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || mt == "" {
		mt = "text/plain"
	}
	data, _ := io.ReadAll(body)
	if strings.HasPrefix(mt, "multipart/") {
		boundary := params["boundary"]
		mr := multipart.NewReader(bytes.NewReader(data), boundary)
		var parts []string
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			parts = append(parts, structureFor(part.Header, part))
		}
		subtype := strings.TrimPrefix(mt, "multipart/")
		paramStr := "NIL"
		if boundary != "" {
			paramStr = fmt.Sprintf(`("boundary" %s)`, quote(boundary))
		}
		if len(parts) == 0 {
			parts = append(parts, `("text" "plain" NIL NIL NIL "7bit" 0 0 NIL NIL NIL)`)
		}
		return fmt.Sprintf("(%s %s %s NIL NIL NIL)", strings.Join(parts, " "), quote(subtype), paramStr)
	}
	typ, subtype := mt, ""
	if i := strings.Index(mt, "/"); i >= 0 {
		typ, subtype = mt[:i], mt[i+1:]
	}
	paramStr := "NIL"
	if len(params) > 0 {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var kv []string
		for _, k := range keys {
			kv = append(kv, quote(strings.ToLower(k)), quote(params[k]))
		}
		paramStr = "(" + strings.Join(kv, " ") + ")"
	} else if typ == "text" {
		paramStr = `("charset" "us-ascii")`
	}
	enc := "7bit"
	if v, ok := hdr["Content-Transfer-Encoding"]; ok && len(v) > 0 && v[0] != "" {
		enc = strings.ToLower(strings.TrimSpace(v[0]))
	}
	id := "NIL"
	if v, ok := hdr["Content-Id"]; ok && len(v) > 0 {
		id = quote(v[0])
	}
	if typ == "text" {
		lines := bytes.Count(data, []byte("\n"))
		if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
			lines++
		}
		return fmt.Sprintf("(%s %s %s %s NIL %s %d %d NIL NIL NIL)",
			quote(typ), quote(subtype), paramStr, id, quote(enc), len(data), lines)
	}
	// disposition
	disp := "NIL"
	if v, ok := hdr["Content-Disposition"]; ok && len(v) > 0 && v[0] != "" {
		dmt, dparams, err := mime.ParseMediaType(v[0])
		if err == nil {
			if len(dparams) > 0 {
				var kv []string
				keys := make([]string, 0, len(dparams))
				for k := range dparams {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					kv = append(kv, quote(strings.ToLower(k)), quote(dparams[k]))
				}
				disp = fmt.Sprintf("(%s (%s))", quote(dmt), strings.Join(kv, " "))
			} else {
				disp = fmt.Sprintf("(%s NIL)", quote(dmt))
			}
		}
	}
	return fmt.Sprintf("(%s %s %s %s NIL %s %d NIL %s NIL)",
		quote(typ), quote(subtype), paramStr, id, quote(enc), len(data), disp)
}

// ---------- SEARCH ----------

type searchPred func(entry) bool

type searchParser struct {
	toks []string
	pos  int
	// collected pushdown criteria
	crit       bridge.SearchCriteria
	pushable   bool
	hasPush    bool
	sawComplex bool
}

func tokenizeSearch(s string) []string {
	var toks []string
	i := 0
	for i < len(s) {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if c == '(' || c == ')' {
			toks = append(toks, string(c))
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var b strings.Builder
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					j++
					b.WriteByte(s[j])
					j++
					continue
				}
				if s[j] == '"' {
					j++
					break
				}
				b.WriteByte(s[j])
				j++
			}
			toks = append(toks, b.String())
			i = j
			continue
		}
		j := i
		for j < len(s) && !strings.ContainsRune(" \t\r\n()", rune(s[j])) {
			j++
		}
		toks = append(toks, s[i:j])
		i = j
	}
	return toks
}

func (sp *searchParser) next() (string, bool) {
	if sp.pos >= len(sp.toks) {
		return "", false
	}
	t := sp.toks[sp.pos]
	sp.pos++
	return t, true
}

func (sp *searchParser) peekTok() (string, bool) {
	if sp.pos >= len(sp.toks) {
		return "", false
	}
	return sp.toks[sp.pos], true
}

func andPred(a, b searchPred) searchPred {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return func(e entry) bool { return a(e) && b(e) }
}

func (ss *session) cmdSearch(tag, rest string, uidMode bool) {
	if ss.sel == nil {
		_ = ss.tagged(tag, "NO", "no mailbox selected")
		return
	}
	args := strings.TrimSpace(rest)
	// optional CHARSET
	if strings.HasPrefix(strings.ToUpper(args), "CHARSET") {
		parts := strings.SplitN(args, " ", 3)
		if len(parts) >= 2 {
			if len(parts) == 3 {
				args = parts[2]
			} else {
				args = ""
			}
		}
	}
	sp := &searchParser{toks: tokenizeSearch(args), pushable: true}
	pred := sp.parseAnd()
	if pred == nil {
		pred = func(entry) bool { return true }
	}
	entries := ss.sel.msgs
	candidates := entries
	// pushdown where possible
	if sp.pushable && sp.hasPush && !sp.sawComplex {
		var ids []string
		crit := sp.crit
		err := ss.call(func(ctx context.Context, p bridge.Provider) error {
			got, err := p.SearchMessages(ctx, ss.sel.folder.ID, crit)
			if err != nil {
				return err
			}
			ids = got
			return nil
		})
		if err != nil {
			_ = ss.no(tag, err)
			return
		}
		idSet := map[string]bool{}
		for _, id := range ids {
			idSet[id] = true
		}
		var filtered []entry
		for _, e := range entries {
			if idSet[e.meta.ID] {
				filtered = append(filtered, e)
			}
		}
		candidates = filtered
	}
	var nums []string
	// need seq numbers relative to full selected list
	seqByUID := map[uint32]int{}
	for i, e := range entries {
		seqByUID[e.uid] = i + 1
	}
	for _, e := range candidates {
		if pred(e) {
			if uidMode {
				nums = append(nums, strconv.FormatUint(uint64(e.uid), 10))
			} else {
				nums = append(nums, strconv.Itoa(seqByUID[e.uid]))
			}
		}
	}
	if len(nums) > 0 {
		_ = ss.printf("* SEARCH %s", strings.Join(nums, " "))
	} else {
		_ = ss.printf("* SEARCH")
	}
	_ = ss.tagged(tag, "OK", "SEARCH completed")
}

func (sp *searchParser) parseAnd() searchPred {
	var pred searchPred
	for {
		tok, ok := sp.peekTok()
		if !ok || tok == ")" {
			break
		}
		p := sp.parseUnary()
		if p != nil {
			pred = andPred(pred, p)
		} else {
			// parseUnary consumed something unrecognised; avoid infinite loop
			if _, ok2 := sp.peekTok(); !ok2 {
				break
			}
		}
	}
	return pred
}

func parseIMAPDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2-Jan-2006", "02-Jan-2006", "2-Jan-06"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad date %q", s)
}

func dateOnly(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func (sp *searchParser) parseUnary() searchPred {
	tok, ok := sp.next()
	if !ok {
		return nil
	}
	switch tok {
	case "(":
		p := sp.parseAnd()
		sp.next() // consume ')'
		return p
	case ")":
		return nil
	}
	up := strings.ToUpper(tok)
	switch up {
	case "ALL":
		return func(entry) bool { return true }
	case "UNSEEN":
		sp.crit.Unseen = true
		sp.hasPush = true
		return func(e entry) bool { return !hasBridgeFlag([]string(e.meta.Flags), "\\seen") }
	case "SEEN":
		sp.pushable = false
		return func(e entry) bool { return hasBridgeFlag([]string(e.meta.Flags), "\\seen") }
	case "FLAGGED":
		sp.crit.Flagged = true
		sp.hasPush = true
		return func(e entry) bool { return hasBridgeFlag([]string(e.meta.Flags), "\\flagged") }
	case "UNFLAGGED":
		sp.pushable = false
		return func(e entry) bool { return !hasBridgeFlag([]string(e.meta.Flags), "\\flagged") }
	case "ANSWERED":
		sp.pushable = false
		return func(e entry) bool { return hasBridgeFlag([]string(e.meta.Flags), "\\answered") }
	case "UNANSWERED":
		sp.pushable = false
		return func(e entry) bool { return !hasBridgeFlag([]string(e.meta.Flags), "\\answered") }
	case "DELETED":
		sp.pushable = false
		return func(e entry) bool { return hasBridgeFlag([]string(e.meta.Flags), "\\deleted") }
	case "UNDELETED":
		sp.pushable = false
		return func(e entry) bool { return !hasBridgeFlag([]string(e.meta.Flags), "\\deleted") }
	case "DRAFT":
		sp.pushable = false
		return func(e entry) bool { return hasBridgeFlag([]string(e.meta.Flags), "\\draft") }
	case "UNDRAFT":
		sp.pushable = false
		return func(e entry) bool { return !hasBridgeFlag([]string(e.meta.Flags), "\\draft") }
	case "FROM", "SUBJECT", "TEXT", "TO", "CC", "BCC", "BODY":
		val, _ := sp.next()
		switch up {
		case "FROM":
			if sp.crit.From != "" {
				sp.pushable = false
			}
			sp.crit.From = val
			sp.hasPush = true
			return func(e entry) bool { return containsFold(e.meta.From, val) }
		case "SUBJECT":
			if sp.crit.Subject != "" {
				sp.pushable = false
			}
			sp.crit.Subject = val
			sp.hasPush = true
			return func(e entry) bool { return containsFold(e.meta.Subject, val) }
		case "TEXT":
			if sp.crit.Text != "" {
				sp.pushable = false
			}
			sp.crit.Text = val
			sp.hasPush = true
			return func(e entry) bool {
				return containsFold(e.meta.Subject+" "+e.meta.From+" "+strings.Join(e.meta.To, " "), val)
			}
		case "TO":
			sp.pushable = false
			return func(e entry) bool { return containsFold(strings.Join(e.meta.To, " "), val) }
		default:
			sp.pushable = false
			return func(e entry) bool {
				return containsFold(e.meta.Subject+" "+e.meta.From+" "+strings.Join(e.meta.To, " "), val)
			}
		}
	case "SINCE", "BEFORE", "ON", "SENT SINCE":
		val, _ := sp.next()
		d, err := parseIMAPDate(val)
		if err != nil {
			sp.pushable = false
			return func(entry) bool { return true }
		}
		if up == "SINCE" {
			sp.crit.Since = d
			sp.hasPush = true
			return func(e entry) bool { return !dateOnly(e.meta.Date).Before(dateOnly(d)) }
		}
		sp.crit.Before = d
		sp.hasPush = true
		return func(e entry) bool { return dateOnly(e.meta.Date).Before(dateOnly(d)) }
	case "NOT":
		sp.pushable = false
		sp.sawComplex = true
		inner := sp.parseUnary()
		if inner == nil {
			return func(entry) bool { return true }
		}
		return func(e entry) bool { return !inner(e) }
	case "OR":
		sp.pushable = false
		sp.sawComplex = true
		a := sp.parseUnary()
		b := sp.parseUnary()
		return func(e entry) bool {
			av, bv := true, true
			if a != nil {
				av = a(e)
			}
			if b != nil {
				bv = b(e)
			}
			return av || bv
		}
	case "UID":
		sp.pushable = false
		setStr, _ := sp.next()
		// UID set predicate needs entries context: approximate by storing
		// We evaluate against a synthetic single-entry list later - instead
		// capture set and check uid membership via parse on the fly using
		// a one-element entry slice in parseSeqSet helper.
		return func(e entry) bool {
			idxs, err := parseSeqSet(setStr, []entry{e}, true)
			// parseSeqSet with single entry won't match arbitrary uid range;
			// do direct range check:
			_ = idxs
			if err != nil {
				return false
			}
			return uidInSet(setStr, e.uid)
		}
	case "SMALLER", "LARGER":
		sp.pushable = false
		val, _ := sp.next()
		n, _ := strconv.ParseInt(val, 10, 64)
		if up == "SMALLER" {
			return func(e entry) bool { return e.meta.Size < n }
		}
		return func(e entry) bool { return e.meta.Size > n }
	case "HEADER":
		sp.pushable = false
		sp.next() // field
		val, _ := sp.next()
		return func(e entry) bool {
			return containsFold(e.meta.Subject+" "+e.meta.From, val)
		}
	case "RECENT", "OLD", "NEW":
		return func(entry) bool { return true }
	default:
		// Unknown atom: could be a seq set in SEARCH (e.g. "1:2") - treat
		// as seq predicate approximated as true; consume nothing else.
		if strings.Contains(tok, ":") || isNumericSet(tok) {
			sp.pushable = false
			return func(e entry) bool { return true }
		}
		sp.pushable = false
		return func(entry) bool { return true }
	}
}

func isNumericSet(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c == ',' || c == '*' || c == ':') {
			return false
		}
	}
	return len(s) > 0
}

func uidInSet(set string, uid uint32) bool {
	for _, part := range strings.Split(set, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			return true
		}
		if strings.Contains(part, ":") {
			ab := strings.SplitN(part, ":", 2)
			var a, b uint64
			if ab[0] == "*" || ab[1] == "*" {
				return true
			}
			a, _ = strconv.ParseUint(ab[0], 10, 32)
			b, _ = strconv.ParseUint(ab[1], 10, 32)
			if a > b {
				a, b = b, a
			}
			if uint64(uid) >= a && uint64(uid) <= b {
				return true
			}
		} else {
			n, err := strconv.ParseUint(part, 10, 32)
			if err == nil && uint32(n) == uid {
				return true
			}
		}
	}
	return false
}
