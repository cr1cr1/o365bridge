package owa

import (
	"fmt"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func eventRecurrenceStart(ev bridge.Event) (time.Time, error) {
	if ev.Start.IsZero() {
		return time.Time{}, fmt.Errorf("owa: event recurrence requires DTSTART")
	}
	if ev.TimeZone == "" {
		return ev.Start, nil
	}
	loc, err := time.LoadLocation(ev.TimeZone)
	if err != nil {
		return time.Time{}, fmt.Errorf("owa: event recurrence timezone: %w", err)
	}
	return ev.Start.In(loc), nil
}

// Events share Exchange's scheduled patterns with tasks, but not their anchor
// or UNTIL rules. Normalize those explicitly before using the strict converter.
func eventRecurrenceToOWA(ev bridge.Event) (map[string]any, error) {
	rule := strings.TrimSpace(ev.Recurrence)
	if rule == "" {
		return nil, nil
	}
	if ev.RecurrenceUnsupported {
		return nil, fmt.Errorf("owa: native event recurrence cannot be safely replaced")
	}
	start, err := eventRecurrenceStart(ev)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.ToUpper(rule), "RRULE:") {
		rule = rule[len("RRULE:"):]
		if strings.TrimSpace(rule) == "" {
			return nil, fmt.Errorf("owa: empty event RRULE")
		}
	}
	terms := strings.Split(rule, ";")
	for i, term := range terms {
		key, value, ok := strings.Cut(term, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "UNTIL") {
			continue
		}
		value = strings.ToUpper(strings.TrimSpace(value))
		var end time.Time
		if ev.AllDay {
			end, err = time.Parse("20060102", value)
		} else {
			var cutoff time.Time
			cutoff, err = time.Parse("20060102T150405Z", value)
			if err == nil {
				local := cutoff.In(start.Location())
				end = time.Date(local.Year(), local.Month(), local.Day(), start.Hour(), start.Minute(), start.Second(), start.Nanosecond(), start.Location())
				// Exchange EndDate is inclusive: never include a local start after UNTIL.
				if end.After(cutoff) {
					end = end.AddDate(0, 0, -1)
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("owa: event UNTIL must match DTSTART's DATE or UTC DATE-TIME type")
		}
		terms[i] = "UNTIL=" + end.Format("20060102")
	}
	rec, err := taskRecurrenceToOWA(strings.Join(terms, ";"), start, "")
	if err != nil {
		return nil, fmt.Errorf("owa: event recurrence: %w", err)
	}
	if eventRecurrencePatternUnsupported(asMap(rec["RecurrencePattern"])) {
		return nil, fmt.Errorf("owa: event recurrence exceeds native interval bounds or would clamp invalid dates")
	}
	rec["__type"] = "RecurrenceType:#Exchange"
	return rec, nil
}

// Enforce documented native interval bounds. Exchange also clamps missing
// month days, so keep the conservative RFC subset that cannot clamp.
func eventRecurrencePatternUnsupported(pattern map[string]any) bool {
	day, _ := taskRecurrenceInteger(pattern, "DayOfMonth")
	interval, _ := taskRecurrenceInteger(pattern, "Interval")
	switch str(pattern, "__type") {
	case "DailyRecurrence:#Exchange":
		return interval > 999
	case "WeeklyRecurrence:#Exchange", "RelativeMonthlyRecurrence:#Exchange":
		return interval > 99
	case "AbsoluteMonthlyRecurrence:#Exchange":
		return interval > 99 || day > 28
	case "AbsoluteYearlyRecurrence:#Exchange":
		month := time.Month(taskStringIndex(taskMonths, str(pattern, "Month")) + 1)
		return time.Date(2001, month, day, 0, 0, 0, 0, time.UTC).Month() != month
	default:
		return false
	}
}

func eventRecurrenceFromOWA(item map[string]any, ev bridge.Event) (string, bool) {
	rec := asMap(item["Recurrence"])
	if rec == nil {
		return "", item["Recurrence"] != nil
	}
	if typ, present := rec["__type"]; present && typ != "RecurrenceType:#Exchange" {
		return "", true
	}
	rng := asMap(rec["RecurrenceRange"])
	normalized := make(map[string]any, len(rng))
	for key, value := range rng {
		normalized[key] = value
	}
	// Range dates describe the event's local calendar, not task fixed-offset anchors.
	for _, key := range []string{"StartDate", "EndDate"} {
		if value, exists := normalized[key]; exists {
			text, ok := value.(string)
			date, _, valid := taskRecurrenceDate(text)
			if !ok || !valid {
				return "", true
			}
			normalized[key] = date.Format("2006-01-02")
		}
	}
	rule, _, _, unsupported := taskRecurrenceFromOWA(map[string]any{"Recurrence": map[string]any{
		"RecurrencePattern": rec["RecurrencePattern"], "RecurrenceRange": normalized,
	}})
	if unsupported || eventRecurrencePatternUnsupported(asMap(rec["RecurrencePattern"])) {
		return "", true
	}
	if ev.AllDay {
		return rule, false
	}
	start, err := eventRecurrenceStart(ev)
	if err != nil {
		return "", true
	}
	terms := strings.Split(rule, ";")
	for i, term := range terms {
		if !strings.HasPrefix(term, "UNTIL=") {
			continue
		}
		end, err := time.Parse("20060102", strings.TrimPrefix(term, "UNTIL="))
		if err != nil {
			return "", true
		}
		last := time.Date(end.Year(), end.Month(), end.Day(), start.Hour(), start.Minute(), start.Second(), 0, start.Location())
		terms[i] = "UNTIL=" + last.UTC().Format("20060102T150405Z")
	}
	return strings.Join(terms, ";"), false
}
