package owa

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/davserver"
	"github.com/rs/zerolog"
)

// Exchange date edits return a detached copy while advancing the original ID.
// Both retain recurrence data and copied UID metadata, even while incomplete.
func TestCalDAVRecurringTaskDateEditKeepsSeriesAndDetachedOccurrenceAddressable(t *testing.T) {
	const email, uid = "user@example.com", "client-series"
	const seriesID, detachedID = "native/series+%2F", "native/detached+%2F"
	var mu sync.Mutex
	makeTask := func(id string, dead bool, date string) map[string]any {
		item := taskReadbackItem(id, uid)
		item["Body"] = map[string]any{"BodyType": "Text", "Value": "Plain notes"}
		item["StartDate"], item["DueDate"] = date+"T00:00:00Z", date+"T00:00:00Z"
		item["Status"], item["PercentComplete"] = "InProgress", "0"
		item["Recurrence"] = map[string]any{"RecurrencePattern": map[string]any{"__type": "DailyRecurrence:#Exchange", "Interval": 1}, "RecurrenceRange": map[string]any{"__type": "NoEndRecurrence:#Exchange", "StartDate": "2026-10-15Z"}}
		if !dead {
			item["IsRecurring"] = true
		}
		value := "false"
		if dead {
			value = "true"
		}
		item["ExtendedProperty"] = append(arr(item, "ExtendedProperty"),
			map[string]any{"ExtendedFieldURI": map[string]any{"DistinguishedPropertySetId": "Task", "PropertyId": 33033, "PropertyType": "Boolean"}, "Value": value},
			map[string]any{"ExtendedFieldURI": map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertyName": "O365BridgeTaskStartDateOnly", "PropertyType": "String"}, "Value": "true"},
			map[string]any{"ExtendedFieldURI": map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertyName": "O365BridgeTaskDueDateOnly", "PropertyType": "String"}, "Value": "true"})
		return item
	}
	stored := map[string]map[string]any{seriesID: makeTask(seriesID, false, "2026-10-15")}
	updates, deletes := 0, 0
	_, upstream := newFake(t, func(action string, request map[string]any) any {
		mu.Lock()
		defer mu.Unlock()
		switch action {
		case "FindFolder":
			return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{}}})
		case "FindItem":
			var rows []any
			// Return the detached occurrence first to rule out first-match ownership.
			for _, id := range []string{detachedID, seriesID} {
				if item := stored[id]; item != nil {
					rows = append(rows, map[string]any{"ItemId": item["ItemId"]})
				}
			}
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": rows, "TotalItemsInView": len(rows), "IncludesLastItemInRange": true}})
		case "GetItem":
			var items []any
			for _, id := range arr(request, "Body", "ItemIds") {
				items = append(items, stored[str(id, "Id")])
			}
			return itemResp(items...)
		case "UpdateItem":
			updates++
			if updates == 2 {
				repairedUID := assertTaskSplitUIDRepair(t, request, detachedID)
				for _, property := range arr(stored[detachedID], "ExtendedProperty") {
					if str(property, "ExtendedFieldURI", "PropertyName") == taskUIDPropertyName {
						asMap(property)["Value"] = repairedUID
					}
				}
				return itemResp(map[string]any{"ItemId": map[string]any{"Id": detachedID}})
			}
			changes := arr(request, "Body", "ItemChanges")
			if len(changes) != 1 || str(changes[0], "ItemId", "Id") != seriesID || updates != 1 {
				t.Errorf("wrong or repeated native update: %+v", changes)
			}
			body, due := "", ""
			for _, change := range arr(changes[0], "Updates") {
				switch str(change, "Path", "FieldURI") {
				case "Body":
					body = str(change, "Item", "Body", "Value")
				case "TaskDueDate":
					due = str(change, "Item", "DueDate")
				case "TaskPercentComplete", "TaskCompleteDate", "Recurrence":
					t.Errorf("date/note edit rewrote unrelated native state: %+v", change)
				}
			}
			if body != "Edited notes" || due != "2026-10-14T00:00:00.000Z" {
				t.Errorf("fixture did not exercise native date-edit split: body=%q due=%q", body, due)
			}
			stored[detachedID] = makeTask(detachedID, true, "2026-10-15")
			stored[seriesID] = makeTask(seriesID, false, "2026-10-16")
			for _, item := range stored {
				item["Body"] = map[string]any{"BodyType": "Text", "Value": body}
			}
			stored[seriesID]["Status"] = "NotStarted"
			return itemResp(map[string]any{"__type": "Task:#Exchange", "ItemId": map[string]any{"Id": detachedID}})
		case "DeleteItem":
			deletes++
			ids := arr(request, "Body", "ItemIds")
			if len(ids) != 1 || str(ids[0], "Id") != seriesID || deletes != 1 {
				t.Errorf("explicit delete targeted wrong native task: %+v", ids)
			}
			delete(stored, seriesID)
			return okBody(map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError"})
		default:
			t.Errorf("unexpected native action %s", action)
			return okBody(nil)
		}
	})
	stateDir := t.TempDir()
	newHandler := func() http.Handler {
		t.Helper()
		reg, err := bridge.NewRegistry(bridge.Config{StateDir: stateDir, Login: calendarReadbackLogin{}, Factory: Factory{BaseURL: upstream.(*provider).base}})
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Store().SaveAccount(bridge.AccountState{Email: email, Session: bridge.Session{Email: email, Channel: bridge.ChannelOWA, OWAAuthHeader: "Bearer fake", TokenExpiry: time.Now().Add(time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		return davserver.New(reg, "bridge-password", zerolog.Nop())
	}
	h := newHandler()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.SetBasicAuth(email, "bridge-password")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	collection := "/caldav/" + email + "/tasks/"
	path := collection + uid + ".ics"
	before := request("GET", path, "")
	if before.Code != 200 || !strings.Contains(before.Body.String(), "DUE;VALUE=DATE:20261015") {
		t.Fatalf("initial task GET=%d %s", before.Code, before.Body.String())
	}
	edited := strings.Replace(before.Body.String(), "DESCRIPTION:Plain notes", "DESCRIPTION:Edited notes", 1)
	edited = strings.Replace(edited, "DUE;VALUE=DATE:20261015", "DUE;VALUE=DATE:20261014", 1)
	put := request("PUT", path, edited)
	if put.Code != 204 {
		t.Fatalf("task date edit=%d %s", put.Code, put.Body.String())
	}
	h = newHandler()
	series := request("GET", path, "")
	if series.Code != 200 || !strings.Contains(series.Body.String(), "UID:"+uid+"\r\n") || !strings.Contains(series.Body.String(), "DTSTART;VALUE=DATE:20261016") || !strings.Contains(series.Body.String(), "RRULE:FREQ=DAILY") {
		t.Fatalf("continuing series lost original href after update/restart=%d %s", series.Code, series.Body.String())
	}
	if tag := put.Header().Get("ETag"); tag != "" && tag != series.Header().Get("ETag") {
		t.Errorf("task PUT returned immediately stale ETag: put=%s canonical=%s", tag, series.Header().Get("ETag"))
	}
	detachedPath := collection + url.PathEscape(detachedID) + ".ics"
	oneoff := request("GET", detachedPath, "")
	if oneoff.Code != 200 || !strings.Contains(oneoff.Body.String(), "UID:"+detachedID+"\r\n") || !strings.Contains(oneoff.Body.String(), "DTSTART;VALUE=DATE:20261015") || strings.Contains(oneoff.Body.String(), "RRULE:") || strings.Contains(oneoff.Body.String(), "X-O365BRIDGE-RECUR-") {
		t.Fatalf("detached native occurrence is missing or falsely recurring=%d %s", oneoff.Code, oneoff.Body.String())
	}
	query := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VTODO"/></C:comp-filter></C:filter></C:calendar-query>`
	for _, report := range []struct {
		body string
		want map[string]string
	}{
		{query, map[string]string{path: uid, detachedPath: detachedID}},
		{`<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>` + path + `</D:href></C:calendar-multiget>`, map[string]string{path: uid}},
		{`<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>` + detachedPath + `</D:href></C:calendar-multiget>`, map[string]string{detachedPath: detachedID}},
	} {
		rr := request("REPORT", collection, report.body)
		var result struct {
			Responses []struct {
				Href string `xml:"href"`
				Data string `xml:"propstat>prop>calendar-data"`
			} `xml:"response"`
		}
		if err := xml.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if rr.Code != 207 || len(result.Responses) != len(report.want) {
			t.Fatalf("calendar query/multiget lost or duplicated tasks: %d %+v", rr.Code, result)
		}
		for _, response := range result.Responses {
			wantedUID, ok := report.want[response.Href]
			if !ok || !strings.Contains(response.Data, "UID:"+wantedUID+"\r\n") {
				t.Fatalf("calendar response reused a href or wrong UID: %+v", response)
			}
			delete(report.want, response.Href)
		}
	}
	if rr := request("DELETE", path, ""); rr.Code != 204 {
		t.Fatalf("explicit continuing-series delete=%d %s", rr.Code, rr.Body.String())
	}
	h = newHandler()
	if rr := request("GET", detachedPath, ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), "UID:"+detachedID+"\r\n") {
		t.Fatalf("detached href changed after series deletion/restart=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("GET", path, ""); rr.Code != 404 {
		t.Fatalf("deleted series was resurrected by copied metadata=%d %s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if updates != 2 || deletes != 1 || len(stored) != 1 {
		t.Fatalf("unexpected native mutations: updates=%d deletes=%d rows=%d", updates, deletes, len(stored))
	}
}
