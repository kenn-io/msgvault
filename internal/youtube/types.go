// Package youtube archives an explicitly selected owned channel through the Data API.
package youtube

import (
	"strings"
)

const SourceType = "youtube"

func validID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func SourceIdentifier(channelID string) string {
	if !validID(channelID) {
		return ""
	}
	return "https://www.youtube.com/channel/" + channelID
}

// ParseSourceIdentifier accepts only an immutable canonical channel URL.
func ParseSourceIdentifier(identifier string) (channelID string, ok bool) {
	channelID, ok = strings.CutPrefix(identifier, "https://www.youtube.com/channel/")
	if !ok || SourceIdentifier(channelID) != identifier {
		return "", false
	}
	return channelID, true
}
