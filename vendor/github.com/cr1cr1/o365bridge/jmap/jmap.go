// Package jmap provides server-side wire types for the JSON Meta
// Application Protocol (JMAP), RFC 8620.
//
// This package is plain data only: it knows nothing about HTTP or
// storage. Method dispatch, result-reference resolution, and backend
// contracts live in later phases on top of these types.
package jmap

import "encoding/json"

// ID is a JMAP record identifier (RFC 8620 §1.2): a string of 1–255
// octets using only the URL- and filename-safe base64 alphabet
// (A–Z, a–z, 0–9, "-", "_"), excluding the "=" pad character.
type ID string

// Valid reports whether id satisfies the RFC 8620 §1.2 syntax. It does
// not check the RFC's defensive-allocation advice (e.g. avoiding ids
// that start with a digit); that is an allocator concern.
func (id ID) Valid() bool {
	if len(id) < 1 || len(id) > 255 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// URI is a JMAP capability URI (RFC 8620 §2). Standard capabilities are
// URNs under "urn:ietf:params:jmap:"; vendor extensions are URLs on a
// domain the vendor owns (§1.8).
type URI string

// Well-known capability URIs. This list is deliberately small: a
// capability constant is added when a data-type package in this module
// implements the corresponding specification.
const (
	// CoreURI is the mandatory JMAP core capability (RFC 8620 §2).
	CoreURI URI = "urn:ietf:params:jmap:core"
	// MailURI is the JMAP Mail capability (RFC 8621).
	MailURI URI = "urn:ietf:params:jmap:mail"
	// SubmissionURI is the JMAP mail submission capability (RFC 8621).
	SubmissionURI URI = "urn:ietf:params:jmap:submission"
	// WebSocketURI is JMAP over WebSocket (RFC 8887).
	WebSocketURI URI = "urn:ietf:params:jmap:websocket"
	// MDNURI is message disposition notifications (RFC 9007).
	MDNURI URI = "urn:ietf:params:jmap:mdn"
	// CalendarsURI is the JMAP Calendars capability (draft-ietf-jmap-calendars).
	CalendarsURI URI = "urn:ietf:params:jmap:calendars"
	// ContactsURI is the JMAP Contacts capability (draft-ietf-jmap-contacts).
	ContactsURI URI = "urn:ietf:params:jmap:contacts"
	// QuotasURI is the JMAP Quotas capability (draft-ietf-jmap-quotas).
	QuotasURI URI = "urn:ietf:params:jmap:quotas"
	// SharingURI is the JMAP Sharing capability (draft-ietf-jmap-sharing).
	SharingURI URI = "urn:ietf:params:jmap:sharing"
)

// CoreCapability is the value of the CoreURI entry in a Session's
// capabilities object (RFC 8620 §2). All limits are UnsignedInt values
// (§1.3): 0 <= v <= 2^53-1.
//
// Note: the worked example in RFC 8620 §2.1 spells one field
// "maxConcurrentRequest" (singular); the normative definition in §2 is
// "maxConcurrentRequests" (plural), which is what is used here.
type CoreCapability struct {
	MaxSizeUpload         uint64   `json:"maxSizeUpload"`
	MaxConcurrentUpload   uint64   `json:"maxConcurrentUpload"`
	MaxSizeRequest        uint64   `json:"maxSizeRequest"`
	MaxConcurrentRequests uint64   `json:"maxConcurrentRequests"`
	MaxCallsInRequest     uint64   `json:"maxCallsInRequest"`
	MaxObjectsInGet       uint64   `json:"maxObjectsInGet"`
	MaxObjectsInSet       uint64   `json:"maxObjectsInSet"`
	CollationAlgorithms   []string `json:"collationAlgorithms"`
}

// Account is a JMAP account as advertised in the Session object
// (RFC 8620 §2).
type Account struct {
	Name       string `json:"name"`
	IsPersonal bool   `json:"isPersonal"`
	IsReadOnly bool   `json:"isReadOnly"`
	// AccountCapabilities holds the per-capability account objects
	// verbatim. Their shape is defined by each capability's own
	// specification, so they are kept as raw JSON rather than decoded
	// into a guessed struct.
	AccountCapabilities map[URI]json.RawMessage `json:"accountCapabilities"`
}

// Session is the JMAP Session object returned by the session resource
// (RFC 8620 §2).
type Session struct {
	// Capabilities maps each capability URI the server supports to
	// that capability's server-level object. Values are kept raw so
	// vendor extensions and capabilities defined by later
	// specifications round-trip verbatim; use CoreCapability to decode
	// the mandatory core entry. The map MUST contain CoreURI on the
	// wire.
	Capabilities    map[URI]json.RawMessage `json:"capabilities"`
	Accounts        map[ID]*Account         `json:"accounts"`
	PrimaryAccounts map[URI]ID              `json:"primaryAccounts"`
	Username        string                  `json:"username"`
	APIURL          string                  `json:"apiUrl"`
	DownloadURL     string                  `json:"downloadUrl"`
	UploadURL       string                  `json:"uploadUrl"`
	EventSourceURL  string                  `json:"eventSourceUrl"`
	// WebSocketURL is present only when the server offers JMAP over
	// WebSocket (RFC 8887); omitted otherwise.
	WebSocketURL string `json:"webSocketUrl,omitempty"`
	// State changes whenever any other property of the Session
	// changes; it is echoed as sessionState in every Response (§3.4).
	State string `json:"state"`
}

// CoreCapability decodes the CoreURI entry of Capabilities. The second
// return value reports whether the entry was present.
func (s *Session) CoreCapability() (CoreCapability, bool, error) {
	raw, ok := s.Capabilities[CoreURI]
	if !ok {
		return CoreCapability{}, false, nil
	}
	var c CoreCapability
	if err := json.Unmarshal(raw, &c); err != nil {
		return CoreCapability{}, true, err
	}
	return c, true, nil
}

// SetCoreCapability encodes c as the CoreURI entry of Capabilities,
// allocating the map if needed.
func (s *Session) SetCoreCapability(c CoreCapability) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if s.Capabilities == nil {
		s.Capabilities = make(map[URI]json.RawMessage)
	}
	s.Capabilities[CoreURI] = raw
	return nil
}
