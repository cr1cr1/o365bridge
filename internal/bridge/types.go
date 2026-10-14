package bridge

import (
	"context"
	"strings"
	"time"
)

// Channel is an upstream Microsoft protocol an account is bound to.
type Channel string

const (
	ChannelGraph Channel = "graph"
	ChannelOWA   Channel = "owa"
)

// Session is everything the bridge captured for one account's
// upstream login. It is persisted in the interim session cache; it
// is never configuration supplied by the user.
type Session struct {
	Email         string            `json:"email"`
	Channel       Channel           `json:"channel"`
	AccessToken   string            `json:"access_token,omitempty"`
	RefreshToken  string            `json:"refresh_token,omitempty"`
	TokenExpiry   time.Time         `json:"token_expiry,omitempty"`
	OWAAuthHeader string            `json:"owa_auth_header,omitempty"`
	OWACookies    map[string]string `json:"owa_cookies,omitempty"`
	OWAHost       string            `json:"owa_host,omitempty"`
}

// Expired reports whether the session's access token is past (or
// within a minute of) expiry. OWA sessions carry no token expiry;
// they are valid until upstream says otherwise.
func (s Session) Expired(now time.Time) bool {
	if s.TokenExpiry.IsZero() {
		return false
	}
	return now.Add(time.Minute).After(s.TokenExpiry)
}

// Account is the registry's public view of a discovered account.
type Account struct {
	Email   string
	Channel Channel
}

// AccountState is the persisted per-account record.
type AccountState struct {
	Email   string  `json:"email"`
	Session Session `json:"session"`
}

// NormalizeEmail canonicalises an account email (the downstream
// username) for use as map key and directory name.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Folder is an upstream mail folder.
type Folder struct {
	ID       string
	Name     string // display name
	ParentID string
	Total    int
	Unread   int
	// Special is "", "inbox", "sent", "drafts", "trash", "junk" or
	// "archive" when the upstream marks the folder as such.
	Special string
}

// Flags is a set of IMAP-style message flags (lower-case keywords,
// e.g. \seen, \flagged, $junk).
type Flags []string

// MessageMeta is message envelope data, without the body.
type MessageMeta struct {
	ID          string // upstream id, stable within its folder
	FolderID    string
	Subject     string
	From        string
	To          []string
	Date        time.Time
	Flags       Flags
	Size        int64
	Attachments bool
}

// SearchCriteria is the push-downable subset of an IMAP SEARCH.
type SearchCriteria struct {
	Text    string
	From    string
	Subject string
	Since   time.Time
	Before  time.Time
	Unseen  bool
	Flagged bool
}

// Calendar is an upstream calendar (events) or task list.
type Calendar struct {
	ID        string
	Name      string
	TasksOnly bool // collection holds tasks, not events
}

// Attendee is an event participant and its participation status
// ("accepted", "declined", "tentative", "needs-action", "").
type Attendee struct {
	Email  string
	Name   string
	Status string
}

// Event is a calendar event in provider-neutral form.
type Event struct {
	ID          string
	CalendarID  string
	UID         string // iCalendar UID
	Summary     string
	Description string
	Location    string
	Categories  []string // nil = not supplied; non-nil empty = clear on provider update
	Start       time.Time
	End         time.Time
	AllDay      bool
	TimeZone    string // IANA name, "" = floating/UTC as given
	Organizer   string
	Attendees   []Attendee
	Recurrence  string // iCalendar RRULE value, "" = none
	ReminderMin *int   // minutes before start, nil = none
	Sequence    int
}

// Task is a to-do item in provider-neutral form.
type Task struct {
	ID               string
	ListID           string
	UID              string
	Title            string
	Body             string
	Start            *time.Time
	Due              *time.Time
	Completed        *time.Time
	Priority         int // iCalendar 1 (high) .. 9 (low), 0/5 = normal
	Reminder         *time.Time
	Recur            string           // iCalendar RRULE value, "" = none
	Status           string           // "needs-action", "in-process", "completed"
	Categories       []string         // nil = unsupplied, non-nil empty = clear
	Attachments      []TaskAttachment // native files; content is fetched on demand
	StartDateOnly    bool
	DueDateOnly      bool
	FullUpdate       bool       // full VTODO replacement; absent nullable fields may be cleared
	BodyUnchanged    bool       // preserve native rich-text notes on unrelated edits
	StatusUnchanged  bool       // preserve native progress/state that VTODO cannot fully represent
	RecurSet         bool       // explicit repeat update or removal
	RecurStart       *time.Time // native repeat range anchor, not the task start date
	RecurTimeZone    string
	RecurUnsupported bool // preserve unrepresentable native repetition on ordinary edits
	RecurAnchorOnly  bool // DAV DTSTART represents the repeat anchor, not a native start date
}

// TaskAttachment describes a native task file without exposing upstream credentials.
type TaskAttachment struct {
	ID          string
	Name        string
	ContentType string
	Size        int64
	URI         string // authenticated bridge URI, assigned by DAV on a serialization copy
}

// MaxTaskAttachmentBytes bounds in-memory downloads without a local file store.
const MaxTaskAttachmentBytes int64 = 32 << 20

// TaskAttachmentProvider is an optional native file-download capability.
// The provider must verify task/list membership before using an attachment token.
type TaskAttachmentProvider interface {
	GetTaskAttachment(ctx context.Context, listID, taskID, attachmentID string) ([]byte, error)
}

// Contact is an address-book entry in provider-neutral form.
type Contact struct {
	ID        string
	BookID    string
	UID       string
	FullName  string
	FirstName string
	LastName  string
	Emails    []string
	Phones    []string
	Org       string
	Notes     string
}

// Provider is the upstream contract: one internal operation set
// covering mail, calendar and contacts. One account binds exactly one Provider,
// created from its Session by a Factory. Implementations must
// classify failures with the Kind helpers in this package:
// auth-class errors drive re-login and Graph→OWA fallback.
type Provider interface {
	// Ping proves the channel answers for this account (mail read).
	Ping(ctx context.Context) error

	ListFolders(ctx context.Context) ([]Folder, error)
	CreateFolder(ctx context.Context, name, parentID string) (Folder, error)
	DeleteFolder(ctx context.Context, id string) error
	RenameFolder(ctx context.Context, id, name string) error

	ListMessages(ctx context.Context, folderID string, offset, limit int) ([]MessageMeta, int, error)
	GetMessage(ctx context.Context, folderID, id string) (MessageMeta, error)
	GetMessageMIME(ctx context.Context, folderID, id string) ([]byte, error)
	SearchMessages(ctx context.Context, folderID string, c SearchCriteria) ([]string, error)
	SetFlags(ctx context.Context, folderID string, ids []string, add, remove Flags) error
	MoveMessages(ctx context.Context, folderID string, ids []string, destFolderID string) error
	CopyMessages(ctx context.Context, folderID string, ids []string, destFolderID string) error
	// DeleteMessages moves messages to Deleted Items, or destroys
	// them if they are already there.
	DeleteMessages(ctx context.Context, folderID string, ids []string) error
	// AppendMessage stores a raw MIME message (drafts, Sent copies).
	AppendMessage(ctx context.Context, folderID string, mime []byte, flags Flags) (string, error)
	// SendMessage sends a raw MIME message.
	SendMessage(ctx context.Context, mime []byte) error

	ListCalendars(ctx context.Context) ([]Calendar, error)
	ListEvents(ctx context.Context, calendarID string, from, to time.Time) ([]Event, error)
	CreateEvent(ctx context.Context, ev Event) (Event, error)
	UpdateEvent(ctx context.Context, ev Event) error
	DeleteEvent(ctx context.Context, calendarID, id string) error
	// RespondEvent records an invitation response ("accepted",
	// "declined", "tentative") via the upstream dedicated
	// invitation-response operation, so the organizer is notified.
	RespondEvent(ctx context.Context, calendarID, id, response string) error
	FreeBusy(ctx context.Context, emails []string, from, to time.Time) (map[string][][2]time.Time, error)

	ListTaskLists(ctx context.Context) ([]Calendar, error)
	ListTasks(ctx context.Context, listID string) ([]Task, error)
	CreateTask(ctx context.Context, t Task) (Task, error)
	UpdateTask(ctx context.Context, t Task) error
	DeleteTask(ctx context.Context, listID, id string) error

	ListContacts(ctx context.Context, bookID string) ([]Contact, error)
	CreateContact(ctx context.Context, c Contact) (Contact, error)
	UpdateContact(ctx context.Context, c Contact) error
	DeleteContact(ctx context.Context, bookID, id string) error
}

// Factory builds the Provider bound to one account session.
type Factory interface {
	New(email string, s Session) Provider
}

// LoginManager obtains and renews upstream sessions. The production
// implementation drives a browser (internal/rodlogin); tests fake
// it. Login is interactive and may block indefinitely — it ends on
// success, an actual upstream error, or user cancellation.
type LoginManager interface {
	Login(ctx context.Context, email string, ch Channel) (Session, error)
	// Renew attempts silent renewal (refresh token / live browser
	// profile). It must not show UI.
	Renew(ctx context.Context, email string, s Session) (Session, error)
}
