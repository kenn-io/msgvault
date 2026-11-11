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

var srtTag = regexp.MustCompile(`<[^ \t\r\n][^>\r\n]*>`)

var srtTimingHeader = regexp.MustCompile("^" + subtitleClock + `[ \t\r\n\f\x0b]*-->` + subtitleClock)

type subtitleSegment struct {
	Speaker, Text string
	Start, End    float64
}

func RenderSubtitles(rawFormat string, raw []byte, options PacketOptions) (*PacketResult, error) {
	segments, err := subtitleTiming(rawFormat, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSubtitles, err)
	}
	content, err := renderSubtitles(segments, string(options.Format))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSubtitles, err)
	}
	if len(content) > options.MaxBytes {
		return nil, ErrBudgetTooSmall
	}
	return &PacketResult{SchemaVersion: packetSchemaVersion, Format: options.Format, Content: content, ContentBytes: len(content), OmittedMessageIDs: []int64{}}, nil
}

func renderSubtitles(segments []subtitleSegment, format string) (string, error) {
	type cue struct {
		start, end int64
		text       string
	}
	cues := make([]cue, 0, len(segments))
	for i, s := range segments {
		if strings.ContainsRune(s.Text, 0) || strings.ContainsRune(s.Speaker, 0) {
			return "", fmt.Errorf("subtitle format error at segment %d: NUL text or speaker label", i+1)
		}
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		s.Speaker = strings.TrimSpace(s.Speaker)
		text := strings.ReplaceAll(strings.ReplaceAll(s.Text, "\r\n", "\n"), "\r", "\n")
		if strings.ContainsAny(s.Speaker, "\r\n") {
			return "", fmt.Errorf("subtitle format error at segment %d: multiline speaker label", i+1)
		}
		for line := range strings.SplitSeq(text, "\n") {
			if strings.TrimSpace(line) == "" {
				return "", fmt.Errorf("subtitle format error at segment %d: blank cue line", i+1)
			}
		}
		start, end := s.Start, s.End
		if !validSubtitleSeconds(start) || !validSubtitleSeconds(end) || end <= start {
			return "", fmt.Errorf("subtitle timing invalid at segment %d: negative, overflowing, or reversed interval", i+1)
		}
		startMS, endMS := int64(math.Round(start*1000)), int64(math.Round(end*1000))
		if endMS <= startMS {
			return "", fmt.Errorf("subtitle timing insufficient at segment %d: zero millisecond interval", i+1)
		}
		sourceText := text
		if s.Speaker != "" {
			text = s.Speaker + ": " + text
		}
		if format == "vtt" {
			text = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
		} else {
			unsafe := strings.ContainsAny(text, "{}\\") || srtTag.MatchString(text)
			for line := range strings.SplitSeq(sourceText, "\n") {
				unsafe = unsafe || strings.Trim(line, " \t\r\f\v") != line
			}
			for line := range strings.SplitSeq(text, "\n") {
				// FFmpeg splits SRT input after 4095 UTF-8 bytes, exposing new headers.
				unsafe = unsafe || len(line) > 4095 || srtTimingHeader.MatchString(line) || strings.Trim(line, " \t\r\f\v") != line
			}
			if unsafe {
				return "", fmt.Errorf("subtitle format error at segment %d: SRT cannot preserve markup, timing syntax, leading/trailing whitespace, or lines over 4095 UTF-8 bytes; use --format vtt", i+1)
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
