package mail

// MDN (RFC 9007) and S/MIME (RFC 9219).
//
// The wire objects and method shapes follow the RFCs normatively:
// MDN/send is the batch method of RFC 9007 §2.1 (send: Id[MDN] with
// sent/notSent responses and mandatory $mdnsent enforcement),
// MDN/parse is §2.2, and the S/MIME capability and status vocabulary
// are RFC 9219 §3-4. Composing the actual message and verifying
// signatures remain backend jobs behind the MDNSender / MDNParser
// contracts.

import (
	"context"
	"errors"
	"sort"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// --- S/MIME (RFC 9219) ---

// SMimeVerifyURI is the S/MIME verification capability of RFC 9219
// §3 ("urn:ietf:params:jmap:smimeverify", empty capability object).
const SMimeVerifyURI = jmap.URI("urn:ietf:params:jmap:smimeverify")

// SMimeStatus is the "smimeStatus" Email response property
// (RFC 9219 §4): a String|null whose defined values are below.
// Servers MAY return extension values; the engine passes the value
// through verbatim and never validates against these constants.
type SMimeStatus string

const (
	// SMimeUnknown: an S/MIME message that was neither signed nor
	// successfully recognised (RFC 9219 §4, "unknown").
	SMimeUnknown SMimeStatus = "unknown"
	// SMimeSigned: signed but not (yet) verified (RFC 9219 §4).
	SMimeSigned SMimeStatus = "signed"
	// SMimeSignedVerified: signature verified per RFC 8551/8550
	// (RFC 9219 §4).
	SMimeSignedVerified SMimeStatus = "signed/verified"
	// SMimeSignedFailed: signature failed verification (RFC 9219 §4).
	SMimeSignedFailed SMimeStatus = "signed/failed"
	// SMimeEncryptedSignedVerified is reserved by RFC 9219 §4 for
	// future use.
	SMimeEncryptedSignedVerified SMimeStatus = "encrypted+signed/verified"
	// SMimeEncryptedSignedFailed is reserved by RFC 9219 §4 for
	// future use.
	SMimeEncryptedSignedFailed SMimeStatus = "encrypted+signed/failed"
)

// SMimeBackend is the optional marker behind advertising the
// RFC 9219 capability. It is only advertised when SMimeCapable
// reports true; Email objects carry the smime* fields when the
// backend fills them in.
type SMimeBackend interface {
	SMimeCapable() bool
}

// --- MDN (RFC 9007) ---

// MDNDisposition is the Disposition object of RFC 9007 §2: exactly
// actionMode ("manual-action"/"automatic-action"), sendingMode
// ("mdn-sent-manually"/"mdn-sent-automatically") and type
// ("deleted"/"dispatched"/"displayed"/"processed"), lowercase.
type MDNDisposition struct {
	ActionMode  string `json:"actionMode"`
	SendingMode string `json:"sendingMode"`
	Type        string `json:"type"`
}

// MDN is an MDN object per RFC 9007 §2. forEmailId must not be null
// in MDN/send; mdnGateway, originalRecipient, originalMessageId and
// error are server-set (filled by the MDNSender contract).
type MDN struct {
	ForEmailID             jmap.ID           `json:"forEmailId"`
	Subject                *string           `json:"subject"`
	TextBody               *string           `json:"textBody"`
	IncludeOriginalMessage bool              `json:"includeOriginalMessage"`
	ReportingUA            *string           `json:"reportingUA"`
	Disposition            MDNDisposition    `json:"disposition"`
	MdnGateway             *string           `json:"mdnGateway,omitempty"`        // server-set
	OriginalRecipient      *string           `json:"originalRecipient,omitempty"` // server-set
	FinalRecipient         *string           `json:"finalRecipient,omitempty"`    // client or server
	OriginalMessageID      *string           `json:"originalMessageId,omitempty"` // server-set
	Error                  []string          `json:"error,omitempty"`             // server-set
	ExtensionFields        map[string]string `json:"extensionFields,omitempty"`
}

// MDNSender composes and sends one MDN for an account on behalf of
// an identity (RFC 9007 §2.1: the identity defines the sender and
// the default finalRecipient). On success it returns the MDN with
// the server-set fields filled in. A SetError failure becomes a
// per-record notSent entry; RFC 9007 §2.1 types include notFound,
// forbidden, forbiddenFrom, overQuota, tooLarge, rateLimit,
// invalidProperties and mdnAlreadySent.
type MDNSender interface {
	SendMDN(ctx context.Context, account, identityID jmap.ID, mdn MDN) (MDN, *jmap.SetError)
}

// MDNParser parses one blob as an MDN message (RFC 9007 §2.2).
// Return ErrMDNNotFound or ErrMDNNotParsable to classify a blob;
// any other error is treated as not parsable.
type MDNParser interface {
	ParseMDN(ctx context.Context, account, blobID jmap.ID) (MDN, error)
}

var (
	// ErrMDNNotFound classifies a blob id that does not exist.
	ErrMDNNotFound = errors.New("mdn: blob not found")
	// ErrMDNNotParsable classifies a blob that is not an MDN message.
	ErrMDNNotParsable = errors.New("mdn: not parsable")
)

// MDNBackend is the optional contract behind MDN/send; MDNParserBackend
// behind MDN/parse. The urn:ietf:params:jmap:mdn capability is
// advertised when either is present.
type MDNBackend interface {
	MDNSender() MDNSender
}

type MDNParserBackend interface {
	MDNParser() MDNParser
}

func registerPhase3a(e *server.Engine, b Backend) {
	if sb, ok := b.(SMimeBackend); ok && sb.SMimeCapable() {
		e.RegisterCapability(SMimeVerifyURI, struct{}{}, struct{}{})
	}
	_, canSend := b.(MDNBackend)
	_, canParse := b.(MDNParserBackend)
	if canSend {
		if b.(MDNBackend).MDNSender() == nil {
			canSend = false
		}
	}
	if canParse {
		if b.(MDNParserBackend).MDNParser() == nil {
			canParse = false
		}
	}
	if !canSend && !canParse {
		return
	}
	e.RegisterCapability(jmap.MDNURI, struct{}{}, struct{}{})
	if canSend {
		sender := b.(MDNBackend).MDNSender()
		e.RegisterMethod("MDN/send", jmap.MDNURI, func(ctx context.Context, call *server.Call) (any, error) {
			return mdnSend(ctx, call, b, sender)
		})
	}
	if canParse {
		parser := b.(MDNParserBackend).MDNParser()
		e.RegisterMethod("MDN/parse", jmap.MDNURI, func(ctx context.Context, call *server.Call) (any, error) {
			return mdnParse(ctx, call, parser)
		})
	}
}

// --- MDN/send (RFC 9007 §2.1) ---

type mdnSendArgs struct {
	AccountID            jmap.ID                   `json:"accountId"`
	IdentityID           jmap.ID                   `json:"identityId"`
	Send                 map[string]MDN            `json:"send"`
	OnSuccessUpdateEmail map[string]map[string]any `json:"onSuccessUpdateEmail"`
}

type mdnSendResponse struct {
	AccountID jmap.ID                   `json:"accountId"`
	Sent      map[string]MDN            `json:"sent"`    // null when nothing sent
	NotSent   map[string]*jmap.SetError `json:"notSent"` // null when all sent
}

func mdnSend(ctx context.Context, call *server.Call, b Backend, sender MDNSender) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args mdnSendArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	if len(args.Send) == 0 {
		return nil, jmap.NewInvalidArguments("send must not be empty")
	}
	// RFC 9007 §2.1: an unknown accountId or identityId is rejected
	// with invalidArguments. Identities are validated when the
	// backend exposes its identity store; otherwise the identity
	// check is the sender's job (single-identity backends).
	if ib, ok := b.(IdentityBackend); ok {
		found, _, err := ib.Identities().Get(ctx, account, []jmap.ID{args.IdentityID})
		if err == nil && len(found) == 0 {
			return nil, jmap.NewInvalidArguments("identityId not found")
		}
	}
	// RFC 9007 §2.1: the server MUST reject an MDN/send that does
	// not result in setting $mdnsent — every created MDN needs an
	// onSuccessUpdateEmail entry (keyed by "#creationId" or the
	// forEmailId) whose patch sets the keyword.
	for cid, mdn := range args.Send {
		if !setsMdnsent(args.OnSuccessUpdateEmail, cid, mdn.ForEmailID) {
			return nil, jmap.NewInvalidArguments(
				"onSuccessUpdateEmail must set keywords/$mdnsent for every sent MDN (creation id " + cid + ")")
		}
	}

	emails := b.Emails()
	cids := make([]string, 0, len(args.Send))
	for cid := range args.Send {
		cids = append(cids, cid)
	}
	sort.Strings(cids) // deterministic processing order

	sent := map[string]MDN{}
	notSent := map[string]*jmap.SetError{}
	for _, cid := range cids {
		mdn := args.Send[cid]
		if mdn.ForEmailID == "" {
			notSent[cid] = &jmap.SetError{Type: "invalidProperties", Description: "forEmailId must not be null"}
			continue
		}
		found, _, err := emails.Get(ctx, account, []jmap.ID{mdn.ForEmailID})
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			notSent[cid] = &jmap.SetError{Type: "notFound", Description: "forEmailId not found"}
			continue
		}
		if found[0].Keywords["$mdnsent"] {
			notSent[cid] = &jmap.SetError{Type: "mdnAlreadySent", Description: "$mdnsent keyword is already present"}
			continue
		}
		out, serr := sender.SendMDN(ctx, account, args.IdentityID, mdn)
		if serr != nil {
			notSent[cid] = serr
			continue
		}
		if out.ForEmailID == "" {
			out.ForEmailID = mdn.ForEmailID
		}
		sent[cid] = out
	}

	// Apply onSuccessUpdateEmail for successfully sent MDNs. Keys
	// may be "#creationId" (backward reference, RFC 9007 §2.1/§3.1)
	// or a direct Email id.
	for key, patch := range args.OnSuccessUpdateEmail {
		target := jmap.ID(key)
		if cid, ok := stringsCutPrefix(key, "#"); ok {
			m, wasSent := sent[cid]
			if !wasSent {
				continue
			}
			target = m.ForEmailID
		}
		applyEmailPatch(ctx, emails, account, target, patch)
	}

	resp := mdnSendResponse{AccountID: account}
	if len(sent) > 0 {
		resp.Sent = sent
	}
	if len(notSent) > 0 {
		resp.NotSent = notSent
	}
	return resp, nil
}

// setsMdnsent reports whether the onSuccessUpdateEmail map contains,
// for the given creation id or its forEmailId, a patch that sets the
// $mdnsent keyword (either "keywords/$mdnsent": true or a nested
// keywords object).
func setsMdnsent(updates map[string]map[string]any, cid string, forEmailID jmap.ID) bool {
	for _, key := range []string{"#" + cid, string(forEmailID)} {
		patch, ok := updates[key]
		if !ok {
			continue
		}
		if v, ok := patch["keywords/$mdnsent"]; ok && truthy(v) {
			return true
		}
		if kw, ok := patch["keywords"].(map[string]any); ok {
			if v, ok := kw["$mdnsent"]; ok && truthy(v) {
				return true
			}
		}
	}
	return false
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func applyEmailPatch(ctx context.Context, emails datatype.Store[Email], account jmap.ID, id jmap.ID, patch map[string]any) {
	found, _, err := emails.Get(ctx, account, []jmap.ID{id})
	if err != nil || len(found) == 0 {
		return // shortcut: patch failures are not reported
	}
	updated, perr := datatype.ApplyPatch(found[0], patch)
	if perr != nil {
		return
	}
	_, _ = emails.Update(ctx, account, updated)
}

func stringsCutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

// --- MDN/parse (RFC 9007 §2.2) ---

type mdnParseArgs struct {
	AccountID jmap.ID   `json:"accountId"`
	BlobIDs   []jmap.ID `json:"blobIds"`
}

type mdnParseResponse struct {
	AccountID   jmap.ID        `json:"accountId"`
	Parsed      map[string]MDN `json:"parsed"`      // null when none
	NotParsable []string       `json:"notParsable"` // null when none
	NotFound    []string       `json:"notFound"`    // null when none
}

func mdnParse(ctx context.Context, call *server.Call, parser MDNParser) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args mdnParseArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	resp := mdnParseResponse{AccountID: account}
	for _, blobID := range args.BlobIDs {
		mdn, perr := parser.ParseMDN(ctx, account, blobID)
		switch {
		case perr == nil:
			if resp.Parsed == nil {
				resp.Parsed = map[string]MDN{}
			}
			resp.Parsed[string(blobID)] = mdn
		case errors.Is(perr, ErrMDNNotFound):
			resp.NotFound = append(resp.NotFound, string(blobID))
		default: // includes ErrMDNNotParsable
			resp.NotParsable = append(resp.NotParsable, string(blobID))
		}
	}
	return resp, nil
}
