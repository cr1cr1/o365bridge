// Package calendars implements the data types of JMAP Calendars
// (IETF jmap working group specification; types follow the
// JSCalendar data model, RFC 8984) on top of the generic datatype
// machinery.
//
// Calendar and CalendarEvent are ordinary stores: Register wires
// them through datatype.Register, which provides get/set/query (and
// changes/queryChanges when the store also implements
// datatype.ChangeStore). ParticipantIdentity is registered the same
// way when the backend implements IdentityBackend. CalendarEvent /
// getAvailability and CalendarEventNotification/get|changes are
// registered only when the backend implements AvailabilitySource /
// NotificationBackend respectively; notifications are backend-
// generated and therefore read-only (no set/query methods).
//
// Capability: datatype.Register advertises the capability URI
// "urn:ietf:params:jmap:calendars" (jmap.CalendarsURI) with an empty
// capability object.
//
// CalendarEvent/getAvailability follows the Calendars
// specification's free/busy method as implemented here; it is
// backend-computed. Availability responses carry busy periods only
// (busyType/start/end) and never event titles.
//
// shortcut: unknown JSCalendar/CalendarEvent properties are dropped
// on decode; only the pragmatic faithful core modelled below
// round-trips. Backends needing extension properties must carry them
// outside this type.
package calendars

import (
	"context"
	"encoding/json"
	"errors"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
	"github.com/cr1cr1/o365bridge/jmap/sharing"
)

// Capability is the JMAP Calendars capability URI
// ("urn:ietf:params:jmap:calendars", the root jmap.CalendarsURI).
const Capability = jmap.CalendarsURI

// --- Calendar ---

// CalendarRights is the myRights object of a Calendar.
type CalendarRights struct {
	MayReadFreeBusy  bool `json:"mayReadFreeBusy"`
	MayReadItems     bool `json:"mayReadItems"`
	MayAddItems      bool `json:"mayAddItems"`
	MayUpdatePrivate bool `json:"mayUpdatePrivate"`
	MayUpdateAll     bool `json:"mayUpdateAll"`
	MayRemoveItems   bool `json:"mayRemoveItems"`
	MayDelete        bool `json:"mayDelete"`
	MayRename        bool `json:"mayRename"`
	MayShare         bool `json:"mayShare"`
}

// Calendar is a calendar (collection of events). ShareWith is null
// (a nil map) when the calendar is not shared.
type Calendar struct {
	ID           jmap.ID            `json:"id,omitempty"`
	Name         string             `json:"name"`
	Description  *string            `json:"description"`
	Color        *string            `json:"color"`
	SortOrder    uint               `json:"sortOrder"`
	IsDefault    bool               `json:"isDefault"`
	IsSubscribed bool               `json:"isSubscribed"`
	ShareWith    sharing.SharedWith `json:"shareWith"`
	MyRights     *CalendarRights    `json:"myRights"`
}

// --- JSCalendar building blocks (RFC 8984) ---

// Participant is one participant of an event (RFC 8984 §5.1-ish).
// Roles maps role names (e.g. "owner", "attendee", "chair") to true.
type Participant struct {
	Name                *string         `json:"name"`
	Email               string          `json:"email"`
	Kind                string          `json:"kind"` // individual|group|resource|location|unknown
	Roles               map[string]bool `json:"roles"`
	ParticipationStatus string          `json:"participationStatus"` // needs-action|accepted|declined|tentative|delegated|completed|in-process
	ExpectReply         bool            `json:"expectReply"`
	ScheduleAgent       string          `json:"scheduleAgent,omitempty"`
	LocationID          string          `json:"locationId,omitempty"`
	Language            *string         `json:"language"`
}

// Location is one location of an event (RFC 8984, simplified).
type Location struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Coordinates *string `json:"coordinates"`
}

// NDay is one entry of a RecurrenceRule byDay list (RFC 8984).
type NDay struct {
	Day         string `json:"day"` // mo|tu|we|th|fr|sa|su
	NthOfPeriod *int   `json:"nthOfPeriod,omitempty"`
}

// RecurrenceRule is a JSCalendar recurrence rule (RFC 8984 §4.3).
// Until is a LocalDateTime string ("2006-01-02T15:04:05").
type RecurrenceRule struct {
	Frequency      string   `json:"frequency"` // yearly|monthly|weekly|daily|hourly|minutely|secondly
	Interval       uint     `json:"interval,omitempty"`
	Count          *uint    `json:"count,omitempty"`
	Until          *string  `json:"until,omitempty"`
	RScale         string   `json:"rscale,omitempty"`
	Skip           string   `json:"skip,omitempty"` // omit|backward|forward
	FirstDayOfWeek string   `json:"firstDayOfWeek,omitempty"`
	ByDay          []NDay   `json:"byDay,omitempty"`
	ByMonthDay     []int    `json:"byMonthDay,omitempty"`
	ByMonth        []string `json:"byMonth,omitempty"`
	ByYearDay      []int    `json:"byYearDay,omitempty"`
	ByWeekNo       []int    `json:"byWeekNo,omitempty"`
	ByHour         []uint   `json:"byHour,omitempty"`
	ByMinute       []uint   `json:"byMinute,omitempty"`
	BySecond       []uint   `json:"bySecond,omitempty"`
	BySetPosition  []int    `json:"bySetPosition,omitempty"`
}

// Alert is a JSCalendar alert (RFC 8984 §4.5). Type is
// "OffsetTrigger" (Offset/RelativeTo set) or "AbsoluteTrigger"
// (When, a UTCDateTime, set).
type Alert struct {
	Type       string `json:"@type"`
	Offset     string `json:"offset,omitempty"`
	RelativeTo string `json:"relativeTo,omitempty"`
	When       string `json:"when,omitempty"`
	Action     string `json:"action,omitempty"` // display|audio|email
}

// RecurrenceOverride is the subset of event properties that may
// override one recurrence instance (keyed by recurrence id).
type RecurrenceOverride struct {
	Title          *string                 `json:"title,omitempty"`
	Description    *string                 `json:"description,omitempty"`
	Start          *string                 `json:"start,omitempty"`
	Duration       *string                 `json:"duration,omitempty"`
	TimeZone       *string                 `json:"timeZone,omitempty"`
	FreeBusyStatus *string                 `json:"freeBusyStatus,omitempty"`
	Status         *string                 `json:"status,omitempty"`
	Participants   map[string]*Participant `json:"participants,omitempty"`
}

// ReplyTo kept minimal: JSCalendar models the organizer through a
// participant with the "owner" role, so no separate field is kept.

// CalendarEvent is a JSCalendar Event (RFC 8984 §5) plus the JMAP
// Calendars calendarIds property. Start is a LocalDateTime string;
// TimeZone is an IANA time zone id (null for floating/all-day as
// modelled by the backend); Duration is an ISO 8601 duration.
type CalendarEvent struct {
	Type                    string                         `json:"@type"`
	ID                      jmap.ID                        `json:"id,omitempty"`
	CalendarIDs             map[jmap.ID]bool               `json:"calendarIds"`
	UID                     string                         `json:"uid"`
	Title                   string                         `json:"title"`
	Description             string                         `json:"description"`
	DescriptionContentType  string                         `json:"descriptionContentType,omitempty"`
	Start                   string                         `json:"start"` // LocalDateTime
	TimeZone                *string                        `json:"timeZone"`
	Duration                string                         `json:"duration"` // ISO 8601 duration, e.g. PT1H
	AllDay                  bool                           `json:"allDay"`
	Status                  string                         `json:"status"`         // confirmed|cancelled|tentative
	FreeBusyStatus          string                         `json:"freeBusyStatus"` // busy|free
	ShowWithoutTime         bool                           `json:"showWithoutTime,omitempty"`
	Keywords                map[string]bool                `json:"keywords"`
	Color                   *string                        `json:"color"`
	RecurrenceRule          *RecurrenceRule                `json:"recurrenceRule"`
	RecurrenceOverrides     map[string]*RecurrenceOverride `json:"recurrenceOverrides"`
	ExcludedRecurrenceRules []RecurrenceRule               `json:"excludedRecurrenceRules,omitempty"`
	Participants            map[string]*Participant        `json:"participants"`
	Locations               map[string]*Location           `json:"locations"`
	Alerts                  map[string]*Alert              `json:"alerts"`
	UseDefaultAlerts        bool                           `json:"useDefaultAlerts"`
	Method                  string                         `json:"method,omitempty"`
	ProdID                  string                         `json:"prodId,omitempty"`
	Created                 string                         `json:"created,omitempty"`
	Updated                 string                         `json:"updated,omitempty"`
	Sequence                uint                           `json:"sequence"`
	Locale                  string                         `json:"locale,omitempty"`
}

// MarshalJSON defaults @type to "Event" when unset.
func (e CalendarEvent) MarshalJSON() ([]byte, error) {
	type alias CalendarEvent
	a := alias(e)
	if a.Type == "" {
		a.Type = "Event"
	}
	return json.Marshal(a)
}

// UnmarshalJSON accepts both the singular "recurrenceRule" object
// used here and JSCalendar's "recurrenceRules" array (first rule
// wins), and defaults @type to "Event".
func (e *CalendarEvent) UnmarshalJSON(data []byte) error {
	type alias CalendarEvent
	var aux struct {
		alias
		RecurrenceRules []*RecurrenceRule `json:"recurrenceRules"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*e = CalendarEvent(aux.alias)
	if e.RecurrenceRule == nil && len(aux.RecurrenceRules) > 0 {
		e.RecurrenceRule = aux.RecurrenceRules[0]
	}
	if e.Type == "" {
		e.Type = "Event"
	}
	return nil
}

// --- CalendarEventNotification ---

// ChangedBy identifies who made the change a notification reports.
type ChangedBy struct {
	Name           *string  `json:"name"`
	Email          *string  `json:"email"`
	CalendarUserID *jmap.ID `json:"calendarUserId,omitempty"`
}

// CalendarEventNotification is a backend-generated notification of
// a created/updated/destroyed calendar event. It is read-only.
type CalendarEventNotification struct {
	ID        jmap.ID    `json:"id,omitempty"`
	Created   string     `json:"created"`
	ChangedBy *ChangedBy `json:"changedBy"`
	EventID   jmap.ID    `json:"eventId"`
	IsDraft   bool       `json:"isDraft"`
	Type      string     `json:"type"` // created|updated|destroyed
}

// --- ParticipantIdentity ---

// ParticipantIdentity is a scheduling identity (an address the user
// may be invited as / organise events as).
type ParticipantIdentity struct {
	ID         jmap.ID  `json:"id,omitempty"`
	Name       *string  `json:"name"`
	Email      string   `json:"email"`
	CalendarID *jmap.ID `json:"calendarId"` // null when not tied to one calendar
}

// --- Backend contracts ---

// Backend supplies the stores behind Register.
type Backend interface {
	Calendars() datatype.Store[Calendar]
	Events() datatype.Store[CalendarEvent]
}

// IdentityBackend is the optional contract behind
// ParticipantIdentity/get|set|query (+changes when the store is a
// datatype.ChangeStore).
type IdentityBackend interface {
	ParticipantIdentities() datatype.Store[ParticipantIdentity]
}

// BusyPeriod is one busy span in an availability response. It
// deliberately carries no title or event details.
type BusyPeriod struct {
	BusyType string `json:"busyType"` // busy|tentative|...
	Start    string `json:"start"`
	End      string `json:"end"`
}

// AvailabilitySource is the optional backend-computed contract
// behind CalendarEvent/getAvailability. Backends implement it on
// the Backend value itself.
type AvailabilitySource interface {
	Availability(ctx context.Context, account jmap.ID, attendees []string, start, end string) (map[string][]BusyPeriod, error)
}

// NotificationStore is the read-only backend contract behind
// CalendarEventNotification/get|changes. Get mirrors
// datatype.Store.Get plus the current state string; Changes follows
// datatype.ChangeStore semantics (return datatype.ErrCannotCalculate,
// wrapped, when changes since sinceState cannot be listed).
type NotificationStore interface {
	Get(ctx context.Context, account jmap.ID, ids []jmap.ID) (found []CalendarEventNotification, notFound []jmap.ID, state string, err error)
	Changes(ctx context.Context, account jmap.ID, sinceState string, maxChanges int) (created, updated, destroyed []jmap.ID, newState string, hasMore bool, err error)
}

// NotificationBackend is the optional contract behind
// CalendarEventNotification/get|changes.
type NotificationBackend interface {
	Notifications() NotificationStore
}

// Register registers Calendar and CalendarEvent methods on e under
// the capability "urn:ietf:params:jmap:calendars"
// (jmap.CalendarsURI, advertised by datatype.Register with an empty
// capability object), plus the optional ParticipantIdentity,
// CalendarEvent/getAvailability and CalendarEventNotification
// methods when the backend implements the corresponding optional
// interface.
func Register(e *server.Engine, b Backend) {
	datatype.Register[Calendar](e, datatype.Options[Calendar]{
		Name:       "Calendar",
		Capability: Capability,
		Store:      b.Calendars(),
		IDOf:       func(c Calendar) jmap.ID { return c.ID },
		Match:      calendarMatch,
		Compare:    calendarCompare,
	})
	datatype.Register[CalendarEvent](e, datatype.Options[CalendarEvent]{
		Name:       "CalendarEvent",
		Capability: Capability,
		Store:      b.Events(),
		IDOf:       func(ev CalendarEvent) jmap.ID { return ev.ID },
		Match:      eventMatch,
		Compare:    eventCompare,
	})
	if ib, ok := b.(IdentityBackend); ok && ib.ParticipantIdentities() != nil {
		datatype.Register[ParticipantIdentity](e, datatype.Options[ParticipantIdentity]{
			Name:       "ParticipantIdentity",
			Capability: Capability,
			Store:      ib.ParticipantIdentities(),
			IDOf:       func(p ParticipantIdentity) jmap.ID { return p.ID },
			Match:      identityMatch,
			Compare:    identityCompare,
		})
	}
	if src, ok := b.(AvailabilitySource); ok {
		e.RegisterMethod("CalendarEvent/getAvailability", Capability, func(ctx context.Context, call *server.Call) (any, error) {
			return getAvailability(ctx, call, src)
		})
	}
	if nb, ok := b.(NotificationBackend); ok && nb.Notifications() != nil {
		ns := nb.Notifications()
		e.RegisterMethod("CalendarEventNotification/get", Capability, func(ctx context.Context, call *server.Call) (any, error) {
			return notificationGet(ctx, call, ns)
		})
		e.RegisterMethod("CalendarEventNotification/changes", Capability, func(ctx context.Context, call *server.Call) (any, error) {
			return notificationChanges(ctx, call, ns)
		})
	}
}

// --- CalendarEvent/getAvailability ---

type availabilityArgs struct {
	AccountID jmap.ID  `json:"accountId"`
	Attendees []string `json:"attendees"`
	Start     string   `json:"start"`
	End       string   `json:"end"`
	BusyOnly  bool     `json:"busyOnly,omitempty"`
}

type availabilityResponse struct {
	AccountID    jmap.ID                 `json:"accountId"`
	Availability map[string][]BusyPeriod `json:"availability"`
}

func getAvailability(ctx context.Context, call *server.Call, src AvailabilitySource) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args availabilityArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	if len(args.Attendees) == 0 || args.Start == "" || args.End == "" {
		return nil, jmap.NewInvalidArguments("getAvailability requires attendees, start and end")
	}
	avail, err := src.Availability(ctx, account, args.Attendees, args.Start, args.End)
	if err != nil {
		return nil, err
	}
	if avail == nil {
		avail = map[string][]BusyPeriod{}
	}
	for k, v := range avail {
		if v == nil {
			avail[k] = []BusyPeriod{}
		}
	}
	return availabilityResponse{AccountID: account, Availability: avail}, nil
}

// --- CalendarEventNotification/get|changes (manual, read-only) ---

type notificationGetResponse struct {
	AccountID jmap.ID           `json:"accountId"`
	State     string            `json:"state"`
	List      []json.RawMessage `json:"list"`
	NotFound  []jmap.ID         `json:"notFound"`
}

func notificationGet(ctx context.Context, call *server.Call, ns NotificationStore) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args jmap.GetArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	found, notFound, state, err := ns.Get(ctx, account, args.IDs)
	if err != nil {
		return nil, err
	}
	resp := notificationGetResponse{
		AccountID: account,
		State:     state,
		List:      make([]json.RawMessage, 0, len(found)),
		NotFound:  notFound,
	}
	if resp.NotFound == nil {
		resp.NotFound = []jmap.ID{}
	}
	for _, n := range found {
		raw, err := projectNotification(n, args.Properties)
		if err != nil {
			return nil, err
		}
		resp.List = append(resp.List, raw)
	}
	return resp, nil
}

// projectNotification mirrors datatype's /get projection.
// shortcut: unknown requested properties are silently dropped.
func projectNotification(n CalendarEventNotification, properties []string) (json.RawMessage, error) {
	raw, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	if properties == nil {
		return raw, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	keep := make(map[string]json.RawMessage, len(properties)+1)
	if id, ok := m["id"]; ok {
		keep["id"] = id
	}
	for _, p := range properties {
		if v, ok := m[p]; ok {
			keep[p] = v
		}
	}
	return json.Marshal(keep)
}

func notificationChanges(ctx context.Context, call *server.Call, ns NotificationStore) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args jmap.ChangesArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	maxChanges := 0
	if args.MaxChanges != nil {
		maxChanges = int(*args.MaxChanges)
	}
	created, updated, destroyed, newState, hasMore, err := ns.Changes(ctx, account, args.SinceState, maxChanges)
	if err != nil {
		if errors.Is(err, datatype.ErrCannotCalculate) {
			return nil, jmap.NewCannotCalculateChanges(err.Error())
		}
		return nil, err
	}
	nonNil := func(ids []jmap.ID) []jmap.ID {
		if ids == nil {
			return []jmap.ID{}
		}
		return ids
	}
	return jmap.ChangesResponse{
		AccountID:      account,
		OldState:       args.SinceState,
		NewState:       newState,
		HasMoreChanges: hasMore,
		Created:        nonNil(created),
		Updated:        nonNil(updated),
		Destroyed:      nonNil(destroyed),
	}, nil
}
