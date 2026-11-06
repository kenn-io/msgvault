package meetingcontent

import (
	"encoding/json/v2"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func subtitleEntry() Entry {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(1500 * time.Millisecond)
	return Entry{Content: Content{Transcript: Transcript{State: StateAvailable, Segments: []Segment{{StartedAt: &start, EndedAt: &end, Speaker: "A<&>", Text: "Hi --> 世界\r\nnext & <b>"}}}}}
}

func TestRenderSubtitles(t *testing.T) {
	entry := subtitleEntry()
	for _, format := range []string{"srt", "vtt"} {
		t.Run(format, func(t *testing.T) {
			before, err := json.Marshal(entry)
			require.NoError(t, err)
			got, err := RenderSubtitles(entry, format)
			require.NoError(t, err)
			header, separator := "", ","
			if format == "vtt" {
				header, separator = "WEBVTT\n\n", "."
			}
			want := header + "1\n00:00:00" + separator + "000 --> 00:00:01" + separator + "500\nA&lt;&amp;&gt;: Hi --&gt; 世界\nnext &amp; &lt;b&gt;\n\n"
			assert.Equal(t, want, got)
			after, err := json.Marshal(entry)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestSubtitleTiming(t *testing.T) {
	entry := subtitleEntry()
	base := *entry.Content.Transcript.Segments[0].StartedAt
	offset := 360000.9996
	entry.Content.Transcript.Segments[0].OffsetSeconds = &offset
	got, err := RenderSubtitles(entry, "srt")
	require.NoError(t, err)
	assert.Contains(t, got, "100:00:01,000 --> 100:00:02,500")
	// A second cue overlaps; input order breaks equal-start ties.
	earlyEnd := base.Add(time.Second)
	entry.Content.Transcript.Segments = append(entry.Content.Transcript.Segments,
		Segment{StartedAt: &base, EndedAt: &earlyEnd, Text: "second"})
	got, err = RenderSubtitles(entry, "srt")
	require.NoError(t, err)
	assert.Contains(t, got, "2\n100:00:01,000 --> 100:00:02,000\nsecond")
	entry.Meeting.SourceType, entry.Meeting.OccurredAt = "hex", &base
	entry.Content.Transcript.Segments = []Segment{{OffsetSeconds: new(2.25), EndedAt: new(base.Add(4 * time.Second)), Text: "Hex"}}
	got, err = RenderSubtitles(entry, "vtt")
	require.NoError(t, err)
	assert.Contains(t, got, "00:00:02.250 --> 00:00:04.000")
}

func TestSubtitleRefusals(t *testing.T) {
	tests := map[string]func(*Entry){
		"partial":     func(e *Entry) { e.Content.Transcript.Reason = "hex_partial" },
		"rolling":     func(e *Entry) { e.Content.Transcript.Reason = "hex_rolling" },
		"unavailable": func(e *Entry) { e.Content.Transcript.State = StateUnavailable },
		"truncated":   func(e *Entry) { e.Content.Transcript.State = StateTruncated },
		"plain text":  func(e *Entry) { e.Content.Transcript.Segments = nil; e.Content.Transcript.Text = "text" },
		"missing end": func(e *Entry) { e.Content.Transcript.Segments[0].EndedAt = nil },
		"offset only": func(e *Entry) {
			e.Content.Transcript.Segments[0].StartedAt = nil
			e.Content.Transcript.Segments[0].EndedAt = nil
			e.Content.Transcript.Segments[0].OffsetSeconds = new(1.0)
		},
		"unproven mixed": func(e *Entry) {
			e.Content.Transcript.Segments[0].StartedAt = nil
			e.Content.Transcript.Segments[0].OffsetSeconds = new(0.0)
		},
		"hex missing origin": func(e *Entry) {
			e.Meeting.SourceType = "hex"
			e.Content.Transcript.Segments[0].StartedAt = nil
			e.Content.Transcript.Segments[0].OffsetSeconds = new(0.0)
		},
		"negative": func(e *Entry) { e.Content.Transcript.Segments[0].OffsetSeconds = new(-1.0) },
		"nan":      func(e *Entry) { e.Content.Transcript.Segments[0].OffsetSeconds = new(math.NaN()) },
		"infinity": func(e *Entry) { e.Content.Transcript.Segments[0].OffsetSeconds = new(math.Inf(1)) },
		"overflow": func(e *Entry) { e.Content.Transcript.Segments[0].OffsetSeconds = new(math.MaxFloat64) },
		"reversed": func(e *Entry) { s := &e.Content.Transcript.Segments[0]; s.EndedAt = new(s.StartedAt.Add(-time.Second)) },
		"zero milliseconds": func(e *Entry) {
			s := &e.Content.Transcript.Segments[0]
			s.EndedAt = new(s.StartedAt.Add(time.Microsecond))
		},
		"inconsistent origin": func(e *Entry) {
			e.Content.Transcript.Segments[0].OffsetSeconds = new(0.0)
			s := e.Content.Transcript.Segments[0]
			s.OffsetSeconds = new(1.0)
			e.Content.Transcript.Segments = append(e.Content.Transcript.Segments, s)
		},
	}
	for name, modify := range tests {
		t.Run(name, func(t *testing.T) {
			entry := subtitleEntry()
			modify(&entry)
			got, err := RenderSubtitles(entry, "srt")
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
	_, err := RenderSubtitles(subtitleEntry(), "txt")
	require.ErrorContains(t, err, "format")
}

// Unsafe cue layout must fail, while representable payloads cannot inject cue
// separators, markup, or timing arrows. Seeds cover actual retained source text.
func FuzzSubtitlePayload(f *testing.F) {
	for _, seed := range []string{"first\n\nsecond", "first\n \nsecond", "A\r\nB", "<v X>& -->", "世界", "\nleading", "trailing\n"} {
		f.Add(seed, "Speaker")
	}
	f.Add("safe", "first\nsecond")
	f.Fuzz(func(t *testing.T, text, speaker string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		entry := subtitleEntry()
		entry.Content.Transcript.Segments[0].Text = text
		entry.Content.Transcript.Segments[0].Speaker = speaker
		normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		unsafe := strings.ContainsAny(speaker, "\r\n")
		for _, line := range strings.Split(normalized, "\n") {
			unsafe = unsafe || strings.TrimSpace(line) == ""
		}
		for _, format := range []string{"srt", "vtt"} {
			got, err := RenderSubtitles(entry, format)
			if unsafe {
				require.Error(t, err)
				assert.Empty(t, got)
				continue
			}
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(got, " --> "))
			assert.NotContains(t, got, "<")
			assert.Equal(t, 1, strings.Count(strings.TrimPrefix(got, "WEBVTT\n\n"), "\n\n"))
		}
	})
}

func TestSubtitleMixedAbsoluteOrigin(t *testing.T) {
	entry := subtitleEntry()
	origin := entry.Content.Transcript.Segments[0].StartedAt.Add(-2 * time.Second)
	entry.Meeting.SourceType, entry.Meeting.OccurredAt = "hex", &origin
	entry.Content.Transcript.Segments = append(entry.Content.Transcript.Segments, Segment{Text: "relative", OffsetSeconds: new(1.0), EndedAt: new(origin.Add(3 * time.Second))})
	got, err := RenderSubtitles(entry, "srt")
	require.NoError(t, err)
	assert.Contains(t, got, "1\n00:00:01,000 --> 00:00:03,000\nrelative")
	assert.Contains(t, got, "2\n00:00:02,000 --> 00:00:03,500")
}
