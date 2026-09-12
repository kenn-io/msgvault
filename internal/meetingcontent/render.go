package meetingcontent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const packetSchemaVersion = 1

func Render(archiveUID string, entries []Entry, options PacketOptions) (*PacketResult, error) {
	if options.Format != FormatJSON && options.Format != FormatMarkdown {
		return nil, fmt.Errorf("unsupported meeting context format %q", options.Format)
	}

	originals := make([]Entry, len(entries))
	for index := range entries {
		originals[index] = normalizeEntry(entries[index])
	}
	sort.SliceStable(originals, func(left, right int) bool {
		return entryLess(originals[left], originals[right])
	})
	originalByID := make(map[int64]Entry, len(originals))
	requested := make([]int64, len(originals))
	for index, entry := range originals {
		requested[index] = entry.Meeting.MessageID
		originalByID[entry.Meeting.MessageID] = entry
	}

	packet := Packet{
		SchemaVersion:       packetSchemaVersion,
		ArchiveUID:          archiveUID,
		RequestedMessageIDs: append([]int64{}, requested...),
		Meetings:            []Entry{},
		OmittedMessageIDs:   append([]int64{}, requested...),
		Truncations:         []Truncation{},
	}
	prepared, rendered, err := preparePacket(packet, originalByID, options)
	if err != nil {
		return nil, err
	}
	if len(rendered) > options.MaxBytes {
		return nil, ErrBudgetTooSmall
	}
	packet = prepared

	for _, original := range originals {
		candidate := clonePacket(packet)
		candidate.Meetings = append(candidate.Meetings, provenanceSkeleton(original, options.IncludeTranscript))
		candidate.OmittedMessageIDs = removeMessageID(candidate.OmittedMessageIDs, original.Meeting.MessageID)
		if admitted, ok, candidateErr := candidateWithinBudget(candidate, originalByID, options); candidateErr != nil {
			return nil, candidateErr
		} else if ok {
			packet = admitted
		}
	}

	packet, err = admitSections(packet, originalByID, options, "summary")
	if err != nil {
		return nil, err
	}
	packet, err = admitSections(packet, originalByID, options, "notes")
	if err != nil {
		return nil, err
	}
	packet, err = admitActions(packet, originalByID, options)
	if err != nil {
		return nil, err
	}
	if options.IncludeTranscript {
		packet, err = admitTranscripts(packet, originalByID, options)
		if err != nil {
			return nil, err
		}
	}

	packet, rendered, err = preparePacket(packet, originalByID, options)
	if err != nil {
		return nil, err
	}
	if len(rendered) > options.MaxBytes {
		return nil, ErrBudgetTooSmall
	}
	return &PacketResult{
		SchemaVersion:     packetSchemaVersion,
		Format:            options.Format,
		Content:           rendered,
		ContentBytes:      len([]byte(rendered)),
		Truncated:         packet.Truncated,
		OmittedMessageIDs: append([]int64{}, packet.OmittedMessageIDs...),
	}, nil
}

func normalizeEntry(entry Entry) Entry {
	entry = cloneEntry(entry)
	participants := append([]Participant{}, entry.Participants...)
	byEmailRole := make(map[string]int, len(participants))
	for index, participant := range participants {
		if key := participantEmailRoleKey(participant); key != "" {
			byEmailRole[key] = index
		}
	}
	for _, source := range entry.Content.SourceParticipants {
		if key := participantEmailRoleKey(source); key != "" {
			if index, exists := byEmailRole[key]; exists {
				if strings.TrimSpace(participants[index].Name) == "" {
					participants[index].Name = strings.TrimSpace(source.Name)
				}
				continue
			}
			byEmailRole[key] = len(participants)
		}
		participants = append(participants, source)
	}
	for index := range participants {
		participants[index].Name = strings.TrimSpace(participants[index].Name)
		participants[index].Email = strings.TrimSpace(participants[index].Email)
		participants[index].Role = strings.TrimSpace(participants[index].Role)
	}
	sort.SliceStable(participants, func(left, right int) bool {
		return participantLess(participants[left], participants[right])
	})
	entry.Participants = participants
	entry.Content.SourceParticipants = nil
	if entry.Content.Actions == nil {
		entry.Content.Actions = []Action{}
	}
	return entry
}

func entryLess(left, right Entry) bool {
	leftTime, rightTime := left.Meeting.OccurredAt, right.Meeting.OccurredAt
	switch {
	case leftTime == nil && rightTime != nil:
		return false
	case leftTime != nil && rightTime == nil:
		return true
	case leftTime != nil && rightTime != nil && !leftTime.Equal(*rightTime):
		return leftTime.Before(*rightTime)
	default:
		return left.Meeting.MessageID < right.Meeting.MessageID
	}
}

func participantEmailRoleKey(participant Participant) string {
	email := strings.ToLower(strings.TrimSpace(participant.Email))
	if email == "" {
		return ""
	}
	return strings.TrimSpace(participant.Role) + "\x00" + email
}

func participantLess(left, right Participant) bool {
	if left.Role != right.Role {
		return left.Role < right.Role
	}
	leftEmail := strings.ToLower(strings.TrimSpace(left.Email))
	rightEmail := strings.ToLower(strings.TrimSpace(right.Email))
	if leftEmail != rightEmail {
		return leftEmail < rightEmail
	}
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	leftID, rightID := int64(0), int64(0)
	if left.ParticipantID != nil {
		leftID = *left.ParticipantID
	}
	if right.ParticipantID != nil {
		rightID = *right.ParticipantID
	}
	return leftID < rightID
}

func provenanceSkeleton(original Entry, includeTranscript bool) Entry {
	skeleton := cloneEntry(original)
	skeleton.Content.Summary.Text = ""
	skeleton.Content.Notes.Text = ""
	skeleton.Content.Actions = []Action{}
	skeleton.Content.SourceParticipants = nil
	if includeTranscript {
		skeleton.Content.Transcript.Text = ""
		skeleton.Content.Transcript.Segments = nil
	} else {
		skeleton.Content.Transcript = Transcript{State: StateOmittedByRequest}
	}
	return skeleton
}

func admitSections(packet Packet, originals map[int64]Entry, options PacketOptions, section string) (Packet, error) {
	for index := range packet.Meetings {
		original := originals[packet.Meetings[index].Meeting.MessageID]
		var source Section
		switch section {
		case "summary":
			source = original.Content.Summary
		case "notes":
			source = original.Content.Notes
		}
		if source.State != StateAvailable || source.Text == "" {
			continue
		}

		full := clonePacket(packet)
		setPacketSection(&full, index, section, source)
		if admitted, ok, err := candidateWithinBudget(full, originals, options); err != nil {
			return Packet{}, err
		} else if ok {
			packet = admitted
			continue
		}

		prefix, err := largestTextPrefix(packet, source.Text, originals, options, func(candidate *Packet, value string) {
			partial := source
			partial.Text = value
			setPacketSection(candidate, index, section, partial)
		})
		if err != nil {
			return Packet{}, err
		}
		packet = prefix
	}
	return packet, nil
}

func setPacketSection(packet *Packet, index int, section string, value Section) {
	if section == "summary" {
		packet.Meetings[index].Content.Summary = value
	} else {
		packet.Meetings[index].Content.Notes = value
	}
}

func admitActions(packet Packet, originals map[int64]Entry, options PacketOptions) (Packet, error) {
	for index := range packet.Meetings {
		original := originals[packet.Meetings[index].Meeting.MessageID]
		for _, action := range original.Content.Actions {
			candidate := clonePacket(packet)
			candidate.Meetings[index].Content.Actions = append(candidate.Meetings[index].Content.Actions, action)
			if admitted, ok, err := candidateWithinBudget(candidate, originals, options); err != nil {
				return Packet{}, err
			} else if ok {
				packet = admitted
				continue
			}
			break
		}
	}
	return packet, nil
}

func admitTranscripts(packet Packet, originals map[int64]Entry, options PacketOptions) (Packet, error) {
	for index := range packet.Meetings {
		original := originals[packet.Meetings[index].Meeting.MessageID]
		if original.Content.Transcript.State != StateAvailable {
			continue
		}
		full := clonePacket(packet)
		full.Meetings[index].Content.Transcript = cloneTranscript(original.Content.Transcript)
		if admitted, ok, err := candidateWithinBudget(full, originals, options); err != nil {
			return Packet{}, err
		} else if ok {
			packet = admitted
			continue
		}

		if original.Content.Transcript.Text != "" {
			prefix, err := largestTextPrefix(packet, original.Content.Transcript.Text, originals, options, func(candidate *Packet, value string) {
				candidate.Meetings[index].Content.Transcript.Text = value
			})
			if err != nil {
				return Packet{}, err
			}
			packet = prefix
		}
		for _, segment := range original.Content.Transcript.Segments {
			candidate := clonePacket(packet)
			candidate.Meetings[index].Content.Transcript.Segments = append(
				candidate.Meetings[index].Content.Transcript.Segments, segment,
			)
			if admitted, ok, err := candidateWithinBudget(candidate, originals, options); err != nil {
				return Packet{}, err
			} else if ok {
				packet = admitted
				continue
			}

			prefix, err := largestTextPrefix(packet, segment.Text, originals, options, func(candidate *Packet, value string) {
				partial := segment
				partial.Text = value
				candidate.Meetings[index].Content.Transcript.Segments = append(
					candidate.Meetings[index].Content.Transcript.Segments, partial,
				)
			})
			if err != nil {
				return Packet{}, err
			}
			packet = prefix
			break
		}
	}
	return packet, nil
}

func largestTextPrefix(
	packet Packet,
	text string,
	originals map[int64]Entry,
	options PacketOptions,
	apply func(*Packet, string),
) (Packet, error) {
	runes := []rune(text)
	low, high := 1, len(runes)-1
	best := packet
	for low <= high {
		middle := low + (high-low)/2
		candidate := clonePacket(packet)
		apply(&candidate, string(runes[:middle]))
		admitted, ok, err := candidateWithinBudget(candidate, originals, options)
		if err != nil {
			return Packet{}, err
		}
		if ok {
			best = admitted
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return best, nil
}

func candidateWithinBudget(packet Packet, originals map[int64]Entry, options PacketOptions) (Packet, bool, error) {
	prepared, rendered, err := preparePacket(packet, originals, options)
	if err != nil {
		return Packet{}, false, err
	}
	return prepared, len(rendered) <= options.MaxBytes, nil
}

func preparePacket(packet Packet, originals map[int64]Entry, options PacketOptions) (Packet, string, error) {
	packet = clonePacket(packet)
	packet.Truncations = []Truncation{}
	packet.Truncated = len(packet.OmittedMessageIDs) > 0
	for index := range packet.Meetings {
		entry := &packet.Meetings[index]
		original, ok := originals[entry.Meeting.MessageID]
		if !ok {
			continue
		}
		recomputeSection(entry, original.Content.Summary, &entry.Content.Summary, "summary", &packet)
		recomputeSection(entry, original.Content.Notes, &entry.Content.Notes, "notes", &packet)

		if len(entry.Content.Actions) < len(original.Content.Actions) {
			packet.Truncated = true
			packet.Truncations = append(packet.Truncations, Truncation{
				MessageID: entry.Meeting.MessageID, Section: "actions",
				OriginalBytes: actionBytes(original.Content.Actions),
				IncludedBytes: actionBytes(entry.Content.Actions),
				OmittedItems:  len(original.Content.Actions) - len(entry.Content.Actions),
			})
		}

		if !options.IncludeTranscript {
			entry.Content.Transcript = Transcript{State: StateOmittedByRequest}
		} else {
			recomputeTranscript(entry, original.Content.Transcript, &packet)
		}
		entry.Content.SourceParticipants = nil
		if entry.Participants == nil {
			entry.Participants = []Participant{}
		}
		if entry.Content.Actions == nil {
			entry.Content.Actions = []Action{}
		}
	}

	rendered, err := serializePacket(packet, options.Format)
	return packet, rendered, err
}

func recomputeSection(entry *Entry, original Section, included *Section, name string, packet *Packet) {
	if original.State != StateAvailable {
		*included = original
		return
	}
	originalBytes := len([]byte(original.Text))
	includedBytes := len([]byte(included.Text))
	if includedBytes >= originalBytes {
		included.State = original.State
		included.Reason = original.Reason
		return
	}
	included.State = StateTruncated
	included.Reason = ""
	packet.Truncated = true
	packet.Truncations = append(packet.Truncations, Truncation{
		MessageID: entry.Meeting.MessageID, Section: name,
		OriginalBytes: int64(originalBytes), IncludedBytes: int64(includedBytes),
	})
}

func recomputeTranscript(entry *Entry, original Transcript, packet *Packet) {
	if original.State != StateAvailable {
		entry.Content.Transcript = cloneTranscript(original)
		return
	}
	originalBytes := transcriptBytes(original)
	includedBytes := transcriptBytes(entry.Content.Transcript)
	if includedBytes >= originalBytes && len(entry.Content.Transcript.Segments) >= len(original.Segments) {
		entry.Content.Transcript.State = original.State
		entry.Content.Transcript.Reason = original.Reason
		return
	}
	entry.Content.Transcript.State = StateTruncated
	entry.Content.Transcript.Reason = ""
	packet.Truncated = true
	packet.Truncations = append(packet.Truncations, Truncation{
		MessageID: entry.Meeting.MessageID, Section: "transcript",
		OriginalBytes: int64(originalBytes), IncludedBytes: int64(includedBytes),
		OmittedItems: max(0, len(original.Segments)-len(entry.Content.Transcript.Segments)),
	})
}

func actionBytes(actions []Action) int64 {
	var total int64
	for _, action := range actions {
		encoded, err := json.Marshal(action)
		if err == nil {
			total += int64(len(encoded))
		}
	}
	return total
}

func transcriptBytes(transcript Transcript) int {
	total := len([]byte(transcript.Text))
	for _, segment := range transcript.Segments {
		total += len([]byte(segment.Text))
	}
	return total
}

func serializePacket(packet Packet, format Format) (string, error) {
	switch format {
	case FormatJSON:
		encoded, err := json.Marshal(packet)
		return string(encoded), err
	case FormatMarkdown:
		return renderMarkdown(packet), nil
	default:
		return "", fmt.Errorf("unsupported meeting context format %q", format)
	}
}

func renderMarkdown(packet Packet) string {
	var builder strings.Builder
	builder.WriteString("# Meeting context\n\n")
	writeMarkdownField(&builder, "Schema version", strconv.Itoa(packet.SchemaVersion))
	writeMarkdownField(&builder, "Archive UID", markdownCode(packet.ArchiveUID))
	writeMarkdownField(&builder, "Requested message IDs", formatIDs(packet.RequestedMessageIDs))
	writeMarkdownField(&builder, "Omitted message IDs", formatIDs(packet.OmittedMessageIDs))
	writeMarkdownField(&builder, "Truncated", strconv.FormatBool(packet.Truncated))

	for _, entry := range packet.Meetings {
		builder.WriteString("\n## ")
		builder.WriteString(markdownSingleLine(entry.Meeting.Title))
		builder.WriteString("\n\n")
		builder.WriteString("[Archived meeting](")
		builder.WriteString(entry.Meeting.ArchivePath)
		builder.WriteString(")\n\n")
		writeMarkdownField(&builder, "Message ID", strconv.FormatInt(entry.Meeting.MessageID, 10))
		writeMarkdownField(&builder, "Conversation ID", strconv.FormatInt(entry.Meeting.ConversationID, 10))
		writeMarkdownField(&builder, "Source ID", strconv.FormatInt(entry.Meeting.SourceID, 10))
		writeMarkdownField(&builder, "Source type", markdownCode(entry.Meeting.SourceType))
		writeMarkdownField(&builder, "Source identifier", markdownCode(entry.Meeting.SourceIdentifier))
		writeMarkdownField(&builder, "Source message ID", markdownCode(entry.Meeting.SourceMessageID))
		if entry.Meeting.OccurredAt == nil {
			writeMarkdownField(&builder, "Occurred at", "unknown")
		} else {
			writeMarkdownField(&builder, "Occurred at", entry.Meeting.OccurredAt.Format(time.RFC3339Nano))
		}
		writeMarkdownParticipants(&builder, entry.Participants)
		if entry.Content.DurationSeconds == nil {
			writeMarkdownField(&builder, "Duration", "unknown")
		} else {
			value := strconv.FormatFloat(*entry.Content.DurationSeconds, 'f', -1, 64) + " seconds"
			if entry.Content.DurationBasis != "" {
				value += " (" + string(entry.Content.DurationBasis) + ")"
			}
			writeMarkdownField(&builder, "Duration", value)
		}
		writeMarkdownField(&builder, "Action coverage", string(entry.Content.ActionCoverage))
		if entry.Content.ActionReason != "" {
			writeMarkdownField(&builder, "Action reason", entry.Content.ActionReason)
		}
		writeMarkdownSection(&builder, "Summary", entry.Content.Summary)
		writeMarkdownSection(&builder, "Notes", entry.Content.Notes)
		writeMarkdownActions(&builder, entry.Content.Actions)
		writeMarkdownTranscript(&builder, entry.Content.Transcript)
	}

	builder.WriteString("\n## Truncations\n\n")
	if len(packet.Truncations) == 0 {
		builder.WriteString("None\n")
	} else {
		for _, truncation := range packet.Truncations {
			fmt.Fprintf(&builder, "- Message %d, %s: %d of %d bytes", truncation.MessageID,
				truncation.Section, truncation.IncludedBytes, truncation.OriginalBytes)
			if truncation.OmittedItems > 0 {
				fmt.Fprintf(&builder, "; %d omitted items", truncation.OmittedItems)
			}
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func writeMarkdownField(builder *strings.Builder, label, value string) {
	builder.WriteString(label)
	builder.WriteString(": ")
	builder.WriteString(value)
	builder.WriteByte('\n')
}

func writeMarkdownParticipants(builder *strings.Builder, participants []Participant) {
	builder.WriteString("\n### Participants\n\n")
	if len(participants) == 0 {
		builder.WriteString("None\n")
		return
	}
	for _, participant := range participants {
		builder.WriteString("- ")
		builder.WriteString(participant.Role)
		if participant.Name != "" {
			builder.WriteString(": ")
			builder.WriteString(markdownSingleLine(participant.Name))
		}
		if participant.Email != "" {
			builder.WriteString(" <")
			builder.WriteString(participant.Email)
			builder.WriteByte('>')
		}
		if participant.ParticipantID != nil {
			fmt.Fprintf(builder, " (participant %d)", *participant.ParticipantID)
		}
		builder.WriteByte('\n')
	}
}

func writeMarkdownSection(builder *strings.Builder, heading string, section Section) {
	builder.WriteString("\n### ")
	builder.WriteString(heading)
	builder.WriteString("\n\n")
	writeMarkdownField(builder, "State", string(section.State))
	if section.Reason != "" {
		writeMarkdownField(builder, "Reason", section.Reason)
	}
	if section.Text != "" {
		builder.WriteByte('\n')
		builder.WriteString(section.Text)
		builder.WriteByte('\n')
	}
}

func writeMarkdownActions(builder *strings.Builder, actions []Action) {
	builder.WriteString("\n### Actions\n\n")
	if len(actions) == 0 {
		builder.WriteString("None\n")
		return
	}
	for _, action := range actions {
		fmt.Fprintf(builder, "- [%s] %s\n", action.Status, markdownSingleLine(action.Title))
		fmt.Fprintf(builder, "  - Ordinal: %d\n", action.Ordinal)
		writeOptionalMarkdownActionField(builder, "Source ID", action.SourceID)
		writeOptionalMarkdownActionField(builder, "Description", action.Description)
		writeOptionalMarkdownActionField(builder, "Assignee", action.AssigneeName)
		writeOptionalMarkdownActionField(builder, "Assignee email", action.AssigneeEmail)
		writeOptionalMarkdownActionField(builder, "Source status", action.SourceStatus)
		writeOptionalMarkdownActionField(builder, "Due date", action.DueDate)
		fmt.Fprintf(builder, "  - Origin: %s\n", action.Origin)
		fmt.Fprintf(builder, "  - Locator: %s\n", action.Locator)
	}
}

func writeOptionalMarkdownActionField(builder *strings.Builder, label, value string) {
	if value == "" {
		return
	}
	builder.WriteString("  - ")
	builder.WriteString(label)
	builder.WriteString(": ")
	builder.WriteString(markdownSingleLine(value))
	builder.WriteByte('\n')
}

func writeMarkdownTranscript(builder *strings.Builder, transcript Transcript) {
	builder.WriteString("\n### Transcript\n\n")
	writeMarkdownField(builder, "State", string(transcript.State))
	if transcript.Reason != "" {
		writeMarkdownField(builder, "Reason", transcript.Reason)
	}
	if transcript.Text != "" {
		builder.WriteByte('\n')
		builder.WriteString(transcript.Text)
		builder.WriteByte('\n')
	}
	for _, segment := range transcript.Segments {
		builder.WriteString("\n- ")
		var timing []string
		if segment.StartedAt != nil {
			timing = append(timing, "start "+segment.StartedAt.Format(time.RFC3339Nano))
		}
		if segment.EndedAt != nil {
			timing = append(timing, "end "+segment.EndedAt.Format(time.RFC3339Nano))
		}
		if segment.OffsetSeconds != nil {
			timing = append(timing, "offset "+strconv.FormatFloat(*segment.OffsetSeconds, 'f', -1, 64)+"s")
		}
		if len(timing) > 0 {
			builder.WriteString("[")
			builder.WriteString(strings.Join(timing, ", "))
			builder.WriteString("] ")
		}
		if segment.Speaker != "" {
			builder.WriteString(markdownSingleLine(segment.Speaker))
			builder.WriteString(": ")
		}
		builder.WriteString(segment.Text)
		builder.WriteByte('\n')
	}
}

func markdownSingleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func markdownCode(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "\\`") + "`"
}

func formatIDs(ids []int64) string {
	if len(ids) == 0 {
		return "none"
	}
	parts := make([]string, len(ids))
	for index, id := range ids {
		parts[index] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

func removeMessageID(ids []int64, target int64) []int64 {
	for index, id := range ids {
		if id == target {
			return append(append([]int64{}, ids[:index]...), ids[index+1:]...)
		}
	}
	return append([]int64{}, ids...)
}

func clonePacket(packet Packet) Packet {
	clone := packet
	clone.RequestedMessageIDs = append([]int64{}, packet.RequestedMessageIDs...)
	clone.OmittedMessageIDs = append([]int64{}, packet.OmittedMessageIDs...)
	clone.Truncations = append([]Truncation{}, packet.Truncations...)
	clone.Meetings = make([]Entry, len(packet.Meetings))
	for index := range packet.Meetings {
		clone.Meetings[index] = cloneEntry(packet.Meetings[index])
	}
	return clone
}

func cloneEntry(entry Entry) Entry {
	clone := entry
	clone.Participants = append([]Participant{}, entry.Participants...)
	clone.Content.Actions = append([]Action{}, entry.Content.Actions...)
	clone.Content.SourceParticipants = append([]Participant(nil), entry.Content.SourceParticipants...)
	clone.Content.Transcript = cloneTranscript(entry.Content.Transcript)
	return clone
}

func cloneTranscript(transcript Transcript) Transcript {
	clone := transcript
	clone.Segments = append([]Segment(nil), transcript.Segments...)
	return clone
}
