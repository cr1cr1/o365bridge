package graph_test

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
	"github.com/cr1cr1/o365bridge/internal/graph"
)

type recorded struct {
	Method      string
	Path        string
	Query       string
	Auth        string
	Prefer      string
	ContentType string
	Body        []byte
}

type fake struct {
	srv  *httptest.Server
	reqs []recorded
	h    func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) *fake {
	t.Helper()
	f := &fake{h: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Prefer"), r.Header.Get("Content-Type"), b})
		f.h(w, r, b)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) provider() bridge.Provider {
	var fac any = &graph.Factory{BaseURL: f.srv.URL}
	factory := fac.(bridge.Factory) // Factory must satisfy bridge.Factory
	return factory.New("user@example.com", bridge.Session{AccessToken: "tok123"})
}

func jsonOut(w http.ResponseWriter, status int, v string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, v)
}

func (f *fake) last() recorded { return f.reqs[len(f.reqs)-1] }

func (f *fake) find(method, pathSuffix string) (recorded, bool) {
	for _, r := range f.reqs {
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			return r, true
		}
	}
	return recorded{}, false
}

func decodeB64MIME(t *testing.T, r recorded, want []byte) {
	t.Helper()
	if r.ContentType != "text/plain" {
		t.Errorf("%s %s Content-Type=%q want text/plain", r.Method, r.Path, r.ContentType)
	}
	dec, err := base64.StdEncoding.DecodeString(string(r.Body))
	if err != nil {
		t.Fatalf("body not base64: %v body=%q", err, r.Body)
	}
	if string(dec) != string(want) {
		t.Errorf("decoded MIME mismatch:\ngot  %q\nwant %q", dec, want)
	}
	if !strings.Contains(string(dec), "attachment") || !strings.Contains(string(dec), "aGVsbG8gYXR0YWNobWVudA==") {
		t.Errorf("attachment part lost in decoded MIME: %q", dec)
	}
}

const attachmentMIME = "From: me@example.com\r\nTo: bob@example.com\r\nSubject: With attachment\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"BB\"\r\n\r\n--BB\r\nContent-Type: text/plain\r\n\r\nPlain body\r\n--BB\r\nContent-Type: application/octet-stream; name=\"a.txt\"\r\nContent-Disposition: attachment; filename=\"a.txt\"\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8gYXR0YWNobWVudA==\r\n--BB--\r\n"

func bodyJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body not JSON: %v %s", err, b)
	}
	return m
}

func TestFactorySatisfiesBridgeFactory(t *testing.T) {
	var _ bridge.Factory = &graph.Factory{}
	var _ bridge.Factory = graph.Factory{}
}

func TestFoldersListMappingSpecialAndAuthHeader(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOut(w, 200, `{"value":[
		 {"id":"inbox","displayName":"Inbox","totalItemCount":10,"unreadItemCount":3},
		 {"id":"AAA=","displayName":"Sent Items","totalItemCount":5,"unreadItemCount":0},
		 {"id":"deleteditems","displayName":"Deleted Items","totalItemCount":2,"unreadItemCount":1},
		 {"id":"junkemail","displayName":"Junk Email","totalItemCount":1,"unreadItemCount":0},
		 {"id":"drafts","displayName":"Drafts","totalItemCount":4,"unreadItemCount":0},
		 {"id":"archive","displayName":"Archive","totalItemCount":7,"unreadItemCount":0},
		 {"id":"xyz","displayName":"Projects","parentFolderId":"inbox","totalItemCount":1,"unreadItemCount":0}
		]}`)
	})
	folders, err := f.provider().ListFolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 7 {
		t.Fatalf("folders=%+v", folders)
	}
	wantSpecial := []string{"inbox", "sent", "trash", "junk", "drafts", "archive", ""}
	for i, ws := range wantSpecial {
		if folders[i].Special != ws {
			t.Errorf("folder %d special=%q want %q (%+v)", i, folders[i].Special, ws, folders[i])
		}
	}
	if folders[0].Total != 10 || folders[0].Unread != 3 || folders[0].Name != "Inbox" {
		t.Errorf("counts %+v", folders[0])
	}
	if folders[6].ParentID != "inbox" {
		t.Errorf("parent %+v", folders[6])
	}
	req := f.last()
	if req.Method != "GET" || req.Path != "/v1.0/me/mailFolders" {
		t.Errorf("req %+v", req)
	}
	if req.Auth != "Bearer tok123" {
		t.Errorf("auth %q", req.Auth)
	}
	if !strings.Contains(req.Prefer, `IdType="ImmutableId"`) || !strings.Contains(req.Prefer, "odata.maxpagesize") {
		t.Errorf("prefer %q", req.Prefer)
	}
}

func TestFolderCreateChildRenameDelete(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == "POST" {
			jsonOut(w, 201, `{"id":"new1","displayName":"Sub","parentFolderId":"parent1"}`)
			return
		}
		w.WriteHeader(204)
	})
	p := f.provider()
	ctx := context.Background()
	folder, err := p.CreateFolder(ctx, "Sub", "parent1")
	if err != nil {
		t.Fatal(err)
	}
	if folder.ID != "new1" || folder.Name != "Sub" {
		t.Errorf("folder %+v", folder)
	}
	if f.last().Path != "/v1.0/me/mailFolders/parent1/childFolders" {
		t.Errorf("path %s", f.last().Path)
	}
	if got := bodyJSON(t, f.last().Body)["displayName"]; got != "Sub" {
		t.Errorf("body %v", got)
	}

	if _, err := p.CreateFolder(ctx, "Top", ""); err != nil {
		t.Fatal(err)
	}
	if f.last().Path != "/v1.0/me/mailFolders" {
		t.Errorf("path %s", f.last().Path)
	}

	if err := p.RenameFolder(ctx, "new1", "Renamed"); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "PATCH" || bodyJSON(t, f.last().Body)["displayName"] != "Renamed" {
		t.Errorf("rename %+v", f.last())
	}

	if err := p.DeleteFolder(ctx, "new1"); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "DELETE" || f.last().Path != "/v1.0/me/mailFolders/new1" {
		t.Errorf("delete %+v", f.last())
	}
}

func TestPingAndEmptyTokenAuth(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { jsonOut(w, 200, `{"id":"inbox"}`) })
	if err := f.provider().Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.last().Path != "/v1.0/me/mailFolders/inbox" {
		t.Errorf("ping path %s", f.last().Path)
	}

	bad := (&graph.Factory{BaseURL: f.srv.URL}).New("u@e", bridge.Session{})
	if err := bad.Ping(context.Background()); !bridge.IsAuth(err) {
		t.Errorf("empty token err=%v want auth", err)
	}
}

func TestMessagesListGetAndRaw(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "Subject: Hi\r\n\r\nbody")
		case strings.Contains(r.URL.Path, "/messages/msg1"):
			jsonOut(w, 200, `{"id":"msg1","subject":"One","from":{"emailAddress":{"address":"a@b.c","name":"A"}},"toRecipients":[{"emailAddress":{"address":"u@e"}}],"receivedDateTime":"2026-01-02T03:04:05Z","isRead":false,"flag":{"flagStatus":"flagged"},"hasAttachments":true}`)
		default:
			jsonOut(w, 200, `{"@odata.count":2,"value":[
			  {"id":"msg1","subject":"One","from":{"emailAddress":{"address":"a@b.c"}},"toRecipients":[{"emailAddress":{"address":"u@e"}}],"receivedDateTime":"2026-01-02T03:04:05Z","isRead":true,"flag":{"flagStatus":"flagged"},"hasAttachments":false},
			  {"id":"msg2","subject":"Two","from":{"emailAddress":{"address":"d@e.f"}},"receivedDateTime":"2026-01-03T03:04:05Z","isRead":false,"flag":{"flagStatus":"notFlagged"},"hasAttachments":true}
			]}`)
		}
	})
	p := f.provider()
	ctx := context.Background()
	msgs, total, err := p.ListMessages(ctx, "inbox", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(msgs) != 2 {
		t.Fatalf("msgs=%v total=%d", msgs, total)
	}
	if msgs[0].FolderID != "inbox" || msgs[0].Subject != "One" || msgs[0].From != "a@b.c" || len(msgs[0].To) != 1 || msgs[0].To[0] != "u@e" {
		t.Errorf("meta %+v", msgs[0])
	}
	flags := strings.Join(msgs[0].Flags, ",")
	if !strings.Contains(flags, `\seen`) || !strings.Contains(flags, `\flagged`) {
		t.Errorf("flags %v", msgs[0].Flags)
	}
	if strings.Contains(strings.Join(msgs[1].Flags, ","), `\seen`) {
		t.Errorf("msg2 flags %v", msgs[1].Flags)
	}
	if msgs[0].Date.IsZero() {
		t.Errorf("date zero")
	}
	q := f.last().Query
	if !strings.Contains(q, "%24top=10") && !strings.Contains(q, "$top=10") {
		t.Errorf("query %s", q)
	}

	meta, err := p.GetMessage(ctx, "inbox", "msg1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Subject != "One" || !meta.Attachments || meta.FolderID != "inbox" {
		t.Errorf("get %+v", meta)
	}

	raw, err := p.GetMessageMIME(ctx, "inbox", "msg1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Subject: Hi") {
		t.Errorf("raw %s", raw)
	}
	if !strings.HasSuffix(f.last().Path, "/msg1/$value") {
		t.Errorf("value path %s", f.last().Path)
	}
}

func TestPaginationFollowed(t *testing.T) {
	var f *fake
	f = newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.Contains(r.URL.RawQuery, "skiptok") || strings.Contains(r.URL.RawQuery, "page2") {
			jsonOut(w, 200, `{"value":[{"id":"m2","subject":"Two","receivedDateTime":"2026-01-02T00:00:00Z"}]}`)
			return
		}
		next := f.srv.URL + "/v1.0/me/mailFolders/inbox/messages?page2=1"
		jsonOut(w, 200, `{"@odata.count":2,"value":[{"id":"m1","subject":"One","receivedDateTime":"2026-01-01T00:00:00Z"}],"@odata.nextLink":"`+next+`"}`)
	})
	msgs, total, err := f.provider().ListMessages(context.Background(), "inbox", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1].ID != "m2" || total != 2 {
		t.Fatalf("msgs=%v total=%d reqs=%d", msgs, total, len(f.reqs))
	}
	if len(f.reqs) != 2 {
		t.Errorf("expected 2 page requests, got %d", len(f.reqs))
	}
}

func TestSetFlagsMoveCopyDeleteSearch(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == "GET" {
			jsonOut(w, 200, `{"value":[{"id":"m1"},{"id":"m2"}]}`)
			return
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		jsonOut(w, 200, `{"id":"m1"}`)
	})
	p := f.provider()
	ctx := context.Background()

	if err := p.SetFlags(ctx, "inbox", []string{"m1"}, bridge.Flags{`\seen`, `\flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "PATCH" {
		t.Errorf("flags method %s", f.last().Method)
	}
	b := bodyJSON(t, f.last().Body)
	if b["isRead"] != true {
		t.Errorf("isRead %v", b)
	}
	if flag, _ := b["flag"].(map[string]any); flag["flagStatus"] != "flagged" {
		t.Errorf("flag %v", b)
	}

	if err := p.SetFlags(ctx, "inbox", []string{"m1"}, nil, bridge.Flags{`\seen`, `\flagged`}); err != nil {
		t.Fatal(err)
	}
	b = bodyJSON(t, f.last().Body)
	if b["isRead"] != false {
		t.Errorf("isRead remove %v", b)
	}
	if flag, _ := b["flag"].(map[string]any); flag["flagStatus"] != "notFlagged" {
		t.Errorf("flag remove %v", b)
	}

	if err := p.MoveMessages(ctx, "inbox", []string{"m1"}, "archive"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.last().Path, "/m1/move") || bodyJSON(t, f.last().Body)["destinationId"] != "archive" {
		t.Errorf("move %+v body %s", f.last(), f.last().Body)
	}
	if err := p.CopyMessages(ctx, "inbox", []string{"m1"}, "archive"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.last().Path, "/m1/copy") {
		t.Errorf("copy %s", f.last().Path)
	}

	if err := p.DeleteMessages(ctx, "inbox", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "DELETE" || !strings.HasSuffix(f.last().Path, "/messages/m1") {
		t.Errorf("delete %+v", f.last())
	}

	ids, err := p.SearchMessages(ctx, "inbox", bridge.SearchCriteria{Unseen: true, Flagged: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "m1" {
		t.Errorf("search ids %v query %s", ids, f.last().Query)
	}
	if q := f.last().Query; !strings.Contains(q, "isRead") && !strings.Contains(q, "%24filter") {
		t.Errorf("search should filter unseen: %s", q)
	}

	ids, err = p.SearchMessages(ctx, "inbox", bridge.SearchCriteria{Text: "hello", From: "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("search2 %v", ids)
	}
	if q := f.last().Query; !strings.Contains(q, "hello") {
		t.Errorf("search text query %s", q)
	}
}

func TestSendMessageRawMIME(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.WriteHeader(202)
	})
	if err := f.provider().SendMessage(context.Background(), []byte(attachmentMIME)); err != nil {
		t.Fatal(err)
	}
	req, ok := f.find("POST", "/v1.0/me/sendMail")
	if !ok {
		t.Fatalf("no sendMail request: %+v", f.reqs)
	}
	decodeB64MIME(t, req, []byte(attachmentMIME))
}

// appendFake serves folder lookup, draft creation from MIME, move and PATCH.
func appendFake(t *testing.T) *fake {
	t.Helper()
	return newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1.0/me/mailFolders/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1.0/me/mailFolders/")
			name := "Sent Items"
			if id == "draftsFolder" {
				name = "Drafts"
			}
			jsonOut(w, 200, `{"id":"`+id+`","displayName":"`+name+`"}`)
		case r.Method == "POST" && r.URL.Path == "/v1.0/me/messages":
			jsonOut(w, 201, `{"id":"draft1"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/move"):
			jsonOut(w, 200, `{"id":"draft1"}`)
		default:
			jsonOut(w, 200, `{"id":"draft1"}`)
		}
	})
}

func TestAppendMessageRawMIMEToDraftsNoMove(t *testing.T) {
	f := appendFake(t)
	id, err := f.provider().AppendMessage(context.Background(), "draftsFolder", []byte(attachmentMIME), bridge.Flags{`\seen`})
	if err != nil {
		t.Fatal(err)
	}
	if id != "draft1" {
		t.Errorf("append id %q", id)
	}
	create, ok := f.find("POST", "/v1.0/me/messages")
	if !ok {
		t.Fatalf("no create-draft request: %+v", f.reqs)
	}
	decodeB64MIME(t, create, []byte(attachmentMIME))
	if _, moved := f.find("POST", "/draft1/move"); moved {
		t.Errorf("append to drafts must not move: %+v", f.reqs)
	}
	// Flags cannot be expressed in the MIME-create call; they are applied after.
	patch, ok := f.find("PATCH", "/v1.0/me/messages/draft1")
	if !ok || bodyJSON(t, patch.Body)["isRead"] != true {
		t.Errorf("expected PATCH isRead=true after append, reqs=%+v", f.reqs)
	}
}

func TestAppendMessageRawMIMEToNonDraftsMoves(t *testing.T) {
	f := appendFake(t)
	id, err := f.provider().AppendMessage(context.Background(), "sentFolder", []byte(attachmentMIME), nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "draft1" {
		t.Errorf("append id %q", id)
	}
	create, ok := f.find("POST", "/v1.0/me/messages")
	if !ok {
		t.Fatalf("no create-draft request: %+v", f.reqs)
	}
	decodeB64MIME(t, create, []byte(attachmentMIME))
	move, ok := f.find("POST", "/v1.0/me/messages/draft1/move")
	if !ok {
		t.Fatalf("append to non-drafts must move draft: %+v", f.reqs)
	}
	if bodyJSON(t, move.Body)["destinationId"] != "sentFolder" {
		t.Errorf("move body %s", move.Body)
	}
}

func TestHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status          int
		auth, transient bool
	}{{401, true, false}, {403, true, false}, {503, false, true}, {429, false, true}} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			jsonOut(w, tc.status, `{"error":{"code":"ErrorAccessDenied","message":"nope"}}`)
		})
		err := f.provider().Ping(context.Background())
		if err == nil {
			t.Fatalf("status %d: nil err", tc.status)
		}
		if bridge.IsAuth(err) != tc.auth || bridge.IsTransient(err) != tc.transient {
			t.Errorf("status %d auth=%v transient=%v err=%v", tc.status, bridge.IsAuth(err), bridge.IsTransient(err), err)
		}
		if !strings.Contains(err.Error(), "ErrorAccessDenied") {
			t.Errorf("error should include code: %v", err)
		}
	}
}

func TestEventsCreateListRespondFreeBusy(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch {
		case strings.Contains(r.URL.Path, "getSchedule"):
			jsonOut(w, 200, `{"value":[{"scheduleId":"bob@example.com","scheduleItems":[{"status":"busy","start":{"dateTime":"2026-02-01T10:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-02-01T11:00:00","timeZone":"UTC"}}]}]}`)
		case strings.Contains(r.URL.Path, "/accept") || strings.Contains(r.URL.Path, "/decline") || strings.Contains(r.URL.Path, "tentativelyAccept"):
			w.WriteHeader(202)
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/events"):
			var in map[string]any
			json.Unmarshal(body, &in)
			jsonOut(w, 201, `{"id":"ev1","iCalUId":"uid1","subject":"`+in["subject"].(string)+`","start":{"dateTime":"2026-02-01T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-02-01T10:00:00","timeZone":"UTC"}}`)
		default:
			jsonOut(w, 200, `{"value":[{"id":"cal1","name":"Calendar"}]}`)
		}
	})
	p := f.provider()
	ctx := context.Background()
	cals, err := p.ListCalendars(ctx)
	if err != nil || len(cals) != 1 || cals[0].Name != "Calendar" {
		t.Fatalf("cals %v %v", cals, err)
	}

	start := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	rem := 15
	ev, err := p.CreateEvent(ctx, bridge.Event{CalendarID: "cal1", Summary: "Standup", Location: "Room 1", Start: start, End: start.Add(time.Hour), TimeZone: "UTC", ReminderMin: &rem, Attendees: []bridge.Attendee{{Email: "bob@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "ev1" || ev.UID != "uid1" {
		t.Errorf("created %+v", ev)
	}
	if f.last().Path != "/v1.0/me/calendars/cal1/events" {
		t.Errorf("create path %s", f.last().Path)
	}
	b := bodyJSON(t, f.last().Body)
	if b["subject"] != "Standup" {
		t.Errorf("subject %v", b)
	}
	if s, _ := b["start"].(map[string]any); s["timeZone"] != "UTC" || !strings.HasPrefix(s["dateTime"].(string), "2026-02-01T09:00:00") {
		t.Errorf("start %v", s)
	}
	if b["reminderMinutesBeforeStart"] != float64(15) {
		t.Errorf("reminder %v", b)
	}
	if loc, _ := b["location"].(map[string]any); loc["displayName"] != "Room 1" {
		t.Errorf("location %v", b)
	}

	if err := p.RespondEvent(ctx, "cal1", "ev1", "accepted"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.last().Path, "/events/ev1/accept") {
		t.Errorf("respond path %s", f.last().Path)
	}
	if bodyJSON(t, f.last().Body)["sendResponse"] != true {
		t.Errorf("respond body %s", f.last().Body)
	}
	if err := p.RespondEvent(ctx, "cal1", "ev1", "declined"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.last().Path, "/events/ev1/decline") {
		t.Errorf("decline path %s", f.last().Path)
	}
	if err := p.RespondEvent(ctx, "cal1", "ev1", "tentative"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.last().Path, "/events/ev1/tentativelyAccept") {
		t.Errorf("tentative path %s", f.last().Path)
	}

	fb, err := p.FreeBusy(ctx, []string{"bob@example.com"}, start, start.Add(8*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ranges := fb["bob@example.com"]
	if len(ranges) != 1 || !ranges[0][0].Equal(time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("freebusy %v req %s", fb, f.last().Body)
	}
	if !strings.Contains(f.last().Path, "getSchedule") {
		t.Errorf("fb path %s", f.last().Path)
	}

	// Update / delete / list events smoke
	if err := p.UpdateEvent(ctx, bridge.Event{ID: "ev1", CalendarID: "cal1", Summary: "New", Start: start, End: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "PATCH" || !strings.HasSuffix(f.last().Path, "/events/ev1") {
		t.Errorf("update %+v", f.last())
	}
	if err := p.DeleteEvent(ctx, "cal1", "ev1"); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "DELETE" {
		t.Errorf("delete event %+v", f.last())
	}
}

func TestListEventsMapping(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOut(w, 200, `{"value":[{"id":"ev1","iCalUId":"uid1","subject":"Meet","body":{"content":"notes","contentType":"text"},"location":{"displayName":"Office"},"isAllDay":false,"start":{"dateTime":"2026-02-01T09:00:00.0000000","timeZone":"UTC"},"end":{"dateTime":"2026-02-01T10:00:00.0000000","timeZone":"UTC"},"organizer":{"emailAddress":{"address":"org@e"}},"attendees":[{"type":"required","status":{"response":"accepted"},"emailAddress":{"address":"bob@e","name":"Bob"}}]}]}`)
	})
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	evs, err := f.provider().ListEvents(context.Background(), "cal1", from, from.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Summary != "Meet" || evs[0].CalendarID != "cal1" || evs[0].Organizer != "org@e" {
		t.Fatalf("evs %+v path %s", evs, f.last().Path)
	}
	if len(evs[0].Attendees) != 1 || evs[0].Attendees[0].Status != "accepted" {
		t.Errorf("attendees %+v", evs[0].Attendees)
	}
	if !strings.Contains(f.last().Query, "startDateTime") && !strings.Contains(f.last().Query, "%24filter") {
		t.Errorf("time range query %s path %s", f.last().Query, f.last().Path)
	}
}

func TestTasksCRUDMapping(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch r.Method {
		case "GET":
			if strings.HasSuffix(r.URL.Path, "/lists") {
				jsonOut(w, 200, `{"value":[{"id":"list1","displayName":"Tasks"}]}`)
				return
			}
			jsonOut(w, 200, `{"value":[{"id":"t1","title":"Do it","status":"inProgress","importance":"high","dueDateTime":{"dateTime":"2026-03-01T12:00:00","timeZone":"UTC"}}]}`)
		case "POST":
			var in map[string]any
			json.Unmarshal(body, &in)
			jsonOut(w, 201, `{"id":"t9","title":"`+in["title"].(string)+`","status":"notStarted","importance":"high"}`)
		default:
			w.WriteHeader(204)
		}
	})
	p := f.provider()
	ctx := context.Background()
	lists, err := p.ListTaskLists(ctx)
	if err != nil || len(lists) != 1 || !lists[0].TasksOnly || lists[0].Name != "Tasks" {
		t.Fatalf("lists %v %v", lists, err)
	}

	tasks, err := p.ListTasks(ctx, "list1")
	if err != nil || len(tasks) != 1 || tasks[0].Status != "in-process" || tasks[0].Priority != 1 || tasks[0].Due == nil {
		t.Fatalf("tasks %+v %v", tasks, err)
	}

	due := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	created, err := p.CreateTask(ctx, bridge.Task{ListID: "list1", Title: "Write tests", Body: "details", Due: &due, Priority: 1, Status: "needs-action"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "t9" {
		t.Errorf("created %+v", created)
	}
	if f.last().Path != "/v1.0/me/todo/lists/list1/tasks" {
		t.Errorf("create path %s", f.last().Path)
	}
	b := bodyJSON(t, f.last().Body)
	if b["title"] != "Write tests" || b["importance"] != "high" || b["status"] != "notStarted" {
		t.Errorf("task body %v", b)
	}
	if d, _ := b["dueDateTime"].(map[string]any); d["timeZone"] != "UTC" {
		t.Errorf("due %v", b)
	}
	if bd, _ := b["body"].(map[string]any); bd["content"] != "details" {
		t.Errorf("task body field %v", b)
	}

	if err := p.UpdateTask(ctx, bridge.Task{ID: "t9", ListID: "list1", Title: "Done", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "PATCH" || bodyJSON(t, f.last().Body)["status"] != "completed" {
		t.Errorf("update task %+v", f.last())
	}
	if err := p.DeleteTask(ctx, "list1", "t9"); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "DELETE" || f.last().Path != "/v1.0/me/todo/lists/list1/tasks/t9" {
		t.Errorf("delete task %+v", f.last())
	}
}

func TestContactsCRUDMapping(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch r.Method {
		case "GET":
			jsonOut(w, 200, `{"value":[{"id":"c1","displayName":"Bob Smith","givenName":"Bob","surname":"Smith","emailAddresses":[{"address":"bob@e"}],"mobilePhone":"123","companyName":"Acme","personalNotes":"note"}]}`)
		case "POST":
			jsonOut(w, 201, `{"id":"c9","displayName":"Bob Smith","givenName":"Bob","surname":"Smith"}`)
		default:
			w.WriteHeader(204)
		}
	})
	p := f.provider()
	ctx := context.Background()
	contacts, err := p.ListContacts(ctx, "")
	if err != nil || len(contacts) != 1 {
		t.Fatalf("contacts %v %v", contacts, err)
	}
	c := contacts[0]
	if c.FullName != "Bob Smith" || c.FirstName != "Bob" || c.LastName != "Smith" || len(c.Emails) != 1 || c.Emails[0] != "bob@e" || len(c.Phones) != 1 || c.Org != "Acme" || c.Notes != "note" {
		t.Errorf("contact %+v", c)
	}

	created, err := p.CreateContact(ctx, bridge.Contact{FullName: "Bob Smith", FirstName: "Bob", LastName: "Smith", Emails: []string{"bob@e"}, Phones: []string{"123"}, Org: "Acme", Notes: "note"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "c9" {
		t.Errorf("created %+v", created)
	}
	b := bodyJSON(t, f.last().Body)
	if b["givenName"] != "Bob" || b["surname"] != "Smith" || b["companyName"] != "Acme" {
		t.Errorf("contact body %v", b)
	}
	eas, _ := b["emailAddresses"].([]any)
	if len(eas) != 1 || eas[0].(map[string]any)["address"] != "bob@e" {
		t.Errorf("emails %v", b)
	}

	if err := p.UpdateContact(ctx, bridge.Contact{ID: "c9", FullName: "Bob Smith"}); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "PATCH" || !strings.HasSuffix(f.last().Path, "/contacts/c9") {
		t.Errorf("update %+v", f.last())
	}
	if err := p.DeleteContact(ctx, "", "c9"); err != nil {
		t.Fatal(err)
	}
	if f.last().Method != "DELETE" {
		t.Errorf("delete %+v", f.last())
	}
}
