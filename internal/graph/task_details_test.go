package graph_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func newTaskUpdateFake(t *testing.T) *fake {
	return newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == http.MethodGet {
			jsonOut(w, http.StatusOK, `{"id":"task1"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestGraphTaskCategoriesReadAndWrite(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == http.MethodGet {
			jsonOut(w, http.StatusOK, `{"value":[{"id":"task1","categories":["Work","Team, one","项目"]},{"id":"task2","categories":[]}]}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	tasks, err := f.provider().ListTasks(context.Background(), "list1")
	if err != nil || len(tasks) != 2 || !reflect.DeepEqual(tasks[0].Categories, []string{"Work", "Team, one", "项目"}) || tasks[1].Categories == nil || len(tasks[1].Categories) != 0 {
		t.Fatalf("categories read = %+v, %v", tasks, err)
	}
	for _, categories := range [][]string{nil, {}, {"Work", "Team, one"}} {
		if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", Categories: categories}); err != nil {
			t.Fatal(err)
		}
		got, exists := bodyJSON(t, f.last().Body)["categories"]
		if exists != (categories != nil) {
			t.Errorf("categories %#v presence = %v", categories, exists)
		}
		if exists {
			values, ok := got.([]any)
			if !ok || len(values) != len(categories) {
				t.Errorf("categories payload = %#v", got)
				continue
			}
			for i, category := range categories {
				if values[i] != category {
					t.Errorf("category %d = %#v, want %q", i, values[i], category)
				}
			}
		}
	}
}

func TestGraphTaskPartialUpdatePreservesAbsentDetails(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
	if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", Title: "Changed"}); err != nil {
		t.Fatal(err)
	}
	got := bodyJSON(t, f.last().Body)
	for _, key := range []string{"body", "dueDateTime", "startDateTime", "completedDateTime", "reminderDateTime", "isReminderOn", "categories", "recurrence"} {
		if _, exists := got[key]; exists {
			t.Errorf("partial update unexpectedly writes %s: %+v", key, got)
		}
	}
}

func TestGraphTaskPartialUpdatesDoNotResetAbsentScalars(t *testing.T) {
	for _, tc := range []struct {
		name string
		task bridge.Task
		want string
	}{
		{"categories only", bridge.Task{Categories: []string{"Work"}}, `{"categories":["Work"]}`},
		{"title only", bridge.Task{Title: "Changed"}, `{"title":"Changed"}`},
		{"status only", bridge.Task{Status: "completed"}, `{"status":"completed"}`},
		{"priority only", bridge.Task{Priority: 9}, `{"importance":"low"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
			tc.task.ID, tc.task.ListID = "task1", "list1"
			if err := f.provider().UpdateTask(context.Background(), tc.task); err != nil {
				t.Fatal(err)
			}
			got, want := bodyJSON(t, f.last().Body), bodyJSON(t, []byte(tc.want))
			if !reflect.DeepEqual(got, want) || len(f.reqs) != 1 {
				t.Fatalf("partial update = %+v calls=%d, want %+v in one PATCH", got, len(f.reqs), want)
			}
		})
	}
}

func TestGraphTaskUnchangedStatusPreservesNativeProgress(t *testing.T) {
	completed := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for _, completion := range []*time.Time{nil, &completed} {
		f := newTaskUpdateFake(t)
		task := bridge.Task{ID: "task1", ListID: "list1", Title: "Unrelated title edit", Status: "needs-action", Completed: completion, FullUpdate: true, StatusUnchanged: true}
		if err := f.provider().UpdateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		got := bodyJSON(t, f.last().Body)
		for _, key := range []string{"status", "completedDateTime"} {
			if _, exists := got[key]; exists {
				t.Errorf("unchanged native progress must omit %s: %+v", key, got)
			}
		}
		if got["title"] != "Unrelated title edit" {
			t.Fatalf("status preservation suppressed unrelated task edit: %+v", got)
		}
	}
}

func TestGraphTaskUnchangedNotePreservesNativeHTML(t *testing.T) {
	f := newTaskUpdateFake(t)
	if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", Body: "Plain projection", FullUpdate: true, BodyUnchanged: true}); err != nil {
		t.Fatal(err)
	}
	got := bodyJSON(t, f.last().Body)
	if _, exists := got["body"]; exists {
		t.Fatalf("unchanged rich native note must not be rewritten: %+v", got)
	}
}

func TestGraphTaskHTMLNoteRead(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOut(w, http.StatusOK, `{"value":[{"id":"task1","title":"Native details","body":{"contentType":"html","content":"<p>First &amp; second</p><p>Next<br>line</p>"},"dueDateTime":{"dateTime":"2026-10-19T12:00:00","timeZone":"UTC"}}]}`)
	})
	tasks, err := f.provider().ListTasks(context.Background(), "list1")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("ListTasks = %+v, %v", tasks, err)
	}
	if tasks[0].Body != "First & second\nNext\nline" {
		t.Fatalf("task plain note = %q", tasks[0].Body)
	}
	if tasks[0].Due == nil || tasks[0].Due.Hour() != 12 {
		t.Fatalf("existing task due instant changed: %+v", tasks[0])
	}
}

func TestGraphTaskUpdateClearsNullableDetails(t *testing.T) {
	f := newTaskUpdateFake(t)
	if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", Title: "Details cleared", Status: "needs-action", FullUpdate: true}); err != nil {
		t.Fatal(err)
	}
	got := bodyJSON(t, f.last().Body)
	for _, key := range []string{"dueDateTime", "startDateTime", "completedDateTime", "reminderDateTime"} {
		if value, exists := got[key]; !exists || value != nil {
			t.Errorf("clear %s = %#v, present %v", key, value, exists)
		}
	}
	if got["isReminderOn"] != false {
		t.Errorf("reminder not disabled: %+v", got)
	}
	body, ok := got["body"].(map[string]any)
	if !ok || body["content"] != "" || body["contentType"] != "html" {
		t.Errorf("empty note not explicitly cleared: %+v", got)
	}
}

func TestGraphTaskNoteWriteEscapesHTML(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		jsonOut(w, http.StatusCreated, `{"id":"created"}`)
	})
	if _, err := f.provider().CreateTask(context.Background(), bridge.Task{ListID: "list1", Title: "Plain note", Body: "A < B & C\r\nNext"}); err != nil {
		t.Fatal(err)
	}
	got, _ := bodyJSON(t, f.last().Body)["body"].(map[string]any)
	if got["contentType"] != "html" || got["content"] != "A &lt; B &amp; C<br>Next" {
		t.Fatalf("safe note payload = %+v", got)
	}
	for _, key := range []string{"dueDateTime", "startDateTime", "completedDateTime", "reminderDateTime"} {
		if _, exists := bodyJSON(t, f.last().Body)[key]; exists {
			t.Errorf("create must omit unset %s", key)
		}
	}
}
