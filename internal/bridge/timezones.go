package bridge

import "sort"

// Exchange-flavoured APIs (OWA service.svc, and Graph when it
// returns Outlook-created items) use Windows time zone IDs, while
// downstream clients speak IANA names (Europe/Bucharest). An IANA
// name sent verbatim to Exchange is rejected with "A time zone
// with the specified ID could not be found."
//
// The map below holds the standard CLDR golden-zone pairings.
// Unrecognised names pass through unchanged (a name that is
// already a Windows ID therefore keeps working).

var ianaToWindows = map[string]string{
	"Africa/Abidjan":       "Greenwich Standard Time",
	"Africa/Accra":         "Greenwich Standard Time",
	"Africa/Addis_Ababa":   "E. Africa Standard Time",
	"Africa/Algiers":       "W. Central Africa Standard Time",
	"Africa/Asmara":        "E. Africa Standard Time",
	"Africa/Cairo":         "Egypt Standard Time",
	"Africa/Casablanca":    "Morocco Standard Time",
	"Africa/Dar_es_Salaam": "E. Africa Standard Time",
	"Africa/Djibouti":      "E. Africa Standard Time",
	"Africa/Harare":        "South Africa Standard Time",
	"Africa/Johannesburg":  "South Africa Standard Time",
	"Africa/Kampala":       "E. Africa Standard Time",
	"Africa/Khartoum":      "Sudan Standard Time",
	"Africa/Lagos":         "W. Central Africa Standard Time",
	"Africa/Luanda":        "W. Central Africa Standard Time",
	"Africa/Lusaka":        "South Africa Standard Time",
	"Africa/Maputo":        "South Africa Standard Time",
	"Africa/Mogadishu":     "E. Africa Standard Time",
	"Africa/Nairobi":       "E. Africa Standard Time",
	"Africa/Tripoli":       "Libya Standard Time",
	"Africa/Tunis":         "Tunisia Standard Time",
	"Africa/Windhoek":      "Namibia Standard Time",

	"America/Anchorage":              "Alaskan Standard Time",
	"America/Argentina/Buenos_Aires": "Argentina Standard Time",
	"America/Asuncion":               "Paraguay Standard Time",
	"America/Atikokan":               "Eastern Standard Time",
	"America/Bahia":                  "Bahia Standard Time",
	"America/Bogota":                 "SA Pacific Standard Time",
	"America/Cancun":                 "Eastern Standard Time (Mexico)",
	"America/Caracas":                "Venezuela Standard Time",
	"America/Cayenne":                "SA Eastern Standard Time",
	"America/Chicago":                "Central Standard Time",
	"America/Chihuahua":              "Mountain Standard Time (Mexico)",
	"America/Costa_Rica":             "Central America Standard Time",
	"America/Cuiaba":                 "Central Brazilian Standard Time",
	"America/Denver":                 "Mountain Standard Time",
	"America/Detroit":                "Eastern Standard Time",
	"America/Edmonton":               "Mountain Standard Time",
	"America/El_Salvador":            "Central America Standard Time",
	"America/Grand_Turk":             "Turks And Caicos Standard Time",
	"America/Guatemala":              "Central America Standard Time",
	"America/Guayaquil":              "SA Pacific Standard Time",
	"America/Guyana":                 "SA Western Standard Time",
	"America/Halifax":                "Atlantic Standard Time",
	"America/Havana":                 "Cuba Standard Time",
	"America/Indiana/Indianapolis":   "US Eastern Standard Time",
	"America/Jamaica":                "SA Pacific Standard Time",
	"America/La_Paz":                 "SA Western Standard Time",
	"America/Lima":                   "SA Pacific Standard Time",
	"America/Los_Angeles":            "Pacific Standard Time",
	"America/Managua":                "Central America Standard Time",
	"America/Manaus":                 "SA Western Standard Time",
	"America/Mazatlan":               "Mountain Standard Time (Mexico)",
	"America/Mexico_City":            "Central Standard Time (Mexico)",
	"America/Monterrey":              "Central Standard Time (Mexico)",
	"America/Montevideo":             "Montevideo Standard Time",
	"America/New_York":               "Eastern Standard Time",
	"America/Nome":                   "Alaskan Standard Time",
	"America/Noronha":                "UTC-02",
	"America/Panama":                 "SA Pacific Standard Time",
	"America/Phoenix":                "US Mountain Standard Time",
	"America/Port-au-Prince":         "Haiti Standard Time",
	"America/Puerto_Rico":            "SA Western Standard Time",
	"America/Recife":                 "SA Eastern Standard Time",
	"America/Regina":                 "Canada Central Standard Time",
	"America/Santiago":               "Pacific SA Standard Time",
	"America/Santo_Domingo":          "SA Western Standard Time",
	"America/Sao_Paulo":              "E. South America Standard Time",
	"America/St_Johns":               "Newfoundland Standard Time",
	"America/Tegucigalpa":            "Central America Standard Time",
	"America/Tijuana":                "Pacific Standard Time (Mexico)",
	"America/Toronto":                "Eastern Standard Time",
	"America/Vancouver":              "Pacific Standard Time",
	"America/Whitehorse":             "Yukon Standard Time",
	"America/Winnipeg":               "Central Standard Time",

	"Antarctica/Palmer": "Magallanes Standard Time",
	"Antarctica/Syowa":  "E. Africa Standard Time",

	"Asia/Almaty":        "Central Asia Standard Time",
	"Asia/Amman":         "Middle East Standard Time",
	"Asia/Anadyr":        "Russia Time Zone 11",
	"Asia/Aqtobe":        "West Asia Standard Time",
	"Asia/Ashgabat":      "West Asia Standard Time",
	"Asia/Baghdad":       "Arab Standard Time",
	"Asia/Baku":          "Azerbaijan Standard Time",
	"Asia/Bangkok":       "SE Asia Standard Time",
	"Asia/Barnaul":       "Altai Standard Time",
	"Asia/Beirut":        "Middle East Standard Time",
	"Asia/Bishkek":       "Central Asia Standard Time",
	"Asia/Brunei":        "Singapore Standard Time",
	"Asia/Chita":         "Transbaikal Standard Time",
	"Asia/Choibalsan":    "Ulaanbaatar Standard Time",
	"Asia/Colombo":       "Sri Lanka Standard Time",
	"Asia/Damascus":      "Syria Standard Time",
	"Asia/Dhaka":         "Bangladesh Standard Time",
	"Asia/Dili":          "Tokyo Standard Time",
	"Asia/Dubai":         "Arabian Standard Time",
	"Asia/Hebron":        "West Bank Standard Time",
	"Asia/Ho_Chi_Minh":   "SE Asia Standard Time",
	"Asia/Hong_Kong":     "China Standard Time",
	"Asia/Irkutsk":       "North Asia East Standard Time",
	"Asia/Jakarta":       "SE Asia Standard Time",
	"Asia/Jayapura":      "Tokyo Standard Time",
	"Asia/Jerusalem":     "Israel Standard Time",
	"Asia/Kabul":         "Afghanistan Standard Time",
	"Asia/Kamchatka":     "Kamchatka Standard Time",
	"Asia/Karachi":       "Pakistan Standard Time",
	"Asia/Kathmandu":     "Nepal Standard Time",
	"Asia/Kolkata":       "India Standard Time",
	"Asia/Krasnoyarsk":   "North Asia Standard Time",
	"Asia/Kuala_Lumpur":  "Singapore Standard Time",
	"Asia/Kuching":       "Singapore Standard Time",
	"Asia/Kuwait":        "Arab Standard Time",
	"Asia/Magadan":       "Magadan Standard Time",
	"Asia/Makassar":      "Singapore Standard Time",
	"Asia/Manila":        "Singapore Standard Time",
	"Asia/Muscat":        "Arabian Standard Time",
	"Asia/Nicosia":       "GTB Standard Time",
	"Asia/Novosibirsk":   "N. Central Asia Standard Time",
	"Asia/Omsk":          "Omsk Standard Time",
	"Asia/Oral":          "West Asia Standard Time",
	"Asia/Phnom_Penh":    "SE Asia Standard Time",
	"Asia/Pontianak":     "SE Asia Standard Time",
	"Asia/Pyongyang":     "North Korea Standard Time",
	"Asia/Qatar":         "Arab Standard Time",
	"Asia/Qyzylorda":     "Qyzylorda Standard Time",
	"Asia/Riyadh":        "Arab Standard Time",
	"Asia/Sakhalin":      "Sakhalin Standard Time",
	"Asia/Samarkand":     "West Asia Standard Time",
	"Asia/Seoul":         "Korea Standard Time",
	"Asia/Shanghai":      "China Standard Time",
	"Asia/Singapore":     "Singapore Standard Time",
	"Asia/Srednekolymsk": "Russia Time Zone 10",
	"Asia/Taipei":        "Taipei Standard Time",
	"Asia/Tashkent":      "West Asia Standard Time",
	"Asia/Tbilisi":       "Georgian Standard Time",
	"Asia/Tehran":        "Iran Standard Time",
	"Asia/Thimphu":       "Bangladesh Standard Time",
	"Asia/Tokyo":         "Tokyo Standard Time",
	"Asia/Tomsk":         "Tomsk Standard Time",
	"Asia/Ulaanbaatar":   "Ulaanbaatar Standard Time",
	"Asia/Urumqi":        "Central Asia Standard Time",
	"Asia/Vientiane":     "SE Asia Standard Time",
	"Asia/Vladivostok":   "Vladivostok Standard Time",
	"Asia/Yakutsk":       "Yakutsk Standard Time",
	"Asia/Yangon":        "Myanmar Standard Time",
	"Asia/Yekaterinburg": "Ekaterinburg Standard Time",
	"Asia/Yerevan":       "Caucasus Standard Time",

	"Atlantic/Azores":        "Azores Standard Time",
	"Atlantic/Bermuda":       "Atlantic Standard Time",
	"Atlantic/Cape_Verde":    "Cape Verde Standard Time",
	"Atlantic/Faroe":         "GMT Standard Time",
	"Atlantic/Reykjavik":     "Greenwich Standard Time",
	"Atlantic/South_Georgia": "UTC-02",
	"Atlantic/St_Helena":     "Greenwich Standard Time",
	"Atlantic/Stanley":       "SA Eastern Standard Time",

	"Australia/Adelaide":    "Cen. Australia Standard Time",
	"Australia/Brisbane":    "E. Australia Standard Time",
	"Australia/Broken_Hill": "Cen. Australia Standard Time",
	"Australia/Darwin":      "AUS Central Standard Time",
	"Australia/Hobart":      "Tasmania Standard Time",
	"Australia/Lord_Howe":   "Lord Howe Standard Time",
	"Australia/Melbourne":   "AUS Eastern Standard Time",
	"Australia/Perth":       "W. Australia Standard Time",
	"Australia/Sydney":      "AUS Eastern Standard Time",

	"Europe/Amsterdam":   "W. Europe Standard Time",
	"Europe/Astrakhan":   "Astrakhan Standard Time",
	"Europe/Athens":      "GTB Standard Time",
	"Europe/Belgrade":    "Central Europe Standard Time",
	"Europe/Berlin":      "W. Europe Standard Time",
	"Europe/Bratislava":  "Central Europe Standard Time",
	"Europe/Brussels":    "Romance Standard Time",
	"Europe/Bucharest":   "E. Europe Standard Time",
	"Europe/Budapest":    "Central Europe Standard Time",
	"Europe/Chisinau":    "E. Europe Standard Time",
	"Europe/Copenhagen":  "Romance Standard Time",
	"Europe/Dublin":      "GMT Standard Time",
	"Europe/Helsinki":    "FLE Standard Time",
	"Europe/Istanbul":    "Turkey Standard Time",
	"Europe/Kaliningrad": "Kaliningrad Standard Time",
	"Europe/Kiev":        "FLE Standard Time",
	"Europe/Kyiv":        "FLE Standard Time",
	"Europe/Lisbon":      "GMT Standard Time",
	"Europe/Ljubljana":   "Central Europe Standard Time",
	"Europe/London":      "GMT Standard Time",
	"Europe/Madrid":      "Romance Standard Time",
	"Europe/Minsk":       "Belarus Standard Time",
	"Europe/Moscow":      "Russian Standard Time",
	"Europe/Oslo":        "W. Europe Standard Time",
	"Europe/Paris":       "Romance Standard Time",
	"Europe/Prague":      "Central Europe Standard Time",
	"Europe/Riga":        "FLE Standard Time",
	"Europe/Rome":        "W. Europe Standard Time",
	"Europe/Samara":      "Russia Time Zone 3",
	"Europe/Saratov":     "Saratov Standard Time",
	"Europe/Sofia":       "FLE Standard Time",
	"Europe/Stockholm":   "W. Europe Standard Time",
	"Europe/Tallinn":     "FLE Standard Time",
	"Europe/Tirane":      "Central Europe Standard Time",
	"Europe/Ulyanovsk":   "Astrakhan Standard Time",
	"Europe/Vienna":      "W. Europe Standard Time",
	"Europe/Vilnius":     "FLE Standard Time",
	"Europe/Volgograd":   "Volgograd Standard Time",
	"Europe/Warsaw":      "Central European Standard Time",
	"Europe/Zagreb":      "Central European Standard Time",
	"Europe/Zurich":      "W. Europe Standard Time",

	"Indian/Mauritius": "Mauritius Standard Time",

	"Pacific/Apia":         "Samoa Standard Time",
	"Pacific/Auckland":     "New Zealand Standard Time",
	"Pacific/Chatham":      "Chatham Islands Standard Time",
	"Pacific/Chuuk":        "West Pacific Standard Time",
	"Pacific/Easter":       "Easter Island Standard Time",
	"Pacific/Efate":        "Central Pacific Standard Time",
	"Pacific/Fiji":         "Fiji Standard Time",
	"Pacific/Guadalcanal":  "Central Pacific Standard Time",
	"Pacific/Guam":         "West Pacific Standard Time",
	"Pacific/Honolulu":     "Hawaiian Standard Time",
	"Pacific/Kiritimati":   "Line Islands Standard Time",
	"Pacific/Kosrae":       "Central Pacific Standard Time",
	"Pacific/Majuro":       "UTC+12",
	"Pacific/Nauru":        "UTC+12",
	"Pacific/Niue":         "UTC-11",
	"Pacific/Noumea":       "Central Pacific Standard Time",
	"Pacific/Pago_Pago":    "UTC-11",
	"Pacific/Palau":        "Tokyo Standard Time",
	"Pacific/Pohnpei":      "Central Pacific Standard Time",
	"Pacific/Port_Moresby": "West Pacific Standard Time",
	"Pacific/Rarotonga":    "Hawaiian Standard Time",
	"Pacific/Tahiti":       "Hawaiian Standard Time",
	"Pacific/Tarawa":       "UTC+12",
	"Pacific/Tongatapu":    "Tonga Standard Time",

	"Etc/UTC": "UTC",
	"UTC":     "UTC",
	"Etc/GMT": "UTC",
	"GMT":     "UTC",
}

var windowsToIANA = buildReverse()

func buildReverse() map[string]string {
	// Several IANA zones share one Windows ID, so the reverse
	// mapping is inherently approximate. Iterate in sorted order so
	// the chosen canonical zone is at least deterministic across
	// processes (Go map iteration order is random).
	keys := make([]string, 0, len(ianaToWindows))
	for iana := range ianaToWindows {
		keys = append(keys, iana)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(ianaToWindows))
	for _, iana := range keys {
		win := ianaToWindows[iana]
		if _, ok := out[win]; !ok {
			out[win] = iana
		}
	}
	return out
}

// ToWindowsZone converts an IANA zone name to the Windows zone
// ID Exchange requires. Anything else (already a Windows ID,
// "UTC", unknown) passes through unchanged.
func ToWindowsZone(iana string) string {
	if win, ok := ianaToWindows[iana]; ok {
		return win
	}
	return iana
}

// FromWindowsZone converts a Windows zone ID back to an IANA name
// for downstream clients. Unknown IDs pass through unchanged.
func FromWindowsZone(win string) string {
	if iana, ok := windowsToIANA[win]; ok {
		return iana
	}
	return win
}
