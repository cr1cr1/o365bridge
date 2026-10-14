package mail

// Phase 2a-2: remaining RFC 8621 types — Identity (§6),
// EmailSubmission (§7), VacationResponse (§8), SearchSnippet (§5).

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
)

// --- Identity (RFC 8621 §6) ---

// Identity is a sending identity (§6).
type Identity struct {
	ID            jmap.ID        `json:"id,omitempty"`
	Name          string         `json:"name"`
	Email         string         `json:"email"`
	ReplyTo       []EmailAddress `json:"replyTo"`
	Bcc           []EmailAddress `json:"bcc"`
	TextSignature string         `json:"textSignature"`
	HTMLSignature string         `json:"htmlSignature"`
	MayDelete     bool           `json:"mayDelete"`
}

// IdentityBackend is the optional contract behind Identity/*; Register
// only registers those methods when the Backend implements it.
type IdentityBackend interface {
	Identities() datatype.Store[Identity]
}

type identityCondition struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

func identityMatch(v Identity, filter json.RawMessage) (bool, error) {
	return evalFilter(v, filter, func(v Identity, raw json.RawMessage) (bool, error) {
		if err := checkKeys(raw, map[string]bool{"name": true, "email": true}); err != nil {
			return false, err
		}
		var c identityCondition
		if err := json.Unmarshal(raw, &c); err != nil {
			return false, err
		}
		// shortcut: RFC query filter name/email are substring-ish on
		// this implementation (case-insensitive substring, not exact).
		if c.Name != nil && !containsFold(v.Name, *c.Name) {
			return false, nil
		}
		if c.Email != nil && !containsFold(v.Email, *c.Email) {
			return false, nil
		}
		return true, nil
	})
}

func identityCompare(a, b Identity, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		return strings.Compare(a.Name, b.Name), true
	case "email":
		return strings.Compare(a.Email, b.Email), true
	}
	return 0, false
}

// --- EmailSubmission (RFC 8621 §7) ---

// EnvelopeAddress is an address with ESMTP parameters (§7).
// A null parameter value means the parameter had no value.
type EnvelopeAddress struct {
	Email      string             `json:"email"`
	Parameters map[string]*string `json:"parameters"`
}

// Envelope is the SMTP envelope of a submission (§7).
type Envelope struct {
	MailFrom EnvelopeAddress   `json:"mailFrom"`
	RcptTo   []EnvelopeAddress `json:"rcptTo"`
}

// DeliveryStatus is the per-recipient delivery status (§7).
type DeliveryStatus struct {
	Attempted string `json:"attempted"`
	Final     string `json:"final"`
	SMTPReply string `json:"smtpReply"`
}

// EmailSubmission is a submitted (or to-be-submitted) email (§7).
//
// Contract: a submission store's Create performs the actual
// submission (sending, or scheduling for SendAt) — that is the
// dogfood adapter's job later; this layer only records the object
// returned by Create. onSuccess* effects are applied by the custom
// EmailSubmission/set handler (see methods2.go), not by the store.
//
// shortcut: an emailId in a create object that is a creation-id
// reference to an Email created earlier in the same request is NOT
// resolved here; clients must use a real (server) email id.
type EmailSubmission struct {
	ID             jmap.ID                    `json:"id,omitempty"`
	IdentityID     jmap.ID                    `json:"identityId"`
	EmailID        jmap.ID                    `json:"emailId"`
	ThreadID       jmap.ID                    `json:"threadId"`
	Envelope       *Envelope                  `json:"envelope"`
	SendAt         *time.Time                 `json:"sendAt"`
	UndoStatus     string                     `json:"undoStatus"`
	DeliveryStatus map[string]*DeliveryStatus `json:"deliveryStatus"`
	DSNBlobIDs     []jmap.ID                  `json:"dsnBlobIds"`
	MDNBlobIDs     []jmap.ID                  `json:"mdnBlobIds"`
}

// SubmissionBackend is the optional contract behind
// EmailSubmission/*; Register only registers those methods when the
// Backend implements it.
type SubmissionBackend interface {
	Submissions() datatype.Store[EmailSubmission]
}

type submissionCondition struct {
	IdentityID *jmap.ID   `json:"identityId"`
	EmailID    *jmap.ID   `json:"emailId"`
	ThreadID   *jmap.ID   `json:"threadId"`
	UndoStatus *string    `json:"undoStatus"`
	Before     *time.Time `json:"before"`
	After      *time.Time `json:"after"`
}

func submissionMatch(v EmailSubmission, filter json.RawMessage) (bool, error) {
	return evalFilter(v, filter, func(v EmailSubmission, raw json.RawMessage) (bool, error) {
		if err := checkKeys(raw, map[string]bool{
			"identityId": true, "emailId": true, "threadId": true,
			"undoStatus": true, "before": true, "after": true,
		}); err != nil {
			return false, err
		}
		var c submissionCondition
		if err := json.Unmarshal(raw, &c); err != nil {
			return false, err
		}
		if c.IdentityID != nil && v.IdentityID != *c.IdentityID {
			return false, nil
		}
		if c.EmailID != nil && v.EmailID != *c.EmailID {
			return false, nil
		}
		if c.ThreadID != nil && v.ThreadID != *c.ThreadID {
			return false, nil
		}
		if c.UndoStatus != nil && v.UndoStatus != *c.UndoStatus {
			return false, nil
		}
		// before/after compare against sendAt; an unscheduled
		// submission (null sendAt) never matches either.
		if c.Before != nil && (v.SendAt == nil || !v.SendAt.Before(*c.Before)) {
			return false, nil
		}
		if c.After != nil && (v.SendAt == nil || !v.SendAt.After(*c.After)) {
			return false, nil
		}
		return true, nil
	})
}

// submissionCompare supports only sendAt (+ id falls through to the
// unsupported-sort error in datatype, per the task contract).
func submissionCompare(a, b EmailSubmission, c jmap.Comparator) (int, bool) {
	if c.Property == "sendAt" {
		var at, bt time.Time
		if a.SendAt != nil {
			at = *a.SendAt
		}
		if b.SendAt != nil {
			bt = *b.SendAt
		}
		return at.Compare(bt), true
	}
	return 0, false
}

// --- VacationResponse (RFC 8621 §8) ---

// VacationResponse is the singleton vacation autoresponder object
// (§8). Its id is always "singleton".
type VacationResponse struct {
	ID        jmap.ID    `json:"id,omitempty"`
	IsEnabled bool       `json:"isEnabled"`
	FromDate  *time.Time `json:"fromDate"`
	ToDate    *time.Time `json:"toDate"`
	Subject   *string    `json:"subject"`
	TextBody  *string    `json:"textBody"`
	HTMLBody  *string    `json:"htmlBody"`
}

// VacationStore is the (non-generic, singleton) contract behind
// VacationResponse/get|set.
type VacationStore interface {
	Vacation(ctx context.Context, account jmap.ID) (VacationResponse, string, error)
	SetVacation(ctx context.Context, account jmap.ID, v VacationResponse, ifInState *string) (oldState, newState string, err error)
}

// VacationBackend is the optional contract behind
// VacationResponse/*.
type VacationBackend interface {
	VacationStore() VacationStore
}

// --- SearchSnippet (RFC 8621 §5, SearchSnippet/get) ---

// SearchSnippet is a subject/preview snippet for one email (§5).
type SearchSnippet struct {
	EmailID jmap.ID `json:"emailId"`
	Subject *string `json:"subject"`
	Preview *string `json:"preview"`
}

// SnippetSource supplies snippets; ids it cannot answer go in notFound.
type SnippetSource interface {
	Snippets(ctx context.Context, account jmap.ID, emailIds []jmap.ID) ([]SearchSnippet, []jmap.ID, error)
}

// SnippetBackend is the optional contract behind SearchSnippet/get.
type SnippetBackend interface {
	SnippetSource() SnippetSource
}

// --- Email/import, Email/copy, Email/parse optional contracts ---

// Importer imports one blob as an Email (Email/import).
type Importer interface {
	ImportEmail(ctx context.Context, account jmap.ID, blobID jmap.ID, mailboxIDs map[jmap.ID]bool, keywords map[string]bool, receivedAt *time.Time) (Email, *jmap.SetError)
}

// ImportBackend is the optional contract behind Email/import.
type ImportBackend interface {
	EmailImporter() Importer
}

// Copier copies one Email from one account to another (Email/copy).
type Copier interface {
	CopyEmail(ctx context.Context, fromAccount, toAccount jmap.ID, id jmap.ID) (Email, *jmap.SetError)
}

// CopyBackend is the optional contract behind Email/copy.
type CopyBackend interface {
	EmailCopier() Copier
}

// Parser parses one blob into an Email without storing it
// (Email/parse).
type Parser interface {
	ParseEmail(ctx context.Context, account jmap.ID, blobID jmap.ID) (Email, error)
}

// ParseBackend is the optional contract behind Email/parse.
type ParseBackend interface {
	EmailParser() Parser
}
