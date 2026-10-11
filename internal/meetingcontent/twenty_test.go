package meetingcontent

import (
	"encoding/json/v2"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTwentyContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte(`{"schema_version":1,"recording":{"startedAt":"2026-09-01T10:00:00Z","endedAt":"2026-09-01T10:10:00Z","summary":{"markdown":"## Decisions\nShip the update."},"transcript":[{"participant":{"name":"Speaker Example"},"words":[{"text":"Hello","start_timestamp":{"relative":1.25}},{"text":"world","end_timestamp":{"relative":2.5}}]},{"words":[{"text":"Confirmed"}]}]},"participants":[{"handle":"Organizer@Example.COM","displayName":"Organizer Example","isOrganizer":true},{"handle":"attendee@example.com","displayName":"Attendee Example"},{"handle":"display only","displayName":"Guest"}]}`)
	c := Decode("twenty_json", raw, nil)
	assert.Equal(StateAvailable, c.Summary.State)
	assert.Equal("## Decisions\nShip the update.", c.Summary.Text)
	assert.Equal(StateAvailable, c.Transcript.State)
	require.Len(c.Transcript.Segments, 2)
	assert.Equal("Hello world", c.Transcript.Segments[0].Text)
	assert.Equal("Speaker Example", c.Transcript.Segments[0].Speaker)
	require.NotNil(c.Transcript.Segments[0].OffsetSeconds)
	assert.InDelta(1.25, *c.Transcript.Segments[0].OffsetSeconds, 1e-9)
	assert.Equal("Unknown speaker", c.Transcript.Segments[1].Speaker)
	assert.Nil(c.Transcript.Segments[1].OffsetSeconds)
	require.NotNil(c.DurationSeconds)
	assert.InDelta(600, *c.DurationSeconds, 1e-9)
	assert.Equal(DurationProvider, c.DurationBasis)
	assert.Equal(CoverageUnsupported, c.ActionCoverage)
	assert.Empty(c.Actions)
	require.Len(c.SourceParticipants, 3)
	assert.Equal("organizer@example.com", c.SourceParticipants[0].Email)
	assert.Equal("from", c.SourceParticipants[0].Role)
	assert.Equal("to", c.SourceParticipants[1].Role)
	assert.Equal("Guest", c.SourceParticipants[2].Name)
	assert.Empty(c.SourceParticipants[2].Email)
}

func TestTwentyMarkersAndInvalidTranscriptPreserveSummary(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		state  State
		reason string
	}{
		{`{"status":"PENDING"}`, StateUnavailable, "pending"},
		{`{"status":"FAILED","subCode":"provider_error"}`, StateUnavailable, "failed"},
		{`{"status":"EMPTY"}`, StateEmpty, ""},
		{`null`, StateUnavailable, reasonMissingField},
		{`[]`, StateEmpty, ""},
		{`"not a transcript"`, StateUnavailable, reasonInvalidSection},
		{`[{"words":[{"text":42}]}]`, StateEmpty, ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			assert := assert.New(t)
			raw := []byte(`{"schema_version":1,"recording":{"summary":{"markdown":"A valid summary"},"transcript":` + tc.raw + `}}`)
			c := Decode("twenty_json", raw, nil)
			assert.Equal("A valid summary", c.Summary.Text)
			assert.Equal(tc.state, c.Transcript.State)
			assert.Equal(tc.reason, c.Transcript.Reason)
			assert.Empty(c.Transcript.Text)
		})
	}
}

// Like Twenty's own transcript parser, a malformed entry or word drops only
// itself rather than the whole transcript.
func TestTwentyTranscriptSkipsMalformedWords(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	transcript := `[` +
		`{"participant":{"name":"Speaker Example"},"words":[{"text":42},"bare",{"text":"Kept","start_timestamp":{"relative":-1}},{"text":"words","start_timestamp":{"relative":2}}]},` +
		`{"words":"not a list"},` +
		`{"participant":"not an object","words":[{"text":"Next","start_timestamp":"not an object"}]}]`
	c := Decode("twenty_json", []byte(`{"schema_version":1,"recording":{"transcript":`+transcript+`}}`), nil)
	assert.Equal(StateAvailable, c.Transcript.State)
	require.Len(c.Transcript.Segments, 2)
	assert.Equal("Kept words", c.Transcript.Segments[0].Text)
	assert.Equal("Speaker Example", c.Transcript.Segments[0].Speaker)
	require.NotNil(c.Transcript.Segments[0].OffsetSeconds, "a negative offset is ignored, not used")
	assert.InDelta(2, *c.Transcript.Segments[0].OffsetSeconds, 1e-9)
	assert.Equal("Next", c.Transcript.Segments[1].Text)
	assert.Equal("Unknown speaker", c.Transcript.Segments[1].Speaker)
	assert.Nil(c.Transcript.Segments[1].OffsetSeconds)
}

func TestTwentyDurationFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, tc := range []struct {
		recording, calendar string
		duration            float64
		basis               DurationBasis
	}{
		{`{"startedAt":"2026-09-01T10:00:00Z","endedAt":"2026-09-01T09:59:00Z"}`, `{"startsAt":"2026-09-01T10:00:00Z","endsAt":"2026-09-01T10:30:00Z"}`, 1800, DurationScheduled},
		{`{"createdAt":"2026-09-01T10:00:00Z","transcript":[{"words":[{"text":"First","start_timestamp":{"relative":3}}]},{"words":[{"text":"Last","start_timestamp":{"relative":13}}]}]}`, `null`, 10, DurationTranscriptSpan},
		{`{"transcript":[{"words":[{"text":"One segment","start_timestamp":{"relative":1.25},"end_timestamp":{"relative":2.5}}]}]}`, `null`, 1.25, DurationTranscriptSpan},
		{`{"transcript":[{"words":[{"text":"","start_timestamp":{"relative":999}},{"text":"Spoken","start_timestamp":{"relative":1},"end_timestamp":{"relative":3}}]}]}`, `null`, 2, DurationTranscriptSpan},
		{`{"transcript":[{"words":[{"text":"Absolute timing","start_timestamp":{"absolute":"2026-09-01T10:00:01Z"},"end_timestamp":{"absolute":"2026-09-01T10:00:04Z"}}]}]}`, `null`, 3, DurationTranscriptSpan},
	} {
		c := Decode("twenty_json", []byte(`{"schema_version":1,"recording":`+tc.recording+`,"calendar_event":`+tc.calendar+`}`), nil)
		require.NotNil(c.DurationSeconds)
		assert.InDelta(tc.duration, *c.DurationSeconds, 1e-9)
		assert.Equal(tc.basis, c.DurationBasis)
	}
	c := Decode("twenty_json", []byte(`{"schema_version":2,"recording":{"summary":{"markdown":"Summary"}}}`), nil)
	assert.Equal("unsupported_schema", c.Summary.Reason)
}

// The arbitrary JSON domain includes invalid wire shapes as well as valid
// evidence. No shape may manufacture a negative or nonfinite duration.
func FuzzTwentyDurationIsFinite(f *testing.F) {
	f.Add([]byte(`{"schema_version":1,"recording":{"startedAt":"2026-09-01T10:00:00Z","endedAt":"2026-09-01T10:01:00Z"}}`))
	f.Add([]byte(`{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"Hello","start_timestamp":{"relative":1e300}}]}]}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		assert := assert.New(t)
		c := Decode("twenty_json", raw, nil)
		if c.DurationSeconds != nil {
			assert.True(*c.DurationSeconds >= 0 && !math.IsNaN(*c.DurationSeconds) && !math.IsInf(*c.DurationSeconds, 0))
		}
	})
}

func FuzzTwentyPreservesWordText(f *testing.F) {
	f.Add("Hello", "world")
	f.Add("日本語", "🙂")
	f.Fuzz(func(t *testing.T, first, second string) {
		assert := assert.New(t)
		require := require.New(t)
		if !utf8.ValidString(first) || !utf8.ValidString(second) {
			return
		}
		// Empty/whitespace words have no transcript content by provider contract.
		first, second = strings.TrimSpace(first), strings.TrimSpace(second)
		words := []map[string]any{{"text": first}, {"text": second}}
		raw, err := json.Marshal(map[string]any{"schema_version": 1, "recording": map[string]any{"transcript": []any{map[string]any{"words": words}}}})
		require.NoError(err)
		c := Decode("twenty_json", raw, nil)
		want := strings.TrimSpace(first + " " + second)
		if want == "" {
			assert.Equal(StateEmpty, c.Transcript.State)
			return
		}
		require.Len(c.Transcript.Segments, 1)
		assert.Equal(want, c.Transcript.Segments[0].Text)
	})
}

func TestTwentySummaryUnavailableNoticeIsNotASummary(t *testing.T) {
	assert := assert.New(t)
	raw := []byte(`{"schema_version":1,"recording":{"summary":{"markdown":"## Summary unavailable\n\nThe call was too short."},"transcript":[{"words":[{"text":"Hello"}]}]}}`)
	c := Decode("twenty_json", raw, nil)
	assert.Equal(StateUnavailable, c.Summary.State)
	assert.Equal("summary_unavailable", c.Summary.Reason)
	assert.Empty(c.Summary.Text)
	assert.Equal(StateAvailable, c.Transcript.State)
}
