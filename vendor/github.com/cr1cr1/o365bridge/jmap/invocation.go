package jmap

import (
	"encoding/json"
	"fmt"
)

// Invocation is a single JMAP method call or method response
// (RFC 8620 §3.2). On the wire it is a 3-element JSON array:
//
//	[name, arguments, methodCallId]
//
// Args is deliberately kept as raw JSON. Unlike a JMAP client library,
// a server must dispatch on Name without resolving it to a statically
// known response type, so arguments are decoded per method by the
// engine (see DecodeArgs) rather than by this type.
type Invocation struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

// NewInvocation builds an Invocation by marshaling args (typically a
// struct or map). A nil args is sent as an empty object, matching the
// RFC's examples.
func NewInvocation(name string, args any, callID string) (*Invocation, error) {
	if args == nil {
		return &Invocation{Name: name, Args: json.RawMessage(`{}`), CallID: callID}, nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return &Invocation{Name: name, Args: raw, CallID: callID}, nil
}

// DecodeArgs unmarshals the invocation's arguments into v.
func (inv *Invocation) DecodeArgs(v any) error {
	if len(inv.Args) == 0 {
		return fmt.Errorf("jmap: invocation %q has no arguments", inv.Name)
	}
	return json.Unmarshal(inv.Args, v)
}

// MarshalJSON encodes the invocation as [name, args, callId] (§3.2).
// A nil Args is encoded as {}.
func (inv Invocation) MarshalJSON() ([]byte, error) {
	args := inv.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return json.Marshal([3]json.RawMessage{
		mustRaw(inv.Name),
		args,
		mustRaw(inv.CallID),
	})
}

// UnmarshalJSON decodes a 3-element [name, args, callId] array (§3.2).
func (inv *Invocation) UnmarshalJSON(data []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	if len(parts) != 3 {
		return fmt.Errorf("jmap: invocation must be a 3-element array, got %d elements", len(parts))
	}
	var name, callID string
	if err := json.Unmarshal(parts[0], &name); err != nil {
		return fmt.Errorf("jmap: invocation name: %w", err)
	}
	if err := json.Unmarshal(parts[2], &callID); err != nil {
		return fmt.Errorf("jmap: invocation method call id: %w", err)
	}
	inv.Name = name
	// Copy: parts aliases data, which the caller may reuse.
	inv.Args = append(json.RawMessage(nil), parts[1]...)
	inv.CallID = callID
	return nil
}

func mustRaw(s string) json.RawMessage {
	raw, _ := json.Marshal(s) // marshaling a string cannot fail
	return raw
}

// Request is a JMAP API request object (RFC 8620 §3.3).
type Request struct {
	Using       []URI         `json:"using"`
	MethodCalls []*Invocation `json:"methodCalls"`
	// CreatedIDs maps client creation ids to server-assigned ids. It
	// is optional in the request; if present, the Response echoes the
	// (extended) map (§3.3, §3.4).
	CreatedIDs map[ID]ID `json:"createdIds,omitempty"`
}

// Response is a JMAP API response object (RFC 8620 §3.4).
type Response struct {
	MethodResponses []*Invocation `json:"methodResponses"`
	CreatedIDs      map[ID]ID     `json:"createdIds,omitempty"`
	SessionState    string        `json:"sessionState"`
}

// ResultReference refers to (part of) the arguments of a previous
// method response in the same request (RFC 8620 §3.7). In a method
// call it appears as the value of an argument whose name is prefixed
// with "#"; the engine replaces that argument with the referenced
// value before dispatch.
//
// Path is a JSON Pointer (RFC 6901) applied to the selected response's
// arguments object, extended with a "*" reference token: when the
// currently referenced value is an array, "*" applies the remaining
// tokens to every element and returns the results flattened into a
// single array (§3.7, step 3).
type ResultReference struct {
	// ResultOf is the method call id of a previous method call.
	ResultOf string `json:"resultOf"`
	// Name is the required name of a response to that method call.
	Name string `json:"name"`
	// Path is the JSON Pointer (with the "*" extension) into the
	// response's arguments.
	Path string `json:"path"`
}
