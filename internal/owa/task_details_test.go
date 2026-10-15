package owa

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWATaskPlainTextBodyPresence(t *testing.T) {
	for _, tc := range []struct {
		name string
		item map[string]any
		want string
	}{
		{"preferred plain text", map[string]any{"TextBody": map[string]any{"Value": "plain <literal>"}, "Body": map[string]any{"BodyType": "HTML", "Value": "<p>html</p>"}}, "plain <literal>"},
		{"empty plain text", map[string]any{"TextBody": map[string]any{"Value": ""}, "Body": map[string]any{"BodyType": "HTML", "Value": "<p><br></p>"}}, ""},
		{"plain body fallback", map[string]any{"Body": map[string]any{"BodyType": "Text", "Value": "literal <tag> & text"}}, "literal <tag> & text"},
		{"html body fallback", map[string]any{"Body": map[string]any{"BodyType": "HTML", "Value": "<p>First &amp; second</p><p>Third</p>"}}, "First & second\nThird"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskFromItem(tc.item, "tasks").Body; got != tc.want {
				t.Fatalf("task plain text = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOWATaskRepeatUpdateAndClear(t *testing.T) {
	start := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for _, rule := range []string{"FREQ=DAILY;INTERVAL=2;COUNT=3", ""} {
		t.Run(rule, func(t *testing.T) {
			f, p := newFake(t, func(string, map[string]any) any {
				return okBody(map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError"})
			})
			if err := p.UpdateTask(context.Background(), bridge.Task{ID: "task-id", Start: &start, Recur: rule, RecurSet: true}); err != nil {
				t.Fatal(err)
			}
			var mutation any
			for _, change := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
				if str(change, "Path", "FieldURI") == "TaskRecurrence" {
					mutation = change
				}
			}
			if mutation == nil {
				t.Fatal("repeat mutation missing from native task update")
			}
			if rule == "" {
				if str(mutation, "__type") != "DeleteItemField:#Exchange" {
					t.Fatalf("repeat clear = %+v", mutation)
				}
			} else if str(mutation, "__type") != "SetItemField:#Exchange" || str(mutation, "Item", "__type") != "Task:#Exchange" || dig(mutation, "Item", "Recurrence") == nil {
				t.Fatalf("not a native task recurrence mutation: %+v", mutation)
			}
		})
	}
}

func TestOWATaskRejectsUnsupportedRepeatBeforeMutation(t *testing.T) {
	start := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for _, rule := range []string{
		"FREQ=HOURLY", "FREQ=DAILY;BYHOUR=12", "FREQ=WEEKLY;BYDAY=1MO", "FREQ=MONTHLY;BYMONTHDAY=1,15", "FREQ=MONTHLY;BYDAY=MO;BYSETPOS=5", "FREQ=DAILY;INTERVAL=0", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;COUNT=2;UNTIL=20261030", "FREQ=DAILY;UNTIL=broken", "FREQ=DAILY;FREQ=WEEKLY", "FREQ=YEARLY;BYMONTHDAY=15", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30", "FREQ=DAILY;INTERVAL=2147483648", "FREQ=DAILY;COUNT=2147483648",
	} {
		t.Run(rule, func(t *testing.T) {
			f, p := newFake(t, func(string, map[string]any) any {
				return itemResp(map[string]any{"ItemId": map[string]any{"Id": "task-id"}})
			})
			task := bridge.Task{ID: "task-id", Start: &start, Recur: rule, RecurSet: true}
			if _, err := p.CreateTask(context.Background(), task); err == nil {
				t.Error("unsupported rule accepted on create")
			}
			if err := p.UpdateTask(context.Background(), task); err == nil {
				t.Error("unsupported rule accepted on update")
			}
			if len(f.requests) != 0 {
				t.Fatalf("unsupported rule mutated upstream: %+v", f.requests)
			}
		})
	}
}

func TestOWATaskCategoryRoundtripAndPresence(t *testing.T) {
	categories := []string{"Work", "Sales, Europe", "Budget; planning", "项目"}
	read := taskFromItem(map[string]any{"Categories": []any{"Work", "Sales, Europe", "Budget; planning", "项目"}}, "tasks")
	if !reflect.DeepEqual(read.Categories, categories) {
		t.Fatalf("task categories dropped: %#v", read.Categories)
	}
	for _, supplied := range [][]string{nil, {}, categories} {
		f, p := newFake(t, func(string, map[string]any) any {
			return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
		})
		if _, err := p.CreateTask(context.Background(), bridge.Task{Categories: supplied}); err != nil {
			t.Fatal(err)
		}
		created := asMap(arr(f.last("CreateItem").Body, "Body", "Items")[0])
		if _, exists := created["Categories"]; exists != (supplied != nil) {
			t.Fatalf("category create presence = %+v", created)
		}
		if err := p.UpdateTask(context.Background(), bridge.Task{ID: "cloud", Title: "Renamed", Categories: supplied}); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
			if str(update, "Path", "FieldURI") == "Categories" {
				found = true
				if str(update, "Item", "__type") != "Task:#Exchange" || len(arr(update, "Item", "Categories")) != len(supplied) {
					t.Fatalf("wrong native task categories: %+v", update)
				}
			}
		}
		if found != (supplied != nil) {
			t.Fatalf("category update presence = %v for %#v", found, supplied)
		}
	}
}

func TestOWATaskUnchangedRichBodyIsNotOverwritten(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
	if err := p.UpdateTask(context.Background(), bridge.Task{ID: "cloud", Title: "Renamed", Body: "plain projection", BodyUnchanged: true}); err != nil {
		t.Fatal(err)
	}
	for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
		if str(update, "Path", "FieldURI") == "Body" {
			t.Fatal("unrelated task edit overwrote native rich body")
		}
	}
}

func TestOWATaskInvalidDateReadFailsInsteadOfDroppingField(t *testing.T) {
	for _, field := range []string{"DueDate", "StartDate", "CompleteDate", "ReminderDueBy"} {
		t.Run(field, func(t *testing.T) {
			_, p := newFake(t, func(action string, _ map[string]any) any {
				if action == "FindItem" {
					return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": map[string]any{"Id": "cloud"}}}, "TotalItemsInView": 1}})
				}
				return itemResp(map[string]any{"__type": "Task:#Exchange", "ItemId": map[string]any{"Id": "cloud"}, field: "Invalid Date"})
			})
			if tasks, err := p.ListTasks(context.Background(), "tasks"); err == nil || len(tasks) != 0 {
				t.Fatalf("invalid %s silently became an undated task: tasks=%+v err=%v", field, tasks, err)
			}
		})
	}
}

func TestOWATaskZeroDateWriteRejectsBeforeMutation(t *testing.T) {
	zero := time.Time{}
	for _, field := range []string{"DueDate", "StartDate", "CompleteDate", "ReminderDueBy"} {
		t.Run(field, func(t *testing.T) {
			task := bridge.Task{ID: "cloud"}
			switch field {
			case "DueDate":
				task.Due = &zero
			case "StartDate":
				task.Start = &zero
			case "CompleteDate":
				task.Completed = &zero
			case "ReminderDueBy":
				task.Reminder = &zero
			}
			f, p := newFake(t, func(string, map[string]any) any {
				return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
			})
			if _, err := p.CreateTask(context.Background(), task); err == nil {
				t.Error("zero task date accepted on create")
			}
			if err := p.UpdateTask(context.Background(), task); err == nil {
				t.Error("zero task date accepted on update")
			}
			if len(f.requests) != 0 {
				t.Fatal("zero date reached native service")
			}
		})
	}
}

func TestOWATaskPartialUpdateDoesNotClearOmittedDates(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
	if err := p.UpdateTask(context.Background(), bridge.Task{ID: "cloud", Title: "Renamed"}); err != nil {
		t.Fatal(err)
	}
	for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
		if str(update, "__type") == "DeleteItemField:#Exchange" {
			t.Fatalf("partial update deleted omitted field: %+v", update)
		}
		if field := str(update, "Path", "FieldURI"); field == "TaskPercentComplete" || field == "Body" || field == "ReminderIsSet" || field == "Recurrence" || field == "TaskRecurrence" {
			t.Fatalf("partial update reset omitted field: %+v", update)
		}
	}
}

func TestOWATaskUnchangedStatusPreservesNativePercentAndCompletion(t *testing.T) {
	completed := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, date := range []*time.Time{nil, &completed} {
		f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
		if err := p.UpdateTask(context.Background(), bridge.Task{ID: "cloud", Title: "Renamed", Status: "in-process", Completed: date, FullUpdate: true, StatusUnchanged: true}); err != nil {
			t.Fatal(err)
		}
		for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
			field := str(update, "Path", "FieldURI")
			if field == "TaskPercentComplete" || field == "TaskCompleteDate" {
				t.Fatalf("unrelated edit overwrote native completion precision: %+v", update)
			}
		}
	}
}

func TestOWATaskRepeatRequiresRealAnchor(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "task-id"}})
	})
	if _, err := p.CreateTask(context.Background(), bridge.Task{Recur: "FREQ=DAILY"}); err == nil || !strings.Contains(err.Error(), "date") {
		t.Fatalf("recurring undated task should reject without a synthetic year-one date: %v", err)
	}
	if len(f.requests) != 0 {
		t.Fatal("undated recurrence mutated upstream")
	}
}
