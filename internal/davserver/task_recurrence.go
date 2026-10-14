package davserver

import (
	"sort"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func sameTaskRule(a, b string) bool {
	normalize := func(rule string) (string, bool) {
		rule = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(rule)), "RRULE:")
		if rule == "" {
			return "", true
		}
		terms := map[string]string{}
		for _, part := range strings.Split(rule, ";") {
			key, value, ok := strings.Cut(part, "=")
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if !ok || key == "" || value == "" || terms[key] != "" {
				return "", false
			}
			if strings.HasPrefix(key, "BY") {
				values := strings.Split(value, ",")
				sort.Strings(values)
				value = strings.Join(values, ",")
			}
			terms[key] = value
		}
		if terms["INTERVAL"] == "" {
			terms["INTERVAL"] = "1"
		}
		if terms["FREQ"] == "WEEKLY" && terms["WKST"] == "" {
			terms["WKST"] = "MO"
		}
		var parts []string
		for key, value := range terms {
			parts = append(parts, key+"="+value)
		}
		sort.Strings(parts)
		return strings.Join(parts, ";"), true
	}
	left, lok := normalize(a)
	right, rok := normalize(b)
	return lok && rok && left == right
}

// taskCalendarRule matches UNTIL's value type to the rendered task DTSTART.
func taskCalendarRule(task bridge.Task) string {
	rule := strings.TrimPrefix(task.Recur, "RRULE:")
	dateOnly := task.Due == nil || task.DueDateOnly
	if task.Start != nil {
		dateOnly = task.StartDateOnly
	}
	if dateOnly || rule == "" {
		return rule
	}
	loc := time.UTC
	if task.RecurStart != nil {
		loc = task.RecurStart.Location()
	}
	if task.RecurTimeZone != "" {
		zone := task.RecurTimeZone
		if zone == "Z" {
			loc = time.UTC
		} else if parsed, err := time.Parse("2006-01-02Z07:00", "2000-01-01"+zone); err == nil {
			loc = parsed.Location()
		} else if parsed, err := time.LoadLocation(bridge.FromWindowsZone(zone)); err == nil {
			loc = parsed
		}
	}
	parts := strings.Split(rule, ";")
	for i, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok || key != "UNTIL" || len(value) != 8 {
			continue
		}
		end, err := time.Parse("20060102", value)
		if err != nil {
			continue
		}
		cutoff := time.Date(end.Year(), end.Month(), end.Day(), 23, 59, 59, 0, loc)
		parts[i] = "UNTIL=" + fmtDateTime(cutoff, false)
	}
	return strings.Join(parts, ";")
}
