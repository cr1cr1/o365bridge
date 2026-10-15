package graph_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestGraphEventCategoriesWrite(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		for _, tc := range []struct {
			name       string
			categories []string
			want       []any
		}{
			{"omitted", nil, nil},
			{"clear", []string{}, []any{}},
			{"multiple", []string{"Work", "Team, one", "Semi;colon", `Back\slash`, "项目"}, []any{"Work", "Team, one", "Semi;colon", `Back\slash`, "项目"}},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
					if r.Method != method {
						t.Errorf("method = %s, want %s", r.Method, method)
					}
					if method == http.MethodPost {
						jsonOut(w, http.StatusCreated, `{"id":"ev1","iCalUId":"uid1"}`)
					} else {
						w.WriteHeader(http.StatusNoContent)
					}
				})
				p := f.provider()
				start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
				ev := bridge.Event{ID: "ev1", CalendarID: "cal1", Summary: "Meeting", Start: start, End: start.Add(time.Hour), Categories: tc.categories}
				if method == http.MethodPost {
					created, err := p.CreateEvent(context.Background(), ev)
					if err != nil || created.ID != "ev1" || created.UID != "uid1" {
						t.Fatalf("CreateEvent = %+v, %v", created, err)
					}
					if f.last().Path != "/v1.0/me/calendars/cal1/events" {
						t.Errorf("category create changed path: %s", f.last().Path)
					}
				} else {
					if err := p.UpdateEvent(context.Background(), ev); err != nil {
						t.Fatal(err)
					}
					if f.last().Path != "/v1.0/me/events/ev1" {
						t.Errorf("category update changed path: %s", f.last().Path)
					}
				}
				body := bodyJSON(t, f.last().Body)
				got, exists := body["categories"]
				if exists != (tc.categories != nil) || (exists && !reflect.DeepEqual(got, tc.want)) {
					t.Errorf("native categories = %#v (present %v), want %#v", got, exists, tc.want)
				}
				if body["subject"] != "Meeting" || body["isAllDay"] != false {
					t.Errorf("category write changed other event fields: %+v", body)
				}
			})
		}
	}
}

func TestGraphEventCategoriesRead(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1.0/me/calendars/cal1/events" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		jsonOut(w, http.StatusOK, `{"value":[
			{"id":"ev1","iCalUId":"uid1","categories":["Work","Team, one","Semi;colon","Back\\slash","项目"],"start":{"dateTime":"2026-06-01T10:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-06-01T11:00:00","timeZone":"UTC"}},
			{"id":"ev2","categories":[],"start":{"dateTime":"2026-06-01T12:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-06-01T13:00:00","timeZone":"UTC"}},
			{"id":"ev3","start":{"dateTime":"2026-06-01T14:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-06-01T15:00:00","timeZone":"UTC"}}
		]}`)
	})
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	events, err := f.provider().ListEvents(context.Background(), "cal1", start, start.Add(24*time.Hour))
	if err != nil || len(events) != 3 {
		t.Fatalf("ListEvents = %+v, %v", events, err)
	}
	for i, want := range [][]string{{"Work", "Team, one", "Semi;colon", `Back\slash`, "项目"}, {}, nil} {
		if !reflect.DeepEqual(events[i].Categories, want) {
			t.Errorf("event %d categories = %#v, want %#v", i, events[i].Categories, want)
		}
	}
	if events[0].ID != "ev1" || events[0].UID != "uid1" || events[0].CalendarID != "cal1" || events[0].End.Sub(events[0].Start) != time.Hour {
		t.Errorf("category mapping changed identity or times: %+v", events[0])
	}
}
