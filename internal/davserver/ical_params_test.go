package davserver

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePropQuotedParameters(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       icalProp
	}{
		{"plain value", `DESCRIPTION:Notes: keep ; and "quotes"`, icalProp{Name: "DESCRIPTION", Params: map[string]string{}, Value: `Notes: keep ; and "quotes"`}},
		{"quoted data URI", `description;altrep="data:text/html;charset=utf-8,%3Cp%3EHTML%3C/p%3E";language=en:Plain notes`, icalProp{Name: "DESCRIPTION", Params: map[string]string{"ALTREP": "data:text/html;charset=utf-8,%3Cp%3EHTML%3C/p%3E", "LANGUAGE": "en"}, Value: "Plain notes"}},
		{"quoted attendee name", `ATTENDEE;CN="Doe; Ada: Lead, Team";PARTSTAT=ACCEPTED:mailto:ada@example.com`, icalProp{Name: "ATTENDEE", Params: map[string]string{"CN": "Doe; Ada: Lead, Team", "PARTSTAT": "ACCEPTED"}, Value: "mailto:ada@example.com"}},
		{"empty description", `DESCRIPTION;ALTREP="https://example.invalid/notes:one;two":`, icalProp{Name: "DESCRIPTION", Params: map[string]string{"ALTREP": "https://example.invalid/notes:one;two"}, Value: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProp(tc.line)
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseProp(%q) = %+v, %v; want %+v", tc.line, got, ok, tc.want)
			}
		})
	}
	for _, line := range []string{"DESCRIPTION", `DESCRIPTION;ALTREP="data:text/html,unterminated:notes`, `DESCRIPTION;ALTREP="data:text/html,no-value-separator"`} {
		if got, ok := parseProp(line); ok {
			t.Errorf("accepted property without an unquoted value separator: %q: %+v", line, got)
		}
	}
}

func TestDAVEventDescriptionALTREPDoesNotLeakIntoNotes(t *testing.T) {
	p := newFake()
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/cal1/quoted-event.ics"
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:quoted-event\r\nSUMMARY:Event\r\nDTSTART:20261015T100000Z\r\nDTEND:20261015T110000Z\r\n" +
		`DESCRIPTION;ALTREP="data:text/html;charset=utf-8,%3Cp%3EHTML%3C/p%3E":Plain event note` +
		"\r\n" + `ATTENDEE;CN="Doe; Ada: Lead";PARTSTAT=ACCEPTED:mailto:ada@example.com` +
		"\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if rr := req(t, h, "PUT", path, data, true); rr.Code != 201 {
		t.Fatalf("create event = %d: %s", rr.Code, rr.Body.String())
	}
	ev := p.events["id-quoted-event"]
	if ev.Description != "Plain event note" || len(ev.Attendees) != 1 || ev.Attendees[0].Name != "Doe; Ada: Lead" || ev.Attendees[0].Email != "ada@example.com" || ev.Attendees[0].Status != "accepted" {
		t.Fatalf("event quoted parameters corrupted native fields: %+v", ev)
	}
	data = strings.Replace(data, "Plain event note", "Edited event note", 1)
	if rr := req(t, h, "PUT", path, data, true); rr.Code != 204 {
		t.Fatalf("edit event = %d: %s", rr.Code, rr.Body.String())
	}
	if got := p.events["id-quoted-event"].Description; got != "Edited event note" {
		t.Fatalf("edited event description metadata leaked upstream: %q", got)
	}
}

func TestVCardQuotedParametersDoNotCorruptNotes(t *testing.T) {
	c, err := parseVCard([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Ada\r\n" + `NOTE;ALTID="urn:notes;primary";LANGUAGE=en:Plain contact notes: keep ; text` + "\r\nEND:VCARD\r\n"))
	if err != nil || c.Notes != "Plain contact notes: keep ; text" {
		t.Fatalf("contact notes = %q, %v", c.Notes, err)
	}
}

func TestDAVTaskDescriptionALTREPDoesNotLeakIntoNotes(t *testing.T) {
	p := newFake()
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/tasks1/quoted-description.ics"
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:quoted-description\r\nSUMMARY:Task\r\n" +
		`DESCRIPTION;ALTREP="data:text/html,%3Cbody%20id%3D%22calendar-item-body%22%3ENote%3C%2Fbody%3E":A task note` +
		"\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	if rr := req(t, h, "PUT", path, data, true); rr.Code != 201 {
		t.Fatalf("create task = %d: %s", rr.Code, rr.Body.String())
	}
	if got := p.tasks["id-quoted-description"].Body; got != "A task note" {
		t.Fatalf("task description metadata leaked upstream: %q", got)
	}
	data = strings.Replace(data, "A task note", "Edited note: keep colon; literal <tag> & text\\nSecond line", 1)
	if rr := req(t, h, "PUT", path, data, true); rr.Code != 204 {
		t.Fatalf("edit task = %d: %s", rr.Code, rr.Body.String())
	}
	if got := p.tasks["id-quoted-description"].Body; got != "Edited note: keep colon; literal <tag> & text\nSecond line" {
		t.Fatalf("edited task description metadata leaked upstream: %q", got)
	}
	get := req(t, h, "GET", path, "", true)
	if get.Code != 200 || !strings.Contains(strings.Join(unfold(get.Body.Bytes()), "\r\n"), `DESCRIPTION:Edited note: keep colon\; literal <tag> & text\nSecond line`) || strings.Contains(get.Body.String(), "ALTREP") {
		t.Fatalf("task notes readback = %d: %s", get.Code, get.Body.String())
	}
}
