package embed_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestAssembleMeetingDocument_PacksWholeTurnsAndKeepsSourceSpans(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := meetingBodyLF()
	doc, err := embed.AssembleMeetingDocument(meetingRow(7, body), embed.AssemblyPolicy{MaxChunkRunes: 80})
	require.NoError(err)
	require.GreaterOrEqual(len(doc.Chunks), 2)
	assert.Contains(sourceText(body, doc.Chunks[0]), "Decision summary.")
	for _, chunk := range doc.Chunks {
		source := sourceText(body, chunk)
		assert.NotContains(source, "Attendees:")
		assert.Contains(chunk.Text, "Weekly sync\nWhen: 2026-08-08 09:00\nAttendees: Alice, Bob")
		assert.Equal(vector.SourceBasisBody, chunk.SourceBasis)
	}
}

func TestAssembleMeetingDocument_NoteOnlyProducesContextualNoteChunks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := "Weekly sync\nWhen: 2026-08-08 09:00\nAttendees: Alice, Bob\n\nSummary first.\n\nDecisions:\n- Ship it."
	doc, err := embed.AssembleMeetingDocument(meetingRow(8, body), embed.AssemblyPolicy{MaxChunkRunes: 24})
	require.NoError(err)
	require.GreaterOrEqual(len(doc.Chunks), 2)
	assert.Contains(sourceText(body, doc.Chunks[0]), "Summary first.")
	assert.NotContains(sourceText(body, doc.Chunks[0]), "Weekly sync")
	assert.Contains(concatenateMeetingSource(body, doc.Chunks), "Decisions:\n- Ship it.")
}

func TestAssembleMeetingDocument_SummaryChunksComeBeforeTurns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := meetingBodyLF()
	doc, err := embed.AssembleMeetingDocument(meetingRow(9, body), embed.AssemblyPolicy{MaxChunkRunes: 200})
	require.NoError(err)
	require.Len(doc.Chunks, 2)
	assert.Equal("Decision summary.", sourceText(body, doc.Chunks[0]))
	assert.Equal("[00:01] Alice: First turn.\n[00:03] Bob: Second turn.", sourceText(body, doc.Chunks[1]))
}

func TestAssembleMeetingDocument_ExactBudgetKeepsConsecutiveTurnsTogether(t *testing.T) {
	turns := "[00:01] Alice: One.\n[00:02] Bob: Two."
	body := "Exact budget\nWhen: 2026-08-08 09:00\n\nTranscript:\n" + turns
	doc, err := embed.AssembleMeetingDocument(meetingRow(10, body), embed.AssemblyPolicy{MaxChunkRunes: utf8.RuneCountInString(turns)})
	require.NoError(t, err)
	require.Len(t, doc.Chunks, 1)
	assert.Equal(t, turns, sourceText(body, doc.Chunks[0]))
}

func TestAssembleMeetingDocument_OversizedSingleTurnUsesGenericChunkerWithoutOverlap(t *testing.T) {
	turn := "[00:01] Alice: " + strings.Repeat("word ", 12) + "done."
	body := "Long turn\nWhen: 2026-08-08 09:00\n\nTranscript:\n" + turn
	doc, err := embed.AssembleMeetingDocument(meetingRow(11, body), embed.AssemblyPolicy{MaxChunkRunes: 24})
	require.NoError(t, err)
	require.Greater(t, len(doc.Chunks), 1)
	assert.Equal(t, turn, concatenateMeetingSource(body, doc.Chunks))
	assertNonOverlappingDense(t, doc.Chunks)
}

func TestAssembleMeetingDocument_CirclebackSectionsRemainOneOrderedNoteRegion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := "Project review\nWhen: 2026-08-08 09:00\nAttendees: Alice, Bob\n\n## Notes\nDecision recorded.\n\nAction items:\n- Alice ships.\n\nInsights:\n- Risk is low.\n\nTags: launch\n\nTranscript:\n[00:04] Alice: We agreed."
	doc, err := embed.AssembleMeetingDocument(meetingRow(12, body), embed.AssemblyPolicy{MaxChunkRunes: 500})
	require.NoError(err)
	require.Len(doc.Chunks, 2)
	assert.Equal("## Notes\nDecision recorded.\n\nAction items:\n- Alice ships.\n\nInsights:\n- Risk is low.\n\nTags: launch", sourceText(body, doc.Chunks[0]))
	assert.Equal("[00:04] Alice: We agreed.", sourceText(body, doc.Chunks[1]))
}

func TestAssembleMeetingDocument_PlainTranscriptUsesStrictGenericFallback(t *testing.T) {
	body := "Speaker 1: first line\n  indented continuation\nSpeaker 2: final line"
	doc, err := embed.AssembleMeetingDocument(meetingRow(13, body), embed.AssemblyPolicy{MaxChunkRunes: 24})
	require.NoError(t, err)
	require.Greater(t, len(doc.Chunks), 1)
	assert.Equal(t, body, concatenateMeetingSource(body, doc.Chunks))
	assertNonOverlappingDense(t, doc.Chunks)
}

func TestAssembleMeetingDocument_NotionCanonicalSectionsRemainSearchable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := "Notion planning\nWhen: 2026-08-29 10:00 - 10:30\nAttendees: Test Attendee\n\nSummary:\nRelease scope agreed.\n\nNotes:\nOwner prepares rollout.\n\nTranscript:\nTest Speaker: Ready to ship."
	doc, err := embed.AssembleMeetingDocument(meetingRow(18, body), embed.AssemblyPolicy{MaxChunkRunes: 500})
	require.NoError(err)
	require.NotEmpty(doc.Chunks)
	joined := concatenateMeetingSource(body, doc.Chunks)
	assert.Contains(joined, "Release scope agreed.")
	assert.Contains(joined, "Owner prepares rollout.")
	assert.Contains(joined, "Test Speaker: Ready to ship.")
	for _, chunk := range doc.Chunks {
		assert.Contains(chunk.Text, "Notion planning")
		assert.Equal(vector.SourceBasisBody, chunk.SourceBasis)
	}
}

func TestAssembleMeetingDocument_IncompleteSpeakerContractFallsBackForTranscriptRegion(t *testing.T) {
	transcript := "[00:01] Alice: Complete.\nBob: missing timestamp."
	body := "Strict parser\nWhen: 2026-08-08 09:00\n\nTranscript:\n" + transcript
	doc, err := embed.AssembleMeetingDocument(meetingRow(14, body), embed.AssemblyPolicy{MaxChunkRunes: 500})
	require.NoError(t, err)
	require.Len(t, doc.Chunks, 1)
	assert.Equal(t, transcript, sourceText(body, doc.Chunks[0]))
}

func TestAssembleMeetingDocument_CRLFAndUnicodeUsePreprocessedRuneOffsets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := "国際会議\r\nWhen: 2026-08-08 09:00\r\nAttendees: 葵, Bob\r\n\r\n要約です。\r\n\r\nTranscript:\r\n[00:01] 葵: 同じ話。"
	doc, err := embed.AssembleMeetingDocument(meetingRow(15, body), embed.AssemblyPolicy{MaxChunkRunes: 100})
	require.NoError(err)
	preprocessed, _ := embed.Preprocess("", body, 0, embed.PreprocessConfig{})
	require.Len(doc.Chunks, 2)
	assert.Equal("要約です。", sourceText(preprocessed, doc.Chunks[0]))
	assert.Equal("[00:01] 葵: 同じ話。", sourceText(preprocessed, doc.Chunks[1]))
	assert.Equal(utf8.RuneCountInString("国際会議\nWhen: 2026-08-08 09:00\nAttendees: 葵, Bob\n\n"), doc.Chunks[0].SourceCharStart)
}

func TestAssembleMeetingDocument_RepeatedTurnTextKeepsDistinctSourceOffsets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	turn := "[00:01] Alice: Same text."
	body := "Repeated\nWhen: 2026-08-08 09:00\n\nTranscript:\n" + turn + "\n" + turn
	doc, err := embed.AssembleMeetingDocument(meetingRow(16, body), embed.AssemblyPolicy{MaxChunkRunes: utf8.RuneCountInString(turn)})
	require.NoError(err)
	require.Len(doc.Chunks, 2)
	assert.Equal(turn, sourceText(body, doc.Chunks[0]))
	assert.Equal(turn, sourceText(body, doc.Chunks[1]))
	assert.Less(doc.Chunks[0].SourceCharStart, doc.Chunks[1].SourceCharStart)
	assert.LessOrEqual(doc.Chunks[0].SourceCharEnd, doc.Chunks[1].SourceCharStart)
}

func TestAssembleMeetingDocument_DocumentBudgetCapsOutputAndMarksTruncation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	turn := "[00:01] Alice: " + strings.Repeat("x", 40)
	body := "Budget\nWhen: 2026-08-08 09:00\n\nTranscript:\n" + turn
	policy := embed.AssemblyPolicy{MaxChunkRunes: 16, MaxDocumentUTF8Bytes: 150}
	doc, err := embed.AssembleMeetingDocument(meetingRow(17, body), policy)
	require.NoError(err)
	require.NotEmpty(doc.Chunks)
	assertNonOverlappingDense(t, doc.Chunks)
	input := embed.DocumentInput{Chunks: make([]string, len(doc.Chunks))}
	for i, chunk := range doc.Chunks {
		input.Chunks[i] = chunk.Text
	}
	_, err = embed.PackDocuments([]embed.DocumentInput{input}, embed.RequestLimits{
		MaxDocuments: 1, MaxChunks: 16_000, MaxUTF8Bytes: policy.MaxDocumentUTF8Bytes,
	})
	require.NoError(err)
	assert.True(doc.Chunks[len(doc.Chunks)-1].Truncated)
	assert.Less(len(concatenateMeetingSource(body, doc.Chunks)), len(turn))
}

func meetingRow(id int64, body string) embed.AssemblyMessage {
	return embed.AssemblyMessage{ID: id, ConversationID: id + 100, MessageType: "meeting_transcript", Body: body, LastModified: "v1", SourceSequence: 9}
}

func meetingBodyLF() string {
	return "Weekly sync\nWhen: 2026-08-08 09:00\nAttendees: Alice, Bob\n\nDecision summary.\n\nTranscript:\n[00:01] Alice: First turn.\n[00:03] Bob: Second turn."
}

func sourceText(body string, chunk embed.OwnedChunk) string {
	runes := []rune(body)
	return string(runes[chunk.SourceCharStart:chunk.SourceCharEnd])
}

func concatenateMeetingSource(body string, chunks []embed.OwnedChunk) string {
	var out strings.Builder
	for _, chunk := range chunks {
		out.WriteString(sourceText(body, chunk))
	}
	return out.String()
}

func assertNonOverlappingDense(t *testing.T, chunks []embed.OwnedChunk) {
	t.Helper()
	for i, chunk := range chunks {
		assert.Equal(t, i, chunk.ChunkIndex)
		if i > 0 {
			assert.LessOrEqual(t, chunks[i-1].SourceCharEnd, chunk.SourceCharStart)
		}
	}
}
