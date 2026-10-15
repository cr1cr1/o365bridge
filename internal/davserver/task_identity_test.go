package davserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestDAVTasksWithMissingUIDKeepCloudIdentity(t *testing.T) {
	p := newFake()
	const first, second = "task/one+%2F", "task/two+%2F"
	for _, id := range []string{first, second} {
		p.tasks[id] = bridge.Task{ID: id, ListID: "tasks1", Title: "Same task", Status: "needs-action"}
	}
	h := newHandler(t, p)
	for _, id := range []string{first, second} {
		path := "/caldav/" + acct + "/tasks1/" + esc(id) + ".ics"
		rr := req(t, h, "GET", path, "", true)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "UID:"+id+"\r\n") || !strings.Contains(rr.Body.String(), "BEGIN:VTODO") || strings.Contains(rr.Body.String(), "BEGIN:VEVENT") {
			t.Fatalf("task fallback identity: %d %s", rr.Code, rr.Body.String())
		}
	}
	path := "/caldav/" + acct + "/tasks1/" + esc(first) + ".ics"
	data := req(t, h, "GET", path, "", true).Body.String()
	if rr := req(t, h, "PUT", path, strings.ReplaceAll(data, "Same task", "Only one updated"), true); rr.Code != http.StatusNoContent {
		t.Fatalf("update task: %d %s", rr.Code, rr.Body.String())
	}
	if p.tasks[first].Title != "Only one updated" || p.tasks[second].Title != "Same task" || len(p.tasks) != 2 || len(p.events) != 0 {
		t.Fatalf("wrong task mutation: %+v", p.tasks)
	}
	if rr := req(t, h, "DELETE", path, "", true); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	if len(p.tasks) != 1 || p.tasks[second].Title != "Same task" {
		t.Fatalf("deleted sibling: %+v", p.tasks)
	}
}

func TestVTODOFieldsAfterAlarmAreParsed(t *testing.T) {
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:task-alarm\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:Alarm only\r\nTRIGGER;VALUE=DATE-TIME:20260602T090000Z\r\nEND:VALARM\r\nSUMMARY:Task after alarm\r\nDUE:20260602T100000Z\r\nPRIORITY:1\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	events, tasks, err := parseICalendar([]byte(data))
	if err != nil || len(events) != 0 || len(tasks) != 1 {
		t.Fatalf("parse=%+v %+v %v", events, tasks, err)
	}
	if tasks[0].Title != "Task after alarm" || tasks[0].Due == nil || tasks[0].Due.Hour() != 10 || tasks[0].Priority != 1 || tasks[0].Body != "" || tasks[0].Reminder == nil || tasks[0].Reminder.UTC().Format("20060102T150405Z") != "20260602T090000Z" {
		t.Fatalf("fields after VALARM lost or alarm content leaked: %+v", tasks[0])
	}
}

func TestDAVPutEnforcesCollectionComponentType(t *testing.T) {
	todo := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:task1\r\nSUMMARY:Task\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	event := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:event1\r\nDTSTART:20260601T100000Z\r\nDTEND:20260601T110000Z\r\nSUMMARY:Event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for _, tc := range []struct {
		name, coll, body string
		status           int
	}{
		{"task in event collection", "cal1", todo, http.StatusForbidden},
		{"event in task collection", "tasks1", event, http.StatusForbidden},
		{"unknown collection", "unknown", todo, http.StatusNotFound},
		{"multiple tasks", "tasks1", strings.Replace(todo, "END:VCALENDAR\r\n", "BEGIN:VTODO\r\nUID:task2\r\nEND:VTODO\r\nEND:VCALENDAR\r\n", 1), http.StatusBadRequest},
		{"mixed components", "tasks1", strings.Replace(todo, "END:VCALENDAR\r\n", "BEGIN:VEVENT\r\nUID:event2\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", 1), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newFake()
			h := newHandler(t, p)
			rr := req(t, h, "PUT", "/caldav/"+acct+"/"+tc.coll+"/object.ics", tc.body, true)
			if rr.Code != tc.status {
				t.Fatalf("PUT=%d %s, want %d", rr.Code, rr.Body.String(), tc.status)
			}
			if len(p.tasks) != 0 || len(p.events) != 0 {
				t.Fatalf("rejected PUT mutated cloud: tasks=%+v events=%+v", p.tasks, p.events)
			}
		})
	}
}
