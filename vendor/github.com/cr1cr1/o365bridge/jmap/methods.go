package jmap

import (
	"encoding/json"
	"fmt"
)

// This file defines argument and response shapes for the standard
// methods of RFC 8620 §5 (/get, /changes, /set, /query,
// /queryChanges), generic over the record type T of the data-type
// package that uses them.
//
// JSON nullability follows the RFC: a nil slice or map marshals to
// JSON null, which for most of these fields is a defined value ("no
// ids" vs "all ids" for /get, "no objects" for /set responses, and so
// on). Callers that must send an empty array instead of null should
// initialise the field to an empty non-nil value.

// PatchObject is the value type of a /set "update" entry (RFC 8620
// §5.3): a map from JSON Pointer paths (RFC 6901, with an implicit
// leading "/") to replacement values, where a null value resets the
// property to its default or removes it. Patch application semantics
// (path validation, the invalidPatch error, array handling) are
// enforced by the engine in a later phase, not by this type.
type PatchObject map[string]any

// GetArgs are the arguments of a "Foo/get" call (RFC 8620 §5.1).
type GetArgs struct {
	AccountID ID `json:"accountId"`
	// IDs lists the records to return; nil (JSON null) means all
	// records of the type, where the type supports that.
	IDs []ID `json:"ids"`
	// Properties lists the properties to return per record; nil
	// means all properties. The "id" property is always returned.
	Properties []string `json:"properties"`
}

// GetResponse is the response of a "Foo/get" call (RFC 8620 §5.1).
type GetResponse[T any] struct {
	AccountID ID     `json:"accountId"`
	State     string `json:"state"`
	List      []T    `json:"list"`
	NotFound  []ID   `json:"notFound"`
}

// ChangesArgs are the arguments of a "Foo/changes" call (RFC 8620 §5.2).
type ChangesArgs struct {
	AccountID  ID     `json:"accountId"`
	SinceState string `json:"sinceState"`
	// MaxChanges caps the total ids across created/updated/destroyed;
	// nil means the server chooses.
	MaxChanges *uint64 `json:"maxChanges"`
}

// ChangesResponse is the response of a "Foo/changes" call (§5.2).
type ChangesResponse struct {
	AccountID      ID     `json:"accountId"`
	OldState       string `json:"oldState"`
	NewState       string `json:"newState"`
	HasMoreChanges bool   `json:"hasMoreChanges"`
	Created        []ID   `json:"created"`
	Updated        []ID   `json:"updated"`
	Destroyed      []ID   `json:"destroyed"`
}

// SetError is a per-record error in a /set (or /copy) response
// (RFC 8620 §5.3). Type is one of the registered error strings
// (forbidden, overQuota, tooLarge, rateLimit, notFound, invalidPatch,
// willDestroy, invalidProperties, singleton, alreadyExists, ...).
// Properties lists the invalid properties for invalidProperties
// errors. ExistingID is required on alreadyExists errors (§5.4). Any
// further method-specific fields are preserved in Extra.
type SetError struct {
	Type        string                     `json:"-"`
	Description string                     `json:"-"`
	Properties  []string                   `json:"-"`
	ExistingID  *ID                        `json:"-"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// Error implements error.
func (e *SetError) Error() string {
	if e.Description != "" {
		return "jmap: " + e.Type + ": " + e.Description
	}
	return "jmap: " + e.Type
}

// MarshalJSON encodes {type, description?, properties?, existingId?, ...extra}.
func (e *SetError) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage, len(e.Extra)+4)
	for k, v := range e.Extra {
		m[k] = v
	}
	m["type"] = mustRaw(e.Type)
	if e.Description != "" {
		m["description"] = mustRaw(e.Description)
	}
	if e.Properties != nil {
		raw, err := json.Marshal(e.Properties)
		if err != nil {
			return nil, err
		}
		m["properties"] = raw
	}
	if e.ExistingID != nil {
		m["existingId"] = mustRaw(string(*e.ExistingID))
	}
	return marshalStringMap(m)
}

// UnmarshalJSON decodes a SetError, keeping unknown fields in Extra.
func (e *SetError) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	e.Extra = nil
	for k, v := range m {
		switch k {
		case "type":
			if err := json.Unmarshal(v, &e.Type); err != nil {
				return err
			}
		case "description":
			if err := json.Unmarshal(v, &e.Description); err != nil {
				return err
			}
		case "properties":
			if err := json.Unmarshal(v, &e.Properties); err != nil {
				return err
			}
		case "existingId":
			var id ID
			if err := json.Unmarshal(v, &id); err != nil {
				return err
			}
			e.ExistingID = &id
		default:
			if e.Extra == nil {
				e.Extra = make(map[string]json.RawMessage)
			}
			e.Extra[k] = v
		}
	}
	if e.Type == "" {
		return fmt.Errorf("jmap: SetError object has no \"type\"")
	}
	return nil
}

// SetArgs are the arguments of a "Foo/set" call (RFC 8620 §5.3),
// generic over the record type T being created. Updates are
// PatchObjects, not T, per the RFC.
type SetArgs[T any] struct {
	AccountID ID `json:"accountId"`
	// IfInState, if non-nil, must match the type's current state or
	// the call fails with a stateMismatch method error.
	IfInState *string `json:"ifInState"`
	// Create maps a client creation id to the new record.
	Create map[ID]T `json:"create"`
	// Update maps a record id to the patch to apply.
	Update map[ID]PatchObject `json:"update"`
	// Destroy lists record ids to delete.
	Destroy []ID `json:"destroy"`
}

// SetResponse is the response of a "Foo/set" call (RFC 8620 §5.3).
//
// Created and Updated values are, per the RFC, partial objects holding
// only properties the server set or changed beyond the client's
// request (Updated maps to nil for records with no such properties).
// They are typed *T as a pragmatic approximation: a data-type package
// whose partial objects are not valid T values should decode them as
// json.RawMessage or a dedicated partial struct instead.
type SetResponse[T any] struct {
	AccountID ID `json:"accountId"`
	// OldState is nil (JSON null) if the server does not know the
	// state before the changes.
	OldState     *string          `json:"oldState"`
	NewState     string           `json:"newState"`
	Created      map[ID]*T        `json:"created"`
	Updated      map[ID]*T        `json:"updated"`
	Destroyed    []ID             `json:"destroyed"`
	NotCreated   map[ID]*SetError `json:"notCreated"`
	NotUpdated   map[ID]*SetError `json:"notUpdated"`
	NotDestroyed map[ID]*SetError `json:"notDestroyed"`
}

// Comparator orders query results by one property (RFC 8620 §5.5).
// IsAscending is a pointer because the RFC makes it optional with a
// default of true: nil means "not specified".
type Comparator struct {
	Property    string `json:"property"`
	IsAscending *bool  `json:"isAscending,omitempty"`
	Collation   string `json:"collation,omitempty"`
}

// FilterOperator combines filter conditions with AND/OR/NOT
// (RFC 8620 §5.5). FilterCondition objects are data-type specific, so
// conditions are kept raw here; data-type packages define their own
// filter structs and typically embed these helpers or work with
// json.RawMessage throughout.
type FilterOperator struct {
	Operator   string            `json:"operator"` // "AND", "OR", or "NOT"
	Conditions []json.RawMessage `json:"conditions"`
}

// QueryArgs are the arguments of a "Foo/query" call (RFC 8620 §5.5).
// Filter is the data type's FilterCondition or FilterOperator object,
// kept raw because its properties are defined per data type.
type QueryArgs struct {
	AccountID ID              `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	Sort      []Comparator    `json:"sort"`
	Position  int64           `json:"position"`
	// Anchor, if non-nil, selects results relative to that record id
	// and Position is ignored (§5.5).
	Anchor         *ID     `json:"anchor"`
	AnchorOffset   int64   `json:"anchorOffset"`
	Limit          *uint64 `json:"limit"`
	CalculateTotal bool    `json:"calculateTotal"`
}

// QueryResponse is the response of a "Foo/query" call (RFC 8620 §5.5).
// Total is only present if the client requested it; Limit only if the
// server clamped the requested limit.
type QueryResponse struct {
	AccountID           ID      `json:"accountId"`
	QueryState          string  `json:"queryState"`
	CanCalculateChanges bool    `json:"canCalculateChanges"`
	Position            uint64  `json:"position"`
	IDs                 []ID    `json:"ids"`
	Total               *uint64 `json:"total,omitempty"`
	Limit               *uint64 `json:"limit,omitempty"`
}

// QueryChangesArgs are the arguments of a "Foo/queryChanges" call
// (RFC 8620 §5.6). Filter and Sort must be the same as were used with
// the /query call that produced SinceQueryState.
type QueryChangesArgs struct {
	AccountID       ID              `json:"accountId"`
	Filter          json.RawMessage `json:"filter"`
	Sort            []Comparator    `json:"sort"`
	SinceQueryState string          `json:"sinceQueryState"`
	MaxChanges      *uint64         `json:"maxChanges"`
	UpToID          *ID             `json:"upToId"`
	CalculateTotal  bool            `json:"calculateTotal"`
}

// AddedItem is one entry of a /queryChanges "added" array (§5.6).
type AddedItem struct {
	ID    ID     `json:"id"`
	Index uint64 `json:"index"`
}

// QueryChangesResponse is the response of a "Foo/queryChanges" call
// (RFC 8620 §5.6). Added must be sorted by index, lowest first.
type QueryChangesResponse struct {
	AccountID     ID          `json:"accountId"`
	OldQueryState string      `json:"oldQueryState"`
	NewQueryState string      `json:"newQueryState"`
	Total         *uint64     `json:"total,omitempty"`
	Removed       []ID        `json:"removed"`
	Added         []AddedItem `json:"added"`
}
