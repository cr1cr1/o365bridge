package jmapadapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// changeRecord is one ordered change within a version.
type changeRecord struct {
	Kind string `json:"kind"` // "c", "u", or "d"
	ID   string `json:"id"`
}

type journalHistory struct {
	Version   int      `json:"version"`
	Created   []string `json:"created"`
	Updated   []string `json:"updated"`
	Destroyed []string `json:"destroyed"`
	// Records preserves the ordered record list for this version
	// (created, then updated, then destroyed) so Changes can page
	// mid-version. Older files lack it; flatten synthesises it.
	Records []changeRecord `json:"records,omitempty"`
}

// flatten returns the ordered records of a history entry.
func (h journalHistory) flatten() []changeRecord {
	if h.Records != nil {
		return h.Records
	}
	out := make([]changeRecord, 0, len(h.Created)+len(h.Updated)+len(h.Destroyed))
	for _, id := range h.Created {
		out = append(out, changeRecord{"c", id})
	}
	for _, id := range h.Updated {
		out = append(out, changeRecord{"u", id})
	}
	for _, id := range h.Destroyed {
		out = append(out, changeRecord{"d", id})
	}
	return out
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
	records := make([]changeRecord, 0, len(created)+len(updated)+len(destroyed))
	for _, id := range created {
		records = append(records, changeRecord{"c", id})
	}
	for _, id := range updated {
		records = append(records, changeRecord{"u", id})
	}
	for _, id := range destroyed {
		records = append(records, changeRecord{"d", id})
	}
	a.data.History = append(a.data.History, journalHistory{Version: a.data.Version, Created: created, Updated: updated, Destroyed: destroyed, Records: records})
	if len(a.data.History) > 500 { // cap history at last 500 versions
		a.data.History = a.data.History[len(a.data.History)-500:]
	}
	if err := j.saveLocked(a); err != nil {
		j.logger.Warn().Str("account", account).Err(err).Msg("jmap journal save failed")
	}
	state := caughtUpState(a.data)
	publish := j.publish
	j.mu.Unlock()
	if publish != nil {
		publish(bridge.NormalizeEmail(account), state)
	}
	return true
}

// State format is "V:I": all change records in versions < V are
// consumed, and the first I records of version V are consumed.
// The caught-up (current) state is therefore
// "currentVersion:len(records[currentVersion])".
func caughtUpState(d journalFile) string {
	n := 0
	if len(d.History) > 0 && d.History[len(d.History)-1].Version == d.Version {
		n = len(d.History[len(d.History)-1].flatten())
	}
	return fmt.Sprintf("%d:%d", d.Version, n)
}

type cursor struct {
	version int
	idx     int  // first unconsumed record index within version
	plain   bool // parsed from a legacy "V" state (caught up at V)
	// firstNeeded is the first version whose records are still
	// unconsumed (may be Version+1 when fully caught up).
	firstNeeded int
}

// parseCursor resolves a state string. A legacy plain "V" (written by
// the pre-cursor code, meaning "caught up at version V") resolves to
// the first record after version V.
func parseCursor(state string, d journalFile) (cursor, error) {
	if v, i, ok := strings.Cut(state, ":"); ok {
		ver, err1 := strconv.Atoi(v)
		idx, err2 := strconv.Atoi(i)
		if err1 != nil || err2 != nil || ver < 0 || idx < 0 || ver > d.Version {
			return cursor{}, datatype.ErrCannotCalculate
		}
		recs, found := recordsOf(d, ver)
		if found && idx > len(recs) {
			return cursor{}, datatype.ErrCannotCalculate
		}
		firstNeeded := ver
		switch {
		case found && idx >= len(recs):
			firstNeeded = ver + 1
		case !found && ver == 0 && idx == 0:
			firstNeeded = 1
		}
		return cursor{version: ver, idx: idx, firstNeeded: firstNeeded}, nil
	}
	ver, err := strconv.Atoi(state)
	if err != nil || ver < 0 || ver > d.Version {
		return cursor{}, datatype.ErrCannotCalculate
	}
	return cursor{version: ver + 1, idx: 0, plain: true, firstNeeded: ver + 1}, nil
}

func recordsOf(d journalFile, version int) ([]changeRecord, bool) {
	for _, h := range d.History {
		if h.Version == version {
			return h.flatten(), true
		}
	}
	return nil, false
}

// State returns the current caught-up state ("version:recordCount").
func (j *Journal) State(account string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	a, err := j.loadLocked(account)
	if err != nil {
		return "0:0"
	}
	return caughtUpState(a.data)
}

// Changes lists changes since sinceState, or datatype.ErrCannotCalculate.
// It never advances newState past unreturned records: with maxChanges>0
// it returns the first maxChanges ordered records after the cursor
// (coalesced only within that page), sets hasMore, and points newState
// at the next unreturned record.
func (j *Journal) Changes(account, sinceState string, maxChanges int) (created, updated, destroyed []jmap.ID, newState string, hasMore bool, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	a, lerr := j.loadLocked(account)
	if lerr != nil {
		return nil, nil, nil, "", false, lerr
	}
	d := a.data
	cur, perr := parseCursor(sinceState, d)
	if perr != nil {
		return nil, nil, nil, "", false, perr
	}
	// Too-old check: the first still-unconsumed version must be retained
	// (or be beyond the current version, i.e. fully caught up).
	if len(d.History) > 0 {
		if oldest := d.History[0].Version; cur.firstNeeded < oldest {
			return nil, nil, nil, "", false, datatype.ErrCannotCalculate
		}
	} else if d.Version > 0 && cur.firstNeeded <= d.Version {
		return nil, nil, nil, "", false, datatype.ErrCannotCalculate
	}
	// Collect every ordered record after the cursor.
	type posRecord struct {
		rec     changeRecord
		version int
		idx     int
	}
	var all []posRecord
	for _, h := range d.History {
		if h.Version < cur.version {
			continue
		}
		recs := h.flatten()
		start := 0
		if h.Version == cur.version {
			start = cur.idx
			if start > len(recs) {
				start = len(recs)
			}
		}
		for i := start; i < len(recs); i++ {
			all = append(all, posRecord{recs[i], h.Version, i})
		}
	}
	page := all
	if maxChanges > 0 && len(page) > maxChanges {
		page = page[:maxChanges]
		hasMore = true
	}
	// Coalesce only within the returned page.
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
	for _, pr := range page {
		switch pr.rec.Kind {
		case "c":
			del(destroyedSet, &orderD, pr.rec.ID)
			add(createdSet, &orderC, pr.rec.ID)
		case "u":
			if !createdSet[pr.rec.ID] {
				add(updatedSet, &orderU, pr.rec.ID)
			}
		case "d":
			if createdSet[pr.rec.ID] {
				del(createdSet, &orderC, pr.rec.ID)
			} else {
				del(updatedSet, &orderU, pr.rec.ID)
				add(destroyedSet, &orderD, pr.rec.ID)
			}
		}
	}
	if hasMore {
		last := page[len(page)-1]
		newState = fmt.Sprintf("%d:%d", last.version, last.idx+1)
	} else {
		newState = caughtUpState(d)
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
