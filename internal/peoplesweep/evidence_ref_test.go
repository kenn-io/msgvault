package peoplesweep

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonSweepEvidenceRefCarriesValidatedLane(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	message := EvidenceRef{SourceLane: SourceConversationText, SourceID: 7, MessageID: 11, SpanEnd: 4}
	document := EvidenceRef{SourceLane: SourceDocumentText, SourceID: 7, MessageID: 11,
		AttachmentID: 13, OccurrenceKey: "occ", ChunkKey: "chunk", SpanStart: 1, SpanEnd: 4}
	for _, ref := range []EvidenceRef{
		message,
		{SourceLane: SourceMeetingText, SourceID: 7, MessageID: 11, SpanEnd: 4},
		document,
		{SourceLane: SourceAttachmentCaption, SourceID: 7, MessageID: 11, AttachmentID: 13, SpanEnd: 4},
		{SourceLane: SourceAttachmentOCR, SourceID: 7, MessageID: 11, AttachmentID: 13, SpanEnd: 4},
	} {
		encoded, err := EncodePersonSweepEvidenceRef(ref)
		requirements.NoError(err)
		checks.True(strings.HasPrefix(encoded, "person-sweep/v1:"))
		checks.NotContains(encoded, "=")
		decoded, err := DecodePersonSweepEvidenceRef(encoded)
		requirements.NoError(err)
		checks.Equal(ref, decoded)
	}

	_, err := EncodePersonSweepEvidenceRef(EvidenceRef{SourceLane: SourceClass("unknown"), SourceID: 7, MessageID: 11})
	requirements.Error(err)
	_, err = DecodePersonSweepEvidenceRef("person-sweep/v1:e30=")
	requirements.Error(err)
	_, err = DecodePersonSweepEvidenceRef("person-sweep/v1:eyJzb3VyY2VfbGFuZSI6ImNvbnZlcnNhdGlvbl90ZXh0Iiwic291cmNlX2lkIjo3LCJtZXNzYWdlX2lkIjoxMSwiYXR0YWNobWVudF9pZCI6MCwic291cmNlX21lc3NhZ2VfaWQiOiIiLCJvY2N1cnJlbmNlX2tleSI6IiIsImNodW5rX2tleSI6IiIsInNwYW5fc3RhcnQiOjAsInNwYW5fZW5kIjo0LCJleHRyYSI6dHJ1ZX0")
	requirements.Error(err)

	err = ValidatePersonSweepEvidenceItem(EvidenceItem{PersonID: 9,
		SourceClass: SourceMeetingText, Ref: message,
		Excerpt: "text", Highlight: TextSpan{End: 4}})
	requirements.Error(err)
}
