package davserver

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func taskAlarmCalendar(props string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:task\r\nSUMMARY:Task\r\n" + props + "END:VTODO\r\nEND:VCALENDAR\r\n"
}

func taskDisplayAlarm(trigger string) string {
	return "BEGIN:VALARM\r\nACTION:DISPLAY\r\nDESCRIPTION:Alarm only\r\n" + trigger + "\r\nEND:VALARM\r\n"
}

func TestTaskAlarmParse(t *testing.T) {
	for _, tc := range []struct{ name, props, want string }{
		{"absolute", taskDisplayAlarm("TRIGGER;VALUE=DATE-TIME:20260602T090000Z"), "2026-06-02T09:00:00Z"},
		{"default start UTC", "DTSTART:20260602T100000Z\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"), "2026-06-02T09:45:00Z"},
		{"explicit start", "DTSTART:20260602T100000Z\r\n" + taskDisplayAlarm("TRIGGER;VALUE=DURATION;RELATED=START:+PT1H2M3S"), "2026-06-02T11:02:03Z"},
		{"end UTC after alarm", taskDisplayAlarm("TRIGGER;RELATED=END:-PT15M") + "DUE:20260602T100000Z\r\n", "2026-06-02T09:45:00Z"},
		{"start IANA after alarm", taskDisplayAlarm("TRIGGER:-PT30M") + "DTSTART;TZID=America/New_York:20260702T100000\r\n", "2026-07-02T13:30:00Z"},
		{"end IANA", "DUE;TZID=Europe/London:20260702T100000\r\n" + taskDisplayAlarm("TRIGGER;RELATED=END:PT0S"), "2026-07-02T09:00:00Z"},
		{"calendar day across spring DST", "DTSTART;TZID=America/New_York:20260308T100000\r\n" + taskDisplayAlarm("TRIGGER:-P1D"), "2026-03-07T15:00:00Z"},
		{"hours across spring DST", "DTSTART;TZID=America/New_York:20260308T100000\r\n" + taskDisplayAlarm("TRIGGER:-PT24H"), "2026-03-07T14:00:00Z"},
		{"week across autumn DST", "DUE;TZID=America/New_York:20261031T100000\r\n" + taskDisplayAlarm("TRIGGER;RELATED=END:P1W"), "2026-11-07T15:00:00Z"},
		{"day then time", "DTSTART;TZID=America/New_York:20260308T100000\r\n" + taskDisplayAlarm("TRIGGER:-P1DT2H3M4S"), "2026-03-07T12:56:56Z"},
		{"synthetic recurrence start", taskDisplayAlarm("TRIGGER:-PT15M") + "DTSTART:20260602T100000Z\r\nX-O365BRIDGE-RECUR-ANCHOR-ONLY:TRUE\r\nRRULE:FREQ=DAILY\r\n", "2026-06-02T09:45:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tasks, err := parseICalendar([]byte(taskAlarmCalendar("DESCRIPTION:Task notes\r\nCATEGORIES:Work\r\n" + tc.props)))
			if err != nil || len(tasks) != 1 {
				t.Fatalf("parse=%+v %v", tasks, err)
			}
			got := tasks[0]
			if got.Reminder == nil || got.Reminder.UTC().Format(time.RFC3339) != tc.want {
				t.Fatalf("reminder=%v, want %s", got.Reminder, tc.want)
			}
			if got.Body != "Task notes" || !reflect.DeepEqual(got.Categories, []string{"Work"}) {
				t.Fatalf("alarm leaked into task: %+v", got)
			}
			if tc.name == "synthetic recurrence start" && (got.Start != nil || got.RecurStart == nil) {
				t.Fatalf("synthetic anchor changed: %+v", got)
			}
		})
	}
}

func invalidTaskAlarms() map[string]string {
	valid := taskDisplayAlarm("TRIGGER;VALUE=DATE-TIME:20260602T090000Z")
	cases := map[string]string{
		"missing trigger":           taskDisplayAlarm(""),
		"missing action":            strings.Replace(valid, "ACTION:DISPLAY\r\n", "", 1),
		"missing description":       strings.Replace(valid, "DESCRIPTION:Alarm only\r\n", "", 1),
		"email":                     strings.Replace(valid, "ACTION:DISPLAY", "ACTION:EMAIL", 1),
		"audio":                     strings.Replace(valid, "ACTION:DISPLAY", "ACTION:AUDIO", 1),
		"duplicate trigger":         strings.Replace(valid, "END:VALARM", "TRIGGER:-PT1M\r\nEND:VALARM", 1),
		"duplicate action":          strings.Replace(valid, "END:VALARM", "ACTION:DISPLAY\r\nEND:VALARM", 1),
		"duplicate description":     strings.Replace(valid, "END:VALARM", "DESCRIPTION:Again\r\nEND:VALARM", 1),
		"multiple alarms":           valid + valid,
		"repeat":                    strings.Replace(valid, "END:VALARM", "REPEAT:2\r\nDURATION:PT5M\r\nEND:VALARM", 1),
		"repeat only":               strings.Replace(valid, "END:VALARM", "REPEAT:0\r\nEND:VALARM", 1),
		"duration only":             strings.Replace(valid, "END:VALARM", "DURATION:PT5M\r\nEND:VALARM", 1),
		"nested alarm":              "BEGIN:VALARM\r\n" + valid + "END:VALARM\r\n",
		"alarm in unknown child":    "BEGIN:X-WRAPPER\r\n" + valid + "END:X-WRAPPER\r\n",
		"unterminated alarm":        strings.Replace(valid, "END:VALARM\r\n", "", 1),
		"wrong end":                 strings.Replace(valid, "END:VALARM", "END:X-WRAPPER", 1),
		"nested component":          strings.Replace(valid, "END:VALARM", "BEGIN:X-WRAPPER\r\nEND:X-WRAPPER\r\nEND:VALARM", 1),
		"parameterized begin":       strings.Replace(valid, "BEGIN:VALARM", "BEGIN;VALUE=TEXT:VALARM", 1),
		"parameterized end":         strings.Replace(valid, "END:VALARM", "END;VALUE=TEXT:VALARM", 1),
		"missing START":             "DUE:20260602T100000Z\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"missing END":               "DTSTART:20260602T100000Z\r\n" + taskDisplayAlarm("TRIGGER;RELATED=END:-PT15M"),
		"unsupported END fallback":  "DTSTART:20260602T100000Z\r\nDURATION:PT1H\r\n" + taskDisplayAlarm("TRIGGER;RELATED=END:-PT15M"),
		"DATE start":                "DTSTART;VALUE=DATE:20260602\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"DATE due":                  "DUE;VALUE=DATE:20260602\r\n" + taskDisplayAlarm("TRIGGER;RELATED=END:-PT15M"),
		"floating start":            "DTSTART:20260602T100000\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"floating due":              "DUE:20260602T100000\r\n" + taskDisplayAlarm("TRIGGER;RELATED=END:-PT15M"),
		"unknown zone":              "DTSTART;TZID=Mars/Olympus:20260602T100000\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"local zone":                "DTSTART;TZID=Local:20260602T100000\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"UTC with TZID":             "DTSTART;TZID=America/New_York:20260602T100000Z\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"duplicate anchor":          "DTSTART:20260602T100000Z\r\nDTSTART:20260602T110000Z\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"DST gap anchor":            "DTSTART;TZID=America/New_York:20260308T023000\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"DST fold anchor":           "DTSTART;TZID=Europe/London:20261025T013000\r\n" + taskDisplayAlarm("TRIGGER:-PT15M"),
		"DST gap nominal target":    "DTSTART;TZID=America/New_York:20260309T023000\r\n" + taskDisplayAlarm("TRIGGER:-P1D"),
		"DST fold nominal target":   "DTSTART;TZID=Europe/London:20261026T013000\r\n" + taskDisplayAlarm("TRIGGER:-P1D"),
		"duration target underflow": "DTSTART:00010101T000000Z\r\n" + taskDisplayAlarm("TRIGGER:-PT1S"),
		"duration target overflow":  "DTSTART:99991231T235959Z\r\n" + taskDisplayAlarm("TRIGGER:PT1S"),
	}
	for _, trigger := range []string{
		"TRIGGER;VALUE=DATE-TIME:20260602T090000", "TRIGGER;VALUE=DATE-TIME:20260230T090000Z",
		"TRIGGER;VALUE=DATE-TIME;RELATED=START:20260602T090000Z", "TRIGGER;VALUE=DATE-TIME;TZID=UTC:20260602T090000Z",
		"TRIGGER:20260602T090000Z", "TRIGGER;VALUE=DATE:20260602", "TRIGGER;VALUE=BOGUS:-PT1M", "TRIGGER;VALUE=:-PT1M",
		"TRIGGER;RELATED=BOGUS:-PT1M", "TRIGGER;RELATED=:-PT1M", "TRIGGER;TZID=UTC:-PT1M",
		"TRIGGER;VALUE=DURATION;VALUE=DATE-TIME:20260602T090000Z", "TRIGGER;RELATED=START;RELATED=END:-PT1M",
		"TRIGGER;VALUE:-PT1M", "TRIGGER;VALUE=\"DATE-TIME:20260602T090000Z", "TRIGGER", "TRIGGER:",
		"TRIGGER:P", "TRIGGER:PT", "TRIGGER:P1Y", "TRIGGER:P1M", "TRIGGER:PT1.5H", "TRIGGER:PT1M1H", "TRIGGER:PT1M2M", "TRIGGER:P1W1D", "TRIGGER:P1DT", "TRIGGER:PT1H2S",
		"TRIGGER:PT99999999999999999999999H", "TRIGGER:P99999999999999999999999D",
	} {
		cases[trigger] = "DTSTART:20260602T100000Z\r\n" + taskDisplayAlarm(trigger)
	}
	return cases
}

func TestTaskAlarmRejectsInvalidBeforeMutation(t *testing.T) {
	for name, props := range invalidTaskAlarms() {
		t.Run(name, func(t *testing.T) {
			data := taskAlarmCalendar(props)
			if _, _, err := parseICalendar([]byte(data)); err == nil {
				t.Error("invalid or unsupported alarm accepted")
			}
			for _, existing := range []bool{false, true} {
				p := newFake()
				old := bridge.Task{ID: "native", UID: "task", ListID: "tasks1", Body: "unchanged"}
				if existing {
					p.tasks[old.ID] = old
				}
				h := newHandler(t, p)
				rr := req(t, h, "PUT", "/caldav/"+acct+"/tasks1/task.ics", data, true)
				if rr.Code != 400 {
					t.Fatalf("existing=%v PUT=%d %s", existing, rr.Code, rr.Body.String())
				}
				if existing && !reflect.DeepEqual(p.tasks[old.ID], old) || !existing && len(p.tasks) != 0 {
					t.Fatalf("rejected alarm mutated tasks: %+v", p.tasks)
				}
			}
		})
	}
}

func TestTaskAlarmGETPUTCreateUpdateClear(t *testing.T) {
	p := newFake()
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/tasks1/task.ics"
	first := taskAlarmCalendar("DESCRIPTION:Notes\r\n" + taskDisplayAlarm("TRIGGER;VALUE=DATE-TIME:20260602T090000Z"))
	if rr := req(t, h, "PUT", path, first, true); rr.Code != 201 {
		t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
	}
	if got := p.tasks["id-task"]; got.Reminder == nil || got.Reminder.UTC().Format(time.RFC3339) != "2026-06-02T09:00:00Z" {
		t.Fatalf("created reminder=%+v", got)
	}
	changed := strings.Replace(first, "20260602T090000Z", "20260602T093000Z", 1)
	if rr := req(t, h, "PUT", path, changed, true); rr.Code != 204 {
		t.Fatalf("update=%d %s", rr.Code, rr.Body.String())
	}
	get := req(t, h, "GET", path, "", true)
	if get.Code != 200 || strings.Count(get.Body.String(), "BEGIN:VALARM") != 1 || !strings.Contains(get.Body.String(), "TRIGGER;VALUE=DATE-TIME:20260602T093000Z") || !strings.Contains(get.Body.String(), "ACTION:DISPLAY\r\nDESCRIPTION:") {
		t.Fatalf("GET alarm=%d %s", get.Code, get.Body.String())
	}
	_, tasks, err := parseICalendar(get.Body.Bytes())
	if err != nil || len(tasks) != 1 || tasks[0].Body != "Notes" || tasks[0].Reminder == nil {
		t.Fatalf("GET roundtrip=%+v %v", tasks, err)
	}
	noAlarm := taskAlarmCalendar("DESCRIPTION:Notes\r\n")
	if rr := req(t, h, "PUT", path, noAlarm, true); rr.Code != 204 {
		t.Fatalf("clear=%d %s", rr.Code, rr.Body.String())
	}
	if got := p.tasks["id-task"]; got.Reminder != nil || !got.FullUpdate {
		t.Fatalf("full PUT did not clear reminder: %+v", got)
	}
	if get := req(t, h, "GET", path, "", true); strings.Contains(get.Body.String(), "VALARM") {
		t.Fatalf("cleared GET still has alarm: %s", get.Body.String())
	}
	p2 := newFake()
	h2 := newHandler(t, p2)
	if rr := req(t, h2, "PUT", path, noAlarm, true); rr.Code != 201 || p2.tasks["id-task"].Reminder != nil {
		t.Fatalf("no-alarm create=%d %+v", rr.Code, p2.tasks)
	}
}

func TestTaskAlarmUnrelatedNoteEditKeepsNativePrecision(t *testing.T) {
	for _, nanos := range []int{0, 123456789} {
		t.Run(time.Duration(nanos).String(), func(t *testing.T) {
			p := newFake()
			reminder := time.Date(2026, 6, 2, 9, 0, 0, nanos, time.FixedZone("native", 7200))
			p.tasks["native"] = bridge.Task{ID: "native", UID: "task", ListID: "tasks1", Title: "Task, title;\nnext", Body: "Original", Reminder: &reminder}
			h := newHandler(t, p)
			path := "/caldav/" + acct + "/tasks1/task.ics"
			get := req(t, h, "GET", path, "", true)
			if !strings.Contains(get.Body.String(), "TRIGGER;VALUE=DATE-TIME:20260602T070000Z") {
				t.Fatalf("GET omitted canonical reminder: %s", get.Body.String())
			}
			r := httptest.NewRequest("PUT", path, strings.NewReader(strings.Replace(get.Body.String(), "DESCRIPTION:Original", "DESCRIPTION:Edited notes", 1)))
			r.SetBasicAuth(acct, pw)
			r.Header.Set("If-Match", get.Header().Get("ETag"))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			got := p.tasks["native"]
			if rr.Code != 204 || got.Body != "Edited notes" || got.Reminder != &reminder || !got.Reminder.Equal(reminder) || !got.ReminderUnchanged {
				t.Fatalf("note edit lost native precision: status=%d task=%+v", rr.Code, got)
			}
		})
	}
}

func TestTaskAlarmCloudChangeInvalidatesValidator(t *testing.T) {
	p := newFake()
	first := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	p.tasks["native"] = bridge.Task{ID: "native", UID: "task", ListID: "tasks1", Title: "Task", Reminder: &first}
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/tasks1/task.ics"
	get := req(t, h, "GET", path, "", true)
	next := first.Add(time.Minute)
	cloud := p.tasks["native"]
	cloud.Reminder = &next
	p.tasks["native"] = cloud
	r := httptest.NewRequest("PUT", path, strings.NewReader(get.Body.String()))
	r.SetBasicAuth(acct, pw)
	r.Header.Set("If-Match", get.Header().Get("ETag"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != 412 || !reflect.DeepEqual(p.tasks["native"], cloud) {
		t.Fatalf("stale alarm overwrote cloud: %d %+v", rr.Code, p.tasks)
	}
}

func TestEventAlarmBehaviorUnchanged(t *testing.T) {
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:event\r\nBEGIN:VALARM\r\nACTION:EMAIL\r\nREPEAT:2\r\nDESCRIPTION:Ignored alarm\r\nEND:VALARM\r\nDESCRIPTION:Event notes\r\nDTSTART:20260602T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	events, tasks, err := parseICalendar([]byte(data))
	if err != nil || len(events) != 1 || len(tasks) != 0 || events[0].Description != "Event notes" || events[0].Start.IsZero() {
		t.Fatalf("event alarm behavior changed: %+v %+v %v", events, tasks, err)
	}
}

func TestTaskAlarmSerializationWithoutReminder(t *testing.T) {
	if data := string(generateTaskICalendar(bridge.Task{UID: "task"})); strings.Contains(data, "VALARM") {
		t.Fatalf("nil reminder emitted alarm: %s", data)
	}
}
