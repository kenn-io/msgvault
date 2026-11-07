package meetingcontent

import (
	"encoding/hex"
	"encoding/json/v2"
	"html"
	"math"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/docbank/document/mediatranscript"
)

func subtitleEntry() Entry {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(1500 * time.Millisecond)
	return Entry{Content: Content{Transcript: Transcript{State: StateAvailable, Segments: []Segment{{StartedAt: &start, EndedAt: &end, Speaker: "R&D", Text: "Hi --> 世界\r\nnext & done"}}}}}
}

func TestRenderSubtitles(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	entry := subtitleEntry()
	for _, format := range []string{"srt", "vtt"} {
		t.Run(format, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			before, err := json.Marshal(entry)
			require.NoError(err)
			got, err := renderSubtitles(entry, format)
			require.NoError(err)
			header, separator := "", ","
			if format == "vtt" {
				header, separator = "WEBVTT\n\n", "."
			}
			payload := "R&D: Hi --> 世界\nnext & done"
			if format == "vtt" {
				payload = "R&amp;D: Hi --&gt; 世界\nnext &amp; done"
			}
			want := header + "1\n00:00:00" + separator + "000 --> 00:00:01" + separator + "500\n" + payload + "\n\n"
			assert.Equal(want, got)
			if format == "srt" {
				parsed, err := mediatranscript.ParseSubRip([]byte(got), "msgvault", "und")
				require.NoError(err)
				require.Len(parsed.Segments, 1)
				assert.Equal(int64(0), parsed.Segments[0].StartMS)
				assert.Equal(int64(1500), parsed.Segments[0].EndMS)
				assert.Equal(payload, parsed.Segments[0].Text)
			}
			after, err := json.Marshal(entry)
			require.NoError(err)
			assert.Equal(before, after)
		})
	}

	accepted := []string{"Transition\n2025 --> 2026", "before\n00:00:03 --> 00:00:04", "before\nTime 00:00:03,000 --> 00:00:04,000", "before\n00 :00:03,000 --> 00:00:04,000", "before\n00:00:03 ,000 --> 00:00:04,000", "before\n\u00a000:00:03,000 --> 00:00:04,000"}
	headers := []string{"+00:00:03,000 --> 00:00:04,000", "-00:00:03,000 --> 00:00:04,000", "0:0:3.1-->0:0:4,123456 trailing", "\t0:\v+0:\f3,1-->0:0:4.2 X1:1"}
	for component := range 8 {
		parts := []string{"00", "00", "03", "000", "00", "00", "04", "000"}
		parts[component] = "+" + parts[component]
		headers = append(headers, parts[0]+":"+parts[1]+":"+parts[2]+","+parts[3]+" --> "+parts[4]+":"+parts[5]+":"+parts[6]+","+parts[7])
	}
	for _, text := range append(accepted, headers...) {
		t.Run(text, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			refuse := !slices.Contains(accepted, text)
			body := text
			if refuse {
				body = "before\n" + text + "\nafter"
			}
			raw, err := json.Marshal(map[string]any{"transcript_segments": []any{map[string]any{"text": body, "start": 1.2, "end": 2.1}}})
			require.NoError(err)
			entry := Entry{Content: Decode("omi_json", raw, nil)}
			result, err := Render("archive", []Entry{entry}, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
			if refuse {
				require.ErrorContains(err, "use --format vtt")
				assert.Nil(result)
				_, err = Render("archive", []Entry{entry}, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
				require.NoError(err)
			} else {
				require.NoError(err)
				parsed, err := mediatranscript.ParseSubRip([]byte(result.Content), "msgvault", "und")
				require.NoError(err)
				require.Len(parsed.Segments, 1)
				assert.Equal(int64(1200), parsed.Segments[0].StartMS)
				assert.Equal(int64(2100), parsed.Segments[0].EndMS)
				assert.Equal("Unknown speaker: "+body, parsed.Segments[0].Text)
				if ffmpeg, err := exec.LookPath("ffmpeg"); err == nil {
					assert.Equal([]string{"0:00:01.20,0:00:02.10," + strings.ReplaceAll("Unknown speaker: "+body, "\n", `\N`)}, subtitleDisplay(t, ffmpeg, "srt", result.Content))
				}
			}
		})
	}
	raw := []byte(`{"transcript_segments":[{"speaker_name":"00","text":"00:03,000-->00:00:04,000\nAfter","start":1.2,"end":2.1}]}`)
	result, err := Render("archive", []Entry{{Content: Decode("omi_json", raw, nil)}}, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
	require.ErrorContains(err, "use --format vtt")
	assert.Nil(result)
	// Expected text and labels come from retained producer strings, before Decode.
	for format, raw := range map[string]string{
		"granola_json": `{"transcript":[{"speaker":{"name":"  Label  "},"text":"  first\n  second  ","start_time":"2026-01-01T00:00:00Z","end_time":"2026-01-01T00:00:01.5Z"}]}`,
		"omi_json":     `{"transcript_segments":[{"speaker_name":"  Label  ","text":"  first\n  second  ","start":0,"end":1.5}]}`,
		"plaud_json":   `{"schema_version":2,"transcript_segments":[{"speaker":"  Label  ","text":"  first\n  second  ","offset_seconds":0,"end_seconds":1.5}]}`,
		"twenty_json":  `{"schema_version":1,"recording":{"transcript":[{"participant":{"name":"  Label  "},"words":[{"text":"  first\n  second  ","start_timestamp":{"relative":0},"end_timestamp":{"relative":1.5}}]}]}}`,
	} {
		content := Decode(format, []byte(raw), nil)
		require.Len(content.Transcript.Segments, 1)
		assert.Equal("  first\n  second  ", content.Transcript.Segments[0].Text, format)
		assert.Equal("  Label  ", content.Transcript.Segments[0].Speaker, format)
		result, err := Render("archive", []Entry{{Content: content}}, PacketOptions{Format: FormatVTT, MaxBytes: 4096})
		require.NoError(err)
		assert.Equal("WEBVTT\n\n1\n00:00:00.000 --> 00:00:01.500\n  Label  :   first\n  second  \n\n", result.Content, format)
		result, err = Render("archive", []Entry{{Content: content}}, PacketOptions{Format: FormatSRT, MaxBytes: 4096})
		require.ErrorContains(err, "use --format vtt")
		assert.Nil(result)
	}
}

func TestSubtitleTiming(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	entry := subtitleEntry()
	base := *entry.Content.Transcript.Segments[0].StartedAt
	offset := 360000.9996
	entry.Content.Transcript.Segments[0].OffsetSeconds = &offset
	entry.Content.Transcript.Segments[0].EndOffsetSeconds = new(offset + 1.5)
	got, err := renderSubtitles(entry, "srt")
	require.NoError(err)
	assert.Contains(got, "100:00:01,000 --> 100:00:02,500")
	// A second cue overlaps; input order breaks equal-start ties.
	earlyEnd := base.Add(time.Second)
	entry.Content.Transcript.Segments = append(entry.Content.Transcript.Segments,
		Segment{StartedAt: &base, EndedAt: &earlyEnd, Text: "second"})
	got, err = renderSubtitles(entry, "srt")
	require.NoError(err)
	assert.Contains(got, "2\n100:00:01,000 --> 100:00:02,000\nsecond")
	entry.Meeting.SourceType, entry.Meeting.OccurredAt = "hex", &base
	entry.Content.Transcript.Segments = []Segment{{OffsetSeconds: new(2.25), EndedAt: new(base.Add(4 * time.Second)), Text: "Hex"}}
	got, err = renderSubtitles(entry, "vtt")
	require.NoError(err)
	assert.Contains(got, "00:00:02.250 --> 00:00:04.000")
	raw := `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"First","start_timestamp":{"relative":1.118,"absolute":"2026-01-01T00:00:01.118Z"},"end_timestamp":{"relative":2.75,"absolute":"2026-01-01T00:00:02.750Z"}}]},{"words":[{"text":"Second","start_timestamp":{"relative":4,"absolute":"2026-01-01T00:00:04.000Z"},"end_timestamp":{"relative":4.5,"absolute":"2026-01-01T00:00:04.500Z"}}]},{"words":[{"text":"Absolute","start_timestamp":{"absolute":"2026-01-01T00:00:02.250Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:02.750Z"}}]}]}}`
	content := Decode("twenty_json", []byte(raw), nil)
	// A fraction below native nanosecond precision still cannot prove an origin.
	unrepresentable := Decode("twenty_json", []byte(strings.Replace(raw, `"relative":1.118`, `"relative":1.1180000001`, 1)), nil)
	for _, format := range []Format{FormatSRT, FormatVTT} {
		t.Run("exact_origin_"+string(format), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			result, err := Render("archive", []Entry{{Content: content}}, PacketOptions{Format: format, MaxBytes: 4096})
			require.NoError(err)
			want := "1\n00:00:01,118 --> 00:00:02,750\nUnknown speaker: First\n\n2\n00:00:02,250 --> 00:00:02,750\nUnknown speaker: Absolute\n\n3\n00:00:04,000 --> 00:00:04,500\nUnknown speaker: Second\n\n"
			if format == FormatVTT {
				want = "WEBVTT\n\n" + strings.ReplaceAll(want, ",", ".")
			}
			assert.Equal(want, result.Content)
			result, err = Render("archive", []Entry{{Content: unrepresentable}}, PacketOptions{Format: format, MaxBytes: 4096})
			require.ErrorContains(err, "precise start time")
			assert.Nil(result)
		})
	}

	for name, bounds := range map[string][2]string{
		"absolute":       {`{"absolute":"2026-01-01T00:00:10Z"}`, `{"absolute":"2026-01-01T00:00:12Z"}`},
		"relative_start": {`{"relative":10}`, `{"absolute":"2026-01-01T00:00:12Z"}`},
		"relative_end":   {`{"absolute":"2026-01-01T00:00:10Z"}`, `{"relative":12}`},
	} {
		for _, format := range []Format{FormatSRT, FormatVTT} {
			t.Run("retained_origin_"+name+"_"+string(format), func(t *testing.T) {
				assert, require := Assert.New(t), Require.New(t)
				raw := `{"schema_version":1,"recording":{"startedAt":"2026-01-01T00:00:00Z","transcript":[{"words":[{"text":"Anchored","start_timestamp":` + bounds[0] + `,"end_timestamp":` + bounds[1] + `}]}]},"calendar_event":{"startsAt":"2025-01-01T00:00:00Z","endsAt":"2025-01-01T01:00:00Z"}}`
				result, err := Render("synthetic", []Entry{{Content: Decode("twenty_json", []byte(raw), nil)}}, PacketOptions{Format: format, MaxBytes: 4096})
				require.NoError(err)
				want := "00:00:10,000 --> 00:00:12,000"
				if format == FormatVTT {
					want = "00:00:10.000 --> 00:00:12.000"
				}
				assert.Contains(result.Content, want)
			})
		}
	}

	content = Decode("twenty_json", []byte(`{"schema_version":1,"recording":{"transcript":[{"participant":{"name":"Example"},"words":[{"text":"Hello","start_timestamp":{"relative":1.2,"absolute":"2026-06-12T10:00:01Z"},"end_timestamp":{"relative":1.6,"absolute":"2026-06-12T10:00:01Z"}},{"text":"there","start_timestamp":{"relative":1.7,"absolute":"2026-06-12T10:00:02Z"},"end_timestamp":{"relative":2.1,"absolute":"2026-06-12T10:00:02Z"}}]}]}}`), nil)
	for _, format := range []string{"srt", "vtt"} {
		got, err = renderSubtitles(Entry{Content: content}, format)
		require.NoError(err)
		separator := ","
		if format == "vtt" {
			separator = "."
		}
		assert.Contains(got, "00:00:01"+separator+"200 --> 00:00:02"+separator+"100")
	}

	content = Decode("omi_json", []byte(`{"transcript_segments":[{"text":"Omi","speaker_name":"Example","start":1.2,"end":2.1}]}`), nil)
	got, err = renderSubtitles(Entry{Content: content}, "srt")
	require.NoError(err)
	assert.Contains(got, "00:00:01,200 --> 00:00:02,100")
}

func TestSubtitleRefusals(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
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
			e.Content.Transcript.Segments = append(e.Content.Transcript.Segments, subtitleEntry().Content.Transcript.Segments[0])
		},
		"hex missing origin": func(e *Entry) {
			e.Meeting.SourceType = "hex"
			e.Content.Transcript.Segments[0].StartedAt = nil
			e.Content.Transcript.Segments[0].OffsetSeconds = new(0.0)
		},
		"overflow": func(e *Entry) {
			e.Content.Transcript.Segments[0].OffsetSeconds = new(0.0)
			e.Content.Transcript.Segments[0].EndOffsetSeconds = new(math.MaxFloat64)
		},
		"reversed": func(e *Entry) { s := &e.Content.Transcript.Segments[0]; s.EndedAt = new(s.StartedAt.Add(-time.Second)) },
		"zero milliseconds": func(e *Entry) {
			s := &e.Content.Transcript.Segments[0]
			s.EndedAt = new(s.StartedAt.Add(time.Microsecond))
		},
	}
	diagnostics := map[string]string{
		"overflow":       "negative, overflowing, or reversed interval",
		"unproven mixed": "recording start is unknown",
	}
	for name, modify := range tests {
		t.Run(name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			entry := subtitleEntry()
			modify(&entry)
			for _, format := range []string{"srt", "vtt"} {
				got, err := renderSubtitles(entry, format)
				require.Error(err)
				if diagnostic := diagnostics[name]; diagnostic != "" {
					require.ErrorContains(err, diagnostic)
				}
				assert.Empty(got)
			}
		})
	}
	for _, raw := range []string{
		`{"transcript":[{"text":"Before\u0000After","start_time":"2026-01-01T00:00:01Z","end_time":"2026-01-01T00:00:02Z"}]}`,
		`{"transcript":[{"text":"Body","speaker":{"name":"Before\u0000After"},"start_time":"2026-01-01T00:00:01Z","end_time":"2026-01-01T00:00:02Z"}]}`,
	} {
		entry := Entry{Content: Decode("granola_json", []byte(raw), nil)}
		require.True(strings.ContainsRune(entry.Content.Transcript.Segments[0].Text, 0) || strings.ContainsRune(entry.Content.Transcript.Segments[0].Speaker, 0))
		for _, format := range []string{"srt", "vtt"} {
			got, err := renderSubtitles(entry, format)
			require.ErrorContains(err, "NUL")
			assert.Empty(got)
		}
	}
	for _, payload := range [][2]string{{"first\n\nsecond", "Speaker"}, {"first\n \nsecond", "Speaker"}, {"text", "first\nsecond"}, {"Before\x00After", "Speaker"}, {"text", "Before\x00After"}, {"literal <b>bold", "Speaker"}, {"text", "A<&>"}, {`literal {\b1}`, "Speaker"}, {`literal \N`, "Speaker"}} {
		entry := subtitleEntry()
		entry.Content.Transcript.Segments[0].Text, entry.Content.Transcript.Segments[0].Speaker = payload[0], payload[1]
		got, err := renderSubtitles(entry, "srt")
		require.Error(err)
		assert.Empty(got)
	}
	boundary := `{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"First","start_timestamp":{"relative":0.1,"absolute":"2026-01-01T00:00:00.100Z"},"end_timestamp":{"relative":0.8,"absolute":"2026-01-01T00:00:00.800Z"}},{"text":"Last","start_timestamp":{"relative":1.1,"absolute":"2026-01-01T00:00:01.100Z"},"end_timestamp":{"relative":2.1,"absolute":"2026-01-01T00:00:02.100Z"}}]}]}}`
	for _, raw := range []string{
		strings.Replace(boundary, `"start_timestamp":{"relative":0.1,"absolute":"2026-01-01T00:00:00.100Z"}`, `"start_timestamp":{}`, 1),
		strings.Replace(boundary, `"end_timestamp":{"relative":2.1,"absolute":"2026-01-01T00:00:02.100Z"}`, `"end_timestamp":{}`, 1),
		strings.Replace(boundary, `"relative":2.1,`, "", 1),
	} {
		_, err := renderSubtitles(Entry{Content: Decode("twenty_json", []byte(raw), nil)}, "srt")
		require.Error(err, "incomplete boundary cannot be filled from another word or clock")
	}
	contradictory := Decode("twenty_json", []byte(`{"schema_version":1,"recording":{"transcript":[{"words":[{"text":"First","start_timestamp":{"relative":1,"absolute":"2026-01-01T00:00:01Z"},"end_timestamp":{"relative":2,"absolute":"2026-01-01T00:00:02Z"}}]},{"words":[{"text":"Second","start_timestamp":{"relative":4,"absolute":"2026-01-01T00:00:02Z"},"end_timestamp":{"relative":5,"absolute":"2026-01-01T00:00:03Z"}}]},{"words":[{"text":"Absolute","start_timestamp":{"absolute":"2026-01-01T00:00:05Z"},"end_timestamp":{"absolute":"2026-01-01T00:00:06Z"}}]}]}}`), nil)
	_, err := renderSubtitles(Entry{Content: contradictory}, "vtt")
	require.ErrorContains(err, "timestamps disagree")
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
	ffmpeg, parserErr := exec.LookPath("ffmpeg")
	if parserErr != nil {
		f.Log("FFmpeg unavailable: portable parsing runs; player semantic fuzz layer skipped")
	}
	f.Add("  first\n  second  ", "  Label  ")
	f.Add("before\n+00:00:03,000 --> 00:00:04,000\nafter", "")
	f.Add("before\n-00:00:03,000 --> 00:00:04,000\nafter", "")
	f.Fuzz(func(t *testing.T, text, speaker string) {
		assert, require := Assert.New(t), Require.New(t)
		if len(text)+len(speaker) > 512 {
			return
		}
		// Hex encoding constructs an independently known-valid text domain. Every fuzz
		// input must successfully export this case, even if its raw case refuses.
		for _, input := range []struct {
			text, speaker string
			valid         bool
		}{
			{text, speaker, false},
			{"Text:" + hex.EncodeToString([]byte(text)) + "\nNext --> 世界 & done", "R&D:" + hex.EncodeToString([]byte(speaker)), true},
		} {
			text, speaker := input.text, input.speaker
			if !utf8.ValidString(text) || !utf8.ValidString(speaker) || strings.TrimSpace(text) == "" {
				continue
			}
			raw, err := json.Marshal(map[string]any{"transcript_segments": []any{map[string]any{"text": text, "speaker_name": speaker, "start": 0, "end": 1.5}}})
			require.NoError(err)
			entry := Entry{Content: Decode("omi_json", raw, nil)}
			require.Len(entry.Content.Transcript.Segments, 1)
			if strings.TrimSpace(speaker) == "" {
				speaker = "Unknown speaker"
			}
			assert.Equal(text, entry.Content.Transcript.Segments[0].Text)
			assert.Equal(speaker, entry.Content.Transcript.Segments[0].Speaker)
			normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
			for _, format := range []string{"srt", "vtt"} {
				got, err := renderSubtitles(entry, format)
				payload := normalized
				if speaker != "" {
					payload = speaker + ": " + payload
				}
				if input.valid {
					require.NoError(err)
				}
				if err != nil {
					assert.Empty(got)
					continue
				}
				if format == "srt" {
					parsed, err := mediatranscript.ParseSubRip([]byte(got), "msgvault", "und")
					require.NoError(err)
					require.Len(parsed.Segments, 1)
					assert.Equal(int64(0), parsed.Segments[0].StartMS)
					assert.Equal(int64(1500), parsed.Segments[0].EndMS)
					assert.Equal(payload, parsed.Segments[0].Text)
					if parserErr == nil {
						assert.Equal([]string{"0:00:00.00,0:00:01.50," + strings.ReplaceAll(payload, "\n", `\N`)}, subtitleDisplay(t, ffmpeg, format, got))
					}
				}
				lines := strings.SplitN(strings.TrimPrefix(got, "WEBVTT\n\n"), "\n", 3)
				require.Len(lines, 3)
				body := strings.TrimSuffix(lines[2], "\n\n")
				if format == "vtt" {
					assert.Equal("00:00:00.000 --> 00:00:01.500", lines[1])
					assert.Equal(1, strings.Count(got, " --> "))
					assert.NotContains(body, "<")
					body = html.UnescapeString(body)
				}
				assert.Equal(payload, body)
				assert.Equal(1, strings.Count(strings.TrimPrefix(got, "WEBVTT\n\n"), "\n\n"))
			}
		}
	})
}

func TestSubtitleContextOutputBudget(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	entry := subtitleEntry()
	entry.Content.Summary = Section{State: StateAvailable, Text: strings.Repeat("summary", 150000)}
	want, err := renderSubtitles(entry, "srt")
	require.NoError(err)
	for _, maxBytes := range []int{4096, len(want)} {
		result, err := Render("archive", []Entry{entry}, PacketOptions{Format: Format("srt"), MaxBytes: maxBytes})
		require.NoError(err)
		assert.Equal(want, result.Content)
		assert.Equal(len([]byte(want)), result.ContentBytes)
		assert.False(result.Truncated)
	}
	_, err = Render("archive", []Entry{entry}, PacketOptions{Format: Format("srt"), MaxBytes: len(want) - 1})
	require.ErrorIs(err, ErrBudgetTooSmall)
}
