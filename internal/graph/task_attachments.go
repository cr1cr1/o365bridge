package graph

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type graphTaskAttachment struct {
	Type        string `json:"@odata.type"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

func (a graphTaskAttachment) isFile() bool {
	return strings.TrimPrefix(a.Type, "#") == "microsoft.graph.taskFileAttachment"
}

func taskAttachmentsPath(listID, taskID string) string {
	return "/me/todo/lists/" + esc(listID) + "/tasks/" + esc(taskID) + "/attachments"
}

func (p *provider) listTaskAttachments(ctx context.Context, listID, taskID string) ([]graphTaskAttachment, error) {
	if listID == "" || taskID == "" {
		return nil, bridge.Wrap(bridge.KindOther, "graph: task attachment requires list and task IDs", nil)
	}
	items, _, err := getAll[graphTaskAttachment](ctx, p, taskAttachmentsPath(listID, taskID), 0, nil)
	return items, err
}

// GetTaskAttachment reads only a verified native task file, never a linked URL.
func (p *provider) GetTaskAttachment(ctx context.Context, listID, taskID, attachmentID string) ([]byte, error) {
	if listID == "" || taskID == "" || attachmentID == "" {
		return nil, bridge.Wrap(bridge.KindOther, "graph: task attachment requires list, task and attachment IDs", nil)
	}
	attachments, err := p.listTaskAttachments(ctx, listID, taskID)
	if err != nil {
		return nil, taskAttachmentError(err)
	}
	var match *graphTaskAttachment
	for i := range attachments {
		if attachments[i].ID == attachmentID {
			if match != nil {
				return nil, bridge.Wrap(bridge.KindOther, "graph: ambiguous duplicate task attachment id", nil)
			}
			match = &attachments[i]
		}
	}
	if match == nil {
		return nil, bridge.Wrap(bridge.KindOther, "graph: task attachment not found in requested task", fs.ErrNotExist)
	}
	if !match.isFile() {
		return nil, bridge.Wrap(bridge.KindOther, "graph: unsupported task attachment type", errors.ErrUnsupported)
	}
	if match.Size < 0 || match.Size > bridge.MaxTaskAttachmentBytes {
		return nil, bridge.Wrap(bridge.KindOther, "graph: invalid or oversized task attachment", nil)
	}
	// The v1.0 taskFileAttachment GET API explicitly supports raw /$value.
	data, err := p.doBytesLimit(ctx, http.MethodGet, taskAttachmentsPath(listID, taskID)+"/"+esc(attachmentID)+"/$value", nil, "", nil, bridge.MaxTaskAttachmentBytes)
	return data, taskAttachmentError(err)
}

func taskAttachmentError(err error) error {
	var status *graphHTTPError
	if errors.As(err, &status) && status.statusCode == http.StatusNotFound {
		return bridge.Wrap(bridge.KindOther, "graph: task attachment no longer exists", errors.Join(fs.ErrNotExist, err))
	}
	return err
}
