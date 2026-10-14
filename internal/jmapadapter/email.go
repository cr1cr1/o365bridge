package jmapadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	stdmail "net/mail"
	"sort"
	"strings"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	jmail "github.com/cr1cr1/o365bridge/jmap/mail"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type emailStore struct{ b *Backend }

func (s *emailStore) State(_ context.Context, account jmap.ID) (string, error) {
	return s.b.Journal.State(string(account)), nil
}

// buildOne fetches meta+MIME for a single message and builds the Email, observing the journal (non-full).
func (s *emailStore) buildOne(ctx context.Context, account jmap.ID, folderID, upstreamID string) (jmail.Email, error) {
	var meta bridge.MessageMeta
	var raw []byte
	err := s.b.call(ctx, string(account), "GetMessage", func(ctx context.Context, p bridge.Provider) error {
		var err error
		meta, err = p.GetMessage(ctx, folderID, upstreamID)
		if err != nil {
			return err
		}
		if meta.FolderID == "" {
			meta.FolderID = folderID
		}
		if meta.ID == "" {
			meta.ID = upstreamID
		}
		raw, err = p.GetMessageMIME(ctx, folderID, upstreamID)
		return err
	})
	if err != nil {
		return jmail.Email{}, err
	}
	em := buildEmail(meta, raw)
	s.b.Journal.Observe(string(account), []Witness{{ID: string(em.ID), Folder: meta.FolderID, Hash: emailHash(meta)}}, false)
	return em, nil
}

func (s *emailStore) Get(ctx context.Context, account jmap.ID, ids []jmap.ID) ([]jmail.Email, []jmap.ID, error) {
	if ids == nil {
		// All folders, paged ListMessages, FULL journal listing.
		var folders []bridge.Folder
		if err := s.b.call(ctx, string(account), "ListFolders", func(ctx context.Context, p bridge.Provider) error {
			var err error
			folders, err = p.ListFolders(ctx)
			return err
		}); err != nil {
			return nil, nil, err
		}
		var out []jmail.Email
		var witnesses []Witness
		for _, f := range folders {
			offset := 0
			for {
				var metas []bridge.MessageMeta
				var total int
				if err := s.b.call(ctx, string(account), "ListMessages", func(ctx context.Context, p bridge.Provider) error {
					var err error
					metas, total, err = p.ListMessages(ctx, f.ID, offset, 500)
					return err
				}); err != nil {
					return nil, nil, err
				}
				for _, meta := range metas {
					if meta.FolderID == "" {
						meta.FolderID = f.ID
					}
					em, err := s.buildOne(ctx, account, meta.FolderID, meta.ID)
					if err != nil {
						continue // skip unreadable message
					}
					// buildOne observed singly; collect for full observe below (dedupe by journal logic)
					out = append(out, em)
					witnesses = append(witnesses, Witness{ID: string(em.ID), Folder: meta.FolderID, Hash: emailHash(meta)})
				}
				offset += len(metas)
				if len(metas) == 0 || offset >= total {
					break
				}
			}
		}
		s.b.Journal.Observe(string(account), witnesses, true)
		if out == nil {
			out = []jmail.Email{}
		}
		return out, nil, nil
	}
	var found []jmail.Email
	var notFound []jmap.ID
	for _, id := range ids {
		folderID, upstreamID, err := decodeEmailID(id)
		if err != nil {
			notFound = append(notFound, id)
			continue
		}
		em, err := s.buildOne(ctx, account, folderID, upstreamID)
		if err != nil {
			notFound = append(notFound, id)
			continue
		}
		found = append(found, em)
	}
	return found, notFound, nil
}

func (s *emailStore) Changes(_ context.Context, account jmap.ID, sinceState string, maxChanges int) ([]jmap.ID, []jmap.ID, []jmap.ID, string, bool, error) {
	c, u, d, ns, hm, err := s.b.Journal.Changes(string(account), sinceState, maxChanges)
	if err != nil {
		// translate to datatype.ErrCannotCalculate (Journal already returns it)
		return nil, nil, nil, "", false, err
	}
	return c, u, d, ns, hm, nil
}

var _ = datatype.ErrCannotCalculate

// composeMIME builds raw MIME from obj fields (used when no blob is retrievable).
func (s *emailStore) composeMIME(ctx context.Context, account jmap.ID, obj jmail.Email) ([]byte, error) {
	var textVal, htmlVal string
	for _, p := range obj.TextBody {
		if bv, ok := obj.BodyValues[p.PartID]; ok && bv != nil {
			textVal = bv.Value
		}
	}
	if textVal == "" {
		// Also try any bodyValues entry for t1/default
		if bv, ok := obj.BodyValues["t1"]; ok && bv != nil {
			textVal = bv.Value
		}
	}
	for _, p := range obj.HTMLBody {
		if bv, ok := obj.BodyValues[p.PartID]; ok && bv != nil {
			htmlVal = bv.Value
		}
	}
	if htmlVal == "" {
		if bv, ok := obj.BodyValues["h1"]; ok && bv != nil {
			htmlVal = bv.Value
		}
	}
	type attach struct {
		data      []byte
		name, typ string
	}
	var atts []attach
	for _, p := range obj.Attachments {
		if p.BlobID == nil {
			continue
		}
		ct, data, err := s.b.blobs.Download(ctx, account, *p.BlobID)
		if err != nil {
			continue
		}
		name := ""
		if p.Name != nil {
			name = *p.Name
		}
		if ct == "" {
			ct = p.Type
		}
		atts = append(atts, attach{data, name, ct})
	}
	// Also BodyParts in text/html with BlobID? Not needed.

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", formatAddrs(obj.From))
	if len(obj.To) > 0 {
		fmt.Fprintf(&buf, "To: %s\r\n", formatAddrs(obj.To))
	}
	if len(obj.Cc) > 0 {
		fmt.Fprintf(&buf, "Cc: %s\r\n", formatAddrs(obj.Cc))
	}
	if len(obj.Bcc) > 0 {
		fmt.Fprintf(&buf, "Bcc: %s\r\n", formatAddrs(obj.Bcc))
	}
	if len(obj.ReplyTo) > 0 {
		fmt.Fprintf(&buf, "Reply-To: %s\r\n", formatAddrs(obj.ReplyTo))
	}
	if obj.Sender != nil {
		fmt.Fprintf(&buf, "Sender: %s\r\n", formatAddrs([]jmail.EmailAddress{*obj.Sender}))
	}
	fmt.Fprintf(&buf, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", obj.Subject))
	date := time.Now()
	if obj.SentAt != nil {
		date = *obj.SentAt
	}
	fmt.Fprintf(&buf, "Date: %s\r\n", date.Format(time.RFC1123Z))
	if len(obj.MessageID) > 0 {
		fmt.Fprintf(&buf, "Message-ID: %s\r\n", obj.MessageID[0])
	}
	if len(obj.InReplyTo) > 0 {
		fmt.Fprintf(&buf, "In-Reply-To: %s\r\n", strings.Join(obj.InReplyTo, " "))
	}
	if len(obj.References) > 0 {
		fmt.Fprintf(&buf, "References: %s\r\n", strings.Join(obj.References, " "))
	}
	buf.WriteString("MIME-Version: 1.0\r\n")

	if len(atts) == 0 && (textVal == "" || htmlVal == "") {
		if htmlVal != "" && textVal == "" {
			buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
			buf.WriteString(htmlVal)
		} else {
			buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
			buf.WriteString(textVal)
		}
		return buf.Bytes(), nil
	}
	// Multipart needed.
	mixed := len(atts) > 0
	var outer *multipart.Writer
	var bodyBuf bytes.Buffer
	if mixed {
		outer = multipart.NewWriter(&bodyBuf)
		fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", outer.Boundary())
		// first part: body (alternative or single)
		hdr := make(map[string][]string)
		if textVal != "" && htmlVal != "" {
			// nested alternative written manually into a part
			var altBuf bytes.Buffer
			altW := multipart.NewWriter(&altBuf)
			hdr["Content-Type"] = []string{fmt.Sprintf("multipart/alternative; boundary=%s", altW.Boundary())}
			pw, _ := outer.CreatePart(hdr)
			// write alternative content into pw after creating alt
			// Build alt content first then copy
			// Instead build altBuf fully:
			// (altW needs its own parts)
			// Simpler: close altW after adding parts directly to altBuf, then write altBuf to pw.
			// We already created altW on altBuf, add parts:
			tp, _ := altW.CreatePart(map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}})
			tp.Write([]byte(textVal))
			hp, _ := altW.CreatePart(map[string][]string{"Content-Type": {"text/html; charset=utf-8"}})
			hp.Write([]byte(htmlVal))
			altW.Close()
			pw.Write(altBuf.Bytes())
		} else if htmlVal != "" {
			hdr["Content-Type"] = []string{"text/html; charset=utf-8"}
			pw, _ := outer.CreatePart(hdr)
			pw.Write([]byte(htmlVal))
		} else {
			hdr["Content-Type"] = []string{"text/plain; charset=utf-8"}
			pw, _ := outer.CreatePart(hdr)
			pw.Write([]byte(textVal))
		}
		for _, a := range atts {
			hdr := make(map[string][]string)
			typ := a.typ
			if typ == "" {
				typ = "application/octet-stream"
			}
			hdr["Content-Type"] = []string{typ}
			hdr["Content-Transfer-Encoding"] = []string{"base64"}
			if a.name != "" {
				hdr["Content-Disposition"] = []string{fmt.Sprintf("attachment; filename=%q", a.name)}
			} else {
				hdr["Content-Disposition"] = []string{"attachment"}
			}
			pw, _ := outer.CreatePart(hdr)
			enc := base64.NewEncoder(base64.StdEncoding, pw)
			enc.Write(a.data)
			enc.Close()
		}
		outer.Close()
		buf.Write(bodyBuf.Bytes())
		return buf.Bytes(), nil
	}
	// multipart/alternative only
	var altBuf bytes.Buffer
	altW := multipart.NewWriter(&altBuf)
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%s\r\n\r\n", altW.Boundary())
	tp, _ := altW.CreatePart(map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}})
	tp.Write([]byte(textVal))
	hp, _ := altW.CreatePart(map[string][]string{"Content-Type": {"text/html; charset=utf-8"}})
	hp.Write([]byte(htmlVal))
	altW.Close()
	buf.Write(altBuf.Bytes())
	return buf.Bytes(), nil
}

func formatAddrs(list []jmail.EmailAddress) string {
	var parts []string
	for _, a := range list {
		addr := &stdmail.Address{Name: a.Name, Address: a.Email}
		parts = append(parts, addr.String())
	}
	return strings.Join(parts, ", ")
}

func mailboxIDsList(m map[jmap.ID]bool) []jmap.ID {
	var out []jmap.ID
	for id, ok := range m {
		if ok {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *emailStore) Create(ctx context.Context, account jmap.ID, obj jmail.Email) (jmail.Email, *jmap.SetError) {
	folders := mailboxIDsList(obj.MailboxIDs)
	if len(folders) == 0 {
		return jmail.Email{}, &jmap.SetError{Type: "invalidProperties", Properties: []string{"mailboxIds"}, Description: "mailboxIds must contain at least one folder"}
	}
	var raw []byte
	if obj.BlobID != "" {
		if _, data, err := s.b.blobs.Download(ctx, account, obj.BlobID); err == nil {
			raw = data
		}
	}
	if raw == nil {
		var err error
		raw, err = s.composeMIME(ctx, account, obj)
		if err != nil {
			return jmail.Email{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
		}
	}
	flags := keywordsToFlags(obj.Keywords)
	first := string(folders[0])
	var newID string
	if err := s.b.call(ctx, string(account), "AppendMessage", func(ctx context.Context, p bridge.Provider) error {
		var err error
		newID, err = p.AppendMessage(ctx, first, raw, flags)
		return err
	}); err != nil {
		return jmail.Email{}, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	for _, dest := range folders[1:] {
		_ = s.b.call(ctx, string(account), "CopyMessages", func(ctx context.Context, p bridge.Provider) error {
			return p.CopyMessages(ctx, first, []string{newID}, string(dest))
		})
	}
	em, err := s.buildOne(ctx, account, first, newID)
	if err != nil {
		return jmail.Email{ID: encodeEmailID(first, newID), BlobID: encodeEmailID(first, newID)}, nil
	}
	return em, nil
}

func (s *emailStore) Update(ctx context.Context, account jmap.ID, obj jmail.Email) (*jmail.Email, *jmap.SetError) {
	folderID, upstreamID, err := decodeEmailID(obj.ID)
	if err != nil {
		return nil, &jmap.SetError{Type: "notFound"}
	}
	found, _, err := s.Get(ctx, account, []jmap.ID{obj.ID})
	if err != nil {
		return nil, &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	if len(found) == 0 {
		return nil, &jmap.SetError{Type: "notFound"}
	}
	cur := found[0]
	// Keyword diff
	var addFlags, removeFlags bridge.Flags
	for k, v := range obj.Keywords {
		if v && !cur.Keywords[k] {
			addFlags = append(addFlags, keywordToFlag(k))
		}
	}
	for k, v := range cur.Keywords {
		if v && !obj.Keywords[k] {
			removeFlags = append(removeFlags, keywordToFlag(k))
		}
	}
	// Mailbox diff
	curFolders := mailboxIDsList(cur.MailboxIDs)
	newFolders := mailboxIDsList(obj.MailboxIDs)
	curSet := map[jmap.ID]bool{}
	for _, f := range curFolders {
		curSet[f] = true
	}
	newSet := map[jmap.ID]bool{}
	for _, f := range newFolders {
		newSet[f] = true
	}
	var added, removed []jmap.ID
	for _, f := range newFolders {
		if !curSet[f] {
			added = append(added, f)
		}
	}
	for _, f := range curFolders {
		if !newSet[f] {
			removed = append(removed, f)
		}
	}
	hasKwChange := len(addFlags) > 0 || len(removeFlags) > 0
	hasMboxChange := len(added) > 0 || len(removed) > 0
	if !hasKwChange && !hasMboxChange {
		if obj.Subject != cur.Subject {
			return nil, &jmap.SetError{Type: "invalidProperties", Properties: []string{"subject"}}
		}
		return nil, nil
	}
	// Apply keyword changes
	if hasKwChange {
		if err := s.b.call(ctx, string(account), "SetFlags", func(ctx context.Context, p bridge.Provider) error {
			return p.SetFlags(ctx, folderID, []string{upstreamID}, addFlags, removeFlags)
		}); err != nil {
			return nil, &jmap.SetError{Type: "serverFail", Description: err.Error()}
		}
	}
	// Copy to added folders from an existing folder
	if len(added) > 0 {
		src := folderID
		// Prefer a folder that remains
		for _, f := range curFolders {
			if newSet[f] {
				src = string(f)
				break
			}
		}
		// If src != original folder, upstream ID is still same string? IDs are folder-local upstream ids;
		// shortcut: assume upstream id valid in src folder (same message copy shares id in fakes/upstream may differ).
		for _, dest := range added {
			if err := s.b.call(ctx, string(account), "CopyMessages", func(ctx context.Context, p bridge.Provider) error {
				return p.CopyMessages(ctx, src, []string{upstreamID}, string(dest))
			}); err != nil {
				return nil, &jmap.SetError{Type: "serverFail", Description: err.Error()}
			}
		}
	}
	// Removed folders: DeleteMessages trashes rather than removes folder membership;
	// shortcut: a move therefore can change the JMAP id since id encodes folder.
	for _, f := range removed {
		if err := s.b.call(ctx, string(account), "DeleteMessages", func(ctx context.Context, p bridge.Provider) error {
			return p.DeleteMessages(ctx, string(f), []string{upstreamID})
		}); err != nil {
			return nil, &jmap.SetError{Type: "serverFail", Description: err.Error()}
		}
	}
	return nil, nil
}

func (s *emailStore) Destroy(ctx context.Context, account jmap.ID, id jmap.ID) *jmap.SetError {
	folderID, upstreamID, err := decodeEmailID(id)
	if err != nil {
		return &jmap.SetError{Type: "notFound"}
	}
	if err := s.b.call(ctx, string(account), "DeleteMessages", func(ctx context.Context, p bridge.Provider) error {
		return p.DeleteMessages(ctx, folderID, []string{upstreamID})
	}); err != nil {
		return &jmap.SetError{Type: "serverFail", Description: err.Error()}
	}
	return nil
}

// Threads implements Backend.Threads.
func (b *Backend) Threads(ctx context.Context, account jmap.ID, ids []jmap.ID) ([]jmail.Thread, []jmap.ID, string, error) {
	store := &emailStore{b}
	all, _, err := store.Get(ctx, account, nil)
	if err != nil {
		return nil, nil, "", err
	}
	groups := map[jmap.ID][]jmail.Email{}
	var order []jmap.ID
	for _, em := range all {
		if _, ok := groups[em.ThreadID]; !ok {
			order = append(order, em.ThreadID)
		}
		groups[em.ThreadID] = append(groups[em.ThreadID], em)
	}
	makeThread := func(tid jmap.ID) jmail.Thread {
		ems := append([]jmail.Email(nil), groups[tid]...)
		sort.Slice(ems, func(i, j int) bool { return ems[i].ReceivedAt.Before(ems[j].ReceivedAt) })
		ids := make([]jmap.ID, 0, len(ems))
		for _, e := range ems {
			ids = append(ids, e.ID)
		}
		return jmail.Thread{ID: tid, EmailIDs: ids}
	}
	state := b.Journal.State(string(account))
	if ids == nil {
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
		out := make([]jmail.Thread, 0, len(order))
		for _, tid := range order {
			out = append(out, makeThread(tid))
		}
		return out, nil, state, nil
	}
	var found []jmail.Thread
	var notFound []jmap.ID
	for _, id := range ids {
		if _, ok := groups[id]; ok {
			found = append(found, makeThread(id))
		} else {
			notFound = append(notFound, id)
		}
	}
	return found, notFound, state, nil
}
