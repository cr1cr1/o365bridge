package owa

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestOWACalendarListingLogsIdentityCountsWithoutContents(t *testing.T) {
	first := calendarReadbackItem("cloud-one", "shared-uid")
	second := calendarReadbackItem("cloud-two", "shared-uid")
	old := calendarReadbackItem("cloud-old", "old-uid")
	delete(first, "Recurrence")
	delete(second, "Recurrence")
	old["Start"], old["End"] = "2026-10-14T10:00:00", "2026-10-14T11:00:00"
	for _, item := range []map[string]any{first, second, old} {
		item["Subject"] = "private calendar title"
		item["Body"] = map[string]any{"Value": "private calendar body"}
	}
	var buf owaLogBuffer
	// Normal summaries exclude content; explicit trace separately allows payloads.
	lg := zerolog.New(&buf).Level(zerolog.DebugLevel)
	p := newLoggedFake(t, func(action string, _ map[string]any) any {
		if action == "FindItem" {
			return okBody(map[string]any{"RootFolder": map[string]any{"Items": []any{
				map[string]any{"ItemId": first["ItemId"]}, map[string]any{"ItemId": second["ItemId"]}, map[string]any{"ItemId": old["ItemId"]},
			}}})
		}
		return itemResp(first, second, old)
	}, &lg)
	from := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	events, err := p.ListEvents(context.Background(), "calendar", from, from.Add(24*time.Hour))
	if err != nil || len(events) != 2 {
		t.Fatalf("ListEvents = %+v, %v", events, err)
	}
	logs := buf.String()
	for _, want := range []string{`"op":"list_events"`, `"discovered":3`, `"returned":2`, `"filtered":1`, `"recurring":1`, `"unique_ids":2`, `"unique_uids":1`, `"duplicate_uids":1`} {
		if !strings.Contains(logs, want) {
			t.Errorf("calendar count log missing %s: %s", want, logs)
		}
	}
	for _, private := range []string{"private calendar title", "private calendar body", "shared-uid", "cloud-one", "cloud-two", "token123", "canary"} {
		if strings.Contains(logs, private) {
			t.Errorf("calendar contents/identifiers leaked into count log: %s", logs)
		}
	}
}
