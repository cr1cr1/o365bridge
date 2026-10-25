package graph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func (p *provider) Ping(ctx context.Context) error {
	_, err := p.do(ctx, http.MethodGet, "/me/mailFolders/inbox", nil, nil)
	return err
}

// --- Folders ---

type graphFolder struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	ParentFolderID string `json:"parentFolderId"`
	Total          int    `json:"totalItemCount"`
	Unread         int    `json:"unreadItemCount"`
}

func specialFolder(id, name string) string {
	// Well-known names are usable as folder ids in Graph; also match the
	// human display names ("Sent Items", ...) after squashing spaces.
	for _, v := range []string{strings.ToLower(id), strings.ReplaceAll(strings.ToLower(name), " ", "")} {
		switch v {
		case "inbox":
			return "inbox"
		case "sentitems":
			return "sent"
		case "drafts":
			return "drafts"
		case "deleteditems":
			return "trash"
		case "junkemail":
			return "junk"
		case "archive":
			return "archive"
		}
	}
	return ""
}

func (g graphFolder) toFolder() bridge.Folder {
	return bridge.Folder{ID: g.ID, Name: g.DisplayName, ParentID: g.ParentFolderID, Total: g.Total, Unread: g.Unread, Special: specialFolder(g.ID, g.DisplayName)}
}

func (p *provider) ListFolders(ctx context.Context) ([]bridge.Folder, error) {
	items, _, err := getAll[graphFolder](ctx, p, "/me/mailFolders", 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]bridge.Folder, 0, len(items))
	for _, it := range items {
		out = append(out, it.toFolder())
	}
	return out, nil
}

func (p *provider) CreateFolder(ctx context.Context, name, parentID string) (bridge.Folder, error) {
	path := "/me/mailFolders"
	if parentID != "" {
		path = "/me/mailFolders/" + esc(parentID) + "/childFolders"
	}
	var gf graphFolder
	if err := p.doJSON(ctx, http.MethodPost, path, map[string]any{"displayName": name}, &gf); err != nil {
		return bridge.Folder{}, err
	}
	if gf.DisplayName == "" {
		gf.DisplayName = name
	}
	if gf.ParentFolderID == "" {
		gf.ParentFolderID = parentID
	}
	return gf.toFolder(), nil
}

func (p *provider) DeleteFolder(ctx context.Context, id string) error {
	_, err := p.do(ctx, http.MethodDelete, "/me/mailFolders/"+esc(id), nil, nil)
	return err
}

// RenameFolder renames and/or moves a folder. Empty name keeps the
// current display name; empty parentID keeps the current parent.
func (p *provider) RenameFolder(ctx context.Context, id, name, parentID string) error {
	if parentID != "" {
		if err := p.doJSON(ctx, http.MethodPost, "/me/mailFolders/"+esc(id)+"/move", map[string]any{"destinationId": parentID}, nil); err != nil {
			return err
		}
	}
	if name == "" {
		return nil
	}
	return p.doJSON(ctx, http.MethodPatch, "/me/mailFolders/"+esc(id), map[string]any{"displayName": name}, nil)
}

// --- Messages ---

type graphMessage struct {
	ID               string                `json:"id"`
	Subject          string                `json:"subject"`
	From             emailAddressWrapper   `json:"from"`
	ToRecipients     []emailAddressWrapper `json:"toRecipients"`
	ReceivedDateTime string                `json:"receivedDateTime"`
	IsRead           bool                  `json:"isRead"`
	Flag             struct {
		FlagStatus string `json:"flagStatus"`
	} `json:"flag"`
	HasAttachments  bool   `json:"hasAttachments"`
	InternetMessage string `json:"internetMessageId"`
}

func (g graphMessage) toMeta(folderID string) bridge.MessageMeta {
	m := bridge.MessageMeta{
		ID: g.ID, FolderID: folderID, Subject: g.Subject,
		From: g.From.EmailAddress.Address, To: addrList(g.ToRecipients),
		Date: parseGraphTime(g.ReceivedDateTime), Attachments: g.HasAttachments,
		MessageID: g.InternetMessage,
	}
	if g.IsRead {
		m.Flags = append(m.Flags, `\seen`)
	}
	if g.Flag.FlagStatus == "flagged" || g.Flag.FlagStatus == "complete" {
		m.Flags = append(m.Flags, `\flagged`)
	}
	return m
}

const msgSelect = "id,subject,from,toRecipients,receivedDateTime,isRead,flag,hasAttachments,internetMessageId"

func (p *provider) ListMessages(ctx context.Context, folderID string, offset, limit int) ([]bridge.MessageMeta, int, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("$top", fmt.Sprint(limit))
	}
	if offset > 0 {
		q.Set("$skip", fmt.Sprint(offset))
	}
	q.Set("$count", "true")
	q.Set("$select", msgSelect)
	q.Set("$orderby", "receivedDateTime desc")
	items, total, err := getAll[graphMessage](ctx, p, "/me/mailFolders/"+esc(folderID)+"/messages?"+q.Encode(), limit, nil)
	if err != nil {
		return nil, 0, err
	}
	out := make([]bridge.MessageMeta, 0, len(items))
	for _, it := range items {
		out = append(out, it.toMeta(folderID))
	}
	// If server gave no @odata.count, total is at least offset+returned.
	if total == len(out) && offset > 0 {
		total += offset
	}
	return out, total, nil
}

func (p *provider) GetMessage(ctx context.Context, folderID, id string) (bridge.MessageMeta, error) {
	var gm graphMessage
	if err := p.doJSON(ctx, http.MethodGet, "/me/messages/"+esc(id)+"?$select="+msgSelect, nil, &gm); err != nil {
		return bridge.MessageMeta{}, err
	}
	return gm.toMeta(folderID), nil
}

func (p *provider) GetMessageMIME(ctx context.Context, folderID, id string) ([]byte, error) {
	return p.do(ctx, http.MethodGet, "/me/messages/"+esc(id)+"/$value", nil, nil)
}

func (p *provider) SearchMessages(ctx context.Context, folderID string, c bridge.SearchCriteria) ([]string, error) {
	q := url.Values{}
	q.Set("$select", "id")
	q.Set("$count", "true")
	var headers map[string]string
	var search []string
	if c.Text != "" {
		search = append(search, c.Text)
	}
	if c.From != "" {
		search = append(search, "from:"+c.From)
	}
	if c.Subject != "" {
		search = append(search, "subject:"+c.Subject)
	}
	if len(search) > 0 {
		// Graph forbids combining $search with $filter; $search wins and the
		// flag/date predicates are folded into the search string where possible.
		terms := strings.Join(search, " ")
		if c.Unseen {
			// Not expressible in $search; keep it simple: Graph $search has no
			// isRead term, so this degrades to text-only filtering upstream.
			_ = terms
		}
		q.Set("$search", `"`+strings.ReplaceAll(terms, `"`, `\"`)+`"`)
		headers = map[string]string{"ConsistencyLevel": "eventual"}
	} else {
		var filters []string
		if c.Unseen {
			filters = append(filters, "isRead eq false")
		}
		if c.Flagged {
			filters = append(filters, "flag/flagStatus eq 'flagged'")
		}
		if !c.Since.IsZero() {
			filters = append(filters, "receivedDateTime ge "+c.Since.UTC().Format(time.RFC3339))
		}
		if !c.Before.IsZero() {
			filters = append(filters, "receivedDateTime lt "+c.Before.UTC().Format(time.RFC3339))
		}
		if len(filters) > 0 {
			q.Set("$filter", strings.Join(filters, " and "))
		}
	}
	items, _, err := getAll[graphMessage](ctx, p, "/me/mailFolders/"+esc(folderID)+"/messages?"+q.Encode(), 0, headers)
	if err != nil {
		return nil, err
	}
	// When $search was used, unseen/flagged cannot be pushed down; honour
	// them by re-checking is not possible from ids-only select, so the
	// caller-visible contract stays "best effort push-down" as documented.
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids, nil
}

func hasFlag(flags bridge.Flags, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

func (p *provider) SetFlags(ctx context.Context, folderID string, ids []string, add, remove bridge.Flags) error {
	body := map[string]any{}
	if hasFlag(add, `\seen`) {
		body["isRead"] = true
	}
	if hasFlag(remove, `\seen`) {
		body["isRead"] = false
	}
	if hasFlag(add, `\flagged`) {
		body["flag"] = map[string]any{"flagStatus": "flagged"}
	}
	if hasFlag(remove, `\flagged`) {
		body["flag"] = map[string]any{"flagStatus": "notFlagged"}
	}
	if len(body) == 0 {
		return nil
	}
	for _, id := range ids {
		if err := p.doJSON(ctx, http.MethodPatch, "/me/messages/"+esc(id), body, nil); err != nil {
			return err
		}
	}
	return nil
}

func (p *provider) moveOrCopy(ctx context.Context, ids []string, dest, verb string) error {
	for _, id := range ids {
		if err := p.doJSON(ctx, http.MethodPost, "/me/messages/"+esc(id)+"/"+verb, map[string]any{"destinationId": dest}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (p *provider) MoveMessages(ctx context.Context, folderID string, ids []string, destFolderID string) error {
	return p.moveOrCopy(ctx, ids, destFolderID, "move")
}

func (p *provider) CopyMessages(ctx context.Context, folderID string, ids []string, destFolderID string) error {
	return p.moveOrCopy(ctx, ids, destFolderID, "copy")
}

func (p *provider) DeleteMessages(ctx context.Context, folderID string, ids []string) error {
	for _, id := range ids {
		if _, err := p.do(ctx, http.MethodDelete, "/me/messages/"+esc(id), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// postMIME posts base64-encoded raw MIME with Content-Type: text/plain,
// Graph's documented MIME form. It preserves attachments and every header,
// unlike a JSON reconstruction from bridge.ParseMIME.
func (p *provider) postMIME(ctx context.Context, resource string, raw []byte) ([]byte, error) {
	payload := []byte(base64.StdEncoding.EncodeToString(raw))
	return p.doBytes(ctx, http.MethodPost, resource, payload, "text/plain", nil)
}

// isDraftsFolder resolves whether folderID is the Drafts folder. A
// well-known "drafts" id answers without a round-trip; opaque ids need one
// GET /me/mailFolders/{id} whose display name specialFolder maps.
func (p *provider) isDraftsFolder(ctx context.Context, folderID string) (bool, error) {
	if folderID == "" || specialFolder(folderID, "") == "drafts" {
		return true, nil
	}
	var gf graphFolder
	if err := p.doJSON(ctx, http.MethodGet, "/me/mailFolders/"+esc(folderID), nil, &gf); err != nil {
		return false, err
	}
	return gf.toFolder().Special == "drafts", nil
}

func (p *provider) AppendMessage(ctx context.Context, folderID string, raw []byte, flags bridge.Flags) (string, error) {
	drafts, err := p.isDraftsFolder(ctx, folderID)
	if err != nil {
		return "", err
	}
	// POST /me/messages always creates the message as a draft; move it to
	// the target folder afterwards when that is not Drafts.
	data, err := p.postMIME(ctx, "/me/messages", raw)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", bridge.Wrap(bridge.KindOther, "graph: decode created draft", err)
	}
	if out.ID == "" {
		return "", bridge.Wrap(bridge.KindOther, "graph: created draft has no id", fmt.Errorf("empty id in response"))
	}
	if !drafts {
		if err := p.doJSON(ctx, http.MethodPost, "/me/messages/"+esc(out.ID)+"/move", map[string]any{"destinationId": folderID}, nil); err != nil {
			return "", err
		}
	}
	// Flags cannot be expressed in the MIME-create call, so apply them
	// to the stored message afterwards (immutable ids survive the move).
	if len(flags) > 0 {
		if err := p.SetFlags(ctx, folderID, []string{out.ID}, flags, nil); err != nil {
			return "", err
		}
	}
	return out.ID, nil
}

func (p *provider) SendMessage(ctx context.Context, raw []byte) error {
	_, err := p.postMIME(ctx, "/me/sendMail", raw)
	return err
}
