package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// serveAPI implements the RFC 8620 §3 API endpoint.
func (e *Engine) serveAPI(w http.ResponseWriter, r *http.Request) {
	p := e.authenticate(w, r)
	if p == nil {
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, _ := strings.Cut(ct, ";")
		if strings.TrimSpace(mt) != "application/json" {
			writeProblem(w, &jmap.Problem{Type: jmap.ProblemNotJSON, Status: http.StatusBadRequest, Detail: "Content-Type is not application/json"})
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(e.core.MaxSizeRequest)))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeProblem(w, &jmap.Problem{Type: jmap.ProblemLimit, Status: http.StatusRequestEntityTooLarge, Limit: "maxSizeRequest"})
			return
		}
		writeProblem(w, &jmap.Problem{Type: jmap.ProblemNotJSON, Status: http.StatusBadRequest, Detail: err.Error()})
		return
	}
	if !json.Valid(body) {
		writeProblem(w, &jmap.Problem{Type: jmap.ProblemNotJSON, Status: http.StatusBadRequest, Detail: "request body is not JSON"})
		return
	}
	var req jmap.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, &jmap.Problem{Type: jmap.ProblemNotRequest, Status: http.StatusBadRequest, Detail: "request body is not a JMAP request object"})
		return
	}
	resp, prob := e.process(r.Context(), p, &req)
	if prob != nil {
		writeProblem(w, prob)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (e *Engine) dispatchOne(ctx context.Context, p *Principal, inv *jmap.Invocation, using map[jmap.URI]bool, prev map[string]*jmap.Invocation, created map[jmap.ID]jmap.ID) (out *jmap.Invocation) {
	args, err := resolveResultReferences(inv.Args, prev)
	if err != nil {
		return jmap.NewInvalidResultReference(err.Error()).Invocation(inv.CallID)
	}
	args = resolveCreationReferences(args, created)
	e.mu.RLock()
	entry, ok := e.methods[inv.Name]
	e.mu.RUnlock()
	if !ok {
		return jmap.NewUnknownMethod(inv.Name).Invocation(inv.CallID)
	}
	if entry.capability != "" && !using[entry.capability] {
		// Decision (documented in the Phase 1b brief): RFC 8620 has no
		// method-level unknownCapability type (§3.6.2) — the
		// unknownCapability problem type is request-level. We still
		// surface the string as a method error type here, pragmatically,
		// so one under-declared call does not fail the whole request;
		// later phases may refine this.
		return jmap.NewMethodError("unknownCapability", "method "+inv.Name+" requires capability "+string(entry.capability)).Invocation(inv.CallID)
	}
	call := &Call{Name: inv.Name, CallID: inv.CallID, Args: args, Principal: p, Created: map[jmap.ID]jmap.ID{}}

	defer func() {
		if rec := recover(); rec != nil {
			out = jmap.NewServerFail(fmt.Sprintf("panic in %s: %v", inv.Name, rec)).Invocation(inv.CallID)
		}
	}()
	res, herr := entry.handler(ctx, call)
	for k, v := range call.Created {
		created[k] = v
	}
	if herr != nil {
		var me *jmap.MethodError
		if errors.As(herr, &me) {
			return me.Invocation(inv.CallID)
		}
		return jmap.NewServerFail(herr.Error()).Invocation(inv.CallID)
	}
	raw, err := marshalArgs(res)
	if err != nil {
		return jmap.NewServerFail(err.Error()).Invocation(inv.CallID)
	}
	return &jmap.Invocation{Name: inv.Name, Args: raw, CallID: inv.CallID}
}

func marshalArgs(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage(`{}`), nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 {
			return json.RawMessage(`{}`), nil
		}
		return raw, nil
	}
	return json.Marshal(v)
}

// --- result references (RFC 8620 §3.7) ---

// resolveResultReferences replaces every "#name" argument whose value
// is a ResultReference with name -> pointer-lookup into the referenced
// earlier response's arguments.
func resolveResultReferences(args json.RawMessage, prev map[string]*jmap.Invocation) (json.RawMessage, error) {
	if len(args) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return nil, fmt.Errorf("arguments are not an object: %w", err)
	}
	changed := false
	for key, raw := range obj {
		if !strings.HasPrefix(key, "#") {
			continue
		}
		var ref jmap.ResultReference
		if err := json.Unmarshal(raw, &ref); err != nil {
			return nil, fmt.Errorf("invalid result reference for %q: %w", key, err)
		}
		val, err := lookupReference(ref, prev)
		if err != nil {
			return nil, err
		}
		enc, err := json.Marshal(val)
		if err != nil {
			return nil, err
		}
		delete(obj, key)
		obj[key[1:]] = enc
		changed = true
	}
	if !changed {
		return args, nil
	}
	return json.Marshal(obj)
}

// resolveCreationReferences replaces any JSON string value exactly
// equal to "#<creationId>" (anywhere nested in objects/arrays) with the
// server id recorded for that creation id, per RFC 8620 §3.3/§5.3
// creation-id references. Only string values are considered: keys
// (including "#"-prefixed result-reference keys, already handled by
// resolveResultReferences) are never touched, and unknown "#..."
// strings are left untouched. Original bytes are returned unchanged
// when nothing matches.
func resolveCreationReferences(args json.RawMessage, created map[jmap.ID]jmap.ID) json.RawMessage {
	if len(args) == 0 || len(created) == 0 {
		return args
	}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return args
	}
	if !substituteCreationRefs(doc, created) {
		return args
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return args
	}
	return out
}

func substituteCreationRefs(v any, created map[jmap.ID]jmap.ID) bool {
	changed := false
	switch cur := v.(type) {
	case map[string]any:
		for k, el := range cur {
			if s, ok := el.(string); ok {
				if id, hit := lookupCreationRef(s, created); hit {
					cur[k] = string(id)
					changed = true
				}
				continue
			}
			if substituteCreationRefs(el, created) {
				changed = true
			}
		}
	case []any:
		for i, el := range cur {
			if s, ok := el.(string); ok {
				if id, hit := lookupCreationRef(s, created); hit {
					cur[i] = string(id)
					changed = true
				}
				continue
			}
			if substituteCreationRefs(el, created) {
				changed = true
			}
		}
	}
	return changed
}

func lookupCreationRef(s string, created map[jmap.ID]jmap.ID) (jmap.ID, bool) {
	if !strings.HasPrefix(s, "#") || len(s) == 1 {
		return "", false
	}
	id, ok := created[jmap.ID(s[1:])]
	return id, ok
}

func lookupReference(ref jmap.ResultReference, prev map[string]*jmap.Invocation) (any, error) {
	target, ok := prev[ref.ResultOf]
	if !ok {
		return nil, fmt.Errorf("no previous result with call id %q", ref.ResultOf)
	}
	if target.Name != ref.Name {
		return nil, fmt.Errorf("result %q is named %q, not %q", ref.ResultOf, target.Name, ref.Name)
	}
	var doc any
	if err := json.Unmarshal(target.Args, &doc); err != nil {
		return nil, err
	}
	if ref.Path == "" || ref.Path == "/" {
		// "" is the whole document; a bare "/" denotes the empty-string
		// key per RFC 6901 — treat both leniently as the whole args only
		// for "", and do a real lookup for "/".
		if ref.Path == "" {
			return doc, nil
		}
	}
	tokens, err := pointerTokens(ref.Path)
	if err != nil {
		return nil, err
	}
	return evalPointer(doc, tokens)
}

func pointerTokens(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path %q is not a JSON Pointer", path)
	}
	parts := strings.Split(path[1:], "/")
	for i, tok := range parts {
		tok = strings.ReplaceAll(tok, "~1", "/")
		tok = strings.ReplaceAll(tok, "~0", "~")
		parts[i] = tok
	}
	return parts, nil
}

// evalPointer evaluates JSON Pointer tokens with the RFC 8620 §3.7 "*"
// extension: on an array, "*" applies the remaining tokens to every
// element and flattens the results into a single array.
func evalPointer(v any, tokens []string) (any, error) {
	if len(tokens) == 0 {
		return v, nil
	}
	tok := tokens[0]
	if tok == "*" {
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("'*' applied to a non-array value")
		}
		out := []any{}
		for _, el := range arr {
			r, err := evalPointer(el, tokens[1:])
			if err != nil {
				return nil, err
			}
			if ra, ok := r.([]any); ok {
				out = append(out, ra...)
			} else {
				out = append(out, r)
			}
		}
		return out, nil
	}
	switch cur := v.(type) {
	case map[string]any:
		next, ok := cur[tok]
		if !ok {
			return nil, fmt.Errorf("no key %q in referenced result", tok)
		}
		return evalPointer(next, tokens[1:])
	case []any:
		i, err := strconv.Atoi(tok)
		if err != nil || i < 0 || i >= len(cur) {
			return nil, fmt.Errorf("invalid array index %q in referenced result", tok)
		}
		return evalPointer(cur[i], tokens[1:])
	default:
		return nil, fmt.Errorf("cannot descend into scalar value at %q", tok)
	}
}
