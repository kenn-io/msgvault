package embed

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector"
)

var stableMeetingTurn = regexp.MustCompile(`^\[(?:[0-9]{2}:[0-9]{2}|[0-9]+:[0-9]{2}:[0-9]{2})\] [^:\n]+: .+$`)

type meetingLine struct {
	text       string
	start, end int
}

type meetingLayout struct {
	header          string
	headerEnd       int
	noteStart       int
	noteEnd         int
	transcriptStart int
	transcriptEnd   int
	hasHeader       bool
	hasTranscript   bool
	turns           []meetingLine
	stableTurns     bool
}

// AssembleMeetingDocument assembles one deterministic meeting document from
// the stable rendered-body contract shared by the meeting importers.
func AssembleMeetingDocument(row AssemblyMessage, policy AssemblyPolicy) (Document, error) {
	if row.ID == 0 {
		return Document{}, errorsNewMeeting("message id is zero")
	}
	body, bodyTruncated := Preprocess("", row.Body, 0, policy.Preprocess)
	layout := parseMeetingLayout(body)
	header := layout.header
	chunks := make([]OwnedChunk, 0)

	addRegion := func(start, end int) {
		chunks = append(chunks, genericMeetingRegion(row.ID, body, header, start, end,
			policy.MaxChunkRunes, len(chunks))...)
	}

	if !layout.hasHeader {
		addRegion(0, utf8.RuneCountInString(body))
	} else {
		if layout.noteEnd > layout.noteStart {
			addRegion(layout.noteStart, layout.noteEnd)
		}
		if layout.hasTranscript && layout.transcriptEnd > layout.transcriptStart {
			oversizedDocument := policy.MaxDocumentUTF8Bytes > 0 && len(body) > policy.MaxDocumentUTF8Bytes
			if layout.stableTurns && !oversizedDocument {
				chunks = append(chunks, packMeetingTurns(row.ID, body, header, layout.turns,
					policy.MaxChunkRunes, len(chunks))...)
			} else {
				addRegion(layout.transcriptStart, layout.transcriptEnd)
			}
		}
	}
	for i := range chunks {
		chunks[i].ChunkIndex = i
		chunks[i].Truncated = chunks[i].Truncated || bodyTruncated
	}
	chunks = limitOwnedChunksToRequest(chunks, policy.MaxDocumentUTF8Bytes, defaultVoyageRequestLimits.MaxChunks)
	key := "meeting:" + strconv.FormatInt(row.ID, 10)
	return Document{
		Key: key, Kind: "meeting-transcript", ScopeKey: key,
		Revision:       documentRevision("meeting", row, policy, chunks),
		SourceSequence: row.SourceSequence,
		Versions:       []SourceVersion{{MessageID: row.ID, LastModified: row.LastModified}},
		Chunks:         chunks,
	}, nil
}

func errorsNewMeeting(detail string) error {
	return fmt.Errorf("assemble meeting document: %s", detail)
}

func parseMeetingLayout(body string) meetingLayout {
	lines := meetingLines(body)
	if len(lines) < 2 || strings.TrimSpace(lines[0].text) == "" ||
		(!strings.HasPrefix(lines[1].text, "When: ") && !strings.HasPrefix(lines[1].text, "Attendees: ")) {
		return meetingLayout{}
	}
	headerLast := 1
	if strings.HasPrefix(lines[1].text, "When: ") && len(lines) > 2 && strings.HasPrefix(lines[2].text, "Attendees: ") {
		headerLast = 2
	}
	headerParts := make([]string, 0, headerLast+1)
	for i := 0; i <= headerLast; i++ {
		headerParts = append(headerParts, lines[i].text)
	}
	layout := meetingLayout{
		header: strings.Join(headerParts, "\n"), headerEnd: lines[headerLast].end,
		hasHeader: true,
	}
	contentLine := headerLast + 1
	for contentLine < len(lines) && strings.TrimSpace(lines[contentLine].text) == "" {
		contentLine++
	}
	transcriptLine := -1
	for i := contentLine; i < len(lines); i++ {
		if lines[i].text == "Transcript:" {
			transcriptLine = i
			break
		}
	}
	if transcriptLine < 0 {
		layout.noteStart, layout.noteEnd = lineRegion(lines, contentLine, len(lines))
		return layout
	}
	layout.hasTranscript = true
	layout.noteStart, layout.noteEnd = lineRegion(lines, contentLine, transcriptLine)
	turnStart := transcriptLine + 1
	for turnStart < len(lines) && strings.TrimSpace(lines[turnStart].text) == "" {
		turnStart++
	}
	layout.transcriptStart, layout.transcriptEnd = lineRegion(lines, turnStart, len(lines))
	if layout.transcriptEnd <= layout.transcriptStart {
		return layout
	}
	layout.stableTurns = true
	for i := turnStart; i < len(lines); i++ {
		if strings.TrimSpace(lines[i].text) == "" || !stableMeetingTurn.MatchString(lines[i].text) {
			layout.stableTurns = false
			layout.turns = nil
			break
		}
		layout.turns = append(layout.turns, lines[i])
	}
	return layout
}

func meetingLines(body string) []meetingLine {
	runes := []rune(body)
	lines := make([]meetingLine, 0, strings.Count(body, "\n")+1)
	start := 0
	for i, r := range runes {
		if r != '\n' {
			continue
		}
		end := i
		if end > start && runes[end-1] == '\r' {
			end--
		}
		lines = append(lines, meetingLine{text: string(runes[start:end]), start: start, end: end})
		start = i + 1
	}
	end := len(runes)
	if end > start && runes[end-1] == '\r' {
		end--
	}
	lines = append(lines, meetingLine{text: string(runes[start:end]), start: start, end: end})
	return lines
}

func lineRegion(lines []meetingLine, first, limit int) (int, int) {
	for first < limit && strings.TrimSpace(lines[first].text) == "" {
		first++
	}
	for limit > first && strings.TrimSpace(lines[limit-1].text) == "" {
		limit--
	}
	if first >= limit {
		return 0, 0
	}
	return lines[first].start, lines[limit-1].end
}

func genericMeetingRegion(messageID int64, body, header string, start, end, maxRunes, firstIndex int) []OwnedChunk {
	if end <= start {
		return nil
	}
	runes := []rune(body)
	region := string(runes[start:end])
	spans, tailDropped := ChunkText(region, maxRunes, 0, maxSpansPerMessage)
	chunks := make([]OwnedChunk, 0, len(spans))
	for i, span := range spans {
		chunks = append(chunks, OwnedChunk{
			MessageID: messageID, ChunkIndex: firstIndex + i,
			Text:            contextualMeetingText(header, span.Text),
			SourceCharStart: start + span.CharStart, SourceCharEnd: start + span.CharEnd,
			SourceBasis: vector.SourceBasisBody, Truncated: tailDropped,
		})
	}
	return chunks
}

func packMeetingTurns(messageID int64, body, header string, turns []meetingLine, maxRunes, firstIndex int) []OwnedChunk {
	if len(turns) == 0 {
		return nil
	}
	chunks := make([]OwnedChunk, 0, len(turns))
	runes := []rune(body)
	flush := func(first, last int) {
		start, end := turns[first].start, turns[last].end
		chunks = append(chunks, OwnedChunk{
			MessageID: messageID, ChunkIndex: firstIndex + len(chunks),
			Text:            contextualMeetingText(header, string(runes[start:end])),
			SourceCharStart: start, SourceCharEnd: end, SourceBasis: vector.SourceBasisBody,
		})
	}
	packStart := -1
	for i, turn := range turns {
		turnRunes := turn.end - turn.start
		if maxRunes > 0 && turnRunes > maxRunes {
			if packStart >= 0 {
				flush(packStart, i-1)
				packStart = -1
			}
			chunks = append(chunks, genericMeetingRegion(messageID, body, header,
				turn.start, turn.end, maxRunes, firstIndex+len(chunks))...)
			continue
		}
		if packStart < 0 {
			packStart = i
			continue
		}
		candidate := turns[i].end - turns[packStart].start
		if maxRunes > 0 && candidate > maxRunes {
			flush(packStart, i-1)
			packStart = i
		}
	}
	if packStart >= 0 {
		flush(packStart, len(turns)-1)
	}
	return chunks
}

func contextualMeetingText(header, source string) string {
	if header == "" {
		return source
	}
	return header + "\n\n" + source
}
