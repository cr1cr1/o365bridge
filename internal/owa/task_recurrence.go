package owa

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var taskWeekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
var taskWeekdayCodes = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}
var taskMonths = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
var taskWeekPositions = map[string]int{"First": 1, "Second": 2, "Third": 3, "Fourth": 4, "Last": -1}

// taskRecurrenceFromOWA does not turn completion-relative regeneration or an
// unknown native rule into a scheduled RRULE. The unsupported bit lets ordinary
// edits leave those rules untouched instead of confusing them with no repeat.
func taskRecurrenceFromOWA(item map[string]any) (rule string, anchor *time.Time, zone string, unsupported bool) {
	rec := asMap(item["Recurrence"])
	if rec == nil {
		return "", nil, "", item["Recurrence"] != nil
	}
	pat, rng := asMap(rec["RecurrencePattern"]), asMap(rec["RecurrenceRange"])
	start, startZone, ok := taskRecurrenceDate(str(rng, "StartDate"))
	if ok {
		anchor, zone = &start, startZone
	}
	fail := func() (string, *time.Time, string, bool) { return "", anchor, zone, true }
	if pat == nil || rng == nil || !ok {
		return fail()
	}
	var parts []string
	typ := str(pat, "__type")
	switch typ {
	case "DailyRecurrence:#Exchange":
		parts = []string{"FREQ=DAILY"}
	case "WeeklyRecurrence:#Exchange":
		parts = []string{"FREQ=WEEKLY"}
	case "AbsoluteMonthlyRecurrence:#Exchange", "RelativeMonthlyRecurrence:#Exchange":
		parts = []string{"FREQ=MONTHLY"}
	case "AbsoluteYearlyRecurrence:#Exchange", "RelativeYearlyRecurrence:#Exchange":
		parts = []string{"FREQ=YEARLY"}
	default:
		return fail()
	}
	if !strings.Contains(typ, "Yearly") {
		interval, valid := taskRecurrenceInteger(pat, "Interval")
		if !valid || interval < 1 {
			return fail()
		}
		if interval > 1 {
			parts = append(parts, "INTERVAL="+strconv.Itoa(interval))
		}
	}
	if strings.Contains(typ, "Yearly") {
		month := taskStringIndex(taskMonths, str(pat, "Month")) + 1
		if month == 0 {
			return fail()
		}
		parts = append(parts, "BYMONTH="+strconv.Itoa(month))
	}
	switch typ {
	case "AbsoluteMonthlyRecurrence:#Exchange", "AbsoluteYearlyRecurrence:#Exchange":
		day, valid := taskRecurrenceInteger(pat, "DayOfMonth")
		if !valid || day < 1 || day > 31 {
			return fail()
		}
		parts = append(parts, "BYMONTHDAY="+strconv.Itoa(day))
	case "WeeklyRecurrence:#Exchange", "RelativeMonthlyRecurrence:#Exchange", "RelativeYearlyRecurrence:#Exchange":
		dayValue := str(pat, "DaysOfWeek")
		// Relative patterns use one DayOfWeekType enum, unlike the weekly
		// pattern's list. Invalid native lists must not become editable rules.
		if typ != "WeeklyRecurrence:#Exchange" && taskStringIndex(taskWeekdays, dayValue) < 0 && dayValue != "Day" && dayValue != "Weekday" && dayValue != "WeekendDay" {
			return fail()
		}
		days, valid := taskNativeDays(dayValue)
		if !valid {
			return fail()
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","))
		if typ == "WeeklyRecurrence:#Exchange" {
			weekStart := taskStringIndex(taskWeekdays, str(pat, "FirstDayOfWeek"))
			if weekStart < 0 {
				return fail()
			}
			parts = append(parts, "WKST="+taskWeekdayCodes[weekStart])
		} else {
			position, valid := taskWeekPositions[str(pat, "DayOfWeekIndex")]
			if !valid {
				return fail()
			}
			parts = append(parts, "BYSETPOS="+strconv.Itoa(position))
		}
	}
	switch str(rng, "__type") {
	case "NoEndRecurrence:#Exchange":
	case "NumberedRecurrence:#Exchange":
		count, valid := taskRecurrenceInteger(rng, "NumberOfOccurrences")
		if !valid || count < 1 {
			return fail()
		}
		parts = append(parts, "COUNT="+strconv.Itoa(count))
	case "EndDateRecurrence:#Exchange":
		end, endZone, valid := taskRecurrenceDate(str(rng, "EndDate"))
		// One bridge timezone cannot preserve two independently zoned range
		// endpoints. Keep the native rule opaque rather than rewriting DST.
		if !valid || end.Before(start) || endZone != zone {
			return fail()
		}
		parts = append(parts, "UNTIL="+end.Format("20060102"))
	default:
		return fail()
	}
	return strings.Join(parts, ";"), anchor, zone, false
}

// taskRecurrenceToOWA accepts only RRULEs that can be represented without
// dropping a selector. The native TaskRecurrenceType is not CalendarItem's
// RecurrenceType, even though the pattern and range contracts are shared.
func taskRecurrenceToOWA(rule string, anchor time.Time, zone string, timings ...*time.Time) (map[string]any, error) {
	bad := func(detail string) (map[string]any, error) {
		return nil, fmt.Errorf("owa: unsupported task recurrence: %s", detail)
	}
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return nil, nil
	}
	if anchor.IsZero() {
		return bad("a start or due date is required")
	}
	if strings.HasPrefix(strings.ToUpper(rule), "RRULE:") {
		rule = rule[len("RRULE:"):]
	}
	values := map[string]string{}
	for _, part := range strings.Split(rule, ";") {
		key, value, ok := strings.Cut(part, "=")
		key, value = strings.ToUpper(strings.TrimSpace(key)), strings.ToUpper(strings.TrimSpace(value))
		if !ok || value == "" || values[key] != "" {
			return bad("malformed or duplicate rule term")
		}
		switch key {
		case "FREQ", "INTERVAL", "COUNT", "UNTIL", "BYDAY", "BYMONTHDAY", "BYMONTH", "BYSETPOS", "WKST":
		default:
			return bad("rule term " + key)
		}
		values[key] = value
	}
	interval := 1
	if value := values["INTERVAL"]; value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > (1<<31)-1 {
			return bad("invalid INTERVAL")
		}
		interval = n
	}
	freq := values["FREQ"]
	if freq == "DAILY" && values["BYDAY"] != "" && interval == 1 {
		freq = "WEEKLY" // Every selected weekday is a weekly native pattern.
	}
	pat := map[string]any{}
	switch freq {
	case "DAILY":
		if values["BYDAY"] != "" || values["BYMONTHDAY"] != "" || values["BYMONTH"] != "" || values["BYSETPOS"] != "" || values["WKST"] != "" {
			return bad("daily selectors")
		}
		pat["__type"], pat["Interval"] = "DailyRecurrence:#Exchange", interval
	case "WEEKLY":
		if values["BYMONTHDAY"] != "" || values["BYMONTH"] != "" || values["BYSETPOS"] != "" {
			return bad("weekly selectors")
		}
		dayValue := values["BYDAY"]
		if dayValue == "" {
			dayValue = taskWeekdayCodes[anchor.Weekday()]
		}
		days, position, valid := taskRuleDays(dayValue)
		if !valid || position != 0 {
			return bad("weekly BYDAY")
		}
		weekStart := values["WKST"]
		if weekStart == "" {
			weekStart = "MO" // RFC 5545's default, not the server locale.
		}
		index := taskStringIndex(taskWeekdayCodes, weekStart)
		if index < 0 {
			return bad("WKST")
		}
		pat["__type"], pat["Interval"], pat["DaysOfWeek"], pat["FirstDayOfWeek"] = "WeeklyRecurrence:#Exchange", interval, strings.Join(days, " "), taskWeekdays[index]
	case "MONTHLY", "YEARLY":
		if values["WKST"] != "" || (freq == "MONTHLY" && values["BYMONTH"] != "") || (freq == "YEARLY" && interval != 1) {
			return bad("monthly/yearly selectors or interval")
		}
		kind := "Monthly"
		if freq == "YEARLY" {
			kind = "Yearly"
			month := int(anchor.Month())
			if value := values["BYMONTH"]; value != "" {
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 || n > 12 {
					return bad("BYMONTH")
				}
				month = n
			} else if values["BYDAY"] != "" || values["BYMONTHDAY"] != "" {
				return bad("yearly day selectors require BYMONTH")
			}
			pat["Month"] = taskMonths[month-1]
		} else {
			pat["Interval"] = interval
		}
		if values["BYDAY"] != "" {
			if values["BYMONTHDAY"] != "" {
				return bad("combined BYDAY and BYMONTHDAY")
			}
			days, position, valid := taskRuleDays(values["BYDAY"])
			if !valid {
				return bad("BYDAY")
			}
			if value := values["BYSETPOS"]; value != "" {
				if position != 0 {
					return bad("combined ordinal BYDAY and BYSETPOS")
				}
				n, err := strconv.Atoi(value)
				if err != nil {
					return bad("BYSETPOS")
				}
				position = n
			}
			index := taskPositionName(position)
			if index == "" {
				return bad("unsupported BYDAY position")
			}
			dayValue := taskRelativeDayEnum(days)
			if dayValue == "" {
				return bad("relative BYDAY is not a native single day or day group")
			}
			pat["__type"], pat["DaysOfWeek"], pat["DayOfWeekIndex"] = "Relative"+kind+"Recurrence:#Exchange", dayValue, index
		} else {
			if values["BYSETPOS"] != "" {
				return bad("BYSETPOS without BYDAY")
			}
			day := anchor.Day()
			if value := values["BYMONTHDAY"]; value != "" {
				n, err := strconv.Atoi(value)
				if err != nil || n == 0 || n < -1 || n > 31 {
					return bad("BYMONTHDAY")
				}
				day = n
			}
			if day == -1 {
				pat["__type"], pat["DaysOfWeek"], pat["DayOfWeekIndex"] = "Relative"+kind+"Recurrence:#Exchange", "Day", "Last"
			} else {
				if freq == "YEARLY" {
					month := time.Month(taskStringIndex(taskMonths, str(pat, "Month")) + 1)
					if time.Date(2000, month, day, 0, 0, 0, 0, time.UTC).Month() != month {
						return bad("impossible yearly month/day")
					}
				}
				pat["__type"], pat["DayOfMonth"] = "Absolute"+kind+"Recurrence:#Exchange", day
			}
		}
	default:
		return bad("FREQ " + freq)
	}
	startDate := anchor.Format("2006-01-02") + zone
	if _, _, valid := taskRecurrenceDate(startDate); !valid {
		return bad("invalid range date or timezone")
	}
	rng := map[string]any{"__type": "NoEndRecurrence:#Exchange", "StartDate": startDate}
	if values["COUNT"] != "" && values["UNTIL"] != "" {
		return bad("COUNT and UNTIL are mutually exclusive")
	}
	if value := values["COUNT"]; value != "" {
		count, err := strconv.Atoi(value)
		if err != nil || count < 1 || count > (1<<31)-1 {
			return bad("COUNT")
		}
		rng["__type"], rng["NumberOfOccurrences"] = "NumberedRecurrence:#Exchange", count
	} else if value := values["UNTIL"]; value != "" {
		end, err := time.Parse("20060102", value)
		if err != nil {
			if zone != "" && zone != "Z" && zone != "+00:00" {
				return bad("timed UNTIL with a non-UTC range timezone")
			}
			end, err = time.Parse("20060102T150405Z", value)
			if err == nil {
				cutoff := end.Sub(end.Truncate(24 * time.Hour))
				// The range anchor can be midnight even when DTSTART/DUE has a
				// later clock. Native date-only ranges must include every supplied
				// task timing, or they could add an occurrence excluded by UNTIL.
				for _, candidate := range append([]*time.Time{&anchor}, timings...) {
					if candidate == nil {
						continue
					}
					if _, offset := candidate.Zone(); offset != 0 {
						return bad("timed UNTIL with a non-UTC task timezone")
					}
					utc := candidate.UTC()
					if utc.Sub(utc.Truncate(24*time.Hour)) > cutoff {
						return bad("UNTIL excludes its last date's task")
					}
				}
			}
		}
		if err != nil || end.Format("2006-01-02") < anchor.Format("2006-01-02") {
			return bad("UNTIL")
		}
		rng["__type"], rng["EndDate"] = "EndDateRecurrence:#Exchange", end.Format("2006-01-02")+zone
	}
	return map[string]any{"__type": "TaskRecurrenceType:#Exchange", "RecurrencePattern": pat, "RecurrenceRange": rng}, nil
}

func taskRecurrenceDate(value string) (time.Time, string, bool) {
	for _, layout := range []string{"2006-01-02", "2006-01-02Z07:00", time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		parsed, err := time.Parse(layout, value)
		if err != nil || parsed.Year() < 2 {
			continue
		}
		zone := ""
		if len(value) > 10 && (value[10] == '+' || value[10] == '-' || value[10] == 'Z') {
			zone = value[10:]
		} else if strings.HasSuffix(value, "Z") || strings.LastIndexAny(value, "+-") > 9 {
			zone = parsed.Format("Z07:00")
		}
		return time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC), zone, true
	}
	return time.Time{}, "", false
}

func taskRecurrenceInteger(item map[string]any, key string) (int, bool) {
	switch value := item[key].(type) {
	case int:
		return value, true
	case float64:
		integer := int(value)
		return integer, float64(integer) == value
	default:
		return 0, false
	}
}

func taskNativeDays(value string) ([]string, bool) {
	groups := map[string][]string{"Day": taskWeekdayCodes, "Weekday": {"MO", "TU", "WE", "TH", "FR"}, "WeekendDay": {"SU", "SA"}}
	if days, ok := groups[value]; ok {
		return days, true
	}
	var out []string
	seen := map[string]bool{}
	for _, day := range strings.Fields(value) {
		index := taskStringIndex(taskWeekdays, day)
		if index < 0 || seen[day] {
			return nil, false
		}
		seen[day] = true
		out = append(out, taskWeekdayCodes[index])
	}
	return out, len(out) != 0
}

func taskRuleDays(value string) (days []string, position int, valid bool) {
	seen := map[string]bool{}
	for _, term := range strings.Split(value, ",") {
		if len(term) < 2 {
			return nil, 0, false
		}
		code := term[len(term)-2:]
		index := taskStringIndex(taskWeekdayCodes, code)
		if index < 0 || seen[code] {
			return nil, 0, false
		}
		seen[code] = true
		if len(term) > 2 {
			n, err := strconv.Atoi(term[:len(term)-2])
			if err != nil || taskPositionName(n) == "" || strings.Contains(value, ",") {
				return nil, 0, false
			}
			position = n
		}
		days = append(days, taskWeekdays[index])
	}
	return days, position, len(days) != 0
}

// taskRelativeDayEnum receives the distinct weekday names already validated by
// taskRuleDays. Only these sets have a scalar native relative-pattern value.
func taskRelativeDayEnum(days []string) string {
	if len(days) == 1 {
		return days[0]
	}
	set := make(map[string]bool, len(days))
	for _, day := range days {
		set[day] = true
	}
	switch {
	case len(set) == 7:
		return "Day"
	case len(set) == 5 && !set["Sunday"] && !set["Saturday"]:
		return "Weekday"
	case len(set) == 2 && set["Sunday"] && set["Saturday"]:
		return "WeekendDay"
	default:
		return ""
	}
}

func taskPositionName(position int) string {
	for name, value := range taskWeekPositions {
		if value == position {
			return name
		}
	}
	return ""
}

func taskStringIndex(values []string, value string) int {
	for index, candidate := range values {
		if candidate == value {
			return index
		}
	}
	return -1
}
