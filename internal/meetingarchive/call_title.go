package meetingarchive

import "strings"

// CallTitle names a phone call by the other party: "Call from" the caller on
// an inbound call, "Call to" the callee otherwise, or fallback when the other
// party's number is unknown.
func CallTitle(fallback, other string, inbound bool) string {
	if other = strings.TrimSpace(other); other == "" {
		return fallback
	}
	if inbound {
		return "Call from " + other
	}
	return "Call to " + other
}
