package owa

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWAEventCategoriesRead(t *testing.T) {
	want := []string{"Work", "Team, one", "Semi;colon", `Back\slash`, "项目"}
	f, p := newFake(t, func(action string, _ map[string]any) any {
		switch action {
		case "FindItem":
			return okBody(map[string]any{"RootFolder": map[string]any{
				"TotalItemsInView": 1, "Items": []any{map[string]any{"ItemId": map[string]any{"Id": "ev1"}}},
			}})
		case "GetItem":
			item := calendarReadbackItem("ev1", "uid1")
			item["Categories"] = want
			return itemResp(item)
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	events, err := p.ListEvents(context.Background(), "calendar", time.Time{}, time.Time{})
	if err != nil || len(events) != 1 {
		t.Fatalf("ListEvents = %+v, %v", events, err)
	}
	if !reflect.DeepEqual(events[0].Categories, want) {
		t.Errorf("categories = %#v, want %#v", events[0].Categories, want)
	}
	requested := false
	for _, property := range arr(f.last("GetItem").Body, "Body", "ItemShape", "AdditionalProperties") {
		if str(property, "FieldURI") == "item:Categories" {
			requested = true
		}
	}
	if !requested {
		t.Error("GetItem must explicitly request item:Categories")
	}
}

func TestOWAEventCategoryReadPresence(t *testing.T) {
	for _, tc := range []struct {
		name string
		item map[string]any
		want []string
	}{
		{"omitted", map[string]any{}, nil},
		{"empty", map[string]any{"Categories": []any{}}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventFromItem(tc.item, "calendar").Categories; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("categories = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestOWAEventCategoriesCreate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		categories []string
		want       []any
	}{
		{"omitted", nil, nil},
		{"empty", []string{}, []any{}},
		{"multiple", []string{"Work", "Team, one", "Semi;colon", `Back\slash`, "项目"}, []any{"Work", "Team, one", "Semi;colon", `Back\slash`, "项目"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newFake(t, func(action string, _ map[string]any) any {
				if action != "CreateItem" {
					t.Errorf("unexpected action %s", action)
				}
				return itemResp(map[string]any{"ItemId": map[string]any{"Id": "ev1"}, "UID": "uid1"})
			})
			start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
			ev, err := p.CreateEvent(context.Background(), bridge.Event{CalendarID: "calendar", UID: "uid1", Summary: "Meeting", Start: start, End: start.Add(time.Hour), Categories: tc.categories})
			if err != nil || ev.ID != "ev1" || ev.UID != "uid1" {
				t.Fatalf("CreateEvent = %+v, %v", ev, err)
			}
			req := f.last("CreateItem").Body
			items := arr(req, "Body", "Items")
			if len(items) != 1 || str(items[0], "__type") != "CalendarItem:#Exchange" {
				t.Fatalf("native calendar item missing: %+v", req)
			}
			got, exists := asMap(items[0])["Categories"]
			if exists != (tc.categories != nil) || (exists && !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("native Categories = %#v (present %v), want %#v", got, exists, tc.want)
			}
			if str(req, "Body", "SendMeetingInvitations") != "SendToNone" {
				t.Error("category create changed invitation policy")
			}
		})
	}
}

func TestOWAEventCategoriesUpdate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		categories []string
		want       []any
	}{
		{"omitted", nil, nil},
		{"clear", []string{}, []any{}},
		{"multiple", []string{"Work", "Team, one", "Semi;colon", `Back\slash`, "项目"}, []any{"Work", "Team, one", "Semi;colon", `Back\slash`, "项目"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newFake(t, func(action string, _ map[string]any) any {
				if action != "UpdateItem" {
					t.Errorf("unexpected action %s", action)
				}
				return okBody(map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError"})
			})
			if err := p.UpdateEvent(context.Background(), bridge.Event{ID: "native/ev+1", Summary: "Renamed", Categories: tc.categories}); err != nil {
				t.Fatal(err)
			}
			req := f.last("UpdateItem").Body
			changes := arr(req, "Body", "ItemChanges")
			if len(changes) != 1 || str(changes[0], "ItemId", "Id") != "native/ev+1" {
				t.Fatalf("category update changed item identity: %+v", req)
			}
			var categoryUpdates []any
			for _, update := range arr(changes[0], "Updates") {
				if str(update, "Path", "FieldURI") == "Categories" {
					categoryUpdates = append(categoryUpdates, update)
				}
			}
			if tc.categories == nil {
				if len(categoryUpdates) != 0 {
					t.Fatalf("omitted categories must not overwrite cloud categories: %+v", categoryUpdates)
				}
				return
			}
			if len(categoryUpdates) != 1 || str(categoryUpdates[0], "__type") != "SetItemField:#Exchange" || str(categoryUpdates[0], "Path", "__type") != "PropertyUri:#Exchange" {
				t.Fatalf("wrong native category mutation: %+v", categoryUpdates)
			}
			update := asMap(categoryUpdates[0])
			if str(update, "Item", "__type") != "CalendarItem:#Exchange" || !reflect.DeepEqual(dig(update, "Item", "Categories"), tc.want) {
				t.Errorf("native category set = %+v, want %#v", update, tc.want)
			}
			if str(req, "Body", "SendCalendarInvitationsOrCancellations") != "SendToNone" || str(req, "Body", "ConflictResolution") != "AlwaysOverwrite" {
				t.Error("category update changed invitation or conflict policy")
			}
		})
	}
}
