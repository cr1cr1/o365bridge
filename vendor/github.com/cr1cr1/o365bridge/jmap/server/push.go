package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

// --- internal EventSource bus, fed by Publish ---

func (e *Engine) subscribeBus(accounts map[jmap.ID]bool) (<-chan StateChange, func()) {
	ch := make(chan StateChange, 16)
	e.busMu.Lock()
	e.bus[ch] = accounts
	e.busMu.Unlock()
	return ch, func() {
		e.busMu.Lock()
		delete(e.bus, ch)
		e.busMu.Unlock()
	}
}

func (e *Engine) publishBus(sc StateChange) {
	changed := sc.changed()
	e.busMu.Lock()
	defer e.busMu.Unlock()
	for ch, accounts := range e.bus {
		if accounts != nil {
			match := false
			for acct := range changed {
				if accounts[jmap.ID(acct)] {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		select {
		case ch <- sc:
		default: // slow subscriber: drop rather than block Publish
		}
	}
}

// Publish feeds the internal EventSource bus and, when a
// SubscriptionStore is configured, delivers the StateChange to every
// stored PushSubscription of the affected account(s) (RFC 8620 §7.1):
// one POST per subscription endpoint, retried twice with small backoff,
// then reported to Options.OnPushError if set.
func (e *Engine) Publish(sc StateChange) {
	e.publishBus(sc)
	if e.opts.Subscriptions == nil {
		return
	}
	changed := sc.changed()
	accounts := make([]jmap.ID, 0, len(changed))
	for acct := range changed {
		accounts = append(accounts, jmap.ID(acct))
	}
	if len(accounts) == 0 && sc.AccountID != "" {
		accounts = []jmap.ID{sc.AccountID}
	}
	ctx := context.Background()
	for _, acct := range accounts {
		subs, err := e.opts.Subscriptions.List(ctx, acct)
		if err != nil {
			e.pushError(nil, err)
			continue
		}
		for _, sub := range subs {
			if sub == nil || sub.URL == "" {
				continue
			}
			if sub.Expires != nil && time.Now().After(*sub.Expires) {
				continue // expired subscription: not delivered to
			}
			if err := e.postWithRetry(ctx, sub.URL, sc); err != nil {
				e.pushError(sub, err)
			}
		}
	}
}

func (e *Engine) pushError(sub *PushSubscription, err error) {
	if e.opts.OnPushError != nil {
		e.opts.OnPushError(sub, err)
	}
}

func (e *Engine) postWithRetry(ctx context.Context, url string, payload any) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ { // initial try + 2 retries
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 50 * time.Millisecond):
			}
		}
		if err = e.postJSON(ctx, url, payload); err == nil {
			return nil
		}
	}
	return err
}

func (e *Engine) postJSON(ctx context.Context, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push POST %s: %s", url, resp.Status)
	}
	return nil
}

// --- EventSource (RFC 8620 §7.3) ---

func (e *Engine) serveEventSource(w http.ResponseWriter, r *http.Request) {
	p := e.authenticate(w, r)
	if p == nil {
		return
	}
	// Note: EventSource is served from the internal Publish bus even
	// when the backend provides no StateChangeSource, so a backend that
	// only calls Publish still gets working EventSource push. When a
	// backend source is present, both are merged.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	accounts := make(map[jmap.ID]bool, len(p.Accounts))
	for id := range p.Accounts {
		accounts[id] = true
	}
	busCh, unsub := e.subscribeBus(accounts)
	defer unsub()

	merged := make(chan StateChange, 16)
	if e.opts.StateChanges != nil {
		for acct := range accounts {
			ch, err := e.opts.StateChanges.Subscribe(ctx, acct)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			go func(ch <-chan StateChange) {
				for {
					select {
					case <-ctx.Done():
						return
					case sc, ok := <-ch:
						if !ok {
							return
						}
						select {
						case merged <- sc:
						case <-ctx.Done():
							return
						}
					}
				}
			}(ch)
		}
	}

	q := r.URL.Query()
	ping := 30 * time.Second
	if v := q.Get("ping"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			ping = time.Duration(n) * time.Second
		}
	}
	closeAfterState := q.Get("closeafter") == "state"
	var typeFilter map[string]bool
	if v := q.Get("types"); v != "" && v != "*" {
		typeFilter = map[string]bool{}
		for _, t := range strings.Split(v, ",") {
			typeFilter[strings.TrimSpace(t)] = true
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": ok\n\n")
	flusher.Flush()

	var ticker *time.Ticker
	var tick <-chan time.Time
	if ping > 0 {
		ticker = time.NewTicker(ping)
		tick = ticker.C
		defer ticker.Stop()
	}

	send := func(sc StateChange) bool {
		out := filterStateChange(sc, typeFilter)
		if out == nil {
			return false
		}
		data, err := json.Marshal(out)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
			return true // client gone; stop
		}
		flusher.Flush()
		return closeAfterState
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case sc := <-busCh:
			if send(sc) {
				return
			}
		case sc := <-merged:
			if send(sc) {
				return
			}
		}
	}
}

// filterStateChange returns nil if no changed type passes the filter.
func filterStateChange(sc StateChange, typeFilter map[string]bool) *StateChange {
	if typeFilter == nil {
		return &sc
	}
	changed := map[string]map[string]string{}
	for acct, types := range sc.changed() {
		keep := map[string]string{}
		for t, s := range types {
			if typeFilter[t] {
				keep[t] = s
			}
		}
		if len(keep) > 0 {
			changed[acct] = keep
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return &StateChange{AccountID: sc.AccountID, Changed: changed}
}

// --- PushSubscription/get|set (RFC 8620 §7.2) ---

func randomID() jmap.ID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return jmap.ID(hex.EncodeToString(b[:]))
}

func (e *Engine) pushSubscriptionGet(ctx context.Context, call *Call) (any, error) {
	accountID, err := call.OptionalAccountID()
	if err != nil {
		return nil, err
	}
	var args struct {
		IDs []jmap.ID `json:"ids"`
	}
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	subs, err := e.opts.Subscriptions.List(ctx, accountID)
	if err != nil {
		return nil, jmap.NewServerFail(err.Error())
	}
	list := []*PushSubscription{}
	notFound := []jmap.ID{}
	if args.IDs == nil {
		list = append(list, subs...)
	} else {
		byID := map[jmap.ID]*PushSubscription{}
		for _, s := range subs {
			byID[s.ID] = s
		}
		for _, id := range args.IDs {
			if s, ok := byID[id]; ok {
				list = append(list, s)
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return struct {
		AccountID jmap.ID             `json:"accountId"`
		State     string              `json:"state"`
		List      []*PushSubscription `json:"list"`
		NotFound  []jmap.ID           `json:"notFound"`
	}{accountID, subState(), list, notFound}, nil
}

// shortcut: the subscription state string is the constant "0" — the
// SubscriptionStore contract carries no state, so ifInState checks and
// client change detection for subscriptions are nominal until a later
// phase extends the store.
func subState() string { return "0" }

type pushSetResponse struct {
	AccountID    jmap.ID                       `json:"accountId"`
	OldState     *string                       `json:"oldState"`
	NewState     string                        `json:"newState"`
	Created      map[jmap.ID]*PushSubscription `json:"created"`
	Updated      map[jmap.ID]*PushSubscription `json:"updated"`
	Destroyed    []jmap.ID                     `json:"destroyed"`
	NotCreated   map[jmap.ID]*jmap.SetError    `json:"notCreated"`
	NotUpdated   map[jmap.ID]*jmap.SetError    `json:"notUpdated"`
	NotDestroyed map[jmap.ID]*jmap.SetError    `json:"notDestroyed"`
}

func (e *Engine) pushSubscriptionSet(ctx context.Context, call *Call) (any, error) {
	accountID, err := call.OptionalAccountID()
	if err != nil {
		return nil, err
	}
	var args struct {
		IfInState *string                     `json:"ifInState"`
		Create    map[jmap.ID]json.RawMessage `json:"create"`
		Update    map[jmap.ID]json.RawMessage `json:"update"`
		Destroy   []jmap.ID                   `json:"destroy"`
	}
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	if args.IfInState != nil && *args.IfInState != subState() {
		return nil, jmap.NewStateMismatch("subscription state is " + subState())
	}
	st := subState()
	resp := &pushSetResponse{
		AccountID: accountID, OldState: &st, NewState: subState(),
		Created: map[jmap.ID]*PushSubscription{}, Updated: map[jmap.ID]*PushSubscription{},
		Destroyed: []jmap.ID{}, NotCreated: map[jmap.ID]*jmap.SetError{},
		NotUpdated: map[jmap.ID]*jmap.SetError{}, NotDestroyed: map[jmap.ID]*jmap.SetError{},
	}

	existing, _ := e.opts.Subscriptions.List(ctx, accountID)
	byID := map[jmap.ID]*PushSubscription{}
	for _, s := range existing {
		byID[s.ID] = s
	}

	for cid, raw := range args.Create {
		var sub PushSubscription
		if err := json.Unmarshal(raw, &sub); err != nil {
			resp.NotCreated[cid] = &jmap.SetError{Type: "invalidProperties", Description: err.Error()}
			continue
		}
		var missing []string
		if sub.URL == "" {
			missing = append(missing, "url")
		}
		if sub.DeviceClientID == "" {
			missing = append(missing, "deviceClientId")
		}
		if len(missing) > 0 {
			resp.NotCreated[cid] = &jmap.SetError{Type: "invalidProperties", Properties: missing}
			continue
		}
		sub.ID = randomID()
		// RFC 8620 §7.2 verification: POST a PushVerification with a
		// fresh code to the subscription URL.
		// shortcut: the code is not retained and the subscription is
		// stored even if the verification POST fails — full
		// verification state tracking arrives with later phases.
		code := string(randomID())
		_ = e.postJSON(ctx, sub.URL, map[string]any{
			"@type":              "PushVerification",
			"pushSubscriptionId": string(sub.ID),
			"verificationCode":   code,
		})
		stored := sub
		if err := e.opts.Subscriptions.Upsert(ctx, accountID, &stored); err != nil {
			resp.NotCreated[cid] = &jmap.SetError{Type: "serverFail", Description: err.Error()}
			continue
		}
		created := stored
		resp.Created[cid] = &created
		call.SetCreated(cid, stored.ID)
	}

	for id, raw := range args.Update {
		cur, ok := byID[id]
		if !ok {
			resp.NotUpdated[id] = &jmap.SetError{Type: "notFound"}
			continue
		}
		var patch PushSubscription
		if err := json.Unmarshal(raw, &patch); err != nil {
			resp.NotUpdated[id] = &jmap.SetError{Type: "invalidProperties", Description: err.Error()}
			continue
		}
		upd := *cur
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if _, ok := fields["url"]; ok {
			upd.URL = patch.URL
		}
		if _, ok := fields["deviceClientId"]; ok {
			upd.DeviceClientID = patch.DeviceClientID
		}
		if _, ok := fields["keys"]; ok {
			upd.Keys = patch.Keys
		}
		if _, ok := fields["expires"]; ok {
			upd.Expires = patch.Expires
		}
		if err := e.opts.Subscriptions.Upsert(ctx, accountID, &upd); err != nil {
			resp.NotUpdated[id] = &jmap.SetError{Type: "serverFail", Description: err.Error()}
			continue
		}
		resp.Updated[id] = nil
	}

	for _, id := range args.Destroy {
		if _, ok := byID[id]; !ok {
			resp.NotDestroyed[id] = &jmap.SetError{Type: "notFound"}
			continue
		}
		if err := e.opts.Subscriptions.Delete(ctx, accountID, id); err != nil {
			resp.NotDestroyed[id] = &jmap.SetError{Type: "serverFail", Description: err.Error()}
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	return resp, nil
}
