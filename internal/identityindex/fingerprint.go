package identityindex

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// ConversationParticipantRows is the streaming subset of sql.Rows needed to
// hash ordered conversation membership without retaining it in memory.
type ConversationParticipantRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// FingerprintConversationParticipants hashes ordered
// (conversation_id, participant_id) pairs using two fixed-width big-endian
// signed-ID bit patterns per row.
func FingerprintConversationParticipants(rows ConversationParticipantRows) (string, error) {
	hash := sha256.New()
	var encoded [16]byte
	for rows.Next() {
		var conversationID, participantID int64
		if err := rows.Scan(&conversationID, &participantID); err != nil {
			return "", fmt.Errorf("scan conversation participant fingerprint: %w", err)
		}
		// IDs are non-negative database keys. Encoding their int64 bit pattern
		// keeps the fingerprint fixed-width and unambiguous.
		if conversationID < 0 || participantID < 0 {
			return "", fmt.Errorf(
				"fingerprint conversation participants: negative IDs (%d, %d)",
				conversationID,
				participantID,
			)
		}
		binary.BigEndian.PutUint64(encoded[:8], uint64(conversationID))
		binary.BigEndian.PutUint64(encoded[8:], uint64(participantID))
		_, _ = hash.Write(encoded[:])
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate conversation participant fingerprint: %w", err)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

// FingerprintConversationTypes hashes ordered (conversation_id, type) rows:
// the ID's fixed-width big-endian bit pattern followed by the type's byte
// length and bytes, so variable-length values can never alias across rows.
func FingerprintConversationTypes(rows ConversationParticipantRows) (string, error) {
	hash := sha256.New()
	var encoded [16]byte
	for rows.Next() {
		var conversationID int64
		var conversationType string
		if err := rows.Scan(&conversationID, &conversationType); err != nil {
			return "", fmt.Errorf("scan conversation type fingerprint: %w", err)
		}
		if conversationID < 0 {
			return "", fmt.Errorf(
				"fingerprint conversation types: negative ID %d",
				conversationID,
			)
		}
		binary.BigEndian.PutUint64(encoded[:8], uint64(conversationID))
		binary.BigEndian.PutUint64(encoded[8:], uint64(len(conversationType)))
		_, _ = hash.Write(encoded[:])
		_, _ = hash.Write([]byte(conversationType))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate conversation type fingerprint: %w", err)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

// FingerprintConversationMetadata hashes ordered
// (conversation_id, type, title) rows. Length-prefixing both strings keeps the
// variable-width values unambiguous, including when either contains a value
// that could otherwise act as a separator.
func FingerprintConversationMetadata(rows ConversationParticipantRows) (string, error) {
	hash := sha256.New()
	var encoded [16]byte
	for rows.Next() {
		var conversationID int64
		var conversationType, title string
		if err := rows.Scan(&conversationID, &conversationType, &title); err != nil {
			return "", fmt.Errorf("scan conversation metadata fingerprint: %w", err)
		}
		if conversationID < 0 {
			return "", fmt.Errorf(
				"fingerprint conversation metadata: negative ID %d",
				conversationID,
			)
		}
		binary.BigEndian.PutUint64(encoded[:8], uint64(conversationID))
		binary.BigEndian.PutUint64(encoded[8:], uint64(len(conversationType)))
		_, _ = hash.Write(encoded[:])
		_, _ = hash.Write([]byte(conversationType))
		binary.BigEndian.PutUint64(encoded[:8], uint64(len(title)))
		_, _ = hash.Write(encoded[:8])
		_, _ = hash.Write([]byte(title))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate conversation metadata fingerprint: %w", err)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}
