package owa

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestOWAEventNativeIntervalBounds(t *testing.T) {
	for _, tt := range []struct {
		name, rule string
		max        int
		pattern    map[string]any
	}{
		{"daily", "FREQ=DAILY", 999, map[string]any{"__type": "DailyRecurrence:#Exchange"}},
		{"weekly", "FREQ=WEEKLY;BYDAY=TU;WKST=MO", 99, map[string]any{"__type": "WeeklyRecurrence:#Exchange", "DaysOfWeek": "Tuesday", "FirstDayOfWeek": "Monday"}},
		{"absolute monthly", "FREQ=MONTHLY;BYMONTHDAY=3", 99, map[string]any{"__type": "AbsoluteMonthlyRecurrence:#Exchange", "DayOfMonth": 3}},
		{"relative monthly", "FREQ=MONTHLY;BYDAY=1TU", 99, map[string]any{"__type": "RelativeMonthlyRecurrence:#Exchange", "DaysOfWeek": "Tuesday", "DayOfWeekIndex": "First"}},
	} {
		for _, interval := range []int{0, 1, tt.max, tt.max + 1} {
			t.Run(fmt.Sprintf("%s/%d", tt.name, interval), func(t *testing.T) {
				valid := interval >= 1 && interval <= tt.max
				pattern := map[string]any{"Interval": interval}
				for key, value := range tt.pattern {
					pattern[key] = value
				}
				item := calendarReadbackItem("native", "series")
				asMap(item["Recurrence"])["RecurrencePattern"] = pattern
				f, p := newFake(t, func(action string, req map[string]any) any {
					switch action {
					case "FindItem":
						return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": item["ItemId"]}}}})
					case "CreateItem":
						written := arr(req, "Body", "Items")[0]
						if num(written, "Recurrence", "RecurrencePattern", "Interval") != float64(interval) {
							t.Errorf("wrong native interval: %+v", written)
						}
					case "UpdateItem":
						found := false
						for _, u := range arr(arr(req, "Body", "ItemChanges")[0], "Updates") {
							if str(u, "Path", "FieldURI") == "Recurrence" {
								found = true
								if num(u, "Item", "Recurrence", "RecurrencePattern", "Interval") != float64(interval) {
									t.Errorf("wrong native update interval: %+v", u)
								}
							}
						}
						if !found {
							t.Error("explicit repeat update missing")
						}
					}
					return itemResp(item)
				})
				ev := recurrenceEvent(t)
				ev.Recurrence = fmt.Sprintf("%s;INTERVAL=%d", tt.rule, interval)
				_, createErr := p.CreateEvent(context.Background(), ev)
				ev.RecurrenceSet = true
				updateErr := p.UpdateEvent(context.Background(), ev)
				if (createErr == nil) != valid || (updateErr == nil) != valid {
					t.Errorf("interval write valid=%v: create=%v update=%v", valid, createErr, updateErr)
				}
				if !valid && len(f.requests) != 0 {
					t.Errorf("invalid interval reached network: %d requests", len(f.requests))
				}
				events, err := p.ListEvents(context.Background(), "calendar", time.Time{}, time.Time{})
				if err != nil || len(events) != 1 {
					t.Fatalf("read native: %+v, %v", events, err)
				}
				if events[0].RecurrenceUnsupported == valid || (events[0].Recurrence != "") != valid {
					t.Errorf("native interval read valid=%v: %+v", valid, events[0])
				}
			})
		}
	}
}
