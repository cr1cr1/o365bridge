package owa

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestOWATaskAttachmentMetadataOnlyAdvertisesNativeFiles(t *testing.T) {
	item := map[string]any{"Attachments": []any{
		map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": "file/one+%2F"}, "Name": "résumé.bin", "ContentType": "application/octet-stream", "Size": float64(3), "ContentLocation": "https://untrusted.invalid/?secret=canary"},
		map[string]any{"__type": "ItemAttachment:#Exchange", "AttachmentId": map[string]any{"Id": "item"}, "Name": "nested message"},
		map[string]any{"__type": "ReferenceAttachment:#Exchange", "AttachmentId": map[string]any{"Id": "reference"}, "ProviderEndpointUrl": "https://untrusted.invalid/"},
		map[string]any{"__type": "FileAttachment:#Exchange", "Name": "missing ID"},
		map[string]any{"AttachmentId": map[string]any{"Id": "unknown-type"}},
		map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": "empty"}, "Size": float64(0)},
	}}
	want := []bridge.TaskAttachment{{ID: "file/one+%2F", Name: "résumé.bin", ContentType: "application/octet-stream", Size: 3}, {ID: "empty"}}
	if got := taskAttachmentsFromItem(item); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata = %+v, want %+v", got, want)
	}
	if got := taskAttachmentsFromItem(map[string]any{}); len(got) != 0 {
		t.Fatalf("missing attachments = %+v", got)
	}
}

func TestOWATaskAttachmentDownloadBindsTaskAndUsesNativeJSON(t *testing.T) {
	const taskID, attachmentID = "task/one+%2F", "attachment/two+%2F"
	f, p := newFake(t, func(action string, request map[string]any) any {
		switch action {
		case "FindItem":
			if ids := arr(request, "Body", "ParentFolderIds"); len(ids) != 1 || str(ids[0], "Id") != "tasks" {
				t.Errorf("membership discovery selected wrong list: %+v", request)
			}
			return okBody(map[string]any{"RootFolder": map[string]any{
				"Items":            []any{map[string]any{"ItemId": map[string]any{"Id": taskID}}},
				"TotalItemsInView": 1, "IncludesLastItemInRange": true,
			}})
		case "GetItem":
			if ids := arr(request, "Body", "ItemIds"); len(ids) != 1 || str(ids[0], "Id") != taskID {
				t.Errorf("ownership lookup selected wrong task: %+v", request)
			}
			found := false
			for _, property := range arr(request, "Body", "ItemShape", "AdditionalProperties") {
				found = found || str(property, "FieldURI") == "item:Attachments"
			}
			if !found {
				t.Error("ownership lookup omitted attachment metadata")
			}
			return itemResp(map[string]any{
				"__type": "Task:#Exchange", "ItemId": map[string]any{"Id": taskID},
				"Attachments": []any{map[string]any{
					"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": attachmentID},
				}},
			})
		case "GetAttachment":
			if str(request, "__type") != "GetAttachmentJsonRequest:#Exchange" || str(request, "Body", "__type") != "GetAttachmentRequest:#Exchange" || str(request, "Header", "RequestServerVersion") != "Exchange2013" {
				t.Errorf("wrong native request envelope: %+v", request)
			}
			ids := arr(request, "Body", "AttachmentIds")
			if len(ids) != 1 || str(ids[0], "__type") != "AttachmentId:#Exchange" || str(ids[0], "Id") != attachmentID {
				t.Errorf("wrong attachment id type or value: %+v", request)
			}
			if str(request, "Body", "AttachmentShape", "__type") != "AttachmentResponseShapeType:#Exchange" {
				t.Errorf("wrong attachment shape contract: %+v", request)
			}
			return okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{
				map[string]any{"ResponseClass": "Success", "ResponseCode": "NoError", "Attachments": []any{
					map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": attachmentID}, "Content": "AAH/"},
				}},
			}}})
		default:
			t.Errorf("unexpected request %s", action)
			return okBody(nil)
		}
	})
	downloader, ok := p.(bridge.TaskAttachmentProvider)
	if !ok {
		t.Fatal("OWA provider does not implement optional task attachment download")
	}
	data, err := downloader.GetTaskAttachment(context.Background(), "tasks", taskID, attachmentID)
	if err != nil || !bytes.Equal(data, []byte{0, 1, 255}) {
		t.Fatalf("download = %v, %v", data, err)
	}
	if len(f.requests) != 3 || f.requests[0].Action != "FindItem" || f.requests[1].Action != "GetItem" || f.requests[2].Action != "GetAttachment" {
		t.Fatalf("download did not verify ownership before fetching bytes: %+v", f.requests)
	}
	if f.last("GetAttachment").Auth != "Bearer token123" {
		t.Fatal("attachment download omitted server-side authentication")
	}
}

func nativeTaskAttachment(id, typ string) map[string]any {
	return map[string]any{"__type": typ, "AttachmentId": map[string]any{"Id": id}}
}

func newTaskAttachmentFake(t *testing.T, attachments []any, result any) (*fake, bridge.TaskAttachmentProvider) {
	t.Helper()
	f, p := newFake(t, func(action string, request map[string]any) any {
		switch action {
		case "FindItem":
			var rows []any
			if str(arr(request, "Body", "ParentFolderIds")[0], "Id") == "tasks" {
				rows = []any{map[string]any{"ItemId": map[string]any{"Id": "owned-task"}}, map[string]any{"ItemId": map[string]any{"Id": "other-task"}}}
			}
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": rows, "TotalItemsInView": len(rows), "IncludesLastItemInRange": true}})
		case "GetItem":
			return itemResp(
				map[string]any{"__type": "Task:#Exchange", "ItemId": map[string]any{"Id": "owned-task"}, "Attachments": attachments},
				map[string]any{"__type": "Task:#Exchange", "ItemId": map[string]any{"Id": "other-task"}, "Attachments": []any{nativeTaskAttachment("foreign-file", "FileAttachment:#Exchange")}},
			)
		case "GetAttachment":
			return result
		default:
			t.Errorf("unexpected attachment action %s", action)
			return okBody(nil)
		}
	})
	return f, p.(bridge.TaskAttachmentProvider)
}

func TestOWATaskAttachmentRejectsUnownedIDsBeforeDownload(t *testing.T) {
	for _, tc := range []struct {
		name, list, task, attachment string
		attachments                  []any
	}{
		{name: "unknown list", list: "unknown-list", task: "owned-task", attachment: "file", attachments: []any{nativeTaskAttachment("file", "FileAttachment:#Exchange")}},
		{name: "unknown task", list: "tasks", task: "unknown-task", attachment: "file", attachments: []any{nativeTaskAttachment("file", "FileAttachment:#Exchange")}},
		{name: "missing file", list: "tasks", task: "owned-task", attachment: "file"},
		{name: "another task's token", list: "tasks", task: "owned-task", attachment: "foreign-file"},
		{name: "embedded item", list: "tasks", task: "owned-task", attachment: "file", attachments: []any{nativeTaskAttachment("file", "ItemAttachment:#Exchange")}},
		{name: "reference", list: "tasks", task: "owned-task", attachment: "file", attachments: []any{nativeTaskAttachment("file", "ReferenceAttachment:#Exchange")}},
		{name: "ambiguous token", list: "tasks", task: "owned-task", attachment: "file", attachments: []any{nativeTaskAttachment("file", "FileAttachment:#Exchange"), nativeTaskAttachment("file", "FileAttachment:#Exchange")}},
		{name: "missing list", task: "owned-task", attachment: "file"},
		{name: "missing task", list: "tasks", attachment: "file"},
		{name: "missing attachment", list: "tasks", task: "owned-task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p := newTaskAttachmentFake(t, tc.attachments, nil)
			data, err := p.GetTaskAttachment(context.Background(), tc.list, tc.task, tc.attachment)
			if !errors.Is(err, fs.ErrNotExist) || len(data) != 0 {
				t.Fatalf("unowned attachment = %v, %v", data, err)
			}
			for _, request := range f.requests {
				if request.Action == "GetAttachment" {
					t.Fatal("unowned attachment token reached the download endpoint")
				}
			}
		})
	}
}

func TestOWATaskAttachmentValidatesNativeResponse(t *testing.T) {
	for _, tc := range []struct {
		name        string
		attachment  map[string]any
		want        []byte
		wantErr     bool
		unsupported bool
	}{
		{name: "empty file", attachment: map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": "file"}, "Content": ""}, want: []byte{}},
		{name: "missing content", attachment: nativeTaskAttachment("file", "FileAttachment:#Exchange"), wantErr: true},
		{name: "nonstring content", attachment: map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": "file"}, "Content": map[string]any{"Value": "AA=="}}, wantErr: true},
		{name: "wrong id", attachment: map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": "foreign-file"}, "Content": "AA=="}, wantErr: true},
		{name: "missing id", attachment: map[string]any{"__type": "FileAttachment:#Exchange", "Content": "AA=="}, wantErr: true},
		{name: "embedded item", attachment: nativeTaskAttachment("file", "ItemAttachment:#Exchange"), wantErr: true, unsupported: true},
		{name: "reference", attachment: nativeTaskAttachment("file", "ReferenceAttachment:#Exchange"), wantErr: true, unsupported: true},
		{name: "unknown type", attachment: nativeTaskAttachment("file", ""), wantErr: true, unsupported: true},
		{name: "bad base64", attachment: map[string]any{"__type": "FileAttachment:#Exchange", "AttachmentId": map[string]any{"Id": "file"}, "Content": "not base64!"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p := newTaskAttachmentFake(t, []any{nativeTaskAttachment("file", "FileAttachment:#Exchange")}, okBody(map[string]any{"Attachments": []any{tc.attachment}}))
			data, err := p.GetTaskAttachment(context.Background(), "tasks", "owned-task", "file")
			if (err != nil) != tc.wantErr || !bytes.Equal(data, tc.want) {
				t.Fatalf("download = %v, %v; want %v, error=%v", data, err, tc.want, tc.wantErr)
			}
			if tc.unsupported && !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("non-file was not classified unsupported: %v", err)
			}
		})
	}
}

func TestOWATaskAttachmentRejectsNativeServiceErrorsAndAmbiguousResults(t *testing.T) {
	file := nativeTaskAttachment("file", "FileAttachment:#Exchange")
	file["Content"] = "AA=="
	for _, result := range []any{
		okBody(map[string]any{"Attachments": []any{}}),
		okBody(map[string]any{"Attachments": []any{file, file}}),
		okBody(map[string]any{"ResponseMessages": map[string]any{"Items": []any{
			map[string]any{"ResponseClass": "Success", "Attachments": []any{file}},
			map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorInvalidAttachmentId"},
		}}}),
		okBody(map[string]any{"ResponseClass": "Error", "ResponseCode": "ErrorInvalidAttachmentId"}),
	} {
		_, p := newTaskAttachmentFake(t, []any{nativeTaskAttachment("file", "FileAttachment:#Exchange")}, result)
		if data, err := p.GetTaskAttachment(context.Background(), "tasks", "owned-task", "file"); err == nil || len(data) != 0 {
			t.Fatalf("invalid native result accepted: %v, %v", data, err)
		}
	}
}

func TestOWATaskAttachmentRejectsOversizeMetadataBeforeDownload(t *testing.T) {
	file := nativeTaskAttachment("file", "FileAttachment:#Exchange")
	file["Size"] = bridge.MaxTaskAttachmentBytes + 1
	f, p := newTaskAttachmentFake(t, []any{file}, nil)
	if _, err := p.GetTaskAttachment(context.Background(), "tasks", "owned-task", "file"); err == nil {
		t.Fatal("oversize metadata was accepted")
	}
	for _, request := range f.requests {
		if request.Action == "GetAttachment" {
			t.Fatal("known oversize file was downloaded")
		}
	}
}

type attachmentTestTransport func(*http.Request) (*http.Response, error)

func (f attachmentTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type attachmentCountingReader struct {
	io.Reader
	reads int
	bytes int64
}

func (r *attachmentCountingReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.Reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

type attachmentPaddingReader struct{}

func (attachmentPaddingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestOWATaskAttachmentBoundsUnknownLengthResponse(t *testing.T) {
	limit := (bridge.MaxTaskAttachmentBytes+2)/3*4 + (1 << 20)
	body := &attachmentCountingReader{Reader: io.MultiReader(
		strings.NewReader(`{"Body":{"padding":"`),
		io.LimitReader(attachmentPaddingReader{}, limit+4096),
		strings.NewReader(`"}}`),
	)}
	p := Factory{BaseURL: "https://owa.invalid/", HTTP: &http.Client{Transport: attachmentTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: io.NopCloser(body), Header: make(http.Header)}, nil
	})}}.New("user@example.com", bridge.Session{}).(*provider)
	if _, err := p.call(context.Background(), "GetAttachment", map[string]any{}); err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("unknown-length oversize response = %v", err)
	}
	if body.bytes > limit+1 {
		t.Fatalf("read %d bytes, beyond wire budget %d", body.bytes, limit+1)
	}
}

func TestOWATaskAttachmentContentEnforcesDecodedLimitBeforeAllocation(t *testing.T) {
	encodedLength := int((bridge.MaxTaskAttachmentBytes + 2) / 3 * 4)
	for _, tc := range []struct {
		name    string
		extra   int
		padding string
		wantErr bool
	}{
		{name: "largest supported file", padding: "="},
		{name: "one byte over with same encoded length", wantErr: true},
		{name: "encoded size overflow", extra: 4, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Repeat("A", encodedLength+tc.extra-len(tc.padding)) + tc.padding
			data, err := decodeTaskAttachmentContent(content)
			if (err != nil) != tc.wantErr {
				t.Fatalf("decoded size %d, error=%v, want error=%v", len(data), err, tc.wantErr)
			}
			if !tc.wantErr && int64(len(data)) != bridge.MaxTaskAttachmentBytes {
				t.Fatalf("decoded %d bytes, want %d", len(data), bridge.MaxTaskAttachmentBytes)
			}
			if tc.wantErr && len(data) != 0 {
				t.Fatal("oversize attachment returned partial file bytes")
			}
		})
	}
}

func TestOWATaskAttachmentResponseSizeBoundDoesNotAffectOtherActions(t *testing.T) {
	for _, action := range []string{"GetAttachment", "GetItem"} {
		t.Run(action, func(t *testing.T) {
			body := &attachmentCountingReader{Reader: strings.NewReader(`{"Body":{"Attachments":[]}}`)}
			p := Factory{BaseURL: "https://owa.invalid/", HTTP: &http.Client{Transport: attachmentTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: (bridge.MaxTaskAttachmentBytes+2)/3*4 + (1 << 20) + 1, Body: io.NopCloser(body), Header: make(http.Header)}, nil
			})}}.New("user@example.com", bridge.Session{}).(*provider)
			_, err := p.call(context.Background(), action, map[string]any{})
			if action == "GetAttachment" {
				if err == nil || body.reads != 0 {
					t.Fatalf("oversize response was decoded: reads=%d, err=%v", body.reads, err)
				}
			} else if err != nil || body.reads == 0 {
				t.Fatalf("nonattachment action changed: reads=%d, err=%v", body.reads, err)
			}
		})
	}
}
