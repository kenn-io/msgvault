package identityindex

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/textutil"
)

// FingerprintRows is the streaming subset of sql.Rows needed to
// hash ordered records without retaining them in memory.
type FingerprintRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// FingerprintConversationParticipants hashes ordered
// (conversation_id, participant_id) pairs using two fixed-width big-endian
// signed-ID bit patterns per row.
func FingerprintConversationParticipants(rows FingerprintRows) (string, error) {
	var encoded [16]byte
	return fingerprintRows(rows, "conversation participant", func(h hash.Hash) error {
		var conversationID, participantID int64
		if err := rows.Scan(&conversationID, &participantID); err != nil {
			return fmt.Errorf("scan conversation participant fingerprint: %w", err)
		}
		// IDs are non-negative database keys. Encoding their int64 bit pattern
		// keeps the fingerprint fixed-width and unambiguous.
		if conversationID < 0 || participantID < 0 {
			return fmt.Errorf(
				"fingerprint conversation participants: negative IDs (%d, %d)",
				conversationID,
				participantID,
			)
		}
		binary.BigEndian.PutUint64(encoded[:8], uint64(conversationID))
		binary.BigEndian.PutUint64(encoded[8:], uint64(participantID))
		_, _ = h.Write(encoded[:])

		return nil
	})
}

// FingerprintConversationTypes hashes ordered (conversation_id, type) rows:
// the ID's fixed-width big-endian bit pattern followed by the type's byte
// length and bytes, so variable-length values can never alias across rows.
func FingerprintConversationTypes(rows FingerprintRows) (string, error) {
	var encoded [16]byte
	return fingerprintRows(rows, "conversation type", func(h hash.Hash) error {
		var conversationID int64
		var conversationType string
		if err := rows.Scan(&conversationID, &conversationType); err != nil {
			return fmt.Errorf("scan conversation type fingerprint: %w", err)
		}
		if conversationID < 0 {
			return fmt.Errorf(
				"fingerprint conversation types: negative ID %d",
				conversationID,
			)
		}
		binary.BigEndian.PutUint64(encoded[:8], uint64(conversationID))
		binary.BigEndian.PutUint64(encoded[8:], uint64(len(conversationType)))
		_, _ = h.Write(encoded[:])
		_, _ = h.Write([]byte(conversationType))

		return nil
	})
}

// FingerprintConversationMetadata hashes ordered
// (conversation_id, type, title) rows. Length-prefixing both strings keeps the
// variable-width values unambiguous, including when either contains a value
// that could otherwise act as a separator. Normalize invalid UTF-8 before
// hashing so raw SQLite rows and repaired cache snapshots agree.
func FingerprintConversationMetadata(rows FingerprintRows) (string, error) {
	var encoded [16]byte
	return fingerprintRows(rows, "conversation metadata", func(h hash.Hash) error {
		var conversationID int64
		var conversationType, title string
		if err := rows.Scan(&conversationID, &conversationType, &title); err != nil {
			return fmt.Errorf("scan conversation metadata fingerprint: %w", err)
		}
		if conversationID < 0 {
			return fmt.Errorf(
				"fingerprint conversation metadata: negative ID %d",
				conversationID,
			)
		}
		if !utf8.ValidString(conversationType) {
			conversationType = textutil.SanitizeUTF8(conversationType)
		}
		if !utf8.ValidString(title) {
			title = textutil.SanitizeUTF8(title)
		}
		binary.BigEndian.PutUint64(encoded[:8], uint64(conversationID))
		binary.BigEndian.PutUint64(encoded[8:], uint64(len(conversationType)))
		_, _ = h.Write(encoded[:])
		_, _ = h.Write([]byte(conversationType))
		binary.BigEndian.PutUint64(encoded[:8], uint64(len(title)))
		_, _ = h.Write(encoded[:8])
		_, _ = h.Write([]byte(title))

		return nil
	})
}

// FingerprintMessageIDs hashes an ordered set of message IDs.
func FingerprintMessageIDs(rows FingerprintRows) (string, error) {
	var encoded [8]byte
	return fingerprintRows(rows, "message", func(h hash.Hash) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan message fingerprint: %w", err)
		}
		if id < 0 {
			return fmt.Errorf("fingerprint message: negative ID %d", id)
		}
		binary.BigEndian.PutUint64(encoded[:], uint64(id))
		_, _ = h.Write(encoded[:])

		return nil
	})
}

// fingerprintRows owns iteration and hash finalization; encoders own scans and validation.
func fingerprintRows(rows FingerprintRows, kind string, encode func(hash.Hash) error) (string, error) {
	h := sha256.New()
	for rows.Next() {
		if err := encode(h); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate %s fingerprint: %w", kind, err)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}
