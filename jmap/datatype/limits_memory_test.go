package datatype_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
)

func TestGetTooManyIDsRequestTooLarge(t *testing.T) {
	srv := newServer(t)
	ids := make([]string, datatype.MaxObjectsInGet+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("id%d", i)
	}
	idsJSON, _ := json.Marshal(ids)
	resps, _ := call(t, srv, fmt.Sprintf(`[["Widget/get",{"accountId":"acct1","ids":%s},"g1"]]`, idsJSON))
	if nameOf(t, resps[0]) != "error" {
		t.Fatalf("response name = %s, want error: %s", nameOf(t, resps[0]), resps[0][1])
	}
	var me jmap.MethodError
	if err := json.Unmarshal(resps[0][1], &me); err != nil || me.Type != jmap.ErrorRequestTooLarge {
		t.Fatalf("error = %s err=%v, want requestTooLarge", resps[0][1], err)
	}
}

func TestSetTooManyCreatesRequestTooLarge(t *testing.T) {
	srv := newServer(t)
	var b strings.Builder
	b.WriteString(`[["Widget/set",{"accountId":"acct1","create":{`)
	for i := 0; i < datatype.MaxObjectsInSet+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"c%d":{"name":"w%d"}`, i, i)
	}
	b.WriteString(`}},"s1"]]`)
	resps, _ := call(t, srv, b.String())
	if nameOf(t, resps[0]) != "error" {
		t.Fatalf("response name = %s, want error: %s", nameOf(t, resps[0]), resps[0][1])
	}
	var me jmap.MethodError
	if err := json.Unmarshal(resps[0][1], &me); err != nil || me.Type != jmap.ErrorRequestTooLarge {
		t.Fatalf("error = %s err=%v, want requestTooLarge", resps[0][1], err)
	}
}

// --- Memory.Changes coalescing ---

func memoryIDs(ids []jmap.ID) []string {
	out := []string{}
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func assertIDs(t *testing.T, label string, got []jmap.ID, want ...string) {
	t.Helper()
	g := memoryIDs(got)
	if len(g) != len(want) {
		t.Fatalf("%s = %v, want %v", label, g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("%s = %v, want %v", label, g, want)
		}
	}
}

func TestMemoryChangesCoalescing(t *testing.T) {
	ctx := context.Background()
	const acct = jmap.ID("acct1")

	setup := func(t *testing.T) *datatype.Memory[widget] {
		t.Helper()
		return datatype.NewMemory[widget](
			func(w widget) jmap.ID { return w.ID },
			func(w *widget, id jmap.ID) { w.ID = id },
		)
	}
	create := func(t *testing.T, m *datatype.Memory[widget], id string) {
		t.Helper()
		if _, serr := m.Create(ctx, acct, widget{ID: jmap.ID(id), Name: id}); serr != nil {
			t.Fatalf("create %s: %v", id, serr)
		}
	}
	update := func(t *testing.T, m *datatype.Memory[widget], id string) {
		t.Helper()
		if _, serr := m.Update(ctx, acct, widget{ID: jmap.ID(id), Name: id + "u"}); serr != nil {
			t.Fatalf("update %s: %v", id, serr)
		}
	}
	destroy := func(t *testing.T, m *datatype.Memory[widget], id string) {
		t.Helper()
		if serr := m.Destroy(ctx, acct, jmap.ID(id)); serr != nil {
			t.Fatalf("destroy %s: %v", id, serr)
		}
	}
	changes := func(t *testing.T, m *datatype.Memory[widget]) (c, u, d []jmap.ID) {
		t.Helper()
		c, u, d, _, _, err := m.Changes(ctx, acct, "0", 0)
		if err != nil {
			t.Fatal(err)
		}
		return c, u, d
	}

	t.Run("createdThenUpdatedIsCreatedOnly", func(t *testing.T) {
		m := setup(t)
		create(t, m, "a")
		update(t, m, "a")
		c, u, d := changes(t, m)
		assertIDs(t, "created", c, "a")
		assertIDs(t, "updated", u)
		assertIDs(t, "destroyed", d)
	})

	t.Run("createdThenDestroyedIsNeither", func(t *testing.T) {
		m := setup(t)
		create(t, m, "a")
		destroy(t, m, "a")
		c, u, d := changes(t, m)
		assertIDs(t, "created", c)
		assertIDs(t, "updated", u)
		assertIDs(t, "destroyed", d)
	})

	t.Run("updatedThenDestroyedIsDestroyedOnly", func(t *testing.T) {
		m := setup(t)
		create(t, m, "a")
		// Replay from after creation so the update/destroy are in window:
		// use a fresh check via since state "1".
		update(t, m, "a")
		destroy(t, m, "a")
		c, u, d, _, _, err := m.Changes(ctx, acct, "1", 0)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, "created", c)
		assertIDs(t, "updated", u)
		assertIDs(t, "destroyed", d, "a")
	})

	t.Run("updatedThenUpdatedIsUpdatedOnce", func(t *testing.T) {
		m := setup(t)
		create(t, m, "a")
		update(t, m, "a")
		update(t, m, "a")
		_, u, d, _, _, err := m.Changes(ctx, acct, "1", 0)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, "updated", u, "a")
		assertIDs(t, "destroyed", d)
	})

	t.Run("destroyedIsTerminal", func(t *testing.T) {
		m := setup(t)
		create(t, m, "a")
		destroy(t, m, "a")
		// Even if a later journal entry referenced the id, destroyed
		// would win; here verify the plain destroy reports once.
		_, _, d, _, _, err := m.Changes(ctx, acct, "1", 0)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, "destroyed", d, "a")
	})

	t.Run("firstSeenOrderingPreserved", func(t *testing.T) {
		m := setup(t)
		create(t, m, "b")
		create(t, m, "a")
		update(t, m, "b") // b stays created, first in created list
		c, _, _ := changes(t, m)
		assertIDs(t, "created", c, "b", "a")
	})
}
