package contacts

import (
	"encoding/json"
	"fmt"
	"strings"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// Filter evaluation shared by AddressBook and ContactCard /query,
// following the pattern of the mail and sharing packages: a filter
// is either a FilterOperator {"operator":"AND"|"OR"|"NOT",
// "conditions":[...]} (recursively) or a single condition object
// whose present properties are ANDed together (RFC 8620 §5.5).
// Unknown condition properties are an error, which datatype turns
// into invalidArguments.

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
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}
			return true, nil
		case "OR":
			for _, c := range op.Conditions {
				ok, err := evalFilter(obj, c, cond)
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		case "NOT":
			for _, c := range op.Conditions {
				ok, err := evalFilter(obj, c, cond)
				if err != nil {
					return false, err
				}
				if ok {
					return false, nil
				}
			}
			return true, nil
		default:
			return false, fmt.Errorf("contacts: unsupported filter operator %q", op.Operator)
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
			return fmt.Errorf("contacts: unknown filter condition %q", k)
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

// --- AddressBook filters and sorts ---

type addressBookCondition struct {
	Name      *string `json:"name"`
	IsDefault *bool   `json:"isDefault"`
}

func addressBookMatch(a AddressBook, filter json.RawMessage) (bool, error) {
	return evalFilter(a, filter, addressBookCond)
}

func addressBookCond(a AddressBook, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, map[string]bool{"name": true, "isDefault": true}); err != nil {
		return false, err
	}
	var c addressBookCondition
	if err := json.Unmarshal(raw, &c); err != nil {
		return false, err
	}
	// name matches as a case-insensitive substring.
	if c.Name != nil && !containsFold(a.Name, *c.Name) {
		return false, nil
	}
	if c.IsDefault != nil && a.IsDefault != *c.IsDefault {
		return false, nil
	}
	return true, nil
}

func addressBookCompare(a, b AddressBook, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		return strings.Compare(a.Name, b.Name), true
	case "sortOrder":
		return cmpUint(a.SortOrder, b.SortOrder), true
	}
	return 0, false
}

// --- ContactCard filters and sorts ---

// cardCondition mirrors the ContactCard FilterCondition of JMAP
// Contacts: inAddressBook, text, email, name and kind.
type cardCondition struct {
	InAddressBook *jmap.ID `json:"inAddressBook"`
	Text          *string  `json:"text"`
	Email         *string  `json:"email"`
	Name          *string  `json:"name"`
	Kind          *string  `json:"kind"`
}

func cardMatch(c Card, filter json.RawMessage) (bool, error) {
	return evalFilter(c, filter, cardCond)
}

func cardCond(c Card, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, map[string]bool{
		"inAddressBook": true, "text": true, "email": true, "name": true, "kind": true,
	}); err != nil {
		return false, err
	}
	var f cardCondition
	if err := json.Unmarshal(raw, &f); err != nil {
		return false, err
	}
	if f.InAddressBook != nil && !c.AddressBookIDs[*f.InAddressBook] {
		return false, nil
	}
	if f.Kind != nil && c.Kind != *f.Kind {
		return false, nil
	}
	// email matches an address in the emails map exactly
	// (case-insensitively); use text for substring search.
	if f.Email != nil {
		found := false
		for _, e := range c.Emails {
			if e != nil && strings.EqualFold(e.Address, *f.Email) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	if f.Name != nil && !nameSearch(c, *f.Name) {
		return false, nil
	}
	if f.Text != nil && !textSearch(c, *f.Text) {
		return false, nil
	}
	return true, nil
}

// nameSearch is the search space for the name condition: the full
// name and every name component value, case-insensitive substring.
func nameSearch(c Card, q string) bool {
	if c.Name == nil {
		return false
	}
	if containsFold(c.Name.Full, q) {
		return true
	}
	for _, comp := range c.Name.Components {
		if containsFold(comp.Value, q) {
			return true
		}
	}
	return false
}

// textSearch is the search space for the text condition: the name
// (as for the name condition) plus every email address.
func textSearch(c Card, q string) bool {
	if nameSearch(c, q) {
		return true
	}
	for _, e := range c.Emails {
		if e != nil && containsFold(e.Address, q) {
			return true
		}
	}
	return false
}

func cardCompare(a, b Card, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		return strings.Compare(fullName(a), fullName(b)), true
	case "created":
		return strings.Compare(a.Created, b.Created), true
	case "updated":
		return strings.Compare(a.Updated, b.Updated), true
	}
	return 0, false
}

func fullName(c Card) string {
	if c.Name == nil {
		return ""
	}
	return c.Name.Full
}
