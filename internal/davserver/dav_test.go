package davserver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// --- in-memory fake provider ---

type fakeLogin struct{}

func (fakeLogin) Login(_ context.Context, email string, ch bridge.Channel) (bridge.Session, error) {
	return bridge.Session{Email: email, Channel: ch, AccessToken: "t", TokenExpiry: time.Now().Add(time.Hour)}, nil
}
func (fakeLogin) Renew(_ context.Context, _ string, s bridge.Session) (bridge.Session, error) {
	s.TokenExpiry = time.Now().Add(time.Hour)
	return s, nil
}

type fakeFactory struct{ p *fakeProvider }

func (f fakeFactory) New(string, bridge.Session) bridge.Provider { return f.p }

type respondCall struct{ Cal, ID, Resp string }

type fakeProvider struct {
	bridge.Provider
	mu       sync.Mutex
	account  string
	events   map[string]bridge.Event // by ID
	tasks    map[string]bridge.Task
	contacts map[string]bridge.Contact // by ID
	responds []respondCall
	seq      int
	listErr  error
}

func newFake() *fakeProvider {
	return &fakeProvider{account: acct, events: map[string]bridge.Event{}, tasks: map[string]bridge.Task{}, contacts: map[string]bridge.Contact{}}
}

func (p *fakeProvider) Ping(context.Context) error { return nil }
func (p *fakeProvider) ListCalendars(context.Context) ([]bridge.Calendar, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return []bridge.Calendar{{ID: "cal1", Name: "Calendar"}}, nil
}
func (p *fakeProvider) ListTaskLists(context.Context) ([]bridge.Calendar, error) {
	return []bridge.Calendar{{ID: "tasks1", Name: "Tasks", TasksOnly: true}}, nil
}
func (p *fakeProvider) ListEvents(_ context.Context, cal string, from, to time.Time) ([]bridge.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []bridge.Event
	for _, e := range p.events {
		if e.CalendarID == cal {
			out = append(out, e)
		}
	}
	return out, nil
}
func (p *fakeProvider) CreateEvent(_ context.Context, ev bridge.Event) (bridge.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	if ev.ID == "" {
		ev.ID = "id-" + ev.UID
	}
	p.events[ev.ID] = ev
	return ev, nil
}
func (p *fakeProvider) UpdateEvent(_ context.Context, ev bridge.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events[ev.ID] = ev
	return nil
}
func (p *fakeProvider) DeleteEvent(_ context.Context, _, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.events, id)
	return nil
}
func (p *fakeProvider) RespondEvent(_ context.Context, cal, id, resp string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responds = append(p.responds, respondCall{cal, id, resp})
	if ev, ok := p.events[id]; ok {
		for i, a := range ev.Attendees {
			if bridge.NormalizeEmail(a.Email) == p.account {
				ev.Attendees[i].Status = resp
			}
		}
		p.events[id] = ev
	}
	return nil
}
func (p *fakeProvider) ListTasks(_ context.Context, list string) ([]bridge.Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []bridge.Task
	for _, tk := range p.tasks {
		if tk.ListID == list {
			out = append(out, tk)
		}
	}
	return out, nil
}
func (p *fakeProvider) CreateTask(_ context.Context, tk bridge.Task) (bridge.Task, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tk.ID == "" {
		tk.ID = "id-" + tk.UID
	}
	p.tasks[tk.ID] = tk
	return tk, nil
}
func (p *fakeProvider) UpdateTask(_ context.Context, tk bridge.Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasks[tk.ID] = tk
	return nil
}
func (p *fakeProvider) DeleteTask(_ context.Context, _, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tasks, id)
	return nil
}
func (p *fakeProvider) ListContacts(context.Context, string) ([]bridge.Contact, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []bridge.Contact
	for _, c := range p.contacts {
		out = append(out, c)
	}
	return out, nil
}
func (p *fakeProvider) CreateContact(_ context.Context, c bridge.Contact) (bridge.Contact, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.ID == "" {
		c.ID = "id-" + c.UID
	}
	p.contacts[c.ID] = c
	return c, nil
}
func (p *fakeProvider) UpdateContact(_ context.Context, c bridge.Contact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contacts[c.ID] = c
	return nil
}
func (p *fakeProvider) DeleteContact(_ context.Context, _, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.contacts, id)
	return nil
}

const (
	acct = "user@example.com"
	pw   = "instance-secret"
)

func newHandler(t *testing.T, p *fakeProvider) http.Handler {
	t.Helper()
	return newHandlerWithLogger(t, p, zerolog.Nop())
}

func newHandlerWithLogger(t *testing.T, p *fakeProvider, logger zerolog.Logger) http.Handler {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{
		StateDir: t.TempDir(), Login: fakeLogin{}, Factory: fakeFactory{p},
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(reg, pw, logger)
}

// Upstream failures must be warnings in the bridge log, per
// account (they used to be debug-only and invisible).
func TestDAVUpstreamErrorLoggedPerAccount(t *testing.T) {
	var buf bytes.Buffer
	fake := newFake()
	fake.listErr = errors.New("upstream exploded")
	h := newHandlerWithLogger(t, fake, zerolog.New(&buf))
	rr := req(t, h, "PROPFIND", "/caldav/"+acct+"/", "", true)
	if rr.Code != 502 {
		t.Fatalf("code = %d, want 502", rr.Code)
	}
	out := buf.String()
	for _, want := range []string{`"level":"warn"`, `"protocol":"dav"`, `"account":"` + acct + `"`, "upstream exploded"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s: %s", want, out)
		}
	}
}

func req(t *testing.T, h http.Handler, method, path, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		r.SetBasicAuth(acct, pw)
	}
	if body != "" {
		r.Header.Set("Content-Type", "text/xml; charset=utf-8")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func TestDAV401WithoutAuth(t *testing.T) {
	h := newHandler(t, newFake())
	rr := req(t, h, "PROPFIND", "/caldav/", "", false)
	if rr.Code != 401 || rr.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("code=%d www-auth=%q, want 401 with header", rr.Code, rr.Header().Get("WWW-Authenticate"))
	}
	rr = req(t, h, "PROPFIND", "/caldav/", "", true)
	// wrong password also 401: rebuild manually
	r := httptest.NewRequest("PROPFIND", "/caldav/", nil)
	r.SetBasicAuth(acct, "wrong")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, r)
	if rr2.Code != 401 {
		t.Fatalf("wrong pw code=%d", rr2.Code)
	}
	_ = rr
}

func TestDAVDiscoveryAndPropfindHome(t *testing.T) {
	h := newHandler(t, newFake())
	rr := req(t, h, "GET", "/.well-known/caldav", "", true)
	if rr.Code != 301 || rr.Header().Get("Location") != "/caldav/" {
		t.Fatalf("well-known caldav: %d loc=%q", rr.Code, rr.Header().Get("Location"))
	}
	rr = req(t, h, "GET", "/.well-known/carddav", "", true)
	if rr.Code != 301 || rr.Header().Get("Location") != "/carddav/" {
		t.Fatalf("well-known carddav: %d loc=%q", rr.Code, rr.Header().Get("Location"))
	}
	rr = req(t, h, "PROPFIND", "/", "", true)
	body := rr.Body.String()
	if rr.Code != 207 || !strings.Contains(body, "calendar-home-set") || !strings.Contains(body, "/caldav/"+acct+"/") {
		t.Fatalf("root propfind %d %s", rr.Code, body)
	}
	if !strings.Contains(body, "addressbook-home-set") || !strings.Contains(body, "current-user-principal") {
		t.Fatalf("root propfind missing principal props: %s", body)
	}
	rr = req(t, h, "PROPFIND", "/caldav/"+acct+"/", "", true)
	body = rr.Body.String()
	if rr.Code != 207 || !strings.Contains(body, "Calendar") || !strings.Contains(body, "cal1") {
		t.Fatalf("home propfind %d %s", rr.Code, body)
	}
	if !strings.Contains(body, "VEVENT") {
		t.Fatalf("home propfind missing supported component: %s", body)
	}
	rr = req(t, h, "PROPFIND", "/carddav/"+acct+"/", "", true)
	if !strings.Contains(rr.Body.String(), "contacts") {
		t.Fatalf("card home %d %s", rr.Code, rr.Body.String())
	}
}

const eventICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:ev1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260601T100000Z\r\nDTEND:20260601T110000Z\r\nSUMMARY:Meet\\, Inc.\r\nDESCRIPTION:Line1\\nLine2\r\nLOCATION:Room 1\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func TestDAVPutEventThenReportAndGet(t *testing.T) {
	h := newHandler(t, newFake())
	rr := req(t, h, "PUT", "/caldav/"+acct+"/cal1/ev1.ics", eventICS, true)
	if rr.Code != 201 && rr.Code != 200 && rr.Code != 204 {
		t.Fatalf("PUT event %d %s", rr.Code, rr.Body.String())
	}
	report := `<?xml version="1.0"?><C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT"><C:time-range start="20260101T000000Z" end="20270101T000000Z"/></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`
	rr = req(t, h, "REPORT", "/caldav/"+acct+"/cal1/", report, true)
	body := rr.Body.String()
	if rr.Code != 207 || !strings.Contains(body, "ev1") || !strings.Contains(body, "Meet") {
		t.Fatalf("REPORT %d %s", rr.Code, body)
	}
	if !strings.Contains(body, "getetag") || !strings.Contains(body, "calendar-data") {
		t.Fatalf("REPORT missing etag/data: %s", body)
	}
	rr = req(t, h, "GET", "/caldav/"+acct+"/cal1/ev1.ics", "", true)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "UID:ev1") {
		t.Fatalf("GET event %d %s", rr.Code, rr.Body.String())
	}
	// multiget
	mg := `<?xml version="1.0"?><C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><D:href>/caldav/` + acct + `/cal1/ev1.ics</D:href></C:calendar-multiget>`
	rr = req(t, h, "REPORT", "/caldav/"+acct+"/cal1/", mg, true)
	if !strings.Contains(rr.Body.String(), "UID:ev1") {
		t.Fatalf("multiget %d %s", rr.Code, rr.Body.String())
	}
	// DELETE then GET 404
	rr = req(t, h, "DELETE", "/caldav/"+acct+"/cal1/ev1.ics", "", true)
	if rr.Code != 204 && rr.Code != 200 {
		t.Fatalf("DELETE %d", rr.Code)
	}
	rr = req(t, h, "GET", "/caldav/"+acct+"/cal1/ev1.ics", "", true)
	if rr.Code != 404 {
		t.Fatalf("GET after delete %d, want 404", rr.Code)
	}
}

func TestDAVRespondEventOnPartstatChange(t *testing.T) {
	fake := newFake()
	fake.events["e9"] = bridge.Event{
		ID: "e9", CalendarID: "cal1", UID: "inv1", Summary: "Invite",
		Start: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		Organizer: "boss@example.com",
		Attendees: []bridge.Attendee{{Email: acct, Status: "needs-action"}},
	}
	h := newHandler(t, fake)
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:inv1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260601T100000Z\r\nDTEND:20260601T110000Z\r\nSUMMARY:Invite\r\nORGANIZER:mailto:boss@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:" + acct + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	rr := req(t, h, "PUT", "/caldav/"+acct+"/cal1/inv1.ics", ics, true)
	if rr.Code >= 300 {
		t.Fatalf("PUT invite %d %s", rr.Code, rr.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.responds) != 1 || fake.responds[0].Resp != "accepted" || fake.responds[0].ID != "e9" {
		t.Fatalf("responds = %+v, want one accepted for e9", fake.responds)
	}
}

func TestDAVTaskPutAndReport(t *testing.T) {
	h := newHandler(t, newFake())
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:task1\r\nSUMMARY:Do it\r\nDUE:20260602T100000Z\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	rr := req(t, h, "PUT", "/caldav/"+acct+"/tasks1/task1.ics", ics, true)
	if rr.Code >= 300 {
		t.Fatalf("PUT task %d %s", rr.Code, rr.Body.String())
	}
	report := `<?xml version="1.0"?><C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VTODO"/></C:comp-filter></C:filter></C:calendar-query>`
	rr = req(t, h, "REPORT", "/caldav/"+acct+"/tasks1/", report, true)
	if rr.Code != 207 || !strings.Contains(rr.Body.String(), "task1") {
		t.Fatalf("task REPORT %d %s", rr.Code, rr.Body.String())
	}
}

const vcard = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:c1\r\nFN:Jane Doe\r\nN:Doe;Jane;;;\r\nEMAIL;TYPE=INTERNET:jane@example.com\r\nTEL:+1-555-0100\r\nORG:Acme\r\nNOTE:Hi\\, there\r\nEND:VCARD\r\n"

func TestDAVCardRoundTrip(t *testing.T) {
	h := newHandler(t, newFake())
	rr := req(t, h, "PUT", "/carddav/"+acct+"/contacts/c1.vcf", vcard, true)
	if rr.Code >= 300 {
		t.Fatalf("PUT vcard %d %s", rr.Code, rr.Body.String())
	}
	rr = req(t, h, "GET", "/carddav/"+acct+"/contacts/c1.vcf", "", true)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Jane Doe") || !strings.Contains(rr.Body.String(), "jane@example.com") {
		t.Fatalf("GET vcard %d %s", rr.Code, rr.Body.String())
	}
	q := `<?xml version="1.0"?><A:addressbook-query xmlns:D="DAV:" xmlns:A="urn:ietf:params:xml:ns:carddav"><D:prop><D:getetag/><A:address-data/></D:prop></A:addressbook-query>`
	rr = req(t, h, "REPORT", "/carddav/"+acct+"/contacts/", q, true)
	if rr.Code != 207 || !strings.Contains(rr.Body.String(), "Jane Doe") {
		t.Fatalf("addressbook-query %d %s", rr.Code, rr.Body.String())
	}
	rr = req(t, h, "DELETE", "/carddav/"+acct+"/contacts/c1.vcf", "", true)
	if rr.Code != 204 && rr.Code != 200 {
		t.Fatalf("DELETE vcard %d", rr.Code)
	}
	rr = req(t, h, "GET", "/carddav/"+acct+"/contacts/c1.vcf", "", true)
	if rr.Code != 404 {
		t.Fatalf("GET deleted vcard %d", rr.Code)
	}
}

// --- codec unit tests ---

func TestICalendarCodecFoldingEscapingAllDay(t *testing.T) {
	long := strings.Repeat("Long summary with, commas; and ", 6) // >75 octets
	ev := bridge.Event{
		UID: "u1", Summary: long, Description: "a,b;c\nd", Location: "Room;1",
		Start: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		AllDay: true, Organizer: "boss@example.com",
		Attendees:  []bridge.Attendee{{Email: acct, Name: "User", Status: "accepted"}},
		Recurrence: "FREQ=WEEKLY",
	}
	data := string(generateEventICalendar(ev))
	for _, line := range strings.Split(strings.TrimRight(data, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("folded line >75 octets: %q (%d)", line, len(line))
		}
	}
	if !strings.Contains(data, "DTSTART;VALUE=DATE:20260601") {
		t.Fatalf("all-day DTSTART missing: %s", data)
	}
	events, _, err := parseICalendar([]byte(data))
	if err != nil || len(events) != 1 {
		t.Fatalf("parse events=%v err=%v", events, err)
	}
	got := events[0]
	if got.Summary != long || got.Description != "a,b;c\nd" || !got.AllDay || got.Recurrence != "FREQ=WEEKLY" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].Status != "accepted" {
		t.Fatalf("attendee round-trip: %+v", got.Attendees)
	}
	if got.Organizer != "boss@example.com" {
		t.Fatalf("organizer = %q", got.Organizer)
	}
}

func TestICalendarVTODOCompleted(t *testing.T) {
	due := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	done := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	tk := bridge.Task{UID: "t1", Title: "Finish", Body: "details", Due: &due, Completed: &done, Status: "completed", Priority: 1, Recur: "FREQ=DAILY"}
	data := string(generateTaskICalendar(tk))
	if !strings.Contains(data, "STATUS:COMPLETED") || !strings.Contains(data, "COMPLETED:20260601T090000Z") {
		t.Fatalf("todo completed missing: %s", data)
	}
	_, tasks, err := parseICalendar([]byte(data))
	if err != nil || len(tasks) != 1 {
		t.Fatalf("parse tasks=%v err=%v", tasks, err)
	}
	got := tasks[0]
	if got.Title != "Finish" || got.Completed == nil || !got.Completed.Equal(done) || got.Due == nil || !got.Due.Equal(due) || got.Priority != 1 || got.Status != "completed" {
		t.Fatalf("todo round-trip: %+v", got)
	}
}

func TestVCardCodec(t *testing.T) {
	c := bridge.Contact{UID: "c9", FullName: "Jane Doe", FirstName: "Jane", LastName: "Doe", Emails: []string{"jane@example.com"}, Phones: []string{"+1-555"}, Org: "Acme, Inc.", Notes: "line1\nline2"}
	data := string(generateVCard(c))
	for _, line := range strings.Split(strings.TrimRight(data, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Fatalf("vcard line >75: %q", line)
		}
	}
	got, err := parseVCard([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if got.FullName != c.FullName || got.FirstName != "Jane" || got.LastName != "Doe" || len(got.Emails) != 1 || got.Emails[0] != "jane@example.com" || got.Org != "Acme, Inc." || got.Notes != "line1\nline2" {
		t.Fatalf("vcard round-trip: %+v", got)
	}
}
