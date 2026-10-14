package owa

import (
	"context"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWATaskDateOnlyMetadataSurvivesNativeRead(t *testing.T) {
	var stored map[string]any
	f, p := newFake(t, func(action string, req map[string]any) any {
		switch action {
		case "CreateItem":
			stored = asMap(arr(req, "Body", "Items")[0])
			stored["ItemId"] = map[string]any{"Id": "cloud"}
			return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
		case "FindItem":
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": map[string]any{"Id": "cloud"}}}, "TotalItemsInView": 1}})
		case "GetItem":
			return itemResp(stored)
		}
		return itemResp()
	})
	date := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	if _, err := p.CreateTask(context.Background(), bridge.Task{UID: "client", Start: &date, Due: &date, StartDateOnly: true, DueDateOnly: true}); err != nil {
		t.Fatal(err)
	}
	properties := arr(stored, "ExtendedProperty")
	if len(properties) != 3 {
		t.Fatalf("date flags not saved beside UID: %+v", properties)
	}
	for _, property := range properties {
		field := asMap(dig(property, "ExtendedFieldURI"))
		if str(field, "PropertyType") != "String" {
			t.Fatalf("date flags must use known String value contract: %+v", property)
		}
		delete(field, "DistinguishedPropertySetId")
		field["PropertySetId"] = "00020329-0000-0000-c000-000000000046"
	}
	restarted := Factory{BaseURL: p.(*provider).base}.New("user@example.com", bridge.Session{OWAAuthHeader: "Bearer fake"})
	tasks, err := restarted.ListTasks(context.Background(), "tasks")
	if err != nil || len(tasks) != 1 || !tasks[0].StartDateOnly || !tasks[0].DueDateOnly || tasks[0].UID != "client" {
		t.Fatalf("native date-only readback = %+v, %v", tasks, err)
	}
	requested := map[string]bool{}
	for _, field := range arr(f.last("GetItem").Body, "Body", "ItemShape", "AdditionalProperties") {
		requested[str(field, "PropertyName")] = true
	}
	for _, name := range []string{"O365BridgeTaskUID", "O365BridgeTaskStartDateOnly", "O365BridgeTaskDueDateOnly"} {
		if !requested[name] {
			t.Errorf("GetItem did not request %s", name)
		}
	}
}

func TestOWATaskDateOnlyDoesNotGuessFromMidnight(t *testing.T) {
	for _, tc := range []struct {
		date string
		want bool
	}{{"2026-10-15", true}, {"2026-10-15T00:00:00Z", false}, {"2026-10-15T00:00:00+03:00", false}} {
		if got := taskFromItem(map[string]any{"StartDate": tc.date, "DueDate": tc.date}, "tasks"); got.StartDateOnly != tc.want || got.DueDateOnly != tc.want {
			t.Errorf("date precision guessed for %s: %+v", tc.date, got)
		}
	}
	item := map[string]any{"DueDate": "2026-10-15", "ExtendedProperty": []any{map[string]any{"ExtendedFieldURI": map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertyName": "O365BridgeTaskDueDateOnly", "PropertyType": "String"}, "Value": "false"}}}
	if taskFromItem(item, "tasks").DueDateOnly {
		t.Fatal("explicit persisted datetime precision ignored")
	}
}

func TestOWATaskDateOnlyMetadataUpdateHasIndependentPaths(t *testing.T) {
	date := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for _, full := range []bool{false, true} {
		f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
		if err := p.UpdateTask(context.Background(), bridge.Task{ID: "cloud", UID: "client", Due: &date, DueDateOnly: true, FullUpdate: full}); err != nil {
			t.Fatal(err)
		}
		found := map[string]string{}
		for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
			if str(update, "Path", "__type") != "ExtendedPropertyUri:#Exchange" {
				continue
			}
			properties := arr(update, "Item", "ExtendedProperty")
			if len(properties) != 1 || str(properties[0], "ExtendedFieldURI", "PropertyName") != str(update, "Path", "PropertyName") {
				t.Fatalf("named property Path carried unrelated properties: %+v", update)
			}
			found[str(update, "Path", "PropertyName")] = str(properties[0], "Value")
		}
		if found["O365BridgeTaskUID"] != "client" || found["O365BridgeTaskDueDateOnly"] != "true" {
			t.Fatalf("date precision/identity mutation missing: %+v", found)
		}
		_, startWritten := found["O365BridgeTaskStartDateOnly"]
		if startWritten != full {
			t.Fatalf("omitted start precision must only clear on full update: %+v", found)
		}
	}
}
