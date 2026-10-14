package owa

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// Calendar discovery falls back to the distinguished calendar folder.
// Tasks keep the stable distinguished collection in ListTaskLists below.
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
	// Preserve the established DAV collection URL independently of opaque folder IDs.
	// shortcut: only the default task list is exposed; custom lists need verified default-folder identity resolution.
	return []bridge.Calendar{{ID: "tasks", Name: "Tasks", TasksOnly: true}}, nil
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
	if categories := arr(item, "Categories"); categories != nil {
		ev.Categories = make([]string, 0, len(categories))
		for _, value := range categories {
			if category, ok := value.(string); ok {
				ev.Categories = append(ev.Categories, category)
			}
		}
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
	if ev.Categories != nil {
		item["Categories"] = ev.Categories
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
		"item:Subject", "item:Body", "item:TextBody", "item:Categories", "item:ReminderIsSet", "item:ReminderMinutesBeforeStart",
		"calendar:Start", "calendar:End", "calendar:UID", "calendar:IsAllDayEvent", "calendar:EnhancedLocation",
		"calendar:Organizer", "calendar:RequiredAttendees", "calendar:OptionalAttendees", "calendar:Recurrence",
		"calendar:StartTimeZone", "calendar:EndTimeZone",
	} {
		properties = append(properties, propURI(field))
	}
	shape := map[string]any{"__type": "ItemResponseShape:#Exchange", "BaseShape": "Default", "AdditionalProperties": properties}
	discovered, recurring := 0, 0
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
		discovered += len(rows)
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
				if ev.Recurrence != "" {
					recurring++
				}
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
	if event := p.log.Debug(); event.Enabled() {
		ids, uids := map[string]bool{}, map[string]bool{}
		for _, object := range out {
			ids[object.ID], uids[object.UID] = true, true
		}
		event.Str("channel", "owa").Str("op", "list_events").Str("account", p.email).
			Int("discovered", discovered).Int("returned", len(out)).Int("filtered", discovered-len(out)).
			Int("recurring", recurring).Int("unique_ids", len(ids)).Int("unique_uids", len(uids)).
			Int("duplicate_uids", len(out)-len(uids)).Msg("owa calendar listing")
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
	if ev.Categories != nil {
		updates = setField(updates, "CalendarItem", "Categories", calItem("Categories", ev.Categories))
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

const (
	taskUIDPropertyName           = "O365BridgeTaskUID"
	taskStartDateOnlyPropertyName = "O365BridgeTaskStartDateOnly"
	taskDueDateOnlyPropertyName   = "O365BridgeTaskDueDateOnly"
)

func taskNamedField(name string) map[string]any {
	return map[string]any{"DistinguishedPropertySetId": "PublicStrings", "PropertyName": name, "PropertyType": "String"}
}

func taskUIDField() map[string]any { return taskNamedField(taskUIDPropertyName) }

func taskNamedString(item map[string]any, name string) (string, bool) {
	for _, property := range arr(item, "ExtendedProperty") {
		field := dig(property, "ExtendedFieldURI")
		publicStrings := str(field, "DistinguishedPropertySetId") == "PublicStrings" || strings.EqualFold(str(field, "PropertySetId"), "00020329-0000-0000-C000-000000000046")
		if publicStrings && str(field, "PropertyName") == name && str(field, "PropertyType") == "String" {
			value, present := dig(property, "Value").(string)
			return value, present
		}
	}
	return "", false
}

func taskBodyText(item map[string]any) string {
	if value, present := dig(item, "TextBody", "Value").(string); present {
		return value
	}
	return bridge.PlainTextBody(str(item, "Body", "Value"), str(item, "Body", "BodyType"))
}

func taskFromItem(item map[string]any, listID string) bridge.Task {
	t := bridge.Task{
		ID: str(item, "ItemId", "Id"), ListID: listID, Title: str(item, "Subject"),
		Body: taskBodyText(item), Attachments: taskAttachmentsFromItem(item),
	}
	if categories := arr(item, "Categories"); categories != nil {
		t.Categories = make([]string, 0, len(categories))
		for _, value := range categories {
			if category, ok := value.(string); ok {
				t.Categories = append(t.Categories, category)
			}
		}
	}
	// Tasks have no native iCalendar UID; keep client identity in a named property.
	t.UID, _ = taskNamedString(item, taskUIDPropertyName)
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
	// A literal date is evidence of DATE precision; midnight is not. Companion
	// metadata preserves client DATE values after the service returns date-times.
	t.StartDateOnly = t.Start != nil && len(str(item, "StartDate")) == 10
	t.DueDateOnly = t.Due != nil && len(firstNonEmpty(str(item, "DueDate"), str(item, "TaskDueDate"))) == 10
	for _, precision := range []struct {
		name string
		flag *bool
	}{
		{taskStartDateOnlyPropertyName, &t.StartDateOnly}, {taskDueDateOnlyPropertyName, &t.DueDateOnly},
	} {
		if value, present := taskNamedString(item, precision.name); present {
			if parsed, err := strconv.ParseBool(value); err == nil {
				*precision.flag = parsed
			}
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
	case t.Completed != nil || pct >= 100 || str(item, "Status") == "Completed" || boolean(item, "IsComplete"):
		t.Status = "completed"
	case pct > 0 || str(item, "Status") == "InProgress":
		t.Status = "in-process"
	default:
		t.Status = "needs-action"
	}
	if s := str(item, "ReminderDueBy"); s != "" && boolean(item, "ReminderIsSet") {
		if v := parseTime(s); !v.IsZero() {
			t.Reminder = &v
		}
	}
	t.Recur, t.RecurStart, t.RecurTimeZone, t.RecurUnsupported = taskRecurrenceFromOWA(item)
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
		"Body": map[string]any{"__type": "BodyContentType:#Exchange", "BodyType": "Text", "Value": t.Body},
	}
	var properties []any
	if t.UID != "" {
		properties = append(properties, map[string]any{"ExtendedFieldURI": taskUIDField(), "Value": t.UID})
	}
	properties = append(properties,
		map[string]any{"ExtendedFieldURI": taskNamedField(taskStartDateOnlyPropertyName), "Value": strconv.FormatBool(t.StartDateOnly)},
		map[string]any{"ExtendedFieldURI": taskNamedField(taskDueDateOnlyPropertyName), "Value": strconv.FormatBool(t.DueDateOnly)},
	)
	item["ExtendedProperty"] = properties
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
	} else {
		item["PercentComplete"] = 0
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
	if t.Categories != nil {
		item["Categories"] = t.Categories
	}
	return item
}

func validateTaskReadDates(item map[string]any) error {
	for _, field := range []string{"StartDate", "DueDate", "TaskDueDate", "CompleteDate", "TaskCompleteDate", "ReminderDueBy"} {
		value, present := item[field]
		if !present || value == nil || value == "" {
			continue
		}
		text, isString := value.(string)
		if !isString || parseTime(text).IsZero() {
			return fmt.Errorf("owa: ListTasks: invalid native %s", field)
		}
	}
	return nil
}

func validateTaskWriteDates(t bridge.Task) error {
	for _, field := range []struct {
		name  string
		value *time.Time
	}{{"StartDate", t.Start}, {"DueDate", t.Due}, {"CompleteDate", t.Completed}, {"ReminderDueBy", t.Reminder}} {
		if field.value != nil && field.value.IsZero() {
			return fmt.Errorf("owa: invalid task %s", field.name)
		}
	}
	return nil
}

func taskRecurrenceForWrite(t bridge.Task) (map[string]any, error) {
	if t.RecurUnsupported {
		return nil, fmt.Errorf("owa: unsupported task recurrence cannot be changed")
	}
	var anchor time.Time
	for _, candidate := range []*time.Time{t.RecurStart, t.Start, t.Due} {
		if candidate != nil && !candidate.IsZero() {
			anchor = *candidate
			break
		}
	}
	return taskRecurrenceToOWA(t.Recur, anchor, t.RecurTimeZone, t.RecurStart, t.Start, t.Due)
}

func (p *provider) ListTasks(ctx context.Context, listID string) ([]bridge.Task, error) {
	listID = orDefault(listID, "tasks")
	var properties []any
	// GetItem property URIs differ from the translated UpdateItem field names.
	for _, field := range []string{
		"item:Subject", "item:Body", "item:TextBody", "item:Importance", "item:Categories", "item:Attachments", "item:HasAttachments", "task:DueDate", "task:StartDate", "task:CompleteDate",
		"task:PercentComplete", "task:Status", "task:IsComplete", "item:ReminderDueBy", "item:ReminderIsSet", "task:Recurrence",
	} {
		properties = append(properties, propURI(field))
	}
	for _, name := range []string{taskUIDPropertyName, taskStartDateOnlyPropertyName, taskDueDateOnlyPropertyName} {
		field := taskNamedField(name)
		field["__type"] = "ExtendedPropertyUri:#Exchange"
		properties = append(properties, field)
	}
	shape := map[string]any{"__type": "ItemResponseShape:#Exchange", "BaseShape": "Default", "AdditionalProperties": properties}
	var out []bridge.Task
	seen := map[string]bool{}
	for offset := 0; ; {
		req := findItemReq(listID, offset, 1000, nil)
		delete(asMap(req["Body"]), "ShapeName")
		res, err := p.call(ctx, "FindItem", req)
		if err != nil {
			return nil, err
		}
		if asMap(dig(res, "RootFolder")) == nil {
			return nil, fmt.Errorf("owa: ListTasks: missing discovery folder in response")
		}
		rows := arr(res, "RootFolder", "Items")
		total := int(num(res, "RootFolder", "TotalItemsInView"))
		if len(rows) == 0 {
			if offset < total {
				return nil, fmt.Errorf("owa: ListTasks: incomplete discovery page")
			}
			break
		}
		for start := 0; start < len(rows); start += 100 {
			var ids []string
			pending := map[string]bool{}
			for _, row := range rows[start:min(start+100, len(rows))] {
				id := str(row, "ItemId", "Id")
				if id == "" || seen[id] {
					return nil, fmt.Errorf("owa: ListTasks: missing or duplicate discovery id")
				}
				ids = append(ids, id)
				pending[id], seen[id] = true, true
			}
			full, err := p.call(ctx, "GetItem", getItemReq(ids, shape))
			if err != nil {
				return nil, err
			}
			for _, item := range allItems(full) {
				if err := validateTaskReadDates(item); err != nil {
					return nil, err
				}
				task := taskFromItem(item, listID)
				typ := str(item, "__type")
				if !pending[task.ID] || (typ != "" && typ != "Task:#Exchange") {
					return nil, fmt.Errorf("owa: ListTasks: incomplete task item readback")
				}
				delete(pending, task.ID)
				out = append(out, task)
			}
			if len(pending) != 0 {
				return nil, fmt.Errorf("owa: ListTasks: missing task items in readback")
			}
		}
		next := int(num(res, "RootFolder", "IndexedPagingOffset"))
		if next == 0 {
			next = offset + len(rows)
		}
		if next < offset+len(rows) {
			return nil, fmt.Errorf("owa: ListTasks: discovery paging made no progress")
		}
		if boolean(res, "RootFolder", "IncludesLastItemInRange") || (total > 0 && next >= total) || (total == 0 && len(rows) < 1000) {
			break
		}
		offset = next
	}
	p.log.Debug().Str("channel", "owa").Str("op", "list_tasks").Str("account", p.email).
		Int("discovered", len(seen)).Int("returned", len(out)).Msg("owa task listing")
	return out, nil
}

func (p *provider) CreateTask(ctx context.Context, t bridge.Task) (bridge.Task, error) {
	if err := validateTaskWriteDates(t); err != nil {
		return bridge.Task{}, err
	}
	recurrence, err := taskRecurrenceForWrite(t)
	if err != nil {
		return bridge.Task{}, err
	}
	listID := orDefault(t.ListID, "tasks")
	item := taskToOWA(t)
	if recurrence != nil {
		item["Recurrence"] = recurrence
	}
	req := envelope("CreateItemJsonRequest:#Exchange", "CreateItemRequest:#Exchange", map[string]any{
		"Items": []any{item}, "SavedItemFolderId": targetFolder(listID),
	})
	res, err := p.call(ctx, "CreateItem", req)
	if err != nil {
		return bridge.Task{}, err
	}
	out := t
	out.ID = ""
	out.ListID = listID
	if item := firstItem(res); item != nil {
		out.ID = str(item, "ItemId", "Id")
	}
	if out.ID == "" {
		return bridge.Task{}, fmt.Errorf("owa: CreateTask: missing task id in response")
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
	if err := validateTaskWriteDates(t); err != nil {
		return err
	}
	var recurrence map[string]any
	if t.RecurSet {
		var err error
		recurrence, err = taskRecurrenceForWrite(t)
		if err != nil {
			return err
		}
	}
	var updates []any
	item := taskToOWA(t)
	taskItem := func(k string, v any) map[string]any { return map[string]any{"__type": "Task:#Exchange", k: v} }
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"Subject", t.FullUpdate || t.Title != ""},
		{"Body", !t.BodyUnchanged && (t.FullUpdate || t.Body != "")},
		{"Importance", t.FullUpdate || t.Priority != 0},
		{"Categories", t.Categories != nil},
	} {
		if field.set {
			updates = setField(updates, "Task", field.name, taskItem(field.name, item[field.name]))
		}
	}
	for _, field := range []string{"DueDate", "StartDate", "CompleteDate"} {
		if field == "CompleteDate" && t.StatusUnchanged {
			continue
		}
		if value, exists := item[field]; exists {
			updates = setField(updates, "Task", "Task"+field, taskItem(field, value))
		} else if t.FullUpdate {
			updates = append(updates, map[string]any{"__type": "DeleteItemField:#Exchange", "Path": propURI("Task" + field)})
		}
	}
	// PercentComplete also changes native Status and clears completion on reopen.
	if !t.StatusUnchanged && (t.FullUpdate || t.Status != "" || t.Completed != nil) {
		updates = setField(updates, "Task", "TaskPercentComplete", taskItem("PercentComplete", item["PercentComplete"]))
	}
	if t.Reminder != nil {
		updates = setField(updates, "Task", "ReminderIsSet", taskItem("ReminderIsSet", true))
		updates = setField(updates, "Task", "ReminderDueBy", taskItem("ReminderDueBy", item["ReminderDueBy"]))
	} else if t.FullUpdate {
		updates = setField(updates, "Task", "ReminderIsSet", taskItem("ReminderIsSet", false))
	}
	if t.RecurSet {
		if recurrence != nil {
			updates = setField(updates, "Task", "Recurrence", taskItem("Recurrence", recurrence))
		} else {
			updates = append(updates, map[string]any{"__type": "DeleteItemField:#Exchange", "Path": propURI("Recurrence")})
		}
	}
	setNamedString := func(name, value string) {
		path := taskNamedField(name)
		path["__type"] = "ExtendedPropertyUri:#Exchange"
		property := map[string]any{"ExtendedFieldURI": taskNamedField(name), "Value": value}
		updates = append(updates, map[string]any{"__type": "SetItemField:#Exchange", "Path": path,
			"Item": taskItem("ExtendedProperty", []any{property})})
	}
	if t.UID != "" {
		setNamedString(taskUIDPropertyName, t.UID)
	}
	if t.FullUpdate || t.Start != nil {
		setNamedString(taskStartDateOnlyPropertyName, strconv.FormatBool(t.StartDateOnly))
	}
	if t.FullUpdate || t.Due != nil {
		setNamedString(taskDueDateOnlyPropertyName, strconv.FormatBool(t.DueDateOnly))
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
	if id == "" {
		return fmt.Errorf("owa: DeleteTask: missing task id")
	}
	req := envelope("DeleteItemJsonRequest:#Exchange", "DeleteItemRequest:#Exchange", map[string]any{
		"ItemIds": itemIDs([]string{id}), "DeleteType": "MoveToDeletedItems",
		"SendMeetingCancellations": "SendToNone", "AffectedTaskOccurrences": "AllOccurrences", "SuppressReadReceipts": true,
	})
	_, err := p.call(ctx, "DeleteItem", req)
	return err
}
