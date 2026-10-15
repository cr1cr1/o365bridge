package owa

import (
	"context"
	"fmt"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func taskReadbackItem(id, uid string) map[string]any {
	item := map[string]any{"__type": "Task:#Exchange", "ItemId": map[string]any{"Id": id}, "Subject": "Do it", "Body": map[string]any{"Value": "Task details"}, "DueDate": "2026-10-15T00:00:00Z", "StartDate": "2026-10-14T00:00:00Z", "Status": "InProgress", "PercentComplete": 50, "Importance": "High"}
	if uid != "" {
		item["ExtendedProperty"] = []any{map[string]any{"ExtendedFieldURI": map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertyName": "O365BridgeTaskUID", "PropertyType": "String"}, "Value": uid}}
	}
	return item
}

func TestOWATasksHydrateSparseDiscovery(t *testing.T) {
	f, p := newFake(t, func(action string, req map[string]any) any {
		switch action {
		case "FindItem":
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": map[string]any{"Id": "task-one"}}, map[string]any{"ItemId": map[string]any{"Id": "task-two"}}}, "TotalItemsInView": 2}})
		case "GetItem":
			return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Items": []any{taskReadbackItem("task-one", "client-uid")}}, map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Items": []any{taskReadbackItem("task-two", "")}}}}})
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	tasks, err := p.ListTasks(context.Background(), "tasks")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("ListTasks = %+v, %v", tasks, err)
	}
	for i, task := range tasks {
		if task.Title != "Do it" || task.Body != "Task details" || task.Due == nil || task.Start == nil || task.Priority != 1 || task.Status != "in-process" {
			t.Fatalf("task %d not hydrated: %+v", i, task)
		}
	}
	if tasks[0].UID != "client-uid" || tasks[1].UID != "task-two" {
		t.Fatalf("durable and legacy task identities = %+v", tasks)
	}
	if _, ok := asMap(f.last("FindItem").Body["Body"])["ShapeName"]; ok {
		t.Fatal("tasks must not use mail list shape")
	}
	props := arr(f.last("GetItem").Body, "Body", "ItemShape", "AdditionalProperties")
	fields := map[string]bool{}
	var foundUID, foundDeadOccurrence bool
	for _, prop := range props {
		fields[str(prop, "FieldURI")] = true
		if str(prop, "__type") == "ExtendedPropertyUri:#Exchange" && str(prop, "DistinguishedPropertySetId") == "Task" && num(prop, "PropertyId") == 0x8109 && str(prop, "PropertyType") == "Boolean" {
			foundDeadOccurrence = true
		}
		if str(prop, "__type") == "ExtendedPropertyUri:#Exchange" && str(prop, "DistinguishedPropertySetId") == "PublicStrings" && str(prop, "PropertyName") == "O365BridgeTaskUID" && str(prop, "PropertyType") == "String" {
			foundUID = true
		}
	}
	for _, field := range []string{"item:Subject", "item:Body", "item:TextBody", "item:Importance", "item:Categories", "item:Attachments", "item:HasAttachments", "task:DueDate", "task:StartDate", "task:CompleteDate", "task:PercentComplete", "task:Status", "task:IsRecurring", "item:ReminderDueBy", "item:ReminderIsSet", "task:Recurrence"} {
		if !fields[field] {
			t.Errorf("missing task property %s", field)
		}
	}
	if !foundUID {
		t.Error("task readback did not request named String UID property")
	}
	if !foundDeadOccurrence {
		t.Error("task readback did not request native Boolean TaskDeadOccurrence property")
	}
}

func TestOWATasksPagedDiscoveryAndBoundedHydration(t *testing.T) {
	var findCalls, getCalls int
	_, p := newFake(t, func(action string, req map[string]any) any {
		switch action {
		case "FindItem":
			findCalls++
			offset := int(num(req, "Body", "Paging", "Offset"))
			var rows []any
			for i := offset; i < min(offset+1000, 1001); i++ {
				rows = append(rows, map[string]any{"ItemId": map[string]any{"Id": fmt.Sprintf("t%d", i)}})
			}
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": rows, "TotalItemsInView": 1001, "IndexedPagingOffset": offset + len(rows), "IncludesLastItemInRange": offset+len(rows) == 1001}})
		case "GetItem":
			getCalls++
			ids := arr(req, "Body", "ItemIds")
			if len(ids) > 100 {
				t.Errorf("GetItem batch = %d, want <=100", len(ids))
			}
			var items []any
			for _, id := range ids {
				items = append(items, taskReadbackItem(str(id, "Id"), ""))
			}
			return itemResp(items...)
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	tasks, err := p.ListTasks(context.Background(), "tasks")
	if err != nil || len(tasks) != 1001 || findCalls != 2 || getCalls != 11 {
		t.Fatalf("tasks=%d err=%v discovery=%d hydration=%d", len(tasks), err, findCalls, getCalls)
	}
}

func TestOWATasksRejectIncompleteReadback(t *testing.T) {
	for _, mode := range []string{"missing folder", "empty page", "missing id", "duplicate id", "missing item", "unexpected item", "duplicate item", "wrong type", "item error", "paging loop"} {
		t.Run(mode, func(t *testing.T) {
			_, p := newFake(t, func(action string, req map[string]any) any {
				if action == "FindItem" {
					if mode == "missing folder" {
						return okBody(nil)
					}
					rows := []any{map[string]any{"ItemId": map[string]any{"Id": "one"}}, map[string]any{"ItemId": map[string]any{"Id": "two"}}}
					switch mode {
					case "empty page":
						rows = nil
					case "missing id":
						rows[1] = map[string]any{}
					case "duplicate id":
						rows[1] = rows[0]
					}
					root := map[string]any{"Items": rows, "TotalItemsInView": 2}
					if mode == "paging loop" {
						root["TotalItemsInView"] = 10
						root["IndexedPagingOffset"] = 1
					}
					return okBody(map[string]any{"RootFolder": root})
				}
				switch mode {
				case "missing item":
					return itemResp(taskReadbackItem("one", ""))
				case "unexpected item":
					return itemResp(taskReadbackItem("one", ""), taskReadbackItem("other", ""))
				case "duplicate item":
					return itemResp(taskReadbackItem("one", ""), taskReadbackItem("one", ""))
				case "wrong type":
					item := taskReadbackItem("two", "")
					item["__type"] = "CalendarItem:#Exchange"
					return itemResp(taskReadbackItem("one", ""), item)
				case "item error":
					return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{map[string]any{"ResponseClass": "Success", "Items": []any{taskReadbackItem("one", "")}}, map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorItemNotFound"}}}})
				default:
					return itemResp(taskReadbackItem("one", ""), taskReadbackItem("two", ""))
				}
			})
			tasks, err := p.ListTasks(context.Background(), "tasks")
			if err == nil || len(tasks) != 0 {
				t.Fatalf("partial success: tasks=%+v err=%v", tasks, err)
			}
		})
	}
}

func TestOWATasksNativeUIDSurvivesProviderRestart(t *testing.T) {
	var stored map[string]any
	f, p := newFake(t, func(action string, req map[string]any) any {
		switch action {
		case "CreateItem":
			stored = asMap(arr(req, "Body", "Items")[0])
			stored["ItemId"] = map[string]any{"Id": "cloud-id"}
			return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud-id"}})
		case "FindItem":
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": map[string]any{"Id": "cloud-id"}}}, "TotalItemsInView": 1}})
		case "GetItem":
			return itemResp(stored)
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	created, err := p.CreateTask(context.Background(), bridge.Task{ListID: "tasks", UID: "client-uid", Title: "Do it"})
	if err != nil || created.ID != "cloud-id" {
		t.Fatalf("CreateTask = %+v, %v", created, err)
	}
	if str(stored, "__type") != "Task:#Exchange" {
		t.Fatalf("not a native task: %+v", stored)
	}
	if _, exists := stored["UID"]; exists {
		t.Fatal("Task has no native UID property")
	}
	ep := arr(stored, "ExtendedProperty")
	var uidProperty any
	for _, property := range ep {
		if str(property, "ExtendedFieldURI", "PropertyName") == "O365BridgeTaskUID" {
			uidProperty = property
		}
	}
	if str(uidProperty, "ExtendedFieldURI", "DistinguishedPropertySetId") != "PublicStrings" || str(uidProperty, "ExtendedFieldURI", "PropertyType") != "String" || str(uidProperty, "Value") != "client-uid" {
		t.Fatalf("UID not saved upstream: %+v", ep)
	}
	restarted := Factory{BaseURL: p.(*provider).base}.New("user@example.com", bridge.Session{OWAAuthHeader: "Bearer fake"})
	tasks, err := restarted.ListTasks(context.Background(), "tasks")
	if err != nil || len(tasks) != 1 || tasks[0].UID != "client-uid" {
		t.Fatalf("restart identity = %+v, %v", tasks, err)
	}
	if len(arr(f.last("GetItem").Body, "Body", "ItemIds")) != 1 {
		t.Fatal("missing native task readback")
	}
}

func TestOWATasksNativeUIDReadbackNormalizesNamespace(t *testing.T) {
	for _, namespace := range []string{"00020329-0000-0000-C000-000000000046", "00020329-0000-0000-c000-000000000046"} {
		t.Run(namespace, func(t *testing.T) {
			var stored map[string]any
			_, p := newFake(t, func(action string, req map[string]any) any {
				switch action {
				case "CreateItem":
					stored = asMap(arr(req, "Body", "Items")[0])
					stored["ItemId"] = map[string]any{"Id": "cloud"}
					return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
				case "FindItem":
					return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": map[string]any{"Id": "cloud"}}}, "TotalItemsInView": 1}})
				case "GetItem":
					field := asMap(dig(arr(stored, "ExtendedProperty")[0], "ExtendedFieldURI"))
					delete(field, "DistinguishedPropertySetId")
					field["PropertySetId"] = namespace
					return itemResp(stored)
				default:
					t.Errorf("unexpected action %s", action)
					return okBody(nil)
				}
			})
			if _, err := p.CreateTask(context.Background(), bridge.Task{UID: "client-uid", Title: "Do it"}); err != nil {
				t.Fatal(err)
			}
			fresh := Factory{BaseURL: p.(*provider).base}.New("user@example.com", bridge.Session{OWAAuthHeader: "Bearer fake"})
			tasks, err := fresh.ListTasks(context.Background(), "tasks")
			if err != nil || len(tasks) != 1 || tasks[0].UID != "client-uid" {
				t.Fatalf("normalized durable UID = %+v, %v", tasks, err)
			}
		})
	}
}

func TestOWATasksUIDPropertyRejectsOtherNamespaces(t *testing.T) {
	item := taskReadbackItem("cloud", "wrong-uid")
	field := asMap(dig(arr(item, "ExtendedProperty")[0], "ExtendedFieldURI"))
	delete(field, "DistinguishedPropertySetId")
	field["PropertySetId"] = "00062003-0000-0000-C000-000000000046"
	if task := taskFromItem(item, "tasks"); task.UID != "cloud" {
		t.Fatalf("accepted unrelated namespace: %+v", task)
	}
}

func TestOWATasksCreateRequiresCloudID(t *testing.T) {
	for _, response := range []any{itemResp(map[string]any{}), okBody(nil)} {
		_, p := newFake(t, func(string, map[string]any) any { return response })
		if task, err := p.CreateTask(context.Background(), bridge.Task{ID: "stale-input-id", UID: "client-uid"}); err == nil {
			t.Fatalf("accepted unusable task identity: %+v", task)
		}
	}
}
