package owa

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// ListCalendars/ListTaskLists: _o has no enumeration call; it uses the
// distinguished "calendar"/"tasks" folders. We discover folders via
// FindFolder and fall back to the distinguished folder.
func (p *provider) listCollections(ctx context.Context, class, distID, name string, tasksOnly bool) ([]bridge.Calendar, error) {
	folders, err := p.ListFolders(ctx)
	if err == nil {
		var out []bridge.Calendar
		// FolderClass isn't in bridge.Folder; ListFolders FindFolder
		// already filtered mail folders, so matching by distinguished
		// name / display name is the tolerant fallback here.
		for _, f := range folders {
			if f.Special == "" && (strings.EqualFold(f.Name, name)) {
				out = append(out, bridge.Calendar{ID: f.ID, Name: f.Name, TasksOnly: tasksOnly})
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return []bridge.Calendar{{ID: distID, Name: name, TasksOnly: tasksOnly}}, nil
}

func (p *provider) ListCalendars(ctx context.Context) ([]bridge.Calendar, error) {
	return p.listCollections(ctx, "IPF.Appointment", "calendar", "Calendar", false)
}
func (p *provider) ListTaskLists(ctx context.Context) ([]bridge.Calendar, error) {
	return p.listCollections(ctx, "IPF.Task", "tasks", "Tasks", true)
}

// --- events ---

func bodyText(item map[string]any) string {
	if v := str(item, "TextBody", "Value"); v != "" {
		return v
	}
	return str(item, "Body", "Value")
}
func attendeeStatus(s string) string {
	switch s {
	case "Accept":
		return "accepted"
	case "Decline":
		return "declined"
	case "Tentative":
		return "tentative"
	case "Organizer":
		return "accepted"
	default:
		return "needs-action"
	}
}
func attendeesFrom(item map[string]any) []bridge.Attendee {
	var out []bridge.Attendee
	for _, key := range []string{"RequiredAttendees", "OptionalAttendees"} {
		for _, a := range arr(item, key) {
			out = append(out, bridge.Attendee{Email: mailboxEmail(a), Name: str(a, "Mailbox", "Name"), Status: attendeeStatus(str(a, "ResponseType"))})
		}
	}
	return out
}

func recurrenceToRRULE(item map[string]any) string {
	rec := asMap(item["Recurrence"])
	if rec == nil {
		return ""
	}
	pat := asMap(rec["RecurrencePattern"])
	if pat == nil {
		return ""
	}
	t := str(pat, "__type")
	freq := ""
	switch {
	case strings.HasPrefix(t, "Daily"):
		freq = "DAILY"
	case strings.HasPrefix(t, "Weekly"):
		freq = "WEEKLY"
	case strings.Contains(t, "Yearly"):
		freq = "YEARLY"
	default:
		freq = "MONTHLY"
	}
	parts := []string{"FREQ=" + freq}
	if iv := int(num(pat, "Interval")); iv > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(iv))
	}
	rng := asMap(rec["RecurrenceRange"])
	if rng != nil {
		if n := int(num(rng, "NumberOfOccurrences")); n > 0 {
			parts = append(parts, "COUNT="+strconv.Itoa(n))
		}
		if until := str(rng, "EndDate"); until != "" {
			parts = append(parts, "UNTIL="+strings.ReplaceAll(strings.TrimSpace(until[:min(10, len(until))]), "-", ""))
		}
	}
	return strings.Join(parts, ";")
}

func eventFromItem(item map[string]any, calendarID string) bridge.Event {
	// Exchange returns Windows zone IDs and naive wall times:
	// convert the ID to IANA and interpret Start/End in that zone.
	iana := ""
	if tz := str(item, "StartTimeZone", "Id"); tz != "" {
		iana = bridge.FromWindowsZone(tz)
	} else if tz := str(item, "StartTimeZone", "Name"); tz != "" {
		iana = bridge.FromWindowsZone(tz)
	}
	ev := bridge.Event{
		ID: str(item, "ItemId", "Id"), CalendarID: calendarID,
		UID: str(item, "UID"), Summary: str(item, "Subject"),
		Description: bodyText(item),
		Start:       parseZoned(str(item, "Start"), iana), End: parseZoned(str(item, "End"), iana),
		AllDay:     boolean(item, "IsAllDayEvent"),
		Attendees:  attendeesFrom(item),
		Recurrence: recurrenceToRRULE(item),
		TimeZone:   iana,
	}
	if loc := str(item, "Location", "DisplayName"); loc != "" {
		ev.Location = loc
	} else {
		ev.Location = str(item, "Location")
	}
	ev.Organizer = mailboxEmail(item["Organizer"])
	if boolean(item, "ReminderIsSet") {
		m := int(num(item, "ReminderMinutesBeforeStart"))
		ev.ReminderMin = &m
	}
	if ev.UID == "" {
		ev.UID = ev.ID
	}
	return ev
}

func rruleToRecurrence(rrule string, start time.Time) map[string]any {
	// Minimal RRULE -> OWA Recurrence conversion (FREQ/INTERVAL/COUNT/UNTIL).
	kv := map[string]string{}
	for _, part := range strings.Split(rrule, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			kv[strings.ToUpper(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	freq := strings.Title(strings.ToLower(kv["FREQ"]))
	if freq == "" {
		return nil
	}
	patType := freq + "Recurrence:#Exchange"
	if freq == "Monthly" || freq == "Yearly" {
		patType = "Absolute" + patType
	}
	pat := map[string]any{"__type": patType}
	if iv, err := strconv.Atoi(kv["INTERVAL"]); err == nil && iv > 0 {
		pat["Interval"] = iv
	}
	startDate := start.Format("2006-01-02")
	var rng map[string]any
	if n, err := strconv.Atoi(kv["COUNT"]); err == nil && n > 0 {
		rng = map[string]any{"__type": "NumberedRecurrence:#Exchange", "StartDate": startDate, "NumberOfOccurrences": n}
	} else if until := kv["UNTIL"]; until != "" {
		u := until
		if len(u) >= 8 {
			u = u[:4] + "-" + u[4:6] + "-" + u[6:8]
		}
		rng = map[string]any{"__type": "EndDateRecurrence:#Exchange", "StartDate": startDate, "EndDate": u}
	} else {
		rng = map[string]any{"__type": "NoEndRecurrence:#Exchange", "StartDate": startDate}
	}
	return map[string]any{"__type": "RecurrenceType:#Exchange", "RecurrencePattern": pat, "RecurrenceRange": rng}
}

func eventToOWA(ev bridge.Event) map[string]any {
	item := map[string]any{
		"__type": "CalendarItem:#Exchange", "Subject": ev.Summary,
		"Body":          map[string]any{"__type": "BodyContentType:#Exchange", "BodyType": "HTML", "Value": ev.Description},
		"IsAllDayEvent": ev.AllDay, "Start": fmtZoned(ev.Start, ev.TimeZone), "End": fmtZoned(ev.End, ev.TimeZone),
		"UID": ev.UID,
		"Location": map[string]any{"__type": "EnhancedLocation:#Exchange", "DisplayName": ev.Location,
			"PostalAddress": map[string]any{"__type": "PersonaPostalAddress:#Exchange", "Type": "Business", "LocationSource": "None"}},
	}
	if len(ev.Attendees) > 0 {
		atts := make([]any, 0, len(ev.Attendees))
		for _, a := range ev.Attendees {
			atts = append(atts, map[string]any{"__type": "AttendeeType:#Exchange", "Mailbox": mailbox(a.Email, a.Name)})
		}
		item["RequiredAttendees"] = atts
	}
	if ev.ReminderMin != nil {
		item["ReminderIsSet"] = true
		item["ReminderMinutesBeforeStart"] = *ev.ReminderMin
	}
	if ev.TimeZone != "" {
		win := bridge.ToWindowsZone(ev.TimeZone)
		item["StartTimeZone"] = map[string]any{"__type": "TimeZoneDefinitionType:#Exchange", "Id": win}
		item["EndTimeZone"] = map[string]any{"__type": "TimeZoneDefinitionType:#Exchange", "Id": win}
	}
	if rec := rruleToRecurrence(ev.Recurrence, ev.Start); rec != nil {
		item["Recurrence"] = rec
	}
	return item
}

func (p *provider) ListEvents(ctx context.Context, calendarID string, from, to time.Time) ([]bridge.Event, error) {
	// FindItem returns discovery rows, not the complete event needed by DAV/JMAP.
	var properties []any
	for _, field := range []string{
		"item:Subject", "item:Body", "item:TextBody", "item:ReminderIsSet", "item:ReminderMinutesBeforeStart",
		"calendar:Start", "calendar:End", "calendar:UID", "calendar:IsAllDayEvent", "calendar:EnhancedLocation",
		"calendar:Organizer", "calendar:RequiredAttendees", "calendar:OptionalAttendees", "calendar:Recurrence",
		"calendar:StartTimeZone", "calendar:EndTimeZone",
	} {
		properties = append(properties, propURI(field))
	}
	shape := map[string]any{"__type": "ItemResponseShape:#Exchange", "BaseShape": "Default", "AdditionalProperties": properties}
	var out []bridge.Event
	for offset := 0; ; {
		req := findItemReq(orDefault(calendarID, "calendar"), offset, 1000, nil)
		delete(asMap(req["Body"]), "ShapeName")
		res, err := p.call(ctx, "FindItem", req)
		if err != nil {
			return nil, err
		}
		if asMap(dig(res, "RootFolder")) == nil {
			return nil, fmt.Errorf("owa: ListEvents: missing discovery folder in response")
		}
		rows := arr(res, "RootFolder", "Items")
		total := int(num(res, "RootFolder", "TotalItemsInView"))
		if len(rows) == 0 {
			if offset < total {
				return nil, fmt.Errorf("owa: ListEvents: incomplete discovery page")
			}
			break
		}
		for start := 0; start < len(rows); start += 100 {
			var ids []string
			pending := map[string]bool{}
			for _, row := range rows[start:min(start+100, len(rows))] {
				id := str(row, "ItemId", "Id")
				if id == "" || pending[id] {
					return nil, fmt.Errorf("owa: ListEvents: missing or duplicate discovery id")
				}
				ids = append(ids, id)
				pending[id] = true
			}
			full, err := p.call(ctx, "GetItem", getItemReq(ids, shape))
			if err != nil {
				return nil, err
			}
			for _, message := range arr(full, "ResponseMessages", "Items") {
				code := str(message, "ResponseCode")
				if str(message, "ResponseClass") == "Error" || (code != "" && code != "NoError") {
					return nil, fmt.Errorf("owa: ListEvents: %s: %s", code, str(message, "MessageText"))
				}
			}
			for _, item := range allItems(full) {
				ev := eventFromItem(item, calendarID)
				if !pending[ev.ID] || ev.Start.IsZero() || ev.End.IsZero() {
					return nil, fmt.Errorf("owa: ListEvents: incomplete calendar item readback")
				}
				delete(pending, ev.ID)
				if !from.IsZero() && ev.End.Before(from) {
					continue
				}
				if !to.IsZero() && ev.Start.After(to) {
					continue
				}
				out = append(out, ev)
			}
			if len(pending) != 0 {
				return nil, fmt.Errorf("owa: ListEvents: missing calendar items in readback")
			}
		}
		next := int(num(res, "RootFolder", "IndexedPagingOffset"))
		if next == 0 {
			next = offset + len(rows)
		}
		if next <= offset {
			return nil, fmt.Errorf("owa: ListEvents: discovery paging made no progress")
		}
		if boolean(res, "RootFolder", "IncludesLastItemInRange") || (total > 0 && next >= total) || (total == 0 && len(rows) < 1000) {
			break
		}
		offset = next
	}
	return out, nil
}

// hdrWithTZ builds the request header with a TimeZoneContext, as
// the web client sends on calendar item creation.
func hdrWithTZ(windowsID string) map[string]any {
	if windowsID == "" {
		windowsID = "UTC"
	}
	h := hdr()
	h["TimeZoneContext"] = map[string]any{
		"__type":             "TimeZoneContext:#Exchange",
		"TimeZoneDefinition": map[string]any{"__type": "TimeZoneDefinitionType:#Exchange", "Id": windowsID},
	}
	return h
}

func (p *provider) CreateEvent(ctx context.Context, ev bridge.Event) (bridge.Event, error) {
	calID := orDefault(ev.CalendarID, "calendar")
	req := envelope("CreateItemJsonRequest:#Exchange", "CreateItemRequest:#Exchange", map[string]any{
		"Items": []any{eventToOWA(ev)}, "SavedItemFolderId": targetFolder(calID),
		"SendMeetingInvitations": "SendToNone",
	})
	req["Header"] = hdrWithTZ(bridge.ToWindowsZone(ev.TimeZone))
	res, err := p.call(ctx, "CreateItem", req)
	if err != nil {
		return bridge.Event{}, err
	}
	out := ev
	out.CalendarID = calID
	if item := firstItem(res); item != nil {
		out.ID = str(item, "ItemId", "Id")
		if u := str(item, "UID"); u != "" {
			out.UID = u
		}
		if out.UID == "" {
			out.UID = out.ID
		}
	}
	return out, nil
}

func setField(updates []any, typ, fieldURI string, item map[string]any) []any {
	return append(updates, map[string]any{"__type": "SetItemField:#Exchange", "Item": item, "Path": propURI(fieldURI)})
}

func (p *provider) UpdateEvent(ctx context.Context, ev bridge.Event) error {
	if ev.ID == "" {
		return fmt.Errorf("owa: UpdateEvent: missing event id")
	}
	var updates []any
	calItem := func(k string, v any) map[string]any {
		return map[string]any{"__type": "CalendarItem:#Exchange", k: v}
	}
	if ev.Summary != "" {
		updates = setField(updates, "CalendarItem", "Subject", calItem("Subject", ev.Summary))
	}
	if ev.Description != "" {
		updates = setField(updates, "CalendarItem", "Body", calItem("Body", map[string]any{"__type": "BodyContentType:#Exchange", "BodyType": "HTML", "Value": ev.Description}))
	}
	if !ev.Start.IsZero() {
		updates = setField(updates, "CalendarItem", "Start", calItem("Start", fmtZoned(ev.Start, ev.TimeZone)))
	}
	if !ev.End.IsZero() {
		updates = setField(updates, "CalendarItem", "End", calItem("End", fmtZoned(ev.End, ev.TimeZone)))
	}
	if ev.TimeZone != "" {
		win := bridge.ToWindowsZone(ev.TimeZone)
		updates = setField(updates, "CalendarItem", "StartTimeZone", calItem("StartTimeZone", map[string]any{"__type": "TimeZoneDefinitionType:#Exchange", "Id": win}))
		updates = setField(updates, "CalendarItem", "EndTimeZone", calItem("EndTimeZone", map[string]any{"__type": "TimeZoneDefinitionType:#Exchange", "Id": win}))
	}
	// Location FieldURI is EnhancedLocation per the reference field map.
	updates = setField(updates, "CalendarItem", "EnhancedLocation", calItem("Location", map[string]any{"__type": "EnhancedLocation:#Exchange", "DisplayName": ev.Location}))
	updates = setField(updates, "CalendarItem", "IsAllDayEvent", calItem("IsAllDayEvent", ev.AllDay))
	if len(updates) == 0 {
		return nil
	}
	req := envelope("UpdateItemJsonRequest:#Exchange", "UpdateItemRequest:#Exchange", map[string]any{
		"ItemChanges":        []any{map[string]any{"__type": "ItemChange:#Exchange", "ItemId": itemID(ev.ID), "Updates": updates}},
		"ConflictResolution": "AlwaysOverwrite", "MessageDisposition": "SaveOnly",
		"SendCalendarInvitationsOrCancellations": "SendToNone", "SuppressReadReceipts": true,
	})
	_, err := p.call(ctx, "UpdateItem", req)
	return err
}

func (p *provider) DeleteEvent(ctx context.Context, calendarID, id string) error {
	req := envelope("DeleteItemJsonRequest:#Exchange", "DeleteItemRequest:#Exchange", map[string]any{
		"ItemIds": itemIDs([]string{id}), "DeleteType": "MoveToDeletedItems",
		"SendMeetingCancellations": "SendToNone", "AffectedTaskOccurrences": "AllOccurrences", "SuppressReadRecipts": true,
	})
	_, err := p.call(ctx, "DeleteItem", req)
	return err
}

func (p *provider) RespondEvent(ctx context.Context, calendarID, id, response string) error {
	var typ string
	switch strings.ToLower(response) {
	case "accepted", "accept":
		typ = "AcceptItem"
	case "declined", "decline":
		typ = "DeclineItem"
	case "tentative":
		typ = "TentativelyAcceptItem"
	default:
		return notSupported("RespondEvent response " + response)
	}
	// _o RespondToInvitation uses CreateItem with <Response>:#Exchange
	// rather than RespondToMeetingMessage because it returns the event.
	req := envelope("CreateItemJsonRequest:#Exchange", "CreateItemRequest:#Exchange", map[string]any{
		"Items":              []any{map[string]any{"__type": typ + ":#Exchange", "ReferenceItemId": itemID(id)}},
		"MessageDisposition": "SendAndSaveCopy",
	})
	_, err := p.call(ctx, "CreateItem", req)
	return err
}

func (p *provider) FreeBusy(ctx context.Context, emails []string, from, to time.Time) (map[string][][2]time.Time, error) {
	out := map[string][][2]time.Time{}
	for _, email := range emails {
		req := map[string]any{"request": map[string]any{
			"__type": "GetUserAvailabilityInternalJsonRequest:#Exchange",
			"Header": map[string]any{"__type": "JsonRequestHeaders:#Exchange", "RequestServerVersion": "Exchange2013",
				"TimeZoneContext": map[string]any{"__type": "TimeZoneContext:#Exchange", "TimeZoneDefinition": map[string]any{"__type": "TimeZoneDefinitionType:#Exchange", "Id": "UTC"}}},
			"Body": map[string]any{"__type": "GetUserAvailabilityRequest:#Exchange",
				"MailboxDataArray": []any{map[string]any{"__type": "MailboxData:#Exchange", "Email": map[string]any{"__type": "EmailAddress:#Exchange", "Address": email}}},
				"FreeBusyViewOptions": map[string]any{"__type": "FreeBusyViewOptions:#Exchange",
					"TimeWindow":    map[string]any{"__type": "Duration:#Exchange", "StartTime": fmtTime(from), "EndTime": fmtTime(to)},
					"RequestedView": "FreeBusy"},
			},
		}}
		res, err := p.call(ctx, "GetUserAvailabilityInternal", req)
		if err != nil {
			return nil, err
		}
		var intervals [][2]time.Time
		responses := arr(res, "Responses")
		if len(responses) > 0 {
			for _, iv := range arr(responses[0], "CalendarView", "Items") {
				if strings.EqualFold(str(iv, "FreeBusyType"), "Free") {
					continue
				}
				intervals = append(intervals, [2]time.Time{parseTime(str(iv, "Start")), parseTime(str(iv, "End"))})
			}
		}
		out[email] = intervals
	}
	return out, nil
}

// --- tasks ---

func taskFromItem(item map[string]any, listID string) bridge.Task {
	t := bridge.Task{
		ID: str(item, "ItemId", "Id"), ListID: listID, Title: str(item, "Subject"),
		Body: bodyText(item), UID: str(item, "UID"),
	}
	if t.UID == "" {
		t.UID = t.ID
	}
	if s := str(item, "StartDate"); s != "" {
		if v := parseTime(s); !v.IsZero() {
			t.Start = &v
		}
	}
	if s := firstNonEmpty(str(item, "DueDate"), str(item, "TaskDueDate")); s != "" {
		if v := parseTime(s); !v.IsZero() {
			t.Due = &v
		}
	}
	if s := firstNonEmpty(str(item, "CompleteDate"), str(item, "TaskCompleteDate")); s != "" {
		if v := parseTime(s); !v.IsZero() {
			t.Completed = &v
		}
	}
	switch str(item, "Importance") {
	case "High":
		t.Priority = 1
	case "Low":
		t.Priority = 9
	default:
		t.Priority = 5
	}
	pct := num(item, "PercentComplete")
	switch {
	case t.Completed != nil || pct >= 100:
		t.Status = "completed"
	case pct > 0:
		t.Status = "in-process"
	default:
		t.Status = "needs-action"
	}
	if s := str(item, "ReminderDueBy"); s != "" && boolean(item, "ReminderIsSet") {
		if v := parseTime(s); !v.IsZero() {
			t.Reminder = &v
		}
	}
	t.Recur = recurrenceToRRULE(item)
	return t
}
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func taskToOWA(t bridge.Task) map[string]any {
	item := map[string]any{
		"__type": "Task:#Exchange", "Subject": t.Title,
		"Body": map[string]any{"__type": "BodyContentType:#Exchange", "BodyType": "HTML", "Value": t.Body},
	}
	if t.Due != nil {
		item["DueDate"] = fmtTime(*t.Due)
	}
	if t.Start != nil {
		item["StartDate"] = fmtTime(*t.Start)
	}
	if t.Completed != nil {
		item["CompleteDate"] = fmtTime(*t.Completed)
		item["PercentComplete"] = 100
	} else if t.Status == "completed" {
		item["PercentComplete"] = 100
	} else if t.Status == "in-process" {
		item["PercentComplete"] = 50
	}
	switch {
	case t.Priority >= 1 && t.Priority < 5:
		item["Importance"] = "High"
	case t.Priority > 5:
		item["Importance"] = "Low"
	default:
		item["Importance"] = "Normal"
	}
	if t.Reminder != nil {
		item["ReminderIsSet"] = true
		item["ReminderDueBy"] = fmtTime(*t.Reminder)
	}
	if rec := rruleToRecurrence(t.Recur, timeOrZero(t.Start)); rec != nil {
		item["Recurrence"] = rec
	}
	return item
}
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func (p *provider) ListTasks(ctx context.Context, listID string) ([]bridge.Task, error) {
	items, _, err := findItems(ctx, p, orDefault(listID, "tasks"), 0, 1000, nil)
	if err != nil {
		return nil, err
	}
	var out []bridge.Task
	for _, it := range items {
		out = append(out, taskFromItem(it, listID))
	}
	return out, nil
}

func (p *provider) CreateTask(ctx context.Context, t bridge.Task) (bridge.Task, error) {
	listID := orDefault(t.ListID, "tasks")
	req := envelope("CreateItemJsonRequest:#Exchange", "CreateItemRequest:#Exchange", map[string]any{
		"Items": []any{taskToOWA(t)}, "SavedItemFolderId": targetFolder(listID),
	})
	res, err := p.call(ctx, "CreateItem", req)
	if err != nil {
		return bridge.Task{}, err
	}
	out := t
	out.ListID = listID
	if item := firstItem(res); item != nil {
		out.ID = str(item, "ItemId", "Id")
	}
	if out.UID == "" {
		out.UID = out.ID
	}
	return out, nil
}

func (p *provider) UpdateTask(ctx context.Context, t bridge.Task) error {
	if t.ID == "" {
		return fmt.Errorf("owa: UpdateTask: missing task id")
	}
	var updates []any
	taskItem := func(k string, v any) map[string]any { return map[string]any{"__type": "Task:#Exchange", k: v} }
	if t.Title != "" {
		updates = setField(updates, "Task", "Subject", taskItem("Subject", t.Title))
	}
	if t.Body != "" {
		updates = setField(updates, "Task", "Body", taskItem("Body", map[string]any{"__type": "BodyContentType:#Exchange", "BodyType": "HTML", "Value": t.Body}))
	}
	if t.Due != nil {
		updates = setField(updates, "Task", "TaskDueDate", taskItem("DueDate", fmtTime(*t.Due)))
	}
	if t.Start != nil {
		updates = setField(updates, "Task", "TaskStartDate", taskItem("StartDate", fmtTime(*t.Start)))
	}
	if t.Completed != nil {
		updates = setField(updates, "Task", "TaskCompleteDate", taskItem("CompleteDate", fmtTime(*t.Completed)))
		updates = setField(updates, "Task", "TaskPercentComplete", taskItem("PercentComplete", 100))
	}
	if len(updates) == 0 {
		return nil
	}
	req := envelope("UpdateItemJsonRequest:#Exchange", "UpdateItemRequest:#Exchange", map[string]any{
		"ItemChanges":        []any{map[string]any{"__type": "ItemChange:#Exchange", "ItemId": itemID(t.ID), "Updates": updates}},
		"ConflictResolution": "AlwaysOverwrite", "MessageDisposition": "SaveOnly",
		"SendCalendarInvitationsOrCancellations": "SendToNone", "SuppressReadReceipts": true,
	})
	_, err := p.call(ctx, "UpdateItem", req)
	return err
}

func (p *provider) DeleteTask(ctx context.Context, listID, id string) error {
	req := envelope("DeleteItemJsonRequest:#Exchange", "DeleteItemRequest:#Exchange", map[string]any{
		"ItemIds": itemIDs([]string{id}), "DeleteType": "MoveToDeletedItems",
		"SendMeetingCancellations": "SendToNone", "AffectedTaskOccurrences": "AllOccurrences", "SuppressReadRecipts": true,
	})
	_, err := p.call(ctx, "DeleteItem", req)
	return err
}
