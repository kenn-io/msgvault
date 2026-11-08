package meetingcontent

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/mail"
	"strings"
)

func decodeNotion(fields map[string]jsontext.Value) Content {
	version, ok := rawInteger(fields["schema_version"])
	if !ok || (version != 1 && version != 2) {
		return unavailableContent("unsupported_schema")
	}
	content := baseRecognizedContent()
	var canonical map[string]jsontext.Value
	if version == 2 {
		if !validNotionV2Header(fields) {
			return unavailableContent("invalid_raw")
		}
		discovery, _ := notionDiscovery(fields["discovery"])
		content.Summary = decodeNotionV2Section(fields["summary"], discovery.SummaryID, discovery.MeetingID, "summary")
		content.Notes = decodeNotionV2Section(fields["notes"], discovery.NotesID, discovery.MeetingID, "notes")
		section := decodeNotionV2Section(fields["transcript"], discovery.TranscriptID, discovery.MeetingID, "transcript")
		content.Transcript = Transcript{State: section.State, Reason: section.Reason, Text: section.Text}
	} else if raw, exists := fields["canonical"]; !exists {
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
	content.Actions, content.ActionCoverage, content.ActionReason = decodeNotionActions(fields, discovery, discoveryOK, version)
	content.SourceParticipants = notionParticipants(fields, discovery, version == 2)
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
	MeetingID    string
	SummaryID    string
	NotesID      string
	TranscriptID string
	AttendeeIDs  []string
	Recording    struct{ Start, End string }
	Calendar     struct{ Start, End string }
}

func notionDiscovery(raw jsontext.Value) (notionDiscoveryWire, bool) {
	var wire struct {
		ID           string `json:"id"`
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
	out.MeetingID = wire.ID
	out.SummaryID = strings.TrimSpace(wire.MeetingNotes.Children.SummaryID)
	out.NotesID = strings.TrimSpace(wire.MeetingNotes.Children.NotesID)
	out.TranscriptID = strings.TrimSpace(wire.MeetingNotes.Children.TranscriptID)
	out.AttendeeIDs = wire.MeetingNotes.Calendar.Attendees
	out.Recording.Start, out.Recording.End = wire.MeetingNotes.Recording.Start, wire.MeetingNotes.Recording.End
	out.Calendar.Start, out.Calendar.End = wire.MeetingNotes.Calendar.Start, wire.MeetingNotes.Calendar.End
	return out, true
}

func decodeNotionActions(fields map[string]jsontext.Value, discovery notionDiscoveryWire, discoveryOK bool, version int) ([]Action, Coverage, string) {
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
		if tree.id == "" && (version != 2 || !notionV2HasRoot(fields[tree.key])) {
			continue
		}
		referenced = true
		var blocks []jsontext.Value
		var complete bool
		if version == 2 {
			var valid bool
			blocks, complete, valid = notionV2TreeBlocks(fields[tree.key], tree.id, discovery.MeetingID, tree.key)
			if !valid {
				blocks = nil
				complete = false
			}
		} else {
			blocks, complete = notionTreeBlocks(fields[tree.key], tree.id)
		}
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

func notionTreeBlocks(raw jsontext.Value, expectedRootID string) ([]jsontext.Value, bool) {
	if len(raw) == 0 || isNull(raw) {
		return nil, false
	}
	var tree struct {
		Root  jsontext.Value   `json:"root"`
		Pages []jsontext.Value `json:"pages"`
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
		blocks:      []jsontext.Value{tree.Root},
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
	pages       []jsontext.Value
	nextPage    int
	pageVisits  int
	blockVisits int
	blocks      []jsontext.Value
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

func notionTraversalPage(raw jsontext.Value) ([]jsontext.Value, bool, string, bool) {
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false, "", false
	}
	resultsRaw, ok := fields["results"]
	if !ok || isNull(resultsRaw) {
		return nil, false, "", false
	}
	var results []jsontext.Value
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

func notionBlockTraversalHeader(raw jsontext.Value) (string, bool, bool) {
	var fields map[string]jsontext.Value
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

// V2 records admitted blocks directly. Unlike v1 it never needs to reconstruct
// traversal from unfiltered child-list pages, which could include other meetings.
type notionFlatTree struct {
	Root                 jsontext.Value   `json:"root"`
	Blocks               []jsontext.Value `json:"blocks"`
	Pages                []jsontext.Value `json:"pages"`
	Complete             *bool            `json:"complete"`
	Filtered             *bool            `json:"filtered"`
	Preserved            bool             `json:"preserved"`
	SourceMeetingBlock   jsontext.Value   `json:"source_meeting_block"`
	SourceLastEditedTime string           `json:"source_last_edited_time"`
}

func validNotionV2Header(fields map[string]jsontext.Value) bool {
	var discovery, block struct {
		Object       string                    `json:"object"`
		ID           string                    `json:"id"`
		Type         string                    `json:"type"`
		MeetingNotes map[string]jsontext.Value `json:"meeting_notes"`
	}
	if json.Unmarshal(fields["discovery"], &discovery) != nil || json.Unmarshal(fields["meeting_block"], &block) != nil {
		return false
	}
	discoveryRaw, blockRaw := fields["discovery"].Clone(), fields["meeting_block"].Clone()
	if discoveryRaw.Canonicalize() != nil || blockRaw.Canonicalize() != nil || !bytes.Equal(discoveryRaw, blockRaw) {
		return false
	}
	return discovery.Object == "block" && discovery.Type == "meeting_notes" && discovery.ID != "" &&
		block.Object == "block" && block.ID == discovery.ID && block.Type == "meeting_notes" && block.MeetingNotes != nil && discovery.MeetingNotes != nil
}

func notionV2HasRoot(raw jsontext.Value) bool {
	var tree notionFlatTree
	return json.Unmarshal(raw, &tree) == nil && len(tree.Root) > 0 && !isNull(tree.Root)
}

func notionV2TreeBlocks(raw jsontext.Value, expectedRootID, meetingID, section string) ([]jsontext.Value, bool, bool) {
	var tree notionFlatTree
	if json.Unmarshal(raw, &tree) != nil || tree.Complete == nil || tree.Filtered == nil || len(tree.Pages) > 0 {
		return nil, false, false
	}
	if len(tree.Root) == 0 || isNull(tree.Root) {
		return nil, false, len(tree.Blocks) == 0
	}
	if len(tree.Blocks) == 0 || len(tree.Blocks) > maxNotionTraversalBlocks {
		return nil, false, false
	}
	root, first := tree.Root.Clone(), tree.Blocks[0].Clone()
	if root.Canonicalize() != nil || first.Canonicalize() != nil || !bytes.Equal(root, first) {
		return nil, false, false
	}
	if tree.Preserved {
		var source struct {
			Object         string `json:"object"`
			ID             string `json:"id"`
			Type           string `json:"type"`
			LastEditedTime string `json:"last_edited_time"`
		}
		prior, ok := notionDiscovery(tree.SourceMeetingBlock)
		if !ok || json.Unmarshal(tree.SourceMeetingBlock, &source) != nil || source.ID != meetingID || source.Object != "block" || source.Type != "meeting_notes" || source.LastEditedTime != tree.SourceLastEditedTime {
			return nil, false, false
		}
		switch section {
		case "summary":
			expectedRootID = prior.SummaryID
		case "notes":
			expectedRootID = prior.NotesID
		case "transcript":
			expectedRootID = prior.TranscriptID
		default:
			return nil, false, false
		}
	}
	rootID, _, valid := notionBlockTraversalHeader(tree.Root)
	if !valid || expectedRootID == "" || rootID != expectedRootID {
		return nil, false, false
	}
	seen := make(map[string]bool, len(tree.Blocks))
	for _, rawBlock := range tree.Blocks {
		var block struct {
			Object string `json:"object"`
			ID     string `json:"id"`
			Type   string `json:"type"`
		}
		if json.Unmarshal(rawBlock, &block) != nil || block.Object != "block" || block.ID == "" || block.Type == "" || block.Type == "meeting_notes" || seen[block.ID] {
			return nil, false, false
		}
		seen[block.ID] = true
	}
	return tree.Blocks, *tree.Complete && !*tree.Filtered, true
}

func decodeNotionV2Section(raw jsontext.Value, expectedRootID, meetingID, section string) Section {
	if expectedRootID == "" && !notionV2HasRoot(raw) {
		return Section{State: StateEmpty}
	}
	blocks, complete, valid := notionV2TreeBlocks(raw, expectedRootID, meetingID, section)
	if !valid {
		return Section{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	lines := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if text := notionBlockText(block); text != "" {
			lines = append(lines, text)
		}
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if !complete {
		if text == "" {
			return Section{State: StateUnavailable, Reason: "incomplete_tree"}
		}
		return Section{State: StateTruncated, Reason: "incomplete_tree", Text: text}
	}
	if text == "" {
		return Section{State: StateEmpty}
	}
	return Section{State: StateAvailable, Text: text}
}

func notionBlockText(raw jsontext.Value) string {
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	var kind string
	if json.Unmarshal(fields["type"], &kind) != nil {
		return ""
	}
	var payload struct {
		RichText []struct {
			PlainText string `json:"plain_text"`
		} `json:"rich_text"`
		Caption []struct {
			PlainText string `json:"plain_text"`
		} `json:"caption"`
		Title      jsontext.Value `json:"title"`
		Expression string         `json:"expression"`
		Cells      [][]struct {
			PlainText string `json:"plain_text"`
		} `json:"cells"`
	}
	if json.Unmarshal(fields[kind], &payload) != nil {
		return ""
	}
	richText := func(parts []struct {
		PlainText string `json:"plain_text"`
	}) string {
		var out strings.Builder
		for _, part := range parts {
			out.WriteString(part.PlainText)
		}
		return out.String()
	}

	text := richText(payload.RichText)
	switch kind {
	case "paragraph", "heading_1", "heading_2", "heading_3", "bulleted_list_item", "numbered_list_item", "quote", "callout", "toggle", "to_do":
	default:
		lines := []string{}
		if text != "" {
			lines = append(lines, text)
		}
		if caption := richText(payload.Caption); caption != "" {
			lines = append(lines, caption)
		}
		var title string
		if json.Unmarshal(payload.Title, &title) == nil {
			if title = strings.TrimSpace(title); title != "" {
				lines = append(lines, title)
			}
		} else {
			var parts []struct {
				PlainText string `json:"plain_text"`
			}
			if json.Unmarshal(payload.Title, &parts) == nil {
				if title = richText(parts); title != "" {
					lines = append(lines, title)
				}
			}
		}
		if expression := strings.TrimSpace(payload.Expression); expression != "" {
			lines = append(lines, expression)
		}
		for _, cell := range payload.Cells {
			if text := richText(cell); text != "" {
				lines = append(lines, text)
			}
		}
		text = strings.Join(lines, "\n")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	switch kind {
	case "bulleted_list_item":
		text = "- " + text
	case "numbered_list_item":
		text = "1. " + text
	case "quote":
		text = "> " + text
	}
	return text
}

type notionResolvedUser struct {
	Name  string
	Email string
}

func notionResolvedUsers(fields map[string]jsontext.Value, requireVerified bool) map[string]notionResolvedUser {
	var users []struct {
		ID            string         `json:"id"`
		Name          string         `json:"name"`
		Email         string         `json:"email"`
		EmailVerified jsontext.Value `json:"email_verified"`
	}
	_ = json.Unmarshal(fields["resolved_users"], &users)
	out := make(map[string]notionResolvedUser, len(users))
	for _, user := range users {
		if id := strings.TrimSpace(user.ID); id != "" {
			email := normalizeExplicitEmail(user.Email)
			if requireVerified {
				address, err := mail.ParseAddress(email)
				if !bytes.Equal(bytes.TrimSpace(user.EmailVerified), []byte("true")) || err != nil || address.Address != email {
					email = ""
				}
			}
			out[id] = notionResolvedUser{Name: strings.TrimSpace(user.Name), Email: email}
		}
	}
	return out
}

func notionAction(raw jsontext.Value, ordinal int) (Action, bool) {
	var fields map[string]jsontext.Value
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return Action{}, false
	}
	id, idOK := optionalString(fields["id"])
	id = strings.TrimSpace(id)
	if !idOK || id == "" {
		return Action{}, false
	}
	var todo map[string]jsontext.Value
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

func notionParticipants(fields map[string]jsontext.Value, discovery notionDiscoveryWire, requireVerified bool) []Participant {
	var labels []string
	_ = json.Unmarshal(fields["attendee_labels"], &labels)
	resolved := notionResolvedUsers(fields, requireVerified)
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
