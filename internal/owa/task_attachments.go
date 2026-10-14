package owa

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

var _ bridge.TaskAttachmentProvider = (*provider)(nil)

const maxTaskAttachmentResponseBytes = (bridge.MaxTaskAttachmentBytes+2)/3*4 + (1 << 20)

func taskAttachmentsFromItem(item map[string]any) []bridge.TaskAttachment {
	var attachments []bridge.TaskAttachment
	for _, attachment := range arr(item, "Attachments") {
		id := str(attachment, "AttachmentId", "Id")
		if str(attachment, "__type") != "FileAttachment:#Exchange" || id == "" {
			continue
		}
		attachments = append(attachments, bridge.TaskAttachment{
			ID: id, Name: str(attachment, "Name"), ContentType: str(attachment, "ContentType"),
			Size: int64(num(attachment, "Size")),
		})
	}
	return attachments
}

// GetTaskAttachment verifies the task/list binding before using a mailbox-wide attachment token.
func (p *provider) GetTaskAttachment(ctx context.Context, listID, taskID, attachmentID string) ([]byte, error) {
	if listID == "" || taskID == "" || attachmentID == "" {
		return nil, fmt.Errorf("owa: task attachment: %w", fs.ErrNotExist)
	}
	tasks, err := p.ListTasks(ctx, listID)
	if err != nil {
		return nil, err
	}
	matches := 0
	for _, task := range tasks {
		if task.ID != taskID {
			continue
		}
		for _, attachment := range task.Attachments {
			if attachment.ID == attachmentID {
				if attachment.Size > bridge.MaxTaskAttachmentBytes {
					return nil, fmt.Errorf("owa: task attachment exceeds size limit")
				}
				matches++
			}
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("owa: task attachment: %w", fs.ErrNotExist)
	}
	request := envelope("GetAttachmentJsonRequest:#Exchange", "GetAttachmentRequest:#Exchange", map[string]any{
		"AttachmentIds": []any{map[string]any{"__type": "AttachmentId:#Exchange", "Id": attachmentID}},
		"AttachmentShape": map[string]any{
			"__type": "AttachmentResponseShapeType:#Exchange", "IncludeMimeContent": false,
		},
	})
	result, err := p.call(ctx, "GetAttachment", request)
	if err != nil {
		return nil, err
	}
	attachments := arr(result, "Attachments")
	if len(attachments) != 1 || str(attachments[0], "AttachmentId", "Id") != attachmentID {
		return nil, fmt.Errorf("owa: task attachment response: %w", fs.ErrNotExist)
	}
	if str(attachments[0], "__type") != "FileAttachment:#Exchange" {
		return nil, fmt.Errorf("owa: task attachment is not a file: %w", errors.ErrUnsupported)
	}
	content, ok := dig(attachments[0], "Content").(string)
	if !ok {
		return nil, fmt.Errorf("owa: task attachment response missing file content")
	}
	return decodeTaskAttachmentContent(content)
}

func decodeTaskAttachmentContent(content string) ([]byte, error) {
	decodedLength := base64.StdEncoding.DecodedLen(len(content))
	if strings.HasSuffix(content, "==") {
		decodedLength -= 2
	} else if strings.HasSuffix(content, "=") {
		decodedLength--
	}
	if int64(decodedLength) > bridge.MaxTaskAttachmentBytes {
		return nil, fmt.Errorf("owa: task attachment exceeds size limit")
	}
	data, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return nil, fmt.Errorf("owa: task attachment: decode content: %w", err)
	}
	if int64(len(data)) > bridge.MaxTaskAttachmentBytes {
		return nil, fmt.Errorf("owa: task attachment exceeds size limit")
	}
	return data, nil
}
