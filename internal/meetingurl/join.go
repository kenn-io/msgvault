// Package meetingurl recognizes meeting provider access points.
package meetingurl

import (
	"net/url"
	"strings"
)

// KnownJoinURL preserves the usable provider URL, including passcodes and
// context parameters. Join-key normalization belongs to the link projection.
func KnownJoinURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "meet.google.com", host == "cal.com",
		host == "teams.microsoft.com", host == "teams.live.com",
		strings.HasSuffix(host, ".teams.microsoft.us"),
		host == "zoom.us", strings.HasSuffix(host, ".zoom.us"),
		host == "zoomgov.com", strings.HasSuffix(host, ".zoomgov.com"):
		return trimmed
	}
	return ""
}
