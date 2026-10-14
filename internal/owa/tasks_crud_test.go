package owa

import (
	"context"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWATasksReadNativeStatus(t *testing.T) {
	for _, tc := range []struct {
		status   string
		complete bool
		want     string
	}{
		{"Completed", false, "completed"}, {"InProgress", false, "in-process"}, {"NotStarted", true, "completed"}, {"NotStarted", false, "needs-action"},
	} {
		t.Run(tc.status+tc.want, func(t *testing.T) {
			item := taskReadbackItem("cloud", "")
			item["Status"] = tc.status
			item["PercentComplete"] = 0
			item["IsComplete"] = tc.complete
			if got := taskFromItem(item, "tasks").Status; got != tc.want {
				t.Fatalf("native status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOWATasksUpdateFullRepresentation(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		task bridge.Task
		pct  int
	}{
		{"complete without timestamp", bridge.Task{ID: "cloud", UID: "client", Status: "completed", Priority: 9}, 100},
		{"complete with timestamp", bridge.Task{ID: "cloud", UID: "client", Completed: &now, Due: &now, Start: &now, Priority: 1}, 100},
		{"reopen and clear", bridge.Task{ID: "cloud", UID: "client", Status: "needs-action"}, 0},
		{"in progress", bridge.Task{ID: "cloud", UID: "client", Status: "in-process"}, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newFake(t, func(string, map[string]any) any {
				return okBody(map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError"})
			})
			if err := p.UpdateTask(context.Background(), tc.task); err != nil {
				t.Fatal(err)
			}
			changes := arr(f.last("UpdateItem").Body, "Body", "ItemChanges")
			if len(changes) != 1 || str(changes[0], "ItemId", "Id") != "cloud" {
				t.Fatalf("wrong update identity: %+v", changes)
			}
			fields := map[string]any{}
			deletes := map[string]bool{}
			var uid bool
			for _, change := range arr(changes[0], "Updates") {
				path := dig(change, "Path")
				key := str(path, "FieldURI")
				if str(change, "__type") == "DeleteItemField:#Exchange" {
					deletes[key] = true
					continue
				}
				item := dig(change, "Item")
				if str(item, "__type") != "Task:#Exchange" {
					t.Fatalf("task update contains non-task item: %+v", change)
				}
				if str(path, "__type") == "ExtendedPropertyUri:#Exchange" && str(path, "PropertyName") == "O365BridgeTaskUID" {
					ep := arr(item, "ExtendedProperty")
					uid = len(ep) == 1 && str(ep[0], "Value") == "client"
					continue
				}
				fields[key] = item
			}
			if int(num(fields["TaskPercentComplete"], "PercentComplete")) != tc.pct {
				t.Fatalf("completion status not written: %+v", fields)
			}
			if _, exists := fields["TaskStatus"]; exists {
				t.Fatal("unverified TaskStatus update enum must not be guessed")
			}

			if _, ok := fields["TaskPercentComplete"]; !ok {
				t.Fatal("percent complete missing")
			}
			if !uid {
				t.Fatal("client UID not persisted on update")
			}
			for _, key := range []string{"Subject", "Body", "Importance"} {
				if fields[key] == nil {
					t.Errorf("full representation missing %s", key)
				}
			}
			if str(fields["Body"], "Body", "BodyType") != "Text" || str(fields["Subject"], "Subject") != "" || str(fields["Body"], "Body", "Value") != "" {
				t.Fatalf("cannot clear title/body as plain text: %+v", fields)
			}
			wantImportance := "Normal"
			if tc.task.Priority == 1 {
				wantImportance = "High"
			}
			if tc.task.Priority == 9 {
				wantImportance = "Low"
			}
			if str(fields["Importance"], "Importance") != wantImportance {
				t.Fatalf("priority not written: %+v", fields["Importance"])
			}
			for _, date := range []struct {
				key     string
				present bool
			}{{"TaskDueDate", tc.task.Due != nil}, {"TaskStartDate", tc.task.Start != nil}, {"TaskCompleteDate", tc.task.Completed != nil}} {
				if date.present {
					if fields[date.key] == nil {
						t.Errorf("missing date %s", date.key)
					}
				} else if !deletes[date.key] {
					t.Errorf("missing date deletion %s", date.key)
				}
			}
		})
	}
}

func TestOWATasksCreatePlainDescription(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
	})
	if _, err := p.CreateTask(context.Background(), bridge.Task{Title: "Do it", Body: "literal <tag> & text"}); err != nil {
		t.Fatal(err)
	}
	item := arr(f.last("CreateItem").Body, "Body", "Items")[0]
	if str(item, "Body", "BodyType") != "Text" || str(item, "Body", "Value") != "literal <tag> & text" {
		t.Fatalf("description misclassified: %+v", item)
	}
}

func TestOWATaskListsKeepEstablishedPrimaryCollection(t *testing.T) {
	_, p := newFake(t, func(action string, _ map[string]any) any {
		if action != "FindFolder" {
			t.Errorf("unexpected action %s", action)
		}
		return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{
			map[string]any{"__type": "TasksFolder:#Exchange", "FolderId": map[string]any{"Id": "localized"}, "DisplayName": "Aufgaben", "FolderClass": "IPF.Task"},
			map[string]any{"__type": "Folder:#Exchange", "FolderId": map[string]any{"Id": "custom"}, "DisplayName": "Project backlog", "FolderClass": "IPF.Task"},
			map[string]any{"__type": "Folder:#Exchange", "FolderId": map[string]any{"Id": "mail"}, "DisplayName": "Tasks", "FolderClass": "IPF.Note"},
			map[string]any{"__type": "CalendarFolder:#Exchange", "FolderId": map[string]any{"Id": "calendar"}, "DisplayName": "Tasks", "FolderClass": "IPF.Appointment"},
		}}})
	})
	lists, err := p.ListTaskLists(context.Background())
	if err != nil || len(lists) != 1 || lists[0].ID != "tasks" || !lists[0].TasksOnly {
		t.Fatalf("established /tasks/ collection changed: lists=%+v err=%v", lists, err)
	}
}

func TestOWATasksDeleteNativeItem(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any {
		return okBody(map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError"})
	})
	if err := p.DeleteTask(context.Background(), "tasks", "cloud-id"); err != nil {
		t.Fatal(err)
	}
	req := f.last("DeleteItem").Body
	ids := arr(req, "Body", "ItemIds")
	if len(ids) != 1 || str(ids[0], "Id") != "cloud-id" || str(req, "Body", "DeleteType") != "MoveToDeletedItems" || str(req, "Body", "AffectedTaskOccurrences") != "AllOccurrences" {
		t.Fatalf("wrong native deletion: %+v", req)
	}
	if !boolean(req, "Body", "SuppressReadReceipts") {
		t.Fatal("incorrect receipt suppression property")
	}
}

func TestOWATasksRejectMissingMutationID(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any { return okBody(nil) })
	if err := p.UpdateTask(context.Background(), bridge.Task{}); err == nil {
		t.Fatal("UpdateTask accepted missing ID")
	}
	if err := p.DeleteTask(context.Background(), "tasks", ""); err == nil {
		t.Fatal("DeleteTask accepted missing ID")
	}
	if len(f.requests) != 0 {
		t.Fatal("sent mutation with no task identity")
	}
}
