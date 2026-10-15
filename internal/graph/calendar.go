package graph

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type graphCalendar struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (p *provider) ListCalendars(ctx context.Context) ([]bridge.Calendar, error) {
	items, _, err := getAll[graphCalendar](ctx, p, "/me/calendars", 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]bridge.Calendar, 0, len(items))
	for _, it := range items {
		out = append(out, bridge.Calendar{ID: it.ID, Name: it.Name})
	}
	return out, nil
}

// --- Events ---

type graphEvent struct {
	ID         string   `json:"id"`
	ICalUID    string   `json:"iCalUId"`
	Type       string   `json:"type"`
	Subject    string   `json:"subject"`
	Categories []string `json:"categories"`
	Body       struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Location struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Start     dateTimeTZ `json:"start"`
	End       dateTimeTZ `json:"end"`
	IsAllDay  bool       `json:"isAllDay"`
	Organizer struct {
		EmailAddress emailAddress `json:"emailAddress"`
	} `json:"organizer"`
	Attendees []struct {
		Type   string `json:"type"`
		Status struct {
			Response string `json:"response"`
		} `json:"status"`
		EmailAddress emailAddress `json:"emailAddress"`
	} `json:"attendees"`
	ReminderMinutes *int                  `json:"reminderMinutesBeforeStart"`
	IsReminderOn    bool                  `json:"isReminderOn"`
	Recurrence      *graphEventRecurrence `json:"recurrence"`
}

func (g graphEvent) toEvent(calendarID string) bridge.Event {
	ev := bridge.Event{
		ID: g.ID, CalendarID: calendarID, UID: g.ICalUID, Summary: g.Subject,
		Description: g.Body.Content, Location: g.Location.DisplayName, Categories: g.Categories,
		Start: fromGraphDT(g.Start), End: fromGraphDT(g.End), AllDay: g.IsAllDay,
		Organizer: g.Organizer.EmailAddress.Address,
	}
	if g.Start.TimeZone != "" && g.Start.TimeZone != "UTC" {
		ev.TimeZone = bridge.FromWindowsZone(g.Start.TimeZone)
	}
	for _, a := range g.Attendees {
		status := a.Status.Response
		// Graph uses "none"/"notResponded"; neutral form uses needs-action.
		if status == "none" || status == "notResponded" {
			status = "needs-action"
		}
		if status == "tentativelyAccepted" {
			status = "tentative"
		}
		ev.Attendees = append(ev.Attendees, bridge.Attendee{Email: a.EmailAddress.Address, Name: a.EmailAddress.Name, Status: status})
	}
	if g.IsReminderOn && g.ReminderMinutes != nil {
		v := *g.ReminderMinutes
		ev.ReminderMin = &v
	}
	if g.Recurrence != nil {
		ev.Recurrence, ev.TimeZone, ev.RecurrenceUnsupported = g.Recurrence.eventRepeat(ev)
	}
	return ev
}

func eventJSON(ev bridge.Event, update bool) (map[string]any, error) {
	m := map[string]any{
		"subject":  ev.Summary,
		"start":    toGraphDT(ev.Start, ev.TimeZone),
		"end":      toGraphDT(ev.End, ev.TimeZone),
		"isAllDay": ev.AllDay,
	}
	if ev.Categories != nil {
		m["categories"] = ev.Categories
	}
	if ev.Description != "" {
		m["body"] = map[string]any{"contentType": "Text", "content": ev.Description}
	}
	if ev.Location != "" {
		m["location"] = map[string]any{"displayName": ev.Location}
	}
	if len(ev.Attendees) > 0 {
		var atts []map[string]any
		for _, a := range ev.Attendees {
			atts = append(atts, map[string]any{"emailAddress": map[string]any{"address": a.Email, "name": a.Name}, "type": "required"})
		}
		m["attendees"] = atts
	}
	if ev.ReminderMin != nil {
		m["reminderMinutesBeforeStart"] = *ev.ReminderMin
		m["isReminderOn"] = true
	} else {
		m["isReminderOn"] = false
	}
	if !update || ev.RecurrenceSet {
		if ev.RecurrenceUnsupported && (!update || ev.Recurrence != "") {
			return nil, bridge.Wrap(bridge.KindOther, "graph: unsupported event recurrence", errors.ErrUnsupported)
		}
		recurrence, err := eventRecurrenceJSON(ev)
		if err != nil {
			return nil, bridge.Wrap(bridge.KindOther, "graph: invalid event recurrence", err)
		}
		if recurrence != nil {
			m["recurrence"] = recurrence
		} else if update {
			m["recurrence"] = nil
		}
	}
	return m, nil
}

func (p *provider) ListEvents(ctx context.Context, calendarID string, from, to time.Time) ([]bridge.Event, error) {
	q := url.Values{}
	q.Set("$orderby", "start/dateTime")
	base := "/me/events"
	if calendarID != "" {
		base = "/me/calendars/" + esc(calendarID) + "/events"
	}
	items, _, err := getAll[graphEvent](ctx, p, base+"?"+q.Encode(), 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]bridge.Event, 0, len(items))
	for _, it := range items {
		ev := it.toEvent(calendarID)
		// shortcut: include masters conservatively; exact window pruning needs recurrence expansion.
		if it.Type != "seriesMaster" && it.Recurrence == nil {
			if (!from.IsZero() && !ev.End.After(from)) || (!to.IsZero() && !ev.Start.Before(to)) {
				continue
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

func (p *provider) CreateEvent(ctx context.Context, ev bridge.Event) (bridge.Event, error) {
	path := "/me/events"
	if ev.CalendarID != "" {
		path = "/me/calendars/" + esc(ev.CalendarID) + "/events"
	}
	body, err := eventJSON(ev, false)
	if err != nil {
		return bridge.Event{}, err
	}
	var response json.RawMessage
	if err := p.doJSON(ctx, http.MethodPost, path, body, &response); err != nil {
		return bridge.Event{}, err
	}
	var ge graphEvent
	if err := json.Unmarshal(response, &ge); err != nil {
		return bridge.Event{}, err
	}
	if strings.TrimSpace(ge.ID) == "" {
		return bridge.Event{}, bridge.Wrap(bridge.KindOther, "graph: create event response missing id", nil)
	}
	out := ge.toEvent(ev.CalendarID)
	// A sparse create response may omit accepted fields, but never replace its real identity.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response, &fields); err != nil {
		return bridge.Event{}, err
	}
	if _, ok := fields["iCalUId"]; !ok {
		out.UID = ev.UID
	}
	if _, ok := fields["subject"]; !ok {
		out.Summary = ev.Summary
	}
	if _, ok := fields["body"]; !ok {
		out.Description = ev.Description
	}
	if _, ok := fields["location"]; !ok {
		out.Location = ev.Location
	}
	if _, ok := fields["categories"]; !ok {
		out.Categories = ev.Categories
	}
	if _, ok := fields["start"]; !ok {
		out.Start, out.TimeZone = ev.Start, ev.TimeZone
	}
	if _, ok := fields["end"]; !ok {
		out.End = ev.End
	}
	if _, ok := fields["isAllDay"]; !ok {
		out.AllDay = ev.AllDay
	}
	if _, ok := fields["attendees"]; !ok {
		out.Attendees = ev.Attendees
	}
	if _, ok := fields["organizer"]; !ok {
		out.Organizer = ev.Organizer
	}
	if _, ok := fields["isReminderOn"]; !ok {
		out.ReminderMin = ev.ReminderMin
	}
	if _, ok := fields["recurrence"]; !ok {
		out.Recurrence = ev.Recurrence
		if ev.TimeZone != "" {
			out.TimeZone = ev.TimeZone
		}
	} else if ge.Recurrence != nil {
		out.Recurrence, out.TimeZone, out.RecurrenceUnsupported = ge.Recurrence.eventRepeat(out)
	}
	return out, nil
}

func (p *provider) UpdateEvent(ctx context.Context, ev bridge.Event) error {
	body, err := eventJSON(ev, true)
	if err != nil {
		return err
	}
	return p.doJSON(ctx, http.MethodPatch, "/me/events/"+esc(ev.ID), body, nil)
}

func (p *provider) DeleteEvent(ctx context.Context, calendarID, id string) error {
	_, err := p.do(ctx, http.MethodDelete, "/me/events/"+esc(id), nil, nil)
	return err
}

func (p *provider) RespondEvent(ctx context.Context, calendarID, id, response string) error {
	var verb string
	switch strings.ToLower(response) {
	case "accepted", "accept":
		verb = "accept"
	case "declined", "decline":
		verb = "decline"
	case "tentative", "tentativelyaccepted", "tentativelyaccept":
		verb = "tentativelyAccept"
	default:
		verb = "accept"
	}
	return p.doJSON(ctx, http.MethodPost, "/me/events/"+esc(id)+"/"+verb, map[string]any{"sendResponse": true}, nil)
}

func (p *provider) FreeBusy(ctx context.Context, emails []string, from, to time.Time) (map[string][][2]time.Time, error) {
	body := map[string]any{
		"schedules":                emails,
		"startTime":                toGraphDT(from, "UTC"),
		"endTime":                  toGraphDT(to, "UTC"),
		"availabilityViewInterval": 60,
	}
	var resp struct {
		Value []struct {
			ScheduleID    string `json:"scheduleId"`
			ScheduleItems []struct {
				Status string     `json:"status"`
				Start  dateTimeTZ `json:"start"`
				End    dateTimeTZ `json:"end"`
			} `json:"scheduleItems"`
		} `json:"value"`
	}
	if err := p.doJSON(ctx, http.MethodPost, "/me/calendar/getSchedule", body, &resp); err != nil {
		return nil, err
	}
	out := make(map[string][][2]time.Time, len(emails))
	for _, e := range emails {
		out[e] = nil
	}
	for _, v := range resp.Value {
		for _, item := range v.ScheduleItems {
			if strings.EqualFold(item.Status, "free") || item.Status == "" {
				continue
			}
			out[v.ScheduleID] = append(out[v.ScheduleID], [2]time.Time{fromGraphDT(item.Start), fromGraphDT(item.End)})
		}
	}
	return out, nil
}
