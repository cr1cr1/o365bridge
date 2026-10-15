package graph_test

import (
	"context"
	"github.com/cr1cr1/o365bridge/internal/bridge"
	"net/http"
	"testing"
	"time"
)

func TestGraphTaskReminderUnchangedOmitsNativeWrites(t *testing.T) {
	reminder := time.Date(2026, 6, 2, 9, 0, 0, 123456700, time.UTC)
	for _, value := range []*time.Time{nil, &reminder} {
		f := newTaskUpdateFake(t)
		task := bridge.Task{ID: "task1", ListID: "list1", Title: "Unrelated edit", Reminder: value, FullUpdate: true, ReminderUnchanged: true}
		if err := f.provider().UpdateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		payload := bodyJSON(t, f.last().Body)
		for _, field := range []string{"isReminderOn", "reminderDateTime"} {
			if _, ok := payload[field]; ok {
				t.Errorf("unchanged reminder wrote %s: %+v", field, payload)
			}
		}
		if payload["title"] != task.Title {
			t.Fatalf("reminder preservation suppressed edit: %+v", payload)
		}
	}
}

func TestGraphTaskReminderCreateIgnoresUnchangedFlag(t *testing.T) {
	reminder := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOut(w, http.StatusCreated, `{"id":"created"}`)
	})
	if _, err := f.provider().CreateTask(context.Background(), bridge.Task{ListID: "list1", Reminder: &reminder, ReminderUnchanged: true}); err != nil {
		t.Fatal(err)
	}
	payload := bodyJSON(t, f.last().Body)
	if payload["isReminderOn"] != true || payload["reminderDateTime"] == nil {
		t.Fatalf("create lost reminder: %+v", payload)
	}
}

func TestGraphTaskReminderDefaultUpdateSetClearAndPartial(t *testing.T) {
	reminder := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		reminder   *time.Time
		full       bool
		wantFields bool
	}{
		{"set", &reminder, false, true}, {"full set", &reminder, true, true}, {"clear", nil, true, true}, {"partial", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTaskUpdateFake(t)
			if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", Title: "Edit", Reminder: tc.reminder, FullUpdate: tc.full}); err != nil {
				t.Fatal(err)
			}
			payload := bodyJSON(t, f.last().Body)
			enabled, exists := payload["isReminderOn"]
			if exists != tc.wantFields || exists && enabled != (tc.reminder != nil) {
				t.Fatalf("wrong reminder state: %+v", payload)
			}
			date, exists := payload["reminderDateTime"]
			if exists != tc.wantFields || exists && (date != nil) != (tc.reminder != nil) {
				t.Fatalf("wrong reminder date presence: %+v", payload)
			}
		})
	}
}
