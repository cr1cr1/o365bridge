package jmapadapter

import (
	"context"
	"sort"
	"strings"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	jmail "github.com/cr1cr1/o365bridge/jmap/mail"
	"github.com/cr1cr1/o365bridge/jmap/server"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// This file implements a custom Email/query handler that works from
// message metadata (bridge.MessageMeta) only, instead of the generic
// datatype /query handler, which calls Email/get with ids == nil and
// therefore builds every Email in full — including a GetMessageMIME
// fetch and MIME parse per message. Here ListFolders/ListMessages
// metadata is converted to light Email values (see lightEmail) that
// carry exactly the fields Email filters and sorts can inspect, so
// no MIME is ever fetched on the query path.
//
// Note on Email/queryChanges: it is deliberately NOT overridden.
// The generic datatype queryChanges handler never calls Store.Get
// when the store does not implement QueryChangesStore (the email
// store does not); it returns a cannotCalculateChanges method error
// directly, so it has no fetch-all cost to fix.

// lightEmail converts message metadata to the light Email used for
// filtering/sorting on the query path.
//
// shortcut: Preview is "" and BodyValues is nil (no MIME is fetched),
// so the "text"/"body" filter conditions — which in the mail package
// search subject, preview and addresses — effectively search only
// subject and addresses here. From is a single EmailAddress carrying
// meta.From verbatim (it may be a display-name form such as
// "Alice <alice@example.com>", not a bare address) and To is one
// EmailAddress per entry of meta.To, likewise unparsed; address
// search is a case-insensitive substring match, so this remains
// usable for query purposes.
func lightEmail(meta bridge.MessageMeta) jmail.Email {
	var from []jmail.EmailAddress
	if meta.From != "" {
		from = []jmail.EmailAddress{{Email: meta.From}}
	}
	var to []jmail.EmailAddress
	for _, s := range meta.To {
		to = append(to, jmail.EmailAddress{Email: s})
	}
	var size uint
	if meta.Size > 0 {
		size = uint(meta.Size)
	}
	id := encodeEmailID(meta.FolderID, meta.ID)
	return jmail.Email{
		ID:            id,
		ThreadID:      "",
		MailboxIDs:    map[jmap.ID]bool{jmap.ID(meta.FolderID): true},
		Keywords:      flagsToKeywords(meta.Flags),
		Size:          size,
		ReceivedAt:    meta.Date,
		HasAttachment: meta.Attachments,
		Subject:       meta.Subject,
		From:          from,
		To:            to,
		Preview:       "",
		BodyValues:    nil,
	}
}

// emailQueryResponse mirrors the datatype /query response shape
// (jmap.QueryResponse, except that total is present as JSON null
// when not calculated rather than omitted).
type emailQueryResponse struct {
	AccountID           jmap.ID   `json:"accountId"`
	QueryState          string    `json:"queryState"`
	CanCalculateChanges bool      `json:"canCalculateChanges"`
	Position            uint64    `json:"position"`
	Total               *uint64   `json:"total"` // null unless calculateTotal
	IDs                 []jmap.ID `json:"ids"`
}

// emailQuery is the custom Email/query handler (RFC 8620 §5.5,
// RFC 8621 §5), registered after mail.Register to replace the
// generic datatype handler. Its protocol mechanics (filter, sort,
// anchor, position, limit, calculateTotal, and the standard method
// errors) mirror datatype's /query handler; only the record source
// differs (light metadata instead of full Email/get records).
func (b *Backend) emailQuery(ctx context.Context, call *server.Call) (any, error) {
	account, err := call.AccountID()
	if err != nil {
		return nil, err
	}
	var args jmap.QueryArgs
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}

	// Fetch light emails from per-folder metadata listings. Unlike
	// the Email/get all-ids path (which skips unreadable messages),
	// a failed folder listing fails the whole call: silently
	// returning a partial result set would be wrong for /query.
	var folders []bridge.Folder
	if err := b.call(ctx, string(account), "ListFolders", func(ctx context.Context, p bridge.Provider) error {
		var err error
		folders, err = p.ListFolders(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	var all []jmail.Email
	var witnesses []Witness
	for _, f := range folders {
		offset := 0
		for {
			var metas []bridge.MessageMeta
			var total int
			if err := b.call(ctx, string(account), "ListMessages", func(ctx context.Context, p bridge.Provider) error {
				var err error
				metas, total, err = p.ListMessages(ctx, f.ID, offset, 500)
				return err
			}); err != nil {
				return nil, err
			}
			for _, meta := range metas {
				if meta.FolderID == "" {
					meta.FolderID = f.ID
				}
				em := lightEmail(meta)
				all = append(all, em)
				// The witness hash is emailHash(meta) — the exact
				// hash the full-build path (email.go buildOne)
				// computes from the same MessageMeta — so journal
				// entries written here match those written by
				// Email/get for the same message.
				witnesses = append(witnesses, Witness{ID: string(em.ID), Folder: meta.FolderID, Hash: emailHash(meta)})
			}
			offset += len(metas)
			if len(metas) == 0 || offset >= total {
				break
			}
		}
	}
	// The listing is complete (any folder error returned above), so
	// observe once with full=true: known IDs absent from the listing
	// are recorded as destroyed.
	b.Journal.Observe(string(account), witnesses, true)
	queryState := b.Journal.State(string(account))

	filtered := make([]jmail.Email, 0, len(all))
	filterEmpty := len(args.Filter) == 0 || strings.TrimSpace(string(args.Filter)) == "null"
	for _, obj := range all {
		match := true
		if !filterEmpty {
			m, err := jmail.EmailMatch(obj, args.Filter)
			if err != nil {
				return nil, jmap.NewInvalidArguments(err.Error())
			}
			match = m
		}
		if match {
			filtered = append(filtered, obj)
		}
	}

	if len(args.Sort) > 0 {
		// Probe support up front: SliceStable may never call the
		// comparator for 0/1-element results, which would silently
		// accept an unknown sort property.
		if len(filtered) > 0 {
			for _, c := range args.Sort {
				if _, ok := jmail.EmailCompare(filtered[0], filtered[0], c); !ok {
					return nil, jmap.NewUnsupportedSort("unsupported sort property: " + c.Property)
				}
			}
		}
		unsupported := ""
		sort.SliceStable(filtered, func(i, j int) bool {
			for _, c := range args.Sort {
				cmp, ok := jmail.EmailCompare(filtered[i], filtered[j], c)
				if !ok {
					unsupported = c.Property
					return false
				}
				if cmp != 0 {
					if c.IsAscending != nil && !*c.IsAscending {
						cmp = -cmp
					}
					return cmp < 0
				}
			}
			return false
		})
		if unsupported != "" {
			return nil, jmap.NewUnsupportedSort("unsupported sort property: " + unsupported)
		}
	}

	total := int64(len(filtered))
	var start int64
	if args.Anchor != nil {
		idx := int64(-1)
		for i, obj := range filtered {
			if obj.ID == *args.Anchor {
				idx = int64(i)
				break
			}
		}
		if idx < 0 {
			return nil, jmap.NewAnchorNotFound("anchor not in results: " + string(*args.Anchor))
		}
		start = idx + args.AnchorOffset
	} else {
		start = args.Position
		if start < 0 {
			start = total + start // negative position counts from the end (§5.5)
		}
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}

	// args.Limit is *uint64, so unset (null) is distinguishable from
	// 0: null means no limit, 0 means return no ids (§5.5).
	end := total
	if args.Limit != nil && start+int64(*args.Limit) < end {
		end = start + int64(*args.Limit)
	}
	ids := make([]jmap.ID, 0, end-start)
	for _, obj := range filtered[start:end] {
		ids = append(ids, obj.ID)
	}

	resp := emailQueryResponse{
		AccountID:           account,
		QueryState:          queryState,
		CanCalculateChanges: true,
		Position:            uint64(start),
		IDs:                 ids,
	}
	if args.CalculateTotal {
		t := uint64(total)
		resp.Total = &t
	}
	return resp, nil
}
