// Package mail implements the RFC 8621 (JMAP Mail) data types
// Mailbox (§2), Email (§4) and Thread (§3) on top of the generic
// datatype machinery.
//
// Mailbox and Email are ordinary stores: Register wires them through
// datatype.Register, which provides get/set/query (and changes /
// queryChanges when the store also implements datatype.ChangeStore).
// Thread is read-only in RFC 8621 — it defines only Thread/get and
// Thread/changes, no set/query — so Thread/get is registered manually
// here and Thread/changes only when the backend additionally
// implements ThreadChangeStore.
//
// Capability: datatype.Register advertises urn:ietf:params:jmap:mail
// (jmap.MailURI) with an empty capability object for both Mailbox and
// Email; Register therefore does not register the capability again.
package mail

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// --- Mailbox (RFC 8621 §2) ---

// MailboxRights is the myRights object of a Mailbox (RFC 8621 §2).
type MailboxRights struct {
	MayReadItems   bool `json:"mayReadItems"`
	MayAddItems    bool `json:"mayAddItems"`
	MayRemoveItems bool `json:"mayRemoveItems"`
	MaySetSeen     bool `json:"maySetSeen"`
	MaySetKeywords bool `json:"maySetKeywords"`
	MayCreateChild bool `json:"mayCreateChild"`
	MayRename      bool `json:"mayRename"`
	MayDelete      bool `json:"mayDelete"`
}

// Mailbox is a folder of emails (RFC 8621 §2).
type Mailbox struct {
	ID            jmap.ID        `json:"id,omitempty"`
	Name          string         `json:"name"`
	ParentID      *jmap.ID       `json:"parentId"` // null for a top-level mailbox
	Role          *string        `json:"role"`     // null when the mailbox has no role
	SortOrder     uint           `json:"sortOrder"`
	TotalEmails   int            `json:"totalEmails"`
	UnreadEmails  int            `json:"unreadEmails"`
	TotalThreads  int            `json:"totalThreads"`
	UnreadThreads int            `json:"unreadThreads"`
	MyRights      *MailboxRights `json:"myRights"`
	IsSubscribed  bool           `json:"isSubscribed"`
}

// --- Email (RFC 8621 §4) ---

// EmailAddress is an address in a Sender/From/To/... field (§4.1.3).
type EmailAddress struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// BodyValue is one entry of an Email's bodyValues map (§4.1.5).
type BodyValue struct {
	Value             string `json:"value"`
	IsEncodingProblem bool   `json:"isEncodingProblem"`
	IsTruncated       bool   `json:"isTruncated"`
}

// BodyPart is one MIME body part description (§4.1.4). Fields the RFC
// allows to be null are pointers so null round-trips distinctly from a
// zero value.
//
// shortcut: the part "headers" property is omitted; Email objects in
// this package do not retain arbitrary part headers.
type BodyPart struct {
	PartID      string   `json:"partId"`
	BlobID      *jmap.ID `json:"blobId"` // null when the part has no blob
	Size        uint     `json:"size"`
	Name        *string  `json:"name"`
	Type        string   `json:"type"`
	Charset     *string  `json:"charset"`
	Disposition *string  `json:"disposition"`
	CID         *string  `json:"cid"`
	Language    []string `json:"language"`
	Location    *string  `json:"location"`
}

// Email is a single message (RFC 8621 §4.1).
type Email struct {
	ID         jmap.ID          `json:"id,omitempty"`
	BlobID     jmap.ID          `json:"blobId"`
	ThreadID   jmap.ID          `json:"threadId"`
	MailboxIDs map[jmap.ID]bool `json:"mailboxIds"`
	Keywords   map[string]bool  `json:"keywords"`
	Size       uint             `json:"size"`

	ReceivedAt time.Time      `json:"receivedAt"`
	MessageID  []string       `json:"messageId"`
	InReplyTo  []string       `json:"inReplyTo"`
	References []string       `json:"references"`
	Sender     *EmailAddress  `json:"sender"`
	From       []EmailAddress `json:"from"`
	To         []EmailAddress `json:"to"`
	Cc         []EmailAddress `json:"cc"`
	Bcc        []EmailAddress `json:"bcc"`
	ReplyTo    []EmailAddress `json:"replyTo"`
	Subject    string         `json:"subject"`
	SentAt     *time.Time     `json:"sentAt"`

	HasAttachment bool                  `json:"hasAttachment"`
	Preview       string                `json:"preview"`
	BodyValues    map[string]*BodyValue `json:"bodyValues"`
	TextBody      []BodyPart            `json:"textBody"`
	HTMLBody      []BodyPart            `json:"htmlBody"`
	Attachments   []BodyPart            `json:"attachments"`
}

// --- Thread (RFC 8621 §3) ---

// Thread is the set of emails sharing a threadId (§3). It is
// server-computed, read-only, and has no set/query methods.
type Thread struct {
	ID       jmap.ID   `json:"id,omitempty"`
	EmailIDs []jmap.ID `json:"emailIds"`
}

// --- Backend contract ---

// Backend supplies the stores and thread lookup behind Register.
//
// Threads: ids == nil must return all threads the backend knows for
// the account; otherwise return the requested threads in request
// order, the missing ids in notFound, and the current Thread state
// string. Thread state is backend-defined and opaque to clients.
type Backend interface {
	Mailboxes() datatype.Store[Mailbox]
	Emails() datatype.Store[Email]
	Threads(ctx context.Context, account jmap.ID, ids []jmap.ID) (found []Thread, notFound []jmap.ID, state string, err error)
}

// ThreadChangeStore is the optional contract behind Thread/changes
// (RFC 8621 §3, RFC 8620 §5.2). It is only consulted when the Backend
// also implements it; otherwise Thread/changes is not registered at
// all, so clients see unknownMethod rather than invented change data.
// maxChanges == 0 means the backend chooses the limit. Implementations
// that cannot answer since sinceState should return (wrapped)
// datatype.ErrCannotCalculate, which Register turns into the
// cannotCalculateChanges method error.
type ThreadChangeStore interface {
	ThreadChanges(ctx context.Context, account jmap.ID, sinceState string, maxChanges int) (created, updated, destroyed []jmap.ID, newState string, hasMore bool, err error)
}

// Register registers Mailbox, Email and Thread methods on e under the
// urn:ietf:params:jmap:mail capability (advertised by
// datatype.Register with an empty capability object — see package doc).
func Register(e *server.Engine, b Backend) {
	datatype.Register[Mailbox](e, datatype.Options[Mailbox]{
		Name:       "Mailbox",
		Capability: jmap.MailURI,
		Store:      b.Mailboxes(),
		IDOf:       func(m Mailbox) jmap.ID { return m.ID },
		Match:      mailboxMatch,
		Compare:    mailboxCompare,
	})
	datatype.Register[Email](e, datatype.Options[Email]{
		Name:       "Email",
		Capability: jmap.MailURI,
		Store:      b.Emails(),
		IDOf:       func(em Email) jmap.ID { return em.ID },
		Match:      emailMatch,
		Compare:    emailCompare,
	})

	e.RegisterMethod("Thread/get", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
		return threadGet(ctx, call, b)
	})
	// Thread/changes is registered only when the backend can honestly
	// answer it (see ThreadChangeStore).
	if tc, ok := b.(ThreadChangeStore); ok {
		e.RegisterMethod("Thread/changes", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
			return threadChanges(ctx, call, tc)
		})
	}
}

type threadGetResponse struct {
	AccountID jmap.ID           `json:"accountId"`
	State     string            `json:"state"`
	List      []json.RawMessage `json:"list"`
	NotFound  []jmap.ID         `json:"notFound"`
}

func threadGet(ctx context.Context, call *server.Call, b Backend) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args jmap.GetArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	found, notFound, state, err := b.Threads(ctx, account, args.IDs)
	if err != nil {
		return nil, err
	}
	resp := threadGetResponse{
		AccountID: account,
		State:     state,
		List:      make([]json.RawMessage, 0, len(found)),
		NotFound:  notFound,
	}
	if resp.NotFound == nil {
		resp.NotFound = []jmap.ID{}
	}
	for _, th := range found {
		raw, err := projectThread(th, args.Properties)
		if err != nil {
			return nil, err
		}
		resp.List = append(resp.List, raw)
	}
	return resp, nil
}

// projectThread mirrors datatype's /get projection for Thread:
// keep only the requested properties, plus "id" always.
// shortcut: unknown requested properties are silently dropped (the
// same documented limitation as datatype.project).
func projectThread(th Thread, properties []string) (json.RawMessage, error) {
	raw, err := json.Marshal(th)
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

func threadChanges(ctx context.Context, call *server.Call, tc ThreadChangeStore) (any, error) {
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
	created, updated, destroyed, newState, hasMore, err := tc.ThreadChanges(ctx, account, args.SinceState, maxChanges)
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
