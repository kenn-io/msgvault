package meetingcontent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlaudCanonicalMeetingProjection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte(`{"schema_version":3,"summary_markdown":"Release plan","notes":"Highlights","duration_seconds":90,"transcript_segments":[{"speaker":"Speaker 1","text":"Ship it","offset_seconds":1.25,"end_seconds":2.75}],"source_participants":[{"name":"Speaker 1","role":"speaker"}]}`)
	c := Decode("plaud_json", raw, nil)
	assert.Equal(StateAvailable, c.Summary.State)
	assert.Equal("Highlights", c.Notes.Text)
	assert.Equal(StateAvailable, c.Transcript.State)
	require.Len(c.Transcript.Segments, 1)
	assert.Equal("Speaker 1", c.Transcript.Segments[0].Speaker)
	require.NotNil(c.Transcript.Segments[0].OffsetSeconds)
	require.NotNil(c.Transcript.Segments[0].EndOffsetSeconds)
	assert.InDelta(1.25, *c.Transcript.Segments[0].OffsetSeconds, 1e-9)
	assert.InDelta(2.75, *c.Transcript.Segments[0].EndOffsetSeconds, 1e-9)
	require.NotNil(c.DurationSeconds)
	assert.InDelta(90.0, *c.DurationSeconds, 1e-9)
	assert.Equal(DurationProvider, c.DurationBasis)
	require.Len(c.SourceParticipants, 1)
	assert.Empty(c.SourceParticipants[0].Email)
	assert.Equal(CoverageUnsupported, c.ActionCoverage)
	for _, version := range []string{"1", "2", "3"} {
		wire := strings.Replace(string(raw), `"schema_version":3`, `"schema_version":`+version, 1)
		if version != "3" {
			wire = strings.ReplaceAll(strings.ReplaceAll(wire, `:1.25`, `:1250`), `:2.75`, `:2750`)
		}
		assert.Equal(c, Decode("plaud_json", []byte(wire), nil))
		quoted := strings.ReplaceAll(strings.ReplaceAll(wire, `"offset_seconds":1250`, `"offset_seconds":"1250"`), `"end_seconds":2750`, `"end_seconds":"2750"`)
		quoted = strings.ReplaceAll(strings.ReplaceAll(quoted, `"offset_seconds":1.25`, `"offset_seconds":"1.25"`), `"end_seconds":2.75`, `"end_seconds":"2.75"`)
		assert.Equal(c, Decode("plaud_json", []byte(quoted), nil))
		if version != "3" {
			tiny := strings.ReplaceAll(strings.ReplaceAll(wire, `:1250`, `:1.25`), `:2750`, `:2.75`)
			assert.InDelta(0.00125, *Decode("plaud_json", []byte(tiny), nil).Transcript.Segments[0].OffsetSeconds, 0)
		}
		zero := strings.Replace(wire, `"offset_seconds":1250`, `"offset_seconds":0`, 1)
		zero = strings.Replace(zero, `"offset_seconds":1.25`, `"offset_seconds":0`, 1)
		content := Decode("plaud_json", []byte(zero), nil)
		for _, format := range []Format{FormatSRT, FormatVTT} {
			result, err := Render("synthetic", []Entry{{Content: content}}, PacketOptions{Format: format, MaxBytes: 4096})
			if version == "1" {
				require.ErrorIs(err, ErrInvalidSubtitles)
				assert.Nil(result)
				assert.Nil(content.Transcript.Segments[0].OffsetSeconds)
			} else {
				require.NoError(err)
			}
		}
		zero = strings.ReplaceAll(strings.ReplaceAll(zero, `"end_seconds":2750`, `"end_seconds":0`), `"end_seconds":2.75`, `"end_seconds":0`)
		end := Decode("plaud_json", []byte(zero), nil).Transcript.Segments[0].EndOffsetSeconds
		if version == "3" {
			require.NotNil(end)
			assert.InDelta(0.0, *end, 0)
		} else {
			assert.Nil(end)
		}
	}
	unversioned := Decode("plaud_json", []byte(strings.Replace(string(raw), `"schema_version":3,`, "", 1)), nil)
	assert.Equal(c.Summary, unversioned.Summary)
	assert.Equal(c.Notes, unversioned.Notes)
	assert.Equal("Ship it", unversioned.Transcript.Segments[0].Text)
	for _, format := range []Format{FormatSRT, FormatVTT} {
		_, err := Render("synthetic", []Entry{{Content: unversioned}}, PacketOptions{Format: format, MaxBytes: 4096})
		require.ErrorContains(err, "timestamp units are unknown")
	}
	future := Decode("plaud_json", []byte(strings.Replace(string(raw), `"schema_version":3`, `"schema_version":4`, 1)), nil)
	assert.Equal(StateUnavailable, future.Transcript.State)
	assert.Equal("unsupported_schema", future.Transcript.Reason)
	for _, version := range []string{"1", "2", "3"} {
		for _, field := range []string{`"offset_seconds":1.25,`, `,"end_seconds":2.75`} {
			wire := strings.Replace(string(raw), `"schema_version":3`, `"schema_version":`+version, 1)
			incomplete := Decode("plaud_json", []byte(strings.Replace(wire, field, "", 1)), nil)
			require.Len(incomplete.Transcript.Segments, 1)
			for _, format := range []Format{FormatSRT, FormatVTT} {
				_, err := Render("synthetic", []Entry{{Content: incomplete}}, PacketOptions{Format: format, MaxBytes: 4096})
				require.ErrorIs(err, ErrInvalidSubtitles)
			}
		}
	}
}

func TestPlaudMissingDurationUsesTranscriptSpan(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	for _, wire := range []string{
		`{"schema_version":2,"duration_seconds":0,"transcript_segments":[{"text":"Start","offset_seconds":12000},{"text":"End","offset_seconds":42000}]}`,
		`{"schema_version":3,"duration_seconds":0,"transcript_segments":[{"text":"Start","offset_seconds":12},{"text":"End","offset_seconds":42}]}`,
	} {
		c := Decode("plaud_json", []byte(wire), nil)
		require.NotNil(c.DurationSeconds)
		assert.InDelta(30.0, *c.DurationSeconds, 0)
		assert.Equal(DurationTranscriptSpan, c.DurationBasis)
		absolute := strings.Replace(wire, `"duration_seconds":0`, `"started_at":"2026-01-01T00:00:00Z","ended_at":"2026-01-01T00:00:30Z"`, 1)
		c = Decode("plaud_json", []byte(absolute), nil)
		require.NotNil(c.DurationSeconds)
		assert.InDelta(30.0, *c.DurationSeconds, 0)
		assert.Equal(DurationProvider, c.DurationBasis)
	}
}
