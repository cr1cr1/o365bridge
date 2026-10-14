package calendars_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/calendars"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, _ *http.Request) (*server.Principal, error) {
	return &server.Principal{
		Username:        "user@example.com",
		Accounts:        map[jmap.ID]*jmap.Account{"acct1": {Name: "User", IsPersonal: true}},
		PrimaryAccounts: map[jmap.URI]jmap.ID{jmap.CoreURI: "acct1", calendars.Capability: "acct1"},
	}, nil
}

type notifStore struct {
	list []calendars.CalendarEventNotification
}

func (s *notifStore) Get(_ context.Context, _ jmap.ID, ids []jmap.ID) ([]calendars.CalendarEventNotification, []jmap.ID, string, error) {
	if ids == nil {
		return s.list, nil, "1", nil
	}
	var found []calendars.CalendarEventNotification
	var notFound []jmap.ID
	for _, id := range ids {
		ok := false
		for _, n := range s.list {
			if n.ID == id {
				found = append(found, n)
				ok = true
			}
		}
		if !ok {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, "1", nil
}

func (s *notifStore) Changes(_ context.Context, _ jmap.ID, since string, _ int) ([]jmap.ID, []jmap.ID, []jmap.ID, string, bool, error) {
	if since == "0" {
		ids := make([]jmap.ID, 0, len(s.list))
		for _, n := range s.list {
			ids = append(ids, n.ID)
		}
		return ids, nil, nil, "1", false, nil
	}
	if since == "1" {
		return nil, nil, nil, "1", false, nil
	}
	return nil, nil, nil, "", false, datatype.ErrCannotCalculate
}

type backend struct {
	cals       *datatype.Memory[calendars.Calendar]
	events     *datatype.Memory[calendars.CalendarEvent]
	identities *datatype.Memory[calendars.ParticipantIdentity]
	notifs     *notifStore
}

func (b *backend) Calendars() datatype.Store[calendars.Calendar] { return b.cals }
func (b *backend) Events() datatype.Store[calendars.CalendarEvent] {
	return b.events
}
func (b *backend) ParticipantIdentities() datatype.Store[calendars.ParticipantIdentity] {
	return b.identities
}
func (b *backend) Notifications() calendars.NotificationStore { return b.notifs }

func (b *backend) Availability(_ context.Context, _ jmap.ID, attendees []string, _, _ string) (map[string][]calendars.BusyPeriod, error) {
	out := map[string][]calendars.BusyPeriod{}
	for _, a := range attendees {
		out[a] = []calendars.BusyPeriod{{BusyType: "busy", Start: "2026-01-05T10:00:00Z", End: "2026-01-05T11:00:00Z"}}
	}
	return out, nil
}

func strptr(s string) *string { return &s }

func seed(t *testing.T) (*httptest.Server, *backend) {
	t.Helper()
	evName := "Bob"
	b := &backend{
		cals: datatype.NewMemory[calendars.Calendar](
			func(c calendars.Calendar) jmap.ID { return c.ID },
			func(c *calendars.Calendar, id jmap.ID) { c.ID = id }),
		events: datatype.NewMemory[calendars.CalendarEvent](
			func(e calendars.CalendarEvent) jmap.ID { return e.ID },
			func(e *calendars.CalendarEvent, id jmap.ID) { e.ID = id }),
		identities: datatype.NewMemory[calendars.ParticipantIdentity](
			func(p calendars.ParticipantIdentity) jmap.ID { return p.ID },
			func(p *calendars.ParticipantIdentity, id jmap.ID) { p.ID = id }),
		notifs: &notifStore{list: []calendars.CalendarEventNotification{
			{ID: "n1", Created: "2026-01-01T00:00:00Z", ChangedBy: &calendars.ChangedBy{Name: &evName, Email: strptr("bob@example.com")}, EventID: "1", Type: "created"},
		}},
	}
	e := server.New(fakeAuth{}, server.Options{})
	calendars.Register(e, b)
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(srv.Close)
	return srv, b
}

func call(t *testing.T, srv *httptest.Server, methodCalls string) [][3]json.RawMessage {
	t.Helper()
	body := fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core",%q],"methodCalls":%s}`, string(calendars.Capability), methodCalls)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/jmap/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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

func TestCalendarSetGetQuery(t *testing.T) {
	srv, _ := seed(t)
	resps := call(t, srv, `[["Calendar/set",{"accountId":"acct1","create":{"c1":{"name":"Work","isDefault":true,"isSubscribed":true,"shareWith":null,"myRights":{"mayReadItems":true}},"c2":{"name":"Personal","isSubscribed":true}}},"s1"]]`)
	args := argsOf(t, resps[0])
	var created map[string]calendars.Calendar
	if err := json.Unmarshal(args["created"], &created); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created["c1"].ID != "1" || !created["c1"].IsDefault {
		t.Fatalf("created=%s", args["created"])
	}
	resps = call(t, srv, `[["Calendar/get",{"accountId":"acct1","ids":["1"]},"g1"]]`)
	var list []calendars.Calendar
	if err := json.Unmarshal(argsOf(t, resps[0])["list"], &list); err != nil || len(list) != 1 || list[0].Name != "Work" || list[0].ShareWith != nil {
		t.Fatalf("get=%s err=%v", argsOf(t, resps[0])["list"], err)
	}
	// query: name substring ci + isDefault, sort by name
	resps = call(t, srv, `[["Calendar/query",{"accountId":"acct1","filter":{"name":"wor","isDefault":true},"sort":[{"property":"name"}]},"q1"]]`)
	var ids []string
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil || len(ids) != 1 || ids[0] != "1" {
		t.Fatalf("query ids=%s", argsOf(t, resps[0])["ids"])
	}
	// operator filter
	resps = call(t, srv, `[["Calendar/query",{"accountId":"acct1","filter":{"operator":"AND","conditions":[{"isSubscribed":true}]}},"q2"]]`)
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil || len(ids) != 2 {
		t.Fatalf("operator query ids=%s", argsOf(t, resps[0])["ids"])
	}
}

func TestCalendarEventRoundTripAndQuery(t *testing.T) {
	srv, _ := seed(t)
	call(t, srv, `[["Calendar/set",{"accountId":"acct1","create":{"c1":{"name":"Work"}}},"s0"]]`)
	create := `[["CalendarEvent/set",{"accountId":"acct1","create":{"e1":{
		"calendarIds":{"1":true},"uid":"uid-1","title":"Standup","description":"daily sync",
		"start":"2026-01-05T09:30:00","timeZone":"Europe/London","duration":"PT30M","allDay":false,
		"status":"confirmed","freeBusyStatus":"busy",
		"participants":{"p1":{"name":"Bob","email":"bob@example.com","kind":"individual","roles":{"attendee":true},"participationStatus":"accepted","expectReply":true,"language":null}},
		"recurrenceRule":{"frequency":"weekly","interval":1,"byDay":[{"day":"mo"}]},
		"alerts":{"a1":{"@type":"OffsetTrigger","offset":"-PT15M","relativeTo":"start","action":"display"}},
		"locations":{"l1":{"name":"Room 1","description":null,"coordinates":null}},
		"useDefaultAlerts":false
	},"e2":{
		"calendarIds":{"1":true},"uid":"uid-2","title":"Early","start":"2026-01-01T08:00:00","timeZone":null,"duration":"PT1H","status":"confirmed","freeBusyStatus":"busy"
	}}},"s1"]]`
	resps := call(t, srv, create)
	args := argsOf(t, resps[0])
	if string(args["notCreated"]) != "null" {
		t.Fatalf("notCreated=%s", args["notCreated"])
	}
	var created map[string]calendars.CalendarEvent
	if err := json.Unmarshal(args["created"], &created); err != nil {
		t.Fatal(err)
	}
	id := string(created["e1"].ID)
	if id == "" {
		t.Fatalf("created=%s", args["created"])
	}
	resps = call(t, srv, fmt.Sprintf(`[["CalendarEvent/get",{"accountId":"acct1","ids":[%q]},"g1"]]`, id))
	var list []calendars.CalendarEvent
	if err := json.Unmarshal(argsOf(t, resps[0])["list"], &list); err != nil || len(list) != 1 {
		t.Fatalf("get=%s err=%v", argsOf(t, resps[0])["list"], err)
	}
	ev := list[0]
	if ev.Type != "Event" || ev.RecurrenceRule == nil || ev.RecurrenceRule.Frequency != "weekly" || len(ev.RecurrenceRule.ByDay) != 1 || ev.RecurrenceRule.ByDay[0].Day != "mo" {
		t.Fatalf("recurrence=%+v type=%q", ev.RecurrenceRule, ev.Type)
	}
	p := ev.Participants["p1"]
	if p == nil || p.ParticipationStatus != "accepted" || p.Email != "bob@example.com" || !p.Roles["attendee"] {
		t.Fatalf("participant=%+v", p)
	}
	a := ev.Alerts["a1"]
	if a == nil || a.Type != "OffsetTrigger" || a.Offset != "-PT15M" || a.RelativeTo != "start" {
		t.Fatalf("alert=%+v", a)
	}
	if !ev.CalendarIDs["1"] {
		t.Fatalf("calendarIds=%v", ev.CalendarIDs)
	}
	// recurrenceRules array form also decodes
	var ev2 calendars.CalendarEvent
	if err := json.Unmarshal([]byte(`{"title":"x","start":"2026-01-05T09:00:00","recurrenceRules":[{"frequency":"daily"}]}`), &ev2); err != nil || ev2.RecurrenceRule == nil || ev2.RecurrenceRule.Frequency != "daily" {
		t.Fatalf("recurrenceRules decode=%+v err=%v", ev2, err)
	}
	// query inCalendar + after (only standup), sorted by start
	resps = call(t, srv, `[["CalendarEvent/query",{"accountId":"acct1","filter":{"inCalendar":"1","after":"2026-01-02T00:00:00"},"sort":[{"property":"start"}]},"q1"]]`)
	var ids []string
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("query ids=%s want [%s]", argsOf(t, resps[0])["ids"], id)
	}
	// text + before + hasRecurrence via operator
	resps = call(t, srv, `[["CalendarEvent/query",{"accountId":"acct1","filter":{"operator":"AND","conditions":[{"text":"stand"},{"hasRecurrence":true}]}},"q2"]]`)
	ids = nil
	if err := json.Unmarshal(argsOf(t, resps[0])["ids"], &ids); err != nil || len(ids) != 1 {
		t.Fatalf("text query ids=%s", argsOf(t, resps[0])["ids"])
	}
	// changes via Memory
	resps = call(t, srv, `[["CalendarEvent/changes",{"accountId":"acct1","sinceState":"0"},"ch1"]]`)
	var chCreated []string
	_ = json.Unmarshal(argsOf(t, resps[0])["created"], &chCreated)
	if len(chCreated) != 2 {
		t.Fatalf("changes created=%v", chCreated)
	}
}

func TestAvailabilityIdentityAndNotifications(t *testing.T) {
	srv, _ := seed(t)
	resps := call(t, srv, `[["CalendarEvent/getAvailability",{"accountId":"acct1","attendees":["bob@example.com"],"start":"2026-01-05T00:00:00Z","end":"2026-01-06T00:00:00Z"},"a1"]]`)
	if nameOf(resps[0]) == "error" {
		t.Fatalf("availability error=%s", resps[0][1])
	}
	var avail map[string][]calendars.BusyPeriod
	if err := json.Unmarshal(argsOf(t, resps[0])["availability"], &avail); err != nil || len(avail["bob@example.com"]) != 1 || avail["bob@example.com"][0].BusyType != "busy" {
		t.Fatalf("availability=%s err=%v", argsOf(t, resps[0])["availability"], err)
	}
	// ParticipantIdentity via datatype
	resps = call(t, srv, `[["ParticipantIdentity/set",{"accountId":"acct1","create":{"i1":{"name":"Me","email":"user@example.com","calendarId":null}}},"pi1"]]`)
	if string(argsOf(t, resps[0])["notCreated"]) != "null" {
		t.Fatalf("identity notCreated=%s", argsOf(t, resps[0])["notCreated"])
	}
	resps = call(t, srv, `[["ParticipantIdentity/get",{"accountId":"acct1","ids":null},"pi2"]]`)
	var ids []calendars.ParticipantIdentity
	if err := json.Unmarshal(argsOf(t, resps[0])["list"], &ids); err != nil || len(ids) != 1 || ids[0].Email != "user@example.com" {
		t.Fatalf("identity get=%s", argsOf(t, resps[0])["list"])
	}
	// Notifications get + changes
	resps = call(t, srv, `[["CalendarEventNotification/get",{"accountId":"acct1","ids":null},"n1"]]`)
	var notifs []calendars.CalendarEventNotification
	if err := json.Unmarshal(argsOf(t, resps[0])["list"], &notifs); err != nil || len(notifs) != 1 || notifs[0].EventID != "1" || notifs[0].Type != "created" {
		t.Fatalf("notif get=%s err=%v", argsOf(t, resps[0])["list"], err)
	}
	if string(argsOf(t, resps[0])["state"]) != `"1"` {
		t.Fatalf("notif state=%s", argsOf(t, resps[0])["state"])
	}
	resps = call(t, srv, `[["CalendarEventNotification/changes",{"accountId":"acct1","sinceState":"0"},"n2"]]`)
	var created []string
	_ = json.Unmarshal(argsOf(t, resps[0])["created"], &created)
	if len(created) != 1 || created[0] != "n1" {
		t.Fatalf("notif changes=%s", resps[0][1])
	}
	// notifications have no set method
	resps = call(t, srv, `[["CalendarEventNotification/set",{"accountId":"acct1","create":{}},"n3"]]`)
	if nameOf(resps[0]) != "error" {
		t.Fatalf("notif set should be unknownMethod, got %s", resps[0][1])
	}
}
