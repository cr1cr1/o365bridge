package owa

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/davserver"
	"github.com/rs/zerolog"
)

func calendarReadbackItem(id, uid string) map[string]any {
	return map[string]any{
		"__type": "CalendarItem:#Exchange", "ItemId": map[string]any{"Id": id},
		"UID": uid, "Subject": "Readback meeting",
		"Start": "2026-10-15T10:00:00", "End": "2026-10-15T11:00:00",
		"StartTimeZone": map[string]any{"Id": "GTB Standard Time"},
		"Body":          map[string]any{"Value": "Meeting details"},
		"RequiredAttendees": []any{map[string]any{
			"Mailbox": map[string]any{"EmailAddress": "guest@example.com"}, "ResponseType": "Accept",
		}},
		"Recurrence": map[string]any{
			"__type":            "RecurrenceType:#Exchange",
			"RecurrencePattern": map[string]any{"__type": "DailyRecurrence:#Exchange", "Interval": 2},
			"RecurrenceRange":   map[string]any{"__type": "NumberedRecurrence:#Exchange", "StartDate": "2026-10-15", "NumberOfOccurrences": 3},
		},
		"ReminderIsSet": true, "ReminderMinutesBeforeStart": 15,
	}
}

func TestListEventsHydratesSparseFindItemResults(t *testing.T) {
	f, p := newFake(t, func(action string, _ map[string]any) any {
		switch action {
		case "FindItem":
			return okBody(map[string]any{"RootFolder": map[string]any{
				"TotalItemsInView": 2, "Items": []any{
					map[string]any{"ItemId": map[string]any{"Id": "ev1"}},
					map[string]any{"ItemId": map[string]any{"Id": "ev2"}},
				},
			}})
		case "GetItem":
			// Exchange can return one response message for each requested item.
			return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{
				map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Items": []any{calendarReadbackItem("ev1", "uid1")}},
				map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Items": []any{calendarReadbackItem("ev2", "uid2")}},
			}}})
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	from := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	events, err := p.ListEvents(context.Background(), "calendar", from, from.Add(24*time.Hour))
	if err != nil || len(events) != 2 {
		t.Fatalf("ListEvents = %+v, %v; want both hydrated events", events, err)
	}
	for i, ev := range events {
		if ev.ID != fmt.Sprintf("ev%d", i+1) || ev.UID != fmt.Sprintf("uid%d", i+1) || ev.CalendarID != "calendar" {
			t.Fatalf("identity not retained: %+v", ev)
		}
		if ev.Start.Hour() != 10 || ev.Start.UTC().Hour() != 7 || ev.TimeZone != "Europe/Bucharest" || ev.End.Sub(ev.Start) != time.Hour {
			t.Fatalf("event time not hydrated: %+v", ev)
		}
		if ev.Description != "Meeting details" || len(ev.Attendees) != 1 || ev.Attendees[0].Status != "accepted" || ev.Recurrence != "FREQ=DAILY;INTERVAL=2;COUNT=3" || ev.ReminderMin == nil || *ev.ReminderMin != 15 {
			t.Fatalf("event fields not hydrated: %+v", ev)
		}
	}
	if _, exists := asMap(f.last("FindItem").Body["Body"])["ShapeName"]; exists {
		t.Fatal("calendar FindItem must not request the mail-list shape")
	}
	get := f.last("GetItem").Body
	if len(arr(get, "Body", "ItemIds")) != 2 {
		t.Fatalf("GetItem should batch event IDs: %+v", get)
	}
	properties := map[string]bool{}
	for _, property := range arr(get, "Body", "ItemShape", "AdditionalProperties") {
		properties[str(property, "FieldURI")] = true
	}
	for _, field := range []string{"calendar:UID", "calendar:Start", "calendar:End", "calendar:StartTimeZone", "calendar:EndTimeZone", "calendar:Recurrence", "calendar:RequiredAttendees", "calendar:OptionalAttendees", "item:Body", "item:ReminderIsSet", "item:ReminderMinutesBeforeStart"} {
		if !properties[field] {
			t.Errorf("GetItem did not request %s", field)
		}
	}
	// Filter hydrated times rather than the sparse discovery rows.
	events, err = p.ListEvents(context.Background(), "calendar", from.Add(24*time.Hour), from.Add(48*time.Hour))
	if err != nil || len(events) != 0 {
		t.Fatalf("out-of-range ListEvents = %+v, %v", events, err)
	}
}

func TestListEventsDoesNotHideIncompleteReadback(t *testing.T) {
	for _, test := range []struct {
		name     string
		response any
	}{
		{"missing item", itemResp(calendarReadbackItem("ev1", "uid1"))},
		{"missing dates", itemResp(calendarReadbackItem("ev1", "uid1"), map[string]any{"ItemId": map[string]any{"Id": "ev2"}, "UID": "uid2"})},
		{"item failure", okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{
			map[string]any{"ResponseClass": "Success", "Items": []any{calendarReadbackItem("ev1", "uid1")}},
			map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorServerBusy", "MessageText": "Try again later"},
		}}})},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, p := newFake(t, func(action string, _ map[string]any) any {
				if action == "FindItem" {
					return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{
						map[string]any{"ItemId": map[string]any{"Id": "ev1"}},
						map[string]any{"ItemId": map[string]any{"Id": "ev2"}},
					}}})
				}
				return test.response
			})
			from := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
			events, err := p.ListEvents(context.Background(), "calendar", from, from.Add(24*time.Hour))
			if err == nil || len(events) != 0 {
				t.Fatalf("incomplete readback must fail, not appear as removed events: %+v, %v", events, err)
			}
		})
	}
}

// A valid persisted fake session keeps this regression independent of browser login.
type calendarReadbackLogin struct{ bridge.LoginManager }

func TestCalDAVCreatedOWAEventRemainsReadable(t *testing.T) {
	testCalDAVCreatedOWAEventRemainsReadable(t, false)
}

func TestCalDAVRecurringOWAEventRemainsReadable(t *testing.T) {
	testCalDAVCreatedOWAEventRemainsReadable(t, true)
}

func testCalDAVCreatedOWAEventRemainsReadable(t *testing.T, recurring bool) {
	t.Helper()
	const uid = "client-event-uid"
	recurrenceUpdates := 0
	var stored map[string]any
	_, upstream := newFake(t, func(action string, req map[string]any) any {
		switch action {
		case "FindFolder":
			return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{}}})
		case "FindItem":
			var items []any
			if stored != nil {
				items = append(items, map[string]any{"ItemId": map[string]any{"Id": "upstream-event"}})
			}
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": items, "TotalItemsInView": len(items)}})
		case "CreateItem":
			candidate := asMap(arr(req, "Body", "Items")[0])
			if recurring && (str(candidate, "Recurrence", "__type") != "RecurrenceType:#Exchange" || str(candidate, "Recurrence", "RecurrencePattern", "__type") != "WeeklyRecurrence:#Exchange" || num(candidate, "Recurrence", "RecurrencePattern", "Interval") != 1 || str(candidate, "Recurrence", "RecurrencePattern", "DaysOfWeek") != "Thursday" || str(candidate, "Recurrence", "RecurrencePattern", "FirstDayOfWeek") != "Monday" || str(candidate, "Recurrence", "RecurrenceRange", "__type") != "NumberedRecurrence:#Exchange" || str(candidate, "Recurrence", "RecurrenceRange", "StartDate") != "2026-10-15" || num(candidate, "Recurrence", "RecurrenceRange", "NumberOfOccurrences") != 4) {
				return okBody(map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorInvalidPropertySet"})
			}
			stored = candidate
			stored["ItemId"] = map[string]any{"Id": "upstream-event"}
			return itemResp(map[string]any{"ItemId": map[string]any{"Id": "upstream-event"}})
		case "GetItem":
			return itemResp(stored)
		case "UpdateItem":
			change := arr(req, "Body", "ItemChanges")[0]
			if str(change, "ItemId", "Id") != "upstream-event" {
				t.Errorf("update used the wrong upstream ID: %+v", change)
			}
			for _, update := range arr(change, "Updates") {
				if str(update, "Path", "FieldURI") == "Subject" {
					stored["Subject"] = str(update, "Item", "Subject")
				}
				if str(update, "Path", "FieldURI") == "Recurrence" {
					recurrenceUpdates++
					if str(req, "Header", "TimeZoneContext", "TimeZoneDefinition", "Id") != "UTC" {
						t.Errorf("recurrence update omitted UTC context: %+v", req)
					}
					if str(update, "__type") == "DeleteItemField:#Exchange" {
						delete(stored, "Recurrence")
					} else {
						stored["Recurrence"] = dig(update, "Item", "Recurrence")
					}
				}
			}
			return itemResp(stored)
		case "DeleteItem":
			ids := arr(req, "Body", "ItemIds")
			if len(ids) != 1 || str(ids[0], "Id") != "upstream-event" {
				t.Errorf("delete used the wrong upstream ID: %+v", ids)
			}
			stored = nil
			return okBody(map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError"})
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	reg, err := bridge.NewRegistry(bridge.Config{
		StateDir: t.TempDir(), Login: calendarReadbackLogin{},
		Factory: Factory{BaseURL: upstream.(*provider).base},
	})
	if err != nil {
		t.Fatal(err)
	}
	const email = "user@example.com"
	if err := reg.Store().SaveAccount(bridge.AccountState{Email: email, Session: bridge.Session{
		Email: email, Channel: bridge.ChannelOWA, OWAAuthHeader: "Bearer fake", TokenExpiry: time.Now().Add(time.Hour),
	}}); err != nil {
		t.Fatal(err)
	}
	h := davserver.New(reg, "bridge-password", zerolog.Nop())
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.SetBasicAuth(email, "bridge-password")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	path := "/caldav/" + email + "/calendar/" + uid + ".ics"
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTART:20261015T070000Z\r\nDTEND:20261015T080000Z\r\nSUMMARY:Readback meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	const repeat = "RRULE:FREQ=WEEKLY;BYDAY=TH;COUNT=4\r\n"
	if recurring {
		ics = strings.ReplaceAll(ics, "END:VEVENT", repeat+"END:VEVENT")
	}
	if rr := request("PUT", path, ics); rr.Code != http.StatusCreated {
		t.Fatalf("PUT = %d: %s", rr.Code, rr.Body.String())
	}
	if rr := request("GET", path, ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "UID:"+uid) || !strings.Contains(rr.Body.String(), "DTSTART:20261015T070000Z") {
		t.Fatalf("GET after successful creation = %d: %s", rr.Code, rr.Body.String())
	}
	report := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT"><C:time-range start="20261015T000000Z" end="20261016T000000Z"/></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`
	if rr := request("REPORT", "/caldav/"+email+"/calendar/", report); rr.Code != http.StatusMultiStatus || !strings.Contains(rr.Body.String(), uid+".ics") || !strings.Contains(rr.Body.String(), "Readback meeting") {
		t.Fatalf("REPORT after creation = %d: %s", rr.Code, rr.Body.String())
	}
	if rr := request("PUT", path, strings.ReplaceAll(ics, "Readback meeting", "Updated meeting")); rr.Code != http.StatusNoContent {
		t.Fatalf("second PUT must update, not duplicate the event: %d: %s", rr.Code, rr.Body.String())
	}
	multiget := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>` + path + `</D:href></C:calendar-multiget>`
	if rr := request("REPORT", "/caldav/"+email+"/calendar/", multiget); rr.Code != http.StatusMultiStatus || !strings.Contains(rr.Body.String(), "Updated meeting") {
		t.Fatalf("multiget after update = %d: %s", rr.Code, rr.Body.String())
	}
	if recurring {
		if recurrenceUpdates != 0 {
			t.Fatalf("ordinary title edit rewrote native repeat %d times", recurrenceUpdates)
		}
		if rr := request("GET", path, ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "FREQ=WEEKLY;BYDAY=TH;WKST=MO;COUNT=4") {
			t.Fatalf("weekly selectors not retained after ordinary edit: %d: %s", rr.Code, rr.Body.String())
		}
		edited := strings.ReplaceAll(ics, repeat, "RRULE:FREQ=MONTHLY;BYMONTHDAY=15;COUNT=3\r\n")
		if rr := request("PUT", path, edited); rr.Code != http.StatusNoContent {
			t.Fatalf("series edit = %d: %s", rr.Code, rr.Body.String())
		}
		if rr := request("GET", path, ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "FREQ=MONTHLY;BYMONTHDAY=15;COUNT=3") {
			t.Fatalf("GET after series edit = %d: %s", rr.Code, rr.Body.String())
		}
		if rr := request("PUT", path, strings.ReplaceAll(ics, repeat, "")); rr.Code != http.StatusNoContent {
			t.Fatalf("series clear = %d: %s", rr.Code, rr.Body.String())
		}
		if rr := request("REPORT", "/caldav/"+email+"/calendar/", multiget); rr.Code != http.StatusMultiStatus || strings.Contains(rr.Body.String(), "RRULE") {
			t.Fatalf("multiget after series clear = %d: %s", rr.Code, rr.Body.String())
		}
		if recurrenceUpdates != 2 {
			t.Fatalf("series edit and clear made %d native repeat updates, want 2", recurrenceUpdates)
		}
	}
	if rr := request("DELETE", path, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", rr.Code, rr.Body.String())
	}
	if rr := request("GET", path, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("GET after real deletion = %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListEventsPagesDiscoveryAndBoundsHydration(t *testing.T) {
	f, p := newFake(t, func(action string, req map[string]any) any {
		switch action {
		case "FindItem":
			offset := int(num(req, "Body", "Paging", "Offset"))
			if offset != 0 && offset != 1000 {
				t.Errorf("unexpected discovery offset %d", offset)
			}
			var rows []any
			for i := offset; i < min(offset+1000, 1001); i++ {
				rows = append(rows, map[string]any{"ItemId": map[string]any{"Id": fmt.Sprintf("ev%d", i)}})
			}
			return okBody(map[string]any{"RootFolder": map[string]any{
				"Items": rows, "TotalItemsInView": 1001, "IndexedPagingOffset": offset + len(rows),
			}})
		case "GetItem":
			ids := arr(req, "Body", "ItemIds")
			if len(ids) == 0 || len(ids) > 100 {
				t.Errorf("unbounded hydration request: %d items", len(ids))
			}
			var items []any
			for _, id := range ids {
				value := str(id, "Id")
				items = append(items, calendarReadbackItem(value, "uid-"+value))
			}
			return itemResp(items...)
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	from := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	events, err := p.ListEvents(context.Background(), "custom-calendar-id", from, from.Add(24*time.Hour))
	if err != nil || len(events) != 1001 || events[1000].ID != "ev1000" {
		t.Fatalf("paged ListEvents returned %d events, %v; want 1001", len(events), err)
	}
	folders := arr(f.last("FindItem").Body, "Body", "ParentFolderIds")
	if len(folders) != 1 || str(folders[0], "Id") != "custom-calendar-id" || str(folders[0], "__type") != "FolderId:#Exchange" {
		t.Fatalf("wrong calendar discovery folder: %+v", folders)
	}
}

func TestListEventsRejectsIncompleteDiscovery(t *testing.T) {
	for _, test := range []struct {
		name string
		root map[string]any
	}{
		{"missing discovery folder", nil},
		{"empty page with more items", map[string]any{"TotalItemsInView": 1, "Items": []any{}}},
		{"missing ID", map[string]any{"Items": []any{map[string]any{}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, p := newFake(t, func(action string, _ map[string]any) any {
				if action != "FindItem" {
					t.Errorf("unexpected action %s", action)
				}
				return okBody(map[string]any{"RootFolder": test.root})
			})
			events, err := p.ListEvents(context.Background(), "calendar", time.Time{}, time.Time{})
			if err == nil || len(events) != 0 {
				t.Fatalf("incomplete discovery must fail: %+v, %v", events, err)
			}
		})
	}
}
