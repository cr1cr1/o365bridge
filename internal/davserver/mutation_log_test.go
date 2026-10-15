package davserver

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/rs/zerolog"
)

func TestDAVMutationLogsRejectedStatusWithoutRequestContents(t *testing.T) {
	for _, mode := range []struct {
		name, method, level string
		status              int
	}{
		{"stale conditional update", "PUT", "warn", 412},
		{"missing delete", "DELETE", "warn", 404},
		{"successful update control", "PUT", "info", 204},
	} {
		t.Run(mode.name, func(t *testing.T) {
			p := newFake()
			p.tasks["native"] = bridge.Task{ID: "native", ListID: "tasks1", UID: "private-task-uid", Title: "Original", Body: "Existing notes"}
			var logs bytes.Buffer
			h := newHandlerWithLogger(t, p, zerolog.New(&logs))
			path := "/caldav/" + acct + "/tasks1/private-task-uid.ics"
			if mode.method == "DELETE" {
				path = "/caldav/" + acct + "/tasks1/private-missing-uid.ics"
			}
			body := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:private-task-uid\r\nSUMMARY:Edited\r\nDESCRIPTION:body-secret\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
			r := httptest.NewRequest(mode.method, path+"?token=query-secret", strings.NewReader(body))
			r.SetBasicAuth(acct, pw)
			r.Header.Set("Cookie", "session=cookie-secret")
			r.Header.Set("X-Diagnostic", "header-secret")
			if mode.status == 412 {
				r.Header.Set("If-Match", `"etag-secret"`)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if rr.Code != mode.status {
				t.Fatalf("response=%d %s", rr.Code, rr.Body.String())
			}
			if mode.status >= 400 && (len(p.tasks) != 1 || p.tasks["native"].Title != "Original") {
				t.Fatal("rejected request mutated a native task")
			}
			if logs.Len() == 0 {
				t.Fatal("DAV mutation status was not logged")
			}
			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
				t.Fatalf("invalid or duplicate mutation log entries: %v: %s", err, logs.String())
			}
			if entry["level"] != mode.level || entry["protocol"] != "dav" || entry["op"] != strings.ToLower(mode.method) || entry["account"] != acct || entry["status"] != float64(mode.status) {
				t.Fatalf("mutation log context missing: %s", logs.String())
			}
			for _, secret := range []string{"body-secret", "query-secret", "cookie-secret", "header-secret", "etag-secret", pw, r.Header.Get("Authorization")} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("mutation log exposed request contents: %q", secret)
				}
			}
			if mode.status >= 400 {
				if _, present := entry["path"]; present {
					t.Fatalf("failed mutation logged a private resource path: %s", logs.String())
				}
				if strings.Contains(logs.String(), "private-task-uid") || strings.Contains(logs.String(), "private-missing-uid") {
					t.Fatalf("failed mutation logged a private resource identifier: %s", logs.String())
				}
			} else if entry["path"] != path {
				t.Fatalf("successful mutation logging changed: %s", logs.String())
			}
		})
	}
}

func TestDAVUnauthorizedMutationKeepsAuthenticationDiagnostic(t *testing.T) {
	var logs bytes.Buffer
	h := newHandlerWithLogger(t, newFake(), zerolog.New(&logs))
	rr := req(t, h, "PUT", "/caldav/"+acct+"/tasks1/private.ics", "body-secret", false)
	if rr.Code != 401 || !strings.Contains(logs.String(), `"op":"auth"`) || strings.Contains(logs.String(), `"op":"put"`) || strings.Contains(logs.String(), "body-secret") {
		t.Fatalf("unauthorized diagnostic changed: %d %s", rr.Code, logs.String())
	}
}
