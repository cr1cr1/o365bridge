package owa

import "time"

// Zone ID conversion lives in internal/bridge (bridge.ToWindowsZone
// / bridge.FromWindowsZone); this file holds only the OWA-specific
// formatting/parsing of zoned date-times.

// fmtZoned formats t as wall-clock time in the given IANA zone
// (no UTC suffix), which is how Exchange interprets Start/End when
// a StartTimeZone is present. Without a usable zone it falls back
// to the UTC format used elsewhere.
func fmtZoned(t time.Time, iana string) string {
	if iana == "" {
		return fmtTime(t)
	}
	if loc, err := time.LoadLocation(iana); err == nil {
		return t.In(loc).Format("2006-01-02T15:04:05")
	}
	return fmtTime(t)
}

// parseZoned parses an Exchange date/time. When the string carries
// no offset and an IANA zone is known, the value is wall time in
// that zone; otherwise the generic parser applies.
func parseZoned(s, iana string) time.Time {
	if iana != "" {
		if loc, err := time.LoadLocation(iana); err == nil {
			for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04:05.9999999"} {
				if t, err := time.ParseInLocation(layout, s, loc); err == nil {
					return t
				}
			}
		}
	}
	return parseTime(s)
}
