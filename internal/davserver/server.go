// Package davserver is a downstream CalDAV/CardDAV server. Local
// clients use HTTP Basic auth (account email + bridge instance
// password); all upstream access goes through bridge.Registry.Call
// so auth renewal/fallback/hold applies. Credentials and bodies are
// never logged.
package davserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

const contactsBookID = "contacts"

var wideFrom = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
var wideTo = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

type handler struct {
	reg      *bridge.Registry
	password string
	logger   zerolog.Logger
}

// New returns the DAV http.Handler.
func New(reg *bridge.Registry, instancePassword string, logger zerolog.Logger) http.Handler {
	return &handler{reg: reg, password: instancePassword, logger: logger}
}

func (h *handler) call(ctx context.Context, email string, op func(context.Context, bridge.Provider) error) error {
	return h.reg.Call(ctx, email, op)
}

// --- XML (DAV multistatus) types, marshalled with encoding/xml ---

type xMultistatus struct {
	XMLName   xml.Name    `xml:"D:multistatus"`
	XmlnsD    string      `xml:"xmlns:D,attr"`
	XmlnsC    string      `xml:"xmlns:C,attr"`
	XmlnsA    string      `xml:"xmlns:A,attr"`
	Responses []xResponse `xml:"D:response"`
}
type xResponse struct {
	Href     string    `xml:"D:href"`
	Propstat xPropstat `xml:"D:propstat"`
}
type xPropstat struct {
	Prop   xProp  `xml:"D:prop"`
	Status string `xml:"D:status"`
}
type xHref struct {
	Href string `xml:"D:href"`
}
type xResourcetype struct {
	Collection  *struct{} `xml:"D:collection"`
	Calendar    *struct{} `xml:"C:calendar"`
	Addressbook *struct{} `xml:"A:addressbook"`
	Principal   *struct{} `xml:"D:principal"`
}
type xComp struct {
	Name string `xml:"name,attr"`
}
type xSupported struct {
	Comps []xComp `xml:"C:comp"`
}
type xProp struct {
	Displayname  string         `xml:"D:displayname,omitempty"`
	Resourcetype *xResourcetype `xml:"D:resourcetype,omitempty"`
	CurrentUser  *xHref         `xml:"D:current-user-principal,omitempty"`
	CalHome      *xHref         `xml:"C:calendar-home-set,omitempty"`
	CardHome     *xHref         `xml:"A:addressbook-home-set,omitempty"`
	Supported    *xSupported    `xml:"C:supported-calendar-component-set,omitempty"`
	Getetag      string         `xml:"D:getetag,omitempty"`
	Getct        string         `xml:"D:getcontenttype,omitempty"`
	CalData      string         `xml:"C:calendar-data,omitempty"`
	AddrData     string         `xml:"A:address-data,omitempty"`
}

func empty() *struct{} { return &struct{}{} }

func resp(href string, p xProp) xResponse {
	return xResponse{Href: href, Propstat: xPropstat{Prop: p, Status: "HTTP/1.1 200 OK"}}
}

func (h *handler) writeMultistatus(w http.ResponseWriter, resps []xResponse) {
	out, err := xml.Marshal(xMultistatus{
		XmlnsD: "DAV:", XmlnsC: "urn:ietf:params:xml:ns:caldav", XmlnsA: "urn:ietf:params:xml:ns:carddav",
		Responses: resps,
	})
	if err != nil {
		http.Error(w, "xml error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(out)
}

// --- helpers ---

func esc(s string) string { return url.PathEscape(s) }

func principalHref(email string) string { return "/principals/" + esc(email) + "/" }
func calHome(email string) string       { return "/caldav/" + esc(email) + "/" }
func cardHome(email string) string      { return "/carddav/" + esc(email) + "/" }
func contactsHref(email string) string  { return cardHome(email) + contactsBookID + "/" }

func etag(id string, content []byte) string {
	sum := sha256.Sum256(content)
	key := id
	if key == "" {
		key = "obj"
	}
	return `"` + key + "-" + hex.EncodeToString(sum[:8]) + `"`
}

// upstreamErr reports an upstream failure to the client AND
// leaves a warning in the bridge log, per account (these used to
// be debug-only and invisible at the default level).
func (h *handler) upstreamErr(w http.ResponseWriter, email string, err error) {
	h.logger.Warn().Err(err).Str("protocol", "dav").Str("account", email).Msg("dav upstream error")
	http.Error(w, "upstream error", http.StatusBadGateway)
}

type collection struct {
	ID, Name string
	Tasks    bool
}

func (h *handler) collections(r *http.Request, email string) ([]collection, error) {
	var cals, lists []bridge.Calendar
	if err := h.call(r.Context(), email, func(ctx context.Context, p bridge.Provider) error {
		var e error
		cals, e = p.ListCalendars(ctx)
		return e
	}); err != nil {
		return nil, err
	}
	if err := h.call(r.Context(), email, func(ctx context.Context, p bridge.Provider) error {
		var e error
		lists, e = p.ListTaskLists(ctx)
		return e
	}); err != nil {
		return nil, err
	}
	var out []collection
	for _, c := range cals {
		out = append(out, collection{ID: c.ID, Name: c.Name})
	}
	for _, c := range lists {
		out = append(out, collection{ID: c.ID, Name: c.Name, Tasks: true})
	}
	return out, nil
}

func findColl(colls []collection, id string) (collection, bool) {
	for _, c := range colls {
		if c.ID == id {
			return c, true
		}
	}
	return collection{}, false
}

func (h *handler) fetchEvents(r *http.Request, email, cal string, from, to time.Time) ([]bridge.Event, error) {
	var out []bridge.Event
	err := h.call(r.Context(), email, func(ctx context.Context, p bridge.Provider) error {
		var e error
		out, e = p.ListEvents(ctx, cal, from, to)
		return e
	})
	return out, err
}
func (h *handler) fetchTasks(r *http.Request, email, list string) ([]bridge.Task, error) {
	var out []bridge.Task
	err := h.call(r.Context(), email, func(ctx context.Context, p bridge.Provider) error {
		var e error
		out, e = p.ListTasks(ctx, list)
		return e
	})
	return out, err
}
func (h *handler) fetchContacts(r *http.Request, email string) ([]bridge.Contact, error) {
	var out []bridge.Contact
	err := h.call(r.Context(), email, func(ctx context.Context, p bridge.Provider) error {
		var e error
		out, e = p.ListContacts(ctx, contactsBookID)
		return e
	})
	return out, err
}

func principalProp(email string, principal bool) xProp {
	rt := &xResourcetype{Collection: empty()}
	if principal {
		rt = &xResourcetype{Principal: empty()}
	}
	return xProp{
		Displayname: email, Resourcetype: rt,
		CurrentUser: &xHref{principalHref(email)},
		CalHome:     &xHref{calHome(email)},
		CardHome:    &xHref{cardHome(email)},
	}
}

func depth(r *http.Request) string {
	// Absent Depth on PROPFIND means infinity per RFC 4918; we
	// serve at most depth 1, so both map to "1".
	d := r.Header.Get("Depth")
	if d == "" || d == "infinity" {
		return "1"
	}
	return d
}

// --- REPORT request parsing (encoding/xml local structs) ---

type xTimeRange struct {
	Start string `xml:"start,attr"`
	End   string `xml:"end,attr"`
}
type xCompFilter struct {
	Name       string        `xml:"name,attr"`
	TimeRange  *xTimeRange   `xml:"time-range"`
	CompFilter []xCompFilter `xml:"comp-filter"`
}
type xReport struct {
	XMLName xml.Name
	Hrefs   []string     `xml:"href"`
	Filter  *xCompFilter `xml:"filter>comp-filter"`
	// calendar-query nests filter>comp-filter(VCALENDAR); handled via Filter above.
}

func parseReport(body []byte) (xReport, error) {
	var rep xReport
	if len(body) == 0 {
		return rep, nil
	}
	err := xml.Unmarshal(body, &rep)
	return rep, err
}

func flattenFilter(cf *xCompFilter, comps *[]string, tr **xTimeRange) {
	if cf == nil {
		return
	}
	*comps = append(*comps, strings.ToUpper(cf.Name))
	if cf.TimeRange != nil {
		*tr = cf.TimeRange
	}
	for i := range cf.CompFilter {
		flattenFilter(&cf.CompFilter[i], comps, tr)
	}
}

func parseRangeTime(s string) (time.Time, bool) {
	t, err := time.Parse("20060102T150405Z", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func uidFromHref(href string) string {
	href = strings.TrimSpace(href)
	if i := strings.Index(href, "://"); i >= 0 { // absolute URI
		if u, err := url.Parse(href); err == nil {
			href = u.EscapedPath()
		}
	}
	base := href[strings.LastIndex(href, "/")+1:]
	if un, err := url.PathUnescape(base); err == nil {
		base = un
	}
	base = strings.TrimSuffix(base, ".ics")
	base = strings.TrimSuffix(base, ".vcf")
	return base
}

// --- main handler ---

// statusRecorder captures the response status for operation logs.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	w = rec
	defer func() {
		// Mutating DAV operations are logged per account at info;
		// reads stay at debug via the call sites that need them.
		if (r.Method == "PUT" || r.Method == "DELETE") && rec.status < 400 {
			user, _, _ := r.BasicAuth()
			h.logger.Info().Str("protocol", "dav").Str("op", strings.ToLower(r.Method)).
				Str("account", bridge.NormalizeEmail(user)).Str("path", r.URL.Path).
				Int("status", rec.status).Msg("dav operation")
		}
	}()
	w.Header().Set("DAV", "1, 2, calendar-access, addressbook")
	user, pass, ok := r.BasicAuth()
	email := bridge.NormalizeEmail(user)
	if !ok || email == "" || !strings.Contains(email, "@") ||
		subtle.ConstantTimeCompare([]byte(pass), []byte(h.password)) != 1 {
		h.logger.Warn().Str("protocol", "dav").Str("op", "auth").
			Str("account", email).Str("remote", r.RemoteAddr).Msg("dav authentication failed")
		w.Header().Set("WWW-Authenticate", `Basic realm="o365bridge"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == "OPTIONS" {
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, REPORT")
		w.WriteHeader(http.StatusOK)
		return
	}

	path := r.URL.Path
	switch {
	case path == "/.well-known/caldav":
		http.Redirect(w, r, "/caldav/", http.StatusMovedPermanently)
		return
	case path == "/.well-known/carddav":
		http.Redirect(w, r, "/carddav/", http.StatusMovedPermanently)
		return
	case path == "/":
		h.handleRoot(w, r, email)
		return
	}

	// Split before decoding: opaque cloud IDs can contain encoded slashes.
	segs := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	for i := 0; i < min(3, len(segs)); i++ {
		decoded, err := url.PathUnescape(segs[i])
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		segs[i] = decoded
	}
	// The object filename stays escaped for uidFromHref to decode exactly once.
	if len(segs) == 0 || segs[0] == "" {
		h.handleRoot(w, r, email)
		return
	}
	switch segs[0] {
	case "principals":
		if len(segs) >= 2 && bridge.NormalizeEmail(segs[1]) != email {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == "PROPFIND" || r.Method == "GET" {
			h.writeMultistatus(w, []xResponse{resp(principalHref(email), principalProp(email, true))})
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case "caldav":
		h.handleCalDAV(w, r, email, segs)
	case "carddav":
		h.handleCardDAV(w, r, email, segs)
	default:
		http.NotFound(w, r)
	}
}

func (h *handler) handleRoot(w http.ResponseWriter, r *http.Request, email string) {
	switch r.Method {
	case "GET", "HEAD":
		http.Redirect(w, r, "/caldav/", http.StatusMovedPermanently)
	case "PROPFIND":
		h.writeMultistatus(w, []xResponse{resp("/", principalProp(email, false))})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// checkPathEmail verifies the <email> segment matches the auth user.
func checkPathEmail(w http.ResponseWriter, segs []string, email string) bool {
	if len(segs) >= 2 && bridge.NormalizeEmail(segs[1]) != email {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (h *handler) handleCalDAV(w http.ResponseWriter, r *http.Request, email string, segs []string) {
	// /caldav/
	if len(segs) == 1 {
		switch r.Method {
		case "GET", "HEAD":
			http.Redirect(w, r, calHome(email), http.StatusMovedPermanently)
		case "PROPFIND":
			resps := []xResponse{resp("/caldav/", principalProp(email, false))}
			if depth(r) != "0" {
				resps = append(resps, resp(calHome(email), xProp{Displayname: email, Resourcetype: &xResourcetype{Collection: empty()}, CurrentUser: &xHref{principalHref(email)}, CalHome: &xHref{calHome(email)}}))
			}
			h.writeMultistatus(w, resps)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if !checkPathEmail(w, segs, email) {
		return
	}
	// /caldav/<email>/
	if len(segs) == 2 {
		if r.Method != "PROPFIND" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resps := []xResponse{resp(calHome(email), xProp{Displayname: email, Resourcetype: &xResourcetype{Collection: empty()}, CurrentUser: &xHref{principalHref(email)}, CalHome: &xHref{calHome(email)}, CardHome: &xHref{cardHome(email)}})}
		if depth(r) != "0" {
			colls, err := h.collections(r, email)
			if err != nil {
				h.upstreamErr(w, email, err)
				return
			}
			for _, c := range colls {
				resps = append(resps, h.collProp(email, c))
			}
		}
		h.writeMultistatus(w, resps)
		return
	}
	collID := segs[2]
	if len(segs) == 3 {
		h.handleCalCollection(w, r, email, collID)
		return
	}
	if segs[3] == "_attachments" {
		h.handleTaskAttachment(w, r, email, collID, segs)
		return
	}
	if len(segs) != 4 {
		http.NotFound(w, r)
		return
	}
	h.handleCalObject(w, r, email, collID, segs[3])
}

func (h *handler) collProp(email string, c collection) xResponse {
	comp := "VEVENT"
	if c.Tasks {
		comp = "VTODO"
	}
	return resp(calHome(email)+esc(c.ID)+"/", xProp{
		Displayname:  c.Name,
		Resourcetype: &xResourcetype{Collection: empty(), Calendar: empty()},
		Supported:    &xSupported{Comps: []xComp{{Name: comp}}},
	})
}

func (h *handler) getColl(r *http.Request, email, id string) (collection, bool, error) {
	colls, err := h.collections(r, email)
	if err != nil {
		return collection{}, false, err
	}
	c, ok := findColl(colls, id)
	return c, ok, nil
}

func (h *handler) handleCalCollection(w http.ResponseWriter, r *http.Request, email, collID string) {
	coll, ok, err := h.getColl(r, email, collID)
	if err != nil {
		h.upstreamErr(w, email, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	href := calHome(email) + esc(collID) + "/"
	switch r.Method {
	case "PROPFIND":
		resps := []xResponse{h.collProp(email, coll)}
		if depth(r) != "0" {
			objs, err := h.calObjects(r, email, coll, wideFrom, wideTo, true)
			if err != nil {
				h.upstreamErr(w, email, err)
				return
			}
			for _, o := range objs {
				resps = append(resps, resp(href+esc(o.uid)+".ics", xProp{
					Displayname: o.uid, Resourcetype: &xResourcetype{},
					Getetag: etag(o.id, o.data), Getct: "text/calendar; charset=utf-8",
				}))
			}
		}
		h.writeMultistatus(w, resps)
	case "REPORT":
		h.reportCalendar(w, r, email, coll)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type calObj struct {
	uid, id string
	data    []byte
}

// calObjects returns objects of a collection in [from,to); for tasks
// the range is ignored (providers list all tasks).
func (h *handler) calObjects(r *http.Request, email string, coll collection, from, to time.Time, _ bool) ([]calObj, error) {
	if coll.Tasks {
		tasks, err := h.fetchTasks(r, email, coll.ID)
		if err != nil {
			return nil, err
		}
		var out []calObj
		for _, tk := range tasks {
			uid := tk.UID
			if uid == "" {
				uid = tk.ID
			}
			tk.UID = uid
			tk = taskWithAttachmentURIs(r, email, coll.ID, tk)
			out = append(out, calObj{uid: uid, id: tk.ID, data: generateTaskICalendar(tk)})
		}
		return out, nil
	}
	evs, err := h.fetchEvents(r, email, coll.ID, from, to)
	if err != nil {
		return nil, err
	}
	var out []calObj
	for _, ev := range evs {
		end := ev.End
		if end.IsZero() {
			end = ev.Start
		}
		// Overlap filter (providers may return everything).
		if !ev.Start.Before(to) || end.Before(from) {
			continue
		}
		uid := ev.UID
		if uid == "" {
			uid = ev.ID
		}
		ev.UID = uid
		out = append(out, calObj{uid: uid, id: ev.ID, data: generateEventICalendar(ev)})
	}
	return out, nil
}

func (h *handler) reportCalendar(w http.ResponseWriter, r *http.Request, email string, coll collection) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rep, err := parseReport(body)
	if err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	root := rep.XMLName.Local
	from, to := wideFrom, wideTo
	var comps []string
	var tr *xTimeRange
	flattenFilter(rep.Filter, &comps, &tr)
	if tr != nil {
		if t, ok := parseRangeTime(tr.Start); ok {
			from = t
		}
		if t, ok := parseRangeTime(tr.End); ok {
			to = t
		}
	}
	// Component mismatch: e.g. VEVENT query on a VTODO collection.
	if coll.Tasks && contains(comps, "VEVENT") && !contains(comps, "VTODO") {
		h.writeMultistatus(w, nil)
		return
	}
	if !coll.Tasks && contains(comps, "VTODO") && !contains(comps, "VEVENT") {
		h.writeMultistatus(w, nil)
		return
	}
	objs, err := h.calObjects(r, email, coll, from, to, true)
	if err != nil {
		h.upstreamErr(w, email, err)
		return
	}
	fetched := len(objs)
	if root == "calendar-multiget" {
		want := map[string]bool{}
		for _, hf := range rep.Hrefs {
			want[uidFromHref(hf)] = true
		}
		var filtered []calObj
		for _, o := range objs {
			if want[o.uid] || want[o.id] {
				filtered = append(filtered, o)
			}
		}
		objs = filtered
	}
	href := calHome(email) + esc(coll.ID) + "/"
	var resps []xResponse
	for _, o := range objs {
		resps = append(resps, resp(href+esc(o.uid)+".ics", xProp{
			Getetag: etag(o.id, o.data), CalData: string(o.data), Getct: "text/calendar; charset=utf-8",
		}))
	}
	if event := h.logger.Debug(); event.Enabled() {
		ids, uids := map[string]bool{}, map[string]bool{}
		for _, object := range objs {
			ids[object.id], uids[object.uid] = true, true
		}
		event.Str("protocol", "dav").Str("op", "calendar_report").Str("account", email).
			Str("report", root).Int("fetched", fetched).Int("requested_hrefs", len(rep.Hrefs)).
			Int("returned", len(objs)).Int("unique_ids", len(ids)).Int("unique_uids", len(uids)).
			Int("duplicate_uids", len(objs)-len(uids)).Msg("dav calendar report")
	}
	h.writeMultistatus(w, resps)
}

func sameTaskNote(a, b string) bool {
	normalize := strings.NewReplacer("\r\n", "\n", "\r", "\n")
	return normalize.Replace(a) == normalize.Replace(b)
}

func sameTaskTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Truncate(time.Second).Equal(b.Truncate(time.Second))
}

func sameTaskAnchor(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Format("2006-01-02") == b.Format("2006-01-02")
}

func matchesETag(values []string, current string) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		for _, tag := range strings.Split(value, ",") {
			if tag = strings.TrimSpace(tag); tag == "*" || tag == current {
				return true
			}
		}
	}
	return false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func (h *handler) handleCalObject(w http.ResponseWriter, r *http.Request, email, collID, file string) {
	uid := uidFromHref(file)
	switch r.Method {
	case "GET", "HEAD", "PROPFIND":
		coll, ok, err := h.getColl(r, email, collID)
		if err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		objs, err := h.calObjects(r, email, coll, wideFrom, wideTo, true)
		if err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		for _, o := range objs {
			if o.uid == uid || o.id == uid {
				if r.Method == "PROPFIND" {
					href := calHome(email) + esc(collID) + "/" + esc(o.uid) + ".ics"
					h.writeMultistatus(w, []xResponse{resp(href, xProp{
						Displayname: o.uid, Resourcetype: &xResourcetype{},
						Getetag: etag(o.id, o.data), Getct: "text/calendar; charset=utf-8",
					})})
					return
				}
				w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
				w.Header().Set("ETag", etag(o.id, o.data))
				w.WriteHeader(http.StatusOK)
				if r.Method == "GET" {
					_, _ = w.Write(o.data)
				}
				return
			}
		}
		http.NotFound(w, r)
	case "PUT":
		h.putCalObject(w, r, email, collID, uid)
	case "DELETE":
		h.deleteCalObject(w, r, email, collID, uid)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handler) putCalObject(w http.ResponseWriter, r *http.Request, email, collID, uid string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	events, tasks, err := parseICalendar(body)
	if err != nil {
		http.Error(w, "bad iCalendar", http.StatusBadRequest)
		return
	}
	if len(events)+len(tasks) != 1 {
		http.Error(w, "expected one VEVENT or VTODO", http.StatusBadRequest)
		return
	}
	coll, ok, err := h.getColl(r, email, collID)
	if err != nil {
		h.upstreamErr(w, email, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if coll.Tasks != (len(tasks) == 1) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><C:supported-calendar-component/></D:error>`)
		return
	}
	ctx := r.Context()
	if coll.Tasks {
		tk := tasks[0]
		if tk.UID == "" {
			tk.UID = uid
		}
		tk.ListID = collID
		existing, err := h.fetchTasks(r, email, collID)
		if err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		for _, ex := range existing {
			if ex.UID == tk.UID || ex.ID == tk.UID {
				if ex.UID == "" {
					ex.UID = ex.ID
				}
				current := taskWithAttachmentURIs(r, email, collID, ex)
				if !matchesETag(r.Header.Values("If-Match"), etag(ex.ID, generateTaskICalendar(current))) {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				if !taskAttachmentRefsMatch(r, email, collID, ex, tk.Attachments) {
					http.Error(w, "task attachments are read-only; upload/removal is not supported", http.StatusNotImplemented)
					return
				}
				if ex.RecurUnsupported {
					if tk.Recur != "" {
						http.Error(w, "native task repetition cannot be safely replaced", http.StatusUnprocessableEntity)
						return
					}
					tk.RecurSet, tk.RecurUnsupported = false, true
				}
				tk.BodyUnchanged = sameTaskNote(tk.Body, ex.Body)
				tk.StatusUnchanged = tk.Status == ex.Status && sameTaskTime(tk.Completed, ex.Completed)
				// The task codec cannot edit alarms yet; do not erase a native reminder.
				tk.Reminder = ex.Reminder
				// Preserve the native repeat anchor if a client omits bridge metadata.
				if tk.Recur != "" && tk.RecurStart == nil {
					tk.RecurStart, tk.RecurTimeZone = ex.RecurStart, ex.RecurTimeZone
					if ex.Start == nil && tk.Start != nil && ex.RecurStart != nil && tk.Start.Equal(*ex.RecurStart) {
						tk.Start, tk.StartDateOnly = nil, false
					}
				}
				if sameRecurrenceRule(tk.Recur, taskCalendarRule(ex)) && sameTaskAnchor(tk.RecurStart, ex.RecurStart) && tk.RecurTimeZone == ex.RecurTimeZone {
					tk.RecurSet, tk.Recur = false, ex.Recur
				}
				tk.ID = ex.ID
				if err := h.call(ctx, email, func(c context.Context, p bridge.Provider) error {
					return p.UpdateTask(c, tk)
				}); err != nil {
					h.upstreamErr(w, email, err)
					return
				}
				tk.Attachments = ex.Attachments
				tk = taskWithAttachmentURIs(r, email, collID, tk)
				w.Header().Set("ETag", etag(tk.ID, generateTaskICalendar(tk)))
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if len(r.Header.Values("If-Match")) > 0 {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		if tk.Attachments != nil && len(tk.Attachments) > 0 {
			http.Error(w, "task attachment uploads are not supported", http.StatusNotImplemented)
			return
		}
		var created bridge.Task
		if err := h.call(ctx, email, func(c context.Context, p bridge.Provider) error {
			var e error
			created, e = p.CreateTask(c, tk)
			return e
		}); err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		if created.ID != "" {
			tk.ID = created.ID
		}
		w.Header().Set("ETag", etag(tk.ID, generateTaskICalendar(tk)))
		w.WriteHeader(http.StatusCreated)
		return
	}
	if len(events) == 0 {
		http.Error(w, "no VEVENT/VTODO", http.StatusBadRequest)
		return
	}
	ev := events[0]
	if ev.UID == "" {
		ev.UID = uid
	}
	ev.CalendarID = collID
	existing, err := h.fetchEvents(r, email, collID, wideFrom, wideTo)
	if err != nil {
		h.upstreamErr(w, email, err)
		return
	}
	ifMatch := r.Header.Values("If-Match")
	for _, ex := range existing {
		if ex.UID == ev.UID || ex.ID == ev.UID {
			if len(ifMatch) > 0 {
				if ex.UID == "" {
					ex.UID = ex.ID
				}
				if !matchesETag(ifMatch, etag(ex.ID, generateEventICalendar(ex))) {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
			}
			ev, err = prepareEventRepeatUpdate(ev, ex)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			ev.ID = ex.ID
			// RespondEvent choice: when the PUT changes the local
			// user's PARTSTAT on an event organised by someone else,
			// we call RespondEvent *instead of* UpdateEvent. A plain
			// update of another organiser's event is not meaningful
			// upstream (and may be rejected); RespondEvent records the
			// reply and notifies the organiser, which is the semantic
			// the client intended. Other-field edits in the same PUT
			// are therefore not applied in this case.
			if resp, changed := partstatChange(ex, ev, email); changed {
				if ev.RecurrenceSet {
					http.Error(w, "send an invitation response separately from recurrence edits", http.StatusUnprocessableEntity)
					return
				}
				if err := h.call(ctx, email, func(c context.Context, p bridge.Provider) error {
					return p.RespondEvent(c, collID, ex.ID, resp)
				}); err != nil {
					h.upstreamErr(w, email, err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if err := h.call(ctx, email, func(c context.Context, p bridge.Provider) error {
				return p.UpdateEvent(c, ev)
			}); err != nil {
				h.upstreamErr(w, email, err)
				return
			}
			// Native recurrence defaults may normalize; GET owns the canonical validator.
			if ev.Recurrence == "" && ex.Recurrence == "" && !ex.RecurrenceUnsupported {
				w.Header().Set("ETag", etag(ev.ID, generateEventICalendar(ev)))
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if len(ifMatch) > 0 {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if ev.RecurrenceUnsupported {
		http.Error(w, "unsupported native repetition cannot be created", http.StatusUnprocessableEntity)
		return
	}
	var created bridge.Event
	if err := h.call(ctx, email, func(c context.Context, p bridge.Provider) error {
		var e error
		created, e = p.CreateEvent(c, ev)
		return e
	}); err != nil {
		h.upstreamErr(w, email, err)
		return
	}
	if created.ID != "" {
		ev.ID = created.ID
	}
	if ev.Recurrence == "" {
		w.Header().Set("ETag", etag(ev.ID, generateEventICalendar(ev)))
	}
	w.WriteHeader(http.StatusCreated)
}

// partstatChange detects a local-user attendee status change to
// accepted/declined/tentative on an event organised by someone else.
func partstatChange(oldEv, newEv bridge.Event, email string) (string, bool) {
	org := newEv.Organizer
	if org == "" {
		org = oldEv.Organizer
	}
	if org == "" || bridge.NormalizeEmail(stripMailto(org)) == email {
		return "", false
	}
	var oldStatus, newStatus string
	for _, a := range oldEv.Attendees {
		if bridge.NormalizeEmail(a.Email) == email {
			oldStatus = strings.ToLower(a.Status)
		}
	}
	for _, a := range newEv.Attendees {
		if bridge.NormalizeEmail(a.Email) == email {
			newStatus = strings.ToLower(a.Status)
		}
	}
	switch newStatus {
	case "accepted", "declined", "tentative":
		if newStatus != oldStatus {
			return newStatus, true
		}
	}
	return "", false
}

func (h *handler) deleteCalObject(w http.ResponseWriter, r *http.Request, email, collID, uid string) {
	coll, ok, err := h.getColl(r, email, collID)
	if err != nil {
		h.upstreamErr(w, email, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if coll.Tasks {
		tasks, err := h.fetchTasks(r, email, collID)
		if err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		for _, tk := range tasks {
			if tk.UID == uid || tk.ID == uid {
				if err := h.call(ctx, email, func(c context.Context, p bridge.Provider) error {
					return p.DeleteTask(c, collID, tk.ID)
				}); err != nil {
					h.upstreamErr(w, email, err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	evs, err := h.fetchEvents(r, email, collID, wideFrom, wideTo)
	if err != nil {
		h.upstreamErr(w, email, err)
		return
	}
	for _, ev := range evs {
		if ev.UID == uid || ev.ID == uid {
			if err := h.call(ctx, email, func(c context.Context, p bridge.Provider) error {
				return p.DeleteEvent(c, collID, ev.ID)
			}); err != nil {
				h.upstreamErr(w, email, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.NotFound(w, r)
}

// --- CardDAV ---

func (h *handler) handleCardDAV(w http.ResponseWriter, r *http.Request, email string, segs []string) {
	if len(segs) == 1 {
		switch r.Method {
		case "GET", "HEAD":
			http.Redirect(w, r, cardHome(email), http.StatusMovedPermanently)
		case "PROPFIND":
			resps := []xResponse{resp("/carddav/", principalProp(email, false))}
			if depth(r) != "0" {
				resps = append(resps, resp(cardHome(email), xProp{Displayname: email, Resourcetype: &xResourcetype{Collection: empty()}, CardHome: &xHref{cardHome(email)}}))
			}
			h.writeMultistatus(w, resps)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if !checkPathEmail(w, segs, email) {
		return
	}
	if len(segs) == 2 {
		if r.Method != "PROPFIND" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resps := []xResponse{resp(cardHome(email), xProp{Displayname: email, Resourcetype: &xResourcetype{Collection: empty()}, CurrentUser: &xHref{principalHref(email)}, CardHome: &xHref{cardHome(email)}})}
		if depth(r) != "0" {
			resps = append(resps, h.contactsProp(email))
		}
		h.writeMultistatus(w, resps)
		return
	}
	if segs[2] != contactsBookID {
		http.NotFound(w, r)
		return
	}
	if len(segs) == 3 {
		h.handleContacts(w, r, email)
		return
	}
	h.handleContactObject(w, r, email, segs[3])
}

func (h *handler) contactsProp(email string) xResponse {
	return resp(contactsHref(email), xProp{
		Displayname:  "Contacts",
		Resourcetype: &xResourcetype{Collection: empty(), Addressbook: empty()},
	})
}

func (h *handler) handleContacts(w http.ResponseWriter, r *http.Request, email string) {
	switch r.Method {
	case "PROPFIND":
		resps := []xResponse{h.contactsProp(email)}
		if depth(r) != "0" {
			contacts, err := h.fetchContacts(r, email)
			if err != nil {
				h.upstreamErr(w, email, err)
				return
			}
			for _, c := range contacts {
				uid := c.UID
				if uid == "" {
					uid = c.ID
				}
				data := generateVCard(c)
				resps = append(resps, resp(contactsHref(email)+esc(uid)+".vcf", xProp{
					Displayname: uid, Resourcetype: &xResourcetype{},
					Getetag: etag(c.ID, data), Getct: "text/vcard; charset=utf-8",
				}))
			}
		}
		h.writeMultistatus(w, resps)
	case "REPORT":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rep, err := parseReport(body)
		if err != nil {
			http.Error(w, "bad report", http.StatusBadRequest)
			return
		}
		contacts, err := h.fetchContacts(r, email)
		if err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		if rep.XMLName.Local == "addressbook-multiget" {
			want := map[string]bool{}
			for _, hf := range rep.Hrefs {
				want[uidFromHref(hf)] = true
			}
			var f []bridge.Contact
			for _, c := range contacts {
				if want[c.UID] || want[c.ID] {
					f = append(f, c)
				}
			}
			contacts = f
		}
		var resps []xResponse
		for _, c := range contacts {
			uid := c.UID
			if uid == "" {
				uid = c.ID
			}
			data := generateVCard(c)
			resps = append(resps, resp(contactsHref(email)+esc(uid)+".vcf", xProp{
				Getetag: etag(c.ID, data), AddrData: string(data), Getct: "text/vcard; charset=utf-8",
			}))
		}
		h.writeMultistatus(w, resps)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handler) handleContactObject(w http.ResponseWriter, r *http.Request, email, file string) {
	uid := uidFromHref(file)
	ctx := r.Context()
	switch r.Method {
	case "GET", "HEAD", "PROPFIND":
		contacts, err := h.fetchContacts(r, email)
		if err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		for _, c := range contacts {
			if c.UID == uid || c.ID == uid {
				data := generateVCard(c)
				if r.Method == "PROPFIND" {
					cuid := c.UID
					if cuid == "" {
						cuid = c.ID
					}
					h.writeMultistatus(w, []xResponse{resp(contactsHref(email)+esc(cuid)+".vcf", xProp{
						Displayname: cuid, Resourcetype: &xResourcetype{},
						Getetag: etag(c.ID, data), Getct: "text/vcard; charset=utf-8",
					})})
					return
				}
				w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
				w.Header().Set("ETag", etag(c.ID, data))
				w.WriteHeader(http.StatusOK)
				if r.Method == "GET" {
					_, _ = w.Write(data)
				}
				return
			}
		}
		http.NotFound(w, r)
	case "PUT":
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		c, err := parseVCard(body)
		if err != nil {
			http.Error(w, "bad vCard", http.StatusBadRequest)
			return
		}
		if c.UID == "" {
			c.UID = uid
		}
		c.BookID = contactsBookID
		contacts, err := h.fetchContacts(r, email)
		if err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		for _, ex := range contacts {
			if ex.UID == c.UID || ex.ID == c.UID {
				c.ID = ex.ID
				if err := h.call(ctx, email, func(ctx2 context.Context, p bridge.Provider) error {
					return p.UpdateContact(ctx2, c)
				}); err != nil {
					h.upstreamErr(w, email, err)
					return
				}
				w.Header().Set("ETag", etag(c.ID, generateVCard(c)))
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		var created bridge.Contact
		if err := h.call(ctx, email, func(ctx2 context.Context, p bridge.Provider) error {
			var e error
			created, e = p.CreateContact(ctx2, c)
			return e
		}); err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		if created.ID != "" {
			c.ID = created.ID
		}
		w.Header().Set("ETag", etag(c.ID, generateVCard(c)))
		w.WriteHeader(http.StatusCreated)
	case "DELETE":
		contacts, err := h.fetchContacts(r, email)
		if err != nil {
			h.upstreamErr(w, email, err)
			return
		}
		for _, c := range contacts {
			if c.UID == uid || c.ID == uid {
				if err := h.call(ctx, email, func(ctx2 context.Context, p bridge.Provider) error {
					return p.DeleteContact(ctx2, contactsBookID, c.ID)
				}); err != nil {
					h.upstreamErr(w, email, err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
