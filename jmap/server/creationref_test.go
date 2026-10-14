package server_test

import (
	"context"
	"encoding/json"
	"testing"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

func TestCreationIDReferencesResolvedForLaterCalls(t *testing.T) {
	e, srv := newEngine(t, server.Options{})
	e.RegisterMethod("Test/create", jmap.CoreURI, func(_ context.Context, c *server.Call) (any, error) {
		c.SetCreated("c1", "srv1")
		return map[string]any{"ok": true}, nil
	})
	var got []json.RawMessage
	e.RegisterMethod("Test/echo-args", jmap.CoreURI, func(_ context.Context, c *server.Call) (any, error) {
		got = append(got, append(json.RawMessage(nil), c.Args...))
		return map[string]any{"ok": true}, nil
	})

	status, resp, raw := postAPI(t, srv, map[string]any{
		"using":      []string{string(jmap.CoreURI)},
		"createdIds": map[string]string{"seed1": "srvSeed"},
		"methodCalls": []any{
			call("Test/create", map[string]any{}, "0"),
			call("Test/echo-args", map[string]any{
				"ref":    "#c1",
				"list":   []any{"#c1", "#unknown", "plain"},
				"nested": map[string]any{"deep": []any{map[string]any{"v": "#c1"}}},
				"seeded": "#seed1",
				// Key "#c1" is a property name, not a value: key must
				// stay, its string value must be substituted.
				"keyTest": map[string]any{"#c1": "#c1"},
			}, "1"),
		},
	})
	if status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	if len(resp.MethodResponses) != 2 || len(got) != 1 {
		t.Fatalf("responses=%d got=%d raw=%s", len(resp.MethodResponses), len(got), raw)
	}
	var args struct {
		Ref    string   `json:"ref"`
		List   []string `json:"list"`
		Nested struct {
			Deep []struct {
				V string `json:"v"`
			} `json:"deep"`
		} `json:"nested"`
		Seeded string `json:"seeded"`
	}
	if err := json.Unmarshal(got[0], &args); err != nil {
		t.Fatal(err)
	}
	if args.Ref != "srv1" {
		t.Fatalf("ref = %q, want srv1 (args %s)", args.Ref, got[0])
	}
	if len(args.List) != 3 || args.List[0] != "srv1" || args.List[1] != "#unknown" || args.List[2] != "plain" {
		t.Fatalf("list = %v (args %s)", args.List, got[0])
	}
	if len(args.Nested.Deep) != 1 || args.Nested.Deep[0].V != "srv1" {
		t.Fatalf("nested = %+v (args %s)", args.Nested, got[0])
	}
	if args.Seeded != "srvSeed" {
		t.Fatalf("seeded = %q, want srvSeed from request createdIds (args %s)", args.Seeded, got[0])
	}
	var rawArgs map[string]json.RawMessage
	if err := json.Unmarshal(got[0], &rawArgs); err != nil {
		t.Fatal(err)
	}
	var keyTest map[string]string
	if err := json.Unmarshal(rawArgs["keyTest"], &keyTest); err != nil || keyTest["#c1"] != "srv1" {
		t.Fatalf("keyTest = %s err=%v, want {\"#c1\":\"srv1\"}", rawArgs["keyTest"], err)
	}
}
