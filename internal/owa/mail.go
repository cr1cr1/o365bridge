package owa

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func findFolderReq(traversal string) map[string]any {
	return envelope("FindFolderJsonRequest:#Exchange", "FindFolderRequest:#Exchange", map[string]any{
		"FolderShape": map[string]any{
			"BaseShape": "Default",
			"AdditionalProperties": []any{
				propURI("folder:FolderClass"), propURI("folder:ParentFolderId"), propURI("folder:DistinguishedFolderId"),
			},
		},
		"Paging":             nil,
		"ParentFolderIds":    []any{map[string]any{"__type": "DistinguishedFolderId:#Exchange", "Id": "msgfolderroot"}},
		"ReturnParentFolder": true,
		"Traversal":          traversal,
	})
}

func (p *provider) Ping(ctx context.Context) error {
	_, err := p.call(ctx, "FindFolder", findFolderReq("Shallow"))
	return err
}

var specialByDistinguished = map[string]string{
	"inbox": "inbox", "sentitems": "sent", "drafts": "drafts",
	"deleteditems": "trash", "junkemail": "junk", "archive": "archive",
	"archivemsgfolderroot": "archive",
}

func folderFromOWA(f map[string]any) bridge.Folder {
	dist := strings.ToLower(str(f, "DistinguishedFolderId"))
	return bridge.Folder{
		ID: str(f, "FolderId", "Id"), Name: str(f, "DisplayName"),
		ParentID: str(f, "ParentFolderId", "Id"),
		Total:    int(num(f, "TotalCount")), Unread: int(num(f, "UnreadCount")),
		Special: specialByDistinguished[dist],
	}
}

func (p *provider) ListFolders(ctx context.Context) ([]bridge.Folder, error) {
	res, err := p.call(ctx, "FindFolder", findFolderReq("Deep"))
	if err != nil {
		return nil, err
	}
	var out []bridge.Folder
	for _, fv := range arr(res, "RootFolder", "Folders") {
		f := asMap(fv)
		if f == nil {
			continue
		}
		if t := str(f, "__type"); t != "" && !strings.HasPrefix(t, "Folder") {
			continue
		}
		out = append(out, folderFromOWA(f))
	}
	return out, nil
}

func (p *provider) CreateFolder(ctx context.Context, name, parentID string) (bridge.Folder, error) {
	req := envelope("CreateFolderJsonRequest:#Exchange", "CreateFolderRequest:#Exchange", map[string]any{
		"ParentFolderId": targetFolder(orDefault(parentID, "msgfolderroot")),
		"Folders":        []any{map[string]any{"__type": "Folder:#Exchange", "FolderClass": "IPF.Note", "DisplayName": name}},
	})
	res, err := p.call(ctx, "CreateFolder", req)
	if err != nil {
		return bridge.Folder{}, err
	}
	f := bridge.Folder{Name: name, ParentID: parentID}
	if item := firstItem(res); item != nil {
		f.ID = str(item, "FolderId", "Id")
		if f.ID == "" {
			f = folderFromOWA(item)
			f.Name = name
		}
	}
	// CreateFolder response Folders[0] shape (not Items)
	if f.ID == "" {
		if fs := arr(res, "Folders"); len(fs) > 0 {
			f.ID = str(fs[0], "FolderId", "Id")
		}
	}
	return f, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (p *provider) DeleteFolder(ctx context.Context, id string) error {
	req := envelope("DeleteFolderJsonRequest:#Exchange", "DeleteFolderRequest:#Exchange", map[string]any{
		"FolderIds": []any{folderRef(id)}, "DeleteType": "SoftDelete",
	})
	_, err := p.call(ctx, "DeleteFolder", req)
	return err
}

// RenameFolder renames and/or moves a folder. Empty name keeps the
// current display name; empty parentID keeps the current parent.
func (p *provider) RenameFolder(ctx context.Context, id, name, parentID string) error {
	if parentID != "" {
		req := envelope("MoveFolderJsonRequest:#Exchange", "MoveFolderRequest:#Exchange", map[string]any{
			"FolderIds": []any{folderRef(id)}, "ToFolderId": targetFolder(parentID),
		})
		if _, err := p.call(ctx, "MoveFolder", req); err != nil {
			return err
		}
	}
	if name == "" {
		return nil
	}
	req := envelope("UpdateFolderJsonRequest:#Exchange", "UpdateFolderRequest:#Exchange", map[string]any{
		"FolderChanges": []any{map[string]any{"__type": "FolderChange:#Exchange",
			"FolderId": folderRef(id),
			"Updates": []any{map[string]any{"__type": "SetFolderField:#Exchange",
				"Folder": map[string]any{"__type": "Folder:#Exchange", "DisplayName": name},
				"Path":   propURI("FolderDisplayName"),
			}},
		}},
	})
	_, err := p.call(ctx, "UpdateFolder", req)
	return err
}

// --- messages ---

func flagsFromItem(item map[string]any) bridge.Flags {
	var fl bridge.Flags
	if boolean(item, "IsRead") {
		fl = append(fl, `\seen`)
	}
	if strings.EqualFold(str(item, "Flag", "FlagStatus"), "Flagged") {
		fl = append(fl, `\flagged`)
	}
	return fl
}

func metaFromItem(item map[string]any, folderID string) bridge.MessageMeta {
	date := parseTime(str(item, "DateTimeSent"))
	if date.IsZero() {
		date = parseTime(str(item, "DateTimeReceived"))
	}
	from := mailboxEmail(item["From"])
	if from == "" {
		from = mailboxEmail(item["Sender"])
	}
	return bridge.MessageMeta{
		ID: str(item, "ItemId", "Id"), FolderID: folderID,
		Subject: str(item, "Subject"), From: from,
		To: recipients(item, "ToRecipients"), Date: date,
		Flags: flagsFromItem(item), Size: int64(num(item, "Size")),
		Attachments: boolean(item, "HasAttachments"),
		MessageID:   str(item, "InternetMessageId"),
	}
}

func findItemReq(folderID string, offset, limit int, extra map[string]any) map[string]any {
	body := map[string]any{
		"ItemShape": map[string]any{"__type": "ItemResponseShape:#Exchange", "BaseShape": "IdOnly",
			// Needed to match a client's Sent copy against the copy
			// the send itself stored (see imapserver cmdAppend).
			"AdditionalProperties": []any{propURI("message:InternetMessageId")}},
		"ParentFolderIds": []any{folderRef(folderID)},
		"Traversal":       "Shallow",
		"Paging":          map[string]any{"__type": "IndexedPageView:#Exchange", "BasePoint": "Beginning", "Offset": offset, "MaxEntriesReturned": limit},
		"ViewFilter":      "All", "IsWarmUpSearch": false, "FocusedViewFilter": -1, "Grouping": nil,
		"ShapeName": "MailListItem",
		"SortOrder": []any{map[string]any{"__type": "SortResults:#Exchange", "Order": "Descending", "Path": propURI("DateTimeCreated")}},
	}
	for k, v := range extra {
		body[k] = v
	}
	return map[string]any{"__type": "FindItemJsonRequest:#Exchange", "Header": hdr(), "Body": func() map[string]any {
		b := map[string]any{"__type": "FindItemRequest:#Exchange"}
		for k, v := range body {
			b[k] = v
		}
		return b
	}()}
}

// isMailItem skips calendar, task, contact and similar rows: deleted
// items of every type land in generic folders (trash), and only mail
// items have a usable MIME form.
func isMailItem(item map[string]any) bool {
	t := str(item, "__type")
	for _, prefix := range []string{"CalendarItem", "TaskItem", "ContactItem", "DistributionListItem"} {
		if strings.HasPrefix(t, prefix) {
			return false
		}
	}
	class := str(item, "ItemClass")
	for _, prefix := range []string{"IPM.Appointment", "IPM.Task", "IPM.Contact", "IPM.DistList", "IPM.Activity"} {
		if strings.HasPrefix(class, prefix) {
			return false
		}
	}
	return true
}

func findItems(ctx context.Context, p *provider, folderID string, offset, limit int, extra map[string]any) ([]map[string]any, int, error) {
	res, err := p.call(ctx, "FindItem", findItemReq(folderID, offset, limit, extra))
	if err != nil {
		return nil, 0, err
	}
	var items []map[string]any
	for _, iv := range arr(res, "RootFolder", "Items") {
		m := asMap(iv)
		if m == nil || !isMailItem(m) {
			continue
		}
		items = append(items, m)
	}
	total := int(num(res, "RootFolder", "TotalItemsInView"))
	if total == 0 {
		total = len(items)
	}
	return items, total, nil
}

func (p *provider) ListMessages(ctx context.Context, folderID string, offset, limit int) ([]bridge.MessageMeta, int, error) {
	if limit <= 0 {
		limit = 50
	}
	items, total, err := findItems(ctx, p, folderID, offset, limit, nil)
	if err != nil {
		return nil, 0, err
	}
	out := make([]bridge.MessageMeta, 0, len(items))
	for _, it := range items {
		out = append(out, metaFromItem(it, folderID))
	}
	return out, total, nil
}

func getItemReq(ids []string, shape map[string]any) map[string]any {
	return envelope("GetItemJsonRequest:#Exchange", "GetItemRequest:#Exchange", map[string]any{
		"ItemShape": shape, "ItemIds": itemIDs(ids),
	})
}

func (p *provider) GetMessage(ctx context.Context, folderID, id string) (bridge.MessageMeta, error) {
	shape := map[string]any{"__type": "ItemResponseShape:#Exchange", "BaseShape": "AllProperties",
		"AdditionalProperties": []any{propURI("item:Size")}}
	res, err := p.call(ctx, "GetItem", getItemReq([]string{id}, shape))
	if err != nil {
		return bridge.MessageMeta{}, err
	}
	item := firstItem(res)
	if item == nil {
		return bridge.MessageMeta{}, fmt.Errorf("owa: GetMessage: item not found")
	}
	return metaFromItem(item, folderID), nil
}

func (p *provider) GetMessageMIME(ctx context.Context, folderID, id string) ([]byte, error) {
	shape := map[string]any{"__type": "ItemResponseShape:#Exchange", "BaseShape": "IdOnly",
		"AdditionalProperties": []any{propURI("item:Size")}, "IncludeMimeContent": true}
	res, err := p.call(ctx, "GetItem", getItemReq([]string{id}, shape))
	if err != nil {
		return nil, err
	}
	item := firstItem(res)
	if item == nil {
		return nil, fmt.Errorf("owa: GetMessageMIME: item not found")
	}
	var b64 string
	if mc := asMap(item["MimeContent"]); mc != nil {
		b64 = str(mc, "Value")
		if b64 == "" {
			b64 = str(mc, "Content")
		}
	} else {
		b64 = str(item, "MimeContent")
	}
	if b64 == "" {
		return nil, fmt.Errorf("owa: GetMessageMIME: no MimeContent in response")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		// try raw (non-base64) tolerant? No: report.
		return nil, fmt.Errorf("owa: GetMessageMIME: decode mime content: %w", err)
	}
	return raw, nil
}

func buildQuery(c bridge.SearchCriteria) string {
	var parts []string
	if c.Text != "" {
		parts = append(parts, c.Text)
	}
	if c.From != "" {
		parts = append(parts, "from:"+c.From)
	}
	if c.Subject != "" {
		parts = append(parts, "subject:"+c.Subject)
	}
	if !c.Since.IsZero() {
		parts = append(parts, "received>="+c.Since.Format("2006-01-02"))
	}
	if !c.Before.IsZero() {
		parts = append(parts, "received<"+c.Before.Format("2006-01-02"))
	}
	if c.Unseen {
		parts = append(parts, "isread:false")
	}
	if c.Flagged {
		parts = append(parts, "flag:flagged")
	}
	return strings.Join(parts, " ")
}

func (p *provider) SearchMessages(ctx context.Context, folderID string, c bridge.SearchCriteria) ([]string, error) {
	extra := map[string]any{}
	if q := buildQuery(c); q != "" {
		extra["QueryString"] = q
	}
	items, _, err := findItems(ctx, p, folderID, 0, 1000, extra)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, it := range items {
		if id := str(it, "ItemId", "Id"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func hasFlag(f bridge.Flags, names ...string) bool {
	for _, x := range f {
		for _, n := range names {
			if strings.EqualFold(x, n) {
				return true
			}
		}
	}
	return false
}

func (p *provider) SetFlags(ctx context.Context, folderID string, ids []string, add, remove bridge.Flags) error {
	var updates []any
	if hasFlag(add, `\seen`, "seen") || hasFlag(remove, `\seen`, "seen") {
		updates = append(updates, map[string]any{"__type": "SetItemField:#Exchange",
			"Item": map[string]any{"__type": "Message:#Exchange", "IsRead": hasFlag(add, `\seen`, "seen")},
			"Path": propURI("IsRead")})
	}
	if hasFlag(add, `\flagged`, "flagged") || hasFlag(remove, `\flagged`, "flagged") {
		status := "NotFlagged"
		if hasFlag(add, `\flagged`, "flagged") {
			status = "Flagged"
		}
		updates = append(updates, map[string]any{"__type": "SetItemField:#Exchange",
			"Item": map[string]any{"__type": "Message:#Exchange", "Flag": map[string]any{"__type": "FlagType:#Exchange", "CompleteDate": nil, "DueDate": nil, "StartDate": nil, "FlagStatus": status}},
			"Path": propURI("Flag")})
	}
	if len(updates) == 0 || len(ids) == 0 {
		return nil
	}
	changes := make([]any, 0, len(ids))
	for _, id := range ids {
		changes = append(changes, map[string]any{"__type": "ItemChange:#Exchange", "ItemId": itemID(id), "Updates": updates})
	}
	req := envelope("UpdateItemJsonRequest:#Exchange", "UpdateItemRequest:#Exchange", map[string]any{
		"ConflictResolution": "AlwaysOverwrite", "ItemChanges": changes,
		"MessageDisposition": "SaveOnly", "SendCalendarInvitationsOrCancellations": "SendToNone", "SuppressReadReceipts": true,
	})
	_, err := p.call(ctx, "UpdateItem", req)
	return err
}

func (p *provider) moveOrCopy(ctx context.Context, action string, ids []string, dest string) error {
	req := envelope(action+"ItemJsonRequest:#Exchange", action+"ItemRequest:#Exchange", map[string]any{
		"ItemIds": itemIDs(ids), "ToFolderId": targetFolder(dest),
	})
	_, err := p.call(ctx, action+"Item", req)
	return err
}
func (p *provider) MoveMessages(ctx context.Context, folderID string, ids []string, destFolderID string) error {
	return p.moveOrCopy(ctx, "Move", ids, destFolderID)
}
func (p *provider) CopyMessages(ctx context.Context, folderID string, ids []string, destFolderID string) error {
	return p.moveOrCopy(ctx, "Copy", ids, destFolderID)
}

func (p *provider) DeleteMessages(ctx context.Context, folderID string, ids []string) error {
	// EXPUNGE semantics: remove permanently. Marking \Deleted already
	// moved the message to Deleted Items (imapserver moveDeletedNow);
	// this path removes messages marked inside the trash itself or
	// left behind when that move failed.
	req := envelope("DeleteItemJsonRequest:#Exchange", "DeleteItemRequest:#Exchange", map[string]any{
		"ItemIds": itemIDs(ids), "DeleteType": "HardDelete", "ReturnMovedItemIds": false, "SuppressReadRecipts": true,
	})
	_, err := p.call(ctx, "DeleteItem", req)
	return err
}

// mimeMessage builds the Message:#Exchange object exactly as _o's
// CreateMime/SendMime + CreateItem do (emails.js): the raw MIME rides
// in MimeContent{CharacterSet:"UTF-8", Value: base64(mime)} — note _o
// puts no __type on MimeContent. Recipients, subject, bodies and
// attachments all ride inside the MIME, so nothing needs the
// ParseMIME-rebuilt object (which silently dropped attachments).
//
// When withFlags is set (Append, mirroring CreateMime), the flag
// fields ride alongside: IsRead, Flag, and the 0x0E07 ExtendedProperty
// carrying MAPI_MSGFLAG_UNSENT (8) for \draft.
//
// Remaining fidelity loss vs _o: for >4MB Office365 messages _o splits
// attachments out (CheckForOffice365Attachments) and uploads them via
// CreateAttachmentFromLocalFile before sending; we always send one
// MimeContent payload, which may hit Office365's size limit there.
func mimeMessage(raw []byte, flags bridge.Flags, withFlags bool) map[string]any {
	msg := map[string]any{
		"__type": "Message:#Exchange",
		"MimeContent": map[string]any{
			"CharacterSet": "UTF-8",
			"Value":        base64.StdEncoding.EncodeToString(raw),
		},
	}
	if withFlags {
		unsent := "0"
		if hasFlag(flags, `\draft`, "draft") {
			unsent = "8" // MAPI_MSGFLAG_UNSENT
		}
		flagStatus := "NotFlagged"
		if hasFlag(flags, `\flagged`, "flagged") {
			flagStatus = "Flagged"
		}
		msg["ExtendedProperty"] = []any{map[string]any{
			"ExtendedFieldURI": map[string]any{"PropertyTag": "0x0E07", "PropertyType": "Integer"},
			"Value":            unsent,
		}}
		msg["IsRead"] = hasFlag(flags, `\seen`, "seen")
		msg["Flag"] = map[string]any{"__type": "FlagType:#Exchange", "CompleteDate": nil, "DueDate": nil, "StartDate": nil, "FlagStatus": flagStatus}
	}
	return msg
}

func (p *provider) createMessageItem(ctx context.Context, mime []byte, flags bridge.Flags, disposition, folderID string, withFlags bool) (string, error) {
	fields := map[string]any{
		"Items": []any{mimeMessage(mime, flags, withFlags)}, "MessageDisposition": disposition,
	}
	if folderID != "" {
		fields["SavedItemFolderId"] = targetFolder(folderID)
	}
	req := envelope("CreateItemJsonRequest:#Exchange", "CreateItemRequest:#Exchange", fields)
	res, err := p.call(ctx, "CreateItem", req)
	if err != nil {
		return "", err
	}
	if item := firstItem(res); item != nil {
		return str(item, "ItemId", "Id"), nil
	}
	return "", nil
}

func (p *provider) AppendMessage(ctx context.Context, folderID string, mime []byte, flags bridge.Flags) (string, error) {
	return p.createMessageItem(ctx, mime, flags, "SaveOnly", folderID, true)
}
func (p *provider) SendMessage(ctx context.Context, mime []byte) error {
	_, err := p.createMessageItem(ctx, mime, nil, "SendAndSaveCopy", "", false)
	return err
}
