package owa

import (
	"context"
	"testing"
	"time"
)

func TestOWAEventRejectsClampingRecurrenceBeforeNetwork(t *testing.T) {
	for _, tt := range []struct {
		name, rule string
		start      time.Time
	}{
		{"monthly Jan31 default", "FREQ=MONTHLY;COUNT=3", time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)},
		{"monthly explicit29", "FREQ=MONTHLY;BYMONTHDAY=29;COUNT=3", time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)},
		{"monthly explicit30", "FREQ=MONTHLY;BYMONTHDAY=30;COUNT=3", time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)},
		{"monthly explicit31", "FREQ=MONTHLY;BYMONTHDAY=31;COUNT=3", time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)},
		{"yearly leap default", "FREQ=YEARLY;COUNT=3", time.Date(2028, 2, 29, 10, 0, 0, 0, time.UTC)},
		{"yearly explicit Feb29", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;COUNT=3", time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)},
		{"yearly invalid Feb30", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30;COUNT=3", time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)},
		{"yearly invalid Apr31", "FREQ=YEARLY;BYMONTH=4;BYMONTHDAY=31;COUNT=3", time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, p := newFake(t, func(string, map[string]any) any {
				return itemResp(map[string]any{"ItemId": map[string]any{"Id": "native"}})
			})
			ev := recurrenceEvent(t)
			ev.Start = tt.start
			ev.End = tt.start.Add(time.Hour)
			ev.TimeZone = "UTC"
			ev.Recurrence = tt.rule
			if _, err := p.CreateEvent(context.Background(), ev); err == nil {
				t.Error("clamping native rule would change RFC skip semantics")
			}
			ev.RecurrenceSet = true
			if err := p.UpdateEvent(context.Background(), ev); err == nil {
				t.Error("clamping recurrence update accepted")
			}
			if len(f.requests) != 0 {
				t.Fatalf("nonrepresentable recurrence reached network: %d requests", len(f.requests))
			}
		})
	}
}

func TestOWAEventNativeClampingRecurrenceIsOpaqueAndPreserved(t *testing.T) {
	for _, pat := range []map[string]any{
		{"__type": "AbsoluteMonthlyRecurrence:#Exchange", "Interval": 1, "DayOfMonth": 29},
		{"__type": "AbsoluteMonthlyRecurrence:#Exchange", "Interval": 1, "DayOfMonth": 30},
		{"__type": "AbsoluteMonthlyRecurrence:#Exchange", "Interval": 1, "DayOfMonth": 31},
		{"__type": "AbsoluteYearlyRecurrence:#Exchange", "Month": "February", "DayOfMonth": 29},
		{"__type": "AbsoluteYearlyRecurrence:#Exchange", "Month": "February", "DayOfMonth": 30},
		{"__type": "AbsoluteYearlyRecurrence:#Exchange", "Month": "April", "DayOfMonth": 31},
	} {
		item := calendarReadbackItem("native", "series")
		rec := asMap(item["Recurrence"])
		rec["RecurrencePattern"] = pat
		f, p := newFake(t, func(action string, req map[string]any) any {
			if action == "FindItem" {
				return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": item["ItemId"]}}}})
			}
			if action == "UpdateItem" {
				for _, u := range arr(arr(req, "Body", "ItemChanges")[0], "Updates") {
					if str(u, "Path", "FieldURI") == "Recurrence" {
						t.Errorf("ordinary edit touched clamping native repeat: %+v", u)
					}
				}
			}
			return itemResp(item)
		})
		events, err := p.ListEvents(context.Background(), "calendar", time.Time{}, time.Time{})
		if err != nil || len(events) != 1 {
			t.Fatalf("read native: %+v, %v", events, err)
		}
		ev := events[0]
		if ev.Recurrence != "" || !ev.RecurrenceUnsupported {
			t.Errorf("native clamp invented skipping RRULE: %+v", ev)
		}
		ev.Summary = "Ordinary edit"
		if err := p.UpdateEvent(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		before := len(f.requests)
		ev.RecurrenceSet = true
		if err := p.UpdateEvent(context.Background(), ev); err == nil || len(f.requests) != before {
			t.Errorf("opaque repeat mutation not rejected before network: %v", err)
		}
	}
}

func TestOWAEventNonClampingBoundaryPatternsRemainSupported(t *testing.T) {
	for _, rule := range []string{"FREQ=MONTHLY;BYMONTHDAY=28", "FREQ=MONTHLY;BYMONTHDAY=-1", "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=31", "FREQ=YEARLY;BYMONTH=4;BYMONTHDAY=30", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=28", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=-1"} {
		t.Run(rule, func(t *testing.T) {
			_, p := newFake(t, func(_ string, req map[string]any) any {
				item := asMap(arr(req, "Body", "Items")[0])
				item["ItemId"] = map[string]any{"Id": "native"}
				if ev := eventFromItem(item, "calendar"); ev.RecurrenceUnsupported || ev.Recurrence == "" {
					t.Errorf("non-clamping boundary incorrectly opaque: %+v", ev)
				}
				return itemResp(item)
			})
			ev := recurrenceEvent(t)
			ev.Recurrence = rule
			if _, err := p.CreateEvent(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOWAEventRecurrenceAllowsAbsentRootTypeOnly(t *testing.T) {
	for _, tt := range []struct {
		name        string
		change      func(map[string]any)
		unsupported bool
	}{
		{"absent root", func(rec map[string]any) { delete(rec, "__type") }, false},
		{"explicit wrong root", func(rec map[string]any) { rec["__type"] = "TaskRecurrenceType:#Exchange" }, true},
		{"explicit empty root", func(rec map[string]any) { rec["__type"] = "" }, true},
		{"explicit null root", func(rec map[string]any) { rec["__type"] = nil }, true},
		{"missing pattern type", func(rec map[string]any) { delete(rec, "__type"); delete(asMap(rec["RecurrencePattern"]), "__type") }, true},
		{"missing range type", func(rec map[string]any) { delete(rec, "__type"); delete(asMap(rec["RecurrenceRange"]), "__type") }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := calendarReadbackItem("native", "series")
			tt.change(asMap(item["Recurrence"]))
			_, p := newFake(t, func(action string, _ map[string]any) any {
				if action == "FindItem" {
					return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": item["ItemId"]}}}})
				}
				return itemResp(item)
			})
			events, err := p.ListEvents(context.Background(), "calendar", time.Time{}, time.Time{})
			if err != nil || len(events) != 1 {
				t.Fatalf("read: %+v, %v", events, err)
			}
			got := events[0]
			if got.RecurrenceUnsupported != tt.unsupported || (!tt.unsupported && got.Recurrence != "FREQ=DAILY;INTERVAL=2;COUNT=3") || (tt.unsupported && got.Recurrence != "") {
				t.Fatalf("native root compatibility: %+v", got)
			}
		})
	}
}
