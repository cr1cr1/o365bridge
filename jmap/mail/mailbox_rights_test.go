package mail_test

// RFC 8621 §2: the Mailbox myRights object has exactly nine
// mayXxx properties. Strict JMAP clients (himalaya) fail to decode
// a Mailbox when one is missing, so the shape is pinned here.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cr1cr1/o365bridge/jmap/mail"
)

func TestMailboxRightsJSONHasAllRFC8621Rights(t *testing.T) {
	raw, err := json.Marshal(mail.MailboxRights{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"mayReadItems", "mayAddItems", "mayRemoveItems", "maySetSeen",
		"maySetKeywords", "mayCreateChild", "mayRename", "mayDelete",
		"maySubmit",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("MailboxRights JSON %s missing %q", raw, want)
		}
	}
}
