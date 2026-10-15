package owa

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/davserver"
	"github.com/rs/zerolog"
)

func TestOWATaskReminderCreateEditAndClearThroughCalDAV(t *testing.T) {
	const email, uid, nativeID = "user@example.com", "task-reminder", "native-task"
	const preciseReminder = "2026-10-15T09:45:00.1234567Z"
	var mu sync.Mutex
	var stored map[string]any
	creates, updates, reminderFields := 0, 0, 0
	_, upstream := newFake(t, func(action string, request map[string]any) any {
		mu.Lock()
		defer mu.Unlock()
		switch action {
		case "FindFolder":
			return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{}}})
		case "FindItem":
			var rows []any
			if stored != nil {
				rows = append(rows, map[string]any{"ItemId": stored["ItemId"]})
			}
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": rows, "TotalItemsInView": len(rows), "IncludesLastItemInRange": true}})
		case "GetItem":
			fields := map[string]bool{}
			for _, field := range arr(request, "Body", "ItemShape", "AdditionalProperties") {
				fields[str(field, "FieldURI")] = true
			}
			if !fields["item:ReminderIsSet"] || !fields["item:ReminderDueBy"] {
				t.Error("native task read omitted reminder fields")
			}
			return itemResp(stored)
		case "CreateItem":
			creates++
			stored = asMap(arr(request, "Body", "Items")[0])
			if str(stored, "__type") != "Task:#Exchange" || !boolean(stored, "ReminderIsSet") || str(stored, "ReminderDueBy") != "2026-10-15T09:45:00.000Z" {
				t.Errorf("client alarm did not become a native reminder: %+v", stored)
			}
			stored["ItemId"] = map[string]any{"Id": nativeID}
			if boolean(stored, "ReminderIsSet") {
				// Native timestamps can carry precision that standard TRIGGER cannot.
				stored["ReminderDueBy"] = preciseReminder
			}
			return itemResp(map[string]any{"ItemId": stored["ItemId"]})
		case "UpdateItem":
			updates++
			changes := arr(request, "Body", "ItemChanges")
			if len(changes) != 1 || str(changes[0], "ItemId", "Id") != nativeID {
				t.Errorf("reminder edit targeted wrong native item: %+v", changes)
			}
			for _, change := range arr(changes[0], "Updates") {
				field := str(change, "Path", "FieldURI")
				if field == "ReminderIsSet" || field == "ReminderDueBy" {
					reminderFields++
				}
				if field == "Recurrence" || field == "TaskRecurrence" {
					t.Error("reminder edit unexpectedly changed task repetition")
				}
				for key, value := range asMap(dig(change, "Item")) {
					if key != "__type" && key != "ExtendedProperty" {
						stored[key] = value
					}
				}
			}
			return itemResp(map[string]any{"ItemId": stored["ItemId"]})
		default:
			t.Errorf("unexpected native action %s", action)
			return okBody(nil)
		}
	})
	stateDir := t.TempDir()
	newHandler := func() *bridge.Registry {
		t.Helper()
		reg, err := bridge.NewRegistry(bridge.Config{StateDir: stateDir, Login: calendarReadbackLogin{}, Factory: Factory{BaseURL: upstream.(*provider).base}})
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Store().SaveAccount(bridge.AccountState{Email: email, Session: bridge.Session{Email: email, Channel: bridge.ChannelOWA, OWAAuthHeader: "Bearer fake", TokenExpiry: time.Now().Add(time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		return reg
	}
	h := davserver.New(newHandler(), "bridge-password", zerolog.Nop())
	path := "/caldav/" + email + "/tasks/" + uid + ".ics"
	request := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.SetBasicAuth(email, "bridge-password")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:" + uid + "\r\nSUMMARY:Task\r\nDESCRIPTION:Original notes\r\nDTSTART:20261015T100000Z\r\nDUE:20261015T110000Z\r\nSTATUS:NEEDS-ACTION\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:Task reminder\r\nTRIGGER;VALUE=DATE-TIME:20261015T094500Z\r\nEND:VALARM\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	if put := request("PUT", data); put.Code != 201 {
		t.Fatalf("create reminder=%d %s", put.Code, put.Body.String())
	}
	h = davserver.New(newHandler(), "bridge-password", zerolog.Nop())
	get := request("GET", "")
	if get.Code != 200 || !strings.Contains(get.Body.String(), "BEGIN:VALARM") || !strings.Contains(get.Body.String(), "TRIGGER;VALUE=DATE-TIME:20261015T094500Z") {
		t.Fatalf("native reminder lost on fresh read=%d %s", get.Code, get.Body.String())
	}
	edited := strings.Replace(get.Body.String(), "DESCRIPTION:Original notes", "DESCRIPTION:Edited notes", 1)
	if put := request("PUT", edited); put.Code != 204 {
		t.Fatalf("unrelated note edit=%d %s", put.Code, put.Body.String())
	}
	mu.Lock()
	precise, writes := str(stored, "ReminderDueBy"), reminderFields
	mu.Unlock()
	if precise != preciseReminder || writes != 0 {
		t.Fatalf("ordinary edit rewrote precise native reminder: timestamp=%s reminder-fields=%d", precise, writes)
	}
	get = request("GET", "")
	relative := strings.Replace(get.Body.String(), "TRIGGER;VALUE=DATE-TIME:20261015T094500Z", "TRIGGER;RELATED=END:-PT5M", 1)
	if put := request("PUT", relative); put.Code != 204 {
		t.Fatalf("relative reminder edit=%d %s", put.Code, put.Body.String())
	}
	get = request("GET", "")
	if get.Code != 200 || !strings.Contains(get.Body.String(), "TRIGGER;VALUE=DATE-TIME:20261015T105500Z") {
		t.Fatalf("relative END reminder not persisted/read as native instant=%d %s", get.Code, get.Body.String())
	}
	cleared := regexp.MustCompile(`(?s)BEGIN:VALARM\r\n.*?END:VALARM\r\n`).ReplaceAllString(get.Body.String(), "")
	if put := request("PUT", cleared); put.Code != 204 {
		t.Fatalf("clear reminder=%d %s", put.Code, put.Body.String())
	}
	get = request("GET", "")
	if get.Code != 200 || strings.Contains(get.Body.String(), "BEGIN:VALARM") {
		t.Fatalf("disabled native reminder reappeared=%d %s", get.Code, get.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if boolean(stored, "ReminderIsSet") || str(stored, "ReminderDueBy") != "2026-10-15T10:55:00.000Z" || reminderFields != 3 || creates != 1 || updates != 3 || str(stored, "Body", "Value") != "Edited notes" {
		t.Fatalf("native reminder lifecycle changed unrelated fields: enabled=%v reminder=%s fields=%d creates=%d updates=%d notes=%s", stored["ReminderIsSet"], stored["ReminderDueBy"], reminderFields, creates, updates, str(stored, "Body", "Value"))
	}
}
