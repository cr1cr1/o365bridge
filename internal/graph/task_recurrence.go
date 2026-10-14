package graph

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type graphTaskRecurrence struct {
	Pattern graphTaskRecurrencePattern `json:"pattern"`
	Range   graphTaskRecurrenceRange   `json:"range"`
}

type graphTaskRecurrencePattern struct {
	Type           string   `json:"type"`
	Interval       int      `json:"interval"`
	DayOfMonth     int      `json:"dayOfMonth,omitempty"`
	DaysOfWeek     []string `json:"daysOfWeek,omitempty"`
	FirstDayOfWeek string   `json:"firstDayOfWeek,omitempty"`
	Index          string   `json:"index,omitempty"`
	Month          int      `json:"month,omitempty"`
}

type graphTaskRecurrenceRange struct {
	Type                string `json:"type"`
	StartDate           string `json:"startDate"`
	EndDate             string `json:"endDate,omitempty"`
	NumberOfOccurrences int    `json:"numberOfOccurrences,omitempty"`
	RecurrenceTimeZone  string `json:"recurrenceTimeZone,omitempty"`
}

var taskWeekdayNames = [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
var taskWeekdayCodes = [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}
var taskWeekIndexes = [...]string{"first", "second", "third", "fourth", "last"}

func taskWeekday(value string, from, to []string) (string, error) {
	for i, day := range from {
		if value == day {
			return to[i], nil
		}
	}
	return "", fmt.Errorf("graph: unsupported repeat weekday %q", value)
}

func taskRepeatPosition(index string) (int, error) {
	if index == "" {
		return 1, nil // Graph's documented default for relative patterns.
	}
	for i, name := range taskWeekIndexes {
		if index == name {
			if i == 4 {
				return -1, nil
			}
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("graph: unsupported repeat week index %q", index)
}

func validTaskRepeatInt(n int) bool { return n > 0 && int64(n) <= 1<<31-1 }

func (p graphTaskRecurrencePattern) rrule() (string, error) {
	if !validTaskRepeatInt(p.Interval) {
		return "", fmt.Errorf("graph: invalid repeat interval")
	}
	var frequency string
	switch p.Type {
	case "daily":
		frequency = "DAILY"
	case "weekly":
		frequency = "WEEKLY"
	case "absoluteMonthly", "relativeMonthly":
		frequency = "MONTHLY"
	case "absoluteYearly", "relativeYearly":
		frequency = "YEARLY"
	default:
		return "", fmt.Errorf("graph: unsupported native task repeat type %q", p.Type)
	}
	parts := []string{"FREQ=" + frequency, "INTERVAL=" + strconv.Itoa(p.Interval)}
	if frequency == "YEARLY" {
		if p.Month < 1 || p.Month > 12 {
			return "", fmt.Errorf("graph: invalid repeat month")
		}
		parts = append(parts, "BYMONTH="+strconv.Itoa(p.Month))
	}
	switch p.Type {
	case "absoluteMonthly", "absoluteYearly":
		if p.DayOfMonth < 1 || p.DayOfMonth > 31 {
			return "", fmt.Errorf("graph: invalid repeat month day")
		}
		parts = append(parts, "BYMONTHDAY="+strconv.Itoa(p.DayOfMonth))
	case "weekly", "relativeMonthly", "relativeYearly":
		if len(p.DaysOfWeek) == 0 || (p.Type != "weekly" && len(p.DaysOfWeek) != 1) {
			// Graph's multi-day relative selection is not an arbitrary RRULE BYSETPOS.
			return "", fmt.Errorf("graph: unrepresentable native relative task repeat")
		}
		var days []string
		seen := map[string]bool{}
		for _, day := range p.DaysOfWeek {
			code, err := taskWeekday(day, taskWeekdayNames[:], taskWeekdayCodes[:])
			if err != nil || seen[code] {
				return "", fmt.Errorf("graph: invalid native repeat weekdays")
			}
			seen[code] = true
			days = append(days, code)
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","))
		if p.Type == "weekly" {
			first := p.FirstDayOfWeek
			if first == "" {
				first = "sunday"
			}
			code, err := taskWeekday(first, taskWeekdayNames[:], taskWeekdayCodes[:])
			if err != nil {
				return "", err
			}
			parts = append(parts, "WKST="+code)
		} else {
			position, err := taskRepeatPosition(p.Index)
			if err != nil {
				return "", err
			}
			parts = append(parts, "BYSETPOS="+strconv.Itoa(position))
		}
	}
	return strings.Join(parts, ";"), nil
}

func (r graphTaskRecurrence) taskRepeat() (string, *time.Time, string, bool) {
	tz := r.Range.RecurrenceTimeZone
	anchor, err := time.ParseInLocation("2006-01-02", r.Range.StartDate, zoneLocation(tz))
	if err != nil || anchor.IsZero() {
		return "", nil, tz, true
	}
	rule, err := r.Pattern.rrule()
	if err != nil {
		return "", &anchor, tz, true
	}
	switch r.Range.Type {
	case "noEnd":
	case "numbered":
		if !validTaskRepeatInt(r.Range.NumberOfOccurrences) {
			return "", &anchor, tz, true
		}
		rule += ";COUNT=" + strconv.Itoa(r.Range.NumberOfOccurrences)
	case "endDate":
		end, err := time.Parse("2006-01-02", r.Range.EndDate)
		if err != nil || r.Range.EndDate < r.Range.StartDate {
			return "", &anchor, tz, true
		}
		rule += ";UNTIL=" + end.Format("20060102")
	default:
		return "", &anchor, tz, true
	}
	return rule, &anchor, tz, false
}

func taskRepeatParts(rule string) (map[string]string, error) {
	rule = strings.TrimSpace(rule)
	if strings.HasPrefix(strings.ToUpper(rule), "RRULE:") {
		rule = rule[6:]
	}
	parts := map[string]string{}
	for _, part := range strings.Split(rule, ";") {
		key, value, ok := strings.Cut(part, "=")
		key, value = strings.ToUpper(key), strings.ToUpper(value)
		if !ok || key == "" || value == "" || parts[key] != "" {
			return nil, fmt.Errorf("graph: malformed or duplicate task RRULE field")
		}
		parts[key] = value
	}
	return parts, nil
}

func taskRepeatInteger(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || !validTaskRepeatInt(n) {
		return 0, fmt.Errorf("graph: invalid task repeat number %q", value)
	}
	return n, nil
}

func taskRepeatRelative(day, position string) (string, string, error) {
	if len(day) < 2 || strings.Contains(day, ",") {
		return "", "", fmt.Errorf("graph: unrepresentable task repeat weekdays")
	}
	code := day[len(day)-2:]
	name, err := taskWeekday(code, taskWeekdayCodes[:], taskWeekdayNames[:])
	if err != nil {
		return "", "", err
	}
	ordinal := day[:len(day)-2]
	if ordinal != "" {
		if position != "" {
			return "", "", fmt.Errorf("graph: conflicting task repeat week positions")
		}
		position = ordinal
	}
	n, err := strconv.Atoi(position)
	if err != nil || (n != -1 && (n < 1 || n > 4)) {
		return "", "", fmt.Errorf("graph: unsupported task repeat week position")
	}
	if n == -1 {
		return name, "last", nil
	}
	return name, taskWeekIndexes[n-1], nil
}

func taskRecurrenceJSON(t bridge.Task) (*graphTaskRecurrence, error) {
	parts, err := taskRepeatParts(t.Recur)
	if err != nil {
		return nil, err
	}
	anchor := t.RecurStart
	if anchor == nil {
		anchor = t.Start
	}
	if anchor == nil {
		anchor = t.Due
	}
	if anchor == nil || anchor.IsZero() {
		return nil, fmt.Errorf("graph: task repeat requires a range start date")
	}
	take := func(key string) string {
		value := parts[key]
		delete(parts, key)
		return value
	}
	frequency := take("FREQ")
	interval := 1
	if value := take("INTERVAL"); value != "" {
		interval, err = taskRepeatInteger(value)
		if err != nil {
			return nil, err
		}
	}
	r := &graphTaskRecurrence{
		Pattern: graphTaskRecurrencePattern{Interval: interval},
		Range:   graphTaskRecurrenceRange{Type: "noEnd", StartDate: anchor.Format("2006-01-02"), RecurrenceTimeZone: t.RecurTimeZone},
	}
	count, until := take("COUNT"), take("UNTIL")
	if count != "" && until != "" {
		return nil, fmt.Errorf("graph: task repeat cannot combine COUNT and UNTIL")
	}
	if count != "" {
		r.Range.Type = "numbered"
		r.Range.NumberOfOccurrences, err = taskRepeatInteger(count)
		if err != nil {
			return nil, err
		}
	}
	if until != "" {
		loc := anchor.Location()
		if t.RecurTimeZone != "" {
			loc = zoneLocation(t.RecurTimeZone)
		}
		end, err := time.ParseInLocation("20060102", until, loc)
		if err != nil {
			end, err = time.Parse("20060102T150405Z", until)
			if err != nil {
				return nil, fmt.Errorf("graph: invalid task repeat end date")
			}
			if t.RecurTimeZone != "" && t.RecurTimeZone != "UTC" {
				return nil, fmt.Errorf("graph: timed task UNTIL requires UTC recurrence")
			}
			cutoffTime := end.Sub(time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC))
			// A date range must not admit an occurrence after the original instant cutoff.
			for _, candidate := range []*time.Time{anchor, t.Start, t.Due} {
				if candidate == nil {
					continue
				}
				if candidate.Location() != time.UTC {
					return nil, fmt.Errorf("graph: timed task UNTIL requires UTC anchors")
				}
				anchorTime := candidate.Sub(time.Date(candidate.Year(), candidate.Month(), candidate.Day(), 0, 0, 0, 0, time.UTC))
				if cutoffTime < anchorTime {
					return nil, fmt.Errorf("graph: task UNTIL time cannot be represented by an inclusive date range")
				}
			}
		}
		if err != nil || end.Format("2006-01-02") < r.Range.StartDate {
			return nil, fmt.Errorf("graph: invalid task repeat end date")
		}
		r.Range.Type, r.Range.EndDate = "endDate", end.Format("2006-01-02")
	}
	switch frequency {
	case "DAILY":
		r.Pattern.Type = "daily"
	case "WEEKLY":
		r.Pattern.Type = "weekly"
		days := take("BYDAY")
		if days == "" {
			days = taskWeekdayCodes[anchor.Weekday()]
		}
		seen := map[string]bool{}
		for _, code := range strings.Split(days, ",") {
			name, err := taskWeekday(code, taskWeekdayCodes[:], taskWeekdayNames[:])
			if err != nil || seen[name] {
				return nil, fmt.Errorf("graph: invalid task repeat weekdays")
			}
			seen[name] = true
			r.Pattern.DaysOfWeek = append(r.Pattern.DaysOfWeek, name)
		}
		first := take("WKST")
		if first == "" {
			first = "MO" // RFC 5545 default, unlike Graph's Sunday default.
		}
		r.Pattern.FirstDayOfWeek, err = taskWeekday(first, taskWeekdayCodes[:], taskWeekdayNames[:])
		if err != nil {
			return nil, err
		}
	case "MONTHLY", "YEARLY":
		absoluteType, relativeType := "absoluteMonthly", "relativeMonthly"
		if frequency == "YEARLY" {
			absoluteType, relativeType = "absoluteYearly", "relativeYearly"
			r.Pattern.Month = int(anchor.Month())
			month := take("BYMONTH")
			if month == "" && (parts["BYDAY"] != "" || parts["BYMONTHDAY"] != "" || parts["BYSETPOS"] != "") {
				return nil, fmt.Errorf("graph: yearly task repeat selectors require an explicit month")
			}
			if month != "" {
				r.Pattern.Month, err = taskRepeatInteger(month)
				if err != nil || r.Pattern.Month > 12 {
					return nil, fmt.Errorf("graph: invalid task repeat month")
				}
			}
		}
		day, position, monthDay := take("BYDAY"), take("BYSETPOS"), take("BYMONTHDAY")
		if day != "" {
			if monthDay != "" {
				return nil, fmt.Errorf("graph: conflicting task repeat month selectors")
			}
			name, index, err := taskRepeatRelative(day, position)
			if err != nil {
				return nil, err
			}
			r.Pattern.Type, r.Pattern.DaysOfWeek, r.Pattern.Index = relativeType, []string{name}, index
		} else {
			if position != "" {
				return nil, fmt.Errorf("graph: repeat week position requires one weekday")
			}
			r.Pattern.Type, r.Pattern.DayOfMonth = absoluteType, anchor.Day()
			if monthDay != "" {
				r.Pattern.DayOfMonth, err = taskRepeatInteger(monthDay)
				if err != nil || r.Pattern.DayOfMonth > 31 {
					return nil, fmt.Errorf("graph: invalid task repeat month day")
				}
			}
		}
	default:
		return nil, fmt.Errorf("graph: unsupported task repeat frequency %q", frequency)
	}
	if len(parts) != 0 {
		return nil, fmt.Errorf("graph: unsupported task RRULE fields")
	}
	return r, nil
}
