package owa

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func recurrenceEvent(t *testing.T) bridge.Event {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 3, 23, 30, 0, 0, loc)
	return bridge.Event{ID: "event", UID: "series", Start: start.UTC(), End: start.Add(time.Hour).UTC(), TimeZone: loc.String()}
}

// These exact native contracts deliberately reject the old omitted intervals/selectors.
func TestOWAEventRecurrenceNativePatterns(t *testing.T) {
	tests := []struct {
		rule, read string
		pattern    map[string]any
	}{
		{"FREQ=DAILY", "FREQ=DAILY", map[string]any{"__type": "DailyRecurrence:#Exchange", "Interval": float64(1)}},
		{"FREQ=WEEKLY", "FREQ=WEEKLY;BYDAY=TU;WKST=MO", map[string]any{"__type": "WeeklyRecurrence:#Exchange", "Interval": float64(1), "DaysOfWeek": "Tuesday", "FirstDayOfWeek": "Monday"}},
		{"FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;WKST=SU", "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;WKST=SU", map[string]any{"__type": "WeeklyRecurrence:#Exchange", "Interval": float64(2), "DaysOfWeek": "Monday Friday", "FirstDayOfWeek": "Sunday"}},
		{"FREQ=MONTHLY", "FREQ=MONTHLY;BYMONTHDAY=3", map[string]any{"__type": "AbsoluteMonthlyRecurrence:#Exchange", "Interval": float64(1), "DayOfMonth": float64(3)}},
		{"FREQ=MONTHLY;BYDAY=-1FR", "FREQ=MONTHLY;BYDAY=FR;BYSETPOS=-1", map[string]any{"__type": "RelativeMonthlyRecurrence:#Exchange", "Interval": float64(1), "DaysOfWeek": "Friday", "DayOfWeekIndex": "Last"}},
		{"FREQ=YEARLY", "FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=3", map[string]any{"__type": "AbsoluteYearlyRecurrence:#Exchange", "Month": "March", "DayOfMonth": float64(3)}},
		{"FREQ=YEARLY;BYMONTH=11;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=2", "FREQ=YEARLY;BYMONTH=11;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=2", map[string]any{"__type": "RelativeYearlyRecurrence:#Exchange", "Month": "November", "DaysOfWeek": "Weekday", "DayOfWeekIndex": "Second"}},
	}
	for _, tt := range tests {
		t.Run(tt.rule, func(t *testing.T) {
			_, p := newFake(t, func(action string, req map[string]any) any {
				item := arr(req, "Body", "Items")[0]
				if action != "CreateItem" || str(item, "Recurrence", "__type") != "RecurrenceType:#Exchange" || !reflect.DeepEqual(asMap(dig(item, "Recurrence", "RecurrencePattern")), tt.pattern) || str(item, "Recurrence", "RecurrenceRange", "__type") != "NoEndRecurrence:#Exchange" || str(item, "Recurrence", "RecurrenceRange", "StartDate") != "2026-03-03" {
					t.Errorf("invalid native calendar schema: %+v", item)
					return okBody(map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorInvalidPropertySet"})
				}
				got := eventFromItem(asMap(item), "calendar")
				if got.RecurrenceUnsupported || got.Recurrence != tt.read {
					t.Errorf("native selectors lost on read: %+v; want %s", got, tt.read)
				}
				return itemResp(map[string]any{"ItemId": map[string]any{"Id": "native-id"}})
			})
			ev := recurrenceEvent(t)
			ev.Recurrence = tt.rule
			if got, err := p.CreateEvent(context.Background(), ev); err != nil || got.ID != "native-id" {
				t.Fatalf("create: %+v, %v", got, err)
			}
		})
	}
}

func TestOWAEventRecurrenceRejectsBeforeNetwork(t *testing.T) {
	for _, rule := range []string{"RRULE:", "RRULE: ", "FREQ=HOURLY", "FREQ=DAILY;BYHOUR=4", "FREQ=DAILY;FREQ=WEEKLY", "FREQ=DAILY;INTERVAL=0", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;COUNT=2;UNTIL=20260310T033000Z", "FREQ=MONTHLY;BYDAY=MO,FR;BYSETPOS=1", "FREQ=YEARLY;INTERVAL=2", "FREQ=WEEKLY;BYDAY=MO,MO", "FREQ=WEEKLY;WKST=XX", "FREQ=DAILY;UNTIL=20260310", "FREQ=DAILY;UNTIL=20260310T033000", "FREQ=DAILY;UNTIL=20260303T033000Z", "FREQ=DAILY;"} {
		t.Run(rule, func(t *testing.T) {
			f, p := newFake(t, nil)
			ev := recurrenceEvent(t)
			ev.Recurrence = rule
			if _, err := p.CreateEvent(context.Background(), ev); err == nil {
				t.Error("invalid recurrence create accepted")
			}
			ev.RecurrenceSet = true
			if err := p.UpdateEvent(context.Background(), ev); err == nil {
				t.Error("invalid recurrence update accepted")
			}
			if len(f.requests) != 0 {
				t.Fatalf("invalid rule reached network: %+v", f.requests)
			}
		})
	}
}

func TestOWAEventRecurrenceUntilLocalDateAndReadback(t *testing.T) {
	for _, tt := range []struct {
		until, end, read string
		allDay           bool
	}{
		{"20260310T032959Z", "2026-03-08", "20260309T033000Z", false},
		{"20260310T033000Z", "2026-03-09", "20260310T033000Z", false},
		{"20260310T080000Z", "2026-03-09", "20260310T033000Z", false},
		{"20260310", "2026-03-10", "20260310", true},
	} {
		t.Run(tt.until, func(t *testing.T) {
			ev := recurrenceEvent(t)
			ev.AllDay = tt.allDay
			ev.Recurrence = "FREQ=DAILY;UNTIL=" + tt.until
			_, p := newFake(t, func(_ string, req map[string]any) any {
				item := asMap(arr(req, "Body", "Items")[0])
				rec := asMap(item["Recurrence"])
				if str(rec, "RecurrenceRange", "EndDate") != tt.end {
					t.Errorf("end range = %+v; want %s", rec, tt.end)
				}
				item["ItemId"] = map[string]any{"Id": "native"}
				got := eventFromItem(item, "calendar")
				if got.RecurrenceUnsupported || got.Recurrence != "FREQ=DAILY;UNTIL="+tt.read {
					t.Errorf("readback: %+v", got)
				}
				return itemResp(item)
			})
			if _, err := p.CreateEvent(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOWAEventRecurrenceUpdateIntent(t *testing.T) {
	for _, tt := range []struct {
		name, rule, typ  string
		set, unsupported bool
		reject           bool
	}{
		{"ordinary edit", "FREQ=DAILY", "", false, false, false},
		{"opaque ordinary edit", "", "", false, true, false},
		{"replace", "FREQ=WEEKLY;BYDAY=MO;COUNT=4", "SetItemField:#Exchange", true, false, false},
		{"clear", "", "DeleteItemField:#Exchange", true, false, false},
		{"opaque replace", "FREQ=DAILY", "", true, true, true},
		{"opaque clear", "", "", true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, p := newFake(t, func(_ string, _ map[string]any) any { return okBody(nil) })
			ev := recurrenceEvent(t)
			ev.Recurrence = tt.rule
			ev.RecurrenceSet = tt.set
			ev.RecurrenceUnsupported = tt.unsupported
			err := p.UpdateEvent(context.Background(), ev)
			if tt.reject {
				if err == nil || len(f.requests) != 0 {
					t.Fatalf("unsupported mutation: %v, requests=%d", err, len(f.requests))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			req := f.last("UpdateItem").Body
			found := false
			for _, update := range arr(arr(req, "Body", "ItemChanges")[0], "Updates") {
				if strings.Contains(str(update, "Path", "FieldURI"), "Recurrence") {
					found = true
					if str(update, "Path", "FieldURI") != "Recurrence" || str(update, "__type") != tt.typ {
						t.Errorf("wrong recurrence update path/type: %+v", update)
					}
					if tt.rule != "" && (str(update, "Item", "Recurrence", "__type") != "RecurrenceType:#Exchange" || num(update, "Item", "Recurrence", "RecurrenceRange", "NumberOfOccurrences") != 4) {
						t.Errorf("wrong recurrence replacement: %+v", update)
					}
				}
			}
			if found != tt.set {
				t.Errorf("recurrence touched = %v, want %v", found, tt.set)
			}
			if tt.set && str(req, "Header", "TimeZoneContext", "TimeZoneDefinition", "Id") != "Eastern Standard Time" {
				t.Errorf("missing timezone context: %+v", req)
			}
		})
	}
}

func TestOWAEventCreateRequiresNativeID(t *testing.T) {
	for _, response := range []any{okBody(nil), itemResp(map[string]any{"UID": "not-an-item-id"})} {
		_, p := newFake(t, func(string, map[string]any) any { return response })
		if _, err := p.CreateEvent(context.Background(), recurrenceEvent(t)); err == nil {
			t.Fatal("creation without native ID reported success")
		}
	}
}

func TestOWAEventNativeEndRangeUsesEventZoneAcrossDST(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		item := map[string]any{
			"ItemId": map[string]any{"Id": "native"},
			"Start":  "2026-03-03T23:30:00", "End": "2026-03-04T00:30:00",
			"StartTimeZone": map[string]any{"Id": "Eastern Standard Time"}, "IsAllDayEvent": allDay,
			"Recurrence": map[string]any{
				"__type":            "RecurrenceType:#Exchange",
				"RecurrencePattern": map[string]any{"__type": "DailyRecurrence:#Exchange", "Interval": 1},
				"RecurrenceRange":   map[string]any{"__type": "EndDateRecurrence:#Exchange", "StartDate": "2026-03-03-05:00", "EndDate": "2026-03-09-04:00"},
			},
		}
		if allDay {
			item["Start"], item["End"] = "2026-03-03T00:00:00", "2026-03-04T00:00:00"
		}
		_, p := newFake(t, func(action string, _ map[string]any) any {
			if action == "FindItem" {
				return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": item["ItemId"]}}}})
			}
			return itemResp(item)
		})
		events, err := p.ListEvents(context.Background(), "calendar", time.Time{}, time.Time{})
		want := "FREQ=DAILY;UNTIL=20260310T033000Z"
		if allDay {
			want = "FREQ=DAILY;UNTIL=20260309"
		}
		if err != nil || len(events) != 1 || events[0].RecurrenceUnsupported || events[0].Recurrence != want {
			t.Fatalf("native local end date allDay=%v: %+v, %v; want %s", allDay, events, err, want)
		}
	}
}

func TestOWAEventUnknownNativeRecurrenceIsOpaque(t *testing.T) {
	for _, typ := range []string{"DailyRegenerationPattern:#Exchange", "NotARecurrence:#Exchange", "MonthlySomething:#Exchange"} {
		item := calendarReadbackItem("native", "uid")
		asMap(asMap(item["Recurrence"])["RecurrencePattern"])["__type"] = typ
		got := eventFromItem(item, "calendar")
		if got.Recurrence != "" || !got.RecurrenceUnsupported {
			t.Errorf("unknown native rule invented: %+v", got)
		}
	}
}
