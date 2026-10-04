package kataevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strings"
	"unicode/utf8"
)

// Canonicalize validates every identity and copies nested payloads so digest
// normalization cannot mutate the caller's reference.
func Canonicalize(r Reference) (Reference, error) {
	invalid := func() (Reference, error) { return Reference{}, ErrInvalidReference }
	if r.Version != Version || r.MessageID < 1 ||
		!validIdentity(r.ArchiveUID) || !validIdentity(r.SourceType) ||
		!validIdentity(r.SourceIdentifier) || !validIdentity(r.SourceMessageID) {
		return invalid()
	}
	switch r.Kind {
	case "message":
		if r.Message == nil || r.DocumentChunk != nil || r.AttachmentID != 0 || r.OccurrenceKey != "" {
			return invalid()
		}
		p := *r.Message
		r.Message = &p
		if !normalizeDigest(&p.BodySHA256) {
			return invalid()
		}
	case "document_chunk":
		if r.DocumentChunk == nil || r.Message != nil || r.AttachmentID < 1 || !validIdentity(r.OccurrenceKey) {
			return invalid()
		}
		p := *r.DocumentChunk
		r.DocumentChunk = &p
		if !validIdentity(p.ExtractionID) || !validIdentity(p.ChunkKey) ||
			!normalizeDigest(&p.CanonicalBlobHash) || !normalizeDigest(&p.ManifestChecksum) || !normalizeDigest(&p.ChunkChecksum) {
			return invalid()
		}
	default:
		return invalid()
	}
	start, end := r.Range()
	if start < 0 || end <= start || end-start > MaxChars {
		return invalid()
	}
	return r, nil
}

// ID binds content, occurrence and range together, excluding presentation.
// r must already be canonical.
func ID(r Reference) string {
	return Digest(r)
}

// Digest is the hex SHA-256 of value's deterministic JSON, a stable identity
// for references and the requests that cite them.
func Digest(value any) string {
	data, _ := json.Marshal(value, json.Deterministic(true))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validIdentity(s string) bool {
	return s != "" && len(s) <= 4096 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func normalizeDigest(s *string) bool {
	if len(*s) != 64 {
		return false
	}
	if _, err := hex.DecodeString(*s); err != nil {
		return false
	}
	*s = strings.ToLower(*s)
	return true
}

// passage names where a citation sits, leaving out the content and
// extraction hashes that change when the source is synced or reprocessed, and
// the attachment row ID, which message repair can reassign; the occurrence
// key names the file instead.
type passage struct {
	Kind, ArchiveUID                              string
	MessageID                                     int64
	SourceType, SourceIdentifier, SourceMessageID string
	OccurrenceKey, ChunkKey                       string
	Start, End                                    int
}

// PassageID is the one identity msgvault uses to decide whether two
// citations quote the same thing: where the text sits plus its words.
func PassageID(ref Reference, words string) string {
	return Digest([]string{PassageLocation(ref), words})
}

// PassageLocation identifies where ref sits, whatever text is there now.
func PassageLocation(ref Reference) string {
	p := passage{Kind: ref.Kind, ArchiveUID: ref.ArchiveUID, MessageID: ref.MessageID, SourceType: ref.SourceType, SourceIdentifier: ref.SourceIdentifier,
		SourceMessageID: ref.SourceMessageID, OccurrenceKey: ref.OccurrenceKey}
	if ref.DocumentChunk != nil {
		p.ChunkKey = ref.DocumentChunk.ChunkKey
	}
	p.Start, p.End = ref.Range()
	return Digest(p)
}
