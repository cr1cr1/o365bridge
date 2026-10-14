package graph_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestGraphNativeTaskRepeatPatternsAndRanges(t *testing.T) {
	patterns := []struct{ name, native, rule string }{
		{"daily", `{"type":"daily","interval":2}`, "FREQ=DAILY;INTERVAL=2"},
		{"weekly", `{"type":"weekly","interval":2,"daysOfWeek":["monday","friday"],"firstDayOfWeek":"monday"}`, "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;WKST=MO"},
		{"monthly date", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":19}`, "FREQ=MONTHLY;INTERVAL=1;BYMONTHDAY=19"},
		{"monthly relative", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["monday"],"index":"last"}`, "FREQ=MONTHLY;INTERVAL=1;BYDAY=MO;BYSETPOS=-1"},
		{"yearly date", `{"type":"absoluteYearly","interval":1,"month":10,"dayOfMonth":19}`, "FREQ=YEARLY;INTERVAL=1;BYMONTH=10;BYMONTHDAY=19"},
		{"yearly relative", `{"type":"relativeYearly","interval":1,"month":10,"daysOfWeek":["monday"],"index":"second"}`, "FREQ=YEARLY;INTERVAL=1;BYMONTH=10;BYDAY=MO;BYSETPOS=2"},
	}
	ranges := []struct{ name, extra, suffix string }{
		{"noEnd", "", ""},
		{"numbered", `,"numberOfOccurrences":5`, ";COUNT=5"},
		{"endDate", `,"endDate":"2026-12-31"`, ";UNTIL=20261231"},
	}
	for _, pattern := range patterns {
		for _, repeatRange := range ranges {
			t.Run(pattern.name+"/"+repeatRange.name, func(t *testing.T) {
				f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
					jsonOut(w, http.StatusOK, `{"value":[{"id":"task1","recurrence":{"pattern":`+pattern.native+`,"range":{"type":"`+repeatRange.name+`","startDate":"2026-10-19","recurrenceTimeZone":"Pacific Standard Time"`+repeatRange.extra+`}}}]}`)
				})
				tasks, err := f.provider().ListTasks(context.Background(), "list1")
				if err != nil || len(tasks) != 1 {
					t.Fatalf("native repeat read = %+v, %v", tasks, err)
				}
				got := tasks[0]
				if got.Recur != pattern.rule+repeatRange.suffix || got.RecurUnsupported || got.RecurStart == nil || got.RecurStart.Format("2006-01-02") != "2026-10-19" || got.RecurTimeZone != "Pacific Standard Time" || got.Start != nil || got.RecurSet {
					t.Fatalf("native repeat mapping = %+v", got)
				}
			})
		}
	}
}

func TestGraphTaskRepeatWriteUsesOnlyRelevantFields(t *testing.T) {
	anchor := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, rule, pattern, repeatRange string }{
		{"daily", "FREQ=DAILY;INTERVAL=2", `{"type":"daily","interval":2}`, `{"type":"noEnd"}`},
		{"weekly", "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;WKST=SU", `{"type":"weekly","interval":2,"daysOfWeek":["monday","friday"],"firstDayOfWeek":"sunday"}`, `{"type":"noEnd"}`},
		{"monthly date", "FREQ=MONTHLY;BYMONTHDAY=19", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":19}`, `{"type":"noEnd"}`},
		{"monthly relative", "FREQ=MONTHLY;BYDAY=MO;BYSETPOS=-1", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["monday"],"index":"last"}`, `{"type":"noEnd"}`},
		{"yearly date", "FREQ=YEARLY;BYMONTH=10;BYMONTHDAY=19", `{"type":"absoluteYearly","interval":1,"month":10,"dayOfMonth":19}`, `{"type":"noEnd"}`},
		{"yearly relative", "FREQ=YEARLY;BYMONTH=10;BYDAY=2MO", `{"type":"relativeYearly","interval":1,"month":10,"daysOfWeek":["monday"],"index":"second"}`, `{"type":"noEnd"}`},
		{"numbered", "FREQ=DAILY;COUNT=5", `{"type":"daily","interval":1}`, `{"type":"numbered","numberOfOccurrences":5}`},
		{"until", "FREQ=DAILY;UNTIL=20261231", `{"type":"daily","interval":1}`, `{"type":"endDate","endDate":"2026-12-31"}`},
		{"until timestamp", "FREQ=DAILY;UNTIL=20261231T235959Z", `{"type":"daily","interval":1}`, `{"type":"endDate","endDate":"2026-12-31"}`},
		{"implicit weekly day", "FREQ=WEEKLY", `{"type":"weekly","interval":1,"daysOfWeek":["monday"],"firstDayOfWeek":"monday"}`, `{"type":"noEnd"}`},
		{"implicit monthly date", "FREQ=MONTHLY", `{"type":"absoluteMonthly","interval":1,"dayOfMonth":19}`, `{"type":"noEnd"}`},
		{"implicit yearly date", "FREQ=YEARLY", `{"type":"absoluteYearly","interval":1,"month":10,"dayOfMonth":19}`, `{"type":"noEnd"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) { w.WriteHeader(http.StatusNoContent) })
			task := bridge.Task{ID: "task1", ListID: "list1", Recur: tc.rule, RecurSet: true, RecurStart: &anchor, RecurTimeZone: "UTC"}
			if err := f.provider().UpdateTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			got, _ := bodyJSON(t, f.last().Body)["recurrence"].(map[string]any)
			var wantPattern, wantRange map[string]any
			_ = json.Unmarshal([]byte(tc.pattern), &wantPattern)
			_ = json.Unmarshal([]byte(tc.repeatRange), &wantRange)
			wantRange["startDate"], wantRange["recurrenceTimeZone"] = "2026-10-19", "UTC"
			if !reflect.DeepEqual(got["pattern"], wantPattern) || !reflect.DeepEqual(got["range"], wantRange) {
				t.Fatalf("recurrence payload = %#v, want pattern=%#v range=%#v", got, wantPattern, wantRange)
			}
		})
	}
}

func TestGraphTaskCreateValidatesRepeatWithoutMutationFlag(t *testing.T) {
	anchor := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for _, rule := range []string{"FREQ=DAILY", "FREQ=HOURLY"} {
		t.Run(rule, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				jsonOut(w, http.StatusCreated, `{"id":"created"}`)
			})
			_, err := f.provider().CreateTask(context.Background(), bridge.Task{ListID: "list1", Recur: rule, RecurStart: &anchor})
			if rule == "FREQ=HOURLY" {
				if err == nil || len(f.reqs) != 0 {
					t.Fatalf("invalid create RRULE must fail before task creation: err=%v calls=%+v", err, f.reqs)
				}
				return
			}
			if err != nil || len(f.reqs) != 1 {
				t.Fatalf("valid create repeat failed: %v", err)
			}
			recurrence, _ := bodyJSON(t, f.last().Body)["recurrence"].(map[string]any)
			pattern, _ := recurrence["pattern"].(map[string]any)
			if pattern["type"] != "daily" || pattern["interval"] != float64(1) {
				t.Fatalf("nonempty create repeat silently omitted: %+v", recurrence)
			}
		})
	}
}

func TestGraphTaskTimedUntilDoesNotExtendPastCutoff(t *testing.T) {
	for _, tc := range []struct {
		name, until, recurrenceZone string
		location                    *time.Location
		separateStart               bool
		wantError                   bool
	}{
		{"UTC cutoff before occurrence", "20261231T080000Z", "UTC", time.UTC, false, true},
		{"UTC cutoff at occurrence", "20261231T090000Z", "UTC", time.UTC, false, false},
		{"UTC cutoff after occurrence", "20261231T235959Z", "UTC", time.UTC, false, false},
		{"non-UTC anchor", "20261231T235959Z", "", time.FixedZone("UTC+2", 2*3600), false, true},
		{"non-UTC recurrence zone", "20261231T235959Z", "Pacific Standard Time", time.UTC, false, true},
		{"separate range anchor does not hide task time", "20261231T080000Z", "UTC", time.UTC, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anchor := time.Date(2026, 10, 19, 9, 0, 0, 0, tc.location)
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if r.Method == http.MethodGet {
					jsonOut(w, http.StatusOK, `{"id":"task1"}`)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			task := bridge.Task{ID: "task1", ListID: "list1", Recur: "FREQ=DAILY;UNTIL=" + tc.until, RecurSet: true, RecurStart: &anchor, RecurTimeZone: tc.recurrenceZone}
			if tc.separateStart {
				start := anchor
				task.Start = &start
				anchor = time.Date(2026, 10, 19, 0, 0, 0, 0, tc.location)
			}
			err := f.provider().UpdateTask(context.Background(), task)
			if tc.wantError {
				if err == nil || len(f.reqs) != 0 {
					t.Fatalf("unsafe UNTIL must fail before network: err=%v calls=%+v", err, f.reqs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			recurrence, _ := bodyJSON(t, f.last().Body)["recurrence"].(map[string]any)
			repeatRange, _ := recurrence["range"].(map[string]any)
			if repeatRange["endDate"] != "2026-12-31" {
				t.Fatalf("safe UTC cutoff date = %+v", repeatRange)
			}
		})
	}
}

func TestGraphTaskTimedUntilPreservesNanosecondCutoffPrecision(t *testing.T) {
	for _, field := range []string{"anchor", "start", "due"} {
		t.Run(field, func(t *testing.T) {
			day := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
			candidate := time.Date(2026, 10, 19, 10, 0, 0, 500_000_000, time.UTC)
			task := bridge.Task{ID: "task1", ListID: "list1", Recur: "FREQ=DAILY;UNTIL=20261231T100000Z", RecurSet: true, RecurStart: &day, RecurTimeZone: "UTC"}
			switch field {
			case "anchor":
				task.RecurStart = &candidate
			case "start":
				task.Start = &candidate
			case "due":
				task.Due = &candidate
			}
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				t.Errorf("fractional cutoff must fail before network: %s", r.URL.Path)
			})
			err := f.provider().UpdateTask(context.Background(), task)
			if err == nil || !strings.Contains(err.Error(), "inclusive date range") || len(f.reqs) != 0 {
				t.Fatalf("10:00:00 cutoff must reject 10:00:00.5 %s: err=%v calls=%+v", field, err, f.reqs)
			}
		})
	}
}

func TestGraphTaskRepeatMutationIsExplicit(t *testing.T) {
	for _, set := range []bool{false, true} {
		f := newTaskUpdateFake(t)
		if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", FullUpdate: true, RecurSet: set}); err != nil {
			t.Fatal(err)
		}
		got, exists := bodyJSON(t, f.last().Body)["recurrence"]
		if exists != set || (exists && got != nil) {
			t.Errorf("explicit clear gate=%v got=%#v exists=%v", set, got, exists)
		}
	}
}

func TestGraphTaskRepeatRejectsUnrepresentableRulesBeforeUpstream(t *testing.T) {
	anchor := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for _, rule := range []string{
		"FREQ=HOURLY", "FREQ=DAILY;BYHOUR=9", "FREQ=DAILY;INTERVAL=0", "FREQ=DAILY;INTERVAL=-1", "FREQ=DAILY;INTERVAL=no",
		"FREQ=DAILY;COUNT=0", "FREQ=DAILY;COUNT=2;UNTIL=20261231", "FREQ=DAILY;UNTIL=bad", "FREQ=DAILY;UNTIL=20260101", "FREQ=DAILY;FREQ=WEEKLY",
		"FREQ=MONTHLY;BYDAY=MO", "FREQ=MONTHLY;BYDAY=MO,TU;BYSETPOS=2", "FREQ=MONTHLY;BYDAY=5MO", "FREQ=MONTHLY;BYMONTHDAY=-1", "FREQ=MONTHLY;BYMONTHDAY=2,3",
		"FREQ=WEEKLY;BYDAY=2MO", "FREQ=WEEKLY;BYDAY=XX", "FREQ=YEARLY;BYMONTH=13", "FREQ=YEARLY;BYMONTH=1,2", "FREQ=DAILY;", "FREQ=DAILY;WKST=MO",
		"FREQ=YEARLY;BYDAY=2MO", "FREQ=YEARLY;BYMONTHDAY=19", "FREQ=YEARLY;BYDAY=MO;BYSETPOS=2",
	} {
		t.Run(rule, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				t.Errorf("unsafe repeat reached upstream: %s", r.URL.Path)
			})
			if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", Recur: rule, RecurSet: true, RecurStart: &anchor}); err == nil {
				t.Fatalf("unsupported rule accepted: %s", rule)
			}
			if len(f.reqs) != 0 {
				t.Error("invalid repeat must fail before network")
			}
		})
	}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("anchorless repeat reached upstream: %s", r.URL.Path)
	})
	if err := f.provider().UpdateTask(context.Background(), bridge.Task{ID: "task1", ListID: "list1", Recur: "FREQ=DAILY", RecurSet: true}); err == nil {
		t.Fatal("Graph repeat without required range start date accepted")
	}
}

func TestGraphUnsupportedNativeRepeatIsMarkedAndPreserved(t *testing.T) {
	for _, native := range []string{
		`{"pattern":{"type":"relativeMonthly","interval":1,"daysOfWeek":["monday","tuesday"],"index":"second"},"range":{"type":"noEnd","startDate":"2026-10-19"}}`,
		`{"pattern":{"type":"daily","interval":1},"range":{"type":"noEnd","startDate":"bad"}}`,
		`{"pattern":{"type":"regenerateDaily","interval":1},"range":{"type":"noEnd","startDate":"2026-10-19"}}`,
	} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if r.Method == http.MethodGet {
				jsonOut(w, http.StatusOK, `{"value":[{"id":"task1","recurrence":`+native+`}]}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		tasks, err := f.provider().ListTasks(context.Background(), "list1")
		if err != nil || len(tasks) != 1 || !tasks[0].RecurUnsupported || tasks[0].Recur != "" || tasks[0].RecurSet {
			t.Fatalf("unsupported repeat mapping = %+v, %v", tasks, err)
		}
		tasks[0].Title = "Unrelated edit"
		if err := f.provider().UpdateTask(context.Background(), tasks[0]); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(f.last().Body), `"recurrence"`) {
			t.Fatalf("unsupported native repeat was overwritten: %s", f.last().Body)
		}
	}
}
