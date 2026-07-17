package circleback

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// buildBody renders the single body_text shared by FTS and embeddings:
// title, time line, attendee DISPLAY NAMES, the notes markdown, action
// items, and the transcript. Attendee email addresses stay out of the body
// (they reach FTS via the toAddrs column only) — same contract as the
// granola importer, including the "[mm:ss] Speaker: text" transcript lines.
func buildBody(m *Meeting, tr *Transcript) string {
	var b strings.Builder
	writeLine := func(s string) {
		if s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	writeLine(meetingTitle(m))
	if start := m.StartedAt(); !start.IsZero() {
		writeLine("When: " + start.UTC().Format("2006-01-02 15:04"))
	}

	var names []string
	for _, a := range m.Attendees {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	if len(names) > 0 {
		writeLine("Attendees: " + strings.Join(names, ", "))
	}

	if notes := strings.TrimSpace(m.NotesMarkdown()); notes != "" {
		b.WriteString("\n")
		writeLine(notes)
	}

	var actionLines []string
	for _, ai := range m.ActionItems {
		title := ai.DisplayTitle()
		if title == "" {
			continue
		}
		line := "- " + title
		if assignee := ai.AssigneeLabel(); assignee != "" {
			line += " (" + assignee + ")"
		}
		if ai.Status != "" {
			line += " [" + ai.Status + "]"
		}
		actionLines = append(actionLines, line)
	}
	if len(actionLines) > 0 {
		b.WriteString("\nAction items:\n")
		for _, line := range actionLines {
			writeLine(line)
		}
	}

	var insightLines []string
	for _, insight := range m.Insights {
		title := insight.DisplayTitle()
		content := insight.DisplayContent()
		switch {
		case title != "" && content != "":
			insightLines = append(insightLines, "- "+title+": "+content)
		case title != "":
			insightLines = append(insightLines, "- "+title)
		case content != "":
			insightLines = append(insightLines, "- "+content)
		}
	}
	if len(insightLines) > 0 {
		b.WriteString("\nInsights:\n")
		for _, line := range insightLines {
			writeLine(line)
		}
	}

	var tags []string
	for _, tag := range m.Tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	if len(tags) > 0 {
		b.WriteString("\n")
		writeLine("Tags: " + strings.Join(tags, ", "))
	}

	writeTranscript(&b, tr)
	return strings.TrimSpace(b.String())
}

// writeTranscript appends the transcript section: structured entries as
// offset-stamped lines, or the plain-text form verbatim.
func writeTranscript(b *strings.Builder, tr *Transcript) {
	if !hasTranscriptContent(tr) {
		return
	}
	entries := tr.ContentEntries()
	if len(entries) == 0 {
		if text := strings.TrimSpace(tr.Text); text != "" {
			b.WriteString("\nTranscript:\n")
			b.WriteString(text)
			b.WriteString("\n")
		}
		return
	}
	b.WriteString("\nTranscript:\n")
	base := entryOffsetBase(entries)
	for _, e := range entries {
		offset := entryOffset(e, base)
		b.WriteString(formatTranscriptLine(offset, e.SpeakerLabel(), e.Utterance()))
		b.WriteString("\n")
	}
}

// hasTranscriptContent centralizes the content-bearing test used by rendering,
// metadata, and archive recovery. Transcript.Classification is the tolerant
// decoder contract; segment count alone cannot recognize plain-text archives.
func hasTranscriptContent(tr *Transcript) bool {
	return tr != nil && tr.Classification() == TranscriptPresent
}

// entryOffsetBase returns the first absolute timestamp. Numeric values remain
// zero-based seconds independently, including in mixed-shape transcripts.
func entryOffsetBase(entries []TranscriptEntry) time.Time {
	for _, e := range entries {
		for _, value := range e.timestampValues() {
			if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				continue
			}
			if t := parseFlexibleTime(value); !t.IsZero() {
				return t
			}
		}
	}
	return time.Time{}
}

// entryOffset computes an entry's offset from the base: numeric seconds when
// present, else parsed timestamp minus base.
func entryOffset(e TranscriptEntry, base time.Time) time.Duration {
	if secs, ok := e.numericOffset(); ok {
		return time.Duration(secs * float64(time.Second))
	}
	if base.IsZero() {
		return 0
	}
	for _, value := range e.timestampValues() {
		if t := parseFlexibleTime(value); !t.IsZero() {
			return t.Sub(base)
		}
	}
	return 0
}

// meetingTitle picks the subject/conversation title with a date fallback so
// the row is never blank.
func meetingTitle(m *Meeting) string {
	if title := m.DisplayName(); title != "" {
		return title
	}
	if t := m.StartedAt(); !t.IsZero() {
		return "Meeting on " + t.UTC().Format("2006-01-02")
	}
	return "Meeting"
}

// formatTranscriptLine renders "[mm:ss] Speaker: text" (or "[h:mm:ss]" past
// the first hour) — the shared rendering contract with the granola importer.
func formatTranscriptLine(offset time.Duration, speaker, text string) string {
	if offset < 0 {
		offset = 0
	}
	if speaker == "" {
		speaker = "Unknown"
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
