package owa

import (
	"context"
	"fmt"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// Contacts use the Persona API (the reference OWA contacts
// integration does the same; there is no Contact item CreateItem).
// (FindPeople / CreatePersona / UpdatePersona / DeletePersonas), not
// Contact:#Exchange CreateItem items — no such shape exists in _o.

func personaToContact(pers map[string]any, bookID string) bridge.Contact {
	c := bridge.Contact{
		ID:        firstNonEmpty(str(pers, "PersonaId", "Id"), str(pers, "ItemId", "Id")),
		BookID:    bookID,
		FirstName: str(pers, "GivenName"), LastName: str(pers, "Surname"),
		FullName: str(pers, "DisplayName"), Org: str(pers, "CompanyName"),
		Notes: str(pers, "Notes"),
	}
	if c.FullName == "" {
		c.FullName = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	c.UID = c.ID
	for _, e := range arr(pers, "EmailAddresses") {
		if addr := firstNonEmpty(str(e, "EmailAddress"), str(e, "Address")); addr != "" {
			c.Emails = append(c.Emails, addr)
		} else if s, ok := e.(string); ok {
			c.Emails = append(c.Emails, s)
		}
	}
	if len(c.Emails) == 0 {
		if addr := str(pers, "EmailAddress", "EmailAddress"); addr != "" {
			c.Emails = append(c.Emails, addr)
		}
	}
	for _, key := range []string{"HomePhonesArray", "BusinessPhoneNumbersArray", "MobilePhonesArray", "WorkFaxesArray"} {
		for _, ph := range arr(pers, key) {
			n := firstNonEmpty(str(ph, "Value", "Number"), str(ph, "Number"))
			if n == "" {
				if s, ok := ph.(string); ok {
					n = s
				}
			}
			if n != "" {
				c.Phones = append(c.Phones, n)
			}
		}
	}
	return c
}

func (p *provider) ListContacts(ctx context.Context, bookID string) ([]bridge.Contact, error) {
	// _o FindPeople needs the filter's FolderId; fetch filters first,
	// fall back to the distinguished contacts folder.
	parent := map[string]any{"__type": "TargetFolderId:#Exchange", "BaseFolderId": folderRef(orDefault(bookID, "contacts"))}
	if bookID == "" || bookID == "contacts" {
		if res, err := p.call(ctx, "GetPeopleFilters", map[string]any{}); err == nil {
			for _, f := range arr(res) {
				if !boolean(f, "IsReadOnly") {
					if fid := dig(f, "FolderId"); fid != nil {
						parent = map[string]any{"__type": "TargetFolderId:#Exchange", "BaseFolderId": fid}
					}
					break
				}
			}
			// GetPeopleFilters may return a bare array at top level.
			if m := asMap(res); m != nil {
				for _, f := range arr(m, "Filters") {
					if !boolean(f, "IsReadOnly") {
						if fid := dig(f, "FolderId"); fid != nil {
							parent = map[string]any{"__type": "TargetFolderId:#Exchange", "BaseFolderId": fid}
						}
						break
					}
				}
			}
		}
	}
	var out []bridge.Contact
	offset := 0
	for {
		req := envelope("FindPeopleJsonRequest:#Exchange", "FindPeopleRequest:#Exchange", map[string]any{
			"IndexedPageItemView": map[string]any{"__type": "IndexedPageView:#Exchange", "BasePoint": "Beginning", "Offset": offset, "MaxEntriesReturned": 100},
			"ParentFolderId":      parent,
			"PersonaShape":        map[string]any{"__type": "PersonaResponseShape:#Exchange", "BaseShape": "Default"},
			"QueryString":         nil, "SearchPeopleSuggestionIndex": false, "ShouldResolveOneOffEmailAddress": false,
		})
		res, err := p.call(ctx, "FindPeople", req)
		if err != nil {
			return nil, err
		}
		results := arr(res, "ResultSet")
		for _, r := range results {
			if m := asMap(r); m != nil {
				out = append(out, personaToContact(m, bookID))
			}
		}
		if len(results) < 100 {
			break
		}
		offset += 100
	}
	return out, nil
}

// personaUpdates builds PersonaPropertyUpdate entries per
// OWAContactFields holds the Persona field names.
func personaUpdates(c bridge.Contact) []any {
	type kv struct{ field, val string }
	var fields []kv
	full := c.FullName
	if full == "" {
		full = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	_ = full
	fields = append(fields,
		kv{"PersonaGivenNames", c.FirstName}, kv{"PersonaSurnames", c.LastName},
		kv{"PersonaCompanyNames", c.Org}, kv{"PersonaBodies", c.Notes},
	)
	if len(c.Emails) > 0 {
		fields = append(fields, kv{"PersonaEmails1", c.Emails[0]}, kv{"PersonaEmails1OriginalDisplayNames", c.Emails[0]})
	}
	if len(c.Emails) > 1 {
		fields = append(fields, kv{"PersonaEmails2", c.Emails[1]}, kv{"PersonaEmails2OriginalDisplayNames", c.Emails[1]})
	}
	if len(c.Phones) > 0 {
		fields = append(fields, kv{"PersonaMobilePhones", c.Phones[0]})
	}
	var updates []any
	for _, f := range fields {
		if f.val == "" {
			continue
		}
		updates = append(updates, map[string]any{
			"__type": "PersonaPropertyUpdate:#Exchange",
			"Path":   propURI(f.field), "OldValue": "", "NewValue": f.val,
		})
	}
	return updates
}

func (p *provider) createOrUpdatePersona(ctx context.Context, c bridge.Contact, update bool) (string, error) {
	typ := "Create"
	if update {
		typ = "Update"
	}
	body := map[string]any{
		"__type": typ + "PersonaRequest:#Exchange", "PersonTypeString": "Person",
		"PropertyUpdates": personaUpdates(c),
	}
	if update {
		body["PersonaId"] = itemID(c.ID)
	} else {
		body["PersonaId"] = nil
		body["ParentFolderId"] = targetFolder(orDefault(c.BookID, "contacts"))
	}
	req := map[string]any{"request": map[string]any{
		"__type": typ + "PersonaJsonRequest:#Exchange", "Header": hdr(), "Body": body,
	}}
	res, err := p.call(ctx, typ+"Persona", req)
	if err != nil {
		return "", err
	}
	if id := str(res, "PersonaId", "Id"); id != "" {
		return id, nil
	}
	if item := firstItem(res); item != nil {
		return firstNonEmpty(str(item, "PersonaId", "Id"), str(item, "ItemId", "Id")), nil
	}
	return "", nil
}

func (p *provider) CreateContact(ctx context.Context, c bridge.Contact) (bridge.Contact, error) {
	id, err := p.createOrUpdatePersona(ctx, c, false)
	if err != nil {
		return bridge.Contact{}, err
	}
	c.ID = id
	c.UID = id
	return c, nil
}

func (p *provider) UpdateContact(ctx context.Context, c bridge.Contact) error {
	if c.ID == "" {
		return fmt.Errorf("owa: UpdateContact: missing contact id")
	}
	_, err := p.createOrUpdatePersona(ctx, c, true)
	return err
}

func (p *provider) DeleteContact(ctx context.Context, bookID, id string) error {
	req := map[string]any{"request": map[string]any{
		"__type":  "DeletePersonasRequest:#Exchange",
		"ItemIds": itemIDs([]string{id}),
	}}
	_, err := p.call(ctx, "DeletePersonas", req)
	return err
}
