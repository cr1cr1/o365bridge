package davserver

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func eventRepeatZone(ev bridge.Event) string {
	zone := ev.TimeZone
	if zone == "" {
		zone = ev.Start.Location().String()
	}
	if zone == "" {
		zone = "UTC"
	}
	zone = bridge.FromWindowsZone(zone)
	switch zone {
	case "UTC", "Etc/UTC", "Etc/GMT", "GMT":
		return "UTC"
	}
	return zone
}

func sameEventRepeatAnchor(a, b bridge.Event) bool {
	return a.AllDay == b.AllDay && a.Start.Truncate(time.Second).Equal(b.Start.Truncate(time.Second)) && eventRepeatZone(a) == eventRepeatZone(b)
}

// eventRuleForComparison expands only native-supported anchor defaults/ordinals.
func eventRuleForComparison(ev bridge.Event) (string, bool) {
	rule := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(ev.Recurrence)), "RRULE:")
	if rule == "" {
		return "", strings.TrimSpace(ev.Recurrence) == ""
	}
	terms := map[string]string{}
	for _, part := range strings.Split(rule, ";") {
		key, value, ok := strings.Cut(part, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" || terms[key] != "" {
			return "", false
		}
		terms[key] = value
	}
	loc, err := time.LoadLocation(eventRepeatZone(ev))
	if err != nil {
		return "", false
	}
	start := ev.Start.In(loc)
	if terms["FREQ"] == "DAILY" && terms["BYDAY"] != "" && (terms["INTERVAL"] == "" || terms["INTERVAL"] == "1") {
		terms["FREQ"] = "WEEKLY"
	}
	if terms["FREQ"] == "WEEKLY" && terms["BYDAY"] == "" {
		terms["BYDAY"] = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}[start.Weekday()]
	}
	if terms["FREQ"] == "YEARLY" && terms["BYMONTH"] == "" && terms["BYDAY"] == "" && terms["BYMONTHDAY"] == "" && terms["BYSETPOS"] == "" {
		terms["BYMONTH"] = strconv.Itoa(int(start.Month()))
	}
	if (terms["FREQ"] == "MONTHLY" || terms["FREQ"] == "YEARLY" && terms["BYMONTH"] != "") && terms["BYDAY"] == "" && terms["BYMONTHDAY"] == "" && terms["BYSETPOS"] == "" {
		terms["BYMONTHDAY"] = strconv.Itoa(start.Day())
	}
	if terms["FREQ"] == "MONTHLY" || terms["FREQ"] == "YEARLY" {
		day := terms["BYDAY"]
		if len(day) > 2 && !strings.Contains(day, ",") {
			position, err := strconv.Atoi(day[:len(day)-2])
			if err != nil || position != -1 && (position < 1 || position > 4) {
				return "", false
			}
			canonical := strconv.Itoa(position)
			if terms["BYSETPOS"] != "" && terms["BYSETPOS"] != canonical {
				return "", false
			}
			terms["BYDAY"], terms["BYSETPOS"] = day[len(day)-2:], canonical
		}
	}
	var parts []string
	for key, value := range terms {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ";"), true
}

// prepareEventRepeatUpdate avoids rewriting native series on unrelated edits.
func prepareEventRepeatUpdate(ev, old bridge.Event) (bridge.Event, error) {
	if ev.AllDay && old.AllDay && ev.TimeZone == "" && old.TimeZone != "" && (old.Recurrence != "" || old.RecurrenceUnsupported) {
		loc, err := time.LoadLocation(bridge.FromWindowsZone(old.TimeZone))
		if err != nil {
			return ev, fmt.Errorf("native repeating event timezone cannot be safely edited")
		}
		// DATE fields name calendar days, not UTC instants; restore their native zone.
		localDate := func(t time.Time) time.Time {
			if t.IsZero() {
				return t
			}
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		}
		ev.TimeZone = old.TimeZone
		ev.Start, ev.End = localDate(ev.Start), localDate(ev.End)
	}
	if old.RecurrenceUnsupported {
		if ev.Recurrence != "" || !sameEventRepeatAnchor(ev, old) {
			return ev, fmt.Errorf("native event repetition cannot be safely replaced or moved")
		}
		ev.RecurrenceSet, ev.RecurrenceUnsupported = false, true
		return ev, nil
	}
	if ev.RecurrenceUnsupported {
		return ev, fmt.Errorf("unsupported repetition marker does not match the native event")
	}
	left, lok := eventRuleForComparison(ev)
	right, rok := eventRuleForComparison(old)
	if lok && rok && sameRecurrenceRule(left, right) && (left == "" && right == "" || sameEventRepeatAnchor(ev, old)) {
		ev.RecurrenceSet = false
		ev.Recurrence = old.Recurrence
	}
	return ev, nil
}
