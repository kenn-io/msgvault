package kataevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
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
// r must already be canonical. A field added to Reference joins the ID only
// under a new domain tag.
func ID(r Reference) string {
	fields := []string{strconv.Itoa(r.Version), r.Kind, r.ArchiveUID, strconv.FormatInt(r.MessageID, 10),
		r.SourceType, r.SourceIdentifier, r.SourceMessageID, strconv.FormatInt(r.AttachmentID, 10), r.OccurrenceKey}
	switch {
	case r.Message != nil:
		p := r.Message
		fields = append(fields, p.BodySHA256, strconv.Itoa(p.StartRune), strconv.Itoa(p.EndRune))
	case r.DocumentChunk != nil:
		p := r.DocumentChunk
		fields = append(fields, p.CanonicalBlobHash, p.ExtractionID, p.ManifestChecksum, p.ChunkKey, p.ChunkChecksum,
			strconv.Itoa(p.StartRune), strconv.Itoa(p.EndRune))
	}
	return Digest("msgvault.kata.evidence.v1", fields...)
}

// Digest is the hex SHA-256 of a domain tag and fields, each written as its
// byte length, a colon and its bytes, so no two field lists share an
// encoding. Kata issues store these digests, so the bytes for a domain never
// change: hashing different fields needs a new domain tag.
func Digest(domain string, fields ...string) string {
	hash := sha256.New()
	for _, field := range append([]string{domain}, fields...) {
		// A hash never fails to write.
		_, _ = io.WriteString(hash, strconv.Itoa(len(field))+":"+field)
	}
	return hex.EncodeToString(hash.Sum(nil))
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

// PassageID is the one identity msgvault uses to decide whether two
// citations quote the same thing: where the text sits plus its words.
func PassageID(ref Reference, words string) string {
	return Digest("msgvault.kata.passage.v1", PassageLocation(ref), words)
}

// PassageLocation identifies where ref sits, whatever text is there now. It
// leaves out the content and extraction hashes that change when the source is
// synced or reprocessed, and the attachment row ID, which message repair can
// reassign; the occurrence key names the file instead.
func PassageLocation(ref Reference) string {
	chunkKey := ""
	if ref.DocumentChunk != nil {
		chunkKey = ref.DocumentChunk.ChunkKey
	}
	start, end := ref.Range()
	return Digest("msgvault.kata.passage-location.v1", ref.Kind, ref.ArchiveUID, strconv.FormatInt(ref.MessageID, 10),
		ref.SourceType, ref.SourceIdentifier, ref.SourceMessageID, ref.OccurrenceKey, chunkKey, strconv.Itoa(start), strconv.Itoa(end))
}
