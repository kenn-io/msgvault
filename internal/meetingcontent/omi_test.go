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
	content := Decode("omi_json", []byte(`{"structured":{"overview":"Summary","sections":[{"heading":"Decisions","body_markdown":"Use the synthetic plan"}],"action_items":[{"description":"Write proposal","completed":true,"due_at":"2026-01-02T12:00:00Z","owner_name":"Synthetic User"},{"description":"Missing status"}]},"transcript_segments":[{"text":"Hello","speaker_name":"Synthetic User","start":0,"end":2},{"text":"Hi","speaker_id":2,"start":2,"end":4}],"started_at":"2026-01-01T12:00:00Z","finished_at":"2026-01-01T12:00:04Z"}`), nil)
	assert.Equal(StateAvailable, content.Summary.State)
	assert.Equal(StateUnsupported, content.Notes.State)
	require.Len(content.Actions, 2)
	assert.Equal(StatusCompleted, content.Actions[0].Status)
	assert.Equal("2026-01-02T12:00:00Z", content.Actions[0].DueDate)
	assert.Equal(StatusUnknown, content.Actions[1].Status)
	assert.Equal("structured.action_items[0]", content.Actions[0].Locator)
	require.Len(content.Transcript.Segments, 2)
	assert.Equal("Synthetic User", content.Transcript.Segments[0].Speaker)
	assert.Equal("Speaker 2", content.Transcript.Segments[1].Speaker)
	require.Len(content.SourceParticipants, 2)
	assert.Empty(content.SourceParticipants[0].Email)
	assert.Equal(DurationProvider, content.DurationBasis)
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
		content := Decode("omi_json", raw, nil)
		assert.Equal(t, StateAvailable, content.Transcript.State)
		require.Len(t, content.Transcript.Segments, 1)
		if start < 0 || end < start || end < 0 {
			assert.Nil(t, content.Transcript.Segments[0].OffsetSeconds)
		}
	})
}

func TestDecodeOmiMissingAndInvalidEvidence(t *testing.T) {
	assert := assert.New(t)
	missing := Decode("omi_json", []byte(`{"structured":{"overview":"","action_items":[]}}`), nil)
	assert.Equal(StateEmpty, missing.Summary.State)
	assert.Equal(StateUnavailable, missing.Transcript.State)
	assert.Equal(CoverageAvailable, missing.ActionCoverage)
	invalid := Decode("omi_json", []byte(`{"structured":{"overview":"Summary","action_items":[{"description":"Valid","completed":false},{"completed":true}]},"transcript_segments":[{"text":"Hello"}]}`), nil)
	assert.Equal(CoveragePartial, invalid.ActionCoverage)
	assert.Equal(StateAvailable, invalid.Transcript.State)
	assert.Nil(invalid.Transcript.Segments[0].OffsetSeconds)
}

func TestDecodeOmiRosterAttribution(t *testing.T) {
	assert := assert.New(t)
	content := Decode("omi_json", []byte(`{"structured":{"overview":"","action_items":[],"participants":[{"name":"Roster User","email":"ROSTER@example.com","source":"roster"},{"name":"Transcript User","email":"guessed@example.com","source":"transcript"},{"name":"AI Assistant","email":"bot@example.com","source":"roster","is_ai_agent":true}]},"transcript_segments":[]}`), nil)
	assert.Empty(content.SourceParticipants)
}

func TestOmiDurationUsesTranscriptEnd(t *testing.T) {
	content := Decode("omi_json", []byte(`{"structured":{"overview":"","action_items":[]},"transcript_segments":[{"text":"One","start":0,"end":3},{"text":"Two","start":4,"end":10}]}`), nil)
	require.NotNil(t, content.DurationSeconds)
	assert.InDelta(t, float64(10), *content.DurationSeconds, 0)
	assert.Equal(t, DurationTranscriptSpan, content.DurationBasis)
}

func TestDecodeOmiBlankSegments(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		state State
		count int
	}{
		{`[{"text":"One","start":0},{"text":"  "},{"text":"Two","start":5}]`, StateAvailable, 2},
		{`[{"text":"  "}]`, StateEmpty, 0},
		{`[{"text":42}]`, StateUnavailable, 0},
		{`[{"text":null}]`, StateUnavailable, 0},
		{`[{}]`, StateUnavailable, 0},
		{`[{"text":"Speech","start":"bad"}]`, StateUnavailable, 0},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			content := Decode("omi_json", []byte(`{"transcript_segments":`+tc.raw+`}`), nil)
			assert.Equal(t, tc.state, content.Transcript.State)
			assert.Len(t, content.Transcript.Segments, tc.count)
		})
	}
}
