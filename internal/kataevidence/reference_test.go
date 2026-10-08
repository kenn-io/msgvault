package kataevidence_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/kataevidence"
)

// Kata issues store these digests, so they must not change between builds or
// toolchains. A failure here means existing issues would stop matching.
func TestDigestsAreStable(t *testing.T) {
	assert := assert.New(t)
	message := kataevidence.Reference{Version: 1, Kind: "message", ArchiveUID: "archive-a", MessageID: 42,
		SourceType: "gmail", SourceIdentifier: "user-a@example.com", SourceMessageID: "source-42",
		Message: &kataevidence.MessageReference{BodySHA256: strings.Repeat("a", 64), StartRune: 5, EndRune: 30}}
	chunk := kataevidence.Reference{Version: 1, Kind: "document_chunk", ArchiveUID: "archive-a", MessageID: 42,
		SourceType: "gmail", SourceIdentifier: "user-a@example.com", SourceMessageID: "source-42", AttachmentID: 7, OccurrenceKey: "occurrence-7",
		DocumentChunk: &kataevidence.DocumentReference{CanonicalBlobHash: strings.Repeat("b", 64), ExtractionID: "extract-1",
			ManifestChecksum: strings.Repeat("c", 64), ChunkKey: "chunk-0", ChunkChecksum: strings.Repeat("d", 64), StartRune: 0, EndRune: 12}}

	assert.Equal("577e2b4ae71cc1458013ca6f0bad0913614477f21e18963889d42d9d843cf6a0", kataevidence.ID(message))
	assert.Equal("dba39aa6941c618acd0b45ce8b12f90371359d1823f7d4e4a1cde3b1e4f8fb42", kataevidence.PassageLocation(message))
	assert.Equal("ae248d383c374df1e2830b2041983f1fb0aa9d5a2c32e8747d34242b3ef43271", kataevidence.PassageID(message, "send the revised budget"))
	assert.Equal("44bffe1907481b2fae383c056dee53c2e2615d4738aba7fb9f212204f579e19c", kataevidence.ID(chunk))
	assert.Equal("d43e7820b8869238c63b2700654671e33b85e7a300437846c2af7b21fec14694", kataevidence.PassageLocation(chunk))
}

func TestDigestSeparatesFields(t *testing.T) {
	assert := assert.New(t)
	assert.NotEqual(kataevidence.Digest("domain", "ab", "c"), kataevidence.Digest("domain", "a", "bc"))
	assert.NotEqual(kataevidence.Digest("domain", "a"), kataevidence.Digest("domain", "a", ""))
	assert.NotEqual(kataevidence.Digest("one", "a"), kataevidence.Digest("two", "a"))
}
