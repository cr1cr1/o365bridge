package bridge

import (
	"bytes"
	"strings"
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
