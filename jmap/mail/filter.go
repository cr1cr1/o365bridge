package mail

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// Filter evaluation shared by Mailbox and Email /query.
//
// A filter is either a FilterOperator
// {"operator":"AND"|"OR"|"NOT","conditions":[...]} (recursively) or a
// single condition object whose present properties are ANDed together
// (RFC 8620 §5.5, RFC 8621 §2.3/§5). Unknown condition properties are
// an error, which datatype turns into invalidArguments.
//
// Each condition evaluator takes the raw condition object; operator
// detection is by the presence of an "operator" property.

type conditionFunc[T any] func(obj T, cond json.RawMessage) (bool, error)

func evalFilter[T any](obj T, filter json.RawMessage, cond conditionFunc[T]) (bool, error) {
	trimmed := strings.TrimSpace(string(filter))
	if trimmed == "" || trimmed == "null" {
		return true, nil
	}
	var probe struct {
		Operator *string `json:"operator"`
	}
	if err := json.Unmarshal(filter, &probe); err != nil {
		return false, err
	}
	if probe.Operator != nil {
		var op jmap.FilterOperator
		if err := json.Unmarshal(filter, &op); err != nil {
			return false, err
		}
		if err := checkKeys(filter, map[string]bool{"operator": true, "conditions": true}); err != nil {
			return false, err
		}
		switch op.Operator {
		case "AND":
			for _, c := range op.Conditions {
				ok, err := evalFilter(obj, c, cond)
				if err != nil {
					return false, err
				}
				if !ok {
					return false, nil
				}
			}
			return true, nil
		case "OR":
			for _, c := range op.Conditions {
				ok, err := evalFilter(obj, c, cond)
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		case "NOT":
			// NOT matches when none of its conditions match.
			for _, c := range op.Conditions {
				ok, err := evalFilter(obj, c, cond)
				if err != nil {
					return false, err
				}
				if ok {
					return false, nil
				}
			}
			return true, nil
		default:
			return false, fmt.Errorf("mail: unsupported filter operator %q", op.Operator)
		}
	}
	return cond(obj, filter)
}

// checkKeys errors when raw (a JSON object) has a key outside allowed.
func checkKeys(raw json.RawMessage, allowed map[string]bool) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	for k := range m {
		if !allowed[k] {
			return fmt.Errorf("mail: unknown filter condition %q", k)
		}
	}
	return nil
}

func cmpUint(a, b uint) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// --- Mailbox filters (RFC 8621 §2.3) and sorts (§2.3) ---

// mailboxCondition mirrors the Mailbox FilterCondition of §2.3.
// parentId may be explicitly null (matching top-level mailboxes), so
// its presence/null-ness is read from the raw object, not this struct.
type mailboxCondition struct {
	Name         *string `json:"name"`
	Role         *string `json:"role"`
	HasAnyRole   *bool   `json:"hasAnyRole"`
	IsSubscribed *bool   `json:"isSubscribed"`
}

func mailboxMatch(m Mailbox, filter json.RawMessage) (bool, error) {
	return evalFilter(m, filter, mailboxCond)
}

func mailboxCond(m Mailbox, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, map[string]bool{
		"parentId": true, "name": true, "role": true,
		"hasAnyRole": true, "isSubscribed": true,
	}); err != nil {
		return false, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, err
	}
	var c mailboxCondition
	if err := json.Unmarshal(raw, &c); err != nil {
		return false, err
	}
	if rawParent, present := obj["parentId"]; present {
		if string(rawParent) == "null" {
			if m.ParentID != nil {
				return false, nil
			}
		} else {
			var want jmap.ID
			if err := json.Unmarshal(rawParent, &want); err != nil {
				return false, err
			}
			if m.ParentID == nil || *m.ParentID != want {
				return false, nil
			}
		}
	}
	// RFC 8621 §2.3: "name" must match the Mailbox name exactly.
	if c.Name != nil && m.Name != *c.Name {
		return false, nil
	}
	if c.Role != nil && (m.Role == nil || *m.Role != *c.Role) {
		return false, nil
	}
	if c.HasAnyRole != nil {
		has := m.Role != nil && *m.Role != ""
		if has != *c.HasAnyRole {
			return false, nil
		}
	}
	if c.IsSubscribed != nil && m.IsSubscribed != *c.IsSubscribed {
		return false, nil
	}
	return true, nil
}

// mailboxCompare orders ascending by comparator.Property; datatype
// applies Comparator.IsAscending (so this never inverts itself).
func mailboxCompare(a, b Mailbox, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "name":
		return strings.Compare(a.Name, b.Name), true
	case "sortOrder":
		return cmpUint(a.SortOrder, b.SortOrder), true
	}
	return 0, false
}

// --- Email filters (RFC 8621 §5) and sorts (§5) ---

type emailCondition struct {
	InMailbox               *jmap.ID   `json:"inMailbox"`
	InMailboxOtherThan      []jmap.ID  `json:"inMailboxOtherThan"`
	Before                  *time.Time `json:"before"`
	After                   *time.Time `json:"after"`
	MinSize                 *uint      `json:"minSize"`
	MaxSize                 *uint      `json:"maxSize"`
	AllInThreadHaveKeyword  *string    `json:"allInThreadHaveKeyword"`
	SomeInThreadHaveKeyword *string    `json:"someInThreadHaveKeyword"`
	NoneInThreadHaveKeyword *string    `json:"noneInThreadHaveKeyword"`
	HasKeyword              *string    `json:"hasKeyword"`
	NotKeyword              *string    `json:"notKeyword"`
	HasAttachment           *bool      `json:"hasAttachment"`
	Text                    *string    `json:"text"`
	From                    *string    `json:"from"`
	To                      *string    `json:"to"`
	Cc                      *string    `json:"cc"`
	Bcc                     *string    `json:"bcc"`
	Subject                 *string    `json:"subject"`
	Body                    *string    `json:"body"`
	Header                  []string   `json:"header"`
}

var emailCondKeys = map[string]bool{
	"inMailbox": true, "inMailboxOtherThan": true, "before": true,
	"after": true, "minSize": true, "maxSize": true,
	"allInThreadHaveKeyword": true, "someInThreadHaveKeyword": true,
	"noneInThreadHaveKeyword": true, "hasKeyword": true,
	"notKeyword": true, "hasAttachment": true, "text": true,
	"from": true, "to": true, "cc": true, "bcc": true,
	"subject": true, "body": true, "header": true,
}

func emailMatch(e Email, filter json.RawMessage) (bool, error) {
	return evalFilter(e, filter, emailCond)
}

// EmailMatch reports whether e matches a /query filter (the raw
// FilterCondition/FilterOperator JSON). An empty or "null" filter
// matches everything. It is the exported form of the Match hook used
// by Register, for callers implementing their own Email/query handler
// (e.g. over light, metadata-only Email values).
func EmailMatch(obj Email, filter json.RawMessage) (bool, error) {
	return emailMatch(obj, filter)
}

func emailCond(e Email, raw json.RawMessage) (bool, error) {
	if err := checkKeys(raw, emailCondKeys); err != nil {
		return false, err
	}
	var c emailCondition
	if err := json.Unmarshal(raw, &c); err != nil {
		return false, err
	}
	if c.InMailbox != nil && !e.MailboxIDs[*c.InMailbox] {
		return false, nil
	}
	if c.InMailboxOtherThan != nil {
		excluded := make(map[jmap.ID]bool, len(c.InMailboxOtherThan))
		for _, id := range c.InMailboxOtherThan {
			excluded[id] = true
		}
		other := false
		for id := range e.MailboxIDs {
			if !excluded[id] {
				other = true
				break
			}
		}
		if !other {
			return false, nil
		}
	}
	if c.Before != nil && !e.ReceivedAt.Before(*c.Before) {
		return false, nil
	}
	if c.After != nil && !e.ReceivedAt.After(*c.After) {
		return false, nil
	}
	if c.MinSize != nil && e.Size < *c.MinSize {
		return false, nil
	}
	if c.MaxSize != nil && e.Size > *c.MaxSize {
		return false, nil
	}
	// shortcut: thread-wide keyword conditions
	// (allInThreadHaveKeyword / someInThreadHaveKeyword /
	// noneInThreadHaveKeyword) are evaluated against this email's own
	// keywords only, since store objects are single emails; a backend
	// needing true thread semantics must pre-compute them.
	if c.AllInThreadHaveKeyword != nil && !e.Keywords[*c.AllInThreadHaveKeyword] {
		return false, nil
	}
	if c.SomeInThreadHaveKeyword != nil && !e.Keywords[*c.SomeInThreadHaveKeyword] {
		return false, nil
	}
	if c.NoneInThreadHaveKeyword != nil && e.Keywords[*c.NoneInThreadHaveKeyword] {
		return false, nil
	}
	if c.HasKeyword != nil && !e.Keywords[*c.HasKeyword] {
		return false, nil
	}
	if c.NotKeyword != nil && e.Keywords[*c.NotKeyword] {
		return false, nil
	}
	if c.HasAttachment != nil && e.HasAttachment != *c.HasAttachment {
		return false, nil
	}
	if c.Text != nil && !textSearch(e, *c.Text) {
		return false, nil
	}
	// shortcut: "body" uses the same search space as "text" (subject,
	// addresses, preview). Full body text is not part of the Email
	// object searched here beyond its preview/bodyValues metadata.
	if c.Body != nil && !textSearch(e, *c.Body) {
		return false, nil
	}
	if c.From != nil && !addrSearch(addressLists(e.Sender, e.From), *c.From) {
		return false, nil
	}
	if c.To != nil && !addrSearch(e.To, *c.To) {
		return false, nil
	}
	if c.Cc != nil && !addrSearch(e.Cc, *c.Cc) {
		return false, nil
	}
	if c.Bcc != nil && !addrSearch(e.Bcc, *c.Bcc) {
		return false, nil
	}
	if c.Subject != nil && !containsFold(e.Subject, *c.Subject) {
		return false, nil
	}
	if c.Header != nil {
		ok, err := headerMatch(e, c.Header)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func addressLists(sender *EmailAddress, list []EmailAddress) []EmailAddress {
	if sender == nil {
		return list
	}
	out := make([]EmailAddress, 0, len(list)+1)
	out = append(out, *sender)
	return append(out, list...)
}

// headerMatch implements the "header" condition ([name] or
// [name, value]).
//
// shortcut: Email objects here retain only the Message-ID header (in
// messageId). header:["Message-ID", v] matches when v is a
// case-insensitive substring of a messageId entry; any other header
// name never matches.
func headerMatch(e Email, header []string) (bool, error) {
	if len(header) != 1 && len(header) != 2 {
		return false, fmt.Errorf("mail: header condition must be [name] or [name, value]")
	}
	if !strings.EqualFold(header[0], "Message-ID") {
		return false, nil
	}
	if len(header) == 1 {
		return len(e.MessageID) > 0, nil
	}
	for _, id := range e.MessageID {
		if containsFold(id, header[1]) {
			return true, nil
		}
	}
	return false, nil
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// textSearch is the search space for the text/body conditions:
// subject, preview, and every address (sender/from/to/cc/bcc/replyTo)
// name and email, case-insensitive substring.
func textSearch(e Email, q string) bool {
	if containsFold(e.Subject, q) || containsFold(e.Preview, q) {
		return true
	}
	var lists [][]EmailAddress
	if e.Sender != nil {
		lists = append(lists, []EmailAddress{*e.Sender})
	}
	lists = append(lists, e.From, e.To, e.Cc, e.Bcc, e.ReplyTo)
	for _, list := range lists {
		if addrSearch(list, q) {
			return true
		}
	}
	return false
}

func addrSearch(list []EmailAddress, q string) bool {
	for _, a := range list {
		if containsFold(a.Name, q) || containsFold(a.Email, q) {
			return true
		}
	}
	return false
}

// EmailCompare orders a and b ascending by c.Property, returning
// ok == false when the property is unsupported. It is the exported
// form of the Compare hook used by Register (see EmailMatch); the
// caller applies Comparator.IsAscending.
func EmailCompare(a, b Email, c jmap.Comparator) (int, bool) {
	return emailCompare(a, b, c)
}

// emailCompare orders ascending by comparator.Property (nil sentAt
// counts as the zero time); datatype applies Comparator.IsAscending.
func emailCompare(a, b Email, c jmap.Comparator) (int, bool) {
	switch c.Property {
	case "receivedAt":
		return a.ReceivedAt.Compare(b.ReceivedAt), true
	case "sentAt":
		var at, bt time.Time
		if a.SentAt != nil {
			at = *a.SentAt
		}
		if b.SentAt != nil {
			bt = *b.SentAt
		}
		return at.Compare(bt), true
	case "size":
		return cmpUint(a.Size, b.Size), true
	case "subject":
		return strings.Compare(a.Subject, b.Subject), true
	case "from":
		return strings.Compare(firstEmail(a.From), firstEmail(b.From)), true
	}
	return 0, false
}

func firstEmail(list []EmailAddress) string {
	if len(list) == 0 {
		return ""
	}
	return list[0].Email
}
