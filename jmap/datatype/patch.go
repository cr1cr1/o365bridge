package datatype

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// ApplyPatch applies a /set update PatchObject (RFC 8620 §5.3) to
// obj and returns the patched value.
//
// Patch keys are property names, or JSON-pointer-style paths joined
// with "/" ("address/street", "items/0/name"; §5.3 makes the leading
// "/" implicit, and ~0/~1 escapes are honoured). A null value resets
// the property: the key is removed from its parent object. Missing
// intermediate objects are created; an intermediate step whose next
// token is an array index creates an array instead. Array indexes
// may address an existing element or append at exactly len (or with
// the "-" token).
//
// Limits of this generic implementation (it round-trips through
// map[string]any, having no schema for T):
//
//   - Numbers round-trip as float64, so integer fields larger than
//     2^53 may lose precision, and a patch cannot distinguish 1 from
//     1.0.
//   - A null applied to an array element removes (splices out) that
//     element rather than nulling it; §5.3 only defines null as
//     resetting a property to its default, and arrays of nullable
//     values cannot be expressed.
//   - Struct fields that marshal with omitempty may not reappear
//     after a create-style patch if the patched value is the zero
//     value; fields unknown to T are dropped on the final unmarshal
//     rather than reported as invalidProperties.
//   - Patching a T that does not marshal to a JSON object is an
//     error.
func ApplyPatch[T any](obj T, patch jmap.PatchObject) (T, error) {
	var zero T
	raw, err := json.Marshal(obj)
	if err != nil {
		return zero, err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return zero, err
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return zero, fmt.Errorf("datatype: ApplyPatch requires a record that marshals to a JSON object")
	}

	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic application order
	for _, key := range keys {
		if err := applyPatchValue(&root, key, patch[key]); err != nil {
			return zero, err
		}
		if root == nil {
			root = map[string]any{}
		}
	}

	out, err := json.Marshal(root)
	if err != nil {
		return zero, err
	}
	var result T
	if err := json.Unmarshal(out, &result); err != nil {
		return zero, fmt.Errorf("datatype: patched value does not decode into record type: %w", err)
	}
	return result, nil
}

func applyPatchValue(root *map[string]any, key string, value any) error {
	if key == "" {
		return fmt.Errorf("datatype: empty patch key")
	}
	tokens := strings.Split(key, "/")
	for i, tok := range tokens {
		tok = strings.ReplaceAll(tok, "~1", "/")
		tok = strings.ReplaceAll(tok, "~0", "~")
		tokens[i] = tok
	}
	v, err := setPath(any(*root), tokens, value)
	if err != nil {
		return fmt.Errorf("datatype: patch %q: %w", key, err)
	}
	m, _ := v.(map[string]any)
	*root = m
	return nil
}

// setPath sets value at tokens within node, creating intermediate
// containers, and returns the (possibly replaced) node. A nil value
// deletes the addressed map key / splices out the array element.
func setPath(node any, tokens []string, value any) (any, error) {
	if len(tokens) == 0 {
		return value, nil
	}
	tok := tokens[0]
	last := len(tokens) == 1

	if isIndexToken(tok) {
		arr, _ := node.([]any)
		if node != nil && arr == nil {
			return nil, fmt.Errorf("index %q applied to a non-array value", tok)
		}
		idx, appendMode, err := arrayIndex(tok, len(arr))
		if err != nil {
			return nil, err
		}
		if last {
			switch {
			case value == nil && !appendMode:
				return append(arr[:idx], arr[idx+1:]...), nil
			case appendMode || idx == len(arr):
				return append(arr, value), nil
			default:
				arr[idx] = value
				return arr, nil
			}
		}
		var child any
		if idx < len(arr) {
			child = arr[idx]
		} else {
			child = newContainer(tokens[1])
		}
		newChild, err := setPath(child, tokens[1:], value)
		if err != nil {
			return nil, err
		}
		if idx == len(arr) {
			return append(arr, newChild), nil
		}
		arr[idx] = newChild
		return arr, nil
	}

	m, _ := node.(map[string]any)
	if node != nil && m == nil {
		return nil, fmt.Errorf("key %q applied to a non-object value", tok)
	}
	if m == nil {
		m = map[string]any{}
	}
	if last {
		if value == nil {
			delete(m, tok)
			return m, nil
		}
		m[tok] = value
		return m, nil
	}
	child, ok := m[tok]
	if !ok {
		child = newContainer(tokens[1])
	}
	newChild, err := setPath(child, tokens[1:], value)
	if err != nil {
		return nil, err
	}
	m[tok] = newChild
	return m, nil
}

func newContainer(nextToken string) any {
	if isIndexToken(nextToken) {
		return []any{}
	}
	return map[string]any{}
}

// isIndexToken reports whether tok should address an array: a
// non-negative decimal integer or the append token "-".
func isIndexToken(tok string) bool {
	if tok == "-" {
		return true
	}
	if tok == "" {
		return false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

func arrayIndex(tok string, length int) (idx int, appendMode bool, err error) {
	if tok == "-" {
		return length, true, nil
	}
	n, err := strconv.Atoi(tok)
	if err != nil || n < 0 || n > length {
		return 0, false, fmt.Errorf("array index %q out of bounds for length %d", tok, length)
	}
	return n, false, nil
}
