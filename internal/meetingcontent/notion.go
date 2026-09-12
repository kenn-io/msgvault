package meetingcontent

import (
	"bytes"
	"encoding/json"
	"strings"
)

func decodeNotion(fields map[string]json.RawMessage) Content {
	version, ok := rawInteger(fields["schema_version"])
	if !ok || version != 1 {
		return unavailableContent("unsupported_schema")
	}
	content := baseRecognizedContent()
	var canonical map[string]json.RawMessage
	if raw, exists := fields["canonical"]; !exists {
		content.Summary = Section{State: StateUnavailable, Reason: reasonMissingField}
		content.Notes = Section{State: StateUnavailable, Reason: reasonMissingField}
		content.Transcript = Transcript{State: StateUnavailable, Reason: reasonMissingField}
	} else if isNull(raw) || json.Unmarshal(raw, &canonical) != nil {
		content.Summary = Section{State: StateUnavailable, Reason: reasonInvalidSection}
		content.Notes = Section{State: StateUnavailable, Reason: reasonInvalidSection}
		content.Transcript = Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	} else {
		content.Summary = decodeStringField(canonical, "summary")
		content.Notes = decodeStringField(canonical, "notes")
		section := decodeStringField(canonical, "transcript")
		content.Transcript = Transcript{State: section.State, Reason: section.Reason, Text: section.Text}
	}
	discovery, discoveryOK := notionDiscovery(fields["discovery"])
	content.Actions, content.ActionCoverage, content.ActionReason = decodeNotionActions(fields, discovery, discoveryOK)
	content.SourceParticipants = notionParticipants(fields, discovery)
	if discoveryOK {
		if start, startOK := parseTimeString(discovery.Recording.Start); startOK {
			if end, endOK := parseTimeString(discovery.Recording.End); endOK && end.After(start) {
				setDuration(&content, end.Sub(start).Seconds(), DurationProvider)
				return content
			}
		}
		if start, startOK := parseTimeString(discovery.Calendar.Start); startOK {
			if end, endOK := parseTimeString(discovery.Calendar.End); endOK && end.After(start) {
				setDuration(&content, end.Sub(start).Seconds(), DurationScheduled)
			}
		}
	}
	return content
}

type notionDiscoveryWire struct {
	SummaryID    string
	NotesID      string
	TranscriptID string
	AttendeeIDs  []string
	Recording    struct{ Start, End string }
	Calendar     struct{ Start, End string }
}

func notionDiscovery(raw json.RawMessage) (notionDiscoveryWire, bool) {
	var wire struct {
		MeetingNotes struct {
			Children struct {
				SummaryID    string `json:"summary_block_id"`
				NotesID      string `json:"notes_block_id"`
				TranscriptID string `json:"transcript_block_id"`
			} `json:"children"`
			Recording struct {
				Start string `json:"start_time"`
				End   string `json:"end_time"`
			} `json:"recording"`
			Calendar struct {
				Start     string   `json:"start_time"`
				End       string   `json:"end_time"`
				Attendees []string `json:"attendees"`
			} `json:"calendar_event"`
		} `json:"meeting_notes"`
	}
	if len(raw) == 0 || isNull(raw) || json.Unmarshal(raw, &wire) != nil {
		return notionDiscoveryWire{}, false
	}
	var out notionDiscoveryWire
	out.SummaryID = strings.TrimSpace(wire.MeetingNotes.Children.SummaryID)
	out.NotesID = strings.TrimSpace(wire.MeetingNotes.Children.NotesID)
	out.TranscriptID = strings.TrimSpace(wire.MeetingNotes.Children.TranscriptID)
	out.AttendeeIDs = wire.MeetingNotes.Calendar.Attendees
	out.Recording.Start, out.Recording.End = wire.MeetingNotes.Recording.Start, wire.MeetingNotes.Recording.End
	out.Calendar.Start, out.Calendar.End = wire.MeetingNotes.Calendar.Start, wire.MeetingNotes.Calendar.End
	return out, true
}

func decodeNotionActions(fields map[string]json.RawMessage, discovery notionDiscoveryWire, discoveryOK bool) ([]Action, Coverage, string) {
	if !discoveryOK {
		return []Action{}, CoverageUnavailable, "incomplete_tree"
	}
	trees := []struct {
		key string
		id  string
	}{{"summary", discovery.SummaryID}, {"notes", discovery.NotesID}}
	referenced := false
	completeTrees := 0
	incomplete := false
	invalid := false
	actions := []Action{}
	seen := map[string]struct{}{}
	ordinal := 0
	for _, tree := range trees {
		if tree.id == "" {
			continue
		}
		referenced = true
		blocks, complete := notionTreeBlocks(fields[tree.key], tree.id)
		if !complete {
			incomplete = true
		} else {
			completeTrees++
		}
		for _, block := range blocks {
			var header struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if json.Unmarshal(block, &header) != nil {
				invalid = true
				continue
			}
			if header.Type != "to_do" {
				continue
			}
			header.ID = strings.TrimSpace(header.ID)
			if header.ID != "" {
				if _, duplicate := seen[header.ID]; duplicate {
					continue
				}
				seen[header.ID] = struct{}{}
			}
			currentOrdinal := ordinal
			ordinal++
			action, ok := notionAction(block, currentOrdinal)
			if !ok {
				invalid = true
				continue
			}
			actions = append(actions, action)
		}
	}
	if !referenced {
		return []Action{}, CoverageUnsupported, "no_structured_actions"
	}
	if incomplete {
		if completeTrees > 0 || len(actions) > 0 {
			return actions, CoveragePartial, "incomplete_tree"
		}
		return actions, CoverageUnavailable, "incomplete_tree"
	}
	return actionResult(actions, invalid)
}

const (
	maxNotionTraversalDepth  = 32
	maxNotionTraversalPages  = 256
	maxNotionTraversalBlocks = 10000
)

func notionTreeBlocks(raw json.RawMessage, expectedRootID string) ([]json.RawMessage, bool) {
	if len(raw) == 0 || isNull(raw) {
		return nil, false
	}
	var tree struct {
		Root  json.RawMessage   `json:"root"`
		Pages []json.RawMessage `json:"pages"`
	}
	if json.Unmarshal(raw, &tree) != nil || len(tree.Root) == 0 || isNull(tree.Root) {
		return nil, false
	}
	rootID, rootHasChildren, rootValid := notionBlockTraversalHeader(tree.Root)
	if rootID != expectedRootID {
		return nil, false
	}
	traversal := notionTreeTraversal{
		pages:       tree.Pages,
		blocks:      []json.RawMessage{tree.Root},
		blockVisits: 1,
	}
	if !rootValid {
		return traversal.blocks, false
	}
	if !rootHasChildren {
		return traversal.blocks, len(tree.Pages) == 0
	}
	complete := traversal.readChildren(1)
	return traversal.blocks, complete && traversal.nextPage == len(traversal.pages)
}

type notionTreeTraversal struct {
	pages       []json.RawMessage
	nextPage    int
	pageVisits  int
	blockVisits int
	blocks      []json.RawMessage
}

func (t *notionTreeTraversal) readChildren(depth int) bool {
	if depth > maxNotionTraversalDepth {
		return false
	}
	seenCursors := map[string]struct{}{}
	for {
		if t.nextPage >= len(t.pages) || t.pageVisits >= maxNotionTraversalPages {
			return false
		}
		rawPage := t.pages[t.nextPage]
		t.nextPage++
		t.pageVisits++
		results, hasMore, nextCursor, pageValid := notionTraversalPage(rawPage)
		if results == nil {
			return false
		}

		childrenComplete := true
		for _, child := range results {
			if t.blockVisits >= maxNotionTraversalBlocks {
				return false
			}
			t.blockVisits++
			t.blocks = append(t.blocks, child)
			childID, hasChildren, childValid := notionBlockTraversalHeader(child)
			if !childValid {
				childrenComplete = false
				continue
			}
			if !hasChildren {
				continue
			}
			if childID == "" || !childrenComplete || !t.readChildren(depth+1) {
				childrenComplete = false
			}
		}
		if !pageValid || !childrenComplete {
			return false
		}
		if !hasMore {
			return true
		}
		if _, duplicate := seenCursors[nextCursor]; duplicate {
			return false
		}
		seenCursors[nextCursor] = struct{}{}
	}
}

func notionTraversalPage(raw json.RawMessage) ([]json.RawMessage, bool, string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false, "", false
	}
	resultsRaw, ok := fields["results"]
	if !ok || isNull(resultsRaw) {
		return nil, false, "", false
	}
	var results []json.RawMessage
	if json.Unmarshal(resultsRaw, &results) != nil {
		return nil, false, "", false
	}
	hasMoreRaw, ok := fields["has_more"]
	if !ok {
		return results, false, "", false
	}
	var hasMore bool
	if json.Unmarshal(hasMoreRaw, &hasMore) != nil || isNull(hasMoreRaw) {
		return results, false, "", false
	}
	nextCursor := ""
	if cursorRaw, exists := fields["next_cursor"]; exists && !isNull(cursorRaw) {
		if json.Unmarshal(cursorRaw, &nextCursor) != nil {
			return results, hasMore, "", false
		}
		nextCursor = strings.TrimSpace(nextCursor)
	}
	return results, hasMore, nextCursor, hasMore == (nextCursor != "")
}

func notionBlockTraversalHeader(raw json.RawMessage) (string, bool, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", false, false
	}
	id, idOK := optionalString(fields["id"])
	if !idOK {
		return "", false, false
	}
	id = strings.TrimSpace(id)
	hasChildrenRaw, exists := fields["has_children"]
	if !exists {
		return id, false, true
	}
	var hasChildren bool
	if isNull(hasChildrenRaw) || json.Unmarshal(hasChildrenRaw, &hasChildren) != nil {
		return id, false, false
	}
	return id, hasChildren, !hasChildren || id != ""
}

type notionResolvedUser struct {
	Name  string
	Email string
}

func notionResolvedUsers(fields map[string]json.RawMessage) map[string]notionResolvedUser {
	var users []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	_ = json.Unmarshal(fields["resolved_users"], &users)
	out := make(map[string]notionResolvedUser, len(users))
	for _, user := range users {
		if id := strings.TrimSpace(user.ID); id != "" {
			out[id] = notionResolvedUser{Name: strings.TrimSpace(user.Name), Email: normalizeExplicitEmail(user.Email)}
		}
	}
	return out
}

func notionAction(raw json.RawMessage, ordinal int) (Action, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return Action{}, false
	}
	id, idOK := optionalString(fields["id"])
	id = strings.TrimSpace(id)
	if !idOK || id == "" {
		return Action{}, false
	}
	var todo map[string]json.RawMessage
	if json.Unmarshal(fields["to_do"], &todo) != nil || todo == nil {
		return Action{}, false
	}
	richRaw, ok := todo["rich_text"]
	if !ok || isNull(richRaw) {
		return Action{}, false
	}
	var parts []struct {
		PlainText string `json:"plain_text"`
	}
	if json.Unmarshal(richRaw, &parts) != nil {
		return Action{}, false
	}
	var title strings.Builder
	for _, part := range parts {
		title.WriteString(part.PlainText)
	}
	if strings.TrimSpace(title.String()) == "" {
		return Action{}, false
	}
	status := StatusUnknown
	sourceStatus := ""
	if checkedRaw, exists := todo["checked"]; exists {
		switch string(bytes.TrimSpace(checkedRaw)) {
		case "true":
			status, sourceStatus = StatusCompleted, "true"
		case "false":
			status, sourceStatus = StatusPending, "false"
		default:
			return Action{}, false
		}
	}
	return Action{
		Ordinal: ordinal, SourceID: id, Title: strings.TrimSpace(title.String()),
		Status: status, SourceStatus: sourceStatus, Origin: "checkbox", Locator: "block:" + id,
	}, true
}

func notionParticipants(fields map[string]json.RawMessage, discovery notionDiscoveryWire) []Participant {
	var labels []string
	_ = json.Unmarshal(fields["attendee_labels"], &labels)
	resolved := notionResolvedUsers(fields)
	count := max(len(labels), len(discovery.AttendeeIDs))
	participants := make([]Participant, 0, count)
	for index := range count {
		name, id := "", ""
		if index < len(labels) {
			name = strings.TrimSpace(labels[index])
		}
		if index < len(discovery.AttendeeIDs) {
			id = strings.TrimSpace(discovery.AttendeeIDs[index])
		}
		user := resolved[id]
		if user.Name != "" {
			name = user.Name
		}
		if name != "" || user.Email != "" {
			participants = append(participants, Participant{Name: name, Email: user.Email, Role: "to"})
		}
	}
	return participants
}
