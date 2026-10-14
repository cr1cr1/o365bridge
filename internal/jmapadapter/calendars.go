package jmapadapter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/calendars"
	"github.com/cr1cr1/o365bridge/jmap/datatype"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// shortcut: calendar/contact changes use live-hash state +
// ErrCannotCalculate fallback (same pattern as mailboxes). The journal
// stays email-only in this phase; generalising it is deferred.

func hashStrings(parts []string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("%x", h)
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- Backend contracts ---

func (b *Backend) Calendars() datatype.Store[calendars.Calendar]   { return &calendarStore{b} }
func (b *Backend) Events() datatype.Store[calendars.CalendarEvent] { return &eventStore{b} }
func (b *Backend) Availability(ctx context.Context, account jmap.ID, attendees []string, start, end string) (map[string][]calendars.BusyPeriod, error) {
	from, err := parseTimeArg(start)
	if err != nil {
		return nil, jmap.NewInvalidArguments("invalid start: " + err.Error())
	}
	to, err := parseTimeArg(end)
	if err != nil {
		return nil, jmap.NewInvalidArguments("invalid end: " + err.Error())
	}
	var res map[string][][2]time.Time
	if err := b.call(ctx, string(account), "FreeBusy", func(ctx context.Context, p bridge.Provider) error {
		var err error
		res, err = p.FreeBusy(ctx, attendees, from, to)
		return err
	}); err != nil {
		return nil, err
	}
	out := make(map[string][]calendars.BusyPeriod, len(res))
	for email, periods := range res {
		list := make([]calendars.BusyPeriod, 0, len(periods))
		for _, per := range periods {
			list = append(list, calendars.BusyPeriod{
				BusyType: "busy",
				Start:    per[0].UTC().Format(time.RFC3339),
				End:      per[1].UTC().Format(time.RFC3339),
			})
		}
		out[email] = list
	}
	return out, nil
}

func parseTimeArg(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02T15:04:05", s, time.UTC)
}

// --- Calendar store ---

type calendarStore struct{ b *Backend }

func calendarRights() *calendars.CalendarRights {
	return &calendars.CalendarRights{
		MayReadFreeBusy: true, MayReadItems: true, MayAddItems: true,
		MayUpdatePrivate: true, MayUpdateAll: true, MayRemoveItems: true,
		MayDelete: true, MayRename: true, MayShare: true,
	}
}

func (s *calendarStore) list(ctx context.Context, account jmap.ID) ([]bridge.Calendar, error) {
	var cals []bridge.Calendar
	err := s.b.call(ctx, string(account), "ListCalendars", func(ctx context.Context, p bridge.Provider) error {
		var err error
		cals, err = p.ListCalendars(ctx)
		return err
	})
	return cals, err
}

func calendarToJMAP(c bridge.Calendar, isDefault bool) calendars.Calendar {
	return calendars.Calendar{
		ID: jmap.ID(c.ID), Name: c.Name, IsDefault: isDefault, IsSubscribed: true,
		MyRights: calendarRights(),
	}
}

func (s *calendarStore) State(ctx context.Context, account jmap.ID) (string, error) {
	cals, err := s.list(ctx, account)
	if err != nil {
		return "", err
	}
	sorted := append([]bridge.Calendar(nil), cals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	parts := make([]string, 0, len(sorted))
	for _, c := range sorted {
		parts = append(parts, c.ID+"|"+c.Name)
	}
	return hashStrings(parts), nil
}

func (s *calendarStore) Get(ctx context.Context, account jmap.ID, ids []jmap.ID) ([]calendars.Calendar, []jmap.ID, error) {
	cals, err := s.list(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	// shortcut: first upstream calendar is the default; providers do
	// not expose a default flag.
	convert := func() []calendars.Calendar {
		out := make([]calendars.Calendar, 0, len(cals))
		for i, c := range cals {
			out = append(out, calendarToJMAP(c, i == 0))
		}
		return out
	}
	if ids == nil {
		return convert(), nil, nil
	}
	byID := map[jmap.ID]calendars.Calendar{}
	for _, c := range convert() {
		byID[c.ID] = c
	}
	var found []calendars.Calendar
	var notFound []jmap.ID
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			found = append(found, c)
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

// Upstream calendars are fixed collections: no provider calendar-
// collection CRUD exists, so create/update/destroy are forbidden.
func forbiddenCalendars() *jmap.SetError {
	return &jmap.SetError{Type: "forbidden", Description: "upstream calendars are fixed collections"}
}

func (s *calendarStore) Create(context.Context, jmap.ID, calendars.Calendar) (calendars.Calendar, *jmap.SetError) {
	return calendars.Calendar{}, forbiddenCalendars()
}
func (s *calendarStore) Update(context.Context, jmap.ID, calendars.Calendar) (*calendars.Calendar, *jmap.SetError) {
	return nil, forbiddenCalendars()
}
func (s *calendarStore) Destroy(context.Context, jmap.ID, jmap.ID) *jmap.SetError {
	return forbiddenCalendars()
}

func (s *calendarStore) Changes(ctx context.Context, account jmap.ID, sinceState string, _ int) ([]jmap.ID, []jmap.ID, []jmap.ID, string, bool, error) {
	cur, err := s.State(ctx, account)
	if err != nil {
		return nil, nil, nil, "", false, err
	}
	if sinceState == cur {
		return []jmap.ID{}, []jmap.ID{}, []jmap.ID{}, cur, false, nil
	}
	return nil, nil, nil, "", false, datatype.ErrCannotCalculate
}

// --- CalendarEvent conversion ---

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	var sb strings.Builder
	sb.WriteString("PT")
	if h > 0 {
		fmt.Fprintf(&sb, "%dH", h)
	}
	if m > 0 {
		fmt.Fprintf(&sb, "%dM", m)
	}
	if sec > 0 {
		fmt.Fprintf(&sb, "%dS", sec)
	}
	if sb.Len() == 2 {
		return "PT0S"
	}
	return sb.String()
}

// parseDuration parses the ISO8601 subset P#DT#H#M#S.
func parseDuration(s string) (time.Duration, error) {
	orig := s
	if !strings.HasPrefix(s, "P") {
		return 0, fmt.Errorf("duration %q does not start with P", orig)
	}
	s = s[1:]
	datePart, timePart := s, ""
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		datePart, timePart = s[:i], s[i+1:]
	} else if strings.ContainsAny(s, "HMS") {
		return 0, fmt.Errorf("duration %q missing T separator", orig)
	}
	var d time.Duration
	parsePart := func(part string, units map[byte]time.Duration) error {
		for part != "" {
			i := 0
			for i < len(part) && part[i] >= '0' && part[i] <= '9' {
				i++
			}
			if i == 0 || i >= len(part) {
				return fmt.Errorf("bad duration %q", orig)
			}
			n, err := strconv.Atoi(part[:i])
			if err != nil {
				return fmt.Errorf("bad duration %q", orig)
			}
			unit, ok := units[part[i]]
			if !ok {
				return fmt.Errorf("unsupported duration unit %q in %q", part[i], orig)
			}
			d += time.Duration(n) * unit
			part = part[i+1:]
		}
		return nil
	}
	if err := parsePart(datePart, map[byte]time.Duration{'D': 24 * time.Hour}); err != nil {
		return 0, err
	}
	if err := parsePart(timePart, map[byte]time.Duration{'H': time.Hour, 'M': time.Minute, 'S': time.Second}); err != nil {
		return 0, err
	}
	return d, nil
}

func eventToJMAP(ev bridge.Event) calendars.CalendarEvent {
	loc := time.UTC
	var tz *string
	if ev.TimeZone != "" {
		if l, err := time.LoadLocation(ev.TimeZone); err == nil {
			loc = l
			z := ev.TimeZone
			tz = &z
		}
	}
	// shortcut: floating events (no IANA zone) are formatted as wall
	// clock in UTC with timeZone null (JSCalendar floating time).
	var start, duration string
	if ev.AllDay {
		start = ev.Start.In(loc).Format("2006-01-02") + "T00:00:00"
		days := int(ev.End.Sub(ev.Start).Hours() / 24)
		if days < 1 {
			days = 1
		}
		duration = fmt.Sprintf("P%dD", days)
	} else {
		start = ev.Start.In(loc).Format("2006-01-02T15:04:05")
		duration = formatDuration(ev.End.Sub(ev.Start))
	}
	out := calendars.CalendarEvent{
		Type: "Event", ID: jmap.ID(ev.ID),
		CalendarIDs:    map[jmap.ID]bool{jmap.ID(ev.CalendarID): true},
		UID:            ev.UID,
		Title:          ev.Summary,
		Description:    ev.Description,
		Start:          start,
		TimeZone:       tz,
		Duration:       duration,
		AllDay:         ev.AllDay,
		Status:         "confirmed",
		FreeBusyStatus: "busy",
		Sequence:       uint(max(ev.Sequence, 0)),
	}
	if r := recurrenceFromRRULE(ev.Recurrence); r != nil {
		out.RecurrenceRule = r
	}
	if ev.Organizer != "" || len(ev.Attendees) > 0 {
		parts := map[string]*calendars.Participant{}
		if ev.Organizer != "" {
			parts["org"] = &calendars.Participant{
				Email: ev.Organizer, Kind: "individual",
				Roles:               map[string]bool{"owner": true},
				ParticipationStatus: "accepted",
			}
		}
		for i, a := range ev.Attendees {
			status := a.Status
			if status == "" {
				status = "needs-action"
			}
			p := &calendars.Participant{
				Email: a.Email, Kind: "individual",
				Roles:               map[string]bool{"attendee": true},
				ParticipationStatus: status,
			}
			if a.Name != "" {
				p.Name = strPtr(a.Name)
			}
			parts[fmt.Sprintf("a%d", i+1)] = p
		}
		out.Participants = parts
	}
	if ev.ReminderMin != nil {
		out.Alerts = map[string]*calendars.Alert{
			"r1": {Type: "OffsetTrigger", Offset: fmt.Sprintf("-PT%dM", *ev.ReminderMin), RelativeTo: "start"},
		}
	}
	return out
}

func recurrenceFromRRULE(rrule string) *calendars.RecurrenceRule {
	s := strings.TrimSpace(rrule)
	if s == "" {
		return nil
	}
	if i := strings.IndexByte(s, ':'); i >= 0 && !strings.Contains(s[:i], "=") && !strings.Contains(s[:i], ";") {
		s = s[i+1:] // strip "RRULE:" prefix
	}
	rule := &calendars.RecurrenceRule{}
	set := false
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "FREQ":
			rule.Frequency = strings.ToLower(strings.TrimSpace(v))
			set = true
		case "INTERVAL":
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				rule.Interval = uint(n)
				set = true
			}
		case "COUNT":
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
				c := uint(n)
				rule.Count = &c
				set = true
			}
		case "UNTIL":
			u := strings.TrimSpace(v)
			rule.Until = &u
			set = true
		default:
			// shortcut: BY* and other RRULE parts are dropped.
		}
	}
	if !set || rule.Frequency == "" {
		return nil
	}
	return rule
}

func recurrenceToRRULE(r *calendars.RecurrenceRule) string {
	if r == nil || r.Frequency == "" {
		return ""
	}
	parts := []string{"FREQ=" + strings.ToUpper(r.Frequency)}
	if r.Interval > 0 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", r.Interval))
	}
	if r.Count != nil {
		parts = append(parts, fmt.Sprintf("COUNT=%d", *r.Count))
	}
	if r.Until != nil && *r.Until != "" {
		parts = append(parts, "UNTIL="+*r.Until)
	}
	return strings.Join(parts, ";")
}

func eventFromJMAP(ce calendars.CalendarEvent) (bridge.Event, *jmap.SetError) {
	invalid := func(desc string, props ...string) *jmap.SetError {
		return &jmap.SetError{Type: "invalidProperties", Description: desc, Properties: props}
	}
	ev := bridge.Event{
		ID: string(ce.ID), UID: ce.UID, Summary: ce.Title, Description: ce.Description,
		AllDay: ce.AllDay, Sequence: int(ce.Sequence),
	}
	calIDs := make([]string, 0, len(ce.CalendarIDs))
	for id, ok := range ce.CalendarIDs {
		if ok {
			calIDs = append(calIDs, string(id))
		}
	}
	sort.Strings(calIDs)
	if len(calIDs) > 0 {
		ev.CalendarID = calIDs[0]
	}
	loc := time.UTC
	if ce.TimeZone != nil && *ce.TimeZone != "" {
		l, err := time.LoadLocation(*ce.TimeZone)
		if err != nil {
			return ev, invalid("unknown timeZone: "+*ce.TimeZone, "timeZone")
		}
		loc = l
		ev.TimeZone = *ce.TimeZone
	}
	// Adapter works in IANA zones; bridge.ToWindowsZone is applied
	// upstream by providers that need Windows zone names.
	start, err := time.ParseInLocation("2006-01-02T15:04:05", ce.Start, loc)
	if err != nil {
		// tolerate fractional seconds
		start, err = time.ParseInLocation("2006-01-02T15:04:05.999999999", ce.Start, loc)
		if err != nil {
			return ev, invalid("bad start: "+err.Error(), "start")
		}
	}
	ev.Start = start
	dur := time.Duration(0)
	if ce.Duration != "" {
		dur, err = parseDuration(ce.Duration)
		if err != nil {
			return ev, invalid("bad duration: "+err.Error(), "duration")
		}
	}
	ev.End = start.Add(dur)

	keys := sortedMapKeys(ce.Participants)
	for _, k := range keys {
		p := ce.Participants[k]
		if p == nil {
			continue
		}
		if p.Roles["owner"] {
			ev.Organizer = p.Email
			continue
		}
		a := bridge.Attendee{Email: p.Email, Status: p.ParticipationStatus}
		if p.Name != nil {
			a.Name = *p.Name
		}
		ev.Attendees = append(ev.Attendees, a)
	}
	ev.Recurrence = recurrenceToRRULE(ce.RecurrenceRule)
	for _, k := range sortedMapKeys(ce.Alerts) {
		al := ce.Alerts[k]
		if al == nil || al.Type != "OffsetTrigger" {
			continue
		}
		off := strings.TrimSpace(al.Offset)
		if !strings.HasPrefix(off, "-") {
			continue
		}
		if d, err := parseDuration(strings.TrimPrefix(off, "-")); err == nil {
			mins := int(d.Minutes())
			ev.ReminderMin = &mins
		}
		break // first OffsetTrigger only
	}
	return ev, nil
}

// --- CalendarEvent store ---

type eventStore struct{ b *Backend }

// eventWindow is the ListEvents scan window.
// shortcut: events outside now-3y..now+3y are invisible to JMAP.
func eventWindow() (time.Time, time.Time) {
	now := time.Now()
	return now.AddDate(-3, 0, 0), now.AddDate(3, 0, 0)
}

func (s *eventStore) fetchBridge(ctx context.Context, account jmap.ID) ([]bridge.Event, error) {
	var cals []bridge.Calendar
	if err := s.b.call(ctx, string(account), "ListCalendars", func(ctx context.Context, p bridge.Provider) error {
		var err error
		cals, err = p.ListCalendars(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	from, to := eventWindow()
	var out []bridge.Event
	for _, c := range cals {
		var evs []bridge.Event
		if err := s.b.call(ctx, string(account), "ListEvents", func(ctx context.Context, p bridge.Provider) error {
			var err error
			evs, err = p.ListEvents(ctx, c.ID, from, to)
			return err
		}); err != nil {
			return nil, err
		}
		for _, ev := range evs {
			if ev.CalendarID == "" {
				ev.CalendarID = c.ID
			}
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s *eventStore) State(ctx context.Context, account jmap.ID) (string, error) {
	evs, err := s.fetchBridge(ctx, account)
	if err != nil {
		return "", err
	}
	sorted := append([]bridge.Event(nil), evs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	parts := make([]string, 0, len(sorted))
	for _, ev := range sorted {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s|%d",
			ev.ID, ev.CalendarID, ev.Start.UTC().Format(time.RFC3339Nano), ev.End.UTC().Format(time.RFC3339Nano), ev.Summary, ev.Sequence))
	}
	return hashStrings(parts), nil
}

func (s *eventStore) Get(ctx context.Context, account jmap.ID, ids []jmap.ID) ([]calendars.CalendarEvent, []jmap.ID, error) {
	evs, err := s.fetchBridge(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	if ids == nil {
		out := make([]calendars.CalendarEvent, 0, len(evs))
		for _, ev := range evs {
			out = append(out, eventToJMAP(ev))
		}
		return out, nil, nil
	}
	// No provider GetEvent exists: scan the window and match by ID.
	byID := map[jmap.ID]bridge.Event{}
	for _, ev := range evs {
		byID[jmap.ID(ev.ID)] = ev
	}
	var found []calendars.CalendarEvent
	var notFound []jmap.ID
	for _, id := range ids {
		if ev, ok := byID[id]; ok {
			found = append(found, eventToJMAP(ev))
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

func (s *eventStore) Create(ctx context.Context, account jmap.ID, obj calendars.CalendarEvent) (calendars.CalendarEvent, *jmap.SetError) {
	ev, serr := eventFromJMAP(obj)
	if serr != nil {
		return calendars.CalendarEvent{}, serr
	}
	var created bridge.Event
	if err := s.b.call(ctx, string(account), "CreateEvent", func(ctx context.Context, p bridge.Provider) error {
		var err error
		created, err = p.CreateEvent(ctx, ev)
		return err
	}); err != nil {
		return calendars.CalendarEvent{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	if created.CalendarID == "" {
		created.CalendarID = ev.CalendarID
	}
	return eventToJMAP(created), nil
}

func (s *eventStore) Update(ctx context.Context, account jmap.ID, obj calendars.CalendarEvent) (*calendars.CalendarEvent, *jmap.SetError) {
	ev, serr := eventFromJMAP(obj)
	if serr != nil {
		return nil, serr
	}
	if err := s.b.call(ctx, string(account), "UpdateEvent", func(ctx context.Context, p bridge.Provider) error {
		return p.UpdateEvent(ctx, ev)
	}); err != nil {
		return nil, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	return nil, nil
}

func (s *eventStore) Destroy(ctx context.Context, account jmap.ID, id jmap.ID) *jmap.SetError {
	evs, err := s.fetchBridge(ctx, account)
	if err != nil {
		return &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	for _, ev := range evs {
		if jmap.ID(ev.ID) == id {
			if err := s.b.call(ctx, string(account), "DeleteEvent", func(ctx context.Context, p bridge.Provider) error {
				return p.DeleteEvent(ctx, ev.CalendarID, string(id))
			}); err != nil {
				return &jmap.SetError{Type: "serverFail", Description: err.Error()}
			}
			return nil
		}
	}
	return &jmap.SetError{Type: "notFound"}
}

func (s *eventStore) Changes(ctx context.Context, account jmap.ID, sinceState string, _ int) ([]jmap.ID, []jmap.ID, []jmap.ID, string, bool, error) {
	cur, err := s.State(ctx, account)
	if err != nil {
		return nil, nil, nil, "", false, err
	}
	if sinceState == cur {
		return []jmap.ID{}, []jmap.ID{}, []jmap.ID{}, cur, false, nil
	}
	return nil, nil, nil, "", false, datatype.ErrCannotCalculate
}
