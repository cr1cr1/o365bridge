package jmapadapter

import (
	"testing"

	"github.com/cr1cr1/o365bridge/jmap/calendars"
)

func TestCategoryUnawareJMAPEventConversionPreservesProviderCategories(t *testing.T) {
	ev, err := eventFromJMAP(calendars.CalendarEvent{Title: "Edited through JMAP", Start: "2026-10-15T10:00:00", Duration: "PT1H"})
	if err != nil {
		t.Fatalf("conversion=%+v", err)
	}
	if ev.Categories != nil {
		t.Fatalf("category-unaware JMAP conversion must leave categories unsupplied, not clear them: %#v", ev.Categories)
	}
}
