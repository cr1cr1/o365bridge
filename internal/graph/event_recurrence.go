package graph

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// Event patterns share Graph's scheduled recurrence schema, but not task date semantics.
type graphEventRecurrence graphTaskRecurrence

func eventRepeatLocation(zone string) (*time.Location, error) {
	if zone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(bridge.FromWindowsZone(zone))
	if err != nil {
		return nil, fmt.Errorf("graph: unknown event recurrence timezone %q", zone)
	}
	return loc, nil
}

func eventPatternSupported(p graphTaskRecurrencePattern) bool {
	// Exchange clamps invalid absolute month days; RRULE skips them instead.
	if p.Type == "absoluteMonthly" && p.DayOfMonth > 28 {
		return false
	}
	if p.Type == "absoluteYearly" {
		if p.Month < 1 || p.Month > 12 || p.DayOfMonth < 1 {
			return false
		}
		if time.Date(2025, time.Month(p.Month), p.DayOfMonth, 0, 0, 0, 0, time.UTC).Month() != time.Month(p.Month) {
			return false
		}
	}
	switch p.Type {
	case "daily":
		return p.DayOfMonth == 0 && p.Month == 0 && len(p.DaysOfWeek) == 0 && p.Index == "" && p.FirstDayOfWeek == ""
	case "weekly":
		return p.DayOfMonth == 0 && p.Month == 0 && p.Index == ""
	case "absoluteMonthly", "absoluteYearly":
		return len(p.DaysOfWeek) == 0 && p.Index == "" && p.FirstDayOfWeek == "" && (p.Type == "absoluteYearly" || p.Month == 0)
	case "relativeMonthly", "relativeYearly":
		return p.DayOfMonth == 0 && p.FirstDayOfWeek == "" && (p.Type == "relativeYearly" || p.Month == 0)
	}
	return false
}

func eventRecurrenceJSON(ev bridge.Event) (*graphEventRecurrence, error) {
	if ev.Recurrence == "" {
		return nil, nil
	}
	if ev.Start.IsZero() {
		return nil, fmt.Errorf("graph: event recurrence requires a start")
	}
	loc, err := eventRepeatLocation(ev.TimeZone)
	if err != nil {
		return nil, err
	}
	anchor := ev.Start.In(loc)
	parts, err := taskRepeatParts(ev.Recurrence)
	if err != nil {
		return nil, err
	}
	until := parts["UNTIL"]
	if until != "" && parts["COUNT"] != "" {
		return nil, fmt.Errorf("graph: event repeat cannot combine COUNT and UNTIL")
	}
	delete(parts, "UNTIL")
	var fields []string
	for key, value := range parts {
		fields = append(fields, key+"="+value)
	}
	// Reuse only the scheduled pattern/count parser; event range anchors and UNTIL are local.
	native, err := taskRecurrenceJSON(bridge.Task{Recur: strings.Join(fields, ";"), RecurStart: &anchor})
	if err != nil {
		return nil, err
	}
	if !eventPatternSupported(native.Pattern) {
		return nil, fmt.Errorf("graph: event repeat selectors cannot be represented faithfully")
	}
	native.Range.RecurrenceTimeZone = toGraphDT(anchor, ev.TimeZone).TimeZone
	if until != "" {
		var end time.Time
		if ev.AllDay {
			end, err = time.ParseInLocation("20060102", until, loc)
		} else {
			var cutoff time.Time
			cutoff, err = time.Parse("20060102T150405Z", until)
			if err == nil {
				local := cutoff.In(loc)
				end = time.Date(local.Year(), local.Month(), local.Day(), anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), loc)
				// Graph endDate is inclusive: a cutoff before this day's occurrence excludes that day.
				if end.After(cutoff) {
					end = end.AddDate(0, 0, -1)
				}
			}
		}
		if err != nil || end.Format("2006-01-02") < native.Range.StartDate {
			return nil, fmt.Errorf("graph: invalid or unrepresentable event UNTIL")
		}
		native.Range.Type, native.Range.EndDate = "endDate", end.Format("2006-01-02")
	}
	r := graphEventRecurrence(*native)
	return &r, nil
}

func (r graphEventRecurrence) eventRepeat(ev bridge.Event) (string, string, bool) {
	// Native responses serialize ignored enum defaults as well as meaningful selectors.
	if r.Pattern.Index == "first" && r.Pattern.Type != "relativeMonthly" && r.Pattern.Type != "relativeYearly" {
		r.Pattern.Index = ""
	}
	if r.Pattern.FirstDayOfWeek == "sunday" && r.Pattern.Type != "weekly" {
		r.Pattern.FirstDayOfWeek = ""
	}
	if (r.Range.Type == "noEnd" || r.Range.Type == "numbered") && r.Range.EndDate == "0001-01-01" {
		r.Range.EndDate = ""
	}
	zone := r.Range.RecurrenceTimeZone
	if zone == "" {
		zone = ev.TimeZone
	}
	zone = bridge.FromWindowsZone(zone)
	loc, err := eventRepeatLocation(zone)
	if err != nil || ev.Start.IsZero() {
		return "", ev.TimeZone, true
	}
	anchor := ev.Start.In(loc)
	start, err := time.ParseInLocation("2006-01-02", r.Range.StartDate, loc)
	if err != nil || start.Format("2006-01-02") != anchor.Format("2006-01-02") || !eventPatternSupported(r.Pattern) {
		return "", zone, true
	}
	rule, err := r.Pattern.rrule()
	if err != nil {
		return "", zone, true
	}
	switch r.Range.Type {
	case "noEnd":
		if r.Range.EndDate != "" || r.Range.NumberOfOccurrences != 0 {
			return "", zone, true
		}
	case "numbered":
		if !validTaskRepeatInt(r.Range.NumberOfOccurrences) || r.Range.EndDate != "" {
			return "", zone, true
		}
		rule += ";COUNT=" + strconv.Itoa(r.Range.NumberOfOccurrences)
	case "endDate":
		end, err := time.ParseInLocation("2006-01-02", r.Range.EndDate, loc)
		if err != nil || r.Range.EndDate < r.Range.StartDate || r.Range.NumberOfOccurrences != 0 {
			return "", zone, true
		}
		if ev.AllDay {
			rule += ";UNTIL=" + end.Format("20060102")
		} else {
			end = time.Date(end.Year(), end.Month(), end.Day(), anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), loc)
			rule += ";UNTIL=" + end.UTC().Format("20060102T150405Z")
		}
	default:
		return "", zone, true
	}
	return rule, bridge.FromWindowsZone(zone), false
}
