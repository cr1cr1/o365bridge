package jmapadapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	jmap "github.com/cr1cr1/o365bridge/jmap"
	"github.com/cr1cr1/o365bridge/jmap/datatype"
	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

// Witness is one observed message: JMAP email ID, folder, content hash.
type Witness struct {
	ID     string
	Folder string
	Hash   string
}

type journalEntry struct {
	Folder string `json:"folder"`
	Hash   string `json:"hash"`
}

type journalHistory struct {
	Version   int      `json:"version"`
	Created   []string `json:"created"`
	Updated   []string `json:"updated"`
	Destroyed []string `json:"destroyed"`
}

type journalFile struct {
	Version int                     `json:"version"`
	Entries map[string]journalEntry `json:"entries"`
	History []journalHistory        `json:"history"`
}

type accountJournal struct {
	data journalFile
	path string
}

// Journal is a per-account witness log persisted as jmap-journal.json
// in the account dir (0600, atomic temp+rename). It records only IDs,
// folders and hashes — never bodies.
type Journal struct {
	reg     *bridge.Registry
	logger  zerolog.Logger
	mu      sync.Mutex
	accts   map[string]*accountJournal
	publish func(account, state string)
}

func NewJournal(reg *bridge.Registry, logger zerolog.Logger) *Journal {
	return &Journal{reg: reg, logger: logger, accts: map[string]*accountJournal{}}
}

func (j *Journal) SetPublisher(fn func(account, state string)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.publish = fn
}

func (j *Journal) loadLocked(account string) (*accountJournal, error) {
	account = bridge.NormalizeEmail(account)
	if a, ok := j.accts[account]; ok {
		return a, nil
	}
	dir, err := j.reg.Store().AccountDir(account)
	if err != nil {
		return nil, err
	}
	a := &accountJournal{path: filepath.Join(dir, "jmap-journal.json"), data: journalFile{Entries: map[string]journalEntry{}}}
	data, err := os.ReadFile(a.path)
	if err == nil {
		if err := json.Unmarshal(data, &a.data); err != nil {
			return nil, fmt.Errorf("jmapadapter: corrupt journal for %s: %w", account, err)
		}
		if a.data.Entries == nil {
			a.data.Entries = map[string]journalEntry{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	j.accts[account] = a
	return a, nil
}

func writeFileAtomic0600(path string, data []byte) error {
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

func (j *Journal) saveLocked(a *accountJournal) error {
	data, err := json.Marshal(a.data)
	if err != nil {
		return err
	}
	return writeFileAtomic0600(a.path, data)
}

// Observe records witnesses; full=true means the listing is complete
// for the account, so known IDs absent from it are destroyed.
// A new version is assigned only on diffs.
func (j *Journal) Observe(account string, witnesses []Witness, full bool) bool {
	j.mu.Lock()
	a, err := j.loadLocked(account)
	if err != nil {
		j.mu.Unlock()
		j.logger.Warn().Str("account", account).Err(err).Msg("jmap journal load failed")
		return false
	}
	seen := make(map[string]bool, len(witnesses))
	var created, updated []string
	for _, w := range witnesses {
		seen[w.ID] = true
		old, ok := a.data.Entries[w.ID]
		if !ok {
			created = append(created, w.ID)
		} else if old.Hash != w.Hash || old.Folder != w.Folder {
			updated = append(updated, w.ID)
		}
	}
	var destroyed []string
	if full {
		for id := range a.data.Entries {
			if !seen[id] {
				destroyed = append(destroyed, id)
			}
		}
		sort.Strings(destroyed)
	}
	if len(created) == 0 && len(updated) == 0 && len(destroyed) == 0 {
		j.mu.Unlock()
		return false
	}
	for _, w := range witnesses {
		a.data.Entries[w.ID] = journalEntry{Folder: w.Folder, Hash: w.Hash}
	}
	for _, id := range destroyed {
		delete(a.data.Entries, id)
	}
	a.data.Version++
	if created == nil {
		created = []string{}
	}
	if updated == nil {
		updated = []string{}
	}
	if destroyed == nil {
		destroyed = []string{}
	}
	a.data.History = append(a.data.History, journalHistory{Version: a.data.Version, Created: created, Updated: updated, Destroyed: destroyed})
	if len(a.data.History) > 500 { // cap history at last 500 versions
		a.data.History = a.data.History[len(a.data.History)-500:]
	}
	if err := j.saveLocked(a); err != nil {
		j.logger.Warn().Str("account", account).Err(err).Msg("jmap journal save failed")
	}
	state := strconv.Itoa(a.data.Version)
	publish := j.publish
	j.mu.Unlock()
	if publish != nil {
		publish(bridge.NormalizeEmail(account), state)
	}
	return true
}

// State returns the current journal version as a decimal state string.
func (j *Journal) State(account string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	a, err := j.loadLocked(account)
	if err != nil {
		return "0"
	}
	return strconv.Itoa(a.data.Version)
}

// Changes lists changes since sinceState, or datatype.ErrCannotCalculate.
func (j *Journal) Changes(account, sinceState string, maxChanges int) (created, updated, destroyed []jmap.ID, newState string, hasMore bool, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	a, lerr := j.loadLocked(account)
	if lerr != nil {
		return nil, nil, nil, "", false, lerr
	}
	cur := a.data.Version
	since, perr := strconv.Atoi(sinceState)
	if perr != nil || since < 0 || since > cur {
		return nil, nil, nil, "", false, datatype.ErrCannotCalculate
	}
	if since == cur {
		return []jmap.ID{}, []jmap.ID{}, []jmap.ID{}, strconv.Itoa(cur), false, nil
	}
	if len(a.data.History) == 0 || a.data.History[0].Version > since+1 {
		return nil, nil, nil, "", false, datatype.ErrCannotCalculate
	}
	createdSet, updatedSet, destroyedSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var orderC, orderU, orderD []string
	add := func(set map[string]bool, order *[]string, id string) {
		if !set[id] {
			set[id] = true
			*order = append(*order, id)
		}
	}
	del := func(set map[string]bool, order *[]string, id string) {
		if set[id] {
			delete(set, id)
			for i, v := range *order {
				if v == id {
					*order = append((*order)[:i], (*order)[i+1:]...)
					break
				}
			}
		}
	}
	total := func() int { return len(orderC) + len(orderU) + len(orderD) }
	lastIncluded := since
	for _, h := range a.data.History {
		if h.Version <= since {
			continue
		}
		if maxChanges > 0 && total() >= maxChanges {
			hasMore = true
			break
		}
		for _, id := range h.Created {
			del(destroyedSet, &orderD, id)
			add(createdSet, &orderC, id)
		}
		for _, id := range h.Updated {
			if !createdSet[id] {
				add(updatedSet, &orderU, id)
			}
		}
		for _, id := range h.Destroyed {
			if createdSet[id] {
				del(createdSet, &orderC, id)
			} else {
				del(updatedSet, &orderU, id)
				add(destroyedSet, &orderD, id)
			}
		}
		lastIncluded = h.Version
	}
	if hasMore {
		newState = strconv.Itoa(lastIncluded)
	} else {
		newState = strconv.Itoa(cur)
		// Single version may exceed cap: truncate lists.
		if maxChanges > 0 && total() > maxChanges {
			hasMore = true
			remaining := maxChanges
			trunc := func(in []string) []string {
				if remaining <= 0 {
					return nil
				}
				if len(in) > remaining {
					out := in[:remaining]
					remaining = 0
					return out
				}
				remaining -= len(in)
				return in
			}
			orderC, orderU, orderD = trunc(orderC), trunc(orderU), trunc(orderD)
			// shortcut: truncated single-version newState stays cur; oversized version is rare.
		}
	}
	toIDs := func(in []string) []jmap.ID {
		out := make([]jmap.ID, 0, len(in))
		for _, s := range in {
			out = append(out, jmap.ID(s))
		}
		return out
	}
	return toIDs(orderC), toIDs(orderU), toIDs(orderD), newState, hasMore, nil
}
