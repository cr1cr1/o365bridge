package graph_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func recurringEvent() bridge.Event {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	return bridge.Event{ID: "client-id", CalendarID: "cal", UID: "client-uid", Summary: "series", Start: start, End: start.Add(time.Hour), Recurrence: "FREQ=DAILY"}
}

func TestEventRecurrenceCreateDaily(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1.0/me/calendars/cal/events" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		m := bodyJSON(t, b)
		rec, ok := m["recurrence"].(map[string]any)
		if !ok {
			t.Errorf("missing native recurrence: %s", b)
		} else {
			pat := rec["pattern"].(map[string]any)
			rng := rec["range"].(map[string]any)
			if pat["type"] != "daily" || pat["interval"] != float64(1) || rng["type"] != "noEnd" || rng["startDate"] != "2026-03-02" {
				t.Errorf("recurrence=%v", rec)
			}
		}
		jsonOut(w, 201, `{"id":"server-id","iCalUId":"server-uid"}`)
	})
	ev := recurringEvent()
	got, err := f.provider().CreateEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "server-id" || got.UID != "server-uid" || got.Recurrence != ev.Recurrence || !got.Start.Equal(ev.Start) || got.Summary != ev.Summary {
		t.Errorf("sparse create=%+v", got)
	}
}

// These literal native fixtures catch lost selectors, wrong defaults, and accidental task date semantics.
func TestEventRecurrenceNativePatterns(t *testing.T) {
	cases := []struct{ name, rule, pattern, rng string }{
		{"daily", "FREQ=DAILY", `{"type":"daily","interval":1}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"UTC"}`},
		{"weekly-default", "FREQ=WEEKLY", `{"type":"weekly","interval":1,"daysOfWeek":["monday"],"firstDayOfWeek":"monday"}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"UTC"}`},
		{"weekly-days", "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE,FR;WKST=SU;COUNT=5", `{"type":"weekly","interval":2,"daysOfWeek":["monday","wednesday","friday"],"firstDayOfWeek":"sunday"}`, `{"type":"numbered","startDate":"2026-03-02","numberOfOccurrences":5,"recurrenceTimeZone":"UTC"}`},
		{"monthly-absolute", "FREQ=MONTHLY;BYMONTHDAY=15", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":15}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"UTC"}`},
		{"monthly-default", "FREQ=MONTHLY", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":2}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"UTC"}`},
		{"monthly-relative", "FREQ=MONTHLY;BYDAY=TU;BYSETPOS=-1;COUNT=6", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["tuesday"],"index":"last"}`, `{"type":"numbered","startDate":"2026-03-02","numberOfOccurrences":6,"recurrenceTimeZone":"UTC"}`},
		{"monthly-ordinal", "FREQ=MONTHLY;BYDAY=2TH", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["thursday"],"index":"second"}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"UTC"}`},
		{"yearly-absolute", "FREQ=YEARLY;INTERVAL=2;BYMONTH=6;BYMONTHDAY=28", `{"type":"absoluteYearly","interval":2,"month":6,"dayOfMonth":28}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"UTC"}`},
		{"yearly-default", "FREQ=YEARLY", `{"type":"absoluteYearly","interval":1,"month":3,"dayOfMonth":2}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"UTC"}`},
		{"yearly-relative", "FREQ=YEARLY;BYMONTH=11;BYDAY=MO;BYSETPOS=1", `{"type":"relativeYearly","interval":1,"month":11,"daysOfWeek":["monday"],"index":"first"}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"UTC"}`},
		{"timed-end", "FREQ=DAILY;UNTIL=20260310T090000Z", `{"type":"daily","interval":1}`, `{"type":"endDate","startDate":"2026-03-02","endDate":"2026-03-10","recurrenceTimeZone":"UTC"}`},
		{"timed-before-occurrence", "FREQ=DAILY;UNTIL=20260310T085959Z", `{"type":"daily","interval":1}`, `{"type":"endDate","startDate":"2026-03-02","endDate":"2026-03-09","recurrenceTimeZone":"UTC"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				got := bodyJSON(t, b)["recurrence"]
				want := bodyJSON(t, []byte(`{"pattern":`+tc.pattern+`,"range":`+tc.rng+`}`))
				if !reflect.DeepEqual(got, want) {
					t.Errorf("native recurrence=%v want=%v", got, want)
				}
				jsonOut(w, 201, `{"id":"native"}`)
			})
			ev := recurringEvent()
			ev.Recurrence = tc.rule
			if _, err := f.provider().CreateEvent(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEventRecurrenceLocalDatesAndUntil(t *testing.T) {
	cases := []struct {
		name, zone, start, until, wantStart, wantEnd, wantZone string
		allDay                                                 bool
	}{
		{"east-local-next-date", "Asia/Tokyo", "2026-03-01T16:30:00Z", "20260303T163000Z", "2026-03-02", "2026-03-04", "Tokyo Standard Time", false},
		{"west-local-previous-date", "America/Los_Angeles", "2026-03-02T07:30:00Z", "20260305T070000Z", "2026-03-01", "2026-03-03", "Pacific Standard Time", false},
		{"dst-cutoff-before-start", "America/New_York", "2026-03-02T14:00:00Z", "20260310T125959Z", "2026-03-02", "2026-03-09", "Eastern Standard Time", false},
		{"dst-cutoff-at-start", "America/New_York", "2026-03-02T14:00:00Z", "20260310T130000Z", "2026-03-02", "2026-03-10", "Eastern Standard Time", false},
		{"all-day-date", "Asia/Tokyo", "2026-03-01T15:00:00Z", "20260310", "2026-03-02", "2026-03-10", "Tokyo Standard Time", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				rec := bodyJSON(t, b)["recurrence"].(map[string]any)
				rng := rec["range"].(map[string]any)
				if rng["startDate"] != tc.wantStart || rng["endDate"] != tc.wantEnd || rng["recurrenceTimeZone"] != tc.wantZone {
					t.Errorf("range=%v", rng)
				}
				jsonOut(w, 201, `{"id":"created","iCalUId":"native-uid"}`)
			})
			ev := recurringEvent()
			ev.Start, _ = time.Parse(time.RFC3339, tc.start)
			ev.End = ev.Start.Add(time.Hour)
			ev.TimeZone = tc.zone
			ev.AllDay = tc.allDay
			ev.Recurrence = "FREQ=DAILY;UNTIL=" + tc.until
			ev.Description = "details"
			ev.Location = "room"
			ev.Categories = []string{"tag"}
			ev.Attendees = []bridge.Attendee{{Email: "a@example.com"}}
			m := 5
			ev.ReminderMin = &m
			got, err := f.provider().CreateEvent(context.Background(), ev)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != "created" || got.UID != "native-uid" || got.TimeZone != tc.zone || got.Recurrence != ev.Recurrence || got.AllDay != tc.allDay || got.Description != "details" || got.Location != "room" || !reflect.DeepEqual(got.Categories, ev.Categories) || !reflect.DeepEqual(got.Attendees, ev.Attendees) || got.ReminderMin == nil || *got.ReminderMin != 5 {
				t.Errorf("sparse=%+v", got)
			}
		})
	}
}

func TestEventRecurrencePatchIntent(t *testing.T) {
	for _, tc := range []struct {
		name, rule                string
		set, unsupported, present bool
	}{
		{"partial-nonempty", "FREQ=DAILY", false, false, false},
		{"partial-empty", "", false, false, false},
		{"partial-unsupported", "not an rrule", false, true, false},
		{"edit", "FREQ=WEEKLY;BYDAY=MO,WE", true, false, true},
		{"clear", "", true, false, true},
		{"clear-native-unsupported", "", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				if r.Method != http.MethodPatch || !strings.HasSuffix(r.URL.Path, "/events/client-id") {
					t.Errorf("request=%s %s", r.Method, r.URL.Path)
				}
				v, ok := bodyJSON(t, b)["recurrence"]
				if ok != tc.present {
					t.Errorf("recurrence presence=%v want=%v body=%s", ok, tc.present, b)
				}
				if tc.present && tc.rule == "" && v != nil {
					t.Errorf("clear=%v want null", v)
				}
				if tc.present && tc.rule != "" && v.(map[string]any)["pattern"].(map[string]any)["type"] != "weekly" {
					t.Errorf("edit=%v", v)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			ev := recurringEvent()
			ev.Recurrence = tc.rule
			ev.RecurrenceSet = tc.set
			ev.RecurrenceUnsupported = tc.unsupported
			if err := f.provider().UpdateEvent(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEventRecurrenceRejectsBeforeNetwork(t *testing.T) {
	rules := []string{
		"FREQ=HOURLY", "FREQ=DAILY;INTERVAL=0", "FREQ=DAILY;INTERVAL=2147483648", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;COUNT=2;COUNT=3",
		"FREQ=DAILY;COUNT=2;UNTIL=20260310T090000Z", "FREQ=DAILY;BYHOUR=9", "FREQ=DAILY;BYDAY=MO", "FREQ=DAILY;UNTIL=20260310", "FREQ=DAILY;UNTIL=20260301T090000Z",
		"FREQ=WEEKLY;BYDAY=MO,MO", "FREQ=WEEKLY;BYDAY=1MO", "FREQ=WEEKLY;WKST=XX", "FREQ=WEEKLY;BYMONTH=3",
		"FREQ=MONTHLY;BYDAY=MO,WE;BYSETPOS=1", "FREQ=MONTHLY;BYDAY=MO;BYSETPOS=5", "FREQ=MONTHLY;BYDAY=2MO;BYSETPOS=2", "FREQ=MONTHLY;BYDAY=MO;BYMONTHDAY=2",
		"FREQ=MONTHLY;BYMONTHDAY=-1", "FREQ=MONTHLY;BYMONTHDAY=29", "FREQ=MONTHLY;BYMONTHDAY=31", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29", "FREQ=YEARLY;BYMONTH=4;BYMONTHDAY=31",
		"FREQ=YEARLY;BYDAY=MO;BYSETPOS=1", "FREQ=YEARLY;BYMONTH=3,4;BYMONTHDAY=2", "FREQ=DAILY;FREQ=WEEKLY", "FREQ=DAILY;", "RRULE:FREQ=DAILY\nEXDATE:20260303T090000Z",
	}
	for _, rule := range rules {
		t.Run(rule, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				t.Errorf("unexpected write: %s", b)
				jsonOut(w, 201, `{"id":"bad"}`)
			})
			ev := recurringEvent()
			ev.Recurrence = rule
			ev.RecurrenceSet = true
			if _, err := f.provider().CreateEvent(context.Background(), ev); err == nil {
				t.Error("create accepted unsupported recurrence")
			}
			if err := f.provider().UpdateEvent(context.Background(), ev); err == nil {
				t.Error("patch accepted unsupported recurrence")
			}
			if len(f.reqs) != 0 {
				t.Errorf("writes=%d", len(f.reqs))
			}
		})
	}
	for _, name := range []string{"unknown-zone", "missing-start", "unsupported-native", "all-day-timed-until", "default-monthday-clamp"} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				t.Errorf("unexpected write %s", b)
				jsonOut(w, 201, `{"id":"bad"}`)
			})
			ev := recurringEvent()
			switch name {
			case "unknown-zone":
				ev.TimeZone = "unknown/zone"
			case "missing-start":
				ev.Start = time.Time{}
			case "unsupported-native":
				ev.RecurrenceUnsupported = true
			case "all-day-timed-until":
				ev.AllDay = true
				ev.Recurrence = "FREQ=DAILY;UNTIL=20260310T090000Z"
			case "default-monthday-clamp":
				ev.Start = time.Date(2026, 1, 31, 9, 0, 0, 0, time.UTC)
				ev.Recurrence = "FREQ=MONTHLY"
			}
			if _, err := f.provider().CreateEvent(context.Background(), ev); err == nil {
				t.Error("invalid event accepted")
			}
			if len(f.reqs) != 0 {
				t.Errorf("writes=%d", len(f.reqs))
			}
		})
	}
}

func TestEventRecurrenceNativeRead(t *testing.T) {
	cases := []struct {
		name, pattern, rng, rule string
		allDay, unsupported      bool
	}{
		{"daily", `{"type":"daily","interval":1}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "FREQ=DAILY;INTERVAL=1", false, false},
		{"weekly", `{"type":"weekly","interval":2,"daysOfWeek":["monday","friday"],"firstDayOfWeek":"sunday"}`, `{"type":"numbered","startDate":"2026-03-02","numberOfOccurrences":4}`, "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;WKST=SU;COUNT=4", false, false},
		{"weekly-native-default", `{"type":"weekly","interval":1,"daysOfWeek":["monday"]}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "FREQ=WEEKLY;INTERVAL=1;BYDAY=MO;WKST=SU", false, false},
		{"monthly", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":15}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "FREQ=MONTHLY;INTERVAL=1;BYMONTHDAY=15", false, false},
		{"relative-monthly", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["thursday"],"index":"last"}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "FREQ=MONTHLY;INTERVAL=1;BYDAY=TH;BYSETPOS=-1", false, false},
		{"yearly", `{"type":"absoluteYearly","interval":1,"month":6,"dayOfMonth":28}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "FREQ=YEARLY;INTERVAL=1;BYMONTH=6;BYMONTHDAY=28", false, false},
		{"relative-yearly", `{"type":"relativeYearly","interval":1,"month":11,"daysOfWeek":["monday"],"index":"second"}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "FREQ=YEARLY;INTERVAL=1;BYMONTH=11;BYDAY=MO;BYSETPOS=2", false, false},
		{"timed-end-dst", `{"type":"daily","interval":1}`, `{"type":"endDate","startDate":"2026-03-02","endDate":"2026-03-10","recurrenceTimeZone":"Eastern Standard Time"}`, "FREQ=DAILY;INTERVAL=1;UNTIL=20260310T130000Z", false, false},
		{"all-day-end", `{"type":"daily","interval":1}`, `{"type":"endDate","startDate":"2026-03-02","endDate":"2026-03-10"}`, "FREQ=DAILY;INTERVAL=1;UNTIL=20260310", true, false},
		{"regeneration", `{"type":"dailyRegeneration","interval":1}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "", false, true},
		{"relative-multiple", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["monday","tuesday"],"index":"first"}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "", false, true},
		{"invalid-interval", `{"type":"daily","interval":0}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "", false, true},
		{"unknown-range", `{"type":"daily","interval":1}`, `{"type":"forever","startDate":"2026-03-02"}`, "", false, true},
		{"bad-count", `{"type":"daily","interval":1}`, `{"type":"numbered","startDate":"2026-03-02","numberOfOccurrences":0}`, "", false, true},
		{"different-anchor", `{"type":"daily","interval":1}`, `{"type":"noEnd","startDate":"2026-03-01"}`, "", false, true},
		{"bad-zone", `{"type":"daily","interval":1}`, `{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"unknown/zone"}`, "", false, true},
		{"ignored-selector", `{"type":"daily","interval":1,"dayOfMonth":5}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "", false, true},
		{"monthly-clamp", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":31}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "", false, true},
		{"yearly-leap-clamp", `{"type":"absoluteYearly","interval":1,"month":2,"dayOfMonth":29}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				allDay := "false"
				if tc.allDay {
					allDay = "true"
				}
				jsonOut(w, 200, `{"value":[{"id":"native","iCalUId":"real-uid","start":{"dateTime":"2026-03-02T09:00:00","timeZone":"Eastern Standard Time"},"end":{"dateTime":"2026-03-02T10:00:00","timeZone":"Eastern Standard Time"},"isAllDay":`+allDay+`,"recurrence":{"pattern":`+tc.pattern+`,"range":`+tc.rng+`}}]}`)
			})
			got, err := f.provider().ListEvents(context.Background(), "cal", time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("events=%v", got)
			}
			if got[0].Recurrence != tc.rule || got[0].RecurrenceUnsupported != tc.unsupported || got[0].RecurrenceSet || got[0].UID != "real-uid" {
				t.Errorf("native read=%+v want rule=%q unsupported=%v", got[0], tc.rule, tc.unsupported)
			}
		})
	}
}

func TestEventRecurrenceCreateMissingID(t *testing.T) {
	for _, response := range []string{`{}`, `{"id":""}`, `{"id":"   ","iCalUId":"uid"}`, `null`} {
		t.Run(response, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) { jsonOut(w, 201, response) })
			if got, err := f.provider().CreateEvent(context.Background(), recurringEvent()); err == nil || got.ID != "" {
				t.Errorf("false success: event=%+v err=%v", got, err)
			}
		})
	}
}

func TestEventRecurrenceCreateRealReadbackWins(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jsonOut(w, 201, `{"id":"server","iCalUId":"server-uid","subject":"normalized","body":{"content":""},"start":{"dateTime":"2026-03-02T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-03-02T10:00:00","timeZone":"UTC"},"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"numbered","startDate":"2026-03-02","numberOfOccurrences":3}}}`)
	})
	ev := recurringEvent()
	ev.Description = "requested"
	got, err := f.provider().CreateEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "server" || got.UID != "server-uid" || got.Summary != "normalized" || got.Description != "" || got.Recurrence != "FREQ=DAILY;INTERVAL=1;COUNT=3" {
		t.Errorf("response overwritten=%+v", got)
	}
}

func TestEventRecurrenceCreateSparseNativeReadback(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		rec := bodyJSON(t, b)["recurrence"]
		response, _ := json.Marshal(map[string]any{"id": "server", "iCalUId": "server-uid", "recurrence": rec})
		jsonOut(w, 201, string(response))
	})
	ev := recurringEvent()
	ev.TimeZone = "America/New_York"
	ev.Start = time.Date(2026, 3, 2, 14, 0, 0, 0, time.UTC)
	ev.End = ev.Start.Add(time.Hour)
	got, err := f.provider().CreateEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got.RecurrenceUnsupported || got.Recurrence != "FREQ=DAILY;INTERVAL=1" || got.TimeZone != "America/New_York" || got.ID != "server" || got.UID != "server-uid" || !got.Start.Equal(ev.Start) {
		t.Errorf("sparse native=%+v", got)
	}
}
