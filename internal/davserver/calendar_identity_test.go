package davserver

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/rs/zerolog"
)

func TestDAVSameTitleEventsWithMissingUIDUseUniqueCloudIDs(t *testing.T) {
	p := newFake()
	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	const first, second = "cloud/one%2F", "cloud/two%2F"
	for _, id := range []string{first, second} {
		p.events[id] = bridge.Event{ID: id, CalendarID: "cal1", Summary: "Same title", Start: start, End: start.Add(time.Hour)}
	}
	h := newHandler(t, p)
	var firstData string
	for _, id := range []string{first, second} {
		rr := req(t, h, "GET", "/caldav/"+acct+"/cal1/"+esc(id)+".ics", "", true)
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "UID:"+id+"\r\n") || !strings.Contains(rr.Body.String(), "SUMMARY:Same title") {
			t.Fatalf("event must serialize its cloud ID fallback, not a blank UID: %d: %s", rr.Code, rr.Body.String())
		}
		if id == first {
			firstData = rr.Body.String()
		}
	}
	firstPath := "/caldav/" + acct + "/cal1/" + esc(first) + ".ics"
	if rr := req(t, h, "PUT", firstPath, strings.ReplaceAll(firstData, "Same title", "Updated one"), true); rr.Code != 204 {
		t.Fatalf("update selected cloud ID: %d: %s", rr.Code, rr.Body.String())
	}
	if p.events[first].Summary != "Updated one" || p.events[second].Summary != "Same title" {
		t.Fatalf("update changed the wrong same-title event: %+v", p.events)
	}
	if rr := req(t, h, "DELETE", firstPath, "", true); rr.Code != 204 {
		t.Fatalf("delete selected cloud ID: %d: %s", rr.Code, rr.Body.String())
	}
	if len(p.events) != 1 || p.events[second].Summary != "Same title" {
		t.Fatalf("delete removed the wrong same-title event: %+v", p.events)
	}
}

func TestDAVEncodedIdentityDoesNotCrossAccounts(t *testing.T) {
	p := newFake()
	h := newHandler(t, p)
	for _, path := range []string{
		"/caldav/other%40example.com/cal1/a%2Fb.ics",
		"/caldav/" + esc(acct+"/other@example.com") + "/cal1/a%2Fb.ics",
	} {
		if rr := req(t, h, "GET", path, "", true); rr.Code != 403 {
			t.Fatalf("escaped segments must retain account isolation: %s -> %d", path, rr.Code)
		}
	}
}

func TestUIDFromHrefPreservesOpaqueIdentifier(t *testing.T) {
	for _, href := range []string{
		"a%2Fb%252Fc+q.ics",
		"/caldav/user/calendar/a%2Fb%252Fc+q.ics",
		"http://localhost/caldav/user/calendar/a%2Fb%252Fc+q.ics",
	} {
		if got := uidFromHref(href); got != "a/b%2Fc+q" {
			t.Errorf("uidFromHref(%q) = %q, want unchanged opaque identifier", href, got)
		}
	}
}

func TestDAVCalendarReportLogsIdentityCountsWithoutContents(t *testing.T) {
	p := newFake()
	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{"cloud-one", "cloud-two"} {
		p.events[id] = bridge.Event{ID: id, CalendarID: "cal1", UID: "shared-uid", Summary: "private meeting title", Description: "private event body", Start: start, End: start.Add(time.Hour)}
	}
	var buf bytes.Buffer
	h := newHandlerWithLogger(t, p, zerolog.New(&buf))
	report := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop></C:calendar-query>`
	rr := req(t, h, "REPORT", "/caldav/"+acct+"/cal1/", report, true)
	if rr.Code != 207 {
		t.Fatalf("REPORT = %d: %s", rr.Code, rr.Body.String())
	}
	logs := buf.String()
	for _, want := range []string{`"op":"calendar_report"`, `"returned":2`, `"unique_ids":2`, `"unique_uids":1`, `"duplicate_uids":1`} {
		if !strings.Contains(logs, want) {
			t.Errorf("calendar count log missing %s: %s", want, logs)
		}
	}
	for _, private := range []string{"private meeting title", "private event body", "shared-uid", "cloud-one", "cloud-two"} {
		if strings.Contains(logs, private) {
			t.Errorf("calendar contents/identifiers leaked into count log: %s", logs)
		}
	}
}
