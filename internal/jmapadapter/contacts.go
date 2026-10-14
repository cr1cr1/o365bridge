package jmapadapter

import (
	"context"
	"fmt"
	"sort"
	"strings"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/contacts"
	"github.com/cr1cr1/o365bridge/jmap/datatype"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// contactsBookID is the single book id the DAV server also uses
// (davserver.contactsBookID): upstream contacts live in one book.
const contactsBookID = "contacts"

func (b *Backend) AddressBooks() datatype.Store[contacts.AddressBook] { return &addressBookStore{b} }
func (b *Backend) Cards() datatype.Store[contacts.Card]               { return &cardStore{b} }

// --- AddressBook store ---

type addressBookStore struct{ b *Backend }

func theAddressBook() contacts.AddressBook {
	return contacts.AddressBook{
		ID: jmap.ID(contactsBookID), Name: "Contacts", IsDefault: true, IsSubscribed: true,
		MyRights: &contacts.AddressBookRights{
			MayReadItems: true, MayAddItems: true, MayUpdateItems: true, MayRemoveItems: true,
			MayDelete: true, MayRename: true, MayShare: true,
		},
	}
}

func (s *addressBookStore) State(context.Context, jmap.ID) (string, error) {
	bk := theAddressBook()
	return hashStrings([]string{string(bk.ID) + "|" + bk.Name}), nil
}

func (s *addressBookStore) Get(_ context.Context, _ jmap.ID, ids []jmap.ID) ([]contacts.AddressBook, []jmap.ID, error) {
	bk := theAddressBook()
	if ids == nil {
		return []contacts.AddressBook{bk}, nil, nil
	}
	var found []contacts.AddressBook
	var notFound []jmap.ID
	for _, id := range ids {
		if id == bk.ID {
			found = append(found, bk)
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

// The provider has no address-book listing/CRUD: the single book is a
// fixed collection, so mutations are forbidden.
func forbiddenBooks() *jmap.SetError {
	return &jmap.SetError{Type: "forbidden", Description: "upstream address books are fixed collections"}
}

func (s *addressBookStore) Create(context.Context, jmap.ID, contacts.AddressBook) (contacts.AddressBook, *jmap.SetError) {
	return contacts.AddressBook{}, forbiddenBooks()
}
func (s *addressBookStore) Update(context.Context, jmap.ID, contacts.AddressBook) (*contacts.AddressBook, *jmap.SetError) {
	return nil, forbiddenBooks()
}
func (s *addressBookStore) Destroy(context.Context, jmap.ID, jmap.ID) *jmap.SetError {
	return forbiddenBooks()
}

func (s *addressBookStore) Changes(ctx context.Context, account jmap.ID, sinceState string, _ int) ([]jmap.ID, []jmap.ID, []jmap.ID, string, bool, error) {
	cur, err := s.State(ctx, account)
	if err != nil {
		return nil, nil, nil, "", false, err
	}
	if sinceState == cur {
		return []jmap.ID{}, []jmap.ID{}, []jmap.ID{}, cur, false, nil
	}
	return nil, nil, nil, "", false, datatype.ErrCannotCalculate
}

// --- Card conversion ---

func contactToCard(c bridge.Contact) contacts.Card {
	card := contacts.Card{
		Type: "Card", Version: "1.0", Kind: "individual",
		ID:             jmap.ID(c.ID),
		AddressBookIDs: map[jmap.ID]bool{jmap.ID(contactsBookID): true},
	}
	if c.FullName != "" || c.FirstName != "" || c.LastName != "" {
		name := &contacts.Name{Full: c.FullName}
		if c.FirstName != "" {
			name.Components = append(name.Components, contacts.NameComponent{Type: "given", Value: c.FirstName})
		}
		if c.LastName != "" {
			name.Components = append(name.Components, contacts.NameComponent{Type: "surname", Value: c.LastName})
		}
		if name.Full == "" {
			name.Full = strings.TrimSpace(c.FirstName + " " + c.LastName)
		}
		card.Name = name
	}
	if len(c.Emails) > 0 {
		card.Emails = make(map[string]*contacts.EmailAddress, len(c.Emails))
		for i, e := range c.Emails {
			card.Emails[fmt.Sprintf("e%d", i+1)] = &contacts.EmailAddress{Address: e}
		}
	}
	if len(c.Phones) > 0 {
		card.Phones = make(map[string]*contacts.Phone, len(c.Phones))
		for i, p := range c.Phones {
			card.Phones[fmt.Sprintf("p%d", i+1)] = &contacts.Phone{Number: p}
		}
	}
	if c.Org != "" {
		card.Organizations = map[string]*contacts.Organization{"o1": {Name: c.Org}}
	}
	if c.Notes != "" {
		card.Notes = map[string]*contacts.Note{"n1": {Note: c.Notes}}
	}
	return card
}

func cardToContact(card contacts.Card) bridge.Contact {
	c := bridge.Contact{ID: string(card.ID), BookID: contactsBookID}
	if card.Name != nil {
		c.FullName = card.Name.Full
		for _, comp := range card.Name.Components {
			switch comp.Type {
			case "given":
				if c.FirstName == "" {
					c.FirstName = comp.Value
				}
			case "surname":
				if c.LastName == "" {
					c.LastName = comp.Value
				}
			}
		}
		if c.FullName == "" && (c.FirstName != "" || c.LastName != "") {
			c.FullName = strings.TrimSpace(c.FirstName + " " + c.LastName)
		}
	}
	if (c.FirstName == "" && c.LastName == "") && c.FullName != "" {
		if i := strings.LastIndex(c.FullName, " "); i >= 0 {
			c.FirstName, c.LastName = c.FullName[:i], c.FullName[i+1:]
		} else {
			c.FirstName = c.FullName
		}
	}
	for _, k := range sortedMapKeys(card.Emails) {
		if e := card.Emails[k]; e != nil && e.Address != "" {
			c.Emails = append(c.Emails, e.Address)
		}
	}
	for _, k := range sortedMapKeys(card.Phones) {
		if p := card.Phones[k]; p != nil && p.Number != "" {
			c.Phones = append(c.Phones, p.Number)
		}
	}
	for _, k := range sortedMapKeys(card.Organizations) {
		if o := card.Organizations[k]; o != nil && o.Name != "" {
			c.Org = o.Name
			break
		}
	}
	for _, k := range sortedMapKeys(card.Notes) {
		if n := card.Notes[k]; n != nil && n.Note != "" {
			c.Notes = n.Note
			break
		}
	}
	return c
}

// --- Card store ---

type cardStore struct{ b *Backend }

func (s *cardStore) list(ctx context.Context, account jmap.ID) ([]bridge.Contact, error) {
	var out []bridge.Contact
	err := s.b.call(ctx, string(account), "ListContacts", func(ctx context.Context, p bridge.Provider) error {
		var err error
		out, err = p.ListContacts(ctx, contactsBookID)
		return err
	})
	return out, err
}

func (s *cardStore) State(ctx context.Context, account jmap.ID) (string, error) {
	list, err := s.list(ctx, account)
	if err != nil {
		return "", err
	}
	sorted := append([]bridge.Contact(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	parts := make([]string, 0, len(sorted))
	for _, c := range sorted {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s",
			c.ID, c.FullName, c.FirstName, c.LastName,
			strings.Join(c.Emails, ","), strings.Join(c.Phones, ","), c.Org, c.Notes))
	}
	return hashStrings(parts), nil
}

func (s *cardStore) Get(ctx context.Context, account jmap.ID, ids []jmap.ID) ([]contacts.Card, []jmap.ID, error) {
	list, err := s.list(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	if ids == nil {
		out := make([]contacts.Card, 0, len(list))
		for _, c := range list {
			out = append(out, contactToCard(c))
		}
		return out, nil, nil
	}
	byID := map[jmap.ID]bridge.Contact{}
	for _, c := range list {
		byID[jmap.ID(c.ID)] = c
	}
	var found []contacts.Card
	var notFound []jmap.ID
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			found = append(found, contactToCard(c))
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

func (s *cardStore) Create(ctx context.Context, account jmap.ID, obj contacts.Card) (contacts.Card, *jmap.SetError) {
	c := cardToContact(obj)
	var created bridge.Contact
	if err := s.b.call(ctx, string(account), "CreateContact", func(ctx context.Context, p bridge.Provider) error {
		var err error
		created, err = p.CreateContact(ctx, c)
		return err
	}); err != nil {
		return contacts.Card{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	return contactToCard(created), nil
}

func (s *cardStore) Update(ctx context.Context, account jmap.ID, obj contacts.Card) (*contacts.Card, *jmap.SetError) {
	c := cardToContact(obj)
	if err := s.b.call(ctx, string(account), "UpdateContact", func(ctx context.Context, p bridge.Provider) error {
		return p.UpdateContact(ctx, c)
	}); err != nil {
		return nil, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	return nil, nil
}

func (s *cardStore) Destroy(ctx context.Context, account jmap.ID, id jmap.ID) *jmap.SetError {
	if err := s.b.call(ctx, string(account), "DeleteContact", func(ctx context.Context, p bridge.Provider) error {
		return p.DeleteContact(ctx, contactsBookID, string(id))
	}); err != nil {
		return &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	return nil
}

func (s *cardStore) Changes(ctx context.Context, account jmap.ID, sinceState string, _ int) ([]jmap.ID, []jmap.ID, []jmap.ID, string, bool, error) {
	cur, err := s.State(ctx, account)
	if err != nil {
		return nil, nil, nil, "", false, err
	}
	if sinceState == cur {
		return []jmap.ID{}, []jmap.ID{}, []jmap.ID{}, cur, false, nil
	}
	return nil, nil, nil, "", false, datatype.ErrCannotCalculate
}
