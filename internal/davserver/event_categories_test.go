package davserver

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestEventCategoriesCodecRoundTrip(t *testing.T) {
	categories := []string{"Work", "Sales, Europe", "Budget; planning", `Literal\name`, "Two\nlines", strings.Repeat("Ședință", 16)}
	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	ev := bridge.Event{UID: "category-event", Summary: "Meeting", Start: start, End: start.Add(time.Hour), Categories: categories}
	data := generateEventICalendar(ev)
	if !strings.Contains(strings.Join(unfold(data), "\r\n"), "CATEGORIES:Work,Sales\\, Europe,Budget\\; planning,Literal\\\\name,Two\\nlines,") {
		t.Fatalf("category TEXT list not serialized: %s", data)
	}
	if !strings.Contains(string(data), "\r\n ") {
		t.Fatal("long category list not folded")
	}
	events, tasks, err := parseICalendar(data)
	if err != nil || len(tasks) != 0 || len(events) != 1 || !reflect.DeepEqual(events[0].Categories, categories) {
		t.Fatalf("roundtrip events=%+v tasks=%+v err=%v", events, tasks, err)
	}
}

func TestEventCategoriesParseListsAndFullRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name, props string
		want        []string
	}{
		{"repeated and escaped", "CATEGORIES:One,Escaped\\, comma,Two\\\\,Three\r\nCATEGORIES:Space name,Semicolon\\; name\r\n", []string{"One", "Escaped, comma", `Two\`, "Three", "Space name", "Semicolon; name"}},
		{"absent clears in full PUT", "SUMMARY:No categories\r\n", []string{}},
		{"empty clears", "CATEGORIES:\r\n", []string{}},
		{"alarm category is unrelated", "CATEGORIES:Actual\r\nBEGIN:VALARM\r\nCATEGORIES:Alarm only\r\nEND:VALARM\r\nCATEGORIES:After alarm\r\n", []string{"Actual", "After alarm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:category-event\r\n" + tc.props + "END:VEVENT\r\nEND:VCALENDAR\r\n"
			events, _, err := parseICalendar([]byte(data))
			if err != nil || len(events) != 1 || !reflect.DeepEqual(events[0].Categories, tc.want) {
				t.Fatalf("categories=%+v err=%v want=%+v", events, err, tc.want)
			}
		})
	}
}

func TestDAVEventCategoriesConditionalPUTProtectsStaleCache(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         string
		missingUID   bool
		missingEvent bool
		want         int
	}{
		{"old cached tag", "old", false, false, 412},
		{"current tag removes categories", "current", false, false, 204},
		{"weak tag rejected", "weak", false, false, 412},
		{"matching tag in list", "list", false, false, 204},
		{"multiple header lines", "lines", false, false, 204},
		{"wildcard existing", "wildcard", false, false, 204},
		{"no precondition", "none", false, false, 204},
		{"missing UID uses GET identity", "current", true, false, 204},
		{"deleted event current tag", "current", false, true, 412},
		{"deleted event wildcard", "wildcard", false, true, 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFake()
			start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
			ev := bridge.Event{ID: "cloud-id", CalendarID: "cal1", UID: "event-uid", Summary: "Meeting", Start: start, End: start.Add(time.Hour), Categories: []string{"Work"}}
			if tc.missingUID {
				ev.UID = ""
			}
			p.events[ev.ID] = ev
			h := newHandler(t, p)
			uid := ev.UID
			if uid == "" {
				uid = ev.ID
			}
			path := "/caldav/" + acct + "/cal1/" + uid + ".ics"
			get := req(t, h, "GET", path, "", true)
			if get.Code != 200 {
				t.Fatalf("GET=%d %s", get.Code, get.Body.String())
			}
			current := get.Header().Get("ETag")
			old := ev
			old.UID = uid
			old.Categories = nil
			oldTag := etag(ev.ID, generateEventICalendar(old))
			body := strings.Replace(get.Body.String(), "CATEGORIES:Work\r\n", "", 1)
			if tc.missingEvent {
				delete(p.events, ev.ID)
			}
			r := httptest.NewRequest("PUT", path, strings.NewReader(body))
			r.SetBasicAuth(acct, pw)
			switch tc.kind {
			case "old":
				r.Header.Set("If-Match", oldTag)
			case "current":
				r.Header.Set("If-Match", current)
			case "weak":
				r.Header.Set("If-Match", "W/"+current)
			case "list":
				r.Header.Set("If-Match", oldTag+", "+current)
			case "lines":
				r.Header.Add("If-Match", oldTag)
				r.Header.Add("If-Match", current)
			case "wildcard":
				r.Header.Set("If-Match", "*")
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != tc.want {
				t.Fatalf("conditional PUT=%d %s want %d", rr.Code, rr.Body.String(), tc.want)
			}
			if tc.want == 412 {
				if tc.missingEvent {
					if len(p.events) != 0 {
						t.Fatalf("conditional PUT recreated deleted event: %+v", p.events)
					}
				} else if !reflect.DeepEqual(p.events[ev.ID].Categories, []string{"Work"}) {
					t.Fatalf("stale PUT erased categories: %+v", p.events)
				}
				if p.seq != 0 || len(p.responds) != 0 {
					t.Fatalf("failed precondition wrote upstream: %+v", p)
				}
			} else if len(p.events[ev.ID].Categories) != 0 {
				t.Fatalf("fresh full PUT did not clear categories: %+v", p.events)
			}
		})
	}
}

func TestDAVStaleCategorizedInvitationPUTDoesNotRespond(t *testing.T) {
	p := newFake()
	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	p.events["inv-cloud"] = bridge.Event{ID: "inv-cloud", CalendarID: "cal1", UID: "inv-uid", Summary: "Invite", Start: start, End: start.Add(time.Hour), Categories: []string{"Work"}, Organizer: "boss@example.com", Attendees: []bridge.Attendee{{Email: acct, Status: "needs-action"}}}
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/cal1/inv-uid.ics"
	get := req(t, h, "GET", path, "", true)
	body := strings.ReplaceAll(get.Body.String(), "PARTSTAT=NEEDS-ACTION", "PARTSTAT=ACCEPTED")
	r := httptest.NewRequest("PUT", path, strings.NewReader(body))
	r.SetBasicAuth(acct, pw)
	r.Header.Set("If-Match", `"stale"`)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != 412 || len(p.responds) != 0 || p.events["inv-cloud"].Attendees[0].Status != "needs-action" {
		t.Fatalf("stale invitation PUT wrote upstream: status=%d responds=%+v events=%+v", rr.Code, p.responds, p.events)
	}
}

func TestDAVEventCategoriesAffectETagAndKeepSibling(t *testing.T) {
	p := newFake()
	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{"one", "two"} {
		p.events[id] = bridge.Event{ID: id, CalendarID: "cal1", UID: id, Summary: "Same meeting", Start: start, End: start.Add(time.Hour), Categories: []string{"Work", "Sales, Europe"}}
	}
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/cal1/one.ics"
	first := req(t, h, "GET", path, "", true)
	if first.Code != 200 || !strings.Contains(first.Body.String(), "CATEGORIES:Work,Sales\\, Europe") {
		t.Fatalf("GET=%d %s", first.Code, first.Body.String())
	}
	updated := strings.Replace(first.Body.String(), "CATEGORIES:Work,Sales\\, Europe", "CATEGORIES:Personal", 1)
	if rr := req(t, h, "PUT", path, updated, true); rr.Code != 204 {
		t.Fatalf("PUT=%d %s", rr.Code, rr.Body.String())
	}
	second := req(t, h, "GET", path, "", true)
	if second.Code != 200 || second.Header().Get("ETag") == first.Header().Get("ETag") || !strings.Contains(second.Body.String(), "CATEGORIES:Personal") {
		t.Fatalf("category update did not change data/ETag: %d %s", second.Code, second.Body.String())
	}
	if !reflect.DeepEqual(p.events["two"].Categories, []string{"Work", "Sales, Europe"}) {
		t.Fatalf("category edit changed sibling: %+v", p.events)
	}
	// Thunderbird removes the final category by omitting the property in a full PUT.
	cleared := strings.Replace(second.Body.String(), "CATEGORIES:Personal\r\n", "", 1)
	if rr := req(t, h, "PUT", path, cleared, true); rr.Code != 204 {
		t.Fatalf("clear=%d %s", rr.Code, rr.Body.String())
	}
	if p.events["one"].Categories == nil || len(p.events["one"].Categories) != 0 {
		t.Fatalf("omitted category in full PUT not authoritative: %+v", p.events["one"])
	}
	if rr := req(t, h, "GET", path, "", true); rr.Code != 200 || strings.Contains(rr.Body.String(), "CATEGORIES:") {
		t.Fatalf("cleared readback=%d %s", rr.Code, rr.Body.String())
	}
}
