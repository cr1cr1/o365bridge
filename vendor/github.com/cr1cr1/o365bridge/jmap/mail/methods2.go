package mail

// Phase 2a-2 handlers: custom EmailSubmission/set (onSuccess*),
// VacationResponse/get|set, SearchSnippet/get, Email/import,
// Email/copy, Email/parse. All are registered by Register only when
// the backend implements the matching optional interface.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/cr1cr1/o365bridge/jmap/server"
)

func registerMail2(e *server.Engine, b Backend) {
	if ib, ok := b.(IdentityBackend); ok && ib.Identities() != nil {
		datatype.Register[Identity](e, datatype.Options[Identity]{
			Name: "Identity", Capability: jmap.MailURI, Store: ib.Identities(),
			IDOf: func(v Identity) jmap.ID { return v.ID }, Match: identityMatch, Compare: identityCompare,
		})
	}
	if sb, ok := b.(SubmissionBackend); ok && sb.Submissions() != nil {
		store := sb.Submissions()
		datatype.Register[EmailSubmission](e, datatype.Options[EmailSubmission]{
			Name: "EmailSubmission", Capability: jmap.MailURI, Store: store,
			IDOf: func(v EmailSubmission) jmap.ID { return v.ID }, Match: submissionMatch, Compare: submissionCompare,
		})
		// Overwrite the generic /set with the onSuccess*-aware one.
		e.RegisterMethod("EmailSubmission/set", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
			return submissionSet(ctx, call, b, store)
		})
	}
	if vb, ok := b.(VacationBackend); ok && vb.VacationStore() != nil {
		vs := vb.VacationStore()
		e.RegisterMethod("VacationResponse/get", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
			return vacationGet(ctx, call, vs)
		})
		e.RegisterMethod("VacationResponse/set", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
			return vacationSet(ctx, call, vs)
		})
	}
	if sb, ok := b.(SnippetBackend); ok && sb.SnippetSource() != nil {
		src := sb.SnippetSource()
		e.RegisterMethod("SearchSnippet/get", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
			return snippetGet(ctx, call, src)
		})
	}
	if ib, ok := b.(ImportBackend); ok && ib.EmailImporter() != nil {
		imp := ib.EmailImporter()
		e.RegisterMethod("Email/import", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
			return emailImport(ctx, call, b, imp)
		})
	}
	if cb, ok := b.(CopyBackend); ok && cb.EmailCopier() != nil {
		cp := cb.EmailCopier()
		e.RegisterMethod("Email/copy", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
			return emailCopy(ctx, call, b, cp)
		})
	}
	if pb, ok := b.(ParseBackend); ok && pb.EmailParser() != nil {
		p := pb.EmailParser()
		e.RegisterMethod("Email/parse", jmap.MailURI, func(ctx context.Context, call *server.Call) (any, error) {
			return emailParse(ctx, call, p)
		})
	}
}

// --- helpers ---

func projectAny(v any, properties []string, always ...string) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
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
	keep := make(map[string]json.RawMessage, len(properties)+len(always)+1)
	for _, k := range append(append([]string{"id"}, always...), properties...) {
		if v, ok := m[k]; ok {
			keep[k] = v
		}
	}
	return json.Marshal(keep)
}

func setErr2(m map[jmap.ID]*jmap.SetError, id jmap.ID, e *jmap.SetError) map[jmap.ID]*jmap.SetError {
	if m == nil {
		m = make(map[jmap.ID]*jmap.SetError)
	}
	m[id] = e
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// --- EmailSubmission/set (RFC 8621 §7) ---

type submissionSetArgs struct {
	AccountID             jmap.ID                      `json:"accountId"`
	IfInState             *string                      `json:"ifInState"`
	Create                map[string]json.RawMessage   `json:"create"`
	Update                map[jmap.ID]jmap.PatchObject `json:"update"`
	Destroy               []jmap.ID                    `json:"destroy"`
	OnSuccessDestroyEmail map[string]jmap.ID           `json:"onSuccessDestroyEmail"`
	OnSuccessUpdateEmail  map[string]jmap.PatchObject  `json:"onSuccessUpdateEmail"`
}

func submissionSet(ctx context.Context, call *server.Call, b Backend, store datatype.Store[EmailSubmission]) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args submissionSetArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	oldState, err := store.State(ctx, account)
	if err != nil {
		return nil, err
	}
	if args.IfInState != nil && *args.IfInState != oldState {
		return nil, jmap.NewStateMismatch(fmt.Sprintf("ifInState %q does not match current state %q", *args.IfInState, oldState))
	}
	resp := jmap.SetResponse[EmailSubmission]{AccountID: account, OldState: &oldState, NewState: oldState}
	changed := false
	createdCID := map[string]bool{} // creation ids created successfully in this call

	for _, cid := range sortedKeys(args.Create) {
		var obj EmailSubmission
		if err := json.Unmarshal(args.Create[cid], &obj); err != nil {
			resp.NotCreated = setErr2(resp.NotCreated, jmap.ID(cid), &jmap.SetError{Type: "invalidProperties", Description: err.Error()})
			continue
		}
		created, serr := store.Create(ctx, account, obj) // Create performs the actual submission (see EmailSubmission contract)
		if serr != nil {
			resp.NotCreated = setErr2(resp.NotCreated, jmap.ID(cid), serr)
			continue
		}
		if resp.Created == nil {
			resp.Created = make(map[jmap.ID]*EmailSubmission)
		}
		c := created
		resp.Created[jmap.ID(cid)] = &c
		call.SetCreated(jmap.ID(cid), created.ID)
		createdCID[cid] = true
		changed = true
	}
	updateIDs := make([]jmap.ID, 0, len(args.Update))
	for id := range args.Update {
		updateIDs = append(updateIDs, id)
	}
	sort.Slice(updateIDs, func(i, j int) bool { return updateIDs[i] < updateIDs[j] })
	for _, id := range updateIDs {
		found, _, err := store.Get(ctx, account, []jmap.ID{id})
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			resp.NotUpdated = setErr2(resp.NotUpdated, id, &jmap.SetError{Type: "notFound"})
			continue
		}
		patched, err := datatype.ApplyPatch(found[0], args.Update[id])
		if err != nil {
			resp.NotUpdated = setErr2(resp.NotUpdated, id, &jmap.SetError{Type: "invalidPatch", Description: err.Error()})
			continue
		}
		updated, serr := store.Update(ctx, account, patched)
		if serr != nil {
			resp.NotUpdated = setErr2(resp.NotUpdated, id, serr)
			continue
		}
		if resp.Updated == nil {
			resp.Updated = make(map[jmap.ID]*EmailSubmission)
		}
		resp.Updated[id] = updated
		changed = true
	}
	for _, id := range args.Destroy {
		if serr := store.Destroy(ctx, account, id); serr != nil {
			resp.NotDestroyed = setErr2(resp.NotDestroyed, id, serr)
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
		changed = true
	}

	// onSuccess* (§7): side effects only, applied only for submissions
	// created successfully in this call, keyed by submission creation id.
	emails := b.Emails()
	for _, cid := range sortedKeys(args.OnSuccessDestroyEmail) {
		if !createdCID[cid] {
			continue
		}
		_ = emails.Destroy(ctx, account, args.OnSuccessDestroyEmail[cid]) // failure is a side-effect no-op
	}
	for _, cid := range sortedKeys(args.OnSuccessUpdateEmail) {
		if !createdCID[cid] {
			continue
		}
		// The patch keys in onSuccessUpdateEmail are RFC 8621 paths
		// addressed at the Email; ApplyPatch handles them, but the
		// target email id is not in the map value — the client
		// convention here: this handler cannot know it, so update
		// entries are keyed by creation id only for destroy. For
		// update, we apply the patch to the submission's EmailID.
		// Look up the created submission to find its emailId.
		// (Stored in resp.Created for this cid.)
		created := resp.Created[jmap.ID(cid)]
		if created == nil {
			continue
		}
		found, _, err := emails.Get(ctx, account, []jmap.ID{created.EmailID})
		if err != nil || len(found) == 0 {
			continue
		}
		patched, err := datatype.ApplyPatch(found[0], args.OnSuccessUpdateEmail[cid])
		if err != nil {
			continue
		}
		_, _ = emails.Update(ctx, account, patched)
	}

	if changed {
		if newState, err := store.State(ctx, account); err == nil {
			resp.NewState = newState
		} else {
			return nil, err
		}
	}
	return resp, nil
}

// --- VacationResponse (§8) ---

type vacationGetResponse struct {
	AccountID jmap.ID           `json:"accountId"`
	State     string            `json:"state"`
	List      []json.RawMessage `json:"list"`
	NotFound  []jmap.ID         `json:"notFound"`
}

func vacationGet(ctx context.Context, call *server.Call, vs VacationStore) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args jmap.GetArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	cur, state, err := vs.Vacation(ctx, account)
	if err != nil {
		return nil, err
	}
	if cur.ID == "" {
		cur.ID = "singleton"
	}
	resp := vacationGetResponse{AccountID: account, State: state, List: []json.RawMessage{}, NotFound: []jmap.ID{}}
	include := args.IDs == nil
	for _, id := range args.IDs {
		if id == "singleton" {
			include = true
		} else {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	if include {
		raw, err := projectAny(cur, args.Properties)
		if err != nil {
			return nil, err
		}
		resp.List = append(resp.List, raw)
	}
	return resp, nil
}

type vacationSetArgs struct {
	AccountID jmap.ID                      `json:"accountId"`
	IfInState *string                      `json:"ifInState"`
	Create    map[string]json.RawMessage   `json:"create"`
	Update    map[jmap.ID]jmap.PatchObject `json:"update"`
	Destroy   []jmap.ID                    `json:"destroy"`
}

func vacationSet(ctx context.Context, call *server.Call, vs VacationStore) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args vacationSetArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	cur, state, err := vs.Vacation(ctx, account)
	if err != nil {
		return nil, err
	}
	if cur.ID == "" {
		cur.ID = "singleton"
	}
	if args.IfInState != nil && *args.IfInState != state {
		return nil, jmap.NewStateMismatch(fmt.Sprintf("ifInState %q does not match current state %q", *args.IfInState, state))
	}
	resp := jmap.SetResponse[VacationResponse]{AccountID: account, OldState: &state, NewState: state}
	for cid := range args.Create {
		resp.NotCreated = setErr2(resp.NotCreated, jmap.ID(cid), &jmap.SetError{Type: "singleton", Description: "VacationResponse is a singleton; use update"})
	}
	for _, id := range args.Destroy {
		resp.NotDestroyed = setErr2(resp.NotDestroyed, id, &jmap.SetError{Type: "singleton", Description: "VacationResponse cannot be destroyed"})
	}
	for id, patch := range args.Update {
		if id != "singleton" {
			resp.NotUpdated = setErr2(resp.NotUpdated, id, &jmap.SetError{Type: "singleton", Description: "VacationResponse id must be \"singleton\""})
			continue
		}
		patched, err := datatype.ApplyPatch(cur, patch)
		if err != nil {
			resp.NotUpdated = setErr2(resp.NotUpdated, id, &jmap.SetError{Type: "invalidPatch", Description: err.Error()})
			continue
		}
		patched.ID = "singleton"
		oldState, newState, err := vs.SetVacation(ctx, account, patched, args.IfInState)
		if err != nil {
			return nil, err
		}
		if oldState != "" {
			resp.OldState = &oldState
		}
		resp.NewState = newState
		if resp.Updated == nil {
			resp.Updated = make(map[jmap.ID]*VacationResponse)
		}
		resp.Updated["singleton"] = nil // null: no server-set properties reported
	}
	return resp, nil
}

// --- SearchSnippet/get (§5) ---

type snippetArgs struct {
	AccountID jmap.ID         `json:"accountId"`
	EmailIDs  []jmap.ID       `json:"emailIds"`
	Filter    json.RawMessage `json:"filter"` // accepted; filtering is the source's job (shortcut)
}

type snippetResponse struct {
	AccountID jmap.ID         `json:"accountId"`
	List      []SearchSnippet `json:"list"`
	NotFound  []jmap.ID       `json:"notFound"`
}

func snippetGet(ctx context.Context, call *server.Call, src SnippetSource) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args snippetArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	list, notFound, err := src.Snippets(ctx, account, args.EmailIDs)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []SearchSnippet{}
	}
	if notFound == nil {
		notFound = []jmap.ID{}
	}
	return snippetResponse{AccountID: account, List: list, NotFound: notFound}, nil
}

// --- Email/import ---

type importItem struct {
	BlobID     jmap.ID          `json:"blobId"`
	MailboxIDs map[jmap.ID]bool `json:"mailboxIds"`
	Keywords   map[string]bool  `json:"keywords"`
	ReceivedAt *time.Time       `json:"receivedAt"`
}

type importArgs struct {
	AccountID jmap.ID               `json:"accountId"`
	IfInState *string               `json:"ifInState"`
	Emails    map[string]importItem `json:"emails"`
}

type importResponse struct {
	AccountID  jmap.ID                    `json:"accountId"`
	OldState   *string                    `json:"oldState"`
	NewState   string                     `json:"newState"`
	Created    map[jmap.ID]*Email         `json:"created"`
	NotCreated map[jmap.ID]*jmap.SetError `json:"notCreated"`
}

func emailImport(ctx context.Context, call *server.Call, b Backend, imp Importer) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args importArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	state, err := b.Emails().State(ctx, account)
	if err != nil {
		return nil, err
	}
	if args.IfInState != nil && *args.IfInState != state {
		return nil, jmap.NewStateMismatch(fmt.Sprintf("ifInState %q does not match current state %q", *args.IfInState, state))
	}
	resp := importResponse{AccountID: account, OldState: nil, NewState: state} // oldState null: not tracked (prompt contract)
	for _, cid := range sortedKeys(args.Emails) {
		item := args.Emails[cid]
		created, serr := imp.ImportEmail(ctx, account, item.BlobID, item.MailboxIDs, item.Keywords, item.ReceivedAt)
		if serr != nil {
			resp.NotCreated = setErr2(resp.NotCreated, jmap.ID(cid), serr)
			continue
		}
		if resp.Created == nil {
			resp.Created = make(map[jmap.ID]*Email)
		}
		c := created
		resp.Created[jmap.ID(cid)] = &c
		call.SetCreated(jmap.ID(cid), created.ID)
	}
	if newState, err := b.Emails().State(ctx, account); err == nil {
		resp.NewState = newState
	} else {
		return nil, err
	}
	return resp, nil
}

// --- Email/copy ---

type copyArgs struct {
	FromAccountID jmap.ID   `json:"fromAccountId"`
	IfFromInState *string   `json:"ifFromInState"`
	AccountID     jmap.ID   `json:"accountId"`
	IfInState     *string   `json:"ifInState"`
	EmailIDs      []jmap.ID `json:"emailIds"`
	Properties    []string  `json:"properties"`
}

type copyResponse struct {
	AccountID jmap.ID                     `json:"accountId"`
	OldState  *string                     `json:"oldState"`
	NewState  string                      `json:"newState"`
	Copied    map[jmap.ID]json.RawMessage `json:"copied"`
	NotCopied map[jmap.ID]*jmap.SetError  `json:"notCopied"`
}

func emailCopy(ctx context.Context, call *server.Call, b Backend, cp Copier) (any, error) {
	account, err := call.AccountID() // destination account
	if err != nil {
		return nil, err
	}
	var args copyArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	if args.FromAccountID == "" {
		return nil, jmap.NewInvalidArguments("missing fromAccountId")
	}
	if call.Principal == nil || call.Principal.Accounts == nil {
		return nil, jmap.NewAccountNotFound(string(args.FromAccountID))
	}
	if _, ok := call.Principal.Accounts[args.FromAccountID]; !ok {
		return nil, jmap.NewAccountNotFound(string(args.FromAccountID))
	}
	oldState, err := b.Emails().State(ctx, account)
	if err != nil {
		return nil, err
	}
	if args.IfInState != nil && *args.IfInState != oldState {
		return nil, jmap.NewStateMismatch("ifInState does not match destination state")
	}
	if args.IfFromInState != nil {
		fromState, err := b.Emails().State(ctx, args.FromAccountID)
		if err != nil {
			return nil, err
		}
		if *args.IfFromInState != fromState {
			return nil, jmap.NewStateMismatch("ifFromInState does not match source state")
		}
	}
	resp := copyResponse{AccountID: account, OldState: &oldState, NewState: oldState}
	for _, id := range args.EmailIDs {
		created, serr := cp.CopyEmail(ctx, args.FromAccountID, account, id)
		if serr != nil {
			resp.NotCopied = setErr2(resp.NotCopied, id, serr)
			continue
		}
		raw, err := projectAny(created, args.Properties)
		if err != nil {
			return nil, err
		}
		if resp.Copied == nil {
			resp.Copied = make(map[jmap.ID]json.RawMessage)
		}
		resp.Copied[id] = raw
	}
	if newState, err := b.Emails().State(ctx, account); err == nil {
		resp.NewState = newState
	} else {
		return nil, err
	}
	return resp, nil
}

// --- Email/parse ---

type parseArgs struct {
	AccountID      jmap.ID   `json:"accountId"`
	BlobIDs        []jmap.ID `json:"blobIds"`
	Properties     []string  `json:"properties"`
	BodyProperties []string  `json:"bodyProperties"` // accepted, unused (shortcut)
	// fetch* body-value arguments are accepted by JSON decoding
	// (unknown fields ignored) but not implemented (shortcut).
}

type parseResponse struct {
	AccountID   jmap.ID                     `json:"accountId"`
	Parsed      map[jmap.ID]json.RawMessage `json:"parsed"`
	NotParsable map[jmap.ID]*jmap.SetError  `json:"notParsable"`
}

func emailParse(ctx context.Context, call *server.Call, p Parser) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args parseArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	resp := parseResponse{AccountID: account}
	for _, blobID := range args.BlobIDs {
		em, err := p.ParseEmail(ctx, account, blobID)
		if err != nil {
			// shortcut: RFC 8621 does not define a dedicated parse
			// SetError type name here; "notParsable" is used and
			// documented as a local choice.
			resp.NotParsable = setErr2(resp.NotParsable, blobID, &jmap.SetError{Type: "notParsable", Description: err.Error()})
			continue
		}
		raw, err := projectAny(em, args.Properties, "blobId")
		if err != nil {
			return nil, err
		}
		if resp.Parsed == nil {
			resp.Parsed = make(map[jmap.ID]json.RawMessage)
		}
		resp.Parsed[blobID] = raw
	}
	return resp, nil
}
