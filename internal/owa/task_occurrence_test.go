package owa

import (
	"context"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func taskOccurrenceItem(id, uid string, dead any, recurring bool) map[string]any {
	item := taskReadbackItem(id, uid)
	item["StartDate"], item["DueDate"] = "2026-10-15T00:00:00Z", "2026-10-15T00:00:00Z"
	item["Recurrence"] = map[string]any{
		"__type":            "TaskRecurrenceType:#Exchange",
		"RecurrencePattern": map[string]any{"__type": "DailyRecurrence:#Exchange", "Interval": 1},
		"RecurrenceRange":   map[string]any{"__type": "NoEndRecurrence:#Exchange", "StartDate": "2026-10-14T00:00:00Z"},
	}
	item["ExtendedProperty"] = append(arr(item, "ExtendedProperty"), map[string]any{
		"ExtendedFieldURI": map[string]any{"DistinguishedPropertySetId": "Task", "PropertyId": 33033, "PropertyType": "Boolean"}, "Value": dead,
	})
	if recurring {
		item["IsRecurring"], item["Status"], item["PercentComplete"] = true, "NotStarted", 0
		item["StartDate"], item["DueDate"] = "2026-10-16T00:00:00Z", "2026-10-16T00:00:00Z"
	}
	return item
}

func taskOccurrenceProvider(t *testing.T, pages [][]map[string]any, reverseHydration bool) bridge.Provider {
	t.Helper()
	items := map[string]map[string]any{}
	var total int
	for _, page := range pages {
		for _, item := range page {
			items[str(item, "ItemId", "Id")] = item
			total++
		}
	}
	_, p := newFake(t, func(action string, req map[string]any) any {
		switch action {
		case "FindItem":
			offset := int(num(req, "Body", "Paging", "Offset"))
			start := 0
			for _, page := range pages {
				if start == offset {
					var rows []any
					for _, item := range page {
						rows = append(rows, map[string]any{"ItemId": item["ItemId"]})
					}
					next := offset + len(page)
					return okBody(map[string]any{"RootFolder": map[string]any{"Items": rows, "TotalItemsInView": total, "IndexedPagingOffset": next, "IncludesLastItemInRange": next == total}})
				}
				start += len(page)
			}
			t.Errorf("unexpected discovery offset %d", offset)
		case "GetItem":
			var full []any
			for _, id := range arr(req, "Body", "ItemIds") {
				full = append(full, items[str(id, "Id")])
			}
			if reverseHydration {
				for i, j := 0, len(full)-1; i < j; i, j = i+1, j-1 {
					full[i], full[j] = full[j], full[i]
				}
			}
			return itemResp(full...)
		default:
			t.Errorf("read must not mutate cloud: unexpected action %s", action)
		}
		return okBody(nil)
	})
	return p
}

func TestOWATasksDeadOccurrenceNativeFlag(t *testing.T) {
	for _, tc := range []struct {
		name                string
		value               any
		field               map[string]any
		wantDead, wantKnown bool
	}{
		{"string true", "true", nil, true, true},
		{"raw true", true, nil, true, true},
		{"native one", "1", nil, true, true},
		{"string false", "false", nil, false, true},
		{"raw false", false, nil, false, true},
		{"native zero", "0", nil, false, true},
		{"malformed", "not-a-bool", nil, false, false},
		{"missing value", nil, nil, false, false},
		{"numeric value", 1, nil, false, false},
		{"whitespace", " true ", nil, false, false},
		{"uppercase", "TRUE", nil, false, false},
		{"guid", true, map[string]any{"PropertySetId": "00062003-0000-0000-C000-000000000046", "PropertyId": 33033, "PropertyType": "Boolean"}, true, true},
		{"lowercase guid", true, map[string]any{"PropertySetId": "00062003-0000-0000-c000-000000000046", "PropertyId": float64(33033), "PropertyType": "Boolean"}, true, true},
		{"other namespace", true, map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertyId": 33033, "PropertyType": "Boolean"}, false, false},
		{"other guid", true, map[string]any{"PropertySetId": "00020329-0000-0000-C000-000000000046", "PropertyId": 33033, "PropertyType": "Boolean"}, false, false},
		{"missing namespace", true, map[string]any{"PropertyId": 33033, "PropertyType": "Boolean"}, false, false},
		{"wrong id", true, map[string]any{"DistinguishedPropertySetId": "Task", "PropertyId": 33034, "PropertyType": "Boolean"}, false, false},
		{"fractional id", true, map[string]any{"DistinguishedPropertySetId": "Task", "PropertyId": 33033.5, "PropertyType": "Boolean"}, false, false},
		{"conflicting guid", "true", map[string]any{"DistinguishedPropertySetId": "Task", "PropertySetId": "00020329-0000-0000-C000-000000000046", "PropertyId": 33033, "PropertyType": "Boolean"}, false, false},
		{"conflicting distinguished set", "true", map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertySetId": "00062003-0000-0000-C000-000000000046", "PropertyId": 33033, "PropertyType": "Boolean"}, false, false},
		{"wrong type", "true", map[string]any{"DistinguishedPropertySetId": "Task", "PropertyId": 33033, "PropertyType": "String"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := taskOccurrenceItem("final-native", "client-uid", tc.value, false)
			if tc.field != nil {
				asMap(arr(item, "ExtendedProperty")[1])["ExtendedFieldURI"] = tc.field
			}
			if dead, known := taskDeadOccurrence(item); dead != tc.wantDead || known != tc.wantKnown {
				t.Fatalf("native flag = (%v, %v), want (%v, %v)", dead, known, tc.wantDead, tc.wantKnown)
			}
			task := taskFromItem(item, "tasks")
			if (task.Recur == "") != tc.wantDead || task.UID != "client-uid" {
				t.Fatalf("dead=%v task=%+v", tc.wantDead, task)
			}
			if tc.wantDead && (task.RecurStart != nil || task.RecurTimeZone != "" || task.RecurUnsupported) {
				t.Fatalf("dead task retains active recurrence metadata: %+v", task)
			}
		})
	}
}

func TestOWATasksOccurrenceUIDStableAcrossPagesOrderAndRestart(t *testing.T) {
	detached := taskOccurrenceItem("detached-native", "client-uid", "true", false)
	generator := taskOccurrenceItem("original-native", "client-uid", "false", true)
	second := taskOccurrenceItem("second-detached", "client-uid", true, false)
	// Deliberately contradict date/status ordering: native flags alone prove identity.
	second["Subject"], second["Status"], second["PercentComplete"] = "Another title", "NotStarted", 0
	second["StartDate"], second["DueDate"] = "2026-10-20T00:00:00Z", "2026-10-20T00:00:00Z"
	for _, pages := range [][][]map[string]any{
		{{detached, generator, second}}, {{second, generator, detached}}, {{detached}, {generator, second}}, {{generator}, {second}, {detached}},
	} {
		for _, reverse := range []bool{false, true} {
			p := taskOccurrenceProvider(t, pages, reverse)
			fresh := Factory{BaseURL: p.(*provider).base}.New("user@example.com", bridge.Session{OWAAuthHeader: "Bearer fake"})
			for _, reader := range []bridge.Provider{p, p, fresh} {
				tasks, err := reader.ListTasks(context.Background(), "tasks")
				if err != nil || len(tasks) != 3 {
					t.Fatalf("ListTasks = %+v, %v", tasks, err)
				}
				for _, task := range tasks {
					want := task.ID
					if task.ID == "original-native" {
						want = "client-uid"
					}
					if task.UID != want {
						t.Fatalf("unstable projection: %+v, want UID %q", task, want)
					}
				}
			}
		}
	}
}

func TestOWATasksSingleDeadFinalAndStandaloneKeepClientUID(t *testing.T) {
	final := taskOccurrenceItem("final-native", "final-client-uid", "true", false)
	final["Recurrence"] = "stale unsupported recurrence"
	standalone := taskReadbackItem("standalone-native", "standalone-client-uid")
	p := taskOccurrenceProvider(t, [][]map[string]any{{final, standalone}}, false)
	tasks, err := p.ListTasks(context.Background(), "tasks")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("ListTasks = %+v, %v", tasks, err)
	}
	if tasks[0].UID != "final-client-uid" || tasks[0].Recur != "" || tasks[0].RecurStart != nil || tasks[0].RecurTimeZone != "" || tasks[0].RecurUnsupported {
		t.Errorf("single native final instance changed identity or retained recurrence: %+v", tasks[0])
	}
	if tasks[1].UID != "standalone-client-uid" {
		t.Errorf("standalone identity changed: %+v", tasks[1])
	}
}

func TestOWATasksRejectAmbiguousOccurrenceUIDs(t *testing.T) {
	for _, mode := range []string{"zero generators", "multiple generators", "unknown sibling", "missing sibling flag", "wrong sibling namespace", "wrong sibling type", "unknown generator", "missing generator flag", "not recurring", "string recurring", "post-projection collision", "vacated stored UID collision"} {
		t.Run(mode, func(t *testing.T) {
			detached := taskOccurrenceItem("detached-native", "client-uid", "true", false)
			generator := taskOccurrenceItem("original-native", "client-uid", "false", true)
			rows := []map[string]any{detached, generator}
			switch mode {
			case "zero generators":
				asMap(arr(generator, "ExtendedProperty")[1])["Value"] = "true"
			case "multiple generators":
				detached["IsRecurring"] = true
				asMap(arr(detached, "ExtendedProperty")[1])["Value"] = "false"
			case "unknown sibling":
				asMap(arr(detached, "ExtendedProperty")[1])["Value"] = "garbage"
			case "missing sibling flag":
				detached["ExtendedProperty"] = arr(detached, "ExtendedProperty")[:1]
			case "wrong sibling namespace":
				asMap(dig(arr(detached, "ExtendedProperty")[1], "ExtendedFieldURI"))["DistinguishedPropertySetId"] = "PublicStrings"
			case "wrong sibling type":
				asMap(dig(arr(detached, "ExtendedProperty")[1], "ExtendedFieldURI"))["PropertyType"] = "String"
			case "unknown generator":
				asMap(arr(generator, "ExtendedProperty")[1])["Value"] = "garbage"
			case "missing generator flag":
				generator["ExtendedProperty"] = arr(generator, "ExtendedProperty")[:1]
			case "not recurring":
				delete(generator, "IsRecurring")
			case "string recurring":
				generator["IsRecurring"] = "true"
			case "post-projection collision":
				rows = append(rows, taskReadbackItem("other-native", "detached-native"))
			case "vacated stored UID collision":
				rows = append(rows, taskOccurrenceItem("other-detached", "detached-native", "true", false), taskOccurrenceItem("other-generator", "detached-native", "false", true))
			}
			p := taskOccurrenceProvider(t, [][]map[string]any{{rows[0]}, rows[1:]}, false)
			tasks, err := p.ListTasks(context.Background(), "tasks")
			if err == nil || len(tasks) != 0 || bridge.KindOf(err) != bridge.KindOther {
				t.Fatalf("ambiguous data must fail closed: tasks=%+v err=%v kind=%v", tasks, err, bridge.KindOf(err))
			}
		})
	}
}

func TestOWATasksDetachedOccurrenceLivePair(t *testing.T) {
	detached := taskOccurrenceItem("detached-native", "client-uid", "true", false)
	generator := taskOccurrenceItem("original-native", "client-uid", "false", true)
	p := taskOccurrenceProvider(t, [][]map[string]any{{detached, generator}}, false)
	tasks, err := p.ListTasks(context.Background(), "tasks")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("ListTasks = %+v, %v", tasks, err)
	}
	one, master := tasks[0], tasks[1]
	if one.UID != "detached-native" || master.UID != "client-uid" {
		t.Errorf("split identities: detached=%+v generator=%+v", one, master)
	}
	if one.Recur != "" || one.RecurStart != nil || one.RecurTimeZone != "" || one.RecurUnsupported {
		t.Errorf("detached copied recurrence must not be active: %+v", one)
	}
	if one.Start == nil || one.Start.Format("2006-01-02") != "2026-10-15" || one.Due == nil || one.Due.Format("2006-01-02") != "2026-10-15" || one.Body != "Task details" || one.Status != "in-process" || one.Completed != nil {
		t.Errorf("detached fields lost: %+v", one)
	}
	if master.Recur != "FREQ=DAILY" || master.RecurStart == nil || master.Status != "needs-action" || master.Completed != nil {
		t.Errorf("continuing generator changed: %+v", master)
	}
	for _, item := range []map[string]any{detached, generator} {
		if uid, _ := taskNamedString(item, taskUIDPropertyName); uid != "client-uid" || item["Recurrence"] == nil {
			t.Errorf("read mutated native metadata: %+v", item)
		}
	}
}
