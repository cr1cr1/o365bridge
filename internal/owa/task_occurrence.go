package owa

import (
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// taskDeadOccurrence reads PidLidTaskDeadOccurrence, not the copied Recurrence definition.
func taskDeadOccurrence(item map[string]any) (dead, known bool) {
	for _, property := range arr(item, "ExtendedProperty") {
		field := dig(property, "ExtendedFieldURI")
		distinguished, guid := str(field, "DistinguishedPropertySetId"), str(field, "PropertySetId")
		taskGUID := strings.EqualFold(guid, "00062003-0000-0000-C000-000000000046")
		taskSet := (distinguished == "Task" || taskGUID) && (distinguished == "" || distinguished == "Task") && (guid == "" || taskGUID)
		id := dig(field, "PropertyId")
		if !taskSet || (id != 33033 && id != float64(33033)) || str(field, "PropertyType") != "Boolean" {
			continue
		}
		switch value := dig(property, "Value").(type) {
		case bool:
			return value, true
		case string:
			switch value {
			case "true", "1":
				return true, true
			case "false", "0":
				return false, true
			}
		}
		return false, false
	}
	return false, false
}

type taskOccurrenceState struct {
	dead, known, recurring, storedUID bool
}

// Resolve only proven native splits after every discovery page has been hydrated.
func projectTaskOccurrenceUIDs(tasks []bridge.Task, states []taskOccurrenceState) error {
	groups := make(map[string][]int, len(tasks))
	for i, task := range tasks {
		groups[task.UID] = append(groups[task.UID], i)
	}
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		generators := 0
		for _, i := range members {
			state := states[i]
			if !state.storedUID || !state.known || (!state.dead && !state.recurring) {
				return bridge.Wrap(bridge.KindOther, "owa: ListTasks: ambiguous repeated task UID", nil)
			}
			if !state.dead {
				generators++
			}
		}
		if generators != 1 {
			return bridge.Wrap(bridge.KindOther, "owa: ListTasks: ambiguous repeated task UID", nil)
		}
		for _, i := range members {
			if !states[i].dead {
				continue
			}
			// Reserve all original UIDs too; never repair a collision with another fallback.
			if _, collision := groups[tasks[i].ID]; collision {
				return bridge.Wrap(bridge.KindOther, "owa: ListTasks: projected task UID collision", nil)
			}
			tasks[i].UID = tasks[i].ID
		}
	}
	return nil
}
