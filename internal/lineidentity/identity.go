// Package lineidentity encodes channel-scoped LINE participant identifiers.
package lineidentity

// UserIdentifier qualifies an opaque ID with a validated decimal channel ID.
func UserIdentifier(channelID, userID string) string {
	return "channel:" + channelID + ":user:" + userID
}
