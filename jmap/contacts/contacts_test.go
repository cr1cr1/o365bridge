package contacts_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/contacts"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
	"github.com/cr1cr1/o365bridge/jmap/sharing"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, _ *http.Request) (*server.Principal, error) {
	return &server.Principal{
		Username:        "user@example.com",
		Accounts:        map[jmap.ID]*jmap.Account{"acct1": {Name: "User", IsPersonal: true}},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.CoreURI: "acct1", jmap.ContactsURI: "acct1"},
	}, nil
}

type backend struct {
	books *datatype.Memory[contacts.AddressBook]
	cards *datatype.Memory[contacts.Card]
}

func (b *backend) AddressBooks() datatype.Store[contacts.AddressBook] { return b.books }
func (b *backend) Cards() datatype.Store[contacts.Card]               { return b.cards }

func strptr(s string) *string { return &s }

func seed(t *testing.T) (*httptest.Server, *backend) {
	t.Helper()
	b := &backend{
		books: datatype.NewMemory[contacts.AddressBook](
			func(a contacts.AddressBook) jmap.ID { return a.ID },
			func(a *contacts.AddressBook, id jmap.ID) { a.ID = id }),
		cards: datatype.NewMemory[contacts.Card](
			func(c contacts.Card) jmap.ID { return c.ID },
			func(c *contacts.Card, id jmap.ID) { c.ID = id }),
	}
	ctx := context.Background()
	rights := &contacts.AddressBookRights{MayReadItems: true, MayAddItems: true, MayUpdateItems: true, MayRemoveItems: true, MayDelete: true, MayRename: true, MayShare: true}
	for _, a := range []contacts.AddressBook{
		{ID: "ab-personal", Name: "Personal", Description: strptr("Personal contacts"), SortOrder: 0, IsDefault: true, IsSubscribed: true,
			ShareWith: sharing.SharedWith{"principal-1": {"mayReadItems": true}}, MyRights: rights},
		{ID: "ab-work", Name: "Work", SortOrder: 10, IsSubscribed: true, MyRights: rights},
	} {
		if _, serr := b.books.Create(ctx, "acct1", a); serr != nil {
			t.Fatal(serr)
		}
	}
	e := server.New(fakeAuth{}, server.Options{})
	contacts.Register(e, b)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv, b
}

func call(t *testing.T, srv *httptest.Server, methodCalls string) [][3]json.RawMessage {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:contacts"],"methodCalls":%s}`, methodCalls)
	resp, err := http.Post(srv.URL+"/jmap/", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d", resp.StatusCode)
	}
	var decoded struct {
		MethodResponses [][3]json.RawMessage `json:"methodResponses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.MethodResponses
}

func argsOf(t *testing.T, resp [3]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(resp[1], &m); err != nil {
		t.Fatalf("args not object: %v (%s)", err, resp[1])
	}
	return m
}

func nameOf(resp [3]json.RawMessage) string {
	var n string
	_ = json.Unmarshal(resp[0], &n)
	return n
}

func queryIDs(t *testing.T, srv *httptest.Server, method, args string) []string {
	t.Helper()
	resps := call(t, srv, fmt.Sprintf(`[[%q,{%s},"q"]]`, method, args))
	if nameOf(resps[0]) == "error" {
		t.Fatalf("%s error: %s", method, resps[0][1])
	}
	var ids []string
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestAddressBookGetSet(t *testing.T) {
	srv, _ := seed(t)
	// get
	resps := call(t, srv, `[["AddressBook/get",{"accountId":"acct1","ids":["ab-personal"]},"g"]]`)
	args := argsOf(t, resps[0])
	var list []contacts.AddressBook
	if err := json.Unmarshal(args["list"], &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %s err %v", args["list"], err)
	}
	if list[0].Name != "Personal" || !list[0].IsDefault || list[0].Description == nil || *list[0].Description != "Personal contacts" ||
		list[0].MyRights == nil || !list[0].MyRights.MayShare || !list[0].ShareWith["principal-1"]["mayReadItems"] {
		t.Fatalf("addressBook = %+v", list[0])
	}
	// query: name substring ci + isDefault
	ids := queryIDs(t, srv, "AddressBook/query", `"accountId":"acct1","filter":{"name":"PERSO","isDefault":true}`)
	if len(ids) != 1 || ids[0] != "ab-personal" {
		t.Fatalf("query ids = %v", ids)
	}
	// set: create
	resps = call(t, srv, `[["AddressBook/set",{"accountId":"acct1","create":{"c1":{"name":"Family","description":null,"sortOrder":5,"isSubscribed":true}}},"s"]]`)
	args = argsOf(t, resps[0])
	var created map[string]contacts.AddressBook
	if err := json.Unmarshal(args["created"], &created); err != nil || created["c1"].ID == "" {
		t.Fatalf("created = %s err %v", args["created"], err)
	}
	newID := string(created["c1"].ID)
	// set: rename via update patch, then destroy
	resps = call(t, srv, fmt.Sprintf(`[["AddressBook/set",{"accountId":"acct1","update":{%q:{"name":"Family 2026"}}},"s"]]`, newID))
	if string(argsOf(t, resps[0])["notUpdated"]) != "null" {
		t.Fatalf("notUpdated = %s", argsOf(t, resps[0])["notUpdated"])
	}
	ids = queryIDs(t, srv, "AddressBook/query", `"accountId":"acct1","filter":{"name":"family"},"sort":[{"property":"sortOrder"}]`)
	if len(ids) != 1 || ids[0] != newID {
		t.Fatalf("renamed query ids = %v", ids)
	}
	resps = call(t, srv, fmt.Sprintf(`[["AddressBook/set",{"accountId":"acct1","destroy":[%q]},"s"]]`, newID))
	var destroyed []string
	if err := json.Unmarshal(argsOf(t, resps[0])["destroyed"], &destroyed); err != nil || len(destroyed) != 1 {
		t.Fatalf("destroyed = %s", argsOf(t, resps[0])["destroyed"])
	}
}

// createCard creates one rich Card via ContactCard/set and returns its server id.
func createCard(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	create := `{
		"@type":"Card","version":"1.0","kind":"individual","language":"en",
		"name":{"full":"Alice Example","components":[{"type":"given","value":"Alice"},{"type":"surname","value":"Example"}],"isOrdered":true},
		"organizations":{"org1":{"name":"Example Inc","units":[{"name":"Engineering"}]}},
		"titles":{"t1":{"name":"Engineer","organizationId":"org1"}},
		"emails":{"e1":{"address":"alice@example.com","contexts":{"work":true},"pref":1}},
		"phones":{"p1":{"number":"+1-555-0100","features":{"voice":true},"contexts":{"work":true}}},
		"onlineServices":{"o1":{"service":"github","user":"alice"}},
		"addresses":{"a1":{"full":"1 Main St, Springfield","components":[{"type":"locality","value":"Springfield"},{"type":"postcode","value":"12345"}],"countryCode":"US","timeZone":"America/New_York","contexts":{"home":true}}},
		"anniversaries":{"ann1":{"type":"birth","date":{"year":1990,"month":4,"day":25}}},
		"notes":{"n1":{"note":"met at conference"}},
		"created":"2024-01-01T00:00:00Z","updated":"2024-01-02T00:00:00Z",
		"addressBookIds":{"ab-personal":true}
	}`
	resps := call(t, srv, fmt.Sprintf(`[["ContactCard/set",{"accountId":"acct1","create":{"c1":%s}},"s"]]`, create))
	args := argsOf(t, resps[0])
	if string(args["notCreated"]) != "null" {
		t.Fatalf("notCreated = %s", args["notCreated"])
	}
	var created map[string]contacts.Card
	if err := json.Unmarshal(args["created"], &created); err != nil || created["c1"].ID == "" {
		t.Fatalf("created = %s err %v", args["created"], err)
	}
	return string(created["c1"].ID)
}

func TestCardCreateGetRoundTrip(t *testing.T) {
	srv, _ := seed(t)
	id := createCard(t, srv)

	resps := call(t, srv, fmt.Sprintf(`[["ContactCard/get",{"accountId":"acct1","ids":[%q]},"g"]]`, id))
	rawList := string(argsOf(t, resps[0])["list"])
	// JSContact JSON shape.
	for _, want := range []string{
		`"@type":"Card"`, `"version":"1.0"`, `"kind":"individual"`,
		`"name":{"full":"Alice Example","components":[{"type":"given","value":"Alice"`,
		`"emails":{"e1":{"address":"alice@example.com"`,
		`"phones":{"p1":{"number":"+1-555-0100","features":{"voice":true}`,
		`"addresses":{"a1":{"full":"1 Main St, Springfield","components":[{"type":"locality","value":"Springfield"`,
		`"countryCode":"US"`,
		`"anniversaries":{"ann1":{"type":"birth","date":{"year":1990,"month":4,"day":25}`,
		`"addressBookIds":{"ab-personal":true}`,
	} {
		if !strings.Contains(rawList, want) {
			t.Fatalf("card JSON missing %s in %s", want, rawList)
		}
	}
	var list []contacts.Card
	if err := json.Unmarshal(argsOf(t, resps[0])["list"], &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %s err %v", rawList, err)
	}
	c := list[0]
	if c.Type != "Card" || c.Name == nil || c.Name.Full != "Alice Example" || len(c.Name.Components) != 2 ||
		c.Name.Components[1].Type != "surname" || c.Emails["e1"] == nil || c.Emails["e1"].Address != "alice@example.com" ||
		c.Phones["p1"] == nil || !c.Phones["p1"].Features["voice"] ||
		c.Addresses["a1"] == nil || c.Addresses["a1"].CountryCode != "US" ||
		c.Anniversaries["ann1"] == nil || c.Anniversaries["ann1"].Date == nil || *c.Anniversaries["ann1"].Date.Year != 1990 ||
		!c.AddressBookIDs["ab-personal"] {
		t.Fatalf("card = %+v", c)
	}
}

func TestCardQueryTextEmailInAddressBook(t *testing.T) {
	srv, _ := seed(t)
	id := createCard(t, srv)
	// Second card, in the other address book, found by none of the Alice queries.
	resps := call(t, srv, `[["ContactCard/set",{"accountId":"acct1","create":{"c2":{"@type":"Card","kind":"individual","name":{"full":"Bob Other"},"emails":{"e1":{"address":"bob@other.example"}},"addressBookIds":{"ab-work":true}}}},"s"]]`)
	var created map[string]contacts.Card
	if err := json.Unmarshal(argsOf(t, resps[0])["created"], &created); err != nil || created["c2"].ID == "" {
		t.Fatalf("created bob = %s", argsOf(t, resps[0])["created"])
	}
	bobID := string(created["c2"].ID)

	// text finds by email substring (case-insensitive).
	ids := queryIDs(t, srv, "ContactCard/query", `"accountId":"acct1","filter":{"text":"ALICE@EXAMPLE"}`)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("text email ids = %v", ids)
	}
	// text also finds by name.
	ids = queryIDs(t, srv, "ContactCard/query", `"accountId":"acct1","filter":{"text":"bob oth"}`)
	if len(ids) != 1 || ids[0] != bobID {
		t.Fatalf("text name ids = %v", ids)
	}
	// email is exact: substring must not match, full address must.
	ids = queryIDs(t, srv, "ContactCard/query", `"accountId":"acct1","filter":{"email":"alice@"}`)
	if len(ids) != 0 {
		t.Fatalf("email substring ids = %v", ids)
	}
	ids = queryIDs(t, srv, "ContactCard/query", `"accountId":"acct1","filter":{"email":"Alice@Example.COM"}`)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("email exact ids = %v", ids)
	}
	// inAddressBook + kind + name.
	ids = queryIDs(t, srv, "ContactCard/query", `"accountId":"acct1","filter":{"inAddressBook":"ab-work"}`)
	if len(ids) != 1 || ids[0] != bobID {
		t.Fatalf("inAddressBook ids = %v", ids)
	}
	ids = queryIDs(t, srv, "ContactCard/query", `"accountId":"acct1","filter":{"operator":"AND","conditions":[{"inAddressBook":"ab-personal"},{"kind":"individual"},{"name":"alice"}]}`)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("AND ids = %v", ids)
	}
	// sort by name.
	ids = queryIDs(t, srv, "ContactCard/query", `"accountId":"acct1","sort":[{"property":"name"}]`)
	if len(ids) != 2 || ids[0] != id || ids[1] != bobID {
		t.Fatalf("name sort ids = %v", ids)
	}
	// Unknown condition => invalidArguments.
	resps = call(t, srv, `[["ContactCard/query",{"accountId":"acct1","filter":{"bogus":1}},"q"]]`)
	if nameOf(resps[0]) != "error" || string(argsOf(t, resps[0])["type"]) != `"invalidArguments"` {
		t.Fatalf("unknown filter resp = %s %s", resps[0][0], resps[0][1])
	}
}

func TestCardChangesViaMemory(t *testing.T) {
	srv, _ := seed(t)
	resps := call(t, srv, `[["ContactCard/get",{"accountId":"acct1","ids":null},"g"]]`)
	var state string
	if err := json.Unmarshal(argsOf(t, resps[0])["state"], &state); err != nil {
		t.Fatal(err)
	}
	id := createCard(t, srv)
	resps = call(t, srv, fmt.Sprintf(`[["ContactCard/changes",{"accountId":"acct1","sinceState":%q},"ch"]]`, state))
	args := argsOf(t, resps[0])
	var chCreated []string
	_ = json.Unmarshal(args["created"], &chCreated)
	if len(chCreated) != 1 || chCreated[0] != id {
		t.Fatalf("changes created = %s, want [%s]", args["created"], id)
	}
	if string(args["oldState"]) != fmt.Sprintf("%q", state) {
		t.Fatalf("oldState = %s", args["oldState"])
	}
	// AddressBook changes are likewise served via Memory.
	resps = call(t, srv, `[["AddressBook/get",{"accountId":"acct1","ids":null},"g"]]`)
	if err := json.Unmarshal(argsOf(t, resps[0])["state"], &state); err != nil {
		t.Fatal(err)
	}
	resps = call(t, srv, `[["AddressBook/set",{"accountId":"acct1","create":{"c9":{"name":"Temp"}}},"s"]]`)
	var created map[string]contacts.AddressBook
	if err := json.Unmarshal(argsOf(t, resps[0])["created"], &created); err != nil || created["c9"].ID == "" {
		t.Fatalf("created book = %s", argsOf(t, resps[0])["created"])
	}
	resps = call(t, srv, fmt.Sprintf(`[["AddressBook/changes",{"accountId":"acct1","sinceState":%q},"ch"]]`, state))
	_ = json.Unmarshal(argsOf(t, resps[0])["created"], &chCreated)
	if len(chCreated) != 1 || chCreated[0] != string(created["c9"].ID) {
		t.Fatalf("book changes created = %s", argsOf(t, resps[0])["created"])
	}
}
