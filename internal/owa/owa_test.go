package owa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type captured struct {
	Action string
	Auth   string
	Canary string
	Cookie string
	Body   map[string]any
}

type fake struct {
	t        *testing.T
	status   int
	respond  func(action string, body map[string]any) any
	requests []captured
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &body); err != nil {
			f.t.Errorf("decode request: %v (%s)", err, data)
		}
	}
	f.requests = append(f.requests, captured{
		Action: r.Header.Get("Action"), Auth: r.Header.Get("Authorization"),
		Canary: r.Header.Get("X-OWA-CANARY"), Cookie: r.Header.Get("Cookie"), Body: body,
	})
	if !strings.HasSuffix(r.URL.Path, "service.svc") {
		f.t.Errorf("path = %s, want .../service.svc", r.URL.Path)
	}
	if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		f.t.Errorf("Content-Type = %s", ct)
	}
	w.Header().Set("Content-Type", "application/json")
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"Body":{"MessageText":"boom"}}`))
		return
	}
	var out any = map[string]any{"Body": map[string]any{}}
	if f.respond != nil {
		out = f.respond(r.Header.Get("Action"), body)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fake) last(action string) captured {
	for i := len(f.requests) - 1; i >= 0; i-- {
		if f.requests[i].Action == action {
			return f.requests[i]
		}
	}
	f.t.Fatalf("no request with action %s (got %+v)", action, f.requests)
	return captured{}
}

func newFake(t *testing.T, respond func(string, map[string]any) any) (*fake, bridge.Provider) {
	t.Helper()
	f := &fake{t: t, respond: respond}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	fac := Factory{BaseURL: srv.URL + "/owa/"}
	p := fac.New("user@example.com", bridge.Session{
		Email: "user@example.com", OWAAuthHeader: "Bearer token123",
		OWACookies: map[string]string{"X-OWA-CANARY": "canary-should-not-win", "session": "abc"},
	})
	var _ bridge.Factory = fac
	return f, p
}

func okBody(inner map[string]any) any { return map[string]any{"Body": inner} }
func itemResp(items ...any) any {
	return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{
		map[string]any{"ResponseClass": "Success", "Items": items},
	}}})
}

func TestPingAndListFolders(t *testing.T) {
	f, p := newFake(t, func(action string, body map[string]any) any {
		if action != "FindFolder" {
			t.Errorf("action %s", action)
		}
		return okBody(map[string]any{"RootFolder": map[string]any{
			"ParentFolder": map[string]any{"FolderId": map[string]any{"Id": "root"}},
			"Folders": []any{
				map[string]any{"__type": "Folder:#Exchange", "FolderId": map[string]any{"Id": "inbox1"}, "DisplayName": "Inbox", "ParentFolderId": map[string]any{"Id": "root"}, "TotalCount": float64(3), "UnreadCount": float64(1), "DistinguishedFolderId": "inbox"},
				map[string]any{"__type": "Folder:#Exchange", "FolderId": map[string]any{"Id": "trash1"}, "DisplayName": "Deleted Items", "TotalCount": float64(0), "UnreadCount": float64(0), "DistinguishedFolderId": "deleteditems"},
			},
		}})
	})
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	folders, err := p.ListFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 || folders[0].ID != "inbox1" || folders[0].Name != "Inbox" || folders[0].ParentID != "root" || folders[0].Total != 3 || folders[0].Unread != 1 || folders[0].Special != "inbox" {
		t.Fatalf("folders %+v", folders)
	}
	if folders[1].Special != "trash" {
		t.Fatalf("trash special %+v", folders[1])
	}
	req := f.last("FindFolder")
	if req.Auth != "Bearer token123" {
		t.Fatalf("Authorization not propagated: %q", req.Auth)
	}
	if !strings.Contains(req.Cookie, "session=abc") {
		t.Fatalf("cookie header %q", req.Cookie)
	}
	// request envelope shape from the reference FindFolder call
	b, _ := json.Marshal(req.Body)
	if !strings.Contains(string(b), "FindFolderJsonRequest:#Exchange") || !strings.Contains(string(b), "Exchange2013") || !strings.Contains(string(b), "msgfolderroot") || !strings.Contains(string(b), `"Traversal":"Deep"`) {
		t.Fatalf("envelope %s", b)
	}
}

func TestCanaryFallback(t *testing.T) {
	f := &fake{t: t, respond: func(string, map[string]any) any {
		return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{}}})
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	p := Factory{BaseURL: srv.URL + "/"}.New("a@b.c", bridge.Session{OWACookies: map[string]string{"canary": "cv1"}})
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.last("FindFolder").Canary; got != "cv1" {
		t.Fatalf("canary header %q", got)
	}
	if got := f.last("FindFolder").Auth; got != "" {
		t.Fatalf("unexpected auth %q", got)
	}
}

func msgItem() map[string]any {
	return map[string]any{
		"ItemId":         map[string]any{"Id": "m1"},
		"Subject":        "Hello",
		"From":           map[string]any{"Mailbox": map[string]any{"Name": "Alice", "EmailAddress": "alice@example.com"}},
		"ToRecipients":   []any{map[string]any{"Name": "Bob", "EmailAddress": "bob@example.com"}},
		"DateTimeSent":   "2024-01-02T03:04:05Z",
		"IsRead":         false,
		"Flag":           map[string]any{"FlagStatus": "Flagged"},
		"HasAttachments": true,
		"Size":           float64(123),
	}
}

func TestListMessages(t *testing.T) {
	_, p := newFake(t, func(action string, body map[string]any) any {
		return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{msgItem()}, "TotalItemsInView": float64(7), "IndexedPagingOffset": float64(1)}})
	})
	msgs, total, err := p.ListMessages(context.Background(), "inbox1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 7 || len(msgs) != 1 {
		t.Fatalf("total=%d msgs=%+v", total, msgs)
	}
	m := msgs[0]
	if m.ID != "m1" || m.Subject != "Hello" || m.From != "alice@example.com" || len(m.To) != 1 || m.To[0] != "bob@example.com" || !m.Attachments || m.Size != 123 || m.FolderID != "inbox1" {
		t.Fatalf("meta %+v", m)
	}
	if m.Date.Format(time.RFC3339) != "2024-01-02T03:04:05Z" {
		t.Fatalf("date %v", m.Date)
	}
	foundFlagged := false
	for _, fl := range m.Flags {
		if fl == `\flagged` {
			foundFlagged = true
		}
		if fl == `\seen` {
			t.Fatalf("unread message has \\seen: %v", m.Flags)
		}
	}
	if !foundFlagged {
		t.Fatalf("flags %v", m.Flags)
	}
}

func TestGetMessageMIMEBase64(t *testing.T) {
	raw := []byte("Subject: Hi\r\n\r\nbody\r\n")
	_, p := newFake(t, func(action string, body map[string]any) any {
		if action != "GetItem" {
			t.Errorf("action %s", action)
		}
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "m1"}, "MimeContent": map[string]any{"Value": base64.StdEncoding.EncodeToString(raw)}})
	})
	got, err := p.GetMessageMIME(context.Background(), "inbox1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("mime %q", got)
	}
}

func TestSetFlagsUpdateItemBody(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
	err := p.SetFlags(context.Background(), "inbox1", []string{"m1"}, bridge.Flags{`\seen`, `\flagged`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := f.last("UpdateItem")
	b, _ := json.Marshal(req.Body)
	if !strings.Contains(string(b), "UpdateItemJsonRequest:#Exchange") || !strings.Contains(string(b), `"IsRead":true`) || !strings.Contains(string(b), `"FlagStatus":"Flagged"`) || !strings.Contains(string(b), `"Id":"m1"`) {
		t.Fatalf("body %s", b)
	}
}

func TestMoveMessages(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any { return itemResp() })
	if err := p.MoveMessages(context.Background(), "inbox1", []string{"m1"}, "archive1"); err != nil {
		t.Fatal(err)
	}
	req := f.last("MoveItem")
	b, _ := json.Marshal(req.Body)
	if !strings.Contains(string(b), "MoveItemJsonRequest:#Exchange") || !strings.Contains(string(b), `"Id":"archive1"`) || !strings.Contains(string(b), `"Id":"m1"`) {
		t.Fatalf("body %s", b)
	}
}

// mimeWithAttachment is a raw MIME fixture whose attachment part must
// survive Append/Send byte-for-byte (the old ParseMIME path dropped it).
func mimeWithAttachment() []byte {
	return []byte("From: me@example.com\r\n" +
		"To: bob@example.com\r\n" +
		"Cc: carol@example.com\r\n" +
		"Subject: Hi there\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BB\"\r\n\r\n" +
		"--BB\r\nContent-Type: text/html\r\n\r\n<p>hello</p>\r\n" +
		"--BB\r\nContent-Type: text/plain; name=\"note.txt\"\r\nContent-Disposition: attachment; filename=\"note.txt\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("attachment-bytes")) + "\r\n" +
		"--BB--\r\n")
}

// createItemMessage extracts Body.Items[0] from a captured CreateItem request.
func createItemMessage(t *testing.T, req captured) map[string]any {
	t.Helper()
	items := arr(req.Body, "Body", "Items")
	if len(items) != 1 {
		t.Fatalf("CreateItem Items = %v", items)
	}
	return asMap(items[0])
}

// assertMimeContent checks the _o MimeContent shape and that Value
// base64-decodes to exactly the input MIME bytes.
func assertMimeContent(t *testing.T, req captured, want []byte) map[string]any {
	t.Helper()
	msg := createItemMessage(t, req)
	if got := str(msg, "__type"); got != "Message:#Exchange" {
		t.Fatalf("Items[0].__type = %q", got)
	}
	mc := asMap(msg["MimeContent"])
	if mc == nil {
		t.Fatalf("no MimeContent in Items[0]: %v", msg)
	}
	if got := str(mc, "CharacterSet"); got != "UTF-8" {
		t.Fatalf("MimeContent.CharacterSet = %q", got)
	}
	raw, err := base64.StdEncoding.DecodeString(str(mc, "Value"))
	if err != nil {
		t.Fatalf("MimeContent.Value not base64: %v", err)
	}
	if string(raw) != string(want) {
		t.Fatalf("MimeContent.Value decodes to %q, want exact input MIME %q", raw, want)
	}
	if _, ok := msg["Body"]; ok {
		t.Fatalf("message rebuilt from parsed parts (Body present), attachments would be lost: %v", msg)
	}
	if _, ok := msg["ToRecipients"]; ok {
		t.Fatalf("message rebuilt from parsed parts (ToRecipients present): %v", msg)
	}
	return msg
}

func TestSendMessageCreateItem(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "new1"}})
	})
	mime := mimeWithAttachment()
	if err := p.SendMessage(context.Background(), mime); err != nil {
		t.Fatal(err)
	}
	req := f.last("CreateItem")
	assertMimeContent(t, req, mime)
	b, _ := json.Marshal(req.Body)
	for _, want := range []string{"CreateItemJsonRequest:#Exchange", `"MessageDisposition":"SendAndSaveCopy"`, "Message:#Exchange"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("body missing %q: %s", want, b)
		}
	}
}

func TestAppendMessageCreateItemMime(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "app1"}})
	})
	mime := mimeWithAttachment()
	id, err := p.AppendMessage(context.Background(), "drafts1", mime, bridge.Flags{`\seen`, `\flagged`, `\draft`})
	if err != nil {
		t.Fatal(err)
	}
	if id != "app1" {
		t.Fatalf("AppendMessage id = %q", id)
	}
	req := f.last("CreateItem")
	msg := assertMimeContent(t, req, mime)
	// Flags ride alongside MimeContent, _o CreateMime shape.
	if !boolean(msg, "IsRead") {
		t.Fatalf("IsRead not set from \\seen: %v", msg)
	}
	if got := str(msg, "Flag", "FlagStatus"); got != "Flagged" {
		t.Fatalf("Flag.FlagStatus = %q", got)
	}
	eps := arr(msg, "ExtendedProperty")
	if len(eps) != 1 || str(eps[0], "Value") != "8" { // MAPI_MSGFLAG_UNSENT for \draft
		t.Fatalf("ExtendedProperty = %v", eps)
	}
	b, _ := json.Marshal(req.Body)
	for _, want := range []string{`"MessageDisposition":"SaveOnly"`, `"Id":"drafts1"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("body missing %q: %s", want, b)
		}
	}
}

// Exchange requires Windows time zone IDs: an IANA zone from the
// client (e.g. Europe/Bucharest) must be converted, Start/End sent
// as wall-clock times in that zone, and a TimeZoneContext supplied.
// Regression: "A time zone with the specified ID could not be found."
func TestCreateCalendarEventConvertsTimeZone(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "ev1"}})
	})
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 14, 11, 44, 0, 0, loc)
	ev := bridge.Event{CalendarID: "calendar", Summary: "Tz", Start: start, End: start.Add(time.Hour), TimeZone: "Europe/Bucharest"}
	if _, err := p.CreateEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(f.last("CreateItem").Body)
	for _, want := range []string{`"Id":"E. Europe Standard Time"`, `"Start":"2026-10-14T11:44:00"`, `"End":"2026-10-14T12:44:00"`, "TimeZoneContext"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("body missing %q: %s", want, b)
		}
	}
	if strings.Contains(string(b), `"Start":"2026-10-14T08:44:00`) {
		t.Fatalf("start sent as UTC instant instead of wall time: %s", b)
	}
}

// Reading an event back must convert the Windows zone ID to IANA
// and interpret naive Start/End as wall times in that zone.
func TestEventFromItemConvertsWindowsTimeZone(t *testing.T) {
	item := map[string]any{
		"ItemId": map[string]any{"Id": "e1"}, "Subject": "Tz",
		"Start": "2026-10-14T11:44:00", "End": "2026-10-14T12:44:00",
		"StartTimeZone": map[string]any{"Id": "GTB Standard Time"},
	}
	ev := eventFromItem(item, "calendar")
	if ev.TimeZone != "Europe/Bucharest" {
		t.Fatalf("TimeZone = %q, want Europe/Bucharest", ev.TimeZone)
	}
	want := time.Date(2026, 10, 14, 8, 44, 0, 0, time.UTC) // 11:44 EEST (UTC+3)
	if !ev.Start.Equal(want) {
		t.Fatalf("Start = %v, want instant %v", ev.Start, want)
	}
}

func TestCreateCalendarEvent(t *testing.T) {
	f, p := newFake(t, func(string, map[string]any) any {
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "ev1"}, "UID": "uid-1"})
	})
	start := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	ev, err := p.CreateEvent(context.Background(), bridge.Event{CalendarID: "calendar", Summary: "Meet", Start: start, End: start.Add(time.Hour), Location: "Room 1", Attendees: []bridge.Attendee{{Email: "bob@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "ev1" || ev.UID != "uid-1" {
		t.Fatalf("event %+v", ev)
	}
	b, _ := json.Marshal(f.last("CreateItem").Body)
	for _, want := range []string{"CalendarItem:#Exchange", "Meet", "Room 1", "bob@example.com"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("body missing %q: %s", want, b)
		}
	}
}

func TestListTasksMapping(t *testing.T) {
	_, p := newFake(t, func(action string, body map[string]any) any {
		if action == "FindItem" {
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{
				map[string]any{"ItemId": map[string]any{"Id": "t1"}},
			}, "TotalItemsInView": float64(1)}})
		}
		return itemResp(map[string]any{"ItemId": map[string]any{"Id": "t1"}, "Subject": "Do it", "DueDate": "2024-06-01T00:00:00Z", "PercentComplete": float64(50), "Importance": "High"})
	})
	tasks, err := p.ListTasks(context.Background(), "tasks")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t1" || tasks[0].Title != "Do it" || tasks[0].Priority != 1 || tasks[0].Status != "in-process" || tasks[0].Due == nil {
		t.Fatalf("tasks %+v", tasks)
	}
}

func TestCreateContact(t *testing.T) {
	f, p := newFake(t, func(action string, body map[string]any) any {
		// reference CreatePersona shape
		return okBody(map[string]any{"PersonaId": map[string]any{"Id": "p1"}})
	})
	c, err := p.CreateContact(context.Background(), bridge.Contact{FullName: "Bob Smith", FirstName: "Bob", LastName: "Smith", Emails: []string{"bob@example.com"}, Org: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "p1" {
		t.Fatalf("contact %+v", c)
	}
	req := f.requests[len(f.requests)-1]
	if req.Action != "CreatePersona" && req.Action != "CreateItem" {
		t.Fatalf("unexpected contact action %q", req.Action)
	}
	b, _ := json.Marshal(req.Body)
	if !strings.Contains(string(b), "Bob") || !strings.Contains(string(b), "bob@example.com") {
		t.Fatalf("body %s", b)
	}
}

func TestHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		check  func(error) bool
		name   string
	}{{440, bridge.IsAuth, "440 auth"}, {401, bridge.IsAuth, "401 auth"}, {500, bridge.IsTransient, "500 transient"}, {429, bridge.IsTransient, "429 transient"}} {
		f := &fake{t: t, status: tc.status}
		srv := httptest.NewServer(f)
		p := Factory{BaseURL: srv.URL + "/"}.New("a@b.c", bridge.Session{})
		err := p.Ping(context.Background())
		srv.Close()
		if err == nil || !tc.check(err) {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
	}
}
