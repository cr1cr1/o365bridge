package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	jmap "github.com/cr1cr1/o365bridge/jmap"
)

func (e *Engine) checkAccount(p *Principal, id jmap.ID) bool {
	_, ok := p.Accounts[id]
	return ok
}

// serveUpload implements RFC 8620 §6.1.
func (e *Engine) serveUpload(w http.ResponseWriter, r *http.Request) {
	p := e.authenticate(w, r)
	if p == nil {
		return
	}
	if e.opts.Blobs == nil {
		http.Error(w, "blob store not supported", http.StatusNotImplemented)
		return
	}
	accountID := jmap.ID(r.PathValue("accountId"))
	if !e.checkAccount(p, accountID) {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(e.core.MaxSizeUpload)))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeProblem(w, &jmap.Problem{Type: jmap.ProblemLimit, Status: http.StatusRequestEntityTooLarge, Limit: "maxSizeUpload"})
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	blobID, err := e.opts.Blobs.Upload(r.Context(), accountID, ct, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"accountId": accountID,
		"blobId":    blobID,
		"type":      ct,
		"size":      uint64(len(data)),
	})
}

// serveDownload implements RFC 8620 §6.2.
func (e *Engine) serveDownload(w http.ResponseWriter, r *http.Request) {
	p := e.authenticate(w, r)
	if p == nil {
		return
	}
	if e.opts.Blobs == nil {
		http.Error(w, "blob store not supported", http.StatusNotImplemented)
		return
	}
	accountID := jmap.ID(r.PathValue("accountId"))
	blobID := jmap.ID(r.PathValue("blobId"))
	if !e.checkAccount(p, accountID) {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	ct, data, err := e.opts.Blobs.Download(r.Context(), accountID, blobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "blob not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	name := r.PathValue("name")
	w.Header().Set("Content-Type", ct)
	if name != "" {
		w.Header().Set("Content-Disposition", "attachment; filename="+strconvQuote(name))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// blobCopy implements Blob/copy (RFC 8620 §6.3).
func (e *Engine) blobCopy(ctx context.Context, call *Call) (any, error) {
	var args struct {
		FromAccountID jmap.ID   `json:"fromAccountId"`
		ToAccountID   jmap.ID   `json:"toAccountId"`
		BlobIDs       []jmap.ID `json:"blobIds"`
	}
	if err := call.Decode(&args); err != nil {
		return nil, jmap.NewInvalidArguments(err.Error())
	}
	for _, id := range []jmap.ID{args.FromAccountID, args.ToAccountID} {
		if _, ok := call.Principal.Accounts[id]; !ok {
			return nil, jmap.NewAccountNotFound(string(id))
		}
	}
	resp := struct {
		FromAccountID jmap.ID                    `json:"fromAccountId"`
		ToAccountID   jmap.ID                    `json:"toAccountId"`
		Copied        map[jmap.ID]jmap.ID        `json:"copied"`
		NotCopied     map[jmap.ID]*jmap.SetError `json:"notCopied"`
	}{args.FromAccountID, args.ToAccountID, map[jmap.ID]jmap.ID{}, map[jmap.ID]*jmap.SetError{}}
	for _, blobID := range args.BlobIDs {
		newID, err := e.opts.Blobs.Copy(ctx, args.FromAccountID, args.ToAccountID, blobID)
		if err != nil {
			typ := "serverFail"
			if errors.Is(err, ErrNotFound) {
				typ = "blobNotFound"
			}
			resp.NotCopied[blobID] = &jmap.SetError{Type: typ, Description: err.Error()}
			continue
		}
		resp.Copied[blobID] = newID
	}
	return resp, nil
}
