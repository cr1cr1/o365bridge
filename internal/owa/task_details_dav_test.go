package owa

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/davserver"
	"github.com/rs/zerolog"
)

func TestNativeTaskDetailsAndFileThroughCalDAV(t *testing.T) {
	const email, taskID, fileID = "user@example.com", "native/task+%2F", "file/1+%2F"
	const originalHTML = "<p><b>Rich note</b></p><p>Second &amp; details</p>"
	content := []byte{0, 1, 255}
	var mu sync.Mutex
	first := map[string]any{"__type": "Task:#Exchange", "ItemId": map[string]any{"Id": taskID}, "Subject": "Native task", "Body": map[string]any{"BodyType": "HTML", "Value": originalHTML}, "TextBody": map[string]any{"Value": "Rich note\r\nSecond & details"}, "DueDate": "2026-10-17T00:00:00Z", "Status": "InProgress", "PercentComplete": float64(25), "Importance": "High", "Categories": []any{"Work", "Sales, East"}, "ReminderIsSet": true, "ReminderDueBy": "2026-10-16T12:00:00Z", "Attachments": []any{map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": fileID}, "Name": "notes; 计划.txt", "ContentType": "text/plain; charset=utf-8", "Size": 3}}, "ExtendedProperty": []any{
		map[string]any{"ExtendedFieldURI": map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertyName": "O365BridgeTaskUID", "PropertyType": "String"}, "Value": "task-uid"},
		map[string]any{"ExtendedFieldURI": map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertyName": "O365BridgeTaskDueDateOnly", "PropertyType": "String"}, "Value": "true"},
	}, "Recurrence": map[string]any{"__type": "TaskRecurrenceType:#Exchange", "RecurrencePattern": map[string]any{"__type": "WeeklyRecurrence:#Exchange", "Interval": 2, "DaysOfWeek": "Monday Wednesday", "FirstDayOfWeek": "Monday"}, "RecurrenceRange": map[string]any{"__type": "NoEndRecurrence:#Exchange", "StartDate": "2026-10-15"}}}
	fileRequests, updates, bodyUpdates := 0, 0, 0
	_, upstream := newFake(t, func(action string, req map[string]any) any {
		mu.Lock()
		defer mu.Unlock()
		switch action {
		case "FindFolder":
			return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{}}})
		case "FindItem":
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": first["ItemId"]}}, "TotalItemsInView": 1}})
		case "GetItem":
			return itemResp(first)
		case "GetAttachment":
			fileRequests++
			if str(arr(req, "Body", "AttachmentIds")[0], "Id") != fileID {
				t.Errorf("wrong attachment request: %+v", req)
			}
			return okBody(map[string]any{"Attachments": []any{map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": fileID}, "Content": "AAH/"}}})
		case "UpdateItem":
			updates++
			changes := arr(req, "Body", "ItemChanges")
			if len(changes) != 1 || str(changes[0], "ItemId", "Id") != taskID {
				t.Errorf("wrong update identity: %+v", req)
			}
			for _, change := range arr(changes[0], "Updates") {
				field := str(change, "Path", "FieldURI")
				item := asMap(dig(change, "Item"))
				switch field {
				case "Subject":
					first["Subject"] = item["Subject"]
				case "Body":
					bodyUpdates++
					first["Body"] = item["Body"]
					first["TextBody"] = map[string]any{"Value": str(item, "Body", "Value")}
				case "Categories":
					first["Categories"] = item["Categories"]
				case "TaskDueDate":
					if str(change, "__type") == "DeleteItemField:#Exchange" {
						delete(first, "DueDate")
					} else {
						first["DueDate"] = item["DueDate"]
					}
				case "Recurrence", "calendar:Recurrence":
					t.Error("native task update targeted calendar recurrence")
				case "TaskRecurrence":
					if str(change, "__type") == "DeleteItemField:#Exchange" {
						delete(first, "Recurrence")
					} else {
						first["Recurrence"] = item["Recurrence"]
					}
				case "ReminderIsSet":
					if !boolean(item, "ReminderIsSet") {
						t.Error("ordinary edit cleared native reminder")
					}
					first["ReminderIsSet"] = item["ReminderIsSet"]
				case "ReminderDueBy":
					first["ReminderDueBy"] = item["ReminderDueBy"]
				case "TaskPercentComplete", "TaskCompleteDate":
					t.Error("ordinary edit rewrote precise native progress")
				default:
					if str(change, "Path", "__type") == "ExtendedPropertyUri:#Exchange" {
						for _, property := range arr(item, "ExtendedProperty") {
							name := str(property, "ExtendedFieldURI", "PropertyName")
							existing := arr(first, "ExtendedProperty")
							merged := false
							for i, old := range existing {
								if str(old, "ExtendedFieldURI", "PropertyName") == name {
									existing[i] = property
									merged = true
								}
							}
							if !merged {
								existing = append(existing, property)
							}
							first["ExtendedProperty"] = existing
						}
					}
				}
			}
			return okBody(map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError"})
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	state := t.TempDir()
	newHandler := func() http.Handler {
		t.Helper()
		reg, err := bridge.NewRegistry(bridge.Config{StateDir: state, Login: calendarReadbackLogin{}, Factory: Factory{BaseURL: upstream.(*provider).base}})
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Store().SaveAccount(bridge.AccountState{Email: email, Session: bridge.Session{Email: email, Channel: bridge.ChannelOWA, OWAAuthHeader: "Bearer fake", TokenExpiry: time.Now().Add(time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		return davserver.New(reg, "bridge-password", zerolog.Nop())
	}
	h := newHandler()
	request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			r.SetBasicAuth(email, "bridge-password")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	collection := "/caldav/" + email + "/tasks/"
	path := collection + "task-uid.ics"
	get := request("GET", path, "", true)
	if get.Code != 200 {
		t.Fatalf("GET=%d %s", get.Code, get.Body.String())
	}
	unfolded := strings.ReplaceAll(get.Body.String(), "\r\n ", "")
	for _, value := range []string{"DUE;VALUE=DATE:20261017", "CATEGORIES:Work,Sales\\, East", "DESCRIPTION:Rich note\\nSecond & details", "RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;WKST=MO", "ATTACH;FMTTYPE=text/plain:http://"} {
		if !strings.Contains(unfolded, value) {
			t.Fatalf("missing %q: %s", value, unfolded)
		}
	}
	var link string
	for _, line := range strings.Split(unfolded, "\r\n") {
		if strings.HasPrefix(line, "ATTACH;") {
			_, link, _ = strings.Cut(line, ":")
		}
	}
	if link == "" || strings.Contains(link, "Bearer") || strings.Contains(link, "bridge-password") {
		t.Fatalf("unsafe attachment link: %q", link)
	}
	if rr := request("GET", link, "", false); rr.Code != 401 {
		t.Fatalf("unauthenticated file=%d", rr.Code)
	}
	file := request("GET", link, "", true)
	if file.Code != 200 || !bytes.Equal(file.Body.Bytes(), content) || file.Header().Get("Content-Type") != "text/plain" || file.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("file download=%d %q %v", file.Code, file.Body.Bytes(), file.Header())
	}
	if rr := request("HEAD", link, "", true); rr.Code != 200 || rr.Body.Len() != 0 {
		t.Fatalf("file HEAD=%d %s", rr.Code, rr.Body.String())
	}
	report := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VTODO"/></C:comp-filter></C:filter></C:calendar-query>`
	rr := request("REPORT", collection, report, true)
	var result struct {
		Responses []struct {
			Props []struct {
				Data string `xml:"prop>calendar-data"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 207 || len(result.Responses) != 1 || !strings.Contains(result.Responses[0].Props[0].Data, "DUE;VALUE=DATE:20261017") {
		t.Fatalf("detail report=%d %s", rr.Code, rr.Body.String())
	}
	multiget := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>http://example.com` + path + `</D:href></C:calendar-multiget>`
	if rr := request("REPORT", collection, multiget, true); rr.Code != 207 || !strings.Contains(rr.Body.String(), "DUE;VALUE=DATE:20261017") || !strings.Contains(rr.Body.String(), "ATTACH;FMTTYPE=text/plain") {
		t.Fatalf("detail multiget=%d %s", rr.Code, rr.Body.String())
	}
	edited := strings.Replace(get.Body.String(), "SUMMARY:Native task", "SUMMARY:Title only", 1)
	if rr := request("PUT", path, edited, true); rr.Code != 204 {
		t.Fatalf("ordinary edit=%d %s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	if bodyUpdates != 0 || str(first, "Body", "Value") != originalHTML || num(first, "PercentComplete") != 25 || len(arr(first, "Attachments")) != 1 || !boolean(first, "ReminderIsSet") {
		t.Fatalf("ordinary edit lost native details: %+v", first)
	}
	mu.Unlock()
	h = newHandler()
	refreshed := request("GET", path, "", true)
	if refreshed.Code != 200 || !strings.Contains(refreshed.Body.String(), "DUE;VALUE=DATE:20261017") {
		t.Fatalf("fresh provider DATE=%d %s", refreshed.Code, refreshed.Body.String())
	}
	unsupported := strings.Replace(edited, "END:VTODO", "ATTACH:https://external.invalid/file.txt\r\nEND:VTODO", 1)
	mu.Lock()
	before := updates
	mu.Unlock()
	if rr := request("PUT", path, unsupported, true); rr.Code != 501 {
		t.Fatalf("unsupported attachment accepted=%d %s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if updates != before || fileRequests != 2 {
		t.Fatalf("rejected attachment wrote/fetched unexpectedly: updates=%d fileRequests=%d", updates, fileRequests)
	}
}
