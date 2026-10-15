package davserver

import (
	"github.com/cr1cr1/o365bridge/internal/bridge"
	"testing"
	"time"
)

func TestOriginalEventRuleDefaultsDoNotRewriteNativeRepeat(t *testing.T) {
	for _, tc := range []struct {
		name, input, native string
		day                 int
	}{
		{"weekly", "FREQ=WEEKLY;COUNT=4", "FREQ=WEEKLY;BYDAY=MO;WKST=MO;COUNT=4", 19},
		{"monthly", "FREQ=MONTHLY", "FREQ=MONTHLY;BYMONTHDAY=19", 19},
		{"yearly", "FREQ=YEARLY", "FREQ=YEARLY;BYMONTH=10;BYMONTHDAY=19", 19},
		{"ordinal", "FREQ=MONTHLY;BYDAY=-1FR", "FREQ=MONTHLY;BYDAY=FR;BYSETPOS=-1", 30},
		{"weekdays", "FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR", "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;WKST=MO", 19},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := bridge.Event{Start: time.Date(2026, 10, tc.day, 9, 0, 0, 0, time.UTC), TimeZone: "UTC", Recurrence: tc.native}
			next := old
			next.Recurrence, next.RecurrenceSet = tc.input, true
			got, err := prepareEventRepeatUpdate(next, old)
			if err != nil || got.RecurrenceSet {
				t.Fatalf("defaulted rule rewritten: %+v %v", got, err)
			}
			next.Start = next.Start.AddDate(0, 0, 1)
			got, err = prepareEventRepeatUpdate(next, old)
			if err != nil || !got.RecurrenceSet {
				t.Fatalf("changed anchor hidden: %+v %v", got, err)
			}
		})
	}
}

func TestRecurringPUTDoesNotReturnUnnormalizedETag(t *testing.T) {
	p := newFake()
	h := newHandler(t, p)
	data := recurrenceCalendar("DTSTART:20261019T090000Z\r\nDTEND:20261019T100000Z\r\nRRULE:FREQ=WEEKLY;COUNT=4\r\n")
	path := "/caldav/" + acct + "/cal1/repeat.ics"
	created := req(t, h, "PUT", path, data, true)
	if created.Code != 201 || created.Header().Get("ETag") != "" {
		t.Fatalf("create advertised an unnormalized tag: %d %v", created.Code, created.Header())
	}
	get := req(t, h, "GET", path, "", true)
	if get.Code != 200 || get.Header().Get("ETag") == "" {
		t.Fatalf("canonical GET tag missing: %d %v", get.Code, get.Header())
	}
	updated := req(t, h, "PUT", path, data, true)
	if updated.Code != 204 || updated.Header().Get("ETag") != "" {
		t.Fatalf("update advertised unnormalized tag: %d %v", updated.Code, updated.Header())
	}
	cleared := req(t, h, "PUT", path, recurrenceCalendar("DTSTART:20261019T090000Z\r\nDTEND:20261019T100000Z\r\n"), true)
	if cleared.Code != 204 || cleared.Header().Get("ETag") != "" {
		t.Fatalf("clear advertised unnormalized tag: %d %v", cleared.Code, cleared.Header())
	}
}
