package davserver

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type reportHrefResponse struct {
	Responses []struct {
		Href   string `xml:"href"`
		Status string `xml:"status"`
		Props  []struct {
			Data string `xml:"prop>calendar-data"`
			ETag string `xml:"prop>getetag"`
		} `xml:"propstat"`
	} `xml:"response"`
}

func TestCalendarUIDFromHrefCollectionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, href, collection, want string
		ok                           bool
	}{
		{"relative basename", "a%2Fb%252Fc+q.ics", "tasks1", "a/b%2Fc+q", true},
		{"canonical", "/caldav/user%40example.com/tasks1/a%2Fb%252Fc+q.ics", "tasks1", "a/b%2Fc+q", true},
		{"normalized account", "/caldav/USER%40EXAMPLE.COM/tasks1/a%2Fb%252Fc+q.ics", "tasks1", "a/b%2Fc+q", true},
		{"Mozilla tail", "/caldav/user%40example.com/tasks1/a/b%252Fc%2Bq.ics", "tasks1", "a/b%2Fc+q", true},
		{"absolute", "https://example.invalid/caldav/user%40example.com/tasks1/a/b%252Fc%2Bq.ics", "tasks1", "a/b%2Fc+q", true},
		{"unicode suffix", "/caldav/user%40example.com/tasks1/a/%E4%BB%BB%E5%8A%A1.ics.ics", "tasks1", "a/任务.ics", true},
		{"literal dot suffix", "a.vcf.ics", "tasks1", "a.vcf", true},
		{"opaque dots", "/caldav/user%40example.com/tasks1/../a.ics", "tasks1", "../a", true},
		{"literal query character", "a%3Fq%23f.ics", "tasks1", "a?q#f", true},
		{"opaque collection", "/caldav/user%40example.com/list%2Fa%252Fb+q/item%2Fone.ics", "list/a%2Fb+q", "item/one", true},
		{"opaque collection Mozilla resource", "/caldav/user%40example.com/list%2Fa%252Fb%2Bq/item/one.ics", "list/a%2Fb+q", "item/one", true},
		{"foreign account", "/caldav/other%40example.com/tasks1/a.ics", "tasks1", "", false},
		{"foreign collection", "/caldav/user%40example.com/tasks2/a.ics", "tasks1", "", false},
		{"foreign collection decoded prefix", "/caldav/user%40example.com/list/a%252Fb%2Bq/item/one.ics", "list/a%2Fb+q", "", false},
		{"encoded account slash", "/caldav/user%40example.com%2Fother/tasks1/a.ics", "tasks1", "", false},
		{"wrong root", "/carddav/user%40example.com/tasks1/a.ics", "tasks1", "", false},
		{"relative nested path", "tasks1/a.ics", "tasks1", "", false},
		{"query", "/caldav/user%40example.com/tasks1/a.ics?q=1", "tasks1", "", false},
		{"fragment", "/caldav/user%40example.com/tasks1/a.ics#fragment", "tasks1", "", false},
		{"empty query", "a.ics?", "tasks1", "", false},
		{"empty fragment", "a.ics#", "tasks1", "", false},
		{"malformed escape", "a%2.ics", "tasks1", "", false},
		{"network path", "//example.invalid/caldav/user%40example.com/tasks1/a.ics", "tasks1", "", false},
		{"userinfo", "https://name:password@example.invalid/caldav/user%40example.com/tasks1/a.ics", "tasks1", "", false},
		{"opaque URI", "urn:a.ics", "tasks1", "", false},
		{"no extension", "a", "tasks1", "", false},
		{"empty identifier", ".ics", "tasks1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := calendarUIDFromHref(tc.href, acct, tc.collection)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("decoded=(%q,%v), want=(%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestDAVOpaqueHrefRoutes(t *testing.T) {
	p := newFake()
	const id = "opaque/a%2Fb+q/任务.ics"
	p.tasks[id] = bridge.Task{ID: id, UID: id, ListID: "tasks1", Title: "Opaque task", Status: "needs-action"}
	h := newHandler(t, p)
	path := "/caldav/" + acct + "/tasks1/opaque/a%252Fb%2Bq/%E4%BB%BB%E5%8A%A1.ics.ics"
	get := req(t, h, "GET", path, "", true)
	if get.Code != 200 || !strings.Contains(get.Body.String(), "UID:"+id+"\r\n") {
		t.Fatalf("Mozilla GET=%d %s", get.Code, get.Body.String())
	}
	if head := req(t, h, "HEAD", path, "", true); head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("ETag") != get.Header().Get("ETag") {
		t.Fatalf("Mozilla HEAD=%d %s", head.Code, head.Body.String())
	}
	edited := strings.Replace(get.Body.String(), "SUMMARY:Opaque task", "SUMMARY:Edited task", 1)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		for _, foreign := range []struct {
			path   string
			status int
		}{
			{strings.Replace(path, acct, "other@example.com", 1), 403},
			{strings.Replace(path, "/tasks1/", "/tasks2/", 1), 404},
		} {
			if got := req(t, h, method, foreign.path, edited, true); got.Code != foreign.status {
				t.Fatalf("foreign %s=%d, want %d", method, got.Code, foreign.status)
			}
		}
	}
	if len(p.tasks) != 1 || p.tasks[id].Title != "Opaque task" {
		t.Fatal("foreign object request changed native task")
	}
	if put := req(t, h, "PUT", path, edited, true); put.Code != 204 || len(p.tasks) != 1 || p.tasks[id].Title != "Edited task" {
		t.Fatalf("Mozilla PUT=%d tasks=%+v", put.Code, p.tasks)
	}
	if deleted := req(t, h, "DELETE", path, "", true); deleted.Code != 204 || len(p.tasks) != 0 {
		t.Fatalf("Mozilla DELETE=%d tasks=%+v", deleted.Code, p.tasks)
	}
}

func TestDAVMultigetMissingAndForeignHrefStatuses(t *testing.T) {
	p := newFake()
	const id = "opaque/a%2Fb+q"
	p.tasks[id] = bridge.Task{ID: id, UID: id, ListID: "tasks1", Title: "Opaque task"}
	h := newHandler(t, p)
	collection := "/caldav/" + acct + "/tasks1/"
	hrefs := []string{
		collection + "opaque/a%252Fb%2Bq.ics",
		collection + "missing.ics",
		"/caldav/other@example.com/tasks1/opaque/a%252Fb%2Bq.ics",
		"/caldav/" + acct + "/tasks2/opaque/a%252Fb%2Bq.ics",
		collection + "opaque/a%252Fb%2Bq.ics?query=1",
		collection + "broken%2.ics",
	}
	body := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop>`
	for _, href := range hrefs {
		body += "<D:href>" + href + "</D:href>"
	}
	body += `</C:calendar-multiget>`
	got := req(t, h, "REPORT", collection, body, true)
	var result reportHrefResponse
	if err := xml.Unmarshal(got.Body.Bytes(), &result); err != nil || got.Code != 207 || len(result.Responses) != len(hrefs) {
		t.Fatalf("multiget status=%d result=%+v err=%v", got.Code, result, err)
	}
	success, missing := 0, map[string]bool{}
	for _, response := range result.Responses {
		if response.Status == "HTTP/1.1 404 Not Found" && len(response.Props) == 0 {
			missing[response.Href] = true
		} else if len(response.Props) == 1 && strings.Contains(response.Props[0].Data, "UID:"+id+"\r\n") {
			success++
		} else {
			t.Fatalf("unexpected multiget response: %+v", response)
		}
	}
	if success != 1 {
		t.Fatalf("resolved resources=%d", success)
	}
	for _, href := range hrefs[1:] {
		if !missing[href] {
			t.Errorf("missing explicit 404 for requested resource")
		}
	}
}

func TestDAVMultigetRejectsAmbiguousNativeIDAlias(t *testing.T) {
	p := newFake()
	p.tasks["native-one"] = bridge.Task{ID: "native-one", UID: "client-one", ListID: "tasks1"}
	p.tasks["native-two"] = bridge.Task{ID: "native-two", UID: "native-one", ListID: "tasks1"}
	h := newHandler(t, p)
	collection := "/caldav/" + acct + "/tasks1/"
	body := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>` + collection + `native-one.ics</D:href></C:calendar-multiget>`
	got := req(t, h, "REPORT", collection, body, true)
	if got.Code != 409 || strings.Contains(got.Body.String(), "BEGIN:VTODO") {
		t.Fatalf("ambiguous resource must fail before returning data: %d %s", got.Code, got.Body.String())
	}
}

func TestDAVOpaqueAliasCRUDRejectsAmbiguity(t *testing.T) {
	for _, kind := range []string{"task", "event"} {
		for _, operation := range []string{"GET", "HEAD", "DELETE", "PUT body UID", "PUT path fallback"} {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				p := newFake()
				const firstID, secondID = "native/one%2F", "native/two"
				collection, component := "tasks1", "VTODO"
				if kind == "task" {
					p.tasks[firstID] = bridge.Task{ID: firstID, UID: "client-one", ListID: collection, Title: "First"}
					p.tasks[secondID] = bridge.Task{ID: secondID, UID: firstID, ListID: collection, Title: "Second"}
				} else {
					collection, component = "cal1", "VEVENT"
					start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
					p.events[firstID] = bridge.Event{ID: firstID, UID: "client-one", CalendarID: collection, Summary: "First", Start: start}
					p.events[secondID] = bridge.Event{ID: secondID, UID: firstID, CalendarID: collection, Summary: "Second", Start: start}
				}
				h := newHandler(t, p)
				path := "/caldav/" + acct + "/" + collection + "/native/one%252F.ics"
				method, body := operation, ""
				if strings.HasPrefix(operation, "PUT") {
					method = "PUT"
					identity := ""
					if operation == "PUT body UID" {
						// Body UID, not the unambiguous URL, owns existing PUT selection.
						path = "/caldav/" + acct + "/" + collection + "/client-one.ics"
						identity = "UID:" + firstID + "\r\n"
					}
					body = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:" + component + "\r\n" + identity + "SUMMARY:Mutation attempt\r\nEND:" + component + "\r\nEND:VCALENDAR\r\n"
				}
				got := req(t, h, method, path, body, true)
				if got.Code != 409 || strings.Contains(got.Body.String(), "BEGIN:VCALENDAR") {
					t.Errorf("ambiguous %s selected a row: status=%d", operation, got.Code)
				}
				if kind == "task" {
					if len(p.tasks) != 2 || p.tasks[firstID].Title != "First" || p.tasks[secondID].Title != "Second" || p.tasks[firstID].UID != "client-one" || p.tasks[secondID].UID != firstID {
						t.Fatalf("ambiguous operation mutated a native task: %+v", p.tasks)
					}
				} else if len(p.events) != 2 || p.events[firstID].Summary != "First" || p.events[secondID].Summary != "Second" || len(p.responds) != 0 {
					t.Fatalf("ambiguous operation mutated a native event: %+v", p.events)
				}
			})
		}
	}
}

func TestDAVTaskPUTUniqueBodyUIDKeepsExistingSelection(t *testing.T) {
	p := newFake()
	const firstID, secondID = "native/one%2F", "native/two"
	p.tasks[firstID] = bridge.Task{ID: firstID, UID: "client-one", ListID: "tasks1", Title: "First"}
	p.tasks[secondID] = bridge.Task{ID: secondID, UID: firstID, ListID: "tasks1", Title: "Second"}
	h := newHandler(t, p)
	body := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:client-one\r\nSUMMARY:Updated first\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	got := req(t, h, "PUT", "/caldav/"+acct+"/tasks1/native/one%252F.ics", body, true)
	if got.Code != 204 || len(p.tasks) != 2 || p.tasks[firstID].Title != "Updated first" || p.tasks[secondID].Title != "Second" {
		t.Fatalf("unique body UID changed existing PUT selection: status=%d tasks=%+v", got.Code, p.tasks)
	}
}

func TestDAVMultigetMozillaOpaqueHref(t *testing.T) {
	p := newFake()
	const id = "opaque/a%2Fb+q/任务.ics"
	p.tasks[id] = bridge.Task{ID: id, UID: id, ListID: "tasks1", Title: "Opaque task", Status: "needs-action"}
	h := newHandler(t, p)
	collection := "/caldav/" + acct + "/tasks1/"
	query := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VTODO"/></C:comp-filter></C:filter></C:calendar-query>`
	listed := req(t, h, "REPORT", collection, query, true)
	var listing reportHrefResponse
	if err := xml.Unmarshal(listed.Body.Bytes(), &listing); err != nil || listed.Code != 207 || len(listing.Responses) != 1 {
		t.Fatalf("query status=%d result=%+v err=%v", listed.Code, listing, err)
	}
	for _, href := range []string{
		collection + "opaque%2Fa%252Fb+q%2F%E4%BB%BB%E5%8A%A1.ics.ics",
		// Literal output of Mozilla ensureEncodedPath(ensureDecodedPath(canonicalHref)).
		collection + "opaque/a%252Fb%2Bq/%E4%BB%BB%E5%8A%A1.ics.ics",
		"https://example.invalid" + collection + "opaque/a%252Fb%2Bq/%E4%BB%BB%E5%8A%A1.ics.ics",
		"opaque%2Fa%252Fb+q%2F%E4%BB%BB%E5%8A%A1.ics.ics",
		strings.Replace(collection, acct, "USER%40EXAMPLE.COM", 1) + "opaque/a%252Fb%2Bq/%E4%BB%BB%E5%8A%A1.ics.ics",
	} {
		body := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><D:href>` + href + `</D:href></C:calendar-multiget>`
		got := req(t, h, "REPORT", collection, body, true)
		var result reportHrefResponse
		if err := xml.Unmarshal(got.Body.Bytes(), &result); err != nil || got.Code != 207 || len(result.Responses) != 1 {
			t.Fatalf("opaque multiget status=%d result=%+v err=%v", got.Code, result, err)
		}
		response := result.Responses[0]
		if len(response.Props) != 1 || !strings.Contains(response.Props[0].Data, "UID:"+id+"\r\n") || response.Href != listing.Responses[0].Href || response.Props[0].ETag != listing.Responses[0].Props[0].ETag {
			t.Fatalf("opaque href did not resolve the canonical resource: %+v", response)
		}
	}
}

func TestDAVMultigetLongNativeIDAliasKeepsClientUID(t *testing.T) {
	p := newFake()
	const prefix = "AAMkAGFiY2RlZjAxMjM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkw"
	const id = prefix + "/native%2F+opaque.ics"
	p.tasks[id] = bridge.Task{ID: id, UID: "client-series", ListID: "tasks1", Title: "Native task"}
	h := newHandler(t, p)
	collection := "/caldav/" + acct + "/tasks1/"
	for _, href := range []string{
		collection + prefix + "%2Fnative%252F+opaque.ics.ics",
		collection + prefix + "/native%252F%2Bopaque.ics.ics",
		collection + "client-series.ics",
	} {
		body := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>` + href + `</D:href></C:calendar-multiget>`
		got := req(t, h, "REPORT", collection, body, true)
		var result reportHrefResponse
		if err := xml.Unmarshal(got.Body.Bytes(), &result); err != nil || got.Code != 207 || len(result.Responses) != 1 || len(result.Responses[0].Props) != 1 || result.Responses[0].Href != collection+"client-series.ics" || !strings.Contains(result.Responses[0].Props[0].Data, "UID:client-series\r\n") {
			t.Fatalf("native ID alias lost client UID: status=%d result=%+v err=%v", got.Code, result, err)
		}
	}
	if len(p.tasks) != 1 || p.tasks[id].UID != "client-series" {
		t.Fatal("href normalization mutated stored identity")
	}
}
