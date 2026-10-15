package owa

import (
	"context"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWATaskRecurrenceUsesTaskSpecificUpdateURI(t *testing.T) {
	for _, mode := range []string{"set", "delete", "unrelated body and categories"} {
		t.Run(mode, func(t *testing.T) {
			calls, recurrenceChanges := 0, 0
			_, p := newFake(t, func(action string, req map[string]any) any {
				calls++
				if action != "UpdateItem" {
					t.Errorf("unexpected action %s", action)
				}
				changes := arr(req, "Body", "ItemChanges")
				if len(changes) != 1 || str(changes[0], "ItemId", "Id") != "native-task" {
					t.Errorf("wrong native update: %+v", req)
					return okBody(nil)
				}
				for _, update := range arr(changes[0], "Updates") {
					switch str(update, "Path", "FieldURI") {
					case "Recurrence", "calendar:Recurrence":
						// The live OWA translator resolves bare Recurrence to the calendar property.
						return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{map[string]any{
							"ResponseClass": "Error", "ResponseCode": "ErrorInvalidPropertyRequest", "MessageText": "Property is not valid for this object type.",
							"MessageXml": `<t:FieldURI xmlns:t="http://schemas.microsoft.com/exchange/services/2006/types" FieldURI="calendar:Recurrence" />`,
						}}}})
					case "TaskRecurrence":
						recurrenceChanges++
						if mode == "set" {
							if str(update, "__type") != "SetItemField:#Exchange" || str(update, "Item", "__type") != "Task:#Exchange" || str(update, "Item", "Recurrence", "__type") != "TaskRecurrenceType:#Exchange" {
								t.Errorf("invalid native task recurrence set: %+v", update)
							}
							pattern := dig(update, "Item", "Recurrence", "RecurrencePattern")
							if str(pattern, "__type") != "WeeklyRecurrence:#Exchange" || num(pattern, "Interval") != 1 || str(pattern, "DaysOfWeek") != "Thursday" || str(pattern, "FirstDayOfWeek") != "Monday" || str(update, "Item", "Recurrence", "RecurrenceRange", "StartDate") != "2026-10-15" {
								t.Errorf("weekly task recurrence payload changed: %+v", update)
							}
						} else if mode == "delete" {
							if str(update, "__type") != "DeleteItemField:#Exchange" || dig(update, "Item") != nil {
								t.Errorf("invalid native task recurrence delete: %+v", update)
							}
						} else {
							t.Errorf("unrelated edit touched native recurrence: %+v", update)
						}
					}
				}
				return itemResp(map[string]any{"ItemId": itemID("native-task")})
			})
			start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
			task := bridge.Task{ID: "native-task", UID: "client-uid", RecurSet: true}
			switch mode {
			case "set":
				task.Start, task.Recur, task.FullUpdate, task.StatusUnchanged = &start, "FREQ=WEEKLY;BYDAY=TH;WKST=MO", true, true
			case "unrelated body and categories":
				task.Recur, task.RecurSet, task.Body, task.Categories = "FREQ=WEEKLY;BYDAY=TH;WKST=MO", false, "Edited note", []string{"Work"}
			}
			if err := p.UpdateTask(context.Background(), task); err != nil {
				t.Fatalf("native task update rejected: %v", err)
			}
			wantRecurrenceChanges := 1
			if mode == "unrelated body and categories" {
				wantRecurrenceChanges = 0
			}
			if calls != 1 || recurrenceChanges != wantRecurrenceChanges {
				t.Fatalf("native calls=%d recurrence changes=%d, want 1/%d", calls, recurrenceChanges, wantRecurrenceChanges)
			}
		})
	}
}
