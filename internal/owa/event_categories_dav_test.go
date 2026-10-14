package owa

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/davserver"
	"github.com/rs/zerolog"
)

func TestOutlookEventCategoriesThroughCalDAV(t *testing.T) {
	const email = "user@example.com"
	var mu sync.Mutex
	first, sibling := calendarReadbackItem("first-cloud-id", "first-uid"), calendarReadbackItem("sibling-cloud-id", "sibling-uid")
	for _, item := range []map[string]any{first, sibling} {
		delete(item, "Recurrence")
		item["Subject"] = "Same title"
		item["Categories"] = []any{"Work", "Sales, Europe", "Budget; planning"}
	}
	items := map[string]map[string]any{"first-cloud-id": first, "sibling-cloud-id": sibling}
	_, upstream := newFake(t, func(action string, req map[string]any) any {
		mu.Lock()
		defer mu.Unlock()
		switch action {
		case "FindFolder":
			return okBody(map[string]any{"RootFolder": map[string]any{"Folders": []any{}}})
		case "FindItem":
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{map[string]any{"ItemId": first["ItemId"]}, map[string]any{"ItemId": sibling["ItemId"]}}, "TotalItemsInView": 2}})
		case "GetItem":
			includeCategories := false
			for _, property := range arr(req, "Body", "ItemShape", "AdditionalProperties") {
				if str(property, "FieldURI") == "item:Categories" {
					includeCategories = true
				}
			}
			var responses []any
			for _, id := range arr(req, "Body", "ItemIds") {
				item := items[str(id, "Id")]
				readback := map[string]any{}
				for key, value := range item {
					if key != "Categories" || includeCategories {
						readback[key] = value
					}
				}
				responses = append(responses, map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Items": []any{readback}})
			}
			return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": responses}})
		case "UpdateItem":
			changes := arr(req, "Body", "ItemChanges")
			if len(changes) != 1 || str(changes[0], "ItemId", "Id") != "first-cloud-id" {
				t.Errorf("category update selected sibling: %+v", req)
			}
			for _, change := range arr(changes[0], "Updates") {
				field := str(change, "Path", "FieldURI")
				switch field {
				case "Subject":
					first["Subject"] = str(change, "Item", "Subject")
				case "Categories":
					if str(change, "__type") != "SetItemField:#Exchange" || str(change, "Item", "__type") != "CalendarItem:#Exchange" {
						t.Errorf("not native category update: %+v", change)
					}
					first["Categories"] = arr(change, "Item", "Categories")
				}
			}
			return itemResp(map[string]any{"ItemId": first["ItemId"]})
		default:
			t.Errorf("unexpected action %s", action)
			return okBody(nil)
		}
	})
	reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: calendarReadbackLogin{}, Factory: Factory{BaseURL: upstream.(*provider).base}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Store().SaveAccount(bridge.AccountState{Email: email, Session: bridge.Session{Email: email, Channel: bridge.ChannelOWA, OWAAuthHeader: "Bearer fake", TokenExpiry: time.Now().Add(time.Hour)}}); err != nil {
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
	path := collection + "first-uid.ics"
	rr := request("GET", path, "")
	categoriesLine := `CATEGORIES:Work,Sales\, Europe,Budget\; planning`
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), categoriesLine) {
		t.Fatalf("cloud categories missing from GET: %d %s", rr.Code, rr.Body.String())
	}
	originalETag := rr.Header().Get("ETag")
	originalData := rr.Body.String()
	query := `<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT"/></C:comp-filter></C:filter></C:calendar-query>`
	rr = request("REPORT", collection, query)
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
	if rr.Code != 207 || len(result.Responses) != 2 {
		t.Fatalf("query=%d %s", rr.Code, rr.Body.String())
	}
	for _, response := range result.Responses {
		if len(response.Props) != 1 || !strings.Contains(response.Props[0].Data, categoriesLine) {
			t.Fatalf("categories lost in query: %+v", response)
		}
	}
	multiget := `<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-data/></D:prop><D:href>http://bridge.example` + path + `</D:href></C:calendar-multiget>`
	rr = request("REPORT", collection, multiget)
	if rr.Code != 207 || !strings.Contains(rr.Body.String(), categoriesLine) || strings.Contains(rr.Body.String(), "sibling-uid.ics") {
		t.Fatalf("category multiget=%d %s", rr.Code, rr.Body.String())
	}
	// Ordinary Thunderbird edits retain categories from the downloaded full object.
	edited := strings.Replace(originalData, "SUMMARY:Same title", "SUMMARY:Edited title", 1)
	if rr := request("PUT", path, edited); rr.Code != 204 {
		t.Fatalf("summary edit=%d %s", rr.Code, rr.Body.String())
	}
	rr = request("GET", path, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), categoriesLine) || !strings.Contains(rr.Body.String(), "SUMMARY:Edited title") {
		t.Fatalf("summary edit lost categories=%d %s", rr.Code, rr.Body.String())
	}
	updated := strings.Replace(rr.Body.String(), categoriesLine, `CATEGORIES:Personal,Client\, East`, 1)
	if rr := request("PUT", path, updated); rr.Code != 204 {
		t.Fatalf("category edit=%d %s", rr.Code, rr.Body.String())
	}
	rr = request("GET", path, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `CATEGORIES:Personal,Client\, East`) || rr.Header().Get("ETag") == originalETag {
		t.Fatalf("category edit readback=%d %s", rr.Code, rr.Body.String())
	}
	cleared := strings.Replace(rr.Body.String(), "CATEGORIES:Personal,Client\\, East\r\n", "", 1)
	if rr := request("PUT", path, cleared); rr.Code != 204 {
		t.Fatalf("remove last categories=%d %s", rr.Code, rr.Body.String())
	}
	if rr := request("GET", path, ""); rr.Code != 200 || strings.Contains(rr.Body.String(), "CATEGORIES:") {
		t.Fatalf("categories not cleared=%d %s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(arr(first, "Categories")) != 0 || !reflect.DeepEqual(arr(sibling, "Categories"), []any{"Work", "Sales, Europe", "Budget; planning"}) {
		t.Fatalf("category mutation lost sibling or did not clear: %+v", items)
	}
}
