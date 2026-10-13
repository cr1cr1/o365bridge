package imapserver

import (
	"fmt"
	"strconv"
	"strings"
)

// --- quoting / canonicalisation ---

func quote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}

func quoteOrNIL(s string) string {
	if s == "" {
		return "NIL"
	}
	return quote(s)
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

var sysFlags = map[string]string{
	"\\seen":     "\\Seen",
	"\\answered": "\\Answered",
	"\\flagged":  "\\Flagged",
	"\\deleted":  "\\Deleted",
	"\\draft":    "\\Draft",
	"\\recent":   "\\Recent",
}

func canonFlag(f string) string {
	f = strings.TrimSpace(f)
	if c, ok := sysFlags[strings.ToLower(f)]; ok {
		return c
	}
	if !strings.HasPrefix(f, "\\") && len(f) > 0 {
		// keyword: keep first char case as-is; normalise rest lower for $-flags
		return f
	}
	return f
}

func toBridgeFlag(f string) string {
	c := canonFlag(f)
	if _, ok := sysFlags[strings.ToLower(c)]; ok {
		return strings.ToLower(c)
	}
	return strings.ToLower(c)
}

func formatFlags(flags []string) string {
	if len(flags) == 0 {
		return "()"
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range flags {
		c := canonFlag(f)
		k := strings.ToLower(c)
		if !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	return "(" + strings.Join(out, " ") + ")"
}

func hasBridgeFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

// --- argument parser over a command remainder ---

type parser struct {
	s    string
	pos  int
	lits [][]byte
}

func (p *parser) skip() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func (p *parser) eof() bool { p.skip(); return p.pos >= len(p.s) }

func (p *parser) peek() byte {
	p.skip()
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *parser) rest() string {
	p.skip()
	r := strings.TrimSpace(p.s[p.pos:])
	p.pos = len(p.s)
	return r
}

// astring: quoted string, literal placeholder, or atom.
func (p *parser) astring() (string, error) {
	p.skip()
	if p.pos >= len(p.s) {
		return "", fmt.Errorf("missing argument")
	}
	// literal placeholder \x00N\x00
	if p.s[p.pos] == 0 {
		end := strings.IndexByte(p.s[p.pos+1:], 0)
		if end < 0 {
			return "", fmt.Errorf("bad literal placeholder")
		}
		idx, err := strconv.Atoi(p.s[p.pos+1 : p.pos+1+end])
		if err != nil || idx < 0 || idx >= len(p.lits) {
			return "", fmt.Errorf("bad literal")
		}
		val := string(p.lits[idx])
		p.pos += end + 2
		return val, nil
	}
	if p.s[p.pos] == '"' {
		p.pos++
		var b strings.Builder
		for p.pos < len(p.s) {
			c := p.s[p.pos]
			if c == '\\' && p.pos+1 < len(p.s) {
				p.pos++
				b.WriteByte(p.s[p.pos])
				p.pos++
				continue
			}
			if c == '"' {
				p.pos++
				return b.String(), nil
			}
			b.WriteByte(c)
			p.pos++
		}
		return "", fmt.Errorf("unterminated quoted string")
	}
	// atom: up to space, '(' or ')'
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '(' || c == ')' {
			break
		}
		// literal placeholder inside atom shouldn't happen
		if c == 0 {
			break
		}
		p.pos++
	}
	if start == p.pos {
		return "", fmt.Errorf("missing argument")
	}
	return p.s[start:p.pos], nil
}

// literalBytes returns raw bytes if next argument is a literal placeholder.
func (p *parser) literalBytes() ([]byte, bool) {
	p.skip()
	if p.pos >= len(p.s) || p.s[p.pos] != 0 {
		return nil, false
	}
	end := strings.IndexByte(p.s[p.pos+1:], 0)
	if end < 0 {
		return nil, false
	}
	idx, err := strconv.Atoi(p.s[p.pos+1 : p.pos+1+end])
	if err != nil || idx < 0 || idx >= len(p.lits) {
		return nil, false
	}
	b := p.lits[idx]
	p.pos += end + 2
	return b, true
}

// paren returns the inner text of a parenthesised group (balanced).
func (p *parser) paren() (string, error) {
	p.skip()
	if p.pos >= len(p.s) || p.s[p.pos] != '(' {
		return "", fmt.Errorf("expected (")
	}
	start := p.pos + 1
	depth := 0
	inQ := false
	for i := p.pos; i < len(p.s); i++ {
		c := p.s[i]
		if inQ {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inQ = false
			}
			continue
		}
		switch c {
		case '"':
			inQ = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				inner := p.s[start:i]
				p.pos = i + 1
				return inner, nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced parens")
}

// parseParenOrRest extracts (...) if present else the whole remainder.
func extractParen(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "(") {
		p := &parser{s: s}
		if inner, err := p.paren(); err == nil {
			return inner
		}
	}
	return s
}

func parseFlagList(s string) []string {
	s = strings.TrimSpace(s)
	s = extractParen(s)
	if s == "" {
		return nil
	}
	fields := strings.Fields(s)
	var out []string
	for _, f := range fields {
		out = append(out, f)
	}
	return out
}

// splitTopLevel splits fetch/search item lists at top-level spaces,
// keeping BODY[...] (with nested parens) and (...) groups intact.
func splitTopLevel(s string) []string {
	var out []string
	depthP, depthB := 0, 0
	inQ := false
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQ {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inQ = false
			}
			continue
		}
		switch c {
		case '"':
			inQ = true
			if start < 0 {
				start = i
			}
		case '(':
			depthP++
			if start < 0 {
				start = i
			}
		case ')':
			depthP--
		case '[':
			depthB++
			if start < 0 {
				start = i
			}
		case ']':
			depthB--
		case ' ', '\t':
			if depthP == 0 && depthB == 0 && start >= 0 {
				out = append(out, s[start:i])
				start = -1
			} else if start < 0 {
				// skip
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// parseSeqSet resolves a sequence set against entries; uidMode selects
// UID space. Returns 0-based indices in ascending order.
func parseSeqSet(set string, entries []entry, uidMode bool) ([]int, error) {
	set = strings.TrimSpace(set)
	if set == "" {
		return nil, fmt.Errorf("empty sequence set")
	}
	maxVal := uint32(len(entries))
	if uidMode && len(entries) > 0 {
		maxVal = entries[len(entries)-1].uid
	}
	resolve := func(tok string) (uint32, error) {
		tok = strings.TrimSpace(tok)
		if tok == "*" {
			return maxVal, nil
		}
		n, err := strconv.ParseUint(tok, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("bad sequence %q", tok)
		}
		return uint32(n), nil
	}
	wanted := map[uint32]bool{} // seq or uid values
	for _, part := range strings.Split(set, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, ":") {
			ab := strings.SplitN(part, ":", 2)
			a, err := resolve(ab[0])
			if err != nil {
				return nil, err
			}
			b, err := resolve(ab[1])
			if err != nil {
				return nil, err
			}
			if a > b {
				a, b = b, a
			}
			for v := a; v <= b; v++ {
				wanted[v] = true
				if v == ^uint32(0) {
					break
				}
			}
		} else {
			v, err := resolve(part)
			if err != nil {
				return nil, err
			}
			wanted[v] = true
		}
	}
	var out []int
	for i, e := range entries {
		var v uint32
		if uidMode {
			v = e.uid
		} else {
			v = uint32(i + 1)
		}
		if wanted[v] {
			out = append(out, i)
		}
	}
	return out, nil
}

func imapMatch(pattern, name string) bool {
	return globIMAP(pattern, name)
}

func globIMAP(pattern, name string) bool {
	// iterative glob with backtracking for * and %
	p, n := 0, 0
	starP, starN := -1, -1
	for n < len(name) {
		if p < len(pattern) {
			switch pattern[p] {
			case '*':
				starP, starN = p, n
				p++
				continue
			case '%':
				if name[n] != '/' {
					p++
					n++
					continue
				}
			default:
				// case-insensitive compare for mailbox names
				if lowerByte(pattern[p]) == lowerByte(name[n]) {
					p++
					n++
					continue
				}
			}
		}
		if starP >= 0 {
			// let '*' consume one more char
			p = starP + 1
			starN++
			n = starN
			continue
		}
		return false
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	// trailing % cannot match remaining (already consumed) - allow % at end if no chars left
	for p < len(pattern) && pattern[p] == '%' {
		p++
	}
	return p == len(pattern)
}

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}
