// Package entrykey parses the stable identities used by Explore and Saved Views.
package entrykey

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrInvalid = errors.New("invalid entry key")

type Kind string

const (
	Message      Kind = "message"
	Conversation Kind = "conversation"
)

type Ref struct {
	SourceID        int64
	Kind            Kind
	SourceMessageID string
	ConversationID  int64
}

func Parse(key string) (Ref, error) {
	parts := strings.SplitN(key, ":", 4)
	invalid := func() (Ref, error) { return Ref{}, fmt.Errorf("%w: %q", ErrInvalid, key) }
	if len(parts) != 4 || parts[0] != "source" || parts[3] == "" {
		return invalid()
	}
	sid, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || sid <= 0 || strconv.FormatInt(sid, 10) != parts[1] {
		return invalid()
	}
	ref := Ref{SourceID: sid, Kind: Kind(parts[2])}
	switch ref.Kind {
	case Message:
		ref.SourceMessageID = parts[3]
	case Conversation:
		id, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != parts[3] {
			return invalid()
		}
		ref.ConversationID = id
	default:
		return invalid()
	}
	return ref, nil
}

// MessageKey renders an email-like entry, falling back to its internal ID.
func MessageKey(sourceID, messageID int64, providerID string) string {
	if providerID == "" {
		providerID = strconv.FormatInt(messageID, 10)
	}
	return "source:" + strconv.FormatInt(sourceID, 10) + ":message:" + providerID
}

// ConversationKey renders a chat conversation entry.
func ConversationKey(sourceID, conversationID int64) string {
	return "source:" + strconv.FormatInt(sourceID, 10) + ":conversation:" + strconv.FormatInt(conversationID, 10)
}
