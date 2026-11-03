package meta

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

// DecodeMentionChange accepts only the two Instagram mention identifiers.
func DecodeMentionChange(entryID string, value []byte) (MentionEvent, error) {
	var wire struct {
		MediaID   string         `json:"media_id"`
		CommentID jsontext.Value `json:"comment_id"`
	}
	if err := json.Unmarshal(value, &wire, json.RejectUnknownMembers(true)); err != nil {
		return MentionEvent{}, errors.New("invalid Instagram mention change")
	}
	var commentID string
	if len(wire.CommentID) > 0 && (json.Unmarshal(wire.CommentID, &commentID) != nil || !ValidAccountID(commentID)) {
		return MentionEvent{}, errors.New("invalid Instagram mention comment identity")
	}
	if !ValidAccountID(entryID) || !ValidAccountID(wire.MediaID) {
		return MentionEvent{}, errors.New("invalid Instagram mention change identity")
	}
	return MentionEvent{MediaID: wire.MediaID, CommentID: commentID}, nil
}
