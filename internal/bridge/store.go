package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store is the interim session cache on disk. Layout is a design
// requirement: one self-contained, owner-only directory per account
// under <root>/accounts/<email>/, so hand-deleting that directory
// while the bridge is stopped resets exactly that account.
type Store struct {
	root string

	mu     sync.Mutex
	idmaps map[string]*IDMap
}

// NewStore opens (creating if needed) the store at root.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "accounts"), 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root, idmaps: map[string]*IDMap{}}, nil
}

// Root returns the store root directory.
func (s *Store) Root() string { return s.root }

// dirName maps an email to a safe single path element: the email is
// user-controlled (it arrives as a client username), so path
// separators and dot segments must never reach the filesystem.
func dirName(email string) string {
	name := NormalizeEmail(email)
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	if name == "." || name == ".." {
		name = "_"
	}
	return name
}

func (s *Store) accountDir(email string) string {
	return filepath.Join(s.root, "accounts", dirName(email))
}

// AccountDir returns the per-account directory, creating it with
// owner-only permissions.
func (s *Store) AccountDir(email string) (string, error) {
	dir := s.accountDir(email)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll honours umask; force the promised permission.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func writeFileAtomic(path string, data []byte) error {
	// Unique temp file: concurrent writers for the same account
	// must not rename each other's temp files away.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SaveAccount persists the account's session state.
func (s *Store) SaveAccount(a AccountState) error {
	a.Email = NormalizeEmail(a.Email)
	a.Session.Email = a.Email
	dir, err := s.AccountDir(a.Email)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "account.json"), data)
}

// LoadAccount reads persisted state, ErrUnknownAccount if absent.
func (s *Store) LoadAccount(email string) (AccountState, error) {
	data, err := os.ReadFile(filepath.Join(s.accountDir(email), "account.json"))
	if errors.Is(err, os.ErrNotExist) {
		return AccountState{}, ErrUnknownAccount
	}
	if err != nil {
		return AccountState{}, err
	}
	var a AccountState
	if err := json.Unmarshal(data, &a); err != nil {
		return AccountState{}, fmt.Errorf("bridge: corrupt account state for %s: %w", email, err)
	}
	return a, nil
}

// DeleteAccount removes exactly one account's whole state directory.
func (s *Store) DeleteAccount(email string) error {
	s.mu.Lock()
	delete(s.idmaps, NormalizeEmail(email))
	s.mu.Unlock()
	return os.RemoveAll(s.accountDir(email))
}

// IDMap persists folder-local IMAP UID assignments. Upstream message
// IDs are strings; IMAP needs stable uint32s per folder.
type IDMap struct {
	mu       sync.Mutex
	path     string
	Validity map[string]uint32            `json:"validity"`
	Next     map[string]uint32            `json:"next"`
	ByFolder map[string]map[string]uint32 `json:"by_folder"`    // folder -> upstream id -> uid
	Unsub    map[string]bool              `json:"unsubscribed"` // folder id -> hidden from LSUB
}

type idMapFile struct {
	Validity map[string]uint32            `json:"validity"`
	Next     map[string]uint32            `json:"next"`
	ByFolder map[string]map[string]uint32 `json:"by_folder"`
	Unsub    map[string]bool              `json:"unsubscribed"`
}

func (s *Store) loadIDMap(email string) (*IDMap, error) {
	dir, err := s.AccountDir(email)
	if err != nil {
		return nil, err
	}
	m := &IDMap{
		path:     filepath.Join(dir, "idmap.json"),
		Validity: map[string]uint32{},
		Next:     map[string]uint32{},
		ByFolder: map[string]map[string]uint32{},
		Unsub:    map[string]bool{},
	}
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var f idMapFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("bridge: corrupt idmap for %s: %w", email, err)
	}
	if f.Validity != nil {
		m.Validity = f.Validity
	}
	if f.Next != nil {
		m.Next = f.Next
	}
	if f.ByFolder != nil {
		m.ByFolder = f.ByFolder
	}
	if f.Unsub != nil {
		m.Unsub = f.Unsub
	}
	return m, nil
}

// IDMap returns the UID mapper for an account. Handles are cached
// per Store: every caller in this process shares ONE mapper, so
// concurrent connections can never allocate duplicate UIDs or
// overwrite each other's persisted mappings.
func (s *Store) IDMap(email string) (*IDMap, error) {
	email = NormalizeEmail(email)
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.idmaps[email]; ok {
		return m, nil
	}
	m, err := s.loadIDMap(email)
	if err != nil {
		return nil, err
	}
	s.idmaps[email] = m
	return m, nil
}

func (m *IDMap) saveLocked() error {
	data, err := json.Marshal(idMapFile{Validity: m.Validity, Next: m.Next, ByFolder: m.ByFolder, Unsub: m.Unsub})
	if err != nil {
		return err
	}
	return writeFileAtomic(m.path, data)
}

// UIDValidity returns the folder's stable UIDVALIDITY, allocating
// one on first sight.
func (m *IDMap) UIDValidity(folderID string) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.Validity[folderID]; ok {
		return v
	}
	v := uint32(len(m.Validity) + 1)
	m.Validity[folderID] = v
	_ = m.saveLocked()
	return v
}

// UID returns the folder-local UID for an upstream message id,
// allocating and persisting a new one on first sight.
func (m *IDMap) UID(folderID, upstreamID string) (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	folder, ok := m.ByFolder[folderID]
	if !ok {
		folder = map[string]uint32{}
		m.ByFolder[folderID] = folder
	}
	if uid, ok := folder[upstreamID]; ok {
		return uid, nil
	}
	if _, ok := m.Validity[folderID]; !ok {
		m.Validity[folderID] = uint32(len(m.Validity) + 1)
	}
	next := m.Next[folderID]
	if next == 0 {
		next = 1
	}
	m.Next[folderID] = next + 1
	folder[upstreamID] = next
	if err := m.saveLocked(); err != nil {
		return 0, err
	}
	return next, nil
}

// UpstreamID reverses UID within a folder.
func (m *IDMap) UpstreamID(folderID string, uid uint32) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, u := range m.ByFolder[folderID] {
		if u == uid {
			return id, true
		}
	}
	return "", false
}

// Forget drops a folder's mapping (folder deleted upstream).
func (m *IDMap) Forget(folderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.ByFolder, folderID)
	delete(m.Next, folderID)
	delete(m.Unsub, folderID)
	return m.saveLocked()
}

// Subscribed reports whether the folder shows in LSUB. Default: yes.
func (m *IDMap) Subscribed(folderID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.Unsub[folderID]
}

// SetSubscribed records and persists the folder's LSUB visibility.
func (m *IDMap) SetSubscribed(folderID string, subscribed bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if subscribed == !m.Unsub[folderID] {
		return nil // no change, skip the disk write
	}
	if subscribed {
		delete(m.Unsub, folderID)
	} else {
		m.Unsub[folderID] = true
	}
	return m.saveLocked()
}
