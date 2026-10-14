package datatype

import (
	"context"
	"strconv"
	"sync"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// Memory is an in-memory Store[T] + ChangeStore[T].
//
// It is an example/test backend (see the design spec: "an in-memory
// backend for tests is test infrastructure, not a product"), not a
// storage implementation: state is per-process, per-account objects
// are kept in insertion order, and ids are sequential decimal
// strings ("1", "2", ...) assigned on Create when the record's id
// is empty. The state string is the account's mutation counter as a
// decimal string; every successful create/update/destroy appends to
// a per-account journal that Changes replays. The journal is never
// truncated, so any state this process issued remains answerable —
// production backends with bounded journals should return
// ErrCannotCalculate for states older than their retention.
type Memory[T any] struct {
	mu       sync.Mutex
	idOf     func(T) jmap.ID
	setID    func(*T, jmap.ID)
	accounts map[jmap.ID]*memoryAccount[T]
}

type memoryAccount[T any] struct {
	objects map[jmap.ID]T
	order   []jmap.ID
	counter uint64
	nextID  uint64
	journal []journalEntry
}

type journalEntry struct {
	counter uint64
	kind    string // "created", "updated", or "destroyed"
	id      jmap.ID
}

// NewMemory builds a Memory store. idOf reads a record's id; setID
// writes a server-assigned id into a record on Create when its id
// is empty (setID may be nil if records always carry their own id).
func NewMemory[T any](idOf func(T) jmap.ID, setID func(*T, jmap.ID)) *Memory[T] {
	return &Memory[T]{
		idOf:     idOf,
		setID:    setID,
		accounts: make(map[jmap.ID]*memoryAccount[T]),
	}
}

func (m *Memory[T]) account(id jmap.ID) *memoryAccount[T] {
	a, ok := m.accounts[id]
	if !ok {
		a = &memoryAccount[T]{objects: make(map[jmap.ID]T)}
		m.accounts[id] = a
	}
	return a
}

func (a *memoryAccount[T]) record(kind string, id jmap.ID) {
	a.counter++
	a.journal = append(a.journal, journalEntry{counter: a.counter, kind: kind, id: id})
}

// State implements Store.
func (m *Memory[T]) State(_ context.Context, account jmap.ID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strconv.FormatUint(m.account(account).counter, 10), nil
}

// Get implements Store. ids == nil returns all records in insertion
// order; otherwise records are returned in the requested order.
func (m *Memory[T]) Get(_ context.Context, account jmap.ID, ids []jmap.ID) ([]T, []jmap.ID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.account(account)
	if ids == nil {
		found := make([]T, 0, len(a.order))
		for _, id := range a.order {
			found = append(found, a.objects[id])
		}
		return found, nil, nil
	}
	var found []T
	var notFound []jmap.ID
	for _, id := range ids {
		if obj, ok := a.objects[id]; ok {
			found = append(found, obj)
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

// Create implements Store. An empty id is assigned the next
// sequential id via setID; an id that already exists fails with an
// alreadyExists SetError.
func (m *Memory[T]) Create(_ context.Context, account jmap.ID, obj T) (T, *jmap.SetError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.account(account)
	var zero T
	id := m.idOf(obj)
	if id == "" {
		if m.setID == nil {
			return zero, &jmap.SetError{Type: "invalidProperties", Description: "record has no id and no setID was provided"}
		}
		for {
			a.nextID++
			id = jmap.ID(strconv.FormatUint(a.nextID, 10))
			if _, exists := a.objects[id]; !exists {
				break
			}
		}
		m.setID(&obj, id)
	} else if _, exists := a.objects[id]; exists {
		return zero, &jmap.SetError{Type: "alreadyExists", ExistingID: &id}
	}
	a.objects[id] = obj
	a.order = append(a.order, id)
	a.record("created", id)
	return obj, nil
}

// Update implements Store, returning a copy of the stored record
// (so /set reports it as the updated partial object).
func (m *Memory[T]) Update(_ context.Context, account jmap.ID, obj T) (*T, *jmap.SetError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.account(account)
	id := m.idOf(obj)
	if _, ok := a.objects[id]; !ok {
		return nil, &jmap.SetError{Type: "notFound"}
	}
	a.objects[id] = obj
	a.record("updated", id)
	updated := a.objects[id]
	return &updated, nil
}

// Destroy implements Store.
func (m *Memory[T]) Destroy(_ context.Context, account jmap.ID, id jmap.ID) *jmap.SetError {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.account(account)
	if _, ok := a.objects[id]; !ok {
		return &jmap.SetError{Type: "notFound"}
	}
	delete(a.objects, id)
	for i, oid := range a.order {
		if oid == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	a.record("destroyed", id)
	return nil
}

// Changes implements ChangeStore by replaying the journal entries
// after sinceState. An unparseable state, or one ahead of the
// current counter (issued by another process/generation), yields
// ErrCannotCalculate. When maxChanges > 0 caps the result,
// newState is the counter of the last returned entry and hasMore
// is true, so the client can page through the journal.
func (m *Memory[T]) Changes(_ context.Context, account jmap.ID, sinceState string, maxChanges int) (created, updated, destroyed []jmap.ID, newState string, hasMore bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.account(account)
	since, perr := strconv.ParseUint(sinceState, 10, 64)
	if perr != nil || since > a.counter {
		return nil, nil, nil, "", false, ErrCannotCalculate
	}
	newState = strconv.FormatUint(a.counter, 10)
	count := 0
	for _, e := range a.journal {
		if e.counter <= since {
			continue
		}
		if maxChanges > 0 && count >= maxChanges {
			hasMore = true
			break
		}
		switch e.kind {
		case "created":
			created = append(created, e.id)
		case "updated":
			updated = append(updated, e.id)
		case "destroyed":
			destroyed = append(destroyed, e.id)
		}
		count++
		if maxChanges > 0 && count >= maxChanges {
			newState = strconv.FormatUint(e.counter, 10)
		}
	}
	if hasMore {
		// newState already at last returned entry (or, if none
		// returned, leave at current: maxChanges >= 1 in practice).
		if count == 0 {
			newState = sinceState
		}
	}
	return created, updated, destroyed, newState, hasMore, nil
}
