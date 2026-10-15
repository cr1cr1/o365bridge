package bridge

import (
	"testing"
	"time"
)

func TestFromWindowsZoneLocalTimeAcrossDST(t *testing.T) {
	for _, tt := range []struct {
		name  string
		win   string
		month time.Month
		want  string
	}{
		{"Eastern/winter", "Eastern Standard Time", time.January, "2026-01-15T14:00:00Z"},
		{"Eastern/summer", "Eastern Standard Time", time.July, "2026-07-15T13:00:00Z"},
		{"MiddleEast/winter", "Middle East Standard Time", time.January, "2026-01-15T07:00:00Z"},
		{"MiddleEast/summer", "Middle East Standard Time", time.July, "2026-07-15T06:00:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := time.LoadLocation(FromWindowsZone(tt.win))
			if err != nil {
				t.Fatal(err)
			}
			got := time.Date(2026, tt.month, 15, 9, 0, 0, 0, loc).UTC().Format(time.RFC3339)
			if got != tt.want {
				t.Errorf("09:00 in %s = %s, want %s", loc, got, tt.want)
			}
		})
	}
}

func TestFromWindowsZoneCanonical(t *testing.T) {
	// Territory 001 expectations from Unicode CLDR release-47, commit
	// 2ef784e3a4168bc2a43cd1b5b9839b6636f5899c:
	// https://raw.githubusercontent.com/unicode-org/cldr/2ef784e3a4168bc2a43cd1b5b9839b6636f5899c/common/supplemental/windowsZones.xml
	for _, tt := range []struct{ win, iana string }{
		{"AUS Eastern Standard Time", "Australia/Sydney"},
		{"Arab Standard Time", "Asia/Riyadh"},
		{"Central America Standard Time", "America/Guatemala"},
		{"Central Asia Standard Time", "Asia/Bishkek"},
		{"Central Europe Standard Time", "Europe/Budapest"},
		{"Central Pacific Standard Time", "Pacific/Guadalcanal"},
		{"China Standard Time", "Asia/Shanghai"},
		{"E. Africa Standard Time", "Africa/Nairobi"},
		{"E. Europe Standard Time", "Europe/Chisinau"},
		{"Eastern Standard Time", "America/New_York"},
		{"FLE Standard Time", "Europe/Kiev"},
		{"GMT Standard Time", "Europe/London"},
		{"GTB Standard Time", "Europe/Bucharest"},
		{"Greenwich Standard Time", "Atlantic/Reykjavik"},
		{"Magallanes Standard Time", "America/Punta_Arenas"},
		{"Middle East Standard Time", "Asia/Beirut"},
		{"Mountain Standard Time (Mexico)", "America/Mazatlan"},
		{"Romance Standard Time", "Europe/Paris"},
		{"Russia Time Zone 11", "Asia/Kamchatka"},
		{"SA Western Standard Time", "America/La_Paz"},
		{"Singapore Standard Time", "Asia/Singapore"},
		{"South Africa Standard Time", "Africa/Johannesburg"},
		{"Tokyo Standard Time", "Asia/Tokyo"},
		{"UTC+12", "Etc/GMT-12"},
		{"UTC-02", "Etc/GMT+2"},
		{"UTC-11", "Etc/GMT+11"},
		{"Ulaanbaatar Standard Time", "Asia/Ulaanbaatar"},
		{"W. Central Africa Standard Time", "Africa/Lagos"},
		{"W. Europe Standard Time", "Europe/Berlin"},
		{"West Asia Standard Time", "Asia/Tashkent"},
		{"West Pacific Standard Time", "Pacific/Port_Moresby"},
		// Preserve existing equivalent spellings, not CLDR's older IANA links.
		{"Argentina Standard Time", "America/Argentina/Buenos_Aires"},
		{"India Standard Time", "Asia/Kolkata"},
		{"Myanmar Standard Time", "Asia/Yangon"},
		{"Nepal Standard Time", "Asia/Kathmandu"},
		{"US Eastern Standard Time", "America/Indiana/Indianapolis"},
		{"UTC", "Etc/GMT"},
		// Legacy Windows IDs absent from this CLDR revision retain their fallback.
		{"Kamchatka Standard Time", "Asia/Kamchatka"},
		{"Tunisia Standard Time", "Africa/Tunis"},
	} {
		t.Run(tt.win, func(t *testing.T) {
			if got := FromWindowsZone(tt.win); got != tt.iana {
				t.Errorf("FromWindowsZone(%q) = %q, want %q", tt.win, got, tt.iana)
			}
			if _, err := time.LoadLocation(FromWindowsZone(tt.win)); err != nil {
				t.Errorf("canonical location for %q: %v", tt.win, err)
			}
		})
	}
}

func TestFromWindowsZoneAllSupportedIDsUsableAndDeterministic(t *testing.T) {
	for i := 0; i < 5; i++ {
		rebuilt := buildReverse()
		for _, win := range ianaToWindows {
			got := FromWindowsZone(win)
			if got == "" || got == win {
				t.Fatalf("supported Windows ID %q has no reverse mapping", win)
			}
			if rebuilt[win] != got {
				t.Fatalf("nondeterministic mapping for %q: %q vs %q", win, rebuilt[win], got)
			}
			if _, err := time.LoadLocation(got); err != nil {
				t.Fatalf("supported Windows ID %q maps to unusable %q: %v", win, got, err)
			}
		}
	}
}

func TestWindowsZoneRoundTrip(t *testing.T) {
	// Forward aliases are approximate; only assert roundtrips for consistent
	// pairings, not e.g. GTB whose canonical Bucharest forward alias differs.
	for _, tt := range []struct{ win, iana string }{
		{"Eastern Standard Time", "America/New_York"},
		{"Middle East Standard Time", "Asia/Beirut"},
		{"E. Europe Standard Time", "Europe/Chisinau"},
		{"W. Europe Standard Time", "Europe/Berlin"},
		{"Tokyo Standard Time", "Asia/Tokyo"},
		{"US Eastern Standard Time", "America/Indiana/Indianapolis"},
		{"UTC", "Etc/GMT"},
	} {
		if got := FromWindowsZone(ToWindowsZone(tt.iana)); got != tt.iana {
			t.Errorf("IANA roundtrip for %q = %q", tt.iana, got)
		}
		if got := ToWindowsZone(FromWindowsZone(tt.win)); got != tt.win {
			t.Errorf("Windows roundtrip for %q = %q", tt.win, got)
		}
	}
}

func TestWindowsZonePassThroughAndUTC(t *testing.T) {
	for _, name := range []string{"", "Unknown Windows Zone", "America/New_York"} {
		if got := FromWindowsZone(name); got != name {
			t.Errorf("FromWindowsZone(%q) = %q, want passthrough", name, got)
		}
	}
	for _, name := range []string{"", "Unknown Windows Zone", "Eastern Standard Time"} {
		if got := ToWindowsZone(name); got != name {
			t.Errorf("ToWindowsZone(%q) = %q, want passthrough", name, got)
		}
	}
	for _, name := range []string{"UTC", "Etc/UTC", "Etc/GMT", "GMT"} {
		if got := ToWindowsZone(name); got != "UTC" {
			t.Errorf("ToWindowsZone(%q) = %q, want UTC", name, got)
		}
	}
	loc, err := time.LoadLocation(FromWindowsZone("UTC"))
	if err != nil {
		t.Fatal(err)
	}
	for _, month := range []time.Month{time.January, time.July} {
		_, offset := time.Date(2026, month, 15, 9, 0, 0, 0, loc).Zone()
		if offset != 0 {
			t.Errorf("UTC offset in %s = %d, want 0", month, offset)
		}
	}
}
