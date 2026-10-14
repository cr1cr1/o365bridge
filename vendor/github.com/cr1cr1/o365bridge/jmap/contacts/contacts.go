// Package contacts implements JMAP Contacts (IETF JMAP WG,
// draft-ietf-jmap-contacts) on JSContact (RFC 9553, with its vCard
// conversion context in RFC 9554 and RFC 9555).
//
// Spec pin: the data type name and method names follow the JMAP
// Contacts specification. That specification defines the data type
// name "ContactCard" (methods ContactCard/get, ContactCard/set,
// ContactCard/changes, ContactCard/query and
// ContactCard/queryChanges) whose objects are JSContact Cards: the
// object's "@type" is "Card", not "ContactCard". The AddressBook
// data type similarly provides AddressBook/get|set|changes|query
// (and queryChanges when the store supports changes). The
// capability URI is "urn:ietf:params:jmap:contacts"
// (jmap.ContactsURI), advertised by datatype.Register with an empty
// capability object for both data types.
//
// AddressBook and Card are ordinary stores: Register wires them
// through datatype.Register, which provides get/set/query (and
// changes / queryChanges when the store also implements
// datatype.ChangeStore, as datatype.Memory does).
//
// shortcut: this package does not default or validate the Card
// "@type" ("Card"), "version" ("1.0" per RFC 9553), kind values or
// other JSContact vocabularies; values supplied on create/update
// round-trip verbatim, and the backend is responsible for supplying
// them. RFC 9554/9555 vCard conversion is likewise out of scope.
package contacts

import (
	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
	"github.com/cr1cr1/o365bridge/jmap/sharing"
)

// Capability is the JMAP Contacts capability URI (jmap.ContactsURI).
const Capability = jmap.ContactsURI

// --- AddressBook (JMAP Contacts) ---

// AddressBookRights is the myRights object of an AddressBook.
// A right is true when the user holds it; there is no per-right
// null state in the specification, so plain bools are used.
type AddressBookRights struct {
	MayReadItems   bool `json:"mayReadItems"`
	MayAddItems    bool `json:"mayAddItems"`
	MayUpdateItems bool `json:"mayUpdateItems"`
	MayRemoveItems bool `json:"mayRemoveItems"`
	MayDelete      bool `json:"mayDelete"`
	MayRename      bool `json:"mayRename"`
	MayShare       bool `json:"mayShare"`
}

// AddressBook is a collection of Cards. Description is nullable in
// the specification, hence a pointer without omitempty: a nil
// pointer marshals to JSON null. ShareWith uses the RFC 9670
// SharedWith shape (see the sharing package); the right names in
// each principal's map are this data type's rights vocabulary.
type AddressBook struct {
	ID           jmap.ID            `json:"id,omitempty"`
	Name         string             `json:"name"`
	Description  *string            `json:"description"` // null when unset
	SortOrder    uint               `json:"sortOrder"`
	IsDefault    bool               `json:"isDefault"`
	IsSubscribed bool               `json:"isSubscribed"`
	ShareWith    sharing.SharedWith `json:"shareWith,omitempty"`
	MyRights     *AddressBookRights `json:"myRights"` // null when unknown
}

// --- Card (JSContact, RFC 9553) ---

// OrgUnit is one unit of an Organization (RFC 9553).
type OrgUnit struct {
	Name string `json:"name,omitempty"`
}

// Organization is one entry of a Card's organizations map.
type Organization struct {
	Name     string          `json:"name,omitempty"`
	Units    []OrgUnit       `json:"units,omitempty"`
	SortAs   *string         `json:"sortAs,omitempty"`
	Contexts map[string]bool `json:"contexts,omitempty"`
}

// NameComponent is one component of a Name (RFC 9553). Type is one
// of: surname, given, given2, surname2, credential, title,
// generation.
type NameComponent struct {
	Type     string  `json:"type"`
	Value    string  `json:"value"`
	Phonetic *string `json:"phonetic,omitempty"`
}

// Name is the Card's name object (RFC 9553): a full name and/or its
// components.
type Name struct {
	Full             string          `json:"full,omitempty"`
	Components       []NameComponent `json:"components,omitempty"`
	IsOrdered        *bool           `json:"isOrdered,omitempty"`
	DefaultSeparator *string         `json:"defaultSeparator,omitempty"`
}

// Title is one entry of a Card's titles map. OrganizationID, when
// set, is the id key of the organization in the Card's
// organizations map this title belongs to.
type Title struct {
	Name           string  `json:"name,omitempty"`
	OrganizationID *string `json:"organizationId,omitempty"`
}

// EmailAddress is one entry of a Card's emails map. Contexts is a
// JSContact contexts object, e.g. {"private":true}; Pref is the
// RFC 9553 preference value (lower is more preferred).
type EmailAddress struct {
	Address  string          `json:"address,omitempty"`
	Label    *string         `json:"label,omitempty"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     *uint           `json:"pref,omitempty"`
}

// Phone is one entry of a Card's phones map. Features is a JSContact
// features object, e.g. {"voice":true,"main-number":true}.
type Phone struct {
	Number   string          `json:"number,omitempty"`
	Features map[string]bool `json:"features,omitempty"`
	Label    *string         `json:"label,omitempty"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     *uint           `json:"pref,omitempty"`
}

// OnlineService is one entry of a Card's onlineServices map. At
// least one of Service, Uri or User is expected to be set; this
// package does not enforce that (see package doc shortcut).
type OnlineService struct {
	Service  *string         `json:"service,omitempty"`
	Uri      *string         `json:"uri,omitempty"`
	User     *string         `json:"user,omitempty"`
	Label    *string         `json:"label,omitempty"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     *uint           `json:"pref,omitempty"`
}

// AddressComponent is one component of an Address (RFC 9553), e.g.
// type: name, street, locality, region, postcode, country.
type AddressComponent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Address is one entry of a Card's addresses map.
type Address struct {
	Full        string             `json:"full,omitempty"`
	Components  []AddressComponent `json:"components,omitempty"`
	CountryCode string             `json:"countryCode,omitempty"`
	Coordinates string             `json:"coordinates,omitempty"`
	TimeZone    string             `json:"timeZone,omitempty"`
	Contexts    map[string]bool    `json:"contexts,omitempty"`
	Pref        *uint              `json:"pref,omitempty"`
}

// Photo is one entry of a Card's photos map. RFC 9553 represents a
// photo by URI (typically a data: URI or a blob reference); only
// the URI and its media type are modelled here.
type Photo struct {
	Uri       string  `json:"uri,omitempty"`
	MediaType *string `json:"mediaType,omitempty"`
}

// PartialDate is a JSContact partial date (RFC 9553): any of year,
// month and day may be absent, e.g. {month:4, day:25} renders the
// "--04-25" form of earlier drafts.
type PartialDate struct {
	Year  *int `json:"year,omitempty"`
	Month *int `json:"month,omitempty"`
	Day   *int `json:"day,omitempty"`
}

// Anniversary is one entry of a Card's anniversaries map. Type is
// e.g. birth, death, wedding. Date is a PartialDate.
type Anniversary struct {
	Type  string       `json:"type,omitempty"`
	Date  *PartialDate `json:"date,omitempty"`
	Label *string      `json:"label,omitempty"`
}

// Note is one entry of a Card's notes map.
//
// shortcut: the RFC 9553 Note author/created properties are not
// modelled; only the note text round-trips.
type Note struct {
	Note string `json:"note,omitempty"`
}

// PersonalInfo is one entry of a Card's personalInfo map. Type is
// one of: expertise, hobby, interest, contact. Level is e.g.
// high, medium, low.
type PersonalInfo struct {
	Type  string  `json:"type,omitempty"`
	Value string  `json:"value,omitempty"`
	Level *string `json:"level,omitempty"`
}

// PreferredLanguage is one entry of a Card's preferredLanguages map.
type PreferredLanguage struct {
	Language string          `json:"language,omitempty"`
	Contexts map[string]bool `json:"contexts,omitempty"`
	Pref     *uint           `json:"pref,omitempty"`
}

// Card is a JSContact Card (RFC 9553) as carried by JMAP Contacts:
// the wire object has "@type":"Card". AddressBookIDs is the JMAP
// Contacts addition recording which AddressBooks the card belongs
// to. Created and Updated are UTC date-time strings as in RFC 9553.
// Members is only meaningful for group cards (kind "group") and
// maps member Card ids (or URIs) to true.
type Card struct {
	Type               string                        `json:"@type"` // "Card"
	ID                 jmap.ID                       `json:"id,omitempty"`
	Version            string                        `json:"version,omitempty"` // JSContact "version" (RFC 9553; early drafts used "@version")
	Kind               string                        `json:"kind,omitempty"`    // individual/group/org/location/device/application
	Language           string                        `json:"language,omitempty"`
	Name               *Name                         `json:"name,omitempty"`
	Organizations      map[string]*Organization      `json:"organizations,omitempty"`
	Titles             map[string]*Title             `json:"titles,omitempty"`
	Emails             map[string]*EmailAddress      `json:"emails,omitempty"`
	Phones             map[string]*Phone             `json:"phones,omitempty"`
	OnlineServices     map[string]*OnlineService     `json:"onlineServices,omitempty"`
	Addresses          map[string]*Address           `json:"addresses,omitempty"`
	Photos             map[string]*Photo             `json:"photos,omitempty"`
	Anniversaries      map[string]*Anniversary       `json:"anniversaries,omitempty"`
	Notes              map[string]*Note              `json:"notes,omitempty"`
	PersonalInfo       map[string]*PersonalInfo      `json:"personalInfo,omitempty"`
	PreferredLanguages map[string]*PreferredLanguage `json:"preferredLanguages,omitempty"`
	Created            string                        `json:"created,omitempty"`
	Updated            string                        `json:"updated,omitempty"`
	Members            map[string]bool               `json:"members,omitempty"` // group cards
	ProdID             string                        `json:"prodId,omitempty"`
	AddressBookIDs     map[jmap.ID]bool              `json:"addressBookIds"`
}

// --- Backend contract ---

// Backend supplies the stores behind Register.
type Backend interface {
	AddressBooks() datatype.Store[AddressBook]
	Cards() datatype.Store[Card]
}

// Register registers AddressBook and ContactCard methods on e under
// the urn:ietf:params:jmap:contacts capability (advertised by
// datatype.Register with an empty capability object — see package
// doc). Note the deliberate asymmetry: the AddressBook data type's
// name equals its object name, while the Card object is served by
// the ContactCard data type.
func Register(e *server.Engine, b Backend) {
	datatype.Register[AddressBook](e, datatype.Options[AddressBook]{
		Name:       "AddressBook",
		Capability: Capability,
		Store:      b.AddressBooks(),
		IDOf:       func(a AddressBook) jmap.ID { return a.ID },
		Match:      addressBookMatch,
		Compare:    addressBookCompare,
	})
	datatype.Register[Card](e, datatype.Options[Card]{
		Name:       "ContactCard",
		Capability: Capability,
		Store:      b.Cards(),
		IDOf:       func(c Card) jmap.ID { return c.ID },
		Match:      cardMatch,
		Compare:    cardCompare,
	})
}
