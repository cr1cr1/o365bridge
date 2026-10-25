package bridge

import (
	"bytes"
	"context"
	"strings"
	"time"
)

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

// FolderHasMessageID reports whether the folder already holds a
// message with this Message-ID. The send path stores its copy at the
// same moment a client "places" the mail, so the listing is retried
// briefly: listings lag. The returned error is the last listing
// error; false with nil error means the folder surely lacks it.
func FolderHasMessageID(ctx context.Context, p Provider, folderID, messageID string, retryDelay time.Duration) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}
		metas, _, err := p.ListMessages(ctx, folderID, 0, 50)
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		for _, m := range metas {
			if m.MessageID == messageID {
				return true, nil
			}
		}
	}
	return false, lastErr
}
