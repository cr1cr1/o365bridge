package owa

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/davserver"
	"github.com/rs/zerolog"
)

// Exercise the HTTP OWA adapter, not just an in-memory bridge.Provider.
func TestCalDAVSameTitleEventsKeepSeparateIdentities(t *testing.T) {
	for _, omitUID := range []bool{false, true} {
		name := "distinct Outlook UIDs"
		if omitUID {
			name = "Microsoft item ID fallback"
		}
		t.Run(name, func(t *testing.T) {
			items := []map[string]any{
				calendarReadbackItem("outlook/item+1%2F", "outlook-uid-1"),
				calendarReadbackItem("outlook/item+2%2F", "outlook-uid-2"),
				calendarReadbackItem("outlook/item+3%2F", "outlook-uid-3"),
			}
			for _, item := range items {
				delete(item, "Recurrence")
				if omitUID {
					delete(item, "UID")
				}
			}
			_, upstream := newFake(t, func(action string, req map[string]any) any {
				switch action {
				case "FindFolder":
					return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{}}})
				case "FindItem":
					var rows []any
					for _, item := range items {
						rows = append(rows, map[string]any{"ItemId": item["ItemId"]})
					}
					return okBody(map[string]any{"RootFolder": map[string]any{"Items": rows, "TotalItemsInView": len(rows)}})
				case "GetItem":
					var responses []any
					for _, id := range arr(req, "Body", "ItemIds") {
						for _, item := range items {
							if str(id, "Id") == str(item, "ItemId", "Id") {
								responses = append(responses, map[string]any{"ResponseClass": "Success", "Items": []any{item}})
							}
						}
					}
					return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": responses}})
				default:
					t.Errorf("unexpected action %s", action)
					return okBody(nil)
				}
			})
			reg, err := bridge.NewRegistry(bridge.Config{
				StateDir: t.TempDir(), Login: calendarReadbackLogin{},
				Factory: Factory{BaseURL: upstream.(*provider).base},
			})
			if err != nil {
				t.Fatal(err)
			}
			const email = "user@example.com"
			if err := reg.Store().SaveAccount(bridge.AccountState{Email: email, Session: bridge.Session{
				Email: email, Channel: bridge.ChannelOWA, OWAAuthHeader: "Bearer fake", TokenExpiry: time.Now().Add(time.Hour),
			}}); err != nil {
				t.Fatal(err)
			}
			h := davserver.New(reg, "bridge-password", zerolog.Nop())
			request := func(method, path, body string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.SetBasicAuth(email, "bridge-password")
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, r)
				return rr
			}
			collection := "/caldav/" + email + "/calendar/"
			report := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT"><C:time-range start="20261015T000000Z" end="20261016T000000Z"/></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`
			rr := request("REPORT", collection, report)
			if rr.Code != http.StatusMultiStatus {
				t.Fatalf("REPORT = %d: %s", rr.Code, rr.Body.String())
			}
			var result struct {
				Responses []struct {
					Href  string `xml:"href"`
					Props []struct {
						Data string `xml:"prop>calendar-data"`
					} `xml:"propstat"`
				} `xml:"response"`
			}
			if err := xml.Unmarshal(rr.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Responses) != len(items) {
				t.Fatalf("same-title events collapsed: %d responses, want %d: %s", len(result.Responses), len(items), rr.Body.String())
			}
			hrefs, uids := map[string]bool{}, map[string]bool{}
			for _, response := range result.Responses {
				if hrefs[response.Href] {
					t.Fatalf("duplicate resource href: %s", response.Href)
				}
				hrefs[response.Href] = true
				if len(response.Props) != 1 {
					t.Fatalf("unexpected properties: %+v", response.Props)
				}
				data := response.Props[0].Data
				var uid string
				for _, line := range strings.Split(data, "\r\n") {
					if strings.HasPrefix(line, "UID:") {
						uid = strings.TrimPrefix(line, "UID:")
					}
				}
				if uid == "" || uids[uid] || !strings.Contains(data, "SUMMARY:Readback meeting") {
					t.Fatalf("same-title event has missing/duplicate UID or title: %s", data)
				}
				uids[uid] = true
				get := request("GET", response.Href, "")
				if get.Code != http.StatusOK || get.Body.String() != data {
					t.Fatalf("GET cannot address the independent event: %d: %s", get.Code, get.Body.String())
				}
				multiget := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>http://localhost` + response.Href + `</D:href></C:calendar-multiget>`
				mg := request("REPORT", collection, multiget)
				if mg.Code != http.StatusMultiStatus || strings.Count(mg.Body.String(), "BEGIN:VEVENT") != 1 || !strings.Contains(mg.Body.String(), "UID:"+uid) {
					t.Fatalf("absolute multiget cannot select one cloud identity: %d: %s", mg.Code, mg.Body.String())
				}
			}
		})
	}
}
