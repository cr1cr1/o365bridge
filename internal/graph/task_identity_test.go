package graph_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestTaskListPreservesCloudAndClientUIDs(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1.0/me/todo/lists/list1/tasks" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		jsonOut(w, http.StatusOK, `{"value":[
			{"id":"native-one","title":"Same title"},
			{"id":"cloud-one","title":"Same title","extensions":[{"extensionName":"com.o365bridge.task","uid":"client-one"}]},
			{"id":"cloud-two","title":"Same title","extensions":[{"extensionName":"com.o365bridge.task","uid":"client-two"}]},
			{"id":"other-extension","title":"Same title","extensions":[{"extensionName":"com.example.other","uid":"not-our-uid"}]},
			{"id":"empty-uid","title":"Same title","extensions":[{"extensionName":"com.o365bridge.task","uid":""}]}
		]}`)
	})
	tasks, err := f.provider().ListTasks(context.Background(), "list1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"native-one": "native-one", "cloud-one": "client-one", "cloud-two": "client-two",
		"other-extension": "other-extension", "empty-uid": "empty-uid",
	}
	if len(tasks) != len(want) {
		t.Fatalf("tasks = %+v", tasks)
	}
	for _, task := range tasks {
		if task.UID != want[task.ID] || task.Title != "Same title" || task.ListID != "list1" {
			t.Errorf("task = %+v, want UID %q and original title/list", task, want[task.ID])
		}
	}
	query, err := url.ParseQuery(f.last().Query)
	if err != nil {
		t.Fatal(err)
	}
	if got := query.Get("$expand"); got != "extensions($filter=id eq 'com.o365bridge.task')" {
		t.Errorf("extension expansion = %q", got)
	}
	if query.Get("$filter") != "" {
		t.Errorf("native tasks must not be excluded by a collection filter: %v", query)
	}
}

func TestTaskClientUIDSurvivesFreshProviderLifecycle(t *testing.T) {
	var stored map[string]any
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		base := "/v1.0/me/todo/lists/list1/tasks"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == base:
			stored = bodyJSON(t, body)
			if _, ok := stored["uid"]; ok {
				t.Error("client UID must not be sent as an unsupported native task property")
			}
			extensions, _ := stored["extensions"].([]any)
			if len(extensions) != 1 {
				t.Errorf("POST must persist UID atomically in one extension: %v", stored)
			} else {
				extension, _ := extensions[0].(map[string]any)
				if extension["@odata.type"] != "microsoft.graph.openTypeExtension" || extension["extensionName"] != "com.o365bridge.task" || extension["uid"] != "client-one" {
					t.Errorf("invalid open extension schema: %v", extension)
				}
			}
			stored["id"] = "cloud-one"
			// Graph's task create response need not expand extensions.
			jsonOut(w, http.StatusCreated, `{"id":"cloud-one","title":"Same title","status":"notStarted"}`)
		case r.Method == http.MethodGet && r.URL.Path == base:
			var tasks []any
			if stored != nil {
				readback := make(map[string]any)
				for key, value := range stored {
					if key != "extensions" || r.URL.Query().Get("$expand") == "extensions($filter=id eq 'com.o365bridge.task')" {
						readback[key] = value
					}
				}
				tasks = append(tasks, readback)
			}
			data, _ := json.Marshal(map[string]any{"value": tasks})
			jsonOut(w, http.StatusOK, string(data))
		case r.Method == http.MethodPatch && r.URL.Path == base+"/cloud-one":
			patch := bodyJSON(t, body)
			if _, ok := patch["extensions"]; ok {
				t.Errorf("ordinary task update must not rewrite immutable UID metadata: %v", patch)
			}
			for key, value := range patch {
				stored[key] = value
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == base+"/cloud-one":
			stored = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	})
	ctx := context.Background()
	created, err := f.provider().CreateTask(ctx, bridge.Task{ListID: "list1", UID: "client-one", Title: "Same title"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "cloud-one" || created.UID != "client-one" {
		t.Errorf("created = %+v, want cloud ID and client UID", created)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("create must be one atomic request, got %d", len(f.reqs))
	}
	// No provider-local state is available to this fresh instance.
	fresh := f.provider()
	tasks, err := fresh.ListTasks(ctx, "list1")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("fresh readback = %+v, %v", tasks, err)
	}
	if tasks[0].ID != "cloud-one" || tasks[0].UID != "client-one" {
		t.Errorf("fresh readback lost identity: %+v", tasks[0])
	}
	tasks[0].Title = "Updated title"
	if err := fresh.UpdateTask(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	tasks, err = f.provider().ListTasks(ctx, "list1")
	if err != nil || len(tasks) != 1 || tasks[0].UID != "client-one" || tasks[0].Title != "Updated title" {
		t.Fatalf("updated readback = %+v, %v", tasks, err)
	}
	if err := fresh.DeleteTask(ctx, "list1", tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	tasks, err = f.provider().ListTasks(ctx, "list1")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("deleted task still listed = %+v, %v", tasks, err)
	}
}

func TestCreateTaskUsesCloudUIDWithoutClientUID(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, body []byte) {
		if _, ok := bodyJSON(t, body)["extensions"]; ok {
			t.Error("task without client UID must not gain empty UID metadata")
		}
		jsonOut(w, http.StatusCreated, `{"id":"native-one","title":"Native task"}`)
	})
	created, err := f.provider().CreateTask(context.Background(), bridge.Task{ListID: "list1", Title: "Native task"})
	if err != nil || created.ID != "native-one" || created.UID != "native-one" {
		t.Fatalf("created = %+v, %v; want cloud ID as fallback UID", created, err)
	}
}

func TestCreateTaskRejectsResponseWithoutID(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		jsonOut(w, http.StatusCreated, `{"title":"Task without cloud ID"}`)
	})
	created, err := f.provider().CreateTask(context.Background(), bridge.Task{ID: "not-created", ListID: "list1", UID: "client-one", Title: "Task"})
	if err == nil || !strings.Contains(err.Error(), "id") || created.ID != "" {
		t.Fatalf("missing cloud ID must fail, got task=%+v err=%v", created, err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("malformed success must not retry creation: %d requests", len(f.reqs))
	}
}
