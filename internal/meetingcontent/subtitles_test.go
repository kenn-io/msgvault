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
	return Entry{Content: Content{Transcript: Transcript{State: StateAvailable, Segments: []Segment{{StartedAt: &start, EndedAt: &end, Speaker: "R&D", Text: "Hi --> 世界\r\nnext & done"}}}}}
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
			payload := "R&D: Hi --> 世界\nnext & done"
			if format == "vtt" {
				payload = "R&amp;D: Hi --&gt; 世界\nnext &amp; done"
			}
			want := header + "1\n00:00:00" + separator + "000 --> 00:00:01" + separator + "500\n" + payload + "\n\n"
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
// separators or markup. SRT preserves literal arrows; VTT escapes them.
func FuzzSubtitlePayload(f *testing.F) {
	for _, seed := range []string{"first\n\nsecond", "first\n \nsecond", "A\r\nB", "<v X>& -->", "世界", "\nleading", "trailing\n"} {
		f.Add(seed, "Speaker")
	}
	f.Add("safe", "first\nsecond")
	f.Add("R&D --> 世界", "R&D")
	f.Add("first\n00:00:03,000 --> 00:00:04,000\ninjected", "")
	f.Fuzz(func(t *testing.T, text, speaker string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		entry := subtitleEntry()
		entry.Content.Transcript.Segments[0].Text = text
		entry.Content.Transcript.Segments[0].Speaker = speaker
		normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		unsafe := strings.ContainsAny(speaker, "\r\n")
		for line := range strings.SplitSeq(normalized, "\n") {
			unsafe = unsafe || strings.TrimSpace(line) == ""
		}
		for _, format := range []string{"srt", "vtt"} {
			got, err := RenderSubtitles(entry, format)
			payload := normalized
			if speaker != "" {
				payload = speaker + ": " + payload
			}
			srtUnsafe := strings.ContainsAny(payload, "<{}\\")
			for line := range strings.SplitSeq(payload, "\n") {
				line = strings.TrimSpace(line)
				srtUnsafe = srtUnsafe || len(line) > 0 && line[0] >= '0' && line[0] <= '9' && strings.Contains(line, "-->")
			}
			if unsafe || format == "srt" && srtUnsafe {
				require.Error(t, err)
				assert.Empty(t, got)
				continue
			}
			require.NoError(t, err)
			if format == "vtt" {
				assert.Equal(t, 1, strings.Count(got, " --> "))
				assert.NotContains(t, got, "<")
			} else {
				assert.True(t, strings.HasSuffix(got, payload+"\n\n"))
			}
			assert.Equal(t, 1, strings.Count(strings.TrimPrefix(got, "WEBVTT\n\n"), "\n\n"))
		}
	})
}

func TestSubtitleFormatRefusals(t *testing.T) {
	for _, payload := range []string{"1 < 2 & 3 > 1", `C:\Temp\note.txt`, "literal <b>bold</b>", "A<&>", `literal {\b1}bold`, `literal \N text`, "first\n00:00:03,000 --> 00:00:04,000\ninjected"} {
		for _, label := range []bool{false, true} {
			if label && strings.ContainsAny(payload, "\r\n") {
				continue
			}
			entry := subtitleEntry()
			if label {
				entry.Content.Transcript.Segments[0].Speaker = payload
			} else {
				entry.Content.Transcript.Segments[0].Text = payload
			}
			got, err := RenderSubtitles(entry, "srt")
			require.ErrorContains(t, err, "use --format vtt")
			assert.Empty(t, got)
			got, err = RenderSubtitles(entry, "vtt")
			require.NoError(t, err)
			assert.NotContains(t, got, "<")
		}
	}
}

func TestSubtitleContextOutputBudget(t *testing.T) {
	entry := subtitleEntry()
	entry.Content.Summary = Section{State: StateAvailable, Text: strings.Repeat("summary", 150000)}
	want, err := RenderSubtitles(entry, "srt")
	require.NoError(t, err)
	result, err := Render("archive", []Entry{entry}, PacketOptions{Format: Format("srt"), MaxBytes: len(want)})
	require.NoError(t, err)
	assert.Equal(t, want, result.Content)
	assert.Equal(t, len([]byte(want)), result.ContentBytes)
	assert.False(t, result.Truncated)
	_, err = Render("archive", []Entry{entry}, PacketOptions{Format: Format("srt"), MaxBytes: len(want) - 1})
	require.ErrorIs(t, err, ErrBudgetTooSmall)
}

func TestSubtitleTwentyExplicitOffsets(t *testing.T) {
	content := Decode("twenty_json", []byte(`{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"First","start_timestamp":{"relative":1.25,"absolute":"2026-01-01T00:00:00.100Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:01.600Z"}}]},{"words":[{"text":"Second","start_timestamp":{"relative":4,"absolute":"2026-01-01T00:00:00.103Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:00.603Z"}}]},{"words":[{"text":"Absolute","start_timestamp":{"absolute":"2026-01-01T00:00:01.100Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:01.600Z"}}]}]}}`), nil)
	got, err := RenderSubtitles(Entry{Content: content}, "srt")
	require.NoError(t, err)
	assert.Contains(t, got, "00:00:01,250 --> 00:00:02,750")
	assert.Contains(t, got, "00:00:04,000 --> 00:00:04,500")
	assert.Contains(t, got, "00:00:02,250 --> 00:00:02,750")
}
