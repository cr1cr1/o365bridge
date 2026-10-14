// Package datatype provides generic RFC 8620 §5 method handlers
// (Name/get, Name/set, Name/changes, Name/query, Name/queryChanges)
// for any record type T, driven by a small backend Store contract.
// A data type registers once with Register; the handlers translate
// between the wire shapes in the root jmap package and Store calls.
//
// Filter and sort vocabularies are data-type specific (§5.5), so they
// are delegated to the Match/Compare hooks in Options; the handlers
// own only the protocol mechanics (state checks, patching, paging,
// anchor handling, and the standard method errors).
package datatype

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// Store is the generic backend contract for one data type.
//
// Get with ids == nil must return all records of the type for the
// account. Create/Update/Destroy report per-record failures as
// *jmap.SetError (nil on success) so /set can build its
// notCreated/notUpdated/notDestroyed maps verbatim.
type Store[T any] interface {
	State(ctx context.Context, account jmap.ID) (string, error)
	Get(ctx context.Context, account jmap.ID, ids []jmap.ID) (found []T, notFound []jmap.ID, err error)
	Create(ctx context.Context, account jmap.ID, obj T) (created T, serr *jmap.SetError)
	Update(ctx context.Context, account jmap.ID, obj T) (updated *T, serr *jmap.SetError)
	Destroy(ctx context.Context, account jmap.ID, id jmap.ID) *jmap.SetError
}

// ChangeStore is the optional contract behind Name/changes. Stores
// that implement it get Name/changes (and Name/queryChanges)
// registered; stores that do not are never asked for changes.
// maxChanges == 0 means the store chooses the limit.
type ChangeStore[T any] interface {
	Changes(ctx context.Context, account jmap.ID, sinceState string, maxChanges int) (created, updated, destroyed []jmap.ID, newState string, hasMore bool, err error)
}

// QueryChangesStore is the optional contract behind
// Name/queryChanges (RFC 8620 §5.6). If a Store implements
// ChangeStore but not QueryChangesStore, Name/queryChanges is
// registered (the type can calculate changes in principle) but
// always fails with a cannotCalculateChanges method error: the
// generic layer cannot derive filtered/sorted query diffs from
// plain id lists honestly.
type QueryChangesStore[T any] interface {
	QueryChanges(ctx context.Context, account jmap.ID, args jmap.QueryChangesArgs) (*jmap.QueryChangesResponse, error)
}

// Advertised core limits enforced by the generic handlers. These
// mirror the engine's advertised default core capability values
// (server.New defaults MaxObjectsInGet/MaxObjectsInSet to 500);
// RFC 8620 §5.1/§5.2 permits the server to return requestTooLarge
// when a client exceeds the advertised maximum.
const (
	MaxObjectsInGet = 500
	MaxObjectsInSet = 500
)

// ErrCannotCalculate is returned by ChangeStore.Changes (and may be
// returned, wrapped, by QueryChangesStore.QueryChanges) when the
// backend cannot list changes since the given state. Register turns
// it into a cannotCalculateChanges method error (§5.2, §5.6).
var ErrCannotCalculate = errors.New("datatype: cannot calculate changes")

// Options configures Register for one data type.
type Options[T any] struct {
	// Name is the data-type name; methods are registered as
	// Name+"/get", Name+"/set", and so on (e.g. "Widget/get").
	Name string
	// Capability is the capability URI clients must list in "using"
	// for these methods. If non-empty (and not the core URI) it is
	// advertised in the session with an empty capability object.
	// shortcut: the advertised capability object is always {}; a
	// data type needing capability fields must register its own
	// capability object with the engine in addition.
	Capability jmap.URI
	// Store is the backend. Required.
	Store Store[T]
	// IDOf returns the server id of a record. Required.
	IDOf func(T) jmap.ID
	// Match reports whether obj matches a /query filter (the raw
	// FilterCondition/FilterOperator JSON, passed through verbatim).
	// A non-nil error becomes an invalidArguments method error.
	// If Match is nil, an empty/null filter matches everything and
	// any other filter fails with unsupportedFilter.
	Match func(obj T, filter json.RawMessage) (bool, error)
	// Compare orders two records by comparator.Property, returning
	// a negative/zero/positive value in ascending order; Register
	// applies Comparator.IsAscending (default true). ok == false
	// means the property (or collation) is not supported and the
	// /query call fails with unsupportedSort.
	Compare func(a, b T, c jmap.Comparator) (cmp int, ok bool)
}

type handlers[T any] struct {
	opts Options[T]
}

// Register registers Name/get, Name/set, and Name/query on e, plus
// Name/changes and Name/queryChanges if opts.Store also implements
// ChangeStore[T]. It panics on a missing Name, Store, or IDOf: those
// are programmer errors, not runtime conditions.
func Register[T any](e *server.Engine, opts Options[T]) {
	if opts.Name == "" {
		panic("datatype: Options.Name is empty")
	}
	if opts.Store == nil {
		panic("datatype: Options.Store is nil")
	}
	if opts.IDOf == nil {
		panic("datatype: Options.IDOf is nil")
	}
	h := &handlers[T]{opts: opts}
	if opts.Capability != "" && opts.Capability != jmap.CoreURI {
		e.RegisterCapability(opts.Capability, map[string]any{}, map[string]any{})
	}
	e.RegisterMethod(opts.Name+"/get", opts.Capability, h.get)
	e.RegisterMethod(opts.Name+"/set", opts.Capability, h.set)
	e.RegisterMethod(opts.Name+"/query", opts.Capability, h.query)
	if _, ok := opts.Store.(ChangeStore[T]); ok {
		e.RegisterMethod(opts.Name+"/changes", opts.Capability, h.changes)
		e.RegisterMethod(opts.Name+"/queryChanges", opts.Capability, h.queryChanges)
	}
}

func accountAndDecode[T any](call *server.Call, v any) (jmap.ID, error) {
	account, err := call.AccountID()
	if err != nil {
		return "", err
	}
	if err := call.Decode(v); err != nil {
		return "", jmap.NewInvalidArguments(err.Error())
	}
	return account, nil
}

// --- /get (§5.1) ---

type getResponse struct {
	AccountID jmap.ID           `json:"accountId"`
	State     string            `json:"state"`
	List      []json.RawMessage `json:"list"`
	NotFound  []jmap.ID         `json:"notFound"`
}

func (h *handlers[T]) get(ctx context.Context, call *server.Call) (any, error) {
	var args jmap.GetArgs
	account, err := accountAndDecode[T](call, &args)
	if err != nil {
		return nil, err
	}
	if len(args.IDs) > MaxObjectsInGet {
		return nil, jmap.NewRequestTooLarge(fmt.Sprintf("too many ids in /get: %d > %d", len(args.IDs), MaxObjectsInGet))
	}
	found, notFound, err := h.opts.Store.Get(ctx, account, args.IDs)
	if err != nil {
		return nil, err
	}
	state, err := h.opts.Store.State(ctx, account)
	if err != nil {
		return nil, err
	}
	resp := getResponse{
		AccountID: account,
		State:     state,
		List:      make([]json.RawMessage, 0, len(found)),
		NotFound:  notFound,
	}
	if resp.NotFound == nil {
		resp.NotFound = []jmap.ID{}
	}
	for _, obj := range found {
		raw, err := project(obj, args.Properties)
		if err != nil {
			return nil, err
		}
		resp.List = append(resp.List, raw)
	}
	return resp, nil
}

// project marshals obj, optionally keeping only the requested
// properties (plus "id", which §5.1 always returns).
//
// shortcut: unknown requested properties are silently dropped. The
// generic layer has no schema for T, so it cannot distinguish a
// misspelled property from one the record simply omits; a data type
// needing invalidProperties errors must validate properties itself.
func project[T any](obj T, properties []string) (json.RawMessage, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	if properties == nil {
		return raw, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("datatype: projecting non-object record: %w", err)
	}
	keep := make(map[string]json.RawMessage, len(properties)+1)
	if id, ok := m["id"]; ok {
		keep["id"] = id
	}
	for _, p := range properties {
		if v, ok := m[p]; ok {
			keep[p] = v
		}
	}
	return json.Marshal(keep)
}

// --- /set (§5.3) ---

// setArgs mirrors jmap.SetArgs but keeps creations raw so each one
// decodes into T individually (a bad creation fails per-record, not
// for the whole call).
type setArgs struct {
	AccountID jmap.ID                      `json:"accountId"`
	IfInState *string                      `json:"ifInState"`
	Create    map[string]json.RawMessage   `json:"create"`
	Update    map[jmap.ID]jmap.PatchObject `json:"update"`
	Destroy   []jmap.ID                    `json:"destroy"`
}

func (h *handlers[T]) set(ctx context.Context, call *server.Call) (any, error) {
	var args setArgs
	account, err := accountAndDecode[T](call, &args)
	if err != nil {
		return nil, err
	}
	if n := len(args.Create) + len(args.Update) + len(args.Destroy); n > MaxObjectsInSet {
		return nil, jmap.NewRequestTooLarge(fmt.Sprintf("too many objects in /set: %d > %d", n, MaxObjectsInSet))
	}
	store := h.opts.Store
	oldState, err := store.State(ctx, account)
	if err != nil {
		return nil, err
	}
	if args.IfInState != nil && *args.IfInState != oldState {
		return nil, jmap.NewStateMismatch(
			fmt.Sprintf("ifInState %q does not match current state %q", *args.IfInState, oldState))
	}

	resp := jmap.SetResponse[T]{AccountID: account, OldState: &oldState, NewState: oldState}
	changed := false

	// Creates, in sorted creation-id order so sequential server ids
	// are assigned deterministically.
	creationIDs := make([]string, 0, len(args.Create))
	for cid := range args.Create {
		creationIDs = append(creationIDs, cid)
	}
	sort.Strings(creationIDs)
	for _, cid := range creationIDs {
		var obj T
		if err := json.Unmarshal(args.Create[cid], &obj); err != nil {
			resp.NotCreated = setErr(resp.NotCreated, jmap.ID(cid),
				&jmap.SetError{Type: "invalidProperties", Description: err.Error()})
			continue
		}
		created, serr := store.Create(ctx, account, obj)
		if serr != nil {
			resp.NotCreated = setErr(resp.NotCreated, jmap.ID(cid), serr)
			continue
		}
		if resp.Created == nil {
			resp.Created = make(map[jmap.ID]*T)
		}
		c := created
		resp.Created[jmap.ID(cid)] = &c
		call.SetCreated(jmap.ID(cid), h.opts.IDOf(created))
		changed = true
	}

	// Updates, in sorted id order (deterministic journal/state).
	updateIDs := make([]jmap.ID, 0, len(args.Update))
	for id := range args.Update {
		updateIDs = append(updateIDs, id)
	}
	sort.Slice(updateIDs, func(i, j int) bool { return updateIDs[i] < updateIDs[j] })
	for _, id := range updateIDs {
		found, _, err := store.Get(ctx, account, []jmap.ID{id})
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			resp.NotUpdated = setErr(resp.NotUpdated, id, &jmap.SetError{Type: "notFound"})
			continue
		}
		patched, err := ApplyPatch(found[0], args.Update[id])
		if err != nil {
			resp.NotUpdated = setErr(resp.NotUpdated, id,
				&jmap.SetError{Type: "invalidPatch", Description: err.Error()})
			continue
		}
		updated, serr := store.Update(ctx, account, patched)
		if serr != nil {
			resp.NotUpdated = setErr(resp.NotUpdated, id, serr)
			continue
		}
		if resp.Updated == nil {
			resp.Updated = make(map[jmap.ID]*T)
		}
		// updated is nil when the store reports no server-set
		// properties beyond the patch: that marshals to JSON null,
		// as §5.3 requires.
		resp.Updated[id] = updated
		changed = true
	}

	for _, id := range args.Destroy {
		if serr := store.Destroy(ctx, account, id); serr != nil {
			resp.NotDestroyed = setErr(resp.NotDestroyed, id, serr)
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
		changed = true
	}

	if changed {
		newState, err := store.State(ctx, account)
		if err != nil {
			return nil, err
		}
		resp.NewState = newState
	}
	return resp, nil
}

func setErr(m map[jmap.ID]*jmap.SetError, id jmap.ID, serr *jmap.SetError) map[jmap.ID]*jmap.SetError {
	if m == nil {
		m = make(map[jmap.ID]*jmap.SetError)
	}
	m[id] = serr
	return m
}

// --- /query (§5.5) ---

type queryResponse struct {
	AccountID           jmap.ID   `json:"accountId"`
	QueryState          string    `json:"queryState"`
	CanCalculateChanges bool      `json:"canCalculateChanges"`
	Position            uint64    `json:"position"`
	Total               *uint64   `json:"total"` // null unless calculateTotal
	IDs                 []jmap.ID `json:"ids"`
}

func (h *handlers[T]) query(ctx context.Context, call *server.Call) (any, error) {
	var args jmap.QueryArgs
	account, err := accountAndDecode[T](call, &args)
	if err != nil {
		return nil, err
	}
	store := h.opts.Store
	all, _, err := store.Get(ctx, account, nil)
	if err != nil {
		return nil, err
	}
	state, err := store.State(ctx, account)
	if err != nil {
		return nil, err
	}

	filtered := make([]T, 0, len(all))
	filterEmpty := len(args.Filter) == 0 || string(args.Filter) == "null"
	for _, obj := range all {
		match := true
		if !filterEmpty {
			if h.opts.Match == nil {
				return nil, jmap.NewUnsupportedFilter("no Match hook registered for this type")
			}
			m, err := h.opts.Match(obj, args.Filter)
			if err != nil {
				return nil, jmap.NewInvalidArguments(err.Error())
			}
			match = m
		}
		if match {
			filtered = append(filtered, obj)
		}
	}

	if len(args.Sort) > 0 {
		if h.opts.Compare == nil {
			return nil, jmap.NewUnsupportedSort("no Compare hook registered for this type")
		}
		// Probe support up front: SliceStable may never call the
		// comparator for 0/1-element results, which would silently
		// accept an unknown sort property.
		if len(filtered) > 0 {
			for _, c := range args.Sort {
				if _, ok := h.opts.Compare(filtered[0], filtered[0], c); !ok {
					return nil, jmap.NewUnsupportedSort("unsupported sort property: " + c.Property)
				}
			}
		}
		unsupported := ""
		sort.SliceStable(filtered, func(i, j int) bool {
			for _, c := range args.Sort {
				cmp, ok := h.opts.Compare(filtered[i], filtered[j], c)
				if !ok {
					unsupported = c.Property
					return false
				}
				if cmp != 0 {
					if c.IsAscending != nil && !*c.IsAscending {
						cmp = -cmp
					}
					return cmp < 0
				}
			}
			return false
		})
		if unsupported != "" {
			return nil, jmap.NewUnsupportedSort("unsupported sort property: " + unsupported)
		}
	}

	total := int64(len(filtered))
	var start int64
	if args.Anchor != nil {
		idx := int64(-1)
		for i, obj := range filtered {
			if h.opts.IDOf(obj) == *args.Anchor {
				idx = int64(i)
				break
			}
		}
		if idx < 0 {
			return nil, jmap.NewAnchorNotFound("anchor not in results: " + string(*args.Anchor))
		}
		start = idx + args.AnchorOffset
	} else {
		start = args.Position
		if start < 0 {
			start = total + start // negative position counts from the end (§5.5)
		}
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}

	// args.Limit is *uint64, so unset (null) is distinguishable from
	// 0: null means no limit, 0 means return no ids (§5.5).
	end := total
	if args.Limit != nil && start+int64(*args.Limit) < end {
		end = start + int64(*args.Limit)
	}
	ids := make([]jmap.ID, 0, end-start)
	for _, obj := range filtered[start:end] {
		ids = append(ids, h.opts.IDOf(obj))
	}

	_, canChanges := store.(ChangeStore[T])
	resp := queryResponse{
		AccountID:           account,
		QueryState:          state,
		CanCalculateChanges: canChanges,
		Position:            uint64(start),
		IDs:                 ids,
	}
	if args.CalculateTotal {
		t := uint64(total)
		resp.Total = &t
	}
	return resp, nil
}

// --- /changes (§5.2) ---

func (h *handlers[T]) changes(ctx context.Context, call *server.Call) (any, error) {
	var args jmap.ChangesArgs
	account, err := accountAndDecode[T](call, &args)
	if err != nil {
		return nil, err
	}
	cs, ok := h.opts.Store.(ChangeStore[T])
	if !ok { // unreachable: Register only registers /changes then
		return nil, jmap.NewCannotCalculateChanges("store does not implement ChangeStore")
	}
	maxChanges := 0
	if args.MaxChanges != nil {
		maxChanges = int(*args.MaxChanges)
	}
	created, updated, destroyed, newState, hasMore, err := cs.Changes(ctx, account, args.SinceState, maxChanges)
	if err != nil {
		if errors.Is(err, ErrCannotCalculate) {
			return nil, jmap.NewCannotCalculateChanges(err.Error())
		}
		return nil, err
	}
	nonNil := func(ids []jmap.ID) []jmap.ID {
		if ids == nil {
			return []jmap.ID{}
		}
		return ids
	}
	return jmap.ChangesResponse{
		AccountID:      account,
		OldState:       args.SinceState,
		NewState:       newState,
		HasMoreChanges: hasMore,
		Created:        nonNil(created),
		Updated:        nonNil(updated),
		Destroyed:      nonNil(destroyed),
	}, nil
}

// --- /queryChanges (§5.6) ---

func (h *handlers[T]) queryChanges(ctx context.Context, call *server.Call) (any, error) {
	var args jmap.QueryChangesArgs
	account, err := accountAndDecode[T](call, &args)
	if err != nil {
		return nil, err
	}
	qcs, ok := h.opts.Store.(QueryChangesStore[T])
	if !ok {
		// Registered because the store is a ChangeStore, but the
		// generic layer cannot diff filtered/sorted result sets
		// from plain change lists. Honest spec error, no invented
		// data (see QueryChangesStore).
		return nil, jmap.NewCannotCalculateChanges("store does not implement QueryChangesStore")
	}
	resp, err := qcs.QueryChanges(ctx, account, args)
	if err != nil {
		if errors.Is(err, ErrCannotCalculate) {
			return nil, jmap.NewCannotCalculateChanges(err.Error())
		}
		return nil, err
	}
	if resp == nil {
		return nil, jmap.NewServerFail("QueryChangesStore returned nil response")
	}
	if resp.Removed == nil {
		resp.Removed = []jmap.ID{}
	}
	if resp.Added == nil {
		resp.Added = []jmap.AddedItem{}
	}
	resp.AccountID = account
	return resp, nil
}
