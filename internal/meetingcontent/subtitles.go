package meetingcontent

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

const subtitleInteger = `[ \t\f\v]*[+-]?[0-9]+`
const subtitleClock = subtitleInteger + `:` + subtitleInteger + `:` + subtitleInteger + `[,.]` + subtitleInteger

var srtControlText = regexp.MustCompile(`<[^ \t\r\n][^>\r\n]*>|<[ \t]+(?i:b|i|u|s|font)(?:[ \t][^>\r\n]*)?>|\{\\[^}\r\n]*\}|(?m)^` + subtitleClock + `[ \t\f\v]*-->` + subtitleClock)

type subtitleSegment struct {
	Speaker, Text string
	Start, End    float64
}

func RenderSubtitles(rawFormat string, raw []byte, options PacketOptions) (*PacketResult, error) {
	segments, err := subtitleTiming(rawFormat, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSubtitles, err)
	}
	content, err := renderSubtitles(segments, string(options.Format))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSubtitles, err)
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
		if strings.ContainsRune(s.Text, 0) || strings.ContainsRune(s.Speaker, 0) || strings.ContainsAny(strings.TrimSpace(s.Speaker), "\r\n") {
			return "", fmt.Errorf("subtitle format error at segment %d: NUL text or invalid speaker label", i+1)
		}
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		s.Speaker = strings.TrimSpace(s.Speaker)
		text := strings.ReplaceAll(strings.ReplaceAll(s.Text, "\r\n", "\n"), "\r", "\n")
		lines := strings.Split(text, "\n")
		kept := lines[:0]
		for _, line := range lines {
			if format == "srt" {
				line = strings.TrimSpace(line)
			}
			if strings.TrimSpace(line) != "" {
				kept = append(kept, line)
			}
		}
		text = strings.Join(kept, "\n")
		start, end := s.Start, s.End
		if !validSubtitleSeconds(start) || !validSubtitleSeconds(end) || end <= start {
			return "", fmt.Errorf("subtitle timing invalid at segment %d: negative, overflowing, zero-length, or reversed interval", i+1)
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
		} else if srtControlText.MatchString(text) {
			return "", fmt.Errorf("subtitle format error at segment %d: SRT cannot preserve control text; use --format vtt", i+1)
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

func validSubtitleSeconds(seconds float64) bool {
	return finite(seconds) && seconds >= 0 && math.Round(seconds*1000) < float64(math.MaxInt64)
}

func subtitleTimestamp(ms int64, separator string) string {
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3600000, ms/60000%60, ms/1000%60, separator, ms%1000)
}
