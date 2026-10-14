package graph

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type graphTaskExtension struct {
	ExtensionName string `json:"extensionName"`
	UID           string `json:"uid"`
	StartDateOnly *bool  `json:"startDateOnly"`
	DueDateOnly   *bool  `json:"dueDateOnly"`
}

func graphTaskDateOnly(value string, stored *bool) bool {
	if stored != nil {
		return *stored
	}
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

// taskDateMetadataChange plans the separate extension write before mutating a task.
func (p *provider) taskDateMetadataChange(ctx context.Context, t bridge.Task) (string, map[string]any, error) {
	if !t.FullUpdate && t.Start == nil && t.Due == nil && !t.StartDateOnly && !t.DueDateOnly {
		return "", nil, nil
	}
	query := url.Values{"$expand": {"extensions($filter=id eq '" + taskUIDExtensionName + "')"}}
	var current graphTask
	path := "/me/todo/lists/" + esc(t.ListID) + "/tasks/" + esc(t.ID)
	if err := p.doJSON(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &current); err != nil {
		return "", nil, err
	}
	if current.ID == "" || current.ID != t.ID {
		return "", nil, bridge.Wrap(bridge.KindOther, "graph: task date metadata readback missing or mismatched id", nil)
	}
	var stored *graphTaskExtension
	for i := range current.Extensions {
		if current.Extensions[i].ExtensionName == taskUIDExtensionName {
			stored = &current.Extensions[i]
			break
		}
	}
	oldStart, oldDue := false, false
	if stored != nil {
		oldStart = stored.StartDateOnly != nil && *stored.StartDateOnly
		oldDue = stored.DueDateOnly != nil && *stored.DueDateOnly
	}
	startDateOnly, dueDateOnly := t.Start != nil && t.StartDateOnly, t.Due != nil && t.DueDateOnly
	if !t.FullUpdate {
		if t.Start == nil {
			startDateOnly = oldStart
		}
		if t.Due == nil {
			dueDateOnly = oldDue
		}
	}
	if startDateOnly == oldStart && dueDateOnly == oldDue {
		return "", nil, nil
	}
	method, uid := http.MethodPost, t.UID
	if stored != nil {
		method, uid = http.MethodPatch, "" // Omission preserves the existing stable UID.
	}
	metadata := taskMetadataJSON(uid, startDateOnly, dueDateOnly)
	if method == http.MethodPatch {
		metadata["@odata.type"] = "#microsoft.graph.openTypeExtension"
	}
	return method, metadata, nil
}

func taskMetadataJSON(uid string, startDateOnly, dueDateOnly bool) map[string]any {
	metadata := map[string]any{
		"@odata.type":   "microsoft.graph.openTypeExtension",
		"extensionName": taskUIDExtensionName,
		"startDateOnly": startDateOnly,
		"dueDateOnly":   dueDateOnly,
	}
	if uid != "" {
		metadata["uid"] = uid
	}
	return metadata
}
