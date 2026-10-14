package davserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cr1cr1/o365bridge/internal/bridge"
)

func TestDAVTaskAttachmentURIsAreCredentialFreeSerializationCopies(t *testing.T) {
	r := httptest.NewRequest("GET", "https://bridge.example:8443/caldav/", nil)
	r.TLS = &tls.ConnectionState{}
	r.URL.User = url.UserPassword("not-for-links", "secret")
	r.SetBasicAuth("not-for-links", "secret")
	r.Header.Set("X-Forwarded-Host", "attacker.invalid")
	r.Header.Set("X-Forwarded-Proto", "http")
	original := bridge.Task{ID: "task/one+%2F", Attachments: []bridge.TaskAttachment{
		{ID: "file/two+%2F", Name: "résumé; #1.bin", ContentType: "text/plain\r\nATTACH:http://untrusted.invalid", URI: "https://upstream.invalid/?X-OWA-CANARY=secret"},
		{ID: "empty-name", ContentType: "text/plain; charset=utf-8"},
	}}
	got := taskWithAttachmentURIs(r, acct, "list/one+%2F", original)
	const first = "https://bridge.example:8443/caldav/user@example.com/list%2Fone+%252F/_attachments/task%2Fone+%252F/file%2Ftwo+%252F/r%C3%A9sum%C3%A9%3B%20%231.bin"
	const second = "https://bridge.example:8443/caldav/user@example.com/list%2Fone+%252F/_attachments/task%2Fone+%252F/empty-name/attachment"
	if got.Attachments[0].URI != first || got.Attachments[1].URI != second {
		t.Fatalf("attachment links = %+v", got.Attachments)
	}
	if got.Attachments[0].ContentType != "application/octet-stream" || got.Attachments[1].ContentType != "text/plain" {
		t.Fatalf("untrusted attachment MIME types were not normalized: %+v", got.Attachments)
	}
	if original.Attachments[0].ContentType != "text/plain\r\nATTACH:http://untrusted.invalid" {
		t.Fatal("MIME normalization mutated provider attachment metadata")
	}
	if original.Attachments[0].URI != "https://upstream.invalid/?X-OWA-CANARY=secret" || original.Attachments[1].URI != "" {
		t.Fatal("serialization mutated provider attachment metadata")
	}
	r.TLS = nil
	if plain := taskWithAttachmentURIs(r, acct, "tasks1", original); plain.Attachments[0].URI[:7] != "http://" {
		t.Fatal("URI scheme trusted request URL or proxy headers instead of TLS state")
	}
}

func TestDAVTaskAttachmentRefsOnlyAllowUnchangedNativeURISet(t *testing.T) {
	r := httptest.NewRequest("PUT", "http://bridge.test/caldav/", nil)
	current := bridge.Task{ID: "native", Attachments: []bridge.TaskAttachment{{ID: "file-one", Name: "a.bin"}, {ID: "file-two", Name: "b.bin"}}}
	const first = "http://bridge.test/caldav/user@example.com/tasks1/_attachments/native/file-one/a.bin"
	const second = "http://bridge.test/caldav/user@example.com/tasks1/_attachments/native/file-two/b.bin"
	for _, tc := range []struct {
		name     string
		incoming []bridge.TaskAttachment
		want     bool
	}{
		{name: "unaware client preserves files", want: true},
		{name: "unchanged", incoming: []bridge.TaskAttachment{{URI: first}, {URI: second}}, want: true},
		{name: "order independent", incoming: []bridge.TaskAttachment{{URI: second}, {URI: first}}, want: true},
		{name: "explicit removal", incoming: []bridge.TaskAttachment{}},
		{name: "partial removal", incoming: []bridge.TaskAttachment{{URI: first}}},
		{name: "duplicate", incoming: []bridge.TaskAttachment{{URI: first}, {URI: first}}},
		{name: "external URI", incoming: []bridge.TaskAttachment{{URI: first}, {URI: "https://untrusted.invalid/file"}}},
		{name: "binary content", incoming: []bridge.TaskAttachment{{URI: first}, {URI: "data:application/octet-stream;base64,AA=="}}},
		{name: "another task", incoming: []bridge.TaskAttachment{{URI: first}, {URI: "http://bridge.test/caldav/user@example.com/tasks1/_attachments/other/file-two/b.bin"}}},
		{name: "another account", incoming: []bridge.TaskAttachment{{URI: first}, {URI: "http://bridge.test/caldav/other@example.com/tasks1/_attachments/native/file-two/b.bin"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskAttachmentRefsMatch(r, acct, "tasks1", current, tc.incoming); got != tc.want {
				t.Fatalf("read-only attachment references accepted=%v, want %v", got, tc.want)
			}
		})
	}
	if taskAttachmentRefsMatch(r, acct, "tasks1", bridge.Task{}, []bridge.TaskAttachment{{URI: first}}) {
		t.Fatal("a new task accepted an attachment reference without native storage")
	}
}

type davTaskAttachmentProvider struct {
	*fakeProvider
	data        []byte
	downloadErr error
	taskListErr error
	downloads   [][3]string
	listings    int
}

func (p *davTaskAttachmentProvider) ListTasks(ctx context.Context, listID string) ([]bridge.Task, error) {
	p.listings++
	if p.taskListErr != nil {
		return nil, p.taskListErr
	}
	return p.fakeProvider.ListTasks(ctx, listID)
}

func (p *davTaskAttachmentProvider) GetTaskAttachment(_ context.Context, listID, taskID, attachmentID string) ([]byte, error) {
	p.downloads = append(p.downloads, [3]string{listID, taskID, attachmentID})
	return p.data, p.downloadErr
}

func newDAVTaskAttachmentProvider() *davTaskAttachmentProvider {
	p := &davTaskAttachmentProvider{fakeProvider: newFake(), data: []byte{0, 1, 255}}
	p.tasks["task/one+%2F"] = bridge.Task{ID: "task/one+%2F", UID: "client-uid", ListID: "tasks1", Attachments: []bridge.TaskAttachment{
		{ID: "file/two+%2F", Name: "résumé; #1.bin", ContentType: "text/plain; charset=utf-8", Size: 3, URI: "https://upstream.invalid/?X-OWA-CANARY=secret"},
	}}
	p.tasks["another-task"] = bridge.Task{ID: "another-task", ListID: "tasks1", Attachments: []bridge.TaskAttachment{{ID: "foreign-file", Name: "foreign.bin"}}}
	return p
}

func newDAVTaskAttachmentHandler(t *testing.T, p bridge.Provider) http.Handler {
	t.Helper()
	reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: fakeLogin{}, Factory: bridge.FactoryFunc(func(string, bridge.Session) bridge.Provider { return p })})
	if err != nil {
		t.Fatal(err)
	}
	return New(reg, pw, zerolog.Nop())
}

const davTaskAttachmentPath = "/caldav/user@example.com/tasks1/_attachments/task%2Fone+%252F/file%2Ftwo+%252F/r%C3%A9sum%C3%A9%3B%20%231.bin"

func TestDAVTaskAttachmentVTODOUsesOnlySafeBridgeURIs(t *testing.T) {
	p := newDAVTaskAttachmentProvider()
	h := newDAVTaskAttachmentHandler(t, p)
	const path = "/caldav/user@example.com/tasks1/client-uid.ics"
	first := req(t, h, "GET", path, "", true)
	second := req(t, h, "GET", path, "", true)
	if first.Code != 200 || second.Code != 200 || first.Header().Get("ETag") != second.Header().Get("ETag") {
		t.Fatalf("task representation or ETag unstable: first=%d second=%d", first.Code, second.Code)
	}
	data := strings.Join(unfold(first.Body.Bytes()), "\r\n")
	if !strings.Contains(data, "ATTACH;FMTTYPE=text/plain:http://example.com"+davTaskAttachmentPath) {
		t.Fatalf("VTODO omitted the safe native-file URI: %s", data)
	}
	for _, secret := range []string{"upstream.invalid", "X-OWA-CANARY", "instance-secret"} {
		if strings.Contains(data, secret) {
			t.Fatalf("VTODO exposed %q", secret)
		}
	}
	if p.tasks["task/one+%2F"].Attachments[0].URI != "https://upstream.invalid/?X-OWA-CANARY=secret" || len(p.downloads) != 0 {
		t.Fatal("VTODO serialization mutated native metadata or downloaded file bytes")
	}
}

func TestDAVTaskAttachmentGETAndHEADUseAuthenticatedBinaryRoute(t *testing.T) {
	p := newDAVTaskAttachmentProvider()
	h := newDAVTaskAttachmentHandler(t, p)
	for _, method := range []string{"GET", "HEAD"} {
		rr := req(t, h, method, davTaskAttachmentPath, "", true)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d, %s", method, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Content-Type") != "text/plain" || rr.Header().Get("Content-Length") != "3" || rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("unsafe binary response headers: %+v", rr.Header())
		}
		disposition, params, err := mime.ParseMediaType(rr.Header().Get("Content-Disposition"))
		if err != nil || disposition != "attachment" || params["filename"] != "résumé; #1.bin" {
			t.Fatalf("filename response = %s, %v", rr.Header().Get("Content-Disposition"), err)
		}
		if method == "GET" && !bytes.Equal(rr.Body.Bytes(), []byte{0, 1, 255}) {
			t.Fatalf("binary file changed: %v", rr.Body.Bytes())
		}
		if method == "HEAD" && rr.Body.Len() != 0 {
			t.Fatal("HEAD returned file bytes")
		}
	}
	if len(p.downloads) != 2 || p.listings != 2 || p.downloads[0] != [3]string{"tasks1", "task/one+%2F", "file/two+%2F"} {
		t.Fatalf("route did not scope and decode native identities once: downloads=%v listings=%d", p.downloads, p.listings)
	}
}

func TestDAVTaskAttachmentRejectsUnownedRoutesWithoutDownload(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		auth               bool
		status             int
	}{
		{name: "no authentication", method: "GET", path: davTaskAttachmentPath, status: 401},
		{name: "wrong account", method: "GET", path: strings.Replace(davTaskAttachmentPath, "user@example.com", "other@example.com", 1), auth: true, status: 403},
		{name: "unknown collection", method: "GET", path: strings.Replace(davTaskAttachmentPath, "tasks1", "unknown", 1), auth: true, status: 404},
		{name: "event collection", method: "GET", path: strings.Replace(davTaskAttachmentPath, "tasks1", "cal1", 1), auth: true, status: 404},
		{name: "unknown task", method: "GET", path: strings.Replace(davTaskAttachmentPath, "task%2Fone+%252F", "unknown", 1), auth: true, status: 404},
		{name: "client UID is not native ID", method: "GET", path: strings.Replace(davTaskAttachmentPath, "task%2Fone+%252F", "client-uid", 1), auth: true, status: 404},
		{name: "foreign same-account token", method: "GET", path: strings.Replace(davTaskAttachmentPath, "file%2Ftwo+%252F", "foreign-file", 1), auth: true, status: 404},
		{name: "double decoding not accepted", method: "GET", path: strings.Replace(davTaskAttachmentPath, "task%2Fone+%252F", "task%2Fone+%2F", 1), auth: true, status: 404},
		{name: "missing filename", method: "GET", path: "/caldav/user@example.com/tasks1/_attachments/task/file", auth: true, status: 404},
		{name: "extra segment", method: "GET", path: davTaskAttachmentPath + "/extra", auth: true, status: 404},
		{name: "upload forbidden", method: "PUT", path: davTaskAttachmentPath, auth: true, status: 405},
		{name: "delete forbidden", method: "DELETE", path: davTaskAttachmentPath, auth: true, status: 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDAVTaskAttachmentProvider()
			rr := req(t, newDAVTaskAttachmentHandler(t, p), tc.method, tc.path, "", tc.auth)
			if rr.Code != tc.status || len(p.downloads) != 0 {
				t.Fatalf("rejected route = %d (%s), downloads=%v; want %d", rr.Code, rr.Body.String(), p.downloads, tc.status)
			}
			if tc.status == 405 && rr.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal("read-only route omitted allowed methods")
			}
		})
	}
}

func TestDAVTaskAttachmentRejectsAmbiguousOrOversizeMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*davTaskAttachmentProvider)
		status int
	}{
		{name: "duplicate native token", status: 404, mutate: func(p *davTaskAttachmentProvider) {
			task := p.tasks["task/one+%2F"]
			task.Attachments = append(task.Attachments, task.Attachments[0])
			p.tasks[task.ID] = task
		}},
		{name: "known oversize file", status: 413, mutate: func(p *davTaskAttachmentProvider) {
			task := p.tasks["task/one+%2F"]
			task.Attachments[0].Size = bridge.MaxTaskAttachmentBytes + 1
			p.tasks[task.ID] = task
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDAVTaskAttachmentProvider()
			tc.mutate(p)
			rr := req(t, newDAVTaskAttachmentHandler(t, p), "GET", davTaskAttachmentPath, "", true)
			if rr.Code != tc.status || len(p.downloads) != 0 {
				t.Fatalf("invalid metadata = %d, downloads=%v", rr.Code, p.downloads)
			}
		})
	}
}

func TestDAVTaskAttachmentUnsupportedCapabilityAndProviderFailures(t *testing.T) {
	p := newDAVTaskAttachmentProvider()
	if rr := req(t, newDAVTaskAttachmentHandler(t, p.fakeProvider), "GET", davTaskAttachmentPath, "", true); rr.Code != 501 {
		t.Fatalf("unsupported provider = %d, %s", rr.Code, rr.Body.String())
	}
	for _, tc := range []struct {
		err    error
		status int
	}{
		{err: fs.ErrNotExist, status: 404},
		{err: errors.ErrUnsupported, status: 501},
		{err: bridge.TransientError("upstream private failure"), status: 502},
	} {
		p := newDAVTaskAttachmentProvider()
		p.downloadErr = tc.err
		rr := req(t, newDAVTaskAttachmentHandler(t, p), "GET", davTaskAttachmentPath, "", true)
		if rr.Code != tc.status || strings.Contains(rr.Body.String(), "private failure") {
			t.Fatalf("provider failure = %d, %s", rr.Code, rr.Body.String())
		}
	}
}

func TestDAVTaskAttachmentEmptyAndUnsafeFileMetadata(t *testing.T) {
	p := newDAVTaskAttachmentProvider()
	p.data = []byte{}
	task := p.tasks["task/one+%2F"]
	task.Attachments[0].Name = "evil\r\nX-Injected: true"
	task.Attachments[0].ContentType = "text/html\r\nX-Injected: true"
	p.tasks[task.ID] = task
	rr := req(t, newDAVTaskAttachmentHandler(t, p), "GET", davTaskAttachmentPath, "", true)
	if rr.Code != 200 || rr.Body.Len() != 0 || rr.Header().Get("Content-Length") != "0" || rr.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("empty unsafe file metadata = %d, %+v, %s", rr.Code, rr.Header(), rr.Body.String())
	}
	if strings.ContainsAny(rr.Header().Get("Content-Disposition"), "\r\n") || rr.Header().Get("X-Injected") != "" {
		t.Fatal("native filename injected a response header")
	}
}

func TestDAVTaskAttachmentRejectsMalformedEscapedSegments(t *testing.T) {
	for _, index := range []int{4, 5, 6} {
		p := newDAVTaskAttachmentProvider()
		h := newDAVTaskAttachmentHandler(t, p).(*handler)
		r := httptest.NewRequest("GET", davTaskAttachmentPath, nil)
		segs := []string{"caldav", acct, "tasks1", "_attachments", "task%2Fone+%252F", "file%2Ftwo+%252F", "name.bin"}
		segs[index] = "bad%2"
		rr := httptest.NewRecorder()
		h.handleTaskAttachment(rr, r, acct, "tasks1", segs)
		if rr.Code != 400 || len(p.downloads) != 0 || p.listings != 0 {
			t.Fatalf("malformed segment %d = %d, downloads=%v listings=%d", index, rr.Code, p.downloads, p.listings)
		}
	}
}

func TestDAVTaskAttachmentRejectsActualOversizeBytes(t *testing.T) {
	p := newDAVTaskAttachmentProvider()
	p.data = make([]byte, bridge.MaxTaskAttachmentBytes+1)
	rr := req(t, newDAVTaskAttachmentHandler(t, p), "GET", davTaskAttachmentPath, "", true)
	if rr.Code != 413 || len(p.downloads) != 1 || rr.Body.Len() > 1024 {
		t.Fatalf("oversize bytes escaped the route: status=%d downloads=%v response size=%d", rr.Code, p.downloads, rr.Body.Len())
	}
}

func TestDAVTaskAttachmentMIMETypeIsAlwaysSafeBareMediaType(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{input: "Text/Plain; charset=utf-8", want: "text/plain"},
		{input: "attachment", want: "application/octet-stream"},
		{input: "", want: "application/octet-stream"},
		{input: "text/html\r\nATTACH:untrusted", want: "application/octet-stream"},
	} {
		if got := taskAttachmentMediaType(tc.input); got != tc.want {
			t.Fatalf("media type for %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}

type davTaskAttachmentRetryLogin struct{ fakeLogin }

func (davTaskAttachmentRetryLogin) Renew(_ context.Context, _ string, s bridge.Session) (bridge.Session, error) {
	s.AccessToken = "renewed"
	s.TokenExpiry = time.Now().Add(time.Hour)
	return s, nil
}

func TestDAVTaskAttachmentRevalidatesProviderOnRegistryRetry(t *testing.T) {
	for _, scenario := range []string{"ownership lost", "capability lost", "new provider succeeds"} {
		t.Run(scenario, func(t *testing.T) {
			old := newDAVTaskAttachmentProvider()
			old.downloadErr = bridge.AuthError("expired")
			fresh := newDAVTaskAttachmentProvider()
			fresh.data = []byte{7}
			var renewed bridge.Provider = fresh
			want := 200
			if scenario == "ownership lost" {
				delete(fresh.tasks, "task/one+%2F")
				want = 404
			}
			if scenario == "capability lost" {
				renewed = fresh.fakeProvider
				want = 501
			}
			reg, err := bridge.NewRegistry(bridge.Config{StateDir: t.TempDir(), Login: davTaskAttachmentRetryLogin{}, Factory: bridge.FactoryFunc(func(_ string, s bridge.Session) bridge.Provider {
				if s.AccessToken == "renewed" {
					return renewed
				}
				return old
			})})
			if err != nil {
				t.Fatal(err)
			}
			rr := req(t, New(reg, pw, zerolog.Nop()), "GET", davTaskAttachmentPath, "", true)
			if rr.Code != want || len(old.downloads) != 1 || old.listings != 1 {
				t.Fatalf("registry retry = %d, old downloads=%v listings=%d; want %d", rr.Code, old.downloads, old.listings, want)
			}
			if scenario == "ownership lost" && (fresh.listings != 1 || len(fresh.downloads) != 0) {
				t.Fatalf("renewed provider ownership was not checked before download: listings=%d downloads=%v", fresh.listings, fresh.downloads)
			}
			if scenario == "new provider succeeds" && (!bytes.Equal(rr.Body.Bytes(), []byte{7}) || len(fresh.downloads) != 1 || fresh.listings != 1) {
				t.Fatalf("new provider was not used: body=%v listings=%d downloads=%v", rr.Body.Bytes(), fresh.listings, fresh.downloads)
			}
		})
	}
}
