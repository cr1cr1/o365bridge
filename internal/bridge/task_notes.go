package bridge

import (
	"html"
	"strings"
)

// PlainTextBody preserves text notes and extracts visible text from native HTML.
func PlainTextBody(content, kind string) string {
	if !strings.EqualFold(kind, "html") {
		return content
	}
	var out strings.Builder
	newline := func() {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
	}
	lowerBytes := []byte(content)
	for i, value := range lowerBytes {
		if value >= 'A' && value <= 'Z' {
			lowerBytes[i] = value + ('a' - 'A')
		}
	}
	lower := string(lowerBytes)
	for i := 0; i < len(content); {
		if content[i] != '<' {
			end := strings.IndexByte(content[i:], '<')
			if end < 0 {
				end = len(content) - i
			}
			out.WriteString(content[i : i+end])
			i += end
			continue
		}
		if strings.HasPrefix(content[i:], "<!--") {
			end := strings.Index(content[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		if strings.HasPrefix(content[i:], "<!") {
			end := strings.IndexByte(content[i:], '>')
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		start := i + 1
		closing := false
		if start < len(content) && content[start] == '/' {
			closing = true
			start++
		}
		endName := start
		for endName < len(content) && ((content[endName] >= 'a' && content[endName] <= 'z') || (content[endName] >= 'A' && content[endName] <= 'Z') || (endName > start && content[endName] >= '0' && content[endName] <= '9')) {
			endName++
		}
		if endName == start {
			out.WriteByte('<')
			i++
			continue
		}
		name := strings.ToLower(content[start:endName])
		end := endName
		var quote byte
		for end < len(content) {
			ch := content[end]
			if quote != 0 {
				if ch == quote {
					quote = 0
				}
			} else if ch == '\'' || ch == '"' {
				quote = ch
			} else if ch == '>' {
				break
			}
			end++
		}
		if end == len(content) {
			out.WriteString(content[i:])
			break
		}
		i = end + 1
		if !closing && (name == "script" || name == "style" || name == "head") {
			search := i
			found := false
			for search < len(content) {
				endHidden := strings.Index(lower[search:], "</"+name)
				if endHidden < 0 {
					break
				}
				candidate := search + endHidden
				boundary := candidate + len(name) + 2
				if boundary < len(content) && strings.ContainsRune("> \t\r\n/", rune(content[boundary])) {
					i, found = candidate, true
					break
				}
				search = boundary
			}
			if !found {
				break
			}
			continue
		}
		switch name {
		case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "pre":
			newline()
		}
	}
	return strings.Trim(html.UnescapeString(out.String()), "\r\n")
}

// TaskNoteHTML uses the documented Graph HTML write form for literal plain notes.
func TaskNoteHTML(plain string) string {
	plain = strings.ReplaceAll(strings.ReplaceAll(plain, "\r\n", "\n"), "\r", "\n")
	return strings.ReplaceAll(html.EscapeString(plain), "\n", "<br>")
}
