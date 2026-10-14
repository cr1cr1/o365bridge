package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

type taskAttachmentDownloader = bridge.TaskAttachmentProvider

func TestGraphTaskAttachmentMetadataOnlyForTasksWithFiles(t *testing.T) {
	var baseURL string
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch r.URL.Path {
		case "/v1.0/me/todo/lists/list1/tasks":
			jsonOut(w, http.StatusOK, `{"value":[{"id":"task1","hasAttachments":true},{"id":"task2","hasAttachments":false}]}`)
		case "/v1.0/me/todo/lists/list1/tasks/task1/attachments":
			if r.URL.Query().Get("$skiptoken") == "page2" {
				jsonOut(w, http.StatusOK, `{"value":[{"@odata.type":"#microsoft.graph.taskFileAttachment","id":"second","name":"second.txt","size":2,"contentType":"text/plain"}]}`)
				return
			}
			data, _ := json.Marshal(map[string]any{
				"@odata.nextLink": baseURL + "/v1.0/me/todo/lists/list1/tasks/task1/attachments?$skiptoken=page2",
				"value": []any{
					map[string]any{"@odata.type": "#microsoft.graph.taskFileAttachment", "id": "first", "name": "note.txt", "contentType": "text/plain", "size": 5},
					map[string]any{"@odata.type": "#microsoft.graph.referenceAttachment", "id": "link", "sourceUrl": "https://example.com/file"},
				},
			})
			jsonOut(w, http.StatusOK, string(data))
		default:
			t.Errorf("must not eagerly fetch file contents, unknown URLs, or no-file task metadata: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	baseURL = f.srv.URL
	tasks, err := f.provider().ListTasks(context.Background(), "list1")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("ListTasks = %+v, %v", tasks, err)
	}
	if len(tasks[0].Attachments) != 2 {
		t.Fatalf("native file metadata missing: %+v", tasks[0].Attachments)
	}
	first := tasks[0].Attachments[0]
	if first.ID != "first" || first.Name != "note.txt" || first.ContentType != "text/plain" || first.Size != 5 || first.URI != "" || tasks[0].Attachments[1].ID != "second" {
		t.Errorf("native metadata = %+v", tasks[0].Attachments)
	}
	if tasks[1].Attachments != nil || len(f.reqs) != 3 {
		t.Errorf("no-file task must not trigger requests: tasks=%+v requests=%+v", tasks, f.reqs)
	}
}

func TestGraphTaskAttachmentDownloadEnforcesMemoryBound(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/$value") {
			_, _ = w.Write([]byte(strings.Repeat("x", int(bridge.MaxTaskAttachmentBytes+1))))
			return
		}
		jsonOut(w, http.StatusOK, `{"value":[{"@odata.type":"#microsoft.graph.taskFileAttachment","id":"file1","name":"huge.bin","size":1}]}`)
	})
	p := f.provider().(taskAttachmentDownloader)
	if data, err := p.GetTaskAttachment(context.Background(), "list1", "task1", "file1"); err == nil || len(data) != 0 {
		t.Fatalf("oversized raw content must fail even when metadata size lies: bytes=%d err=%v", len(data), err)
	}
}

func TestGraphTaskAttachmentDownloadIsScopedAndAuthenticated(t *testing.T) {
	const listID, taskID, attachmentID = "list/+%", "task/+%", "file/+%"
	base := "/v1.0/me/todo/lists/" + url.PathEscape(listID) + "/tasks/" + url.PathEscape(taskID) + "/attachments"
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer tok123" {
			t.Errorf("download is not authenticated GET: %s %v", r.Method, r.Header)
		}
		switch r.URL.EscapedPath() {
		case base:
			data, _ := json.Marshal(map[string]any{"value": []any{map[string]any{"@odata.type": "#microsoft.graph.taskFileAttachment", "id": attachmentID, "name": "note.txt", "contentType": "text/plain", "size": 5}}})
			jsonOut(w, http.StatusOK, string(data))
		case base + "/" + url.PathEscape(attachmentID) + "/$value":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello"))
		default:
			t.Errorf("unexpected or unescaped attachment path: %s", r.URL.EscapedPath())
			http.NotFound(w, r)
		}
	})
	p, ok := f.provider().(taskAttachmentDownloader)
	if !ok {
		t.Fatal("Graph provider lacks task attachment download capability")
	}
	data, err := p.GetTaskAttachment(context.Background(), listID, taskID, attachmentID)
	if err != nil || string(data) != "hello" {
		t.Fatalf("GetTaskAttachment = %q, %v", data, err)
	}
	if len(f.reqs) != 2 {
		t.Fatalf("download must validate metadata then retrieve raw content: %+v", f.reqs)
	}
}

func TestGraphTaskAttachmentDownloadRejectsMissingOrNonFile(t *testing.T) {
	for _, tc := range []struct {
		name, metadata string
		want           error
	}{
		{"missing", `{"value":[]}`, fs.ErrNotExist},
		{"wrong id", `{"value":[{"@odata.type":"#microsoft.graph.taskFileAttachment","id":"other","name":"other.txt"}]}`, fs.ErrNotExist},
		{"unsupported type", `{"value":[{"@odata.type":"#microsoft.graph.referenceAttachment","id":"file1","name":"link","sourceUrl":"https://example.com/file"}]}`, errors.ErrUnsupported},
		{"duplicate id", `{"value":[{"@odata.type":"#microsoft.graph.taskFileAttachment","id":"file1","name":"one"},{"@odata.type":"#microsoft.graph.taskFileAttachment","id":"file1","name":"two"}]}`, nil},
		{"metadata error", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if r.URL.Path != "/v1.0/me/todo/lists/list1/tasks/task1/attachments" {
					t.Errorf("must not download absent or unsupported file: %s", r.URL.Path)
				}
				if tc.metadata == "" {
					jsonOut(w, http.StatusForbidden, `{"error":{"code":"ErrorAccessDenied","message":"denied"}}`)
					return
				}
				jsonOut(w, http.StatusOK, tc.metadata)
			})
			p, ok := f.provider().(taskAttachmentDownloader)
			if !ok {
				t.Fatal("Graph provider lacks task attachment download capability")
			}
			data, err := p.GetTaskAttachment(context.Background(), "list1", "task1", "file1")
			if err == nil || len(data) != 0 {
				t.Fatalf("unsafe download = %q, %v", data, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("attachment error = %v, want sentinel %v", err, tc.want)
			}
			if len(f.reqs) != 1 {
				t.Errorf("unexpected content request: %+v", f.reqs)
			}
		})
	}
}

func TestGraphTaskAttachmentDownloadRejectsEmptyIDs(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("invalid ID reached upstream: %s", r.URL.Path)
	})
	p, ok := f.provider().(taskAttachmentDownloader)
	if !ok {
		t.Fatal("Graph provider lacks task attachment download capability")
	}
	for _, ids := range [][3]string{{"", "t", "a"}, {"l", "", "a"}, {"l", "t", ""}} {
		if _, err := p.GetTaskAttachment(context.Background(), ids[0], ids[1], ids[2]); err == nil {
			t.Errorf("empty ID accepted: %q", ids)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatalf("invalid identifiers must not make requests: %+v", f.reqs)
	}
}

func TestGraphTaskAttachmentUpstreamNotFoundUsesRouteSentinel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		raw    bool
	}{
		{"task gone", http.StatusNotFound, false},
		{"file gone", http.StatusNotFound, true},
		{"metadata auth", http.StatusUnauthorized, false},
		{"content auth", http.StatusUnauthorized, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if tc.raw && !strings.HasSuffix(r.URL.Path, "/$value") {
					jsonOut(w, http.StatusOK, `{"value":[{"@odata.type":"#microsoft.graph.taskFileAttachment","id":"file1","size":5}]}`)
					return
				}
				jsonOut(w, tc.status, `{"error":{"code":"TestError","message":"upstream failure"}}`)
			})
			data, err := f.provider().(taskAttachmentDownloader).GetTaskAttachment(context.Background(), "list1", "task1", "file1")
			if err == nil || len(data) != 0 {
				t.Fatalf("error became content: %q, %v", data, err)
			}
			if tc.status == http.StatusNotFound {
				if !errors.Is(err, fs.ErrNotExist) || bridge.KindOf(err) != bridge.KindOther {
					t.Fatalf("attachment 404 must map to missing file, not auth fallback: %v", err)
				}
			} else if !bridge.IsAuth(err) || errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("401 classification changed: %v", err)
			}
		})
	}
}

func TestGraphOrdinaryHTTPErrorsKeepClassificationAndText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"not found", http.StatusNotFound, "graph: GET /me/todo/lists: status 404: code=TestError: upstream failure"},
		{"unauthorized", http.StatusUnauthorized, "graph: GET /me/todo/lists: status 401: code=TestError: upstream failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				jsonOut(w, tc.status, `{"error":{"code":"TestError","message":"upstream failure"}}`)
			})
			_, err := f.provider().ListTaskLists(context.Background())
			if err == nil || err.Error() != tc.want || !bridge.IsAuth(err) || errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("ordinary Graph error changed: %v", err)
			}
		})
	}
}

func TestGraphTaskAttachmentRawErrorIsNotReturnedAsFile(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/$value") {
			jsonOut(w, http.StatusNotFound, `{"error":{"code":"ErrorItemNotFound","message":"removed"}}`)
			return
		}
		jsonOut(w, http.StatusOK, `{"value":[{"@odata.type":"#microsoft.graph.taskFileAttachment","id":"file1","name":"note.txt","size":5}]}`)
	})
	p, ok := f.provider().(taskAttachmentDownloader)
	if !ok {
		t.Fatal("Graph provider lacks task attachment download capability")
	}
	if data, err := p.GetTaskAttachment(context.Background(), "list1", "task1", "file1"); err == nil || len(data) != 0 {
		t.Fatalf("upstream error returned as content: %q, %v", data, err)
	}
}
