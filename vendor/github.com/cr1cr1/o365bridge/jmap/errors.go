package jmap

import (
	"encoding/json"
	"fmt"
	"sort"
)

// MethodError is a JMAP method-level error object (RFC 8620 §3.6.2),
// returned as the arguments of an ["error", {...}, callId] invocation.
// It implements the Go error interface.
//
// Type is one of the error type strings registered in the JMAP Error
// Codes registry (§9.5); the New* constructors below cover the types
// defined by RFC 8620. Fields other than type and description that a
// particular error defines are preserved verbatim in Extra.
type MethodError struct {
	Type        string                     `json:"-"`
	Description string                     `json:"-"`
	Extra       map[string]json.RawMessage `json:"-"`
}

// Error implements error.
func (e *MethodError) Error() string {
	if e.Description != "" {
		return "jmap: " + e.Type + ": " + e.Description
	}
	return "jmap: " + e.Type
}

// Invocation returns this error as an ["error", {...}, callID]
// invocation for a Response's methodResponses (§3.6.2).
func (e *MethodError) Invocation(callID string) *Invocation {
	inv, _ := NewInvocation("error", e, callID) // MethodError marshaling cannot fail
	return inv
}

// MarshalJSON encodes {type, description?, ...extra}. Extra keys must
// not be "type" or "description"; if they are, the explicit fields win.
func (e *MethodError) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage, len(e.Extra)+2)
	for k, v := range e.Extra {
		m[k] = v
	}
	m["type"] = mustRaw(e.Type)
	if e.Description != "" {
		m["description"] = mustRaw(e.Description)
	}
	return marshalStringMap(m)
}

// UnmarshalJSON decodes a method error, keeping unknown fields in Extra.
func (e *MethodError) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	e.Extra = nil
	for k, v := range m {
		switch k {
		case "type":
			if err := json.Unmarshal(v, &e.Type); err != nil {
				return fmt.Errorf("jmap: error type: %w", err)
			}
		case "description":
			if err := json.Unmarshal(v, &e.Description); err != nil {
				return fmt.Errorf("jmap: error description: %w", err)
			}
		default:
			if e.Extra == nil {
				e.Extra = make(map[string]json.RawMessage)
			}
			e.Extra[k] = v
		}
	}
	if e.Type == "" {
		return fmt.Errorf("jmap: method error object has no \"type\"")
	}
	return nil
}

// marshalStringMap marshals a map with sorted keys for deterministic
// output (error objects are small; stable bytes ease golden tests).
func marshalStringMap(m map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []byte{'{'}
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		kb, _ := json.Marshal(k)
		out = append(out, kb...)
		out = append(out, ':')
		out = append(out, m[k]...)
	}
	return append(out, '}'), nil
}

// Method error type strings (RFC 8620 §3.6.2, plus the method-specific
// types defined for the standard methods in §5). These are the values
// used in MethodError.Type and SetError.Type, without any prefix.
const (
	ErrorInvalidArguments            = "invalidArguments"            // §3.6.2
	ErrorInvalidResultReference      = "invalidResultReference"      // §3.6.2
	ErrorForbidden                   = "forbidden"                   // §3.6.2
	ErrorAccountNotFound             = "accountNotFound"             // §3.6.2
	ErrorAccountNotSupportedByMethod = "accountNotSupportedByMethod" // §3.6.2
	ErrorAccountReadOnly             = "accountReadOnly"             // §3.6.2
	ErrorServerUnavailable           = "serverUnavailable"           // §3.6.2
	ErrorServerFail                  = "serverFail"                  // §3.6.2
	ErrorServerPartialFail           = "serverPartialFail"           // §3.6.2
	ErrorUnknownMethod               = "unknownMethod"               // §3.6.2
	ErrorRequestTooLarge             = "requestTooLarge"             // §5.1, §5.3
	ErrorStateMismatch               = "stateMismatch"               // §5.3
	ErrorCannotCalculateChanges      = "cannotCalculateChanges"      // §5.2, §5.6
	ErrorAnchorNotFound              = "anchorNotFound"              // §5.5
	ErrorUnsupportedFilter           = "unsupportedFilter"           // §5.5
	ErrorUnsupportedSort             = "unsupportedSort"             // §5.5
	ErrorTooManyChanges              = "tooManyChanges"              // §5.6
)

// NewMethodError builds a MethodError of the given registered type.
// Prefer the typed constructors below for the RFC 8620 types.
func NewMethodError(errorType, description string) *MethodError {
	return &MethodError{Type: errorType, Description: description}
}

// Typed constructors for the RFC 8620 method error types (§3.6.2, §5).
// The description argument is optional debugging text ("" to omit);
// it is not localised and not for end users (§3.6.2).
func NewInvalidArguments(desc string) *MethodError {
	return NewMethodError(ErrorInvalidArguments, desc)
}
func NewInvalidResultReference(desc string) *MethodError {
	return NewMethodError(ErrorInvalidResultReference, desc)
}
func NewForbidden(desc string) *MethodError { return NewMethodError(ErrorForbidden, desc) }
func NewAccountNotFound(desc string) *MethodError {
	return NewMethodError(ErrorAccountNotFound, desc)
}
func NewAccountNotSupportedByMethod(desc string) *MethodError {
	return NewMethodError(ErrorAccountNotSupportedByMethod, desc)
}
func NewAccountReadOnly(desc string) *MethodError {
	return NewMethodError(ErrorAccountReadOnly, desc)
}
func NewServerUnavailable(desc string) *MethodError {
	return NewMethodError(ErrorServerUnavailable, desc)
}
func NewServerFail(desc string) *MethodError { return NewMethodError(ErrorServerFail, desc) }
func NewServerPartialFail(desc string) *MethodError {
	return NewMethodError(ErrorServerPartialFail, desc)
}
func NewUnknownMethod(desc string) *MethodError {
	return NewMethodError(ErrorUnknownMethod, desc)
}
func NewRequestTooLarge(desc string) *MethodError {
	return NewMethodError(ErrorRequestTooLarge, desc)
}
func NewStateMismatch(desc string) *MethodError {
	return NewMethodError(ErrorStateMismatch, desc)
}
func NewCannotCalculateChanges(desc string) *MethodError {
	return NewMethodError(ErrorCannotCalculateChanges, desc)
}
func NewAnchorNotFound(desc string) *MethodError {
	return NewMethodError(ErrorAnchorNotFound, desc)
}
func NewUnsupportedFilter(desc string) *MethodError {
	return NewMethodError(ErrorUnsupportedFilter, desc)
}
func NewUnsupportedSort(desc string) *MethodError {
	return NewMethodError(ErrorUnsupportedSort, desc)
}
func NewTooManyChanges(desc string) *MethodError {
	return NewMethodError(ErrorTooManyChanges, desc)
}

// Request-level error types (RFC 8620 §3.6.1) are NOT MethodError
// types: the whole request is rejected with an HTTP error status and
// an RFC 7807 problem-details body whose "type" is one of these full
// URNs (the urn:ietf:params:jmap:error: prefix is included here,
// unlike in method errors; §9.5). Problem is that body.
const (
	ProblemUnknownCapability = "urn:ietf:params:jmap:error:unknownCapability"
	ProblemNotJSON           = "urn:ietf:params:jmap:error:notJSON"
	ProblemNotRequest        = "urn:ietf:params:jmap:error:notRequest"
	ProblemLimit             = "urn:ietf:params:jmap:error:limit"
)

// Problem is an RFC 7807 problem-details object as used for JMAP
// request-level errors (RFC 8620 §3.6.1).
type Problem struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Limit names the exceeded limit (e.g. "maxSizeRequest"); it is
	// required on ProblemLimit errors and empty otherwise (§3.6.1).
	Limit string `json:"limit,omitempty"`
}

// Error implements error, so a Problem can flow through Go error paths
// before the engine renders it as an HTTP response.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return "jmap: " + p.Type + ": " + p.Detail
	}
	return "jmap: " + p.Type
}
