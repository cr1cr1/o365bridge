package jmapadapter

import (
	"context"
	"fmt"
	"strconv"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	jmail "github.com/cr1cr1/o365bridge/jmap/mail"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// --- Identity ---

type identityStore struct{ b *Backend }

func (s *identityStore) State(_ context.Context, _ jmap.ID) (string, error) { return "1", nil }

func (s *identityStore) Get(_ context.Context, account jmap.ID, ids []jmap.ID) ([]jmail.Identity, []jmap.ID, error) {
	ident := jmail.Identity{ID: jmap.ID(account), Name: "", Email: string(account), MayDelete: false}
	if ids == nil {
		return []jmail.Identity{ident}, nil, nil
	}
	var found []jmail.Identity
	var notFound []jmap.ID
	for _, id := range ids {
		if id == ident.ID {
			found = append(found, ident)
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

// shortcut: identities are derived from the account; creation/mutation is forbidden.
func (s *identityStore) Create(_ context.Context, _ jmap.ID, _ jmail.Identity) (jmail.Identity, *jmap.SetError) {
	return jmail.Identity{}, &jmap.SetError{Type: "forbidden"}
}
func (s *identityStore) Update(_ context.Context, _ jmap.ID, _ jmail.Identity) (*jmail.Identity, *jmap.SetError) {
	return nil, &jmap.SetError{Type: "forbidden"}
}
func (s *identityStore) Destroy(_ context.Context, _ jmap.ID, _ jmap.ID) *jmap.SetError {
	return &jmap.SetError{Type: "forbidden"}
}

// --- EmailSubmission ---

type submissionStore struct{ b *Backend }

func (s *submissionStore) State(_ context.Context, account jmap.ID) (string, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	// shortcut: submissions are not persisted and cannot be listed after restart (in-memory only)
	return strconv.Itoa(s.b.subVersion[string(account)]), nil
}

func (s *submissionStore) Get(_ context.Context, account jmap.ID, ids []jmap.ID) ([]jmail.EmailSubmission, []jmap.ID, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	m := s.b.subs[string(account)]
	if ids == nil {
		out := make([]jmail.EmailSubmission, 0, len(m))
		for _, v := range m {
			out = append(out, *v)
		}
		return out, nil, nil
	}
	var found []jmail.EmailSubmission
	var notFound []jmap.ID
	for _, id := range ids {
		if v, ok := m[id]; ok {
			found = append(found, *v)
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

func (s *submissionStore) Create(ctx context.Context, account jmap.ID, obj jmail.EmailSubmission) (jmail.EmailSubmission, *jmap.SetError) {
	// Validation first: none of these cases may send.
	if obj.SendAt != nil && obj.SendAt.After(time.Now().Add(time.Minute)) {
		return jmail.EmailSubmission{}, &jmap.SetError{Type: "invalidProperties", Properties: []string{"sendAt"}, Description: "scheduled sending is not supported yet"}
	}
	// The sole identity's id is the account email (see identityStore).
	if obj.IdentityID != "" && obj.IdentityID != account {
		return jmail.EmailSubmission{}, &jmap.SetError{Type: "invalidProperties", Properties: []string{"identityId"}, Description: "unknown identity"}
	}
	if obj.Envelope != nil && obj.Envelope.MailFrom.Email != "" && obj.Envelope.MailFrom.Email != string(account) {
		return jmail.EmailSubmission{}, &jmap.SetError{Type: "invalidProperties", Properties: []string{"envelope"}, Description: "envelope mailFrom must be the account email"}
	}
	folderID, upstreamID, err := decodeEmailID(obj.EmailID)
	if err != nil {
		return jmail.EmailSubmission{}, &jmap.SetError{Type: "invalidProperties", Properties: []string{"emailId"}, Description: "emailId must be a message id"}
	}
	// Build email for ThreadID (also verifies existence)
	estore := &emailStore{s.b}
	em, _, berr := estore.buildOne(ctx, account, folderID, upstreamID)
	if berr != nil {
		return jmail.EmailSubmission{}, &jmap.SetError{Type: "notFound", Description: berr.Error()}
	}
	var raw []byte
	if err := s.b.call(ctx, string(account), "GetMessageMIME", func(ctx context.Context, p bridge.Provider) error {
		var err error
		raw, err = p.GetMessageMIME(ctx, folderID, upstreamID)
		return err
	}); err != nil {
		return jmail.EmailSubmission{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	if err := s.b.call(ctx, string(account), "SendMessage", func(ctx context.Context, p bridge.Provider) error {
		return p.SendMessage(ctx, raw)
	}); err != nil {
		return jmail.EmailSubmission{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	now := time.Now()
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	acct := string(account)
	if s.b.subs[acct] == nil {
		s.b.subs[acct] = map[jmap.ID]*jmail.EmailSubmission{}
	}
	s.b.subSeq[acct]++
	id := jmap.ID(fmt.Sprintf("sub-%d", s.b.subSeq[acct]))
	rec := obj
	rec.ID = id
	rec.ThreadID = em.ThreadID
	if rec.IdentityID == "" {
		rec.IdentityID = jmap.ID(acct)
	}
	rec.UndoStatus = "final"
	if rec.SendAt == nil {
		rec.SendAt = &now
	}
	cp := rec
	s.b.subs[acct][id] = &cp
	s.b.subVersion[acct]++
	return rec, nil
}

func (s *submissionStore) Update(_ context.Context, _ jmap.ID, _ jmail.EmailSubmission) (*jmail.EmailSubmission, *jmap.SetError) {
	return nil, &jmap.SetError{Type: "forbidden"}
}

func (s *submissionStore) Destroy(_ context.Context, account jmap.ID, id jmap.ID) *jmap.SetError {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	m := s.b.subs[string(account)]
	if _, ok := m[id]; !ok {
		return &jmap.SetError{Type: "notFound"}
	}
	delete(m, id)
	s.b.subVersion[string(account)]++
	return nil
}

// --- Snippets ---

func (b *Backend) Snippets(ctx context.Context, account jmap.ID, emailIds []jmap.ID) ([]jmail.SearchSnippet, []jmap.ID, error) {
	estore := &emailStore{b}
	found, notFound, err := estore.Get(ctx, account, emailIds)
	if err != nil {
		return nil, nil, err
	}
	byID := map[jmap.ID]jmail.Email{}
	for _, em := range found {
		byID[em.ID] = em
	}
	var out []jmail.SearchSnippet
	// Preserve requested order; also surface found even if emailIds nil? Snippet/get requires ids.
	if emailIds == nil {
		for _, em := range found {
			out = append(out, jmail.SearchSnippet{EmailID: em.ID, Subject: strPtr(em.Subject), Preview: strPtr(em.Preview)})
		}
		return out, notFound, nil
	}
	for _, id := range emailIds {
		if em, ok := byID[id]; ok {
			out = append(out, jmail.SearchSnippet{EmailID: em.ID, Subject: strPtr(em.Subject), Preview: strPtr(em.Preview)})
		}
	}
	return out, notFound, nil
}

// --- Importer ---

func (b *Backend) ImportEmail(ctx context.Context, account jmap.ID, blobID jmap.ID, mailboxIDs map[jmap.ID]bool, keywords map[string]bool, receivedAt *time.Time) (jmail.Email, *jmap.SetError) {
	if len(mailboxIDs) != 1 {
		return jmail.Email{}, &jmap.SetError{Type: "invalidProperties", Properties: []string{"mailboxIds"}, Description: "mailboxIds must contain exactly one folder"}
	}
	var folder jmap.ID
	for id, ok := range mailboxIDs {
		if ok {
			folder = id
		}
	}
	_, data, err := b.blobs.Download(ctx, account, blobID)
	if err != nil {
		return jmail.Email{}, &jmap.SetError{Type: "blobNotFound", Description: err.Error()}
	}
	flags := keywordsToFlags(keywords)
	var newID string
	if err := b.call(ctx, string(account), "AppendMessage", func(ctx context.Context, p bridge.Provider) error {
		var err error
		newID, err = p.AppendMessage(ctx, string(folder), data, flags)
		return err
	}); err != nil {
		return jmail.Email{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	estore := &emailStore{b}
	em, _, err := estore.buildOne(ctx, account, string(folder), newID)
	if err != nil {
		id := encodeEmailID(string(folder), newID)
		return jmail.Email{ID: id, BlobID: id}, nil
	}
	return em, nil
}

// --- Copier ---

func (b *Backend) CopyEmail(ctx context.Context, fromAccount, toAccount jmap.ID, id jmap.ID) (jmail.Email, *jmap.SetError) {
	folderID, upstreamID, err := decodeEmailID(id)
	if err != nil {
		return jmail.Email{}, &jmap.SetError{Type: "notFound"}
	}
	// Source email (for its mailboxIds) + MIME via source account
	estore := &emailStore{b}
	srcEmail, _, berr := estore.buildOne(ctx, fromAccount, folderID, upstreamID)
	if berr != nil {
		return jmail.Email{}, &jmap.SetError{Type: "notFound", Description: berr.Error()}
	}
	var raw []byte
	if err := b.call(ctx, string(fromAccount), "GetMessageMIME", func(ctx context.Context, p bridge.Provider) error {
		var err error
		raw, err = p.GetMessageMIME(ctx, folderID, upstreamID)
		return err
	}); err != nil {
		return jmail.Email{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	// Destination folders: source mailboxIds that exist in dest, else inbox-equivalent
	var destFolders []bridge.Folder
	if err := b.call(ctx, string(toAccount), "ListFolders", func(ctx context.Context, p bridge.Provider) error {
		var err error
		destFolders, err = p.ListFolders(ctx)
		return err
	}); err != nil {
		return jmail.Email{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	if len(destFolders) == 0 {
		return jmail.Email{}, &jmap.SetError{Type: "notFound", Description: "no destination folders"}
	}
	exists := map[jmap.ID]bool{}
	var inboxFolder string
	for _, f := range destFolders {
		exists[jmap.ID(f.ID)] = true
		if f.Special == "inbox" && inboxFolder == "" {
			inboxFolder = f.ID
		}
	}
	var destIDs []jmap.ID
	for mid := range srcEmail.MailboxIDs {
		if srcEmail.MailboxIDs[mid] && exists[mid] {
			destIDs = append(destIDs, mid)
		}
	}
	if len(destIDs) == 0 {
		if inboxFolder == "" {
			inboxFolder = destFolders[0].ID
		}
		destIDs = []jmap.ID{jmap.ID(inboxFolder)}
	}
	first := string(destIDs[0])
	var newID string
	if err := b.call(ctx, string(toAccount), "AppendMessage", func(ctx context.Context, p bridge.Provider) error {
		var err error
		newID, err = p.AppendMessage(ctx, first, raw, keywordsToFlags(srcEmail.Keywords))
		return err
	}); err != nil {
		return jmail.Email{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	for _, dest := range destIDs[1:] {
		_ = b.call(ctx, string(toAccount), "CopyMessages", func(ctx context.Context, p bridge.Provider) error {
			return p.CopyMessages(ctx, first, []string{newID}, string(dest))
		})
	}
	em, _, err2 := estore.buildOne(ctx, toAccount, first, newID)
	if err2 != nil {
		eid := encodeEmailID(first, newID)
		return jmail.Email{ID: eid, BlobID: eid}, nil
	}
	return em, nil
}

// --- Parser ---

func (b *Backend) ParseEmail(ctx context.Context, account jmap.ID, blobID jmap.ID) (jmail.Email, error) {
	_, data, err := b.blobs.Download(ctx, account, blobID)
	if err != nil {
		return jmail.Email{}, err
	}
	return buildParsed(data, blobID)
}
