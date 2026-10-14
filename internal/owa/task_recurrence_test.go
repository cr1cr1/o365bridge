package owa

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWATaskNativeRecurrencePatterns(t *testing.T) {
	anchor := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		rule    string
		pattern map[string]any
	}{
		{"daily", "FREQ=DAILY;INTERVAL=2", map[string]any{"__type": "DailyRecurrence:#Exchange", "Interval": 2}},
		{"weekdays", "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;WKST=MO", map[string]any{"__type": "WeeklyRecurrence:#Exchange", "Interval": 1, "DaysOfWeek": "Monday Tuesday Wednesday Thursday Friday", "FirstDayOfWeek": "Monday"}},
		{"weekly", "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,TH;WKST=SU", map[string]any{"__type": "WeeklyRecurrence:#Exchange", "Interval": 2, "DaysOfWeek": "Tuesday Thursday", "FirstDayOfWeek": "Sunday"}},
		{"monthly date", "FREQ=MONTHLY;INTERVAL=2;BYMONTHDAY=15", map[string]any{"__type": "AbsoluteMonthlyRecurrence:#Exchange", "Interval": 2, "DayOfMonth": 15}},
		{"monthly relative", "FREQ=MONTHLY;BYDAY=MO;BYSETPOS=2", map[string]any{"__type": "RelativeMonthlyRecurrence:#Exchange", "Interval": 1, "DaysOfWeek": "Monday", "DayOfWeekIndex": "Second"}},
		{"monthly last weekday", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1", map[string]any{"__type": "RelativeMonthlyRecurrence:#Exchange", "Interval": 1, "DaysOfWeek": "Weekday", "DayOfWeekIndex": "Last"}},
		{"monthly last weekend", "FREQ=MONTHLY;BYDAY=SU,SA;BYSETPOS=-1", map[string]any{"__type": "RelativeMonthlyRecurrence:#Exchange", "Interval": 1, "DaysOfWeek": "WeekendDay", "DayOfWeekIndex": "Last"}},
		{"monthly last day", "FREQ=MONTHLY;BYDAY=SU,MO,TU,WE,TH,FR,SA;BYSETPOS=-1", map[string]any{"__type": "RelativeMonthlyRecurrence:#Exchange", "Interval": 1, "DaysOfWeek": "Day", "DayOfWeekIndex": "Last"}},
		{"yearly date", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29", map[string]any{"__type": "AbsoluteYearlyRecurrence:#Exchange", "Month": "February", "DayOfMonth": 29}},
		{"yearly relative", "FREQ=YEARLY;BYMONTH=11;BYDAY=TH;BYSETPOS=4", map[string]any{"__type": "RelativeYearlyRecurrence:#Exchange", "Month": "November", "DaysOfWeek": "Thursday", "DayOfWeekIndex": "Fourth"}},
		{"yearly first weekday", "FREQ=YEARLY;BYMONTH=10;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=1", map[string]any{"__type": "RelativeYearlyRecurrence:#Exchange", "Month": "October", "DaysOfWeek": "Weekday", "DayOfWeekIndex": "First"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newFake(t, func(string, map[string]any) any {
				return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
			})
			if _, err := p.CreateTask(context.Background(), bridge.Task{Due: &anchor, Recur: tc.rule}); err != nil {
				t.Fatal(err)
			}
			item := arr(f.last("CreateItem").Body, "Body", "Items")[0]
			rec := dig(item, "Recurrence")
			if str(rec, "__type") != "TaskRecurrenceType:#Exchange" {
				t.Fatalf("calendar recurrence used for task: %+v", rec)
			}
			// Request capture has passed through JSON, so normalize the expected map too.
			b, _ := json.Marshal(tc.pattern)
			var expected map[string]any
			_ = json.Unmarshal(b, &expected)
			if got := dig(rec, "RecurrencePattern"); !reflect.DeepEqual(got, expected) {
				t.Fatalf("native pattern = %#v, want %#v", got, expected)
			}
			if str(rec, "RecurrenceRange", "__type") != "NoEndRecurrence:#Exchange" || str(rec, "RecurrenceRange", "StartDate") != "2026-10-15" {
				t.Fatalf("due-only anchor lost: %+v", rec)
			}
			read := taskFromItem(asMap(item), "tasks")
			if read.Recur != tc.rule || read.RecurUnsupported || read.RecurStart == nil || read.RecurStart.Format("2006-01-02") != "2026-10-15" {
				t.Fatalf("native task roundtrip = %+v", read)
			}
		})
	}
}

func TestOWATaskRelativeNativeDaysRequireScalarEnum(t *testing.T) {
	anchor := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for _, rule := range []string{"FREQ=MONTHLY;BYDAY=MO,WE;BYSETPOS=1", "FREQ=YEARLY;BYMONTH=10;BYDAY=SU,FR;BYSETPOS=-1"} {
		f, p := newFake(t, func(string, map[string]any) any {
			return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
		})
		task := bridge.Task{ID: "cloud", Due: &anchor, Recur: rule, RecurSet: true}
		if _, err := p.CreateTask(context.Background(), task); err == nil {
			t.Errorf("relative multi-day rule has no native scalar enum: %s", rule)
		}
		if err := p.UpdateTask(context.Background(), task); err == nil {
			t.Errorf("relative multi-day update accepted: %s", rule)
		}
		if len(f.requests) != 0 {
			t.Errorf("unsupported relative list reached native service: %s", rule)
		}
	}
	for _, typ := range []string{"RelativeMonthlyRecurrence", "RelativeYearlyRecurrence"} {
		pattern := map[string]any{"__type": typ + ":#Exchange", "Interval": float64(1), "Month": "October", "DaysOfWeek": "Monday Wednesday", "DayOfWeekIndex": "First"}
		task := taskFromItem(map[string]any{"Recurrence": map[string]any{"__type": "TaskRecurrenceType:#Exchange", "RecurrencePattern": pattern, "RecurrenceRange": map[string]any{"__type": "NoEndRecurrence:#Exchange", "StartDate": "2026-10-15"}}}, "tasks")
		if !task.RecurUnsupported || task.Recur != "" {
			t.Errorf("invalid relative native list was accepted: %+v", task)
		}
	}
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
	})
	if _, err := p.CreateTask(context.Background(), bridge.Task{Due: &anchor, Recur: "FREQ=MONTHLY;BYDAY=FR,WE,MO,TH,TU;BYSETPOS=-1"}); err != nil {
		t.Fatal(err)
	}
	if got := str(arr(f.last("CreateItem").Body, "Body", "Items")[0], "Recurrence", "RecurrencePattern", "DaysOfWeek"); got != "Weekday" {
		t.Fatalf("unordered weekday set did not become scalar enum: %q", got)
	}
}

func TestOWATaskNativeRecurrenceRangesAndGroupedDays(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rangeItem map[string]any
		days      string
		want      string
	}{
		{"count weekdays", map[string]any{"__type": "NumberedRecurrence:#Exchange", "StartDate": "2026-10-15+03:00", "NumberOfOccurrences": float64(3)}, "Weekday", "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;WKST=MO;COUNT=3"},
		{"until weekend", map[string]any{"__type": "EndDateRecurrence:#Exchange", "StartDate": "2026-10-15+03:00", "EndDate": "2026-12-31+03:00"}, "WeekendDay", "FREQ=WEEKLY;BYDAY=SU,SA;WKST=MO;UNTIL=20261231"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]any{"Recurrence": map[string]any{"__type": "TaskRecurrenceType:#Exchange", "RecurrencePattern": map[string]any{"__type": "WeeklyRecurrence:#Exchange", "Interval": float64(1), "DaysOfWeek": tc.days, "FirstDayOfWeek": "Monday"}, "RecurrenceRange": tc.rangeItem}}
			got := taskFromItem(item, "tasks")
			if got.Recur != tc.want || got.RecurUnsupported || got.RecurStart == nil {
				t.Fatalf("native range read = %+v, want %s", got, tc.want)
			}
			if got.RecurTimeZone != "+03:00" {
				t.Fatalf("range timezone lost: %q", got.RecurTimeZone)
			}
			got.ID, got.RecurSet = "cloud", true
			f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
			if err := p.UpdateTask(context.Background(), got); err != nil {
				t.Fatal(err)
			}
			var written any
			for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
				if str(update, "Path", "FieldURI") == "Recurrence" {
					written = dig(update, "Item", "Recurrence", "RecurrenceRange")
				}
			}
			if !reflect.DeepEqual(written, tc.rangeItem) {
				t.Fatalf("native range changed on write: got %#v want %#v", written, tc.rangeItem)
			}
		})
	}
}

func TestOWATaskUnsupportedNativeRecurrenceIsPreserved(t *testing.T) {
	for _, typ := range []string{"DailyRegeneration", "WeeklyRegeneration", "MonthlyRegeneration", "YearlyRegeneration", "UnknownPattern"} {
		t.Run(typ, func(t *testing.T) {
			task := taskFromItem(map[string]any{"ItemId": map[string]any{"Id": "cloud"}, "Recurrence": map[string]any{"__type": "TaskRecurrenceType:#Exchange", "RecurrencePattern": map[string]any{"__type": typ + ":#Exchange", "Interval": float64(2)}, "RecurrenceRange": map[string]any{"__type": "NoEndRecurrence:#Exchange", "StartDate": "2026-10-15"}}}, "tasks")
			if !task.RecurUnsupported || task.Recur != "" {
				t.Fatalf("native completion-relative repeat was invented: %+v", task)
			}
			f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
			// An unrelated edit must not erase an unrepresentable cloud rule.
			task.Title = "Renamed"
			if err := p.UpdateTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
				if str(update, "Path", "FieldURI") == "Recurrence" {
					t.Fatalf("unrelated edit touched unsupported recurrence: %+v", update)
				}
			}
			task.RecurSet = true
			before := len(f.requests)
			if err := p.UpdateTask(context.Background(), task); err == nil {
				t.Fatal("unsupported recurrence mutation accepted")
			}
			if len(f.requests) != before {
				t.Fatal("unsupported recurrence mutation reached upstream")
			}
		})
	}
}

func TestOWATaskRecurrenceMixedOffsetsArePreserved(t *testing.T) {
	for _, startDate := range []string{"2026-06-01+03:00", "2026-06-01"} {
		item := map[string]any{"ItemId": map[string]any{"Id": "cloud"}, "Recurrence": map[string]any{"__type": "TaskRecurrenceType:#Exchange", "RecurrencePattern": map[string]any{"__type": "DailyRecurrence:#Exchange", "Interval": float64(1)}, "RecurrenceRange": map[string]any{"__type": "EndDateRecurrence:#Exchange", "StartDate": startDate, "EndDate": "2026-12-31+02:00"}}}
		task := taskFromItem(item, "tasks")
		if !task.RecurUnsupported || task.Recur != "" {
			t.Fatalf("different endpoint offsets would be rewritten: %+v", task)
		}
		task.Title = "Renamed"
		f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
		if err := p.UpdateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
			if str(update, "Path", "FieldURI") == "Recurrence" {
				t.Fatal("ordinary edit rewrote native endpoint offsets")
			}
		}
	}
}

func TestOWATaskTimedUntilWithOffsetFailsBeforeMutation(t *testing.T) {
	anchor := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
	if _, err := p.CreateTask(context.Background(), bridge.Task{Recur: "FREQ=DAILY;UNTIL=20261230T215959Z", RecurStart: &anchor, RecurTimeZone: "+03:00"}); err == nil {
		t.Fatal("UTC timed cutoff with native local-date offset was silently shortened")
	}
	if len(f.requests) != 0 {
		t.Fatal("ambiguous timed cutoff reached native service")
	}
}

func TestOWATaskTimedUntilChecksAllSuppliedTaskTimes(t *testing.T) {
	anchor := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	ten := anchor.Add(10 * time.Hour)
	fractional := ten.Add(500 * time.Millisecond)
	local := time.Date(2026, 10, 15, 10, 0, 0, 0, time.FixedZone("offset", 3*60*60))
	for _, tc := range []struct {
		name       string
		start, due *time.Time
		until      string
		wantError  bool
	}{
		{"start and due after cutoff", &ten, &ten, "20261030T090000Z", true},
		{"due after cutoff", nil, &ten, "20261030T090000Z", true},
		{"fractional start after cutoff", &fractional, &ten, "20261030T100000Z", true},
		{"equal cutoff", &ten, &ten, "20261030T100000Z", false},
		{"later cutoff", &ten, &ten, "20261030T110000Z", false},
		{"non UTC start hidden by range anchor", &local, &ten, "20261030T110000Z", true},
		{"non UTC due hidden by range anchor", &ten, &local, "20261030T110000Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newFake(t, func(string, map[string]any) any {
				return itemResp(map[string]any{"ItemId": map[string]any{"Id": "cloud"}})
			})
			task := bridge.Task{ID: "cloud", RecurStart: &anchor, Start: tc.start, Due: tc.due, Recur: "FREQ=DAILY;UNTIL=" + tc.until, RecurSet: true}
			if _, err := p.CreateTask(context.Background(), task); (err != nil) != tc.wantError {
				t.Errorf("create error=%v wantError=%v", err, tc.wantError)
			}
			if err := p.UpdateTask(context.Background(), task); (err != nil) != tc.wantError {
				t.Errorf("update error=%v wantError=%v", err, tc.wantError)
			}
			if tc.wantError && len(f.requests) != 0 {
				t.Fatal("unsafe timed cutoff reached native service")
			}
		})
	}
	f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
	task := bridge.Task{ID: "cloud", Title: "Renamed", RecurStart: &anchor, Start: &ten, Due: &ten, Recur: "FREQ=DAILY;UNTIL=20261030T090000Z", RecurSet: false}
	if err := p.UpdateTask(context.Background(), task); err != nil {
		t.Fatalf("unrelated edit validated an untouched recurrence: %v", err)
	}
	for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
		if str(update, "Path", "FieldURI") == "Recurrence" {
			t.Fatal("unrelated edit touched native recurrence")
		}
	}
}

func TestOWATaskRecurrenceRangeAnchorSurvivesUndatedRead(t *testing.T) {
	item := map[string]any{"ItemId": map[string]any{"Id": "cloud"}, "Recurrence": map[string]any{"__type": "TaskRecurrenceType:#Exchange", "RecurrencePattern": map[string]any{"__type": "DailyRecurrence:#Exchange", "Interval": float64(1)}, "RecurrenceRange": map[string]any{"__type": "NoEndRecurrence:#Exchange", "StartDate": "2026-10-15+03:00"}}}
	task := taskFromItem(item, "tasks")
	if task.Start != nil || task.Due != nil || task.RecurStart == nil {
		t.Fatalf("range anchor must not invent DTSTART/DUE: %+v", task)
	}
	task.RecurSet = true
	f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
	if err := p.UpdateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	for _, update := range arr(arr(f.last("UpdateItem").Body, "Body", "ItemChanges")[0], "Updates") {
		if str(update, "Path", "FieldURI") == "Recurrence" {
			if got := str(update, "Item", "Recurrence", "RecurrenceRange", "StartDate"); got != "2026-10-15+03:00" {
				t.Fatalf("native range anchor = %q", got)
			}
			return
		}
	}
	t.Fatal("recurrence update missing")
}
