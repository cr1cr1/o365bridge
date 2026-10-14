package mail

// Phase 3a: MDN (RFC 9007) and S/MIME (RFC 9219) additions.
//
// shortcut: this layer only models the JMAP wire objects and method
// plumbing. Actually composing/sending the MDN message and actually
// verifying/decrypting S/MIME signatures are the backend's job.

import (
	"context"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

// --- S/MIME (RFC 9219) ---

// SMimeStatus is the "smimeStatus" Email property (RFC 9219 §4). It
// is a plain string type so any RFC/backend-supplied value survives
// round-tripping, even values this package does not know about.
//
// RFC 9219 §4 defines values including "signed/valid",
// "signed/validAtTimeOfReading" and "signed/invalid"; backends may
// supply further values (for example for encryption failures). The
// constants below name the common ones; they are not exhaustive and
// the engine never validates against them — the value is passed
// through from the backend verbatim.
type SMimeStatus string

const (
	// SignedValid is RFC 9219 "signed/valid".
	SignedValid SMimeStatus = "signed/valid"
	// SignedValidAtTimeOfReading is RFC 9219
	// "signed/validAtTimeOfReading".
	SignedValidAtTimeOfReading SMimeStatus = "signed/validAtTimeOfReading"
	// SignedInvalid is RFC 9219 "signed/invalid".
	SignedInvalid SMimeStatus = "signed/invalid"
	// SignedUnknown covers a signed message whose status the backend
	// could not determine ("signed/unknown", backend-supplied).
	SignedUnknown SMimeStatus = "signed/unknown"
)

// SMimeBackend is the optional marker behind advertising the S/MIME
// capability (urn:ietf:params:jmap:smime, RFC 9219). The capability
// is only advertised when SMimeCapable reports true; Email objects
// simply carry the smime* fields when the backend fills them in.
type SMimeBackend interface {
	SMimeCapable() bool
}

// --- MDN (RFC 9007) ---

// MDNDisposition is the disposition object of an MDN (RFC 9007 §4,
// RFC 8098 disposition fields): ActionMode is e.g.
// "manual-action"/"automatic-action", SendingMode is e.g.
// "mdn-sent-manually"/"mdn-sent-automatically", Type is e.g.
// "displayed"/"deleted"/"dispatched"/"processed", and Modified is
// the optional disposition modifier.
type MDNDisposition struct {
	ActionMode  string  `json:"actionMode"`
	SendingMode string  `json:"sendingMode"`
	Type        string  `json:"type"`
	Modified    *string `json:"modified,omitempty"`
}

// MDN is a Message Disposition Notification (RFC 9007). It is used
// both as the payload of MDN/send (without id) and as the object the
// method returns (with the server-assigned id).
type MDN struct {
	ID                     jmap.ID        `json:"id,omitempty"`
	EmailID                jmap.ID        `json:"emailId"`
	IdentityID             jmap.ID        `json:"identityId,omitempty"`
	Subject                *string        `json:"subject,omitempty"`
	TextBody               *string        `json:"textBody,omitempty"`
	ReportingUA            *string        `json:"reportingUA,omitempty"`
	Disposition            MDNDisposition `json:"disposition"`
	IncludeOriginalMessage bool           `json:"includeOriginalMessage"`
}

// MDNSender composes and sends one MDN for an account. The returned
// MDN is the created object (with its id filled in). A non-nil
// SetError reports a send failure; its Type becomes the method error
// type of the MDN/send response (see mdnSend).
type MDNSender interface {
	SendMDN(ctx context.Context, account jmap.ID, mdn MDN) (MDN, *jmap.SetError)
}

// MDNBackend is the optional contract behind MDN/send; Register only
// registers that method (and the urn:ietf:params:jmap:mdn
// capability) when the Backend implements it and returns a non-nil
// sender.
type MDNBackend interface {
	MDNSender() MDNSender
}

func registerPhase3a(e *server.Engine, b Backend) {
	if sb, ok := b.(SMimeBackend); ok && sb.SMimeCapable() {
		// RFC 9219 capability URI. The root package has no constant
		// for it (only jmap.MDNURI exists), so the literal is used
		// here rather than editing the root package.
		e.RegisterCapability(jmap.URI("urn:ietf:params:jmap:smime"), struct{}{}, struct{}{})
	}
	if mb, ok := b.(MDNBackend); ok && mb.MDNSender() != nil {
		sender := mb.MDNSender()
		e.RegisterCapability(jmap.MDNURI, struct{}{}, struct{}{})
		e.RegisterMethod("MDN/send", jmap.MDNURI, func(ctx context.Context, call *server.Call) (any, error) {
			return mdnSend(ctx, call, sender)
		})
	}
}

// --- MDN/send (RFC 9007 §4) ---

type mdnSendArgs struct {
	AccountID              jmap.ID        `json:"accountId"`
	IdentityID             jmap.ID        `json:"identityId"`
	EmailID                jmap.ID        `json:"emailId"`
	Subject                *string        `json:"subject"`
	TextBody               *string        `json:"textBody"`
	ReportingUA            *string        `json:"reportingUA"`
	Disposition            MDNDisposition `json:"disposition"`
	IncludeOriginalMessage bool           `json:"includeOriginalMessage"`
}

type mdnSendResponse struct {
	AccountID jmap.ID `json:"accountId"`
	MDN       MDN     `json:"mdn"`
}

func mdnSend(ctx context.Context, call *server.Call, sender MDNSender) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args mdnSendArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	sent, serr := sender.SendMDN(ctx, account, MDN{
		EmailID:                args.EmailID,
		IdentityID:             args.IdentityID,
		Subject:                args.Subject,
		TextBody:               args.TextBody,
		ReportingUA:            args.ReportingUA,
		Disposition:            args.Disposition,
		IncludeOriginalMessage: args.IncludeOriginalMessage,
	})
	if serr != nil {
		// /send failures surface as a method error whose type is the
		// sender-supplied SetError type.
		return nil, &jmap.MethodError{Type: serr.Type, Description: serr.Description}
	}
	return mdnSendResponse{AccountID: account, MDN: sent}, nil
}
