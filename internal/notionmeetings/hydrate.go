package notionmeetings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
)

const (
	maxHydrationDepth    = 32
	maxHydrationRequests = 256
	maxHydrationBytes    = 32 << 20
	maxHydrationBlocks   = 10000
)

type hydrationSource interface {
	RetrievePageMarkdown(ctx context.Context, pageID string, includeTranscript bool) (*MarkdownPage, error)
	RetrieveBlock(ctx context.Context, blockID string) (*Block, error)
	RetrieveBlockChildren(ctx context.Context, blockID, cursor string) (*BlockPage, error)
	ListUsers(ctx context.Context, cursor string) (*UserPage, error)
}

type blockTree struct {
	Root   *Block
	Blocks []Block
	Pages  []json.RawMessage
}

type HydratedMeeting struct {
	Discovery                  MeetingNote
	MeetingBlock               *Block
	SummaryTree                blockTree
	NotesTree                  blockTree
	TranscriptTree             blockTree
	PageMarkdown               *MarkdownPage
	Summary                    string
	Notes                      string
	Transcript                 string
	Attendees                  []meetingarchive.Person
	AttendeeLabels             []string
	UnresolvedAttendeeIDs      []string
	AttendeeResolutionDegraded bool
	Warnings                   []string
	MarkdownTranscriptFallback bool
	ResolvedUsers              []resolvedUser
}

type resolvedUser struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Email         string   `json:"email,omitempty"`
	EmailAliases  []string `json:"email_aliases,omitempty"`
	EmailVerified bool     `json:"email_verified,omitempty"`
}

type Hydrator struct {
	source           hydrationSource
	users            map[string]User
	usersRead        bool
	usersUnavailable bool
	usersIncomplete  bool
	usersFailure     error
	requests         int
	bytes            int
	blocks           int
}

func NewHydrator(source hydrationSource) *Hydrator {
	return &Hydrator{source: source}
}

func (h *Hydrator) Hydrate(ctx context.Context, meeting MeetingNote) (*HydratedMeeting, error) {
	if h == nil || h.source == nil {
		return nil, errors.New("notion meeting hydrator is unavailable")
	}
	if strings.TrimSpace(meeting.ID) == "" {
		return nil, errors.New("notion meeting query returned a blank block ID")
	}
	h.requests, h.bytes, h.blocks = 0, 0, 0

	meetingBlock, err := h.retrieveBlock(ctx, meeting.ID)
	if err != nil {
		return nil, fmt.Errorf("retrieve meeting-note block %s: %w", meeting.ID, err)
	}
	if meetingBlock.Object != "block" || meetingBlock.ID != meeting.ID || meetingBlock.Type != "meeting_notes" {
		return nil, fmt.Errorf("%w: retrieved meeting-note block has unexpected identity or type", ErrMalformedResponse)
	}
	if strings.TrimSpace(meeting.Parent.PageID) == "" {
		meeting.Parent = meetingBlock.Parent
	}
	if strings.TrimSpace(meeting.Parent.PageID) == "" {
		return nil, fmt.Errorf("notion meeting %s has no parent page ID", meeting.ID)
	}
	result := &HydratedMeeting{Discovery: meeting, MeetingBlock: meetingBlock}
	children := meeting.MeetingNotes.Children
	if children.SummaryBlockID != "" {
		result.SummaryTree, err = h.readTree(ctx, children.SummaryBlockID)
		if err != nil {
			return nil, fmt.Errorf("hydrate summary block %s: %w", children.SummaryBlockID, err)
		}
		result.Summary = renderTree(result.SummaryTree)
	}
	if children.NotesBlockID != "" {
		result.NotesTree, err = h.readTree(ctx, children.NotesBlockID)
		if err != nil {
			return nil, fmt.Errorf("hydrate notes block %s: %w", children.NotesBlockID, err)
		}
		result.Notes = renderTree(result.NotesTree)
	}
	if children.TranscriptBlockID != "" {
		result.TranscriptTree, err = h.readTree(ctx, children.TranscriptBlockID)
		if err != nil {
			if !transcriptTemporarilyUnavailable(err) {
				return nil, fmt.Errorf("hydrate transcript block %s: %w", children.TranscriptBlockID, err)
			}
			result.Warnings = append(result.Warnings, "structured transcript was temporarily unavailable; continued with page Markdown")
		} else {
			result.Transcript = renderTree(result.TranscriptTree)
		}
	}

	if err := h.reserveRequest(); err != nil {
		return nil, err
	}
	result.PageMarkdown, err = h.source.RetrievePageMarkdown(ctx, meeting.Parent.PageID, true)
	if err != nil {
		return nil, fmt.Errorf("retrieve Notion page Markdown: %w", err)
	}
	if err := h.reserveBytes(result.PageMarkdown.Raw); err != nil {
		return nil, err
	}
	markdownTranscript := extractMarkdownTranscript(result.PageMarkdown.Markdown)
	markdownComplete := !result.PageMarkdown.Truncated && len(result.PageMarkdown.UnknownBlockIDs) == 0
	if result.Transcript == "" && markdownTranscript != "" && !markdownComplete {
		result.Warnings = append(result.Warnings, "page Markdown transcript was incomplete; transcript remains pending")
	} else if result.Transcript == "" && markdownTranscript != "" {
		result.Transcript = markdownTranscript
		result.MarkdownTranscriptFallback = true
		result.Warnings = append(result.Warnings, "structured transcript was empty; used page Markdown transcript")
	} else if result.Transcript != "" && markdownTranscript != "" && markdownComplete &&
		normalizedContent(result.Transcript) != normalizedContent(markdownTranscript) {
		result.Warnings = append(result.Warnings, "structured transcript and page Markdown transcript differed")
	}

	if err := h.resolveAttendees(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (h *Hydrator) retrieveBlock(ctx context.Context, id string) (*Block, error) {
	if err := h.reserveRequest(); err != nil {
		return nil, err
	}
	block, err := h.source.RetrieveBlock(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := h.reserveBytes(block.Raw); err != nil {
		return nil, err
	}
	return block, nil
}

func (h *Hydrator) readTree(ctx context.Context, id string) (blockTree, error) {
	root, err := h.retrieveBlock(ctx, id)
	if err != nil {
		return blockTree{}, err
	}
	tree := blockTree{Root: root, Blocks: []Block{*root}}
	h.blocks++
	if h.blocks > maxHydrationBlocks {
		return blockTree{}, errors.New("notion block traversal exceeded block limit")
	}
	if root.HasChildren {
		if err := h.readChildren(ctx, root.ID, 1, &tree); err != nil {
			return blockTree{}, err
		}
	}
	return tree, nil
}

func (h *Hydrator) readChildren(ctx context.Context, blockID string, depth int, tree *blockTree) error {
	if depth > maxHydrationDepth {
		return fmt.Errorf("notion block traversal exceeded depth limit at %s", blockID)
	}
	cursor := ""
	seen := map[string]struct{}{}
	for {
		if err := h.reserveRequest(); err != nil {
			return err
		}
		page, err := h.source.RetrieveBlockChildren(ctx, blockID, cursor)
		if err != nil {
			return err
		}
		if err := h.reserveBytes(page.Raw); err != nil {
			return err
		}
		tree.Pages = append(tree.Pages, append(json.RawMessage(nil), page.Raw...))
		for _, child := range page.Results {
			h.blocks++
			if h.blocks > maxHydrationBlocks {
				return errors.New("notion block traversal exceeded block limit")
			}
			tree.Blocks = append(tree.Blocks, child)
			if child.HasChildren {
				if err := h.readChildren(ctx, child.ID, depth+1, tree); err != nil {
					return err
				}
			}
		}
		if !page.HasMore {
			return nil
		}
		next := strings.TrimSpace(page.NextCursor)
		if next == "" {
			return fmt.Errorf("notion child block %s has_more without next_cursor", blockID)
		}
		if _, duplicate := seen[next]; duplicate {
			return fmt.Errorf("notion child block %s returned repeated child cursor %q", blockID, next)
		}
		seen[next] = struct{}{}
		cursor = next
	}
}

func (h *Hydrator) resolveAttendees(ctx context.Context, result *HydratedMeeting) error {
	if !h.usersRead {
		h.users = map[string]User{}
		cursor := ""
		seen := map[string]struct{}{}
		for {
			if err := h.reserveRequest(); err != nil {
				return err
			}
			page, err := h.source.ListUsers(ctx, cursor)
			if err != nil {
				if errors.Is(err, ErrUnauthorized) || errors.Is(err, context.Canceled) ||
					errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				h.usersUnavailable = true
				h.usersFailure = err
				h.usersRead = true
				break
			}
			if err := h.reserveBytes(page.Raw); err != nil {
				return err
			}
			for _, user := range page.Results {
				h.users[user.ID] = user
			}
			if !page.HasMore {
				h.usersRead = true
				break
			}
			next := strings.TrimSpace(page.NextCursor)
			if next == "" {
				h.usersUnavailable = true
				h.usersIncomplete = true
				h.usersFailure = errors.New("has_more without next_cursor")
				h.usersRead = true
				break
			}
			if _, duplicate := seen[next]; duplicate {
				h.usersUnavailable = true
				h.usersIncomplete = true
				h.usersFailure = fmt.Errorf("repeated cursor %q", next)
				h.usersRead = true
				break
			}
			seen[next] = struct{}{}
			cursor = next
		}
	}
	if h.usersUnavailable {
		result.AttendeeResolutionDegraded = true
		if h.usersIncomplete {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("Notion User Information pagination was incomplete: %v", h.usersFailure))
		} else if errors.Is(h.usersFailure, ErrUserInformation) {
			result.Warnings = append(result.Warnings, "Notion User Information access unavailable; attendee emails were not resolved")
		} else {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("Notion User Information lookup failed: %v; attendee emails were not resolved", h.usersFailure))
		}
	}

	for _, id := range result.Discovery.MeetingNotes.CalendarEvent.Attendees {
		user, ok := h.users[id]
		label := id
		if ok && strings.TrimSpace(user.Name) != "" {
			label = strings.TrimSpace(user.Name)
		}
		result.AttendeeLabels = append(result.AttendeeLabels, label)
		if !ok || !user.Person.EmailVerified || strings.TrimSpace(user.Person.Email) == "" {
			result.UnresolvedAttendeeIDs = append(result.UnresolvedAttendeeIDs, id)
			continue
		}
		result.Attendees = append(result.Attendees, meetingarchive.Person{
			Name:  strings.TrimSpace(user.Name),
			Email: strings.ToLower(strings.TrimSpace(user.Person.Email)),
		})
		result.ResolvedUsers = append(result.ResolvedUsers, resolvedUser{
			ID: id, Name: strings.TrimSpace(user.Name),
			Email: strings.ToLower(strings.TrimSpace(user.Person.Email)), EmailVerified: true,
		})
	}
	return nil
}

func (h *Hydrator) reserveRequest() error {
	h.requests++
	if h.requests > maxHydrationRequests {
		return errors.New("notion meeting hydration exceeded request limit")
	}
	return nil
}

func (h *Hydrator) reserveBytes(raw []byte) error {
	h.bytes += len(raw)
	if h.bytes > maxHydrationBytes {
		return errors.New("notion meeting hydration exceeded response byte limit")
	}
	return nil
}

func renderTree(tree blockTree) string {
	lines := make([]string, 0, len(tree.Blocks))
	for _, block := range tree.Blocks {
		text := strings.TrimSpace(block.PlainText())
		if text == "" {
			continue
		}
		switch block.Type {
		case "bulleted_list_item":
			text = "- " + text
		case "numbered_list_item":
			text = "1. " + text
		case "quote":
			text = "> " + text
		}
		lines = append(lines, text)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func transcriptTemporarilyUnavailable(err error) bool {
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404 && apiErr.Code == "object_not_found"
}

func extractMarkdownTranscript(markdown string) string {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	for index, line := range lines {
		headingLevel, heading, isHeading := markdownHeading(line)
		if !isHeading {
			heading = strings.TrimSpace(line)
		}
		inline, found := transcriptMarker(heading)
		if !found {
			continue
		}
		if inline != "" {
			return inline
		}
		end := len(lines)
		if !isHeading {
			headingLevel = 6
		}
		for next := index + 1; next < len(lines); next++ {
			level, _, ok := markdownHeading(lines[next])
			if ok && level <= headingLevel {
				end = next
				break
			}
		}
		return strings.TrimSpace(strings.Join(lines[index+1:end], "\n"))
	}
	return ""
}

func markdownHeading(line string) (int, string, bool) {
	trimmed := strings.TrimSpace(line)
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || (level < len(trimmed) && trimmed[level] != ' ' && trimmed[level] != '\t') {
		return 0, "", false
	}
	heading := strings.TrimSpace(trimmed[level:])
	heading = strings.TrimSpace(strings.TrimRight(heading, "#"))
	return level, heading, true
}

func transcriptMarker(value string) (string, bool) {
	label := strings.TrimSpace(value)
	inline := ""
	if colon := strings.Index(label, ":"); colon >= 0 {
		inline = strings.TrimSpace(label[colon+1:])
		label = strings.TrimSpace(label[:colon])
	}
	if !strings.EqualFold(label, "transcript") {
		return "", false
	}
	return inline, true
}

func normalizedContent(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func parseNotionTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return parsed
}
