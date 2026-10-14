package jmapadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/server"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

const (
	blobTTL        = 15 * time.Minute
	blobCapPerAcct = 32 << 20 // 32 MiB per account
)

type blobEntry struct {
	data    []byte
	ct      string
	expires time.Time
}

// blobStore implements server.BlobStore: ephemeral uploads in memory
// plus message/part blobs derived live from upstream MIME.
// shortcut: ephemeral blob IDs do not survive a bridge restart.
type blobStore struct {
	b      *Backend
	mu     sync.Mutex
	byAcct map[string]map[string]*blobEntry
	totals map[string]int
}

func newBlobStore(b *Backend) *blobStore {
	return &blobStore{b: b, byAcct: map[string]map[string]*blobEntry{}, totals: map[string]int{}}
}

func (s *blobStore) cleanupLocked(account string, now time.Time) {
	m := s.byAcct[account]
	for id, e := range m {
		if now.After(e.expires) {
			s.totals[account] -= len(e.data)
			delete(m, id)
		}
	}
}

func (s *blobStore) Upload(_ context.Context, accountID jmap.ID, contentType string, data []byte) (jmap.ID, error) {
	account := string(accountID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byAcct[account] == nil {
		s.byAcct[account] = map[string]*blobEntry{}
	}
	s.cleanupLocked(account, time.Now())
	if s.totals[account]+len(data) > blobCapPerAcct {
		return "", fmt.Errorf("jmapadapter: ephemeral blob cap exceeded for account")
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	id := jmap.ID("u." + hex.EncodeToString(rnd[:]))
	cp := append([]byte(nil), data...)
	s.byAcct[account][string(id)] = &blobEntry{data: cp, ct: contentType, expires: time.Now().Add(blobTTL)}
	s.totals[account] += len(data)
	return id, nil
}

func (s *blobStore) Download(ctx context.Context, accountID, blobID jmap.ID) (string, []byte, error) {
	account := string(accountID)
	id := string(blobID)
	if strings.HasPrefix(id, "u.") {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.cleanupLocked(account, time.Now())
		e, ok := s.byAcct[account][id]
		if !ok {
			return "", nil, server.ErrNotFound
		}
		return e.ct, append([]byte(nil), e.data...), nil
	}
	if strings.HasPrefix(id, "p.") {
		rest := strings.TrimPrefix(id, "p.")
		idx := strings.LastIndex(rest, ".")
		if idx < 0 {
			return "", nil, server.ErrNotFound
		}
		emailID, partID := jmap.ID(rest[:idx]), rest[idx+1:]
		folderID, upstreamID, err := decodeEmailID(emailID)
		if err != nil {
			return "", nil, server.ErrNotFound
		}
		raw, err := s.fetchMIME(ctx, account, folderID, upstreamID)
		if err != nil {
			return "", nil, err
		}
		info, err := parseMIME(raw, emailID)
		if err != nil {
			return "", nil, server.ErrNotFound
		}
		part, ok := info.Parts[partID]
		if !ok {
			return "", nil, server.ErrNotFound
		}
		ct := part.Type
		if ct == "" {
			ct = "application/octet-stream"
		}
		return ct, part.Data, nil
	}
	// Message blob: blobID == email ID
	folderID, upstreamID, err := decodeEmailID(blobID)
	if err != nil {
		return "", nil, server.ErrNotFound
	}
	raw, err := s.fetchMIME(ctx, account, folderID, upstreamID)
	if err != nil {
		return "", nil, err
	}
	return "message/rfc822", raw, nil
}

func (s *blobStore) fetchMIME(ctx context.Context, account, folderID, upstreamID string) ([]byte, error) {
	var raw []byte
	err := s.b.call(ctx, account, "GetMessageMIME", func(ctx context.Context, p bridge.Provider) error {
		var err error
		raw, err = p.GetMessageMIME(ctx, folderID, upstreamID)
		return err
	})
	if err != nil {
		return nil, server.ErrNotFound
	}
	return raw, nil
}

func (s *blobStore) Copy(ctx context.Context, fromAccount, toAccount, blobID jmap.ID) (jmap.ID, error) {
	ct, data, err := s.Download(ctx, fromAccount, blobID)
	if err != nil {
		return "", err
	}
	return s.Upload(ctx, toAccount, ct, data)
}
