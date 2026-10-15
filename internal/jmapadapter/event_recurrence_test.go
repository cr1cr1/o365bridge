package jmapadapter

import (
	"github.com/cr1cr1/o365bridge/jmap/calendars"
	"testing"
)

func TestJMAPRejectsRecurrenceFieldsItCannotTranslate(t *testing.T) {
	for _, tc := range []calendars.CalendarEvent{
		{RecurrenceRule: &calendars.RecurrenceRule{Frequency: "monthly", ByMonthDay: []int{1, 15}}},
		{RecurrenceRule: &calendars.RecurrenceRule{Frequency: "weekly", FirstDayOfWeek: "su"}},
		{RecurrenceOverrides: map[string]*calendars.RecurrenceOverride{"2026-10-22T09:00:00": {}}},
		{ExcludedRecurrenceRules: []calendars.RecurrenceRule{{Frequency: "daily"}}},
	} {
		tc.Start, tc.Duration = "2026-10-19T09:00:00", "PT1H"
		if _, err := eventFromJMAP(tc); err == nil || err.Type != "invalidProperties" {
			t.Fatalf("lossy recurrence accepted: %+v %v", tc, err)
		}
	}
	ev, err := eventFromJMAP(calendars.CalendarEvent{Start: "2026-10-19T09:00:00", Duration: "PT1H", RecurrenceRule: &calendars.RecurrenceRule{Frequency: "daily"}})
	if err != nil || ev.Recurrence != "FREQ=DAILY" || ev.RecurrenceSet {
		t.Fatalf("basic create/partial-update semantics changed: %+v %v", ev, err)
	}
}
