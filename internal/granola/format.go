package granola

import (
	"fmt"
	"strings"
	"time"
)

// buildBody renders the single body_text shared by FTS and embeddings:
// title, time range, attendee DISPLAY NAMES, the AI summary, and the
// transcript. Raw attendee email addresses are deliberately excluded — they
// reach FTS via the toAddrs column only (calsync precedent).
func buildBody(n *Note) string {
	var b strings.Builder
	writeLine := func(s string) {
		if s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	writeLine(noteTitle(n))
	writeLine(whenLine(n))

	var names []string
	for _, a := range n.Attendees {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	if len(names) > 0 {
		writeLine("Attendees: " + strings.Join(names, ", "))
	}

	summary := n.SummaryMarkdown
	if summary == "" {
		summary = n.SummaryText
	}
	if summary != "" {
		b.WriteString("\n")
		writeLine(strings.TrimSpace(summary))
	}

	if len(n.Transcript) > 0 {
		b.WriteString("\nTranscript:\n")
		base := transcriptStartTime(n)
		for _, seg := range n.Transcript {
			writeLine(formatTranscriptLine(seg.StartTime.Sub(base), speakerLabel(seg.Speaker), seg.Text))
		}
	}
	return strings.TrimSpace(b.String())
}

// noteTitle picks the display/subject title: the note's own title, then the
// calendar event's, then a date-derived fallback so the row is never blank.
func noteTitle(n *Note) string {
	if n.Title != "" {
		return n.Title
	}
	if n.CalendarEvent != nil && n.CalendarEvent.EventTitle != "" {
		return n.CalendarEvent.EventTitle
	}
	if t := noteStartTime(n); !t.IsZero() {
		return "Meeting on " + t.UTC().Format("2006-01-02")
	}
	return "Meeting"
}

// noteStartTime is the universal time axis: scheduled start, then the first
// transcript utterance, then note creation.
func noteStartTime(n *Note) time.Time {
	if n.CalendarEvent != nil && !n.CalendarEvent.ScheduledStartTime.IsZero() {
		return n.CalendarEvent.ScheduledStartTime
	}
	return transcriptStartTime(n)
}

// transcriptStartTime returns the first usable transcript timestamp. Granola
// can omit timestamps on leading or all segments, so note creation is the
// stable fallback for both message dating and transcript-relative offsets.
func transcriptStartTime(n *Note) time.Time {
	for _, segment := range n.Transcript {
		if !segment.StartTime.IsZero() {
			return segment.StartTime
		}
	}
	return n.CreatedAt
}

func whenLine(n *Note) string {
	if n.CalendarEvent == nil || n.CalendarEvent.ScheduledStartTime.IsZero() {
		return ""
	}
	start := n.CalendarEvent.ScheduledStartTime
	if end := n.CalendarEvent.ScheduledEndTime; !end.IsZero() {
		return "When: " + start.Format("2006-01-02 15:04") + " - " + end.Format("15:04")
	}
	return "When: " + start.Format("2006-01-02 15:04")
}

// speakerLabel resolves a display label for a transcript segment: the
// identified name, then the anonymous diarization bucket, then a Me/Them
// fallback derived from the audio source.
func speakerLabel(sp Speaker) string {
	if sp.Name != "" {
		return sp.Name
	}
	if sp.DiarizationLabel != "" {
		return sp.DiarizationLabel
	}
	if sp.Source == "microphone" {
		return "Me"
	}
	return "Them"
}

// formatTranscriptLine renders "[mm:ss] Speaker: text" (or "[h:mm:ss]" past
// the first hour). This line format is the rendering contract shared with the
// circleback importer.
func formatTranscriptLine(offset time.Duration, speaker, text string) string {
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

// snippet is a short preview derived from the body.
func snippet(body string) string {
	const maxSnippetLength = 200
	body = strings.TrimSpace(body)
	runes := []rune(body)
	if len(runes) <= maxSnippetLength {
		return body
	}
	return string(runes[:maxSnippetLength])
}
