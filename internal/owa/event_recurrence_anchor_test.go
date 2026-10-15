package owa

import (
	"context"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWAEventRecurrenceRequiresRealStartAndZone(t *testing.T) {
	for _, tt := range []struct {
		name   string
		modify func(*bridge.Event)
	}{
		{"missing start", func(ev *bridge.Event) { ev.Start = time.Time{} }},
		{"invalid zone", func(ev *bridge.Event) { ev.TimeZone = "Not/AZone" }},
		{"all day datetime until", func(ev *bridge.Event) { ev.AllDay = true; ev.Recurrence = "FREQ=DAILY;UNTIL=20260310T033000Z" }},
		{"duplicate until", func(ev *bridge.Event) { ev.Recurrence = "FREQ=DAILY;UNTIL=20260310T033000Z;UNTIL=20260311T033000Z" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, p := newFake(t, nil)
			ev := recurrenceEvent(t)
			ev.Recurrence = "FREQ=DAILY"
			tt.modify(&ev)
			if _, err := p.CreateEvent(context.Background(), ev); err == nil {
				t.Error("unsafe anchor/range accepted")
			}
			ev.RecurrenceSet = true
			if err := p.UpdateEvent(context.Background(), ev); err == nil {
				t.Error("unsafe update anchor/range accepted")
			}
			if len(f.requests) != 0 {
				t.Fatalf("unsafe recurrence reached network: %d requests", len(f.requests))
			}
		})
	}
}

func TestOWAEventRecurrenceUsesStartLocationWithoutExplicitZone(t *testing.T) {
	ev := recurrenceEvent(t)
	loc, err := time.LoadLocation(ev.TimeZone)
	if err != nil {
		t.Fatal(err)
	}
	ev.Start = ev.Start.In(loc)
	ev.End = ev.End.In(loc)
	ev.TimeZone = ""
	ev.Recurrence = "FREQ=WEEKLY"
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "native"}})
	})
	if _, err := p.CreateEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	req := f.last("CreateItem").Body
	item := arr(req, "Body", "Items")[0]
	if str(item, "Start") != "2026-03-03T23:30:00" || str(item, "StartTimeZone", "Id") != "Eastern Standard Time" || str(req, "Header", "TimeZoneContext", "TimeZoneDefinition", "Id") != "Eastern Standard Time" || str(item, "Recurrence", "RecurrenceRange", "StartDate") != "2026-03-03" {
		t.Fatalf("local DTSTART and native range disagree: %+v", req)
	}
	ev.RecurrenceSet = true
	if err := p.UpdateEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	update := f.last("UpdateItem").Body
	if str(update, "Header", "TimeZoneContext", "TimeZoneDefinition", "Id") != "Eastern Standard Time" {
		t.Errorf("update lost start location: %+v", update)
	}
}

func TestOWAEventMalformedNativeRecurrenceIsOpaque(t *testing.T) {
	for _, tt := range []struct {
		name   string
		modify func(map[string]any)
	}{
		{"root type", func(rec map[string]any) { rec["__type"] = "TaskRecurrenceType:#Exchange" }},
		{"missing range start", func(rec map[string]any) { delete(asMap(rec["RecurrenceRange"]), "StartDate") }},
		{"missing range type", func(rec map[string]any) { delete(asMap(rec["RecurrenceRange"]), "__type") }},
		{"fractional interval", func(rec map[string]any) { asMap(rec["RecurrencePattern"])["Interval"] = 1.5 }},
		{"negative count", func(rec map[string]any) { asMap(rec["RecurrenceRange"])["NumberOfOccurrences"] = -1 }},
		{"reversed end range", func(rec map[string]any) {
			rec["RecurrenceRange"] = map[string]any{"__type": "EndDateRecurrence:#Exchange", "StartDate": "2026-10-15", "EndDate": "2026-10-14"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := calendarReadbackItem("native", "uid")
			tt.modify(asMap(item["Recurrence"]))
			got := eventFromItem(item, "calendar")
			if got.Recurrence != "" || !got.RecurrenceUnsupported {
				t.Errorf("malformed native rule not opaque: %+v", got)
			}
		})
	}
}
