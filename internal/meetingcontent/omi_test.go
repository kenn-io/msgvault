package meetingcontent

import (
	"encoding/json/v2"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeOmiSectionsActionsAndSpeakers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := Decode("omi_json", []byte(`{"structured":{"overview":"Summary","sections":[{"heading":"Decisions","body_markdown":"Use the synthetic plan"}],"action_items":[{"description":"Write proposal","completed":true,"due_at":"2026-01-02T12:00:00Z","owner_name":"Synthetic User"},{"description":"Missing status"}]},"transcript_segments":[{"text":"Hello","is_user":true,"start":0,"end":2},{"text":"Hi","speaker_id":2,"start":2,"end":4}],"started_at":"2026-01-01T12:00:00Z","finished_at":"2026-01-01T12:00:04Z"}`), nil)
	assert.Equal(StateAvailable, content.Summary.State)
	assert.Equal("Decisions\nUse the synthetic plan", content.Notes.Text)
	require.Len(content.Actions, 2)
	assert.Equal(StatusCompleted, content.Actions[0].Status)
	assert.Equal("2026-01-02T12:00:00Z", content.Actions[0].DueDate)
	assert.Equal(StatusUnknown, content.Actions[1].Status)
	assert.Equal("structured.action_items[0]", content.Actions[0].Locator)
	require.Len(content.Transcript.Segments, 2)
	assert.Equal("You", content.Transcript.Segments[0].Speaker)
	assert.Equal("Speaker 2", content.Transcript.Segments[1].Speaker)
	require.Len(content.SourceParticipants, 2)
	assert.Empty(content.SourceParticipants[0].Email)
	assert.Equal(DurationProvider, content.DurationBasis)
}

func TestDecodeOmiReversedTranscriptInterval(t *testing.T) {
	content := Decode("omi_json", []byte(`{"structured":{"overview":"Summary","action_items":[]},"transcript_segments":[{"text":"Replacement","start":10,"end":5}]}`), nil)
	assert.Equal(t, StateUnavailable, content.Transcript.State)
}

func FuzzDecodeOmiTranscriptInterval(f *testing.F) {
	f.Add(10.0, 5.0)
	f.Add(0.0, 0.0)
	f.Add(0.0, 5.0)
	f.Add(-1.0, 5.0)
	f.Fuzz(func(t *testing.T, start, end float64) {
		// Nonfinite numbers cannot be represented by JSON. Their raw-input
		// rejection belongs to Decode's invalid-JSON tests, not this interval.
		if math.IsNaN(start) || math.IsNaN(end) || math.IsInf(start, 0) || math.IsInf(end, 0) {
			t.Skip()
		}
		raw, err := json.Marshal(map[string]any{"transcript_segments": []map[string]any{{"text": "Speech", "start": start, "end": end}}})
		require.NoError(t, err)
		want := StateAvailable
		if start < 0 || end < start {
			want = StateUnavailable
		}
		assert.Equal(t, want, Decode("omi_json", raw, nil).Transcript.State)
	})
}

func TestDecodeOmiMissingAndInvalidEvidence(t *testing.T) {
	assert := assert.New(t)
	missing := Decode("omi_json", []byte(`{"structured":{"overview":"","action_items":[]}}`), nil)
	assert.Equal(StateEmpty, missing.Summary.State)
	assert.Equal(StateUnavailable, missing.Transcript.State)
	assert.Equal(CoverageAvailable, missing.ActionCoverage)
	invalid := Decode("omi_json", []byte(`{"structured":{"overview":"Summary","action_items":[{"description":"Valid","completed":false},{"completed":true}]},"transcript_segments":[{"text":"Hello","start":-1}]}`), nil)
	assert.Equal(CoveragePartial, invalid.ActionCoverage)
	assert.Equal(StateUnavailable, invalid.Transcript.State)
}

func TestDecodeOmiRosterAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := Decode("omi_json", []byte(`{"structured":{"overview":"","action_items":[],"participants":[{"name":"Roster User","email":"ROSTER@example.com","source":"roster"},{"name":"Transcript User","email":"guessed@example.com","source":"transcript"},{"name":"AI Assistant","email":"bot@example.com","source":"roster","is_ai_agent":true}]},"transcript_segments":[]}`), nil)
	require.Len(content.SourceParticipants, 2)
	assert.Equal("roster@example.com", content.SourceParticipants[0].Email)
	assert.Empty(content.SourceParticipants[1].Email)
}

func TestOmiDurationUsesTranscriptEnd(t *testing.T) {
	content := Decode("omi_json", []byte(`{"structured":{"overview":"","action_items":[]},"transcript_segments":[{"text":"One","start":0,"end":3},{"text":"Two","start":4,"end":10}]}`), nil)
	require.NotNil(t, content.DurationSeconds)
	assert.InDelta(t, float64(10), *content.DurationSeconds, 0)
	assert.Equal(t, DurationTranscriptSpan, content.DurationBasis)
}

func TestDecodeOmiBlankOptionalNotes(t *testing.T) {
	content := Decode("omi_json", []byte(`{"structured":{"overview":"","action_items":[],"sections":[{"heading":"  ","body_markdown":"\n\t"}]},"transcript_segments":null}`), nil)
	assert.Equal(t, StateEmpty, content.Notes.State)
	assert.Empty(t, content.Notes.Text)
}
