package davserver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// taskAlarm holds wire properties until all task dates have been read.
// A native task can represent one non-repeating reminder instant only.
type taskAlarm struct {
	seen                        bool
	actionSeen, descriptionSeen bool
	trigger                     *icalProp
	starts, dues                []icalProp
}

func (a *taskAlarm) apply(p icalProp) error {
	if p.invalidParams {
		return fmt.Errorf("davserver: invalid task alarm parameters")
	}
	switch p.Name {
	case "ACTION":
		if a.actionSeen || !strings.EqualFold(p.Value, "DISPLAY") {
			return fmt.Errorf("davserver: task alarm requires one DISPLAY action")
		}
		a.actionSeen = true
	case "DESCRIPTION":
		if a.descriptionSeen {
			return fmt.Errorf("davserver: duplicate task alarm DESCRIPTION")
		}
		a.descriptionSeen = true
	case "TRIGGER":
		if a.trigger != nil {
			return fmt.Errorf("davserver: duplicate task alarm TRIGGER")
		}
		a.trigger = &p
	case "REPEAT", "DURATION":
		return fmt.Errorf("davserver: repeated task alarms are not supported")
	case "ATTACH", "ATTENDEE", "SUMMARY":
		return fmt.Errorf("davserver: unsupported DISPLAY task alarm property")
	}
	return nil
}

func (a *taskAlarm) reminder() (*time.Time, error) {
	if !a.seen {
		return nil, nil
	}
	if !a.actionSeen || !a.descriptionSeen || a.trigger == nil {
		return nil, fmt.Errorf("davserver: task alarm requires ACTION, DESCRIPTION and TRIGGER")
	}
	p := *a.trigger
	value, hasValue := p.Params["VALUE"]
	_, hasRelated := p.Params["RELATED"]
	_, hasZone := p.Params["TZID"]
	if hasZone {
		return nil, fmt.Errorf("davserver: task alarm TRIGGER must not have TZID")
	}
	if strings.EqualFold(value, "DATE-TIME") {
		if hasRelated || len(p.Value) != 16 {
			return nil, fmt.Errorf("davserver: absolute task alarm requires UTC DATE-TIME without RELATED")
		}
		t, err := time.Parse("20060102T150405Z", p.Value)
		if err != nil || t.Year() < 1 {
			return nil, fmt.Errorf("davserver: invalid absolute task alarm")
		}
		return &t, nil
	}
	if hasValue && !strings.EqualFold(value, "DURATION") {
		return nil, fmt.Errorf("davserver: unsupported task alarm TRIGGER value type")
	}
	anchors := a.starts
	if hasRelated {
		switch strings.ToUpper(p.Params["RELATED"]) {
		case "START":
		case "END":
			anchors = a.dues
		default:
			return nil, fmt.Errorf("davserver: invalid task alarm RELATED")
		}
	}
	// RFC 5545 also permits DTSTART+DURATION for END; Task has no duration field.
	if len(anchors) != 1 {
		return nil, fmt.Errorf("davserver: relative task alarm requires one explicit DTSTART or DUE anchor")
	}
	anchor, err := taskAlarmAnchor(anchors[0])
	if err != nil {
		return nil, err
	}
	reminder, err := applyTaskAlarmDuration(anchor, p.Value)
	if err != nil {
		return nil, err
	}
	return &reminder, nil
}

func taskAlarmAnchor(p icalProp) (time.Time, error) {
	invalid := func() (time.Time, error) {
		return time.Time{}, fmt.Errorf("davserver: relative task alarm requires an explicitly zoned DATE-TIME anchor")
	}
	if p.invalidParams {
		return invalid()
	}
	if value, ok := p.Params["VALUE"]; ok && !strings.EqualFold(value, "DATE-TIME") {
		return invalid()
	}
	zone, hasZone := p.Params["TZID"]
	if !hasZone {
		if len(p.Value) != 16 {
			return invalid()
		}
		t, err := time.Parse("20060102T150405Z", p.Value)
		if err != nil || t.Year() < 1 {
			return invalid()
		}
		return t, nil
	}
	if zone == "" || zone == "Local" || len(p.Value) != 15 {
		return invalid()
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return invalid()
	}
	t, err := time.ParseInLocation("20060102T150405", p.Value, loc)
	if err != nil || t.Year() < 1 || t.Format("20060102T150405") != p.Value {
		return invalid()
	}
	// Do not let Go choose an unspecified side of a clock-change overlap.
	wall, _ := time.Parse("20060102T150405", p.Value)
	start, end := t.ZoneBounds()
	if !start.IsZero() {
		start = start.Add(-time.Second)
	}
	for _, adjacent := range []time.Time{start, end} {
		if adjacent.IsZero() {
			continue
		}
		_, offset := adjacent.In(loc).Zone()
		other := wall.Add(-time.Duration(offset) * time.Second).In(loc)
		if !other.Equal(t) && other.Format("20060102T150405") == p.Value {
			return invalid()
		}
	}
	return t, nil
}

var taskAlarmDurationPattern = regexp.MustCompile(`^([+-]?)P(?:(\d+)W|(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?)$`)

func applyTaskAlarmDuration(anchor time.Time, value string) (time.Time, error) {
	invalid := func() (time.Time, error) {
		return time.Time{}, fmt.Errorf("davserver: invalid or unrepresentable task alarm duration")
	}
	parts := taskAlarmDurationPattern.FindStringSubmatch(value)
	if parts == nil {
		return invalid()
	}
	if parts[2] == "" && parts[3] == "" && parts[4] == "" && parts[5] == "" && parts[6] == "" {
		return invalid()
	}
	if strings.Contains(value, "T") && parts[4] == "" && parts[5] == "" && parts[6] == "" {
		return invalid()
	}
	// RFC 5545 §3.3.6's hour production only permits seconds after minutes.
	if parts[4] != "" && parts[6] != "" && parts[5] == "" {
		return invalid()
	}
	const maxDays = 3652059 // Calendar dates in the absolute wire range, years 1..9999.
	const maxSeconds = int64(1<<63-1) / int64(time.Second)
	var days, seconds int64
	for i, part := range parts[2:] {
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return invalid()
		}
		switch i {
		case 0:
			if n > maxDays/7 {
				return invalid()
			}
			days = n * 7
		case 1:
			if n > maxDays {
				return invalid()
			}
			days = n
		default:
			unit := []int64{3600, 60, 1}[i-2]
			if n > (maxSeconds-seconds)/unit {
				return invalid()
			}
			seconds += n * unit
		}
	}
	sign := int64(1)
	if parts[1] == "-" {
		sign = -1
	}
	// Nominal days/weeks precede elapsed HMS, preserving wall time across DST.
	if days != 0 {
		wall := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), anchor.Hour(), anchor.Minute(), anchor.Second(), 0, time.UTC).AddDate(0, 0, int(sign*days))
		if wall.Year() < 1 || wall.Year() > 9999 {
			return invalid()
		}
		var err error
		anchor, err = taskAlarmAnchor(icalProp{Value: wall.Format("20060102T150405"), Params: map[string]string{"TZID": anchor.Location().String()}})
		if err != nil {
			return invalid()
		}
	}
	reminder := anchor.Add(time.Duration(sign*seconds) * time.Second)
	if reminder.Year() < 1 || reminder.Year() > 9999 || reminder.UTC().Year() < 1 || reminder.UTC().Year() > 9999 {
		return invalid()
	}
	return reminder, nil
}
