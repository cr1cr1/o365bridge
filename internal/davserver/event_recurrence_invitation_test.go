package davserver

import (
	"github.com/cr1cr1/o365bridge/internal/bridge"
	"strings"
	"testing"
	"time"
)

func TestInvitationResponseCannotApplyWithRecurrenceChange(t *testing.T) {
	for _, rule := range []string{"FREQ=HOURLY", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;COUNT=3"} {
		t.Run(rule, func(t *testing.T) {
			p := newFake()
			start := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
			p.events["cloud"] = bridge.Event{ID: "cloud", UID: "repeat", CalendarID: "cal1", Start: start, End: start.Add(time.Hour), Organizer: "other@example.com", Attendees: []bridge.Attendee{{Email: acct, Status: "needs-action"}}, Recurrence: "FREQ=WEEKLY;BYDAY=MO;COUNT=3"}
			data := recurrenceCalendar("DTSTART:20261019T090000Z\r\nDTEND:20261019T100000Z\r\nORGANIZER:mailto:other@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:" + acct + "\r\nRRULE:" + rule + "\r\n")
			rr := req(t, newHandler(t, p), "PUT", "/caldav/"+acct+"/cal1/repeat.ics", data, true)
			if rr.Code != 422 || len(p.responds) != 0 || p.events["cloud"].Attendees[0].Status != "needs-action" {
				t.Fatalf("partial invitation response: status=%d responses=%v event=%+v", rr.Code, p.responds, p.events["cloud"])
			}
		})
	}
}

func TestNonrecurringZonedInvitationStillResponds(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	p := newFake()
	start := time.Date(2026, 10, 19, 9, 0, 0, 0, loc)
	old := bridge.Event{ID: "cloud", UID: "repeat", CalendarID: "cal1", Start: start, End: start.Add(time.Hour), TimeZone: loc.String(), Organizer: "other@example.com", Attendees: []bridge.Attendee{{Email: acct, Status: "needs-action"}}}
	p.events[old.ID] = old
	data := strings.Replace(string(generateEventICalendar(old)), "PARTSTAT=NEEDS-ACTION", "PARTSTAT=ACCEPTED", 1)
	rr := req(t, newHandler(t, p), "PUT", "/caldav/"+acct+"/cal1/repeat.ics", data, true)
	if rr.Code != 204 || len(p.responds) != 1 {
		t.Fatalf("ordinary response blocked: status=%d responses=%v body=%s", rr.Code, p.responds, rr.Body.String())
	}
}
