package bridge

import (
	"bytes"
	"strings"
	"time"
)

// sentCopyWindow is how long a sent Message-ID stays dedupe-eligible.
// Clients append their Sent copy seconds after the SMTP accept, so the
// window only needs to cover slow clients, not history.
const sentCopyWindow = 10 * time.Minute

// HeaderMessageID returns the trimmed Message-ID header value of a raw
// MIME message, or "" when the header is absent. Only the header block
// is scanned (a "Message-ID:" string in a body never matches) and one
// folded continuation line is joined. It is a scanner, not a parser:
// callers use it for equality comparison only.
func HeaderMessageID(mime []byte) string {
	rest := mime
	found := false
	var value strings.Builder
	for len(rest) > 0 {
		var line []byte
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			line, rest = rest, nil
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			break // end of header block
		}
		if !found {
			if len(line) >= len("Message-ID:") && strings.EqualFold(string(line[:len("Message-ID:")]), "Message-ID:") {
				value.WriteString(strings.TrimSpace(string(line[len("Message-ID:"):])))
				found = true
			}
			continue
		}
		// Folded continuation (leading SP/HTAB) extends the value.
		if line[0] == ' ' || line[0] == '\t' {
			value.WriteString(strings.TrimSpace(string(line)))
			continue
		}
		break
	}
	if !found {
		return ""
	}
	return strings.TrimSpace(value.String())
}

// RecordSentCopy remembers that the bridge sent a message with this
// Message-ID for the account. Called after the upstream send succeeded
// (SendAndSaveCopy / sendMail already stored a copy in Sent Items), so
// the IMAP APPEND of the client's own Sent copy can be deduplicated.
func (r *Registry) RecordSentCopy(email, messageID string) {
	if messageID == "" {
		return
	}
	r.sentMu.Lock()
	defer r.sentMu.Unlock()
	r.pruneSentLocked()
	if r.sent == nil {
		r.sent = map[string]time.Time{}
	}
	r.sent[sentCopyKey(email, messageID)] = r.cfg.Now()
}

// SentCopyStored reports whether RecordSentCopy recorded this
// account/message-ID pair inside the dedupe window.
func (r *Registry) SentCopyStored(email, messageID string) bool {
	if messageID == "" {
		return false
	}
	r.sentMu.Lock()
	defer r.sentMu.Unlock()
	r.pruneSentLocked()
	_, ok := r.sent[sentCopyKey(email, messageID)]
	return ok
}

func sentCopyKey(email, messageID string) string {
	return NormalizeEmail(email) + "\x00" + strings.ToLower(messageID)
}

func (r *Registry) pruneSentLocked() {
	now := r.cfg.Now()
	for k, at := range r.sent {
		if now.Sub(at) > sentCopyWindow {
			delete(r.sent, k)
		}
	}
}
