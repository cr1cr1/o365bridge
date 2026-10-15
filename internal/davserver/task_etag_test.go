package davserver

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestRecurringTaskPUTUsesCanonicalGETValidator(t *testing.T) {
	for _, mode := range []string{"create series", "edit series", "clear series", "ordinary task"} {
		t.Run(mode, func(t *testing.T) {
			p := newFake()
			start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
			due := start.AddDate(0, 0, 1)
			rule := "FREQ=DAILY"
			if mode == "ordinary task" {
				rule = ""
			}
			task := bridge.Task{ID: "native", ListID: "tasks1", UID: "task", Title: "Task", Start: &start, Due: &due, StartDateOnly: true, DueDateOnly: true, Recur: rule, RecurStart: &start, RecurTimeZone: "UTC"}
			if mode != "create series" {
				p.tasks[task.ID] = task
			}
			h := newHandler(t, p)
			path := "/caldav/" + acct + "/tasks1/task.ics"
			data := string(generateTaskICalendar(task))
			data = strings.Replace(data, "SUMMARY:Task", "SUMMARY:Edited", 1)
			if mode == "clear series" {
				data = strings.Replace(data, "RRULE:FREQ=DAILY\r\n", "", 1)
			}
			put := req(t, h, "PUT", path, data, true)
			wantStatus := 204
			if mode == "create series" {
				wantStatus = 201
			}
			if put.Code != wantStatus {
				t.Fatalf("PUT=%d %s", put.Code, put.Body.String())
			}
			if mode == "ordinary task" {
				if put.Header().Get("ETag") == "" {
					t.Error("ordinary task lost its existing PUT validator")
				}
			} else if tag := put.Header().Get("ETag"); tag != "" {
				t.Errorf("recurring-task PUT returned a predicted validator before native readback: %s", tag)
			}
			get := req(t, h, "GET", path, "", true)
			if get.Code != 200 || get.Header().Get("ETag") == "" {
				t.Fatalf("canonical GET missing validator=%d %s", get.Code, get.Body.String())
			}
			r := httptest.NewRequest("PUT", path, strings.NewReader(get.Body.String()))
			r.SetBasicAuth(acct, pw)
			r.Header.Set("If-Match", get.Header().Get("ETag"))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != 204 {
				t.Fatalf("canonical conditional PUT=%d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
