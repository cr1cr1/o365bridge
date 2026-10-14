// Package sharing implements the Principal data type of "JSON Meta
// Application Protocol (JMAP) Sharing", RFC 9670 (formerly
// draft-ietf-jmap-sharing, which RFC 9670 obsoletes; RFC 9670 also
// updates RFC 8620). The Principal object shape below follows
// RFC 9670 §2; the Principal/get|set|query|changes methods are the
// standard RFC 8620 §5 methods registered via datatype.Register
// under the capability URI jmap.SharingURI
// ("urn:ietf:params:jmap:sharing").
//
// Principals are the sharing surface this library provides: they
// identify the individuals, groups, resources and locations that
// objects may be shared with. Object-level sharing rights are NOT
// defined here: each shareable data type (calendars, contacts, ...)
// defines its own rights vocabulary on its own objects, typically
// as a shareWith-style property using the SharedWith shape below.
// Data-type packages in later phases use SharedWith for that.
package sharing

import (
	"encoding/json"
	"fmt"
	"strings"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// Capability is the JMAP Sharing capability URI (RFC 9670).
const Capability = jmap.SharingURI

// Principal is a principal that objects may be shared with
// (RFC 9670 §2). Name, Description, Email and TimeZone are nullable
// in the specification, hence pointers without omitempty: a nil
// pointer marshals to JSON null.
//
// Capabilities is the object of capabilities (keyed by capability
// URI) that this principal may access. It must not be null on the
// wire; MarshalJSON below substitutes an empty object for a nil
// map, so hosts may simply leave the field unset.
type Principal struct {
	ID           jmap.ID                      `json:"id"`
	Type         string                       `json:"type"` // "individual" | "group" | "resource" | "location" | "virtual"
	Name         *string                      `json:"name"`
	Description  *string                      `json:"description"`
	Email        *string                      `json:"email"`
	TimeZone     *string                      `json:"timeZone"`
	Capabilities map[jmap.URI]json.RawMessage `json:"capabilities"`
}

// MarshalJSON encodes the Principal, rendering a nil Capabilities
// map as {} rather than null.
func (p Principal) MarshalJSON() ([]byte, error) {
	type principal Principal // alias: avoid recursing into MarshalJSON
	a := principal(p)
	if a.Capabilities == nil {
		a.Capabilities = map[jmap.URI]json.RawMessage{}
	}
	return json.Marshal(a)
}

// SharedWith is the JSON shape used for shareWith-style properties
// on shareable objects: it maps a principal id to that principal's
// rights on the object, where each right name maps to true (a right
// is either present and true, or absent). The right names are
// defined by the data type that owns the object (e.g. calendars
// define mayRead, mayWrite, ...), not by RFC 9670 or this package.
//
// Example wire form: {"principal-1":{"mayRead":true,"mayWrite":true}}
type SharedWith map[jmap.ID]map[string]bool

// Register registers Principal/get, Principal/set and
// Principal/query on e, plus Principal/changes and
// Principal/queryChanges when store also implements
// datatype.ChangeStore[Principal] (datatype.Memory does).
func Register(e *server.Engine, store datatype.Store[Principal]) {
	datatype.Register[Principal](e, datatype.Options[Principal]{
		Name:       "Principal",
		Capability: Capability,
		Store:      store,
		IDOf:       func(p Principal) jmap.ID { return p.ID },
		Match:      principalMatch,
		Compare:    principalCompare,
	})
}

// --- Filter and sort ---

type principalCondition struct {
	Type  *string `json:"type"`
	Email *string `json:"email"`
	Name  *string `json:"name"`
}

func principalMatch(p Principal, filter json.RawMessage) (bool, error) {
	return evalFilter(p, filter, principalCond)
}

func principalCond(p Principal, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, map[string]bool{
		"type": true, "email": true, "name": true,
	}); err != nil {
		return false, err
	}
	var c principalCondition
	if err := json.Unmarshal(raw, &c); err != nil {
		return false, err
	}
	if c.Type != nil && p.Type != *c.Type {
		return false, nil
	}
	if c.Email != nil && (p.Email == nil || !strings.EqualFold(*p.Email, *c.Email)) {
		return false, nil
	}
	// name matches as a case-insensitive substring.
	if c.Name != nil && (p.Name == nil ||
		!strings.Contains(strings.ToLower(*p.Name), strings.ToLower(*c.Name))) {
		return false, nil
	}
	return true, nil
}

func principalCompare(a, b Principal, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		return strings.Compare(strVal(a.Name), strVal(b.Name)), true
	case "email":
		return strings.Compare(strVal(a.Email), strVal(b.Email)), true
	}
	return 0, false
}

func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- shared filter helpers (RFC 8620 §5.5 FilterOperator) ---

type conditionFunc func(obj Principal, cond json.RawMessage) (bool, error)

func evalFilter(p Principal, filter json.RawMessage, cond conditionFunc) (bool, error) {
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
				ok, err := evalFilter(p, c, cond)
				if err != nil || !ok {
					return ok, err
				}
			}
			return true, nil
		case "OR":
			for _, c := range op.Conditions {
				ok, err := evalFilter(p, c, cond)
				if err != nil || ok {
					return ok, err
				}
			}
			return false, nil
		case "NOT":
			for _, c := range op.Conditions {
				ok, err := evalFilter(p, c, cond)
				if err != nil || ok {
					return false, err
				}
			}
			return true, nil
		default:
			return false, fmt.Errorf("sharing: unsupported filter operator %q", op.Operator)
		}
	}
	return cond(p, filter)
}

func checkKeys(raw json.RawMessage, allowed map[string]bool) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	for k := range m {
		if !allowed[k] {
			return fmt.Errorf("sharing: unknown filter condition %q", k)
		}
	}
	return nil
}
