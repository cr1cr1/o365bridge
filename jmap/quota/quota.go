// Package quota implements the Quota data type of "JSON Meta
// Application Protocol (JMAP) for Quotas", RFC 9425 (formerly
// draft-ietf-jmap-quotas). The Quota object shape below follows
// RFC 9425 §2; the Quota/get|set|query|changes methods are the
// standard RFC 8620 §5 methods registered via datatype.Register
// under the capability URI jmap.QuotasURI
// ("urn:ietf:params:jmap:quotas").
//
// Quota objects are normally server-computed and read-only: a host
// whose quotas are derived rather than stored should back Register
// with ComputedStore (see below) instead of a writable store, so
// Quota/set reports every create/update/destroy as forbidden.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// Capability is the JMAP Quotas capability URI (RFC 9425).
const Capability = jmap.QuotasURI

// Quota is a single quota object (RFC 9425 §2). AccountID is not a
// property of the Quota object in the specification (quotas are
// returned per account by the method responses); it is carried here
// with omitempty as a convenience for hosts that track it on the
// record, and is omitted from the wire when empty.
type Quota struct {
	ID           jmap.ID `json:"id"`
	AccountID    jmap.ID `json:"accountId,omitempty"`
	ResourceType string  `json:"resourceType"` // "octets" | "messages"
	Used         uint64  `json:"used"`
	// HardLimit is the maximum the quota allows; nil (JSON null)
	// means unlimited.
	HardLimit *uint64 `json:"hardLimit"`
	Scope     string  `json:"scope"` // "account" | "domain" | "global"
	Name      string  `json:"name"`
	// Types lists the data type URIs this quota counts, if any.
	Types []string `json:"types,omitempty"`
}

// Register registers Quota/get, Quota/set and Quota/query on e,
// plus Quota/changes and Quota/queryChanges when store also
// implements datatype.ChangeStore[Quota] (datatype.Memory and
// ComputedStore both do).
func Register(e *server.Engine, store datatype.Store[Quota]) {
	datatype.Register[Quota](e, datatype.Options[Quota]{
		Name:       "Quota",
		Capability: Capability,
		Store:      store,
		IDOf:       func(q Quota) jmap.ID { return q.ID },
		Match:      quotaMatch,
		Compare:    quotaCompare,
	})
}

// --- Filter (RFC 9425 §3 FilterCondition) and sort ---

type quotaCondition struct {
	ResourceType *string `json:"resourceType"`
	Scope        *string `json:"scope"`
	Name         *string `json:"name"`
}

func quotaMatch(q Quota, filter json.RawMessage) (bool, error) {
	return evalFilter(q, filter, quotaCond)
}

func quotaCond(q Quota, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, map[string]bool{
		"resourceType": true, "scope": true, "name": true,
	}); err != nil {
		return false, err
	}
	var c quotaCondition
	if err := json.Unmarshal(raw, &c); err != nil {
		return false, err
	}
	// resourceType, scope and name all match exactly.
	if c.ResourceType != nil && q.ResourceType != *c.ResourceType {
		return false, nil
	}
	if c.Scope != nil && q.Scope != *c.Scope {
		return false, nil
	}
	if c.Name != nil && q.Name != *c.Name {
		return false, nil
	}
	return true, nil
}

func quotaCompare(a, b Quota, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		return strings.Compare(a.Name, b.Name), true
	case "used":
		switch {
		case a.Used < b.Used:
			return -1, true
		case a.Used > b.Used:
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// --- ComputedStore: read-only Store over a live Source ---

// Source is the backend contract behind ComputedStore: it computes
// the current quotas and state for an account on demand.
type Source interface {
	Quotas(ctx context.Context, account jmap.ID) ([]Quota, error)
	State(ctx context.Context, account jmap.ID) (string, error)
}

// ComputedStore implements datatype.Store[Quota] (and
// datatype.ChangeStore[Quota]) over a Source. Quotas are computed,
// not stored, so Create, Update and Destroy all fail with a
// "forbidden" SetError, which Quota/set surfaces in
// notCreated/notUpdated/notDestroyed.
//
// shortcut: ComputedStore keeps no change history, so Changes can
// only answer "nothing changed" when sinceState equals the current
// state; for any other state it returns datatype.ErrCannotCalculate
// (a cannotCalculateChanges method error). Hosts needing real
// change lists must track them and supply their own ChangeStore.
type ComputedStore struct {
	Source Source
}

var (
	_ datatype.Store[Quota]       = (*ComputedStore)(nil)
	_ datatype.ChangeStore[Quota] = (*ComputedStore)(nil)
)

// NewComputedStore returns a ComputedStore backed by src.
func NewComputedStore(src Source) *ComputedStore { return &ComputedStore{Source: src} }

// State implements datatype.Store.
func (s *ComputedStore) State(ctx context.Context, account jmap.ID) (string, error) {
	return s.Source.State(ctx, account)
}

// Get implements datatype.Store. ids == nil returns all quotas for
// the account; otherwise quotas are returned in the requested order.
func (s *ComputedStore) Get(ctx context.Context, account jmap.ID, ids []jmap.ID) ([]Quota, []jmap.ID, error) {
	all, err := s.Source.Quotas(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	for i := range all {
		if all[i].AccountID == "" {
			all[i].AccountID = account
		}
	}
	if ids == nil {
		return all, nil, nil
	}
	byID := make(map[jmap.ID]Quota, len(all))
	for _, q := range all {
		byID[q.ID] = q
	}
	var found []Quota
	var notFound []jmap.ID
	for _, id := range ids {
		if q, ok := byID[id]; ok {
			found = append(found, q)
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

func forbidden() *jmap.SetError {
	return &jmap.SetError{Type: "forbidden", Description: "quota objects are server-computed and read-only"}
}

// Create implements datatype.Store; it always fails as forbidden.
func (s *ComputedStore) Create(_ context.Context, _ jmap.ID, _ Quota) (Quota, *jmap.SetError) {
	return Quota{}, forbidden()
}

// Update implements datatype.Store; it always fails as forbidden.
func (s *ComputedStore) Update(_ context.Context, _ jmap.ID, _ Quota) (*Quota, *jmap.SetError) {
	return nil, forbidden()
}

// Destroy implements datatype.Store; it always fails as forbidden.
func (s *ComputedStore) Destroy(_ context.Context, _ jmap.ID, _ jmap.ID) *jmap.SetError {
	return forbidden()
}

// Changes implements datatype.ChangeStore without history: it
// reports no changes when sinceState is the current state, and
// datatype.ErrCannotCalculate otherwise.
func (s *ComputedStore) Changes(ctx context.Context, account jmap.ID, sinceState string, _ int) (created, updated, destroyed []jmap.ID, newState string, hasMore bool, err error) {
	current, err := s.Source.State(ctx, account)
	if err != nil {
		return nil, nil, nil, "", false, err
	}
	if sinceState != current {
		return nil, nil, nil, "", false,
			fmt.Errorf("%w: quota changes since state %q cannot be calculated", datatype.ErrCannotCalculate, sinceState)
	}
	return nil, nil, nil, current, false, nil
}

// --- shared filter helpers (RFC 8620 §5.5 FilterOperator) ---

type conditionFunc func(obj Quota, cond json.RawMessage) (bool, error)

func evalFilter(q Quota, filter json.RawMessage, cond conditionFunc) (bool, error) {
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
				ok, err := evalFilter(q, c, cond)
				if err != nil || !ok {
					return ok, err
				}
			}
			return true, nil
		case "OR":
			for _, c := range op.Conditions {
				ok, err := evalFilter(q, c, cond)
				if err != nil || ok {
					return ok, err
				}
			}
			return false, nil
		case "NOT":
			for _, c := range op.Conditions {
				ok, err := evalFilter(q, c, cond)
				if err != nil || ok {
					return false, err
				}
			}
			return true, nil
		default:
			return false, fmt.Errorf("quota: unsupported filter operator %q", op.Operator)
		}
	}
	return cond(q, filter)
}

func checkKeys(raw json.RawMessage, allowed map[string]bool) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	for k := range m {
		if !allowed[k] {
			return fmt.Errorf("quota: unknown filter condition %q", k)
		}
	}
	return nil
}
