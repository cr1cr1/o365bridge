package davserver

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// --- minimal iCalendar (RFC 5545 subset) codec ---
// Supports VEVENT and VTODO as mapped to bridge.Event / bridge.Task.
// VALARM and unknown components/properties are ignored on parse.
// TEXT escaping and 75-octet line folding are handled both ways.

type icalProp struct {
	Name   string
	Params map[string]string
	Value  string
}

func escapeText(s string) string {
	r := strings.NewReplacer("\\", "\\\\", ";", "\\;", ",", "\\,", "\r\n", "\\n", "\n", "\\n", "\r", "\\n")
	return r.Replace(s)
}

func unescapeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n', 'N':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// foldLine folds a logical line to <=75-octet physical lines (CRLF + space).
func foldLine(line string) string {
	if len(line) <= 75 {
		return line
	}
	var out strings.Builder
	limit := 75
	for len(line) > 0 {
		if len(line) <= limit {
			out.WriteString(line)
			break
		}
		// Back off to a rune boundary.
		n := limit
		for n > 0 && (line[n]&0xC0) == 0x80 {
			n--
		}
		if n == 0 {
			n = limit
		}
		out.WriteString(line[:n])
		out.WriteString("\r\n ")
		line = line[n:]
		limit = 74 // continuation line spends 1 octet on the space
	}
	return out.String()
}

func joinCRLF(lines []string) []byte {
	var folded []string
	for _, l := range lines {
		folded = append(folded, foldLine(l))
	}
	return []byte(strings.Join(folded, "\r\n") + "\r\n")
}

// unfold splits content into logical lines.
func unfold(data []byte) []string {
	raw := strings.ReplaceAll(string(data), "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(out) > 0 {
			out[len(out)-1] += line[1:]
			continue
		}
		out = append(out, line)
	}
	return out
}

func parseProp(line string) (icalProp, bool) {
	// Quoted parameters (e.g. Thunderbird's ALTREP data URL) may contain separators.
	var parts []string
	start, colon := 0, -1
	quoted := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			quoted = !quoted
		case ';', ':':
			if !quoted {
				parts = append(parts, line[start:i])
				start = i + 1
				if line[i] == ':' {
					colon = i
				}
			}
		}
		if colon >= 0 {
			break
		}
	}
	if colon < 0 {
		return icalProp{}, false
	}
	p := icalProp{Name: strings.ToUpper(parts[0]), Params: map[string]string{}, Value: line[colon+1:]}
	for _, kv := range parts[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			p.Params[strings.ToUpper(k)] = strings.Trim(v, `"`)
		}
	}
	return p, true
}

// parseICalendar parses a VCALENDAR into events and tasks.
func parseICalendar(data []byte) ([]bridge.Event, []bridge.Task, error) {
	var events []bridge.Event
	var tasks []bridge.Task
	var comp string // current component: VEVENT / VTODO / other
	var parents []string
	var ev *bridge.Event
	var tk *bridge.Task
	sawCal := false
	eventRuleSeen := false
	flush := func() {
		if ev != nil {
			events = append(events, *ev)
			ev = nil
		}
		if tk != nil {
			if tk.RecurAnchorOnly && tk.Start != nil {
				if tk.RecurStart == nil {
					tk.RecurStart = tk.Start
				}
				tk.Start = nil
				tk.StartDateOnly = false
			}
			tasks = append(tasks, *tk)
			tk = nil
		}
	}
	for _, line := range unfold(data) {
		if line == "" {
			continue
		}
		prop, ok := parseProp(line)
		if !ok {
			continue
		}
		switch prop.Name {
		case "BEGIN":
			parents = append(parents, comp)
			switch strings.ToUpper(prop.Value) {
			case "VCALENDAR":
				sawCal = true
				comp = "VCALENDAR"
			case "VEVENT":
				flush()
				// A CalDAV PUT is a full representation; omitted categories/repetition are cleared.
				ev = &bridge.Event{Categories: []string{}, RecurrenceSet: true}
				eventRuleSeen = false
				comp = "VEVENT"
			case "VTODO":
				flush()
				tk = &bridge.Task{FullUpdate: true, RecurSet: true, Categories: []string{}}
				comp = "VTODO"
			default: // VALARM etc: ignored
				comp = strings.ToUpper(prop.Value)
			}
			continue
		case "END":
			switch strings.ToUpper(prop.Value) {
			case "VEVENT", "VTODO", "VCALENDAR":
				flush()
			}
			comp = ""
			if len(parents) > 0 {
				comp = parents[len(parents)-1]
				parents = parents[:len(parents)-1]
			}
			continue
		}
		switch comp {
		case "VEVENT":
			if ev != nil {
				if prop.Name == "RDATE" || prop.Name == "EXDATE" || prop.Name == "RECURRENCE-ID" {
					return nil, nil, fmt.Errorf("davserver: event recurrence exceptions are not supported")
				}
				if prop.Name == "RRULE" {
					if eventRuleSeen || strings.TrimSpace(prop.Value) == "" {
						return nil, nil, fmt.Errorf("davserver: empty or duplicate event RRULE")
					}
					eventRuleSeen = true
				}
				if prop.Name == "DTSTART" || prop.Name == "DTEND" {
					if parsed, _ := parseDateTime(prop); parsed.IsZero() {
						return nil, nil, fmt.Errorf("davserver: invalid event date")
					}
					if zone := prop.Params["TZID"]; zone != "" {
						if _, err := time.LoadLocation(zone); err != nil {
							return nil, nil, fmt.Errorf("davserver: unsupported event timezone")
						}
					}
				}
				applyEventProp(ev, prop)
			}
		case "VTODO":
			if tk != nil {
				if prop.Name == "RDATE" || prop.Name == "EXDATE" || prop.Name == "RECURRENCE-ID" {
					return nil, nil, fmt.Errorf("davserver: task recurrence exceptions are not supported")
				}
				applyTaskProp(tk, prop)
			}
		}
	}
	flush()
	if !sawCal {
		return nil, nil, fmt.Errorf("davserver: not an iCalendar object")
	}
	for _, event := range events {
		if event.Recurrence != "" && event.Start.IsZero() {
			return nil, nil, fmt.Errorf("davserver: recurring event requires DTSTART")
		}
	}
	for _, task := range tasks {
		for _, value := range []*time.Time{task.Start, task.Due, task.Completed} {
			if value != nil && value.IsZero() {
				return nil, nil, fmt.Errorf("davserver: invalid task date")
			}
		}
	}
	return events, tasks, nil
}

func parseDateTime(prop icalProp) (t time.Time, allDay bool) {
	v := strings.TrimSpace(prop.Value)
	if strings.EqualFold(prop.Params["VALUE"], "DATE") || (len(v) == 8 && !strings.Contains(v, "T")) {
		if parsed, err := time.Parse("20060102", v); err == nil {
			return parsed, true
		}
		return time.Time{}, true
	}
	if tzid := prop.Params["TZID"]; tzid != "" {
		if loc, err := time.LoadLocation(tzid); err == nil {
			if parsed, err := time.ParseInLocation("20060102T150405", v, loc); err == nil {
				return parsed, false
			}
		}
	}
	if parsed, err := time.Parse("20060102T150405Z", v); err == nil {
		return parsed, false
	}
	if parsed, err := time.Parse("20060102T150405", v); err == nil {
		return parsed.UTC(), false // floating: treated as UTC
	}
	return time.Time{}, false
}

func partstatToBridge(s string) string {
	switch strings.ToUpper(s) {
	case "ACCEPTED":
		return "accepted"
	case "DECLINED":
		return "declined"
	case "TENTATIVE":
		return "tentative"
	case "NEEDS-ACTION":
		return "needs-action"
	case "IN-PROCESS":
		return "in-process"
	case "COMPLETED":
		return "completed"
	}
	return strings.ToLower(s)
}

func partstatFromBridge(s string) string {
	switch strings.ToLower(s) {
	case "accepted":
		return "ACCEPTED"
	case "declined":
		return "DECLINED"
	case "tentative":
		return "TENTATIVE"
	case "in-process":
		return "IN-PROCESS"
	case "completed":
		return "COMPLETED"
	default:
		return "NEEDS-ACTION"
	}
}

func stripMailto(s string) string {
	if len(s) >= 7 && strings.EqualFold(s[:7], "mailto:") {
		return s[7:]
	}
	return s
}

// parseCategories splits the TEXT list before unescaping literal commas.
func parseCategories(value string) []string {
	var categories []string
	if value == "" {
		return categories
	}
	start := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			if i+1 < len(value) {
				i++
			}
		case ',':
			categories = append(categories, unescapeText(value[start:i]))
			start = i + 1
		}
	}
	return append(categories, unescapeText(value[start:]))
}

func applyEventProp(ev *bridge.Event, p icalProp) {
	switch p.Name {
	case "UID":
		ev.UID = p.Value
	case "SUMMARY":
		ev.Summary = unescapeText(p.Value)
	case "DESCRIPTION":
		ev.Description = unescapeText(p.Value)
	case "LOCATION":
		ev.Location = unescapeText(p.Value)
	case "CATEGORIES":
		ev.Categories = append(ev.Categories, parseCategories(p.Value)...)
	case "DTSTART":
		ev.Start, ev.AllDay = parseDateTime(p)
		if tz := p.Params["TZID"]; tz != "" {
			ev.TimeZone = tz
		} else if !ev.AllDay && strings.HasSuffix(strings.ToUpper(p.Value), "Z") {
			ev.TimeZone = "UTC"
		}
	case "DTEND":
		ev.End, _ = parseDateTime(p)
	case "RRULE":
		ev.Recurrence = p.Value
	case "X-O365BRIDGE-RECUR-UNSUPPORTED":
		ev.RecurrenceUnsupported, _ = strconv.ParseBool(p.Value)
	case "ORGANIZER":
		ev.Organizer = stripMailto(unescapeText(p.Value))
	case "ATTENDEE":
		a := bridge.Attendee{Email: stripMailto(unescapeText(p.Value)), Name: p.Params["CN"], Status: partstatToBridge(p.Params["PARTSTAT"])}
		ev.Attendees = append(ev.Attendees, a)
	case "SEQUENCE":
		ev.Sequence, _ = strconv.Atoi(strings.TrimSpace(p.Value))
	}
}

func statusToBridge(s string) string {
	switch strings.ToUpper(s) {
	case "COMPLETED":
		return "completed"
	case "IN-PROCESS":
		return "in-process"
	default:
		return "needs-action"
	}
}
func statusFromBridge(s string) string {
	switch strings.ToLower(s) {
	case "completed":
		return "COMPLETED"
	case "in-process":
		return "IN-PROCESS"
	default:
		return "NEEDS-ACTION"
	}
}

func applyTaskProp(tk *bridge.Task, p icalProp) {
	switch p.Name {
	case "UID":
		tk.UID = p.Value
	case "SUMMARY":
		tk.Title = unescapeText(p.Value)
	case "DESCRIPTION":
		tk.Body = unescapeText(p.Value)
	case "DTSTART":
		t, dateOnly := parseDateTime(p)
		tk.Start, tk.StartDateOnly = &t, dateOnly
	case "DUE":
		t, dateOnly := parseDateTime(p)
		tk.Due, tk.DueDateOnly = &t, dateOnly
	case "CATEGORIES":
		tk.Categories = append(tk.Categories, parseCategories(p.Value)...)
	case "ATTACH":
		if tk.Attachments == nil {
			tk.Attachments = []bridge.TaskAttachment{}
		}
		tk.Attachments = append(tk.Attachments, bridge.TaskAttachment{URI: p.Value, ContentType: p.Params["FMTTYPE"]})
	case "X-O365BRIDGE-RECUR-START":
		t, _ := parseDateTime(icalProp{Value: p.Value, Params: map[string]string{"VALUE": "DATE"}})
		tk.RecurStart = &t
	case "X-O365BRIDGE-RECUR-TZ":
		tk.RecurTimeZone = unescapeText(p.Value)
	case "X-O365BRIDGE-RECUR-ANCHOR-ONLY":
		tk.RecurAnchorOnly = strings.EqualFold(p.Value, "TRUE")
	case "X-O365BRIDGE-RECUR-UNSUPPORTED":
		tk.RecurUnsupported = strings.EqualFold(p.Value, "TRUE")
	case "COMPLETED":
		t, _ := parseDateTime(p)
		tk.Completed = &t
		tk.Status = "completed"
	case "STATUS":
		tk.Status = statusToBridge(p.Value)
	case "PRIORITY":
		tk.Priority, _ = strconv.Atoi(strings.TrimSpace(p.Value))
	case "RRULE":
		tk.Recur = p.Value
		// PERCENT-COMPLETE has no bridge.Task field; only used via
		// STATUS/COMPLETED above (100 implies completed upstream).
	}
}

func fmtDateTime(t time.Time, allDay bool) string {
	if allDay {
		return t.Format("20060102")
	}
	return t.UTC().Format("20060102T150405Z")
}

func dtstamp(t time.Time) string {
	if t.IsZero() {
		t = time.Unix(0, 0).UTC()
	}
	return t.UTC().Format("20060102T150405Z")
}

func mailto(addr string) string {
	if addr == "" || strings.Contains(addr, ":") {
		return addr
	}
	return "mailto:" + addr
}

func generateEventICalendar(ev bridge.Event) []byte {
	lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//o365bridge//DAV//EN", "BEGIN:VEVENT"}
	lines = append(lines, "UID:"+ev.UID)
	// Deterministic DTSTAMP (derived from Start) keeps the generated
	// bytes — and therefore the ETag derived from them — stable.
	lines = append(lines, "DTSTAMP:"+dtstamp(ev.Start))
	zone := ""
	if ev.Recurrence != "" || ev.RecurrenceUnsupported {
		if loc, err := time.LoadLocation(bridge.FromWindowsZone(ev.TimeZone)); ev.TimeZone != "" && err == nil {
			ev.Start, ev.End = ev.Start.In(loc), ev.End.In(loc)
			if eventRepeatZone(ev) != "UTC" && !ev.AllDay {
				zone = loc.String()
			}
		}
	}
	if ev.AllDay {
		lines = append(lines, "DTSTART;VALUE=DATE:"+fmtDateTime(ev.Start, true))
		if !ev.End.IsZero() {
			lines = append(lines, "DTEND;VALUE=DATE:"+fmtDateTime(ev.End, true))
		}
	} else if zone != "" {
		// shortcut: IANA TZIDs rely on client tzdata; add VTIMEZONE for clients without it.
		lines = append(lines, "DTSTART;TZID="+zone+":"+ev.Start.Format("20060102T150405"))
		if !ev.End.IsZero() {
			lines = append(lines, "DTEND;TZID="+zone+":"+ev.End.Format("20060102T150405"))
		}
	} else {
		lines = append(lines, "DTSTART:"+fmtDateTime(ev.Start, false))
		if !ev.End.IsZero() {
			lines = append(lines, "DTEND:"+fmtDateTime(ev.End, false))
		}
	}
	if ev.Summary != "" {
		lines = append(lines, "SUMMARY:"+escapeText(ev.Summary))
	}
	if ev.Description != "" {
		lines = append(lines, "DESCRIPTION:"+escapeText(ev.Description))
	}
	if ev.Location != "" {
		lines = append(lines, "LOCATION:"+escapeText(ev.Location))
	}
	if len(ev.Categories) > 0 {
		categories := make([]string, len(ev.Categories))
		for i, category := range ev.Categories {
			categories[i] = escapeText(category)
		}
		lines = append(lines, "CATEGORIES:"+strings.Join(categories, ","))
	}
	if ev.Recurrence != "" {
		lines = append(lines, "RRULE:"+strings.TrimPrefix(ev.Recurrence, "RRULE:"))
	}
	if ev.RecurrenceUnsupported {
		lines = append(lines, "X-O365BRIDGE-RECUR-UNSUPPORTED:TRUE")
	}
	if ev.Organizer != "" {
		lines = append(lines, "ORGANIZER:"+mailto(ev.Organizer))
	}
	for _, a := range ev.Attendees {
		line := "ATTENDEE;PARTSTAT=" + partstatFromBridge(a.Status)
		if a.Name != "" {
			line += ";CN=" + a.Name
		}
		line += ":" + mailto(a.Email)
		lines = append(lines, line)
	}
	lines = append(lines, "SEQUENCE:"+strconv.Itoa(ev.Sequence))
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	return joinCRLF(lines)
}

func generateTaskICalendar(tk bridge.Task) []byte {
	lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//o365bridge//DAV//EN", "BEGIN:VTODO"}
	lines = append(lines, "UID:"+tk.UID)
	var stamp time.Time
	if tk.Start != nil {
		stamp = *tk.Start
	} else if tk.Due != nil {
		stamp = *tk.Due
	} else if tk.Completed != nil {
		stamp = *tk.Completed
	}
	lines = append(lines, "DTSTAMP:"+dtstamp(stamp))
	if tk.Title != "" {
		lines = append(lines, "SUMMARY:"+escapeText(tk.Title))
	}
	if tk.Body != "" {
		lines = append(lines, "DESCRIPTION:"+escapeText(tk.Body))
	}
	if tk.Start != nil {
		name := "DTSTART:"
		if tk.StartDateOnly {
			name = "DTSTART;VALUE=DATE:"
		}
		lines = append(lines, name+fmtDateTime(*tk.Start, tk.StartDateOnly))
	} else if tk.Recur != "" && tk.RecurStart != nil {
		dateOnly := tk.Due == nil || tk.DueDateOnly
		name := "DTSTART:"
		if dateOnly {
			name = "DTSTART;VALUE=DATE:"
		}
		lines = append(lines, name+fmtDateTime(*tk.RecurStart, dateOnly), "X-O365BRIDGE-RECUR-ANCHOR-ONLY:TRUE")
	}
	if tk.Due != nil {
		name := "DUE:"
		if tk.DueDateOnly {
			name = "DUE;VALUE=DATE:"
		}
		lines = append(lines, name+fmtDateTime(*tk.Due, tk.DueDateOnly))
	}
	if len(tk.Categories) > 0 {
		categories := make([]string, len(tk.Categories))
		for i, category := range tk.Categories {
			categories[i] = escapeText(category)
		}
		lines = append(lines, "CATEGORIES:"+strings.Join(categories, ","))
	}
	for _, attachment := range tk.Attachments {
		if attachment.URI != "" {
			name := "ATTACH"
			if attachment.ContentType != "" {
				name += ";FMTTYPE=" + attachment.ContentType
			}
			lines = append(lines, name+":"+attachment.URI)
		}
	}
	status := statusFromBridge(tk.Status)
	if tk.Completed != nil {
		lines = append(lines, "COMPLETED:"+fmtDateTime(*tk.Completed, false))
		status = "COMPLETED"
		lines = append(lines, "STATUS:"+status)
		lines = append(lines, "PERCENT-COMPLETE:100")
	} else {
		lines = append(lines, "STATUS:"+status)
	}
	if tk.Priority != 0 {
		lines = append(lines, "PRIORITY:"+strconv.Itoa(tk.Priority))
	}
	if tk.Recur != "" {
		lines = append(lines, "RRULE:"+taskCalendarRule(tk))
	}
	if tk.RecurStart != nil {
		lines = append(lines, "X-O365BRIDGE-RECUR-START:"+fmtDateTime(*tk.RecurStart, true))
	}
	if tk.RecurTimeZone != "" {
		lines = append(lines, "X-O365BRIDGE-RECUR-TZ:"+escapeText(tk.RecurTimeZone))
	}
	if tk.RecurUnsupported {
		lines = append(lines, "X-O365BRIDGE-RECUR-UNSUPPORTED:TRUE")
	}
	lines = append(lines, "END:VTODO", "END:VCALENDAR")
	return joinCRLF(lines)
}
