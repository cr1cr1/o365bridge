package davserver

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestTaskDetailCodecPreservesDatesNotesCategoriesAndRepeatAnchor(t *testing.T) {
	due := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	anchor := due.AddDate(0, 0, -2)
	tk := bridge.Task{ID: "native", UID: "task", Title: "Task", Body: "Literal <tag> & notes\n第二行", Due: &due, DueDateOnly: true, Categories: []string{"Work", "Sales, Europe", `Folder\label`}, Recur: "FREQ=WEEKLY;BYDAY=MO,WE;INTERVAL=2;COUNT=4", RecurStart: &anchor, RecurTimeZone: "UTC", Attachments: []bridge.TaskAttachment{{URI: "http://bridge/caldav/user/tasks/_attachments/native/file/note.txt", ContentType: "text/plain"}}}
	data := string(generateTaskICalendar(tk))
	for _, value := range []string{"DUE;VALUE=DATE:20261017", "DTSTART;VALUE=DATE:20261015", "CATEGORIES:Work,Sales\\, Europe,Folder\\\\label", "DESCRIPTION:Literal <tag> & notes\\n第二行", "RRULE:FREQ=WEEKLY;BYDAY=MO,WE;INTERVAL=2;COUNT=4", "ATTACH;FMTTYPE=text/plain:http://bridge/"} {
		if !strings.Contains(strings.Join(unfold([]byte(data)), "\r\n"), value) {
			t.Errorf("missing %q in %s", value, data)
		}
	}
	_, tasks, err := parseICalendar([]byte(data))
	if err != nil || len(tasks) != 1 {
		t.Fatalf("parse=%+v %v", tasks, err)
	}
	got := tasks[0]
	if got.Start != nil || got.RecurStart == nil || !got.RecurStart.Equal(anchor) || !got.DueDateOnly || got.Due == nil || !got.Due.Equal(due) || got.Body != tk.Body || !reflect.DeepEqual(got.Categories, tk.Categories) || got.Recur != tk.Recur || !got.FullUpdate || !got.RecurSet || len(got.Attachments) != 1 {
		t.Fatalf("task detail loss: %+v", got)
	}
	// A due date-time requires a date-time repeat DTSTART as well.
	tk.DueDateOnly = false
	if data := string(generateTaskICalendar(tk)); !strings.Contains(data, "DTSTART:20261015T000000Z") || strings.Contains(data, "DTSTART;VALUE=DATE") {
		t.Fatalf("mixed recurrence DTSTART/DUE value types: %s", data)
	}
}

func TestTaskRepeatPositiveOffsetAndWireEndBound(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, 10, 19, 0, 0, 0, 0, loc)
	due := time.Date(2026, 10, 20, 10, 0, 0, 0, loc)
	task := bridge.Task{UID: "repeat", Recur: "FREQ=DAILY;UNTIL=20261025", RecurStart: &anchor, RecurTimeZone: "GMT Standard Time", Due: &due}
	data := generateTaskICalendar(task)
	logical := strings.Join(unfold(data), "\r\n")
	if !strings.Contains(logical, "DTSTART:20261018T230000Z") || !strings.Contains(logical, "UNTIL=20261025T235959Z") {
		t.Fatalf("invalid timezone/dateTime recurrence projection: %s", logical)
	}
	_, tasks, err := parseICalendar(data)
	if err != nil || len(tasks) != 1 || tasks[0].Start != nil || tasks[0].RecurStart == nil || tasks[0].RecurStart.Format("2006-01-02") != "2026-10-19" {
		t.Fatalf("positive-offset repeat anchor shifted: %+v %v", tasks, err)
	}
}

func TestTaskUnrelatedEditPreservesRemindersProgressAndWirePrecision(t *testing.T) {
	p := newFake()
	start := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	completed := start.Add(time.Second + 123*time.Millisecond)
	reminder := start.Add(time.Hour)
	p.tasks["native"] = bridge.Task{ID: "native", ListID: "tasks1", UID: "task", Title: "Task", Body: "First\r\nSecond", Due: &start, Completed: &completed, Status: "completed", Reminder: &reminder, Recur: "FREQ=DAILY;UNTIL=20261030", RecurStart: &start, RecurTimeZone: "+03:00"}
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/tasks1/task.ics"
	get := req(t, h, "GET", path, "", true)
	// Category-unaware clients may omit private bridge metadata; preserve native anchor.
	var lines []string
	for _, line := range unfold(get.Body.Bytes()) {
		if !strings.HasPrefix(line, "X-O365BRIDGE-") {
			lines = append(lines, line)
		}
	}
	edited := strings.Replace(string(joinCRLF(lines)), "SUMMARY:Task", "SUMMARY:Changed", 1)
	if rr := req(t, h, "PUT", path, edited, true); rr.Code != 204 {
		t.Fatalf("ordinary detail edit=%d %s", rr.Code, rr.Body.String())
	}
	got := p.tasks["native"]
	if !got.BodyUnchanged || !got.StatusUnchanged || got.Reminder == nil || !got.Reminder.Equal(reminder) || got.RecurSet || got.Recur != "FREQ=DAILY;UNTIL=20261030" {
		t.Fatalf("unrelated edit changed opaque native details: %+v", got)
	}
}

func TestTaskRuleOrderingDoesNotRewriteNativeRepeat(t *testing.T) {
	if !sameTaskRule("FREQ=WEEKLY;BYDAY=WE,MO;WKST=MO", "BYDAY=MO,WE;INTERVAL=1;FREQ=WEEKLY") {
		t.Fatal("equivalent weekday/rule ordering not recognized")
	}
	if sameTaskRule("FREQ=DAILY;COUNT=3", "FREQ=DAILY;COUNT=4") || sameTaskRule("FREQ=DAILY;FREQ=DAILY", "FREQ=DAILY") {
		t.Fatal("changed or duplicate rule treated as unchanged")
	}
}

func TestTaskRepeatAnchorChangesAreNotSilentlyIgnored(t *testing.T) {
	p := newFake()
	anchor := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	p.tasks["native"] = bridge.Task{ID: "native", ListID: "tasks1", UID: "task", Title: "Task", Due: &anchor, DueDateOnly: true, Recur: "FREQ=DAILY", RecurStart: &anchor, RecurTimeZone: "UTC"}
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/tasks1/task.ics"
	get := req(t, h, "GET", path, "", true)
	edited := strings.Replace(get.Body.String(), "X-O365BRIDGE-RECUR-START:20261015", "X-O365BRIDGE-RECUR-START:20261016", 1)
	if rr := req(t, h, "PUT", path, edited, true); rr.Code != 204 {
		t.Fatalf("repeat anchor edit=%d %s", rr.Code, rr.Body.String())
	}
	if got := p.tasks["native"]; !got.RecurSet || got.RecurStart == nil || got.RecurStart.Format("20060102") != "20261016" {
		t.Fatalf("range change silently dropped: %+v", got)
	}
}

func TestTaskDetailParserRejectsInvalidDatesAndExceptions(t *testing.T) {
	for _, line := range []string{"DUE;VALUE=DATE:invalid", "DTSTART:invalid", "COMPLETED:invalid", "RDATE:20261015T100000Z", "EXDATE:20261015T100000Z", "RECURRENCE-ID:20261015T100000Z"} {
		data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:task\r\n" + line + "\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
		if _, _, err := parseICalendar([]byte(data)); err == nil {
			t.Fatalf("unsupported task detail accepted: %s", line)
		}
	}
}

func TestTaskConditionalPUTProtectsCloudDetails(t *testing.T) {
	for _, mode := range []string{"stale", "weak", "deleted wildcard"} {
		t.Run(mode, func(t *testing.T) {
			p := newFake()
			due := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
			p.tasks["native"] = bridge.Task{ID: "native", ListID: "tasks1", UID: "task", Title: "Task", Due: &due, Categories: []string{"Work"}}
			h := newHandler(t, p)
			path := "/caldav/" + acct + "/tasks1/task.ics"
			get := req(t, h, "GET", path, "", true)
			tag := get.Header().Get("ETag")
			if mode == "stale" {
				changed := p.tasks["native"]
				changed.Categories = []string{"Cloud changed"}
				p.tasks["native"] = changed
			}
			if mode == "weak" {
				tag = "W/" + tag
			}
			if mode == "deleted wildcard" {
				delete(p.tasks, "native")
				tag = "*"
			}
			r := httptest.NewRequest("PUT", path, strings.NewReader(get.Body.String()))
			r.SetBasicAuth(acct, pw)
			r.Header.Set("If-Match", tag)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != 412 {
				t.Fatalf("conditional task update=%d %s", rr.Code, rr.Body.String())
			}
			if mode == "stale" && !reflect.DeepEqual(p.tasks["native"].Categories, []string{"Cloud changed"}) {
				t.Fatalf("stale task erased cloud categories: %+v", p.tasks)
			}
			if mode == "deleted wildcard" && len(p.tasks) != 0 {
				t.Fatalf("conditional task recreated: %+v", p.tasks)
			}
		})
	}
}

func TestUnsupportedNativeTaskRepeatAndNotesArePreserved(t *testing.T) {
	p := newFake()
	due := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	p.tasks["native"] = bridge.Task{ID: "native", ListID: "tasks1", UID: "task", Title: "Task", Body: "Rich note visible text", Due: &due, RecurUnsupported: true}
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/tasks1/task.ics"
	get := req(t, h, "GET", path, "", true)
	edited := strings.Replace(get.Body.String(), "SUMMARY:Task", "SUMMARY:Changed", 1)
	if rr := req(t, h, "PUT", path, edited, true); rr.Code != 204 {
		t.Fatalf("ordinary unsupported-repeat edit: %d %s", rr.Code, rr.Body.String())
	}
	if !p.tasks["native"].RecurUnsupported || p.tasks["native"].RecurSet || !p.tasks["native"].BodyUnchanged {
		t.Fatalf("unsupported/native details weren't preserved: %+v", p.tasks["native"])
	}
	changedRule := strings.Replace(edited, "END:VTODO", "RRULE:FREQ=DAILY\r\nEND:VTODO", 1)
	if rr := req(t, h, "PUT", path, changedRule, true); rr.Code != 422 {
		t.Fatalf("unsupported native repeat was overwritten: %d %s", rr.Code, rr.Body.String())
	}
}
