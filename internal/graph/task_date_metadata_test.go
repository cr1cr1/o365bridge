package graph_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestGraphTaskDateOnlyCreateSurvivesFreshProvider(t *testing.T) {
	for _, uid := range []string{"client-task", ""} {
		t.Run(uid, func(t *testing.T) {
			var stored map[string]any
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
				if r.Method == http.MethodPost {
					stored = bodyJSON(t, body)
					stored["id"] = "cloud-task"
					jsonOut(w, http.StatusCreated, `{"id":"cloud-task"}`)
					return
				}
				data, _ := json.Marshal(map[string]any{"value": []any{stored}})
				jsonOut(w, http.StatusOK, string(data))
			})
			day := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
			created, err := f.provider().CreateTask(context.Background(), bridge.Task{ListID: "list1", UID: uid, Start: &day, Due: &day, StartDateOnly: true, DueDateOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			extensions, _ := stored["extensions"].([]any)
			if len(extensions) != 1 {
				t.Fatalf("DATE flags must share the atomic task extension create: %+v", stored)
			}
			extension, _ := extensions[0].(map[string]any)
			if extension["extensionName"] != "com.o365bridge.task" || extension["startDateOnly"] != true || extension["dueDateOnly"] != true || (uid != "" && extension["uid"] != uid) {
				t.Fatalf("date metadata extension = %+v", extension)
			}
			if !created.StartDateOnly || !created.DueDateOnly || len(f.reqs) != 1 {
				t.Fatalf("create lost date flags or used fallible follow-up: task=%+v requests=%+v", created, f.reqs)
			}
			tasks, err := f.provider().ListTasks(context.Background(), "list1")
			if err != nil || len(tasks) != 1 || !tasks[0].StartDateOnly || !tasks[0].DueDateOnly || tasks[0].Start == nil || tasks[0].Due == nil {
				t.Fatalf("fresh provider date metadata = %+v, %v", tasks, err)
			}
			wantUID := uid
			if wantUID == "" {
				wantUID = "cloud-task"
			}
			if tasks[0].UID != wantUID {
				t.Fatalf("DATE metadata changed stable UID: %+v", tasks[0])
			}
		})
	}
}

func TestGraphTaskDateOnlyUpdateUsesScopedExtensionMethods(t *testing.T) {
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                     string
		oldExtension             bool
		oldStart, oldDue         bool
		full, startOnly, dueOnly bool
		setStart, setDue         bool
		wantMethod               string
		wantStart, wantDue       bool
	}{
		{"clear flags", true, true, true, true, false, false, false, false, http.MethodPatch, false, false},
		{"existing unchanged", true, false, true, true, false, true, false, true, "", false, true},
		{"native unchanged", false, false, false, true, false, false, false, false, "", false, false},
		{"native DATE", false, false, false, true, false, true, false, true, http.MethodPost, false, true},
		{"partial DATE preserves other flag", true, false, true, false, true, false, true, false, http.MethodPatch, true, true},
		{"partial DATETIME clears due flag", true, true, true, false, false, false, false, true, http.MethodPatch, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const taskPath = "/v1.0/me/todo/lists/list1/tasks/task1"
			var metadata map[string]any
			if tc.oldExtension {
				metadata = map[string]any{"extensionName": "com.o365bridge.task", "uid": "original-client", "startDateOnly": tc.oldStart, "dueDateOnly": tc.oldDue}
			}
			var nativePatches, metadataWrites int
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == taskPath:
					if r.URL.Query().Get("$expand") != "extensions($filter=id eq 'com.o365bridge.task')" {
						t.Errorf("date metadata must use the known task extension: %s", r.URL.RawQuery)
					}
					current := map[string]any{"id": "task1"}
					if metadata != nil {
						current["extensions"] = []any{metadata}
					}
					data, _ := json.Marshal(current)
					jsonOut(w, http.StatusOK, string(data))
				case r.Method == http.MethodPatch && r.URL.Path == taskPath:
					nativePatches++
					if _, exists := bodyJSON(t, body)["extensions"]; exists {
						t.Error("task PATCH must not attempt unsupported nested extension update")
					}
					w.WriteHeader(http.StatusNoContent)
				case r.Method == tc.wantMethod && (r.URL.Path == taskPath+"/extensions/com.o365bridge.task" || r.URL.Path == taskPath+"/extensions"):
					metadataWrites++
					if nativePatches != 1 {
						t.Error("extension metadata must follow the successful native task update")
					}
					patch := bodyJSON(t, body)
					if patch["extensionName"] != "com.o365bridge.task" || patch["startDateOnly"] != tc.wantStart || patch["dueDateOnly"] != tc.wantDue {
						t.Errorf("date flag write = %+v", patch)
					}
					if tc.oldExtension {
						if _, exists := patch["uid"]; exists {
							t.Error("metadata PATCH must preserve existing stable UID by omission")
						}
					} else if patch["uid"] != "incoming-client" {
						t.Error("new metadata extension must preserve supplied client UID")
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected task/date metadata request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			task := bridge.Task{ID: "task1", ListID: "list1", UID: "incoming-client", FullUpdate: tc.full, StartDateOnly: tc.startOnly, DueDateOnly: tc.dueOnly}
			if tc.setStart {
				task.Start = &day
			}
			if tc.setDue {
				task.Due = &day
			}
			if err := f.provider().UpdateTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			wantWrites := 0
			if tc.wantMethod != "" {
				wantWrites = 1
			}
			if nativePatches != 1 || metadataWrites != wantWrites || len(f.reqs) != 2+wantWrites {
				t.Fatalf("date metadata requests = %+v", f.reqs)
			}
		})
	}
}

func TestGraphTaskDateMetadataFailurePropagatesWithoutTaskRecreation(t *testing.T) {
	const taskPath = "/v1.0/me/todo/lists/list1/tasks/task1"
	var taskPatches, extensionPatches int
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == taskPath:
			jsonOut(w, http.StatusOK, `{"id":"task1","extensions":[{"extensionName":"com.o365bridge.task","uid":"stable","dueDateOnly":true}]}`)
		case r.Method == http.MethodPatch && r.URL.Path == taskPath:
			taskPatches++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && r.URL.Path == taskPath+"/extensions/com.o365bridge.task":
			extensionPatches++
			if extensionPatches == 1 {
				jsonOut(w, http.StatusServiceUnavailable, `{"error":{"code":"ServiceUnavailable","message":"metadata write failed"}}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("retry must never create another task: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	task := bridge.Task{ID: "task1", ListID: "list1", FullUpdate: true}
	if err := f.provider().UpdateTask(context.Background(), task); err == nil {
		t.Fatal("successful native PATCH followed by failed metadata write must return error")
	}
	if err := f.provider().UpdateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if taskPatches != 2 || extensionPatches != 2 {
		t.Fatalf("retry calls task=%d metadata=%d", taskPatches, extensionPatches)
	}
}

func TestGraphTaskDateMetadataPreflightFailurePreventsNativeWrite(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method != http.MethodGet {
			t.Errorf("failed preflight must prevent writes: %s %s", r.Method, r.URL.Path)
		}
		jsonOut(w, http.StatusServiceUnavailable, `{"error":{"code":"ServiceUnavailable","message":"metadata read failed"}}`)
	})
	if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", FullUpdate: true}); err == nil {
		t.Fatal("date metadata preflight failure was ignored")
	}
	if len(f.reqs) != 1 {
		t.Errorf("preflight failure made extra requests: %+v", f.reqs)
	}
}

func TestGraphTaskDateOnlyReadUsesMetadataOrLiteralNotTimeHeuristics(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		jsonOut(w, http.StatusOK, `{"value":[
			{"id":"native-midnight","dueDateTime":{"dateTime":"2026-10-19T00:00:00","timeZone":"UTC"}},
			{"id":"native-noon","dueDateTime":{"dateTime":"2026-10-19T12:00:00","timeZone":"UTC"}},
			{"id":"literal","dueDateTime":{"dateTime":"2026-10-19","timeZone":"UTC"}},
			{"id":"ours","dueDateTime":{"dateTime":"2026-10-19T00:00:00","timeZone":"UTC"},"extensions":[{"extensionName":"com.o365bridge.task","dueDateOnly":true}]},
			{"id":"foreign","dueDateTime":{"dateTime":"2026-10-19T00:00:00","timeZone":"UTC"},"extensions":[{"extensionName":"com.example.foreign","dueDateOnly":true}]}
		]}`)
	})
	tasks, err := f.provider().ListTasks(context.Background(), "list1")
	if err != nil || len(tasks) != 5 {
		t.Fatalf("task dates = %+v, %v", tasks, err)
	}
	for i, want := range []bool{false, false, true, true, false} {
		if tasks[i].DueDateOnly != want {
			t.Errorf("task %s DATE=%v want %v", tasks[i].ID, tasks[i].DueDateOnly, want)
		}
	}
}
