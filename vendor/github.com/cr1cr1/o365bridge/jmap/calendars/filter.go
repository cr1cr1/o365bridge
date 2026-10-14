package calendars

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// Filter evaluation shared by Calendar, CalendarEvent and
// ParticipantIdentity /query: a filter is either a FilterOperator
// {"operator":"AND"|"OR"|"NOT","conditions":[...]} (recursively) or
// a single condition object whose present properties are ANDed
// together (RFC 8620 §5.5). Unknown condition properties are an
// error, which datatype turns into invalidArguments.

type conditionFunc[T any] func(obj T, cond json.RawMessage) (bool, error)

func evalFilter[T any](obj T, filter json.RawMessage, cond conditionFunc[T]) (bool, error) {
	trimmed := strings.TrimSpace(string(filter))
	if trimmed == "" || trimmed == "null" {
		return true, nil
	}
	var probe struct {
		Operator *string `json:"operator"`
	}
	if err := json.Unmarshal(filter, &probe); err != nil {
		return false, err
	}
	if probe.Operator != nil {
		var op jmap.FilterOperator
		if err := json.Unmarshal(filter, &op); err != nil {
			return false, err
		}
		if err := checkKeys(filter, map[string]bool{"operator": true, "conditions": true}); err != nil {
			return false, err
		}
		switch op.Operator {
		case "AND":
			for _, c := range op.Conditions {
				ok, err := evalFilter(obj, c, cond)
				if err != nil || !ok {
					return ok, err
				}
			}
			return true, nil
		case "OR":
			for _, c := range op.Conditions {
				ok, err := evalFilter(obj, c, cond)
				if err != nil || ok {
					return ok, err
				}
			}
			return false, nil
		case "NOT":
			for _, c := range op.Conditions {
				ok, err := evalFilter(obj, c, cond)
				if err != nil || ok {
					return false, err
				}
			}
			return true, nil
		default:
			return false, fmt.Errorf("calendars: unsupported filter operator %q", op.Operator)
		}
	}
	return cond(obj, filter)
}

func checkKeys(raw json.RawMessage, allowed map[string]bool) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	for k := range m {
		if !allowed[k] {
			return fmt.Errorf("calendars: unknown filter condition %q", k)
		}
	}
	return nil
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func cmpUint(a, b uint) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// --- Calendar ---

type calendarCondition struct {
	Name         *string `json:"name"`
	IsDefault    *bool   `json:"isDefault"`
	IsSubscribed *bool   `json:"isSubscribed"`
}

func calendarMatch(c Calendar, filter json.RawMessage) (bool, error) {
	return evalFilter(c, filter, calendarCond)
}

func calendarCond(c Calendar, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, map[string]bool{"name": true, "isDefault": true, "isSubscribed": true}); err != nil {
		return false, err
	}
	var cond calendarCondition
	if err := json.Unmarshal(raw, &cond); err != nil {
		return false, err
	}
	if cond.Name != nil && !containsFold(c.Name, *cond.Name) {
		return false, nil
	}
	if cond.IsDefault != nil && c.IsDefault != *cond.IsDefault {
		return false, nil
	}
	if cond.IsSubscribed != nil && c.IsSubscribed != *cond.IsSubscribed {
		return false, nil
	}
	return true, nil
}

func calendarCompare(a, b Calendar, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		return strings.Compare(a.Name, b.Name), true
	case "sortOrder":
		return cmpUint(a.SortOrder, b.SortOrder), true
	}
	return 0, false
}

// --- CalendarEvent ---

type eventCondition struct {
	InCalendar    *jmap.ID `json:"inCalendar"`
	After         *string  `json:"after"`
	Before        *string  `json:"before"`
	Text          *string  `json:"text"`
	HasRecurrence *bool    `json:"hasRecurrence"`
}

func eventMatch(e CalendarEvent, filter json.RawMessage) (bool, error) {
	return evalFilter(e, filter, eventCond)
}

// compareLocalDateTime compares LocalDateTime strings; it parses
// the canonical layout when both sides parse and falls back to
// lexical comparison (which is correct for same-format values).
func compareLocalDateTime(a, b string) int {
	const layout = "2006-01-02T15:04:05"
	ta, ea := time.Parse(layout, a)
	tb, eb := time.Parse(layout, b)
	if ea == nil && eb == nil {
		return ta.Compare(tb)
	}
	return strings.Compare(a, b)
}

func eventCond(e CalendarEvent, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, map[string]bool{
		"inCalendar": true, "after": true, "before": true, "text": true, "hasRecurrence": true,
	}); err != nil {
		return false, err
	}
	var cond eventCondition
	if err := json.Unmarshal(raw, &cond); err != nil {
		return false, err
	}
	if cond.InCalendar != nil && !e.CalendarIDs[*cond.InCalendar] {
		return false, nil
	}
	if cond.After != nil && compareLocalDateTime(e.Start, *cond.After) < 0 {
		return false, nil
	}
	if cond.Before != nil && compareLocalDateTime(e.Start, *cond.Before) >= 0 {
		return false, nil
	}
	if cond.Text != nil && !containsFold(e.Title, *cond.Text) && !containsFold(e.Description, *cond.Text) {
		return false, nil
	}
	if cond.HasRecurrence != nil && (e.RecurrenceRule != nil) != *cond.HasRecurrence {
		return false, nil
	}
	return true, nil
}

func eventCompare(a, b CalendarEvent, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "start":
		return compareLocalDateTime(a.Start, b.Start), true
	case "title":
		return strings.Compare(a.Title, b.Title), true
	case "uid":
		return strings.Compare(a.UID, b.UID), true
	}
	return 0, false
}

// --- ParticipantIdentity ---

type identityCondition struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

func identityMatch(p ParticipantIdentity, filter json.RawMessage) (bool, error) {
	return evalFilter(p, filter, identityCond)
}

func identityCond(p ParticipantIdentity, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, map[string]bool{"name": true, "email": true}); err != nil {
		return false, err
	}
	var cond identityCondition
	if err := json.Unmarshal(raw, &cond); err != nil {
		return false, err
	}
	if cond.Email != nil && !strings.EqualFold(p.Email, *cond.Email) {
		return false, nil
	}
	if cond.Name != nil && (p.Name == nil || !containsFold(*p.Name, *cond.Name)) {
		return false, nil
	}
	return true, nil
}

func identityCompare(a, b ParticipantIdentity, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		var an, bn string
		if a.Name != nil {
			an = *a.Name
		}
		if b.Name != nil {
			bn = *b.Name
		}
		return strings.Compare(an, bn), true
	case "email":
		return strings.Compare(a.Email, b.Email), true
	}
	return 0, false
}
