package meetingcontent

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Each decimal conversion permits leading C whitespace and a sign. Match a
// complete timing prefix: readers accept trailing coordinates or other data.
const subtitleInteger = `[ \t\r\n\f\x0b]*[+-]?[0-9]+`
const subtitleClock = subtitleInteger + ":" + subtitleInteger + ":" + subtitleInteger + "[,.]" + subtitleInteger

var srtTimingHeader = regexp.MustCompile("^" + subtitleClock + `[ \t\r\n\f\x0b]*-->` + subtitleClock)

// renderSubtitles preserves complete retained intervals without changing entry.
func renderSubtitles(entry Entry, format string) (string, error) {
	transcript := entry.Content.Transcript
	if transcript.State != StateAvailable || transcript.Reason != "" || len(transcript.Segments) == 0 {
		return "", fmt.Errorf("subtitle timing unavailable: complete structured transcript required (state %s, reason %s)", transcript.State, transcript.Reason)
	}
	var origin *time.Time
	relative, absolute := false, false
	for i, s := range transcript.Segments {
		if strings.ContainsRune(s.Text, 0) || strings.ContainsRune(s.Speaker, 0) {
			return "", fmt.Errorf("subtitle format error at segment %d: NUL text or speaker label", i+1)
		}
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		if s.OffsetSeconds != nil || s.EndOffsetSeconds != nil {
			relative = true
		} else {
			absolute = true
			if s.StartedAt != nil && (origin == nil || s.StartedAt.Before(*origin)) {
				origin = s.StartedAt
			}
		}
	}
	if relative && absolute {
		origin = nil
		for _, s := range transcript.Segments {
			if strings.TrimSpace(s.Text) == "" || s.OffsetSeconds == nil && s.EndOffsetSeconds == nil {
				continue
			}
			if s.OffsetSeconds == nil || s.EndOffsetSeconds == nil || s.StartedAt == nil || s.EndedAt == nil {
				return "", errors.New("subtitle timing insufficient: mixed coordinates have no proven recording origin")
			}
			startOrigin, err := subtitleOrigin(*s.StartedAt, *s.OffsetSeconds)
			if err != nil {
				return "", err
			}
			endOrigin, err := subtitleOrigin(*s.EndedAt, *s.EndOffsetSeconds)
			if err != nil {
				return "", err
			}
			if !startOrigin.Equal(endOrigin) || origin != nil && !origin.Equal(startOrigin) {
				return "", errors.New("subtitle timing insufficient: contradictory recording origins")
			}
			origin = &startOrigin
		}
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
		case s.OffsetSeconds != nil && s.EndOffsetSeconds != nil:
			start, end = *s.OffsetSeconds, *s.EndOffsetSeconds
		case s.OffsetSeconds != nil && s.EndOffsetSeconds == nil && s.StartedAt == nil && s.EndedAt != nil && entry.Meeting.SourceType == "hex" && entry.Meeting.OccurredAt != nil:
			// Hex1850 retains end = recording origin + end_ms and start_ms as offset.
			start = *s.OffsetSeconds
			end, err = subtitleSeconds(*entry.Meeting.OccurredAt, *s.EndedAt)
		case s.OffsetSeconds == nil && s.EndOffsetSeconds == nil && s.StartedAt != nil && s.EndedAt != nil && origin != nil:
			start, err = subtitleSeconds(*origin, *s.StartedAt)
			if err == nil {
				end, err = subtitleSeconds(*origin, *s.EndedAt)
			}
		default:
			return "", fmt.Errorf("subtitle timing insufficient at segment %d: complete bounds in one timing coordinate system required", i+1)
		}
		if err != nil {
			return "", fmt.Errorf("subtitle timing at segment %d: %w", i+1, err)
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
				unsafe = unsafe || srtTimingHeader.MatchString(line)
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

// A mixed transcript needs an exact origin at time.Time's native precision;
// coarse clocks or sub-nanosecond offsets cannot prove that conversion.
func subtitleOrigin(stamp time.Time, offset float64) (time.Time, error) {
	nanos := math.Round(offset * float64(time.Second))
	if !finite(nanos) || nanos < 0 || nanos >= float64(math.MaxInt64) {
		return time.Time{}, errors.New("subtitle timing invalid: recording origin offset overflows")
	}
	elapsed := time.Duration(nanos)
	if elapsed.Seconds() != offset {
		return time.Time{}, errors.New("subtitle timing insufficient: recording origin is not exact")
	}
	return stamp.Add(-elapsed), nil
}
