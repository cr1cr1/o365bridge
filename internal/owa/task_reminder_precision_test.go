package owa

import (
	"context"
	"github.com/cr1cr1/o365bridge/internal/bridge"
	"testing"
	"time"
)

func TestOWATaskReminderUnchangedOmitsNativeWrites(t *testing.T) {
	reminder := time.Date(2026, 6, 2, 9, 0, 0, 123456700, time.UTC)
	for _, value := range []*time.Time{nil, &reminder} {
		f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
		task := bridge.Task{ID: "cloud", Title: "Unrelated edit", Reminder: value, FullUpdate: true, ReminderUnchanged: true}
		if err := p.UpdateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		subject := false
		for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
			field := str(update, "Path", "FieldURI")
			if field == "ReminderIsSet" || field == "ReminderDueBy" {
				t.Errorf("unchanged native reminder rewritten: %+v", update)
			}
			if field == "Subject" && str(update, "Item", "Subject") == task.Title {
				subject = true
			}
		}
		if !subject {
			t.Fatal("reminder preservation suppressed unrelated update")
		}
	}
}

func TestOWATaskReminderCreateIgnoresUnchangedFlag(t *testing.T) {
	reminder := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "created"}})
	})
	if _, err := p.CreateTask(context.Background(), bridge.Task{ListID: "tasks", Reminder: &reminder, ReminderUnchanged: true}); err != nil {
		t.Fatal(err)
	}
	item := arr(f.last("CreateItem").Body, "Body", "Items")[0]
	if dig(item, "ReminderIsSet") != true || str(item, "ReminderDueBy") != "2026-06-02T09:00:00.000Z" {
		t.Fatalf("create lost reminder: %+v", item)
	}
}

func TestOWATaskReminderDefaultUpdateSetClearAndPartial(t *testing.T) {
	reminder := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		reminder *time.Time
		full     bool
		want     int
	}{
		{"set", &reminder, false, 2}, {"full set", &reminder, true, 2}, {"clear", nil, true, 1}, {"partial", nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
			if err := p.UpdateTask(context.Background(), bridge.Task{ID: "cloud", Title: "Edit", Reminder: tc.reminder, FullUpdate: tc.full}); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
				switch str(update, "Path", "FieldURI") {
				case "ReminderIsSet":
					count++
					if dig(update, "Item", "ReminderIsSet") != (tc.reminder != nil) {
						t.Fatalf("wrong enabled state: %+v", update)
					}
				case "ReminderDueBy":
					count++
					if str(update, "Item", "ReminderDueBy") != "2026-06-02T09:00:00.000Z" {
						t.Fatalf("wrong reminder date: %+v", update)
					}
				}
			}
			if count != tc.want {
				t.Fatalf("reminder updates=%d want=%d", count, tc.want)
			}
		})
	}
}
