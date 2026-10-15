package davserver

import (
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func recurrenceCalendar(props string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:repeat\r\n" + props + "END:VEVENT\r\nEND:VCALENDAR\r\n"
}

func TestEventRecurrenceFullRepresentationAndExceptions(t *testing.T) {
	for _, rule := range []string{"", "RRULE:FREQ=WEEKLY;BYDAY=MO,WE\r\n"} {
		events, _, err := parseICalendar([]byte(recurrenceCalendar("DTSTART:20261019T090000Z\r\nDTEND:20261019T100000Z\r\n" + rule)))
		if err != nil || len(events) != 1 || !events[0].RecurrenceSet {
			t.Fatalf("full repeat presence lost: %+v %v", events, err)
		}
	}
	for _, prop := range []string{"RDATE:20261020T090000Z", "EXDATE:20261026T090000Z", "RECURRENCE-ID:20261019T090000Z", "RRULE:", "RRULE:FREQ=DAILY\r\nRRULE:FREQ=WEEKLY"} {
		if _, _, err := parseICalendar([]byte(recurrenceCalendar("DTSTART:20261019T090000Z\r\n" + prop + "\r\n"))); err == nil {
			t.Fatalf("unsupported recurrence accepted: %s", prop)
		}
	}
}

func TestRecurringEventKeepsNativeWallTimeAndZone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 21, 9, 0, 0, 0, loc)
	ev := bridge.Event{UID: "repeat", Start: start, End: start.Add(time.Hour), TimeZone: loc.String(), Recurrence: "FREQ=WEEKLY;BYDAY=WE;COUNT=3"}
	data := generateEventICalendar(ev)
	if !strings.Contains(string(data), "DTSTART;TZID=Europe/Bucharest:20261021T090000") || !strings.Contains(string(data), "DTEND;TZID=Europe/Bucharest:20261021T100000") {
		t.Fatalf("recurring local clock lost: %s", data)
	}
	got, _, err := parseICalendar(data)
	if err != nil || len(got) != 1 || !got[0].Start.Equal(start) || got[0].TimeZone != ev.TimeZone || got[0].Recurrence != ev.Recurrence {
		t.Fatalf("repeat roundtrip: %+v %v", got, err)
	}
}

func TestEventRecurrencePUTMutationAndPreservation(t *testing.T) {
	for _, tc := range []struct {
		name, rule string
		set        bool
	}{
		{"reordered unchanged", "BYDAY=WE,MO;FREQ=WEEKLY", false},
		{"changed", "FREQ=WEEKLY;BYDAY=TU", true},
		{"cleared", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFake()
			start := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
			p.events["cloud"] = bridge.Event{ID: "cloud", UID: "repeat", CalendarID: "cal1", Start: start, End: start.Add(time.Hour), TimeZone: "UTC", Recurrence: "FREQ=WEEKLY;INTERVAL=1;BYDAY=MO,WE;WKST=MO"}
			data := recurrenceCalendar("DTSTART:20261019T090000Z\r\nDTEND:20261019T100000Z\r\nSUMMARY:Renamed\r\n")
			if tc.rule != "" {
				data = strings.Replace(data, "END:VEVENT", "RRULE:"+tc.rule+"\r\nEND:VEVENT", 1)
			}
			rr := req(t, newHandler(t, p), "PUT", "/caldav/"+acct+"/cal1/repeat.ics", data, true)
			if rr.Code != 204 {
				t.Fatalf("PUT=%d %s", rr.Code, rr.Body.String())
			}
			got := p.events["cloud"]
			if got.RecurrenceSet != tc.set {
				t.Fatalf("repeat mutation=%t, want %t: %+v", got.RecurrenceSet, tc.set, got)
			}
			if tc.rule == "" && got.Recurrence != "" {
				t.Fatalf("rule not cleared: %+v", got)
			}
		})
	}
}

func TestUnsupportedNativeEventRepeatPreservedAndProtected(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		want         int
	}{
		{"title only", "", 204},
		{"replace", "RRULE:FREQ=DAILY\r\n", 422},
		{"move", "", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFake()
			start := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
			old := bridge.Event{ID: "cloud", UID: "repeat", CalendarID: "cal1", Start: start, End: start.Add(time.Hour), RecurrenceUnsupported: true}
			p.events[old.ID] = old
			data := recurrenceCalendar("DTSTART:20261019T090000Z\r\nDTEND:20261019T100000Z\r\nSUMMARY:Renamed\r\n" + tc.change)
			if tc.name == "move" {
				data = strings.ReplaceAll(data, "20261019", "20261020")
			}
			rr := req(t, newHandler(t, p), "PUT", "/caldav/"+acct+"/cal1/repeat.ics", data, true)
			if rr.Code != tc.want {
				t.Fatalf("PUT=%d %s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
			if tc.want == 204 {
				got := p.events[old.ID]
				if got.RecurrenceSet || !got.RecurrenceUnsupported {
					t.Fatalf("unsupported repeat overwritten: %+v", got)
				}
			}
		})
	}
}

func TestRecurringAllDayDateSurvivesPositiveAndNegativeZones(t *testing.T) {
	for _, zone := range []string{"Asia/Tokyo", "America/Los_Angeles"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 10, 19, 0, 0, 0, 0, loc)
			old := bridge.Event{ID: "cloud", UID: "repeat", CalendarID: "cal1", Start: start, End: start.AddDate(0, 0, 1), AllDay: true, TimeZone: zone, Recurrence: "FREQ=DAILY;COUNT=3"}
			data := generateEventICalendar(old)
			if !strings.Contains(string(data), "DTSTART;VALUE=DATE:20261019") || !strings.Contains(string(data), "DTEND;VALUE=DATE:20261020") {
				t.Fatalf("all-day date shifted: %s", data)
			}
			p := newFake()
			p.events[old.ID] = old
			rr := req(t, newHandler(t, p), "PUT", "/caldav/"+acct+"/cal1/repeat.ics", string(data), true)
			if rr.Code != 204 {
				t.Fatalf("PUT=%d %s", rr.Code, rr.Body.String())
			}
			got := p.events[old.ID]
			if !got.Start.Equal(start) || got.TimeZone != zone || got.RecurrenceSet {
				t.Fatalf("all-day anchor changed: %+v", got)
			}
		})
	}
}
