package meetingcontent

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// RenderSubtitles exports complete retained cue intervals without changing entry.
// Absolute-only transcripts start at their earliest cue; explicit offsets retain
// their recording coordinates. Cue ends are never estimated.
func RenderSubtitles(entry Entry, format string) (string, error) {
	if format != "srt" && format != "vtt" {
		return "", fmt.Errorf("unsupported subtitle format %q: use srt or vtt", format)
	}
	transcript := entry.Content.Transcript
	if transcript.State != StateAvailable || transcript.Reason != "" || len(transcript.Segments) == 0 {
		return "", fmt.Errorf("subtitle timing unavailable: complete structured transcript required (state %s, reason %s)", transcript.State, transcript.Reason)
	}
	var earliest *time.Time
	for _, s := range transcript.Segments {
		if strings.TrimSpace(s.Text) != "" && s.StartedAt != nil && (earliest == nil || s.StartedAt.Before(*earliest)) {
			earliest = s.StartedAt
		}
	}
	// An absolute start plus explicit offset fixes the common recording origin.
	origin := 0.0
	anchored := false
	for i, s := range transcript.Segments {
		if strings.TrimSpace(s.Text) == "" || s.StartedAt == nil || s.OffsetSeconds == nil {
			continue
		}
		delta, err := subtitleSeconds(*earliest, *s.StartedAt)
		if err != nil {
			return "", err
		}
		candidate := delta - *s.OffsetSeconds
		if anchored && math.Abs(candidate-origin) > 0.000001 {
			return "", fmt.Errorf("subtitle timing insufficient at segment %d: inconsistent offset origin", i+1)
		}
		origin, anchored = candidate, true
	}
	type cue struct {
		start, end int64
		text       string
	}
	cues := make([]cue, 0, len(transcript.Segments))
	for i, s := range transcript.Segments {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		text := strings.ReplaceAll(strings.ReplaceAll(s.Text, "\r\n", "\n"), "\r", "\n")
		if strings.ContainsAny(s.Speaker, "\r\n") {
			return "", fmt.Errorf("subtitle format error at segment %d: multiline speaker label", i+1)
		}
		for line := range strings.SplitSeq(text, "\n") {
			if strings.TrimSpace(line) == "" {
				return "", fmt.Errorf("subtitle format error at segment %d: blank cue line", i+1)
			}
		}
		var start, end float64
		var err error
		switch {
		case s.StartedAt != nil && s.EndedAt != nil:
			start, err = subtitleSeconds(*earliest, *s.StartedAt)
			if err == nil {
				var duration float64
				duration, err = subtitleSeconds(*s.StartedAt, *s.EndedAt)
				start -= origin
				end = start + duration
			}
		case s.StartedAt == nil && s.OffsetSeconds != nil && s.EndedAt != nil && entry.Meeting.SourceType == "hex" && entry.Meeting.OccurredAt != nil:
			// Hex retains end = recording origin + end_ms, and start_ms as offset.
			start = *s.OffsetSeconds
			end, err = subtitleSeconds(*entry.Meeting.OccurredAt, *s.EndedAt)
		default:
			return "", fmt.Errorf("subtitle timing insufficient at segment %d: explicit start and end required", i+1)
		}
		if err != nil {
			return "", fmt.Errorf("subtitle timing at segment %d: %w", i+1, err)
		}
		if s.OffsetSeconds != nil && !validSubtitleSeconds(*s.OffsetSeconds) {
			return "", fmt.Errorf("subtitle timing invalid at segment %d: invalid start offset", i+1)
		}
		if !validSubtitleSeconds(start) || !validSubtitleSeconds(end) || end <= start {
			return "", fmt.Errorf("subtitle timing invalid at segment %d: negative, overflowing, or reversed interval", i+1)
		}
		startMS, endMS := int64(math.Round(start*1000)), int64(math.Round(end*1000))
		if endMS <= startMS {
			return "", fmt.Errorf("subtitle timing insufficient at segment %d: zero millisecond interval", i+1)
		}
		if s.Speaker != "" {
			text = s.Speaker + ": " + text
		}
		if format == "vtt" {
			text = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
		} else {
			unsafe := strings.ContainsAny(text, "<{}\\")
			for line := range strings.SplitSeq(text, "\n") {
				line = strings.TrimSpace(line)
				// SRT readers can start another cue at a timing line without a blank.
				unsafe = unsafe || len(line) > 0 && line[0] >= '0' && line[0] <= '9' && strings.Contains(line, "-->")
			}
			if unsafe {
				return "", fmt.Errorf("subtitle format error at segment %d: SRT cannot preserve literal markup or timing/control syntax; use --format vtt", i+1)
			}
		}
		cues = append(cues, cue{startMS, endMS, text})
	}
	if len(cues) == 0 {
		return "", errors.New("subtitle timing unavailable: no nonblank cues")
	}
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].start < cues[j].start })
	var out strings.Builder
	separator := ","
	if format == "vtt" {
		out.WriteString("WEBVTT\n\n")
		separator = "."
	}
	for i, c := range cues {
		fmt.Fprintf(&out, "%d\n%s --> %s\n%s\n\n", i+1, subtitleTimestamp(c.start, separator), subtitleTimestamp(c.end, separator), c.text)
	}
	return out.String(), nil
}

func subtitleSeconds(start, end time.Time) (float64, error) {
	delta := end.Sub(start)
	if !start.Add(delta).Equal(end) {
		return 0, errors.New("interval overflows supported timing range")
	}
	return delta.Seconds(), nil
}

func validSubtitleSeconds(seconds float64) bool {
	return finite(seconds) && seconds >= 0 && math.Round(seconds*1000) < float64(math.MaxInt64)
}

func subtitleTimestamp(ms int64, separator string) string {
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3600000, ms/60000%60, ms/1000%60, separator, ms%1000)
}
