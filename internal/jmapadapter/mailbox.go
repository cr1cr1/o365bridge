package jmapadapter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	jmail "github.com/cr1cr1/o365bridge/jmap/mail"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type mailboxStore struct{ b *Backend }

func folderToMailbox(f bridge.Folder) jmail.Mailbox {
	var parent *jmap.ID
	if f.ParentID != "" {
		p := jmap.ID(f.ParentID)
		parent = &p
	}
	var role *string
	switch f.Special {
	case "inbox", "sent", "drafts", "trash", "junk", "archive":
		r := f.Special
		role = &r
	}
	return jmail.Mailbox{
		ID: jmap.ID(f.ID), Name: f.Name, ParentID: parent, Role: role, SortOrder: 0,
		TotalEmails: f.Total, UnreadEmails: f.Unread,
		// shortcut: thread counts approximated by email counts
		TotalThreads: f.Total, UnreadThreads: f.Unread,
		MyRights:     &jmail.MailboxRights{MayReadItems: true, MayAddItems: true, MayRemoveItems: true, MaySetSeen: true, MaySetKeywords: true, MayCreateChild: true, MayRename: true, MayDelete: true, MaySubmit: true},
		IsSubscribed: true,
	}
}

func (s *mailboxStore) listFolders(ctx context.Context, account jmap.ID) ([]bridge.Folder, error) {
	var folders []bridge.Folder
	err := s.b.call(ctx, string(account), "ListFolders", func(ctx context.Context, p bridge.Provider) error {
		var err error
		folders, err = p.ListFolders(ctx)
		return err
	})
	return folders, err
}

func (s *mailboxStore) State(ctx context.Context, account jmap.ID) (string, error) {
	folders, err := s.listFolders(ctx, account)
	if err != nil {
		return "", err
	}
	return mailboxState(folders), nil
}

func mailboxState(folders []bridge.Folder) string {
	sorted := append([]bridge.Folder(nil), folders...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	parts := make([]string, 0, len(sorted))
	for _, f := range sorted {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%d|%d", f.ID, f.Name, f.ParentID, f.Total, f.Unread))
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("%x", h)
}

func (s *mailboxStore) Get(ctx context.Context, account jmap.ID, ids []jmap.ID) ([]jmail.Mailbox, []jmap.ID, error) {
	folders, err := s.listFolders(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	if ids == nil {
		out := make([]jmail.Mailbox, 0, len(folders))
		for _, f := range folders {
			out = append(out, folderToMailbox(f))
		}
		return out, nil, nil
	}
	byID := map[jmap.ID]bridge.Folder{}
	for _, f := range folders {
		byID[jmap.ID(f.ID)] = f
	}
	var found []jmail.Mailbox
	var notFound []jmap.ID
	for _, id := range ids {
		if f, ok := byID[id]; ok {
			found = append(found, folderToMailbox(f))
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, nil
}

func (s *mailboxStore) Create(ctx context.Context, account jmap.ID, obj jmail.Mailbox) (jmail.Mailbox, *jmap.SetError) {
	parent := ""
	if obj.ParentID != nil {
		parent = string(*obj.ParentID)
	}
	var created bridge.Folder
	err := s.b.call(ctx, string(account), "CreateFolder", func(ctx context.Context, p bridge.Provider) error {
		var err error
		created, err = p.CreateFolder(ctx, obj.Name, parent)
		return err
	})
	if err != nil {
		return jmail.Mailbox{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	return folderToMailbox(created), nil
}

func (s *mailboxStore) Update(ctx context.Context, account jmap.ID, obj jmail.Mailbox) (*jmail.Mailbox, *jmap.SetError) {
	found, _, err := s.Get(ctx, account, []jmap.ID{obj.ID})
	if err != nil {
		return nil, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	if len(found) == 0 {
		return nil, &jmap.SetError{Type: "notFound"}
	}
	cur := found[0]
	// shortcut: only Name change is supported; ParentID change or other
	// immutable diff => invalidProperties
	var bad []string
	if !idPtrEqual(cur.ParentID, obj.ParentID) {
		bad = append(bad, "parentId")
	}
	if !strPtrEqual(cur.Role, obj.Role) {
		bad = append(bad, "role")
	}
	if cur.SortOrder != obj.SortOrder {
		bad = append(bad, "sortOrder")
	}
	if len(bad) > 0 {
		return nil, &jmap.SetError{Type: "invalidProperties", Properties: bad}
	}
	if cur.Name != obj.Name {
		err := s.b.call(ctx, string(account), "RenameFolder", func(ctx context.Context, p bridge.Provider) error {
			return p.RenameFolder(ctx, string(obj.ID), obj.Name)
		})
		if err != nil {
			return nil, &jmap.SetError{Type: "serverFail", Description: err.Error()}
		}
	}
	return nil, nil
}

func (s *mailboxStore) Destroy(ctx context.Context, account jmap.ID, id jmap.ID) *jmap.SetError {
	err := s.b.call(ctx, string(account), "DeleteFolder", func(ctx context.Context, p bridge.Provider) error {
		return p.DeleteFolder(ctx, string(id))
	})
	if err != nil {
		return &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	return nil
}

func (s *mailboxStore) Changes(ctx context.Context, account jmap.ID, sinceState string, maxChanges int) ([]jmap.ID, []jmap.ID, []jmap.ID, string, bool, error) {
	cur, err := s.State(ctx, account)
	if err != nil {
		return nil, nil, nil, "", false, err
	}
	if sinceState == cur {
		return []jmap.ID{}, []jmap.ID{}, []jmap.ID{}, cur, false, nil
	}
	return nil, nil, nil, "", false, datatype.ErrCannotCalculate
}

func idPtrEqual(a, b *jmap.ID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func strPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
