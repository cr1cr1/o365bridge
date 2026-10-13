package davserver

import (
	"fmt"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// --- minimal vCard 3.0 codec (RFC 2426 subset) ---
// Maps UID, FN, N, EMAIL, TEL, ORG, NOTE to bridge.Contact.

func generateVCard(c bridge.Contact) []byte {
	fn := c.FullName
	if fn == "" {
		fn = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	lines := []string{"BEGIN:VCARD", "VERSION:3.0"}
	if c.UID != "" {
		lines = append(lines, "UID:"+c.UID)
	}
	lines = append(lines, "FN:"+escapeText(fn))
	lines = append(lines, "N:"+escapeText(c.LastName)+";"+escapeText(c.FirstName)+";;;")
	for _, e := range c.Emails {
		lines = append(lines, "EMAIL;TYPE=INTERNET:"+escapeText(e))
	}
	for _, ph := range c.Phones {
		lines = append(lines, "TEL:"+escapeText(ph))
	}
	if c.Org != "" {
		lines = append(lines, "ORG:"+escapeText(c.Org))
	}
	if c.Notes != "" {
		lines = append(lines, "NOTE:"+escapeText(c.Notes))
	}
	lines = append(lines, "END:VCARD")
	return joinCRLF(lines)
}

func parseVCard(data []byte) (bridge.Contact, error) {
	var c bridge.Contact
	saw := false
	for _, line := range unfold(data) {
		if line == "" {
			continue
		}
		prop, ok := parseProp(line)
		if !ok {
			continue
		}
		switch prop.Name {
		case "BEGIN":
			if strings.EqualFold(prop.Value, "VCARD") {
				saw = true
			}
		case "UID":
			c.UID = prop.Value
		case "FN":
			c.FullName = unescapeText(prop.Value)
		case "N":
			// N: Last;First;Additional;Prefix;Suffix (fields escaped)
			parts := strings.Split(prop.Value, ";")
			// split respects escaped \; poorly; acceptable for subset:
			if len(parts) > 0 {
				c.LastName = unescapeText(parts[0])
			}
			if len(parts) > 1 {
				c.FirstName = unescapeText(parts[1])
			}
		case "EMAIL":
			c.Emails = append(c.Emails, unescapeText(prop.Value))
		case "TEL":
			c.Phones = append(c.Phones, unescapeText(prop.Value))
		case "ORG":
			c.Org = unescapeText(strings.Split(prop.Value, ";")[0])
		case "NOTE":
			c.Notes = unescapeText(prop.Value)
		}
	}
	if !saw {
		return bridge.Contact{}, fmt.Errorf("davserver: not a vCard object")
	}
	if c.FullName == "" {
		c.FullName = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	return c, nil
}
