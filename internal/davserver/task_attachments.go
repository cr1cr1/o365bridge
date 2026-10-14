package davserver

import (
	"context"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func taskWithAttachmentURIs(r *http.Request, email, collID string, tk bridge.Task) bridge.Task {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	origin := (&url.URL{Scheme: scheme, Host: r.Host}).String()
	tk.Attachments = slices.Clone(tk.Attachments)
	for i := range tk.Attachments {
		attachment := &tk.Attachments[i]
		attachment.ContentType = taskAttachmentMediaType(attachment.ContentType)
		name := attachment.Name
		if name == "" {
			name = "attachment"
		}
		attachment.URI = origin + calHome(email) + esc(collID) + "/_attachments/" + esc(tk.ID) + "/" + esc(attachment.ID) + "/" + esc(name)
	}
	return tk
}

func taskAttachmentMediaType(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.Contains(mediaType, "/") {
		return "application/octet-stream"
	}
	return mediaType
}

func taskAttachmentRefsMatch(r *http.Request, email, collID string, current bridge.Task, incoming []bridge.TaskAttachment) bool {
	if incoming == nil {
		return true // Clients that cannot represent attachments must not delete native files.
	}
	expected := taskWithAttachmentURIs(r, email, collID, current).Attachments
	if len(expected) != len(incoming) {
		return false
	}
	allowed := make(map[string]bool, len(expected))
	for _, attachment := range expected {
		if attachment.URI == "" || allowed[attachment.URI] {
			return false
		}
		allowed[attachment.URI] = true
	}
	for _, attachment := range incoming {
		if !allowed[attachment.URI] {
			return false
		}
		delete(allowed, attachment.URI)
	}
	return len(allowed) == 0
}

var errTaskAttachmentTooLarge = errors.New("task attachment exceeds size limit")

func (h *handler) handleTaskAttachment(w http.ResponseWriter, r *http.Request, email, collID string, segs []string) {
	if len(segs) != 7 || segs[3] != "_attachments" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var decoded [3]string
	for i := range decoded {
		value, err := url.PathUnescape(segs[i+4])
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if value == "" {
			http.NotFound(w, r)
			return
		}
		decoded[i] = value
	}
	taskID, attachmentID := decoded[0], decoded[1]
	coll, ok, err := h.getColl(r, email, collID)
	if err != nil {
		h.upstreamErr(w, email, err)
		return
	}
	if !ok || !coll.Tasks {
		http.NotFound(w, r)
		return
	}
	var attachment bridge.TaskAttachment
	var data []byte
	err = h.call(r.Context(), email, func(ctx context.Context, p bridge.Provider) error {
		attachment, data = bridge.TaskAttachment{}, nil
		tasks, err := p.ListTasks(ctx, collID)
		if err != nil {
			return err
		}
		matches := 0
		for _, task := range tasks {
			if task.ID != taskID {
				continue
			}
			for _, candidate := range task.Attachments {
				if candidate.ID == attachmentID {
					attachment = candidate
					matches++
				}
			}
		}
		if matches != 1 {
			return fs.ErrNotExist
		}
		if attachment.Size > bridge.MaxTaskAttachmentBytes {
			return errTaskAttachmentTooLarge
		}
		downloader, ok := p.(bridge.TaskAttachmentProvider)
		if !ok {
			return errors.ErrUnsupported
		}
		data, err = downloader.GetTaskAttachment(ctx, collID, taskID, attachmentID)
		if err != nil {
			return err
		}
		if int64(len(data)) > bridge.MaxTaskAttachmentBytes {
			data = nil
			return errTaskAttachmentTooLarge
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			http.NotFound(w, r)
		case errors.Is(err, errors.ErrUnsupported):
			http.Error(w, "task attachment download unsupported", http.StatusNotImplemented)
		case errors.Is(err, errTaskAttachmentTooLarge):
			http.Error(w, "task attachment too large", http.StatusRequestEntityTooLarge)
		default:
			h.upstreamErr(w, email, err)
		}
		return
	}
	name := attachment.Name
	if name == "" {
		name = "attachment"
	}
	w.Header().Set("Content-Type", taskAttachmentMediaType(attachment.ContentType))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}
