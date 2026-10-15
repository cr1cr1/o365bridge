package graph_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestEventRecurrenceUnsupportedZonePreservesOrdinaryEdit(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.Method == http.MethodGet {
			jsonOut(w, 200, `{"value":[{"id":"native","start":{"dateTime":"2026-03-02T09:00:00","timeZone":"Eastern Standard Time"},"end":{"dateTime":"2026-03-02T10:00:00","timeZone":"Eastern Standard Time"},"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"noEnd","startDate":"2026-03-02","recurrenceTimeZone":"unknown/zone"}}}]}`)
			return
		}
		body := bodyJSON(t, b)
		if _, ok := body["recurrence"]; ok {
			t.Errorf("ordinary edit rewrites unsupported rule: %s", b)
		}
		start := body["start"].(map[string]any)
		if start["timeZone"] != "Eastern Standard Time" || start["dateTime"] != "2026-03-02T09:00:00.0000000" {
			t.Errorf("ordinary edit lost event zone: %s", b)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	p := f.provider()
	events, err := p.ListEvents(context.Background(), "cal", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].RecurrenceUnsupported {
		t.Fatalf("events=%+v", events)
	}
	ev := events[0]
	ev.Summary = "changed"
	if err := p.UpdateEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func TestEventRecurrenceSparseUTCResponseRetainsRequestedZone(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jsonOut(w, 201, `{"id":"server","iCalUId":"real-uid","start":{"dateTime":"2026-03-02T14:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-03-02T15:00:00","timeZone":"UTC"}}`)
	})
	ev := recurringEvent()
	ev.TimeZone = "America/New_York"
	ev.Start = time.Date(2026, 3, 2, 14, 0, 0, 0, time.UTC)
	ev.End = ev.Start.Add(time.Hour)
	got, err := f.provider().CreateEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "server" || got.UID != "real-uid" || got.TimeZone != ev.TimeZone || got.Recurrence != ev.Recurrence || !got.Start.Equal(ev.Start) {
		t.Errorf("sparse UTC response=%+v", got)
	}
}
