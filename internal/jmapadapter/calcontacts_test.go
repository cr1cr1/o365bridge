package jmapadapter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// --- fake provider: calendars / events / contacts ---

func (p *fakeProvider) seedCalendar(c bridge.Calendar, evs ...bridge.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cals = append(p.cals, c)
	if p.events == nil {
		p.events = map[string][]bridge.Event{}
	}
	p.events[c.ID] = append(p.events[c.ID], evs...)
}

func (p *fakeProvider) seedContact(c bridge.Contact) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contacts = append(p.contacts, c)
}

func (p *fakeProvider) ListCalendars(context.Context) ([]bridge.Calendar, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bridge.Calendar(nil), p.cals...), nil
}

func (p *fakeProvider) ListEvents(_ context.Context, calendarID string, from, to time.Time) ([]bridge.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []bridge.Event
	for _, ev := range p.events[calendarID] {
		if !ev.Start.Before(from) && ev.Start.Before(to) {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (p *fakeProvider) CreateEvent(_ context.Context, ev bridge.Event) (bridge.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("ev-new-%d", p.seq)
	}
	p.createdEvents = append(p.createdEvents, ev)
	if p.events == nil {
		p.events = map[string][]bridge.Event{}
	}
	p.events[ev.CalendarID] = append(p.events[ev.CalendarID], ev)
	return ev, nil
}

func (p *fakeProvider) UpdateEvent(_ context.Context, ev bridge.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createdEvents = append(p.createdEvents, ev) // record last write
	for i, e := range p.events[ev.CalendarID] {
		if e.ID == ev.ID {
			p.events[ev.CalendarID][i] = ev
		}
	}
	return nil
}

func (p *fakeProvider) DeleteEvent(_ context.Context, calendarID, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	evs := p.events[calendarID]
	for i, e := range evs {
		if e.ID == id {
			p.events[calendarID] = append(evs[:i], evs[i+1:]...)
		}
	}
	return nil
}

func (p *fakeProvider) FreeBusy(context.Context, []string, time.Time, time.Time) (map[string][][2]time.Time, error) {
	return map[string][][2]time.Time{}, nil
}

func (p *fakeProvider) ListContacts(_ context.Context, _ string) ([]bridge.Contact, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bridge.Contact(nil), p.contacts...), nil
}

func (p *fakeProvider) CreateContact(_ context.Context, c bridge.Contact) (bridge.Contact, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	if c.ID == "" {
		c.ID = fmt.Sprintf("ct-new-%d", p.seq)
	}
	p.createdContacts = append(p.createdContacts, c)
	p.contacts = append(p.contacts, c)
	return c, nil
}

func (p *fakeProvider) UpdateContact(_ context.Context, c bridge.Contact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createdContacts = append(p.createdContacts, c)
	for i, e := range p.contacts {
		if e.ID == c.ID {
			p.contacts[i] = c
		}
	}
	return nil
}

func (p *fakeProvider) DeleteContact(_ context.Context, _, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.contacts {
		if e.ID == id {
			p.contacts = append(p.contacts[:i], p.contacts[i+1:]...)
		}
	}
	return nil
}

// --- helpers ---

func callJMAPAll(t *testing.T, h http.Handler, user string, calls ...[3]any) []json.RawMessage {
	t.Helper()
	var mcs []any
	for _, c := range calls {
		args, _ := json.Marshal(c[1])
		mcs = append(mcs, []any{c[0], json.RawMessage(args), c[2]})
	}
	body, _ := json.Marshal(map[string]any{
		"using": []string{
			"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail",
			"urn:ietf:params:jmap:calendars", "urn:ietf:params:jmap:contacts",
		},
		"methodCalls": mcs,
	})
	rec := doReq(t, h, "POST", "/jmap/", user, pw, body, "application/json")
	if rec.Code != 200 {
		t.Fatalf("api status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v %s", err, rec.Body.String())
	}
	var out []json.RawMessage
	for _, mr := range resp.MethodResponses {
		if len(mr) >= 2 {
			out = append(out, mr[1])
		}
	}
	return out
}

func TestSessionHasCalendarContactCapabilities(t *testing.T) {
	pa := newFakeProvider()
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})
	rec := doReq(t, h, "GET", "/jmap/session", "a@example.com", pw, nil, "")
	if rec.Code != 200 {
		t.Fatalf("session %d", rec.Code)
	}
	body := rec.Body.String()
	for _, uri := range []string{"urn:ietf:params:jmap:calendars", "urn:ietf:params:jmap:contacts"} {
		if !strings.Contains(body, uri) {
			t.Fatalf("session missing %s: %s", uri, body)
		}
	}
}

func TestCalendarAndEventGet(t *testing.T) {
	pa := newFakeProvider()
	berlin, _ := time.LoadLocation("Europe/Berlin")
	start := time.Date(2026, 6, 1, 10, 0, 0, 0, berlin)
	rem := 15
	pa.seedCalendar(bridge.Calendar{ID: "cal1", Name: "Work"}, bridge.Event{
		ID: "ev1", CalendarID: "cal1", UID: "uid1", Summary: "Standup", Description: "daily",
		Start: start, End: start.Add(time.Hour), TimeZone: "Europe/Berlin",
		Organizer:   "boss@example.com",
		Attendees:   []bridge.Attendee{{Email: "bob@example.com", Name: "Bob", Status: "accepted"}},
		ReminderMin: &rem, Recurrence: "FREQ=WEEKLY;INTERVAL=2;COUNT=5",
	})
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})
	acct := "a@example.com"

	res := callJMAPAll(t, h, acct, [3]any{"Calendar/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})
	s := string(res[0])
	if !strings.Contains(s, `"Work"`) || !strings.Contains(s, `"isDefault":true`) {
		t.Fatalf("Calendar/get: %s", s)
	}

	res = callJMAPAll(t, h, acct, [3]any{"CalendarEvent/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})
	s = string(res[0])
	for _, want := range []string{"Standup", `"start":"2026-06-01T10:00:00"`, "Europe/Berlin", "bob@example.com", `"accepted"`, "PT1H", "weekly", "-PT15M"} {
		if !strings.Contains(s, want) {
			t.Fatalf("CalendarEvent/get missing %q: %s", want, s)
		}
	}
}

func TestCalendarEventCreate(t *testing.T) {
	pa := newFakeProvider()
	pa.seedCalendar(bridge.Calendar{ID: "cal1", Name: "Work"})
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})
	acct := "a@example.com"

	res := callJMAPAll(t, h, acct, [3]any{"CalendarEvent/set", map[string]any{
		"accountId": acct,
		"create": map[string]any{"e1": map[string]any{
			"@type": "Event", "calendarIds": map[string]bool{"cal1": true},
			"title": "Planning", "start": "2026-06-02T09:00:00", "timeZone": "Europe/Berlin",
			"duration": "PT1H30M", "status": "confirmed", "freeBusyStatus": "busy",
			"participants": map[string]any{
				"org": map[string]any{"email": "boss@example.com", "roles": map[string]bool{"owner": true}, "participationStatus": "accepted"},
				"a1":  map[string]any{"email": "bob@example.com", "roles": map[string]bool{"attendee": true}, "participationStatus": "needs-action"},
			},
		}},
	}, "c1"})
	if !strings.Contains(string(res[0]), `"created"`) || strings.Contains(string(res[0]), `"notCreated":{`) || strings.Contains(string(res[0]), `"e1":{"type"`) {
		t.Fatalf("create: %s", res[0])
	}
	if len(pa.createdEvents) != 1 {
		t.Fatalf("provider CreateEvent not called: %s", res[0])
	}
	ev := pa.createdEvents[0]
	if ev.Summary != "Planning" || ev.CalendarID != "cal1" || ev.Organizer != "boss@example.com" {
		t.Fatalf("mapped event: %+v", ev)
	}
	if len(ev.Attendees) != 1 || ev.Attendees[0].Email != "bob@example.com" {
		t.Fatalf("attendees: %+v", ev.Attendees)
	}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	wantStart := time.Date(2026, 6, 2, 9, 0, 0, 0, berlin)
	if !ev.Start.Equal(wantStart) || ev.TimeZone != "Europe/Berlin" || ev.End.Sub(ev.Start) != 90*time.Minute {
		t.Fatalf("start/tz/duration: %v %q -> %v", ev.Start, ev.TimeZone, ev.End)
	}
}

func TestContactCardGetAndCreate(t *testing.T) {
	pa := newFakeProvider()
	pa.seedContact(bridge.Contact{ID: "ct1", FullName: "Alice Adams", FirstName: "Alice", LastName: "Adams", Emails: []string{"alice@example.com"}, Phones: []string{"+123"}, Org: "Acme", Notes: "hi"})
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa})
	acct := "a@example.com"

	res := callJMAPAll(t, h, acct, [3]any{"ContactCard/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})
	s := string(res[0])
	for _, want := range []string{"Alice Adams", "alice@example.com", "Acme", `"@type":"Card"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("ContactCard/get missing %q: %s", want, s)
		}
	}
	res = callJMAPAll(t, h, acct, [3]any{"AddressBook/get", map[string]any{"accountId": acct, "ids": nil}, "c1"})
	if !strings.Contains(string(res[0]), `"Contacts"`) {
		t.Fatalf("AddressBook/get: %s", res[0])
	}

	res = callJMAPAll(t, h, acct, [3]any{"ContactCard/set", map[string]any{
		"accountId": acct,
		"create": map[string]any{"c1": map[string]any{
			"@type": "Card", "addressBookIds": map[string]bool{"contacts": true},
			"name":   map[string]any{"full": "Carol Danvers"},
			"emails": map[string]any{"e1": map[string]any{"address": "carol@example.com"}},
			"phones": map[string]any{"p1": map[string]any{"number": "+999"}},
		}},
	}, "c1"})
	if len(pa.createdContacts) != 1 {
		t.Fatalf("provider CreateContact not called: %s", res[0])
	}
	c := pa.createdContacts[0]
	if c.FullName != "Carol Danvers" || c.FirstName != "Carol" || c.LastName != "Danvers" ||
		len(c.Emails) != 1 || c.Emails[0] != "carol@example.com" || len(c.Phones) != 1 || c.Phones[0] != "+999" {
		t.Fatalf("mapped contact: %+v", c)
	}
}

func TestEventIsolationTwoAccounts(t *testing.T) {
	pa := newFakeProvider()
	now := time.Now()
	pa.seedCalendar(bridge.Calendar{ID: "cal1", Name: "A"}, bridge.Event{ID: "evA", CalendarID: "cal1", Summary: "Secret A Event", Start: now, End: now.Add(time.Hour)})
	pb := newFakeProvider()
	pb.seedCalendar(bridge.Calendar{ID: "cal1", Name: "B"}, bridge.Event{ID: "evB", CalendarID: "cal1", Summary: "Secret B Event", Start: now, End: now.Add(time.Hour)})
	h := setup(t, map[string]*fakeProvider{"a@example.com": pa, "b@example.com": pb})

	res := callJMAPAll(t, h, "b@example.com", [3]any{"CalendarEvent/query", map[string]any{"accountId": "b@example.com"}, "c1"})
	var q struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(res[0], &q); err != nil || len(q.IDs) != 1 {
		t.Fatalf("B query: %v %s", err, res[0])
	}
	res = callJMAPAll(t, h, "b@example.com", [3]any{"CalendarEvent/get", map[string]any{"accountId": "b@example.com", "ids": q.IDs}, "c1"})
	s := string(res[0])
	if strings.Contains(s, "Secret A Event") || !strings.Contains(s, "Secret B Event") {
		t.Fatalf("isolation: %s", s)
	}
}
