package twilio

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotSnippetPreservesUnicode(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	text := strings.Repeat("界", 501)
	snapshot, err := (archivedCall{Call: Call{SID: testCA}, Evidence: Evidence{Transcripts: []Transcript{{Kind: "classic", SourceID: testRE, Complete: true, Usable: true, Segments: []Segment{{Text: text}}}}}}).snapshot(1, "owner@example.com")
	require.NoError(err)
	assert.True(utf8.ValidString(snapshot.Snippet))
	assert.Equal(strings.Repeat("界", 500), snapshot.Snippet)
	assert.Equal(text, snapshot.Body)
}

func TestArchiveRetainsCorrelationEvidenceAfterOmission(t *testing.T) {
	assert := assert.New(t)

	old := archivedCall{Evidence: Evidence{
		RelayEvents:   []json.RawMessage{json.RawMessage(`{"conversation_relay_data":{"session_id":"VXaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`)},
		Conversations: []json.RawMessage{json.RawMessage(`{"id":"conversation-example"}`)},
	}}
	merged := mergeArchive(old, Call{SID: testCA}, nil, Evidence{})
	assert.Equal(old.Evidence.RelayEvents, merged.Evidence.RelayEvents)
	assert.Equal(old.Evidence.Conversations, merged.Evidence.Conversations)
	assert.Empty(merged.LatestAcquisition.RelayEvents)

	fresh := Evidence{RelayEvents: []json.RawMessage{json.RawMessage(`{"conversation_relay_data":{"session_id":"VXbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`)}}
	merged = mergeArchive(old, Call{SID: testCA}, nil, fresh)
	assert.Len(merged.Evidence.RelayEvents, 2)
	assert.Equal(old.Evidence.Conversations, merged.Evidence.Conversations)
	assert.Equal(fresh, merged.LatestAcquisition)
}

func TestSnapshotInvalidDurationDoesNotLoseCall(t *testing.T) {
	for _, duration := range []string{"NaN", "Inf", "-Inf", "-1", "invalid"} {
		t.Run(duration, func(t *testing.T) {
			snapshot, err := (archivedCall{Call: Call{SID: testCA, Duration: duration}}).snapshot(1, "owner@example.com")
			require.NoError(t, err)
			assert.Contains(t, string(snapshot.Raw), `"duration_seconds":0`)
		})
	}
}

func TestSnapshotPreservesExplicitZeroDuration(t *testing.T) {
	snapshot, err := (archivedCall{Call: Call{
		SID: testCA, StartTime: "2026-10-03T10:00:00Z", EndTime: "2026-10-03T10:05:00Z", Duration: "0",
	}}).snapshot(1, "owner@example.com")
	require.NoError(t, err)
	assert.Contains(t, string(snapshot.Raw), `"duration_seconds":0`)
}

func TestBatchMetadataWithoutRetainedTextIsUnavailable(t *testing.T) {
	assert := assert.New(t)

	segments, complete := canonicalSegments([]Transcript{{Kind: "batch", ID: "batch-example", SourceID: testRE, Complete: true}})
	assert.Empty(segments)
	assert.False(complete, "a completed Batch job is not evidence of an empty retained transcript")
}

func TestCanonicalSegmentsOrderTimestampedBeforeUntimestamped(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	earlier := time.Date(2026, time.October, 3, 9, 0, 0, 0, time.UTC)
	later := time.Date(2026, time.October, 3, 11, 0, 0, 0, time.UTC)
	transcripts := []Transcript{{
		Kind:   "orchestrator",
		Usable: true,
		Segments: []Segment{
			{Text: "missing-first"},
			{Text: "later-first", StartedAt: &later},
			{Text: "missing-second"},
			{Text: "earlier", StartedAt: &earlier},
			{Text: "later-second", StartedAt: &later},
		},
	}}

	segments, complete := canonicalSegments(transcripts)
	require.True(complete)
	texts := make([]string, 0, len(segments))
	for _, segment := range segments {
		texts = append(texts, segment.Text)
	}
	assert.Equal([]string{"earlier", "later-first", "later-second", "missing-first", "missing-second"}, texts)
}
