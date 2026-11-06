package meetingcontent

import (
	"fmt"
	"time"
)

// FormatTranscriptLine renders a meeting-relative timestamp and source speaker.
func FormatTranscriptLine(offset time.Duration, speaker, text string) string {
	if offset < 0 {
		offset = 0
	}
	total := int(offset.Seconds())
	h, m, s := total/3600, (total%3600)/60, total%60
	stamp := fmt.Sprintf("[%02d:%02d]", m, s)
	if h > 0 {
		stamp = fmt.Sprintf("[%d:%02d:%02d]", h, m, s)
	}
	return stamp + " " + speaker + ": " + text
}
