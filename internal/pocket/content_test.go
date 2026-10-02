package pocket

import (
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

// The REST docs leave these interiors unspecified. These fixtures exercise
// compatibility with the documented webhook shapes, not a captured REST account.
const recordingFixture = `{"id":"rec-a","title":"Planning","recording_at":"2026-09-01T10:00:00Z","created_at":"2026-09-02T10:00:00Z","duration":60,"recorded_by":{"email":"owner@example.com","user_id":"user-a"},"transcript":[{"speaker":"Speaker One","text":"Decide roadmap","start":1.25,"end":3}],"summarizations":{"sum-a":{"processingStatus":"completed","updatedAt":"2026-09-02T10:00:00Z","v2":{"summary":{"markdown":"Roadmap decision"},"actionItems":{"actionItems":[{"id":"task-a","title":"Publish roadmap","status":"TODO","isCompleted":false,"is_completed":false,"dueDate":"2026-09-10"}]}}}}}`

func TestPocketContent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	rec, err := DecodeRecording([]byte(recordingFixture))
	requirements.NoError(err)
	content, err := Normalize(rec)
	requirements.NoError(err)
	assertions.Equal("Roadmap decision", content.Summary.Text)
	requirements.Len(content.Transcript.Segments, 1)
	assertions.InDelta(1.25, *content.Transcript.Segments[0].OffsetSeconds, 0)
	requirements.Len(content.Actions, 1)
	assertions.Equal(meetingcontent.StatusPending, content.Actions[0].Status)
	assertions.Equal("task-a", content.Actions[0].SourceID)
	requirements.NotNil(content.DurationSeconds)
	assertions.InDelta(60.0, *content.DurationSeconds, 0)
	assertions.Equal(1, rec.StartedAt.Day())
}

func TestPocketContentAvailability(t *testing.T) {
	for _, tt := range []struct {
		name, fields        string
		transcript, summary meetingcontent.State
		actions             meetingcontent.Coverage
	}{
		{"missing", ``, meetingcontent.StateUnavailable, meetingcontent.StateUnavailable, meetingcontent.CoverageUnavailable},
		{"empty", `,"transcript":[],"summarizations":{"a":{"processingStatus":"completed","v2":{"summary":{"markdown":""},"actionItems":{"actionItems":[]}}}}`, meetingcontent.StateEmpty, meetingcontent.StateEmpty, meetingcontent.CoverageAvailable},
		{"failed transcript", `,"transcript":[],"transcript_error":"processing failed"`, meetingcontent.StateUnavailable, meetingcontent.StateUnavailable, meetingcontent.CoverageUnavailable},
		{"pending placeholders", `,"summarizations":{"a":{"processingStatus":"processing","v2":{"summary":{"markdown":""},"actionItems":{"actionItems":[]}}}}`, meetingcontent.StateUnavailable, meetingcontent.StateUnavailable, meetingcontent.CoverageUnavailable},
		{"missing actions", `,"summarizations":{"a":{"processingStatus":"completed","v2":{"summary":{"markdown":"Notes"}}}}`, meetingcontent.StateUnavailable, meetingcontent.StateAvailable, meetingcontent.CoverageUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			rec, err := DecodeRecording([]byte(`{"id":"r"` + tt.fields + `}`))
			requirements.NoError(err)
			content, err := Normalize(rec)
			requirements.NoError(err)
			assertions.Equal(tt.transcript, content.Transcript.State)
			assertions.Equal(tt.summary, content.Summary.State)
			assertions.Equal(tt.actions, content.ActionCoverage)
		})
	}
}

func TestPocketContentRejectsMalformedSections(t *testing.T) {
	for _, fields := range []string{`"transcript":{}`, `"transcript":[{"text":"x","start":-1}]`, `"duration":-1`, `"recording_at":"bad"`, `"summarizations":[]`, `"summarizations":{"a":{"processingStatus":"completed","v2":{"summary":{"markdown":"x"},"actionItems":{"actionItems":[{"title":"x","status":"TODO","isCompleted":true}]}}}}`} {
		rec, err := DecodeRecording([]byte(`{"id":"r",` + fields + `}`))
		if err == nil {
			_, err = Normalize(rec)
		}
		require.Error(t, err, fields)
	}
}

func TestPocketContentSelectsLatestCompletedSummary(t *testing.T) {
	rec, err := DecodeRecording([]byte(`{"id":"r","summarizations":{
		"a":{"processingStatus":"completed","updatedAt":"2026-09-02T10:00:00Z","v2":{"summary":{"markdown":"Older"}}},
		"b":{"processingStatus":"completed","updatedAt":"2026-09-03T10:00:00Z","v2":{"summary":{"markdown":"","bulletPoints":["Latest"]}}},
		"c":{"processingStatus":"processing","updatedAt":"2026-09-04T10:00:00Z","v2":{"summary":{"markdown":""}}}
	}}`))
	require.NoError(t, err)
	content, err := Normalize(rec)
	require.NoError(t, err)
	assert.Equal(t, "- Latest", content.Summary.Text)
}

func TestPocketTranscriptOffsetBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		valid       bool
	}{
		{"zero", "0", true}, {"negative", "-1", false},
		{"largest finite", "1.7976931348623157e308", true}, {"not a number", "NaN", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := DecodeRecording([]byte(`{"id":"r","transcript":[{"text":"x","start":` + tt.value + `}]}`))
			if err == nil {
				_, err = Normalize(rec)
			}
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func FuzzPocketTranscriptOffsets(f *testing.F) {
	for _, n := range []float64{0, math.Copysign(0, -1), 1.25, -1, math.MaxFloat64, math.Inf(1), math.NaN()} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, offset float64) {
		raw := fmt.Sprintf(`{"id":"r","transcript":[{"text":"x","start":%s}]}`, strconv.FormatFloat(offset, 'g', -1, 64))
		rec, err := DecodeRecording([]byte(raw))
		var content meetingcontent.Content
		if err == nil {
			content, err = Normalize(rec)
		}
		if math.IsNaN(offset) || math.IsInf(offset, 0) || offset < 0 {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		require.Len(t, content.Transcript.Segments, 1)
		require.NotNil(t, content.Transcript.Segments[0].OffsetSeconds)
		// Zero delta checks exact numeric equality, including both signed zeros.
		assert.InDelta(t, offset, *content.Transcript.Segments[0].OffsetSeconds, 0)
	})
}
