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

// Exercise the HTTP adapter rather than a fake that preserves UIDs in memory.
func TestCalDAVNativeTaskReadbackAfterCreationAndRestart(t *testing.T) {
	const email, uid = "user@example.com", "thunderbird-task-uid"
	const legacyID, createdID = "cloud/legacy+%2F", "cloud/new+%2F"
	var mu sync.Mutex
	stored := map[string]map[string]any{
		legacyID: {"__type": "Task:#Exchange", "ItemId": map[string]any{"Id": legacyID}, "Subject": "Already in Outlook", "Status": "NotStarted", "PercentComplete": 0},
	}
	creates := 0
	_, upstream := newFake(t, func(action string, req map[string]any) any {
		mu.Lock()
		defer mu.Unlock()
		switch action {
		case "FindFolder":
			return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{
				map[string]any{"__type": "TasksFolder:#Exchange", "FolderId": map[string]any{"Id": "opaque-primary-task-folder"}, "FolderClass": "IPF.Task", "DisplayName": "Localized tasks"},
				map[string]any{"__type": "Folder:#Exchange", "FolderId": map[string]any{"Id": "mail-folder-named-tasks"}, "FolderClass": "IPF.Note", "DisplayName": "Tasks"},
			}}})
		case "FindItem":
			if str(arr(req, "Body", "ParentFolderIds")[0], "Id") != "tasks" {
				t.Errorf("task discovery targeted non-task folder: %+v", req)
			}
			var rows []any
			for _, item := range stored {
				rows = append(rows, map[string]any{"ItemId": item["ItemId"]})
			}
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": rows, "TotalItemsInView": len(rows), "IncludesLastItemInRange": true}})
		case "GetItem":
			var responses []any
			for _, id := range arr(req, "Body", "ItemIds") {
				if item := stored[str(id, "Id")]; item != nil {
					readback := make(map[string]any, len(item))
					for key, value := range item {
						readback[key] = value
					}
					// Exchange may return the equivalent GUID instead of PublicStrings.
					var properties []any
					for _, property := range arr(item, "ExtendedProperty") {
						properties = append(properties, map[string]any{
							"ExtendedFieldURI": map[string]any{"PropertySetId": "00020329-0000-0000-C000-000000000046", "PropertyName": str(property, "ExtendedFieldURI", "PropertyName"), "PropertyType": "String"},
							"Value":            str(property, "Value"),
						})
					}
					readback["ExtendedProperty"] = properties
					responses = append(responses, map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Items": []any{readback}})
				}
			}
			return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": responses}})
		case "CreateItem":
			items := arr(req, "Body", "Items")
			if len(items) != 1 || str(items[0], "__type") != "Task:#Exchange" || str(req, "Body", "SavedItemFolderId", "BaseFolderId", "Id") != "tasks" {
				t.Errorf("not native task creation: %+v", req)
			}
			item := asMap(items[0])
			creates++
			item["ItemId"] = map[string]any{"Id": createdID}
			stored[createdID] = item
			return itemResp(map[string]any{"ItemId": item["ItemId"]})
		case "UpdateItem":
			changes := arr(req, "Body", "ItemChanges")
			if len(changes) != 1 || str(changes[0], "ItemId", "Id") != createdID {
				t.Errorf("update selected wrong native task: %+v", req)
			}
			for _, update := range arr(changes[0], "Updates") {
				if str(update, "__type") == "DeleteItemField:#Exchange" {
					switch str(update, "Path", "FieldURI") {
					case "TaskDueDate":
						delete(stored[createdID], "DueDate")
					case "TaskStartDate":
						delete(stored[createdID], "StartDate")
					case "TaskCompleteDate":
						delete(stored[createdID], "CompleteDate")
					}
					continue
				}
				item := asMap(dig(update, "Item"))
				if str(item, "__type") != "Task:#Exchange" {
					t.Errorf("task update used non-task item: %+v", update)
				}
				for key, value := range item {
					if key != "__type" {
						stored[createdID][key] = value
					}
				}
			}
			return itemResp(map[string]any{"ItemId": stored[createdID]["ItemId"]})
		case "DeleteItem":
			ids := arr(req, "Body", "ItemIds")
			if len(ids) != 1 || str(ids[0], "Id") != createdID {
				t.Errorf("delete selected wrong native task: %+v", req)
			}
			delete(stored, createdID)
			return okBody(map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError"})
		default:
			t.Errorf("unexpected upstream action %s", action)
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
	if rr := request("PROPFIND", "/caldav/"+email+"/", ""); rr.Code != http.StatusMultiStatus || !strings.Contains(rr.Body.String(), collection) || !strings.Contains(rr.Body.String(), `name="VTODO"`) || !strings.Contains(rr.Body.String(), `name="VEVENT"`) {
		t.Fatalf("tasks must be discovered separately from appointments: %d %s", rr.Code, rr.Body.String())
	}
	legacyPath := collection + url.PathEscape(legacyID) + ".ics"
	if rr := request("GET", legacyPath, ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "UID:"+legacyID+"\r\n") || !strings.Contains(rr.Body.String(), "SUMMARY:Already in Outlook") {
		t.Fatalf("existing cloud task readback=%d %s", rr.Code, rr.Body.String())
	}
	path := collection + uid + ".ics"
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:" + uid + "\r\nSUMMARY:Thunderbird task\r\nDESCRIPTION:Details\r\nDUE:20261016T100000Z\r\nSTATUS:NEEDS-ACTION\r\nPRIORITY:1\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	if rr := request("PUT", path, ics); rr.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
	}
	// No adapter-memory or handler-memory UID map may be needed to read back.
	h = newHandler()
	if rr := request("GET", path, ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "UID:"+uid+"\r\n") || !strings.Contains(rr.Body.String(), "SUMMARY:Thunderbird task") || !strings.Contains(rr.Body.String(), "DUE:20261016T100000Z") || !strings.Contains(rr.Body.String(), "BEGIN:VTODO") || strings.Contains(rr.Body.String(), "BEGIN:VEVENT") {
		t.Fatalf("GET after native task create/restart=%d %s", rr.Code, rr.Body.String())
	}
	report := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VTODO"/></C:comp-filter></C:filter></C:calendar-query>`
	rr := request("REPORT", collection, report)
	var result struct {
		Responses []struct {
			Href  string `xml:"href"`
			Props []struct {
				Data string `xml:"prop>calendar-data"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusMultiStatus || len(result.Responses) != 2 {
		t.Fatalf("task query=%d %s", rr.Code, rr.Body.String())
	}
	for _, response := range result.Responses {
		if len(response.Props) != 1 || !strings.Contains(response.Props[0].Data, "BEGIN:VTODO") {
			t.Fatalf("not task response: %+v", response)
		}
	}
	updated := strings.ReplaceAll(ics, "Thunderbird task", "Updated task")
	if rr := request("PUT", path, updated); rr.Code != http.StatusNoContent {
		t.Fatalf("repeat PUT must update=%d %s", rr.Code, rr.Body.String())
	}
	completed := strings.ReplaceAll(updated, "STATUS:NEEDS-ACTION", "STATUS:COMPLETED")
	if rr := request("PUT", path, completed); rr.Code != http.StatusNoContent {
		t.Fatalf("complete=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("GET", path, ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "STATUS:COMPLETED") {
		t.Fatalf("completion readback=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("PUT", path, updated); rr.Code != http.StatusNoContent {
		t.Fatalf("reopen=%d %s", rr.Code, rr.Body.String())
	}
	multiget := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>http://bridge.example` + path + `</D:href></C:calendar-multiget>`
	rr = request("REPORT", collection, multiget)
	if rr.Code != http.StatusMultiStatus || !strings.Contains(rr.Body.String(), "SUMMARY:Updated task") || !strings.Contains(rr.Body.String(), "STATUS:NEEDS-ACTION") || strings.Contains(rr.Body.String(), "Already in Outlook") {
		t.Fatalf("selective multiget=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("DELETE", path, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("GET", path, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("deleted task GET=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("GET", legacyPath, ""); rr.Code != http.StatusOK {
		t.Fatalf("deleting task removed sibling=%d %s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 || len(stored) != 1 {
		t.Fatalf("duplicate/wrong task created: creates=%d stored=%+v", creates, stored)
	}
}
