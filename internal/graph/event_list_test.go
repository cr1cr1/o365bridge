package graph_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGraphEventListUsesMastersAndFiltersSingles(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	for _, calendar := range []string{"", "cal"} {
		t.Run("calendar="+calendar, func(t *testing.T) {
			cases := []struct {
				name     string
				from, to time.Time
				want     []string
			}{
				{"bounded", start, start.Add(time.Hour), []string{"master", "overlap-start", "inside", "overlap-end", "spanning", "master-opaque", "page2"}},
				{"unbounded", time.Time{}, time.Time{}, []string{"master", "before", "ends-at-from", "overlap-start", "inside", "overlap-end", "starts-at-to", "after", "spanning", "master-opaque", "page2"}},
				{"lower-only", start, time.Time{}, []string{"master", "overlap-start", "inside", "overlap-end", "starts-at-to", "after", "spanning", "master-opaque", "page2"}},
				{"upper-only", time.Time{}, start.Add(time.Hour), []string{"master", "before", "ends-at-from", "overlap-start", "inside", "overlap-end", "spanning", "master-opaque", "page2"}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					path := "/v1.0/me/events"
					if calendar != "" {
						path = "/v1.0/me/calendars/" + calendar + "/events"
					}
					f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
						if strings.Contains(strings.ToLower(r.URL.Path), "calendarview") {
							t.Error("expanded occurrence source must not be used")
							jsonOut(w, 400, `{"error":{"message":"calendarview forbidden"}}`)
							return
						}
						if r.Method != http.MethodGet || r.URL.Path != path {
							t.Errorf("request %s %s", r.Method, r.URL.Path)
						}
						if r.URL.Query().Has("startDateTime") || r.URL.Query().Has("endDateTime") || r.URL.Query().Has("$filter") {
							t.Errorf("master source has window filter: %s", r.URL.RawQuery)
						}
						if r.URL.Query().Get("$skiptoken") == "next" {
							jsonOut(w, 200, `{"value":[{"id":"page2","type":"singleInstance","start":{"dateTime":"2026-03-02T09:15:00","timeZone":"UTC"},"end":{"dateTime":"2026-03-02T09:45:00","timeZone":"UTC"}}]}`)
							return
						}
						if r.URL.Query().Get("$orderby") != "start/dateTime" {
							t.Errorf("order=%s", r.URL.RawQuery)
						}
						items := []map[string]any{{"id": "master", "type": "seriesMaster", "iCalUId": "series-uid", "start": map[string]string{"dateTime": "2020-01-01T09:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2020-01-01T10:00:00", "timeZone": "UTC"}, "recurrence": map[string]any{"pattern": map[string]any{"type": "daily", "interval": 1}, "range": map[string]any{"type": "noEnd", "startDate": "2020-01-01"}}}}
						for _, single := range []struct{ id, start, end string }{
							{"before", "07:00", "08:00"}, {"ends-at-from", "08:00", "09:00"}, {"overlap-start", "08:30", "09:30"}, {"inside", "09:10", "09:20"}, {"overlap-end", "09:30", "10:30"}, {"starts-at-to", "10:00", "11:00"}, {"after", "11:00", "12:00"}, {"spanning", "08:00", "11:00"},
						} {
							items = append(items, map[string]any{"id": single.id, "type": "singleInstance", "start": map[string]string{"dateTime": "2026-03-02T" + single.start + ":00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-03-02T" + single.end + ":00", "timeZone": "UTC"}})
						}
						items = append(items, map[string]any{"id": "master-opaque", "type": "seriesMaster", "start": map[string]string{"dateTime": "2020-01-01T09:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2020-01-01T10:00:00", "timeZone": "UTC"}})
						response, _ := json.Marshal(map[string]any{"value": items, "@odata.nextLink": "http://" + r.Host + path + "?$skiptoken=next"})
						jsonOut(w, 200, string(response))
					})
					got, err := f.provider().ListEvents(context.Background(), calendar, tc.from, tc.to)
					if err != nil {
						t.Fatal(err)
					}
					var ids []string
					for _, event := range got {
						ids = append(ids, event.ID)
						if event.CalendarID != calendar {
							t.Errorf("calendar=%q", event.CalendarID)
						}
					}
					if !reflect.DeepEqual(ids, tc.want) {
						t.Fatalf("events=%v want=%v", ids, tc.want)
					}
					if got[0].Recurrence != "FREQ=DAILY;INTERVAL=1" || got[0].RecurrenceUnsupported || got[0].UID != "series-uid" {
						t.Errorf("master=%+v", got[0])
					}
					if len(f.reqs) != 2 {
						t.Errorf("page requests=%d", len(f.reqs))
					}
				})
			}
		})
	}
}

func TestGraphEventOfficialRecurrenceResponses(t *testing.T) {
	for _, tc := range []struct {
		name, fixture, rule, zone string
		collection                bool
	}{
		{"group-list-events", officialGroupListEvents, "FREQ=WEEKLY;INTERVAL=1;BYDAY=MO,WE,FR;WKST=SU", "America/New_York", true},
		{"user-post-events-example3", officialPostRecurringEvent, "FREQ=WEEKLY;INTERVAL=1;BYDAY=MO;WKST=SU;UNTIL=20171231T200000Z", "America/Los_Angeles", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collection := tc.fixture
			if !tc.collection {
				collection = `{"value":[` + tc.fixture + `]}`
			}
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				if r.URL.Path != "/v1.0/me/events" || r.Method != http.MethodGet {
					t.Errorf("not native master source: %s %s", r.Method, r.URL.Path)
					jsonOut(w, 400, `{"error":{"message":"events only"}}`)
					return
				}
				jsonOut(w, 200, collection)
			})
			// An old DTSTART does not remove a series master from a later client expansion window.
			from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
			events, err := f.provider().ListEvents(context.Background(), "", from, from.AddDate(0, 0, 1))
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 {
				t.Fatalf("events=%+v", events)
			}
			got := events[0]
			if got.RecurrenceUnsupported || got.Recurrence != tc.rule || got.TimeZone != tc.zone || got.ID == "" || got.UID == "" || got.ReminderMin == nil || *got.ReminderMin != 15 || len(got.Attendees) == 0 {
				t.Errorf("official native=%+v want=%s", got, tc.rule)
			}
			if !tc.collection {
				create := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) { jsonOut(w, 201, tc.fixture) })
				got, err := create.provider().CreateEvent(context.Background(), recurringEvent())
				if err != nil {
					t.Fatal(err)
				}
				if got.RecurrenceUnsupported || got.Recurrence != tc.rule || got.ID != "AAMkADQwMD" {
					t.Errorf("official create=%+v", got)
				}
			}
		})
	}
}

func TestGraphEventReadDefaultsAndSentinels(t *testing.T) {
	for _, tc := range []struct {
		name, pattern, rng, rule string
		unsupported              bool
	}{
		{"daily-default-fields", `{"type":"daily","interval":1,"month":0,"dayOfMonth":0,"daysOfWeek":[],"firstDayOfWeek":"sunday","index":"first"}`, `{"type":"noEnd","startDate":"2026-03-02","endDate":"0001-01-01","numberOfOccurrences":0}`, "FREQ=DAILY;INTERVAL=1", false},
		{"weekly-default-index", `{"type":"weekly","interval":1,"daysOfWeek":["monday"],"index":"first"}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "FREQ=WEEKLY;INTERVAL=1;BYDAY=MO;WKST=SU", false},
		{"relative-omitted-index", `{"type":"relativeMonthly","interval":1,"daysOfWeek":["monday"],"firstDayOfWeek":"sunday"}`, `{"type":"numbered","startDate":"2026-03-02","endDate":"0001-01-01","numberOfOccurrences":3}`, "FREQ=MONTHLY;INTERVAL=1;BYDAY=MO;BYSETPOS=1;COUNT=3", false},
		{"noend-meaningful-end", `{"type":"daily","interval":1}`, `{"type":"noEnd","startDate":"2026-03-02","endDate":"2026-04-01"}`, "", true},
		{"numbered-meaningful-end", `{"type":"daily","interval":1}`, `{"type":"numbered","startDate":"2026-03-02","endDate":"2026-04-01","numberOfOccurrences":3}`, "", true},
		{"noend-nonzero-count", `{"type":"daily","interval":1}`, `{"type":"noEnd","startDate":"2026-03-02","endDate":"0001-01-01","numberOfOccurrences":3}`, "", true},
		{"weekly-nondefault-index", `{"type":"weekly","interval":1,"daysOfWeek":["monday"],"index":"last"}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "", true},
		{"daily-nondefault-weekstart", `{"type":"daily","interval":1,"firstDayOfWeek":"monday"}`, `{"type":"noEnd","startDate":"2026-03-02"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				jsonOut(w, 200, fmt.Sprintf(`{"value":[{"id":"native","type":"seriesMaster","start":{"dateTime":"2026-03-02T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-03-02T10:00:00","timeZone":"UTC"},"recurrence":{"pattern":%s,"range":%s}}]}`, tc.pattern, tc.rng))
			})
			events, err := f.provider().ListEvents(context.Background(), "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Recurrence != tc.rule || events[0].RecurrenceUnsupported != tc.unsupported {
				t.Errorf("native=%+v want rule=%q unsupported=%v", events, tc.rule, tc.unsupported)
			}
		})
	}
}

// Full Microsoft Learn group-list-events response, not a reduced recurrence mock:
// https://learn.microsoft.com/en-us/graph/api/group-list-events?view=graph-rest-1.0
const officialGroupListEvents = `{
  "value": [
    {
      "id": "AQMkAGI5MWY5ZmUyLTJiNzYtNDE0ZC04OWEwLWM3M2FjYmM3NwAzZWYARgAAA_b2VnUAiWNLj0xeSOs499YHAMT2RdsuOqRIlQZ4vOzp66YAAAIBDQAAAMT2RdsuOqRIlQZ4vOzp66YAAAIJOwAAAA==",
      "createdDateTime": "2017-07-31T18:59:01.982289Z",
      "lastModifiedDateTime": "2017-09-06T04:29:38.6647687Z",
      "changeKey": "xPZF2y46pEiVBni87OnrpgAAFq78Xw==",
      "categories": [],
      "originalStartTimeZone": "Eastern Standard Time",
      "originalEndTimeZone": "Eastern Standard Time",
      "iCalUId": "040000008200E00074C5B7101A82E00800000000824DDB122F0AD301000000000000000010000000824A8905B038D54AA7735F117B3442ED",
      "reminderMinutesBeforeStart": 15,
      "isReminderOn": true,
      "hasAttachments": false,
      "subject": "New Training Plans",
      "bodyPreview": "Meeting to plan new trainings.\r\n\r\n\r\n\r\nJoin Microsoft Teams Online Meeting<https://teams.microsoft.com/l/meetup-join/19%3a900876baa3134907b0dcb41a0d220e31%40thread.skype/1501527539926?tenantId=dcd219dd-bc68-4b9b-bf0b-4a33a796be35>",
      "importance": "normal",
      "sensitivity": "normal",
      "isAllDay": false,
      "isCancelled": false,
      "isOrganizer": true,
      "responseRequested": true,
      "seriesMasterId": null,
      "showAs": "busy",
      "type": "seriesMaster",
      "webLink": "https://outlook.office365.com/owa/?itemid=AQMkAGI5MWY5ZmUyLTJiNzYtNDE0ZC04OWEwLWM3M2FjYmM3NwAzZWYARgAAA%2Bb2VnUAiWNLj0xeSOs499YHAMT2RdsuOqRIlQZ4vOzp66YAAAIBDQAAAMT2RdsuOqRIlQZ4vOzp66YAAAIJOwAAAA%3D%3D&exvsurl=1&path=/calendar/item",
      "onlineMeetingUrl": null,
      "responseStatus": {"response": "organizer", "time": "0001-01-01T00:00:00Z"},
      "body": {
          "contentType": "html",
          "content": "<html>\r\n<head>\r\n<meta http-equiv=\"Content-Type\" content=\"text/html; charset=utf-8\">\r\n</head>\r\n<body>\r\n<div>Meeting to plan new trainings.</div>\r\n<div><br>\r\n<br>\r\n<br>\r\n<a href=\"https://teams.microsoft.com/l/meetup-join/19%3a900876baa3134907b0dcb41a0d220e31%40thread.skype/1501527539926?tenantId=dcd219dd-bc68-4b9b-bf0b-4a33a796be35\">Join Microsoft Teams Online Meeting</a></div>\r\n</body>\r\n</html>\r\n"
      },
      "start": {"dateTime": "2017-08-14T21:00:00.0000000", "timeZone": "UTC"},
      "end": {"dateTime": "2017-08-14T22:00:00.0000000", "timeZone": "UTC"},
      "location": {"displayName": "HR Taskforce / Facilities"},
      "recurrence": {
          "pattern": {"type": "weekly", "interval": 1, "month": 0, "dayOfMonth": 0, "daysOfWeek": ["monday", "wednesday", "friday"], "firstDayOfWeek": "sunday", "index": "first"},
          "range": {"type": "noEnd", "startDate": "2017-08-14", "endDate": "0001-01-01", "recurrenceTimeZone": "Eastern Standard Time", "numberOfOccurrences": 0}
      },
      "attendees": [
          {"type": "required", "status": {"response": "accepted", "time": "2017-07-31T18:59:06.4180028Z"}, "emailAddress": {"name": "Joni Sherman", "address": "JoniS@contoso.com"}},
          {"type": "required", "status": {"response": "none", "time": "0001-01-01T00:00:00Z"}, "emailAddress": {"name": "HR Taskforce", "address": "HRTaskforce@contoso.com"}},
          {"type": "required", "status": {"response": "none", "time": "0001-01-01T00:00:00Z"}, "emailAddress": {"name": "Megan Bowen", "address": "MeganB@contoso.com"}},
          {"type": "required", "status": {"response": "none", "time": "0001-01-01T00:00:00Z"}, "emailAddress": {"name": "Lidia Holloway", "address": "LidiaH@contoso.com"}},
          {"type": "required", "status": {"response": "none", "time": "0001-01-01T00:00:00Z"}, "emailAddress": {"name": "Emily Braun", "address": "EmilyB@contoso.com"}}
      ],
      "organizer": {"emailAddress": {"name": "HR Taskforce", "address": "HRTaskforce@contoso.com"}}
    }
  ]
}`

// Full Microsoft Learn user-post-events Example 3 response; its trailing comma is removed for valid JSON.
// https://learn.microsoft.com/en-us/graph/api/user-post-events?view=graph-rest-1.0#example-3-create-a-recurring-event
const officialPostRecurringEvent = `{
    "@odata.context":"https://graph.microsoft.com/v1.0/$metadata#users('919717da-0460-4cca-a6be-d25382429896')/events/$entity",
    "@odata.etag":"W/\"+T8RDneHMkKe2BGYEaQZ4wAA5a9Acw==\"",
    "id":"AAMkADQwMD",
    "createdDateTime":"2017-10-07T04:59:12.9698856Z",
    "lastModifiedDateTime":"2017-10-07T04:59:13.8136423Z",
    "changeKey":"+T8RDneHMkKe2BGYEaQZ4wAA5a9Acw==",
    "categories":[],
    "originalStartTimeZone":"Pacific Standard Time",
    "originalEndTimeZone":"Pacific Standard Time",
    "iCalUId":"040000008200E00074C5B7101A82E0080000000028CEBE04293FD3010000000000000000100000009F85AB8AF8ED4D4FAC777FA89954BDB7",
    "reminderMinutesBeforeStart":15,
    "isReminderOn":true,
    "hasAttachments":false,
    "hideAttendees":false,
    "subject":"Let's go for lunch",
    "bodyPreview":"Does late morning work for you?",
    "importance":"normal",
    "sensitivity":"normal",
    "isAllDay":false,
    "isCancelled":false,
    "isDraft":false,
    "isOrganizer":true,
    "responseRequested":true,
    "seriesMasterId":null,
    "showAs":"busy",
    "type":"seriesMaster",
    "webLink":"https://outlook.office365.com/owa/?itemid=AAMkADQwMD&exvsurl=1&path=/calendar/item",
    "onlineMeetingUrl":null,
    "isOnlineMeeting":true,
    "onlineMeetingProvider":"unknown",
    "onlineMeeting":null,
    "allowNewTimeProposals":true,
    "responseStatus":{"response":"organizer","time":"0001-01-01T00:00:00Z"},
    "body":{"contentType":"html","content":"<html>\r\n<head>\r\n<meta http-equiv=\"Content-Type\" content=\"text/html; charset=utf-8\">\r\n<meta content=\"text/html; charset=us-ascii\">\r\n</head>\r\n<body>\r\nDoes late morning work for you?\r\n</body>\r\n</html>\r\n"},
    "start":{"dateTime":"2017-09-04T12:00:00.0000000","timeZone":"Pacific Standard Time"},
    "end":{"dateTime":"2017-09-04T14:00:00.0000000","timeZone":"Pacific Standard Time"},
    "location":{"displayName":"Harry's Bar","locationType":"default","uniqueId":"Harry's Bar","uniqueIdType":"private"},
    "locations":[{"displayName":"Harry's Bar","locationType":"default","uniqueIdType":"unknown"}],
    "recurrence":{
        "pattern":{"type":"weekly","interval":1,"month":0,"dayOfMonth":0,"daysOfWeek":["monday"],"firstDayOfWeek":"sunday","index":"first"},
        "range":{"type":"endDate","startDate":"2017-09-04","endDate":"2017-12-31","recurrenceTimeZone":"Pacific Standard Time","numberOfOccurrences":0}
    },
    "attendees":[{"type":"required","status":{"response":"none","time":"0001-01-01T00:00:00Z"},"emailAddress":{"name":"Adele Vance","address":"AdeleV@contoso.com"}}],
    "organizer":{"emailAddress":{"name":"Alex Wilber","address":"AlexW@contoso.com"}}
}`
