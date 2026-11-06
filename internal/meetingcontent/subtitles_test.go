package meetingcontent

import (
	"encoding/json/v2"
	"html"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mediatranscript"
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
			if format == "srt" {
				parsed, err := mediatranscript.ParseSubRip([]byte(got), "msgvault", "und")
				require.NoError(t, err)
				require.Len(t, parsed.Segments, 1)
				assert.Equal(t, int64(0), parsed.Segments[0].StartMS)
				assert.Equal(t, int64(1500), parsed.Segments[0].EndMS)
				assert.Equal(t, payload, parsed.Segments[0].Text)
			}
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
	entry.Content.Transcript.Segments[0].EndOffsetSeconds = new(offset + 1.5)
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
	content := Decode("twenty_json", []byte(`{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"First","start_timestamp":{"relative":1.25,"absolute":"2026-01-01T00:00:01.250Z"},"end_timestamp":{"relative":2.75,"absolute":"2026-01-01T00:00:02.750Z"}}]},{"words":[{"text":"Second","start_timestamp":{"relative":4,"absolute":"2026-01-01T00:00:04.000Z"},"end_timestamp":{"relative":4.5,"absolute":"2026-01-01T00:00:04.500Z"}}]},{"words":[{"text":"Absolute","start_timestamp":{"absolute":"2026-01-01T00:00:02.250Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:02.750Z"}}]}]}}`), nil)
	got, err = RenderSubtitles(Entry{Content: content}, "srt")
	require.NoError(t, err)
	assert.Equal(t, "1\n00:00:01,250 --> 00:00:02,750\nUnknown speaker: First\n\n2\n00:00:02,250 --> 00:00:02,750\nUnknown speaker: Absolute\n\n3\n00:00:04,000 --> 00:00:04,500\nUnknown speaker: Second\n\n", got)

	content = Decode("twenty_json", []byte(`{"schema_version":1,"recording":{"transcript":[{"participant":{"name":"Example"},"words":[{"text":"Hello","start_timestamp":{"relative":1.2,"absolute":"2026-06-12T10:00:01Z"},"end_timestamp":{"relative":1.6,"absolute":"2026-06-12T10:00:01Z"}},{"text":"there","start_timestamp":{"relative":1.7,"absolute":"2026-06-12T10:00:02Z"},"end_timestamp":{"relative":2.1,"absolute":"2026-06-12T10:00:02Z"}}]}]}}`), nil)
	for _, format := range []string{"srt", "vtt"} {
		got, err = RenderSubtitles(Entry{Content: content}, format)
		require.NoError(t, err)
		separator := ","
		if format == "vtt" {
			separator = "."
		}
		assert.Contains(t, got, "00:00:01"+separator+"200 --> 00:00:02"+separator+"100")
	}

	content = Decode("omi_json", []byte(`{"transcript_segments":[{"text":"Omi","speaker_name":"Example","start":1.2,"end":2.1}]}`), nil)
	got, err = RenderSubtitles(Entry{Content: content}, "srt")
	require.NoError(t, err)
	assert.Contains(t, got, "00:00:01,200 --> 00:00:02,100")

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
			for _, format := range []string{"srt", "vtt"} {
				got, err := RenderSubtitles(entry, format)
				require.Error(t, err)
				assert.Empty(t, got)
			}
		})
	}
	for _, raw := range []string{
		`{"transcript":[{"text":"Before\u0000After","start_time":"2026-01-01T00:00:01Z","end_time":"2026-01-01T00:00:02Z"}]}`,
		`{"transcript":[{"text":"Body","speaker":{"name":"Before\u0000After"},"start_time":"2026-01-01T00:00:01Z","end_time":"2026-01-01T00:00:02Z"}]}`,
	} {
		entry := Entry{Content: Decode("granola_json", []byte(raw), nil)}
		require.True(t, strings.ContainsRune(entry.Content.Transcript.Segments[0].Text, 0) || strings.ContainsRune(entry.Content.Transcript.Segments[0].Speaker, 0))
		for _, format := range []string{"srt", "vtt"} {
			got, err := RenderSubtitles(entry, format)
			assert.ErrorContains(t, err, "NUL")
			assert.Empty(t, got)
		}
	}
	boundary := `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"First","start_timestamp":{"relative":0.1,"absolute":"2026-01-01T00:00:00.100Z"},"end_timestamp":{"relative":0.8,"absolute":"2026-01-01T00:00:00.800Z"}},{"text":"Last","start_timestamp":{"relative":1.1,"absolute":"2026-01-01T00:00:01.100Z"},"end_timestamp":{"relative":2.1,"absolute":"2026-01-01T00:00:02.100Z"}}]}]}}`
	for _, raw := range []string{
		strings.Replace(boundary, `"start_timestamp":{"relative":0.1,"absolute":"2026-01-01T00:00:00.100Z"}`, `"start_timestamp":{}`, 1),
		strings.Replace(boundary, `"end_timestamp":{"relative":2.1,"absolute":"2026-01-01T00:00:02.100Z"}`, `"end_timestamp":{}`, 1),
		strings.Replace(boundary, `"relative":2.1,`, "", 1),
	} {
		_, err := RenderSubtitles(Entry{Content: Decode("twenty_json", []byte(raw), nil)}, "srt")
		assert.Error(t, err, "incomplete boundary cannot be filled from another word or clock")
	}
	contradictory := Decode("twenty_json", []byte(`{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"First","start_timestamp":{"relative":1,"absolute":"2026-01-01T00:00:01Z"},"end_timestamp":{"relative":2,"absolute":"2026-01-01T00:00:02Z"}}]},{"words":[{"text":"Second","start_timestamp":{"relative":4,"absolute":"2026-01-01T00:00:02Z"},"end_timestamp":{"relative":5,"absolute":"2026-01-01T00:00:03Z"}}]},{"words":[{"text":"Absolute","start_timestamp":{"absolute":"2026-01-01T00:00:05Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:06Z"}}]}]}}`), nil)
	_, err := RenderSubtitles(Entry{Content: contradictory}, "vtt")
	assert.ErrorContains(t, err, "origin")

	_, err = RenderSubtitles(subtitleEntry(), "txt")
	require.ErrorContains(t, err, "format")
}

// Unsafe cue layout must fail, while representable payloads cannot inject cue
// separators or markup. SRT preserves literal arrows; VTT escapes them.
func FuzzSubtitlePayload(f *testing.F) {
	for _, seed := range []string{"first\n\nsecond", "first\n \nsecond", "A\r\nB", "<v X>& -->", "世界", "\nleading", "trailing\n"} {
		f.Add(seed, "Speaker")
	}
	for _, payload := range []string{"1 < 2 & 3 > 1", `C:\Temp\note.txt`, "literal <b>bold</b>", "A<&>", `literal {\b1}bold`, `literal \N text`} {
		f.Add(payload, "Speaker")
		f.Add("body", payload)
	}
	f.Add("Before\x00After", "Speaker")
	f.Add("Body", "Before\x00After")
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
		unsafe := strings.ContainsAny(speaker, "\r\n") || strings.ContainsRune(text, 0) || strings.ContainsRune(speaker, 0)
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
				if !unsafe {
					assert.ErrorContains(t, err, "use --format vtt")
				}
				assert.Empty(t, got)
				continue
			}
			require.NoError(t, err)
			lines := strings.SplitN(strings.TrimPrefix(got, "WEBVTT\n\n"), "\n", 3)
			require.Len(t, lines, 3)
			body := strings.TrimSuffix(lines[2], "\n\n")
			if format == "vtt" {
				assert.Equal(t, 1, strings.Count(got, " --> "))
				assert.NotContains(t, body, "<")
				body = html.UnescapeString(body)
			}
			assert.Equal(t, payload, body)
			assert.Equal(t, 1, strings.Count(strings.TrimPrefix(got, "WEBVTT\n\n"), "\n\n"))
		}
	})
}

func TestSubtitleContextOutputBudget(t *testing.T) {
	entry := subtitleEntry()
	entry.Content.Summary = Section{State: StateAvailable, Text: strings.Repeat("summary", 150000)}
	want, err := RenderSubtitles(entry, "srt")
	require.NoError(t, err)
	for _, maxBytes := range []int{4096, len(want)} {
		result, err := Render("archive", []Entry{entry}, PacketOptions{Format: Format("srt"), MaxBytes: maxBytes})
		require.NoError(t, err)
		assert.Equal(t, want, result.Content)
		assert.Equal(t, len([]byte(want)), result.ContentBytes)
		assert.False(t, result.Truncated)
	}
	_, err = Render("archive", []Entry{entry}, PacketOptions{Format: Format("srt"), MaxBytes: len(want) - 1})
	require.ErrorIs(t, err, ErrBudgetTooSmall)
	for _, entries := range [][]Entry{nil, {entry, entry}} {
		_, err := Render("archive", entries, PacketOptions{Format: FormatSRT, MaxBytes: len(want)})
		require.ErrorIs(t, err, ErrInvalidSubtitles)
	}
}
