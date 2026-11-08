package notionmeetings

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingidentity"
)

const (
	UserLookupTimeout    = time.Minute
	maxHydrationDepth    = 32
	maxHydrationRequests = 256
	maxHydrationBytes    = 32 << 20
	maxHydrationBlocks   = 10000
)

type hydrationSource interface {
	RetrieveBlock(ctx context.Context, blockID string) (*Block, error)
	RetrieveBlockChildren(ctx context.Context, blockID, cursor string) (*BlockPage, error)
	ListUsers(ctx context.Context, cursor string) (*UserPage, error)
}

type blockTree struct {
	Root                 *Block
	Blocks               []Block
	Complete             bool
	Filtered             bool
	SourceLastEditedTime string
	Preserved            bool
	SourceMeetingBlock   jsontext.Value
}

type HydratedMeeting struct {
	// With a users token, archived emails are recovered only for failed lookups.
	failedAttendeeIDs map[string]bool

	Discovery                  MeetingNote
	MeetingBlock               *Block
	SummaryTree                blockTree
	NotesTree                  blockTree
	TranscriptTree             blockTree
	Summary                    string
	Notes                      string
	Transcript                 string
	Attendees                  []meetingarchive.Person
	AttendeeLabels             []string
	UnresolvedAttendeeIDs      []string
	AttendeeResolutionDegraded bool
	Warnings                   []string
	ResolvedUsers              []resolvedUser
}

type resolvedUser struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	Email         string   `json:"email,omitempty"`
	EmailAliases  []string `json:"email_aliases,omitempty"`
	EmailVerified bool     `json:"email_verified,omitzero"`
}

// UserSource is used only for workspace members and guest identities.
type UserSource interface {
	RetrieveUser(ctx context.Context, id string) (*User, error)
}

type Hydrator struct {
	userSource       UserSource
	source           hydrationSource
	users            map[string]*User
	usersRead        bool
	usersUnavailable bool
	usersIncomplete  bool
	usersFailure     error
	requests         int
	bytes            int
	blocks           int
	visited          map[string]bool
}

func NewHydrator(source hydrationSource) *Hydrator {
	return &Hydrator{source: source, users: map[string]*User{}}
}

// WithUserSource routes user requests to a separate credential.
func (h *Hydrator) WithUserSource(source UserSource) *Hydrator {
	h.userSource = source
	return h
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
		return nil, hydrationFailure("retrieve meeting-note block", err)
	}

	if meetingBlock == nil || meetingBlock.ID != meeting.ID {
		return nil, fmt.Errorf("%w: retrieved meeting-note block has unexpected identity", ErrMalformedResponse)
	}
	return h.hydrateBlock(ctx, meetingBlock)
}

// HydrateBlock hydrates an already retrieved, authoritative meeting header.
// Callers performing identity selection must match this header before calling.
func (h *Hydrator) HydrateBlock(ctx context.Context, meetingBlock *Block) (*HydratedMeeting, error) {
	if h == nil || h.source == nil {
		return nil, errors.New("notion meeting hydrator is unavailable")
	}
	h.requests, h.bytes, h.blocks = 0, 0, 0
	if meetingBlock != nil {
		if err := h.reserveBytes(meetingBlock.Raw); err != nil {
			return nil, err
		}
	}
	return h.hydrateBlock(ctx, meetingBlock)
}

func (h *Hydrator) hydrateBlock(ctx context.Context, meetingBlock *Block) (*HydratedMeeting, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if meetingBlock == nil || meetingBlock.Object != "block" || meetingBlock.Type != "meeting_notes" || strings.TrimSpace(meetingBlock.ID) == "" {
		return nil, fmt.Errorf("%w: invalid meeting-note block", ErrMalformedResponse)
	}
	if err := validateBlockResponse(meetingBlock.Raw, meetingBlock.ID, "meeting_notes"); err != nil {
		return nil, hydrationFailure("validate meeting header", err)
	}
	var meeting MeetingNote
	if err := json.Unmarshal(meetingBlock.Raw, &meeting); err != nil || meeting.Object != "block" || meeting.ID != meetingBlock.ID || meeting.Type != "meeting_notes" {
		return nil, fmt.Errorf("%w: invalid authoritative meeting header", ErrMalformedResponse)
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(meetingBlock.Raw, &fields); err != nil || len(fields["meeting_notes"]) == 0 || string(fields["meeting_notes"]) == "null" {
		return nil, fmt.Errorf("%w: missing meeting-note data", ErrMalformedResponse)
	}
	h.visited = map[string]bool{meeting.ID: true}
	result := &HydratedMeeting{Discovery: meeting, MeetingBlock: meetingBlock}
	children := meeting.MeetingNotes.Children
	sections := []struct {
		name, id string
		tree     *blockTree
		text     *string
	}{
		{"summary", children.SummaryBlockID, &result.SummaryTree, &result.Summary},
		{"notes", children.NotesBlockID, &result.NotesTree, &result.Notes},
		{"transcript", children.TranscriptBlockID, &result.TranscriptTree, &result.Transcript},
	}
	for _, section := range sections {
		if section.id == "" {
			continue
		}
		tree, err := h.readTree(ctx, section.id)
		tree.SourceLastEditedTime = meeting.LastEditedTime
		if err != nil {
			if !sectionTemporarilyUnavailable(err) {
				return nil, hydrationFailure("hydrate "+section.name, err)
			}
			result.Warnings = append(result.Warnings, section.name+" blocks were incomplete; retained available meeting content")
		} else if !tree.Complete {
			result.Warnings = append(result.Warnings, section.name+" blocks were incomplete; retained available meeting content")
		}
		*section.tree = tree
		*section.text = renderTree(tree)
	}
	if err := h.resolveAttendees(ctx, result); err != nil {
		return nil, hydrationFailure("resolve attendees", err)
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
	if block == nil {
		return nil, fmt.Errorf("%w: missing block", ErrMalformedResponse)
	}
	if err := h.reserveBytes(block.Raw); err != nil {
		return nil, err
	}
	return block, nil
}

func (h *Hydrator) readTree(ctx context.Context, id string) (blockTree, error) {
	if h.visited[id] {
		return blockTree{}, nil
	}
	root, err := h.retrieveBlock(ctx, id)
	if err != nil {
		return blockTree{}, err
	}
	if root.ID != id || root.Object != "block" || root.Type == "" {
		return blockTree{}, fmt.Errorf("%w: invalid section root", ErrMalformedResponse)
	}
	if root.Type == "meeting_notes" {
		return blockTree{Filtered: true}, nil
	}
	h.visited[id] = true
	h.blocks++
	if h.blocks > maxHydrationBlocks {
		return blockTree{}, errHydrationLimit
	}
	tree := blockTree{Root: root, Blocks: []Block{*root}, Complete: true}
	if root.HasChildren {
		if err := h.readChildren(ctx, root.ID, 1, &tree); err != nil {
			tree.Complete = false
			return tree, err
		}
	}
	return tree, nil
}

func (h *Hydrator) readChildren(ctx context.Context, blockID string, depth int, tree *blockTree) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > maxHydrationDepth {
		return errHydrationLimit
	}
	cursor := ""
	seen := map[string]bool{}
	for {
		if err := h.reserveRequest(); err != nil {
			return err
		}
		page, err := h.source.RetrieveBlockChildren(ctx, blockID, cursor)
		if err != nil {
			return err
		}
		if page == nil {
			return fmt.Errorf("%w: missing child page", ErrMalformedResponse)
		}
		if err := h.reserveBytes(page.Raw); err != nil {
			return err
		}
		for _, child := range page.Results {
			if child.ID == "" || child.Object != "block" || child.Type == "" {
				return fmt.Errorf("%w: invalid section child", ErrMalformedResponse)
			}
			if child.Type == "meeting_notes" {
				tree.Filtered = true
				continue
			}
			if h.visited[child.ID] {
				tree.Complete = false
				continue
			}
			h.visited[child.ID] = true
			h.blocks++
			if h.blocks > maxHydrationBlocks {
				return errHydrationLimit
			}
			tree.Blocks = append(tree.Blocks, child)
			if child.HasChildren {
				if err := h.readChildren(ctx, child.ID, depth+1, tree); err != nil {
					return err
				}
			}
		}
		if !page.HasMore {
			if strings.TrimSpace(page.NextCursor) != "" {
				tree.Complete = false
			}
			return nil
		}
		next := strings.TrimSpace(page.NextCursor)
		if next == "" || seen[next] {
			tree.Complete = false
			return nil
		}
		seen[next] = true
		cursor = next
	}
}

var errHydrationLimit = errors.New("notion meeting hydration exceeded traversal limit")

func sectionTemporarilyUnavailable(err error) bool {
	if errors.Is(err, errHydrationLimit) || errors.Is(err, ErrRateLimited) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Status == 404 || apiErr.Status >= 500)
}

func (h *Hydrator) resolveAttendees(ctx context.Context, result *HydratedMeeting) error {
	if h.userSource == nil && !h.usersRead {
		cursor := ""
		seen := map[string]struct{}{}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := h.reserveRequest(); err != nil {
				h.usersUnavailable, h.usersRead, h.usersIncomplete, h.usersFailure = true, true, true, err
				break
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
			if page == nil {
				h.usersUnavailable, h.usersRead, h.usersIncomplete, h.usersFailure = true, true, true, ErrMalformedResponse
				break
			}
			if err := h.reserveBytes(page.Raw); err != nil {
				h.usersUnavailable, h.usersRead, h.usersIncomplete, h.usersFailure = true, true, true, err
				break
			}
			for i := range page.Results {
				user := &page.Results[i]
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
	if h.userSource == nil && h.usersUnavailable {
		result.AttendeeResolutionDegraded = true
		if h.usersIncomplete {
			result.Warnings = append(result.Warnings,
				"Notion User Information pagination was incomplete; attendee emails were not resolved")
		} else if errors.Is(h.usersFailure, ErrUserInformation) {
			result.Warnings = append(result.Warnings, "Notion User Information access unavailable; attendee emails were not resolved")
		} else {
			result.Warnings = append(result.Warnings,
				"Notion User Information lookup failed; attendee emails were not resolved")
		}
	}

	// Preserve archived identity only for an attendee whose lookup failed. A
	// successfully fetched unverified user stays authoritative even when a later
	// directory page fails for other attendees.
	result.failedAttendeeIDs = map[string]bool{}
	for _, id := range result.Discovery.MeetingNotes.CalendarEvent.Attendees {
		user := h.users[id]
		if h.userSource == nil && h.usersUnavailable && user == nil {
			result.failedAttendeeIDs[id] = true
		}
		if h.userSource != nil {
			var err error
			user, err = h.lookupUser(ctx, id)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if len(result.failedAttendeeIDs) == 0 {
					result.Warnings = append(result.Warnings, "Notion attendee lookup failed; preserved meeting content")
				}
				result.failedAttendeeIDs[id] = true
				result.AttendeeResolutionDegraded = true
			}
		}
		label := id
		if user != nil && strings.TrimSpace(user.Name) != "" {
			label = strings.TrimSpace(user.Name)
		}
		result.AttendeeLabels = append(result.AttendeeLabels, label)
		email := ""
		if user != nil && user.Person.EmailVerified {
			email = normalizedUserEmail(user.Person.Email)
		}
		if email == "" {
			result.UnresolvedAttendeeIDs = append(result.UnresolvedAttendeeIDs, id)
			continue
		}
		result.Attendees = append(result.Attendees, meetingarchive.Person{
			Name: strings.TrimSpace(user.Name), Email: email, Anchor: userAnchor(id),
		})
		result.ResolvedUsers = append(result.ResolvedUsers, resolvedUser{
			ID: id, Name: strings.TrimSpace(user.Name), Email: email, EmailVerified: true,
		})
	}
	return nil
}

// A verified provider flag does not make a malformed address usable identity.
func normalizedUserEmail(value string) string {
	email := meetingidentity.Normalize(value)
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return ""
	}
	return email
}

// lookupUser shares successful responses and service failures across this import.
func (h *Hydrator) lookupUser(ctx context.Context, id string) (*User, error) {
	if user, cached := h.users[id]; cached {
		if user == nil {
			return nil, errors.New("notion attendee information is unavailable")
		}
		return user, nil
	}
	if h.usersUnavailable {
		return nil, h.usersFailure
	}
	if err := h.reserveRequest(); err != nil {
		return nil, err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, UserLookupTimeout)
	user, err := h.userSource.RetrieveUser(lookupCtx, id)
	cancel()
	if err != nil {
		var apiErr *APIError
		if errors.Is(err, ErrMalformedResponse) || errors.As(err, &apiErr) && apiErr.Status == 404 {
			h.users[id] = nil
		} else {
			h.usersUnavailable, h.usersFailure = true, err
		}
		return nil, err
	}
	if user == nil {
		h.users[id] = nil
		return nil, ErrMalformedResponse
	}
	if err := h.reserveBytes(user.Raw); err != nil {
		return nil, err
	}
	h.users[id] = user
	return user, nil
}

// Preserve error identity for callers without exposing request URLs, block IDs,
// cursors or arbitrary provider error strings through progress diagnostics.
type hydrationError struct {
	stage string
	cause error
}

func (e *hydrationError) Error() string {
	reason := "Notion content is unavailable"
	switch {
	case errors.Is(e.cause, context.Canceled):
		reason = "operation canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		reason = "operation deadline exceeded"
	case errors.Is(e.cause, ErrUnauthorized):
		reason = "integration token is invalid or expired"
	case errors.Is(e.cause, ErrReadContent):
		reason = "Read Content access is unavailable"
	case errors.Is(e.cause, ErrMalformedResponse):
		reason = "malformed response"
	case errors.Is(e.cause, ErrRateLimited):
		reason = "provider retry budget exhausted"
	case errors.Is(e.cause, errHydrationLimit):
		reason = errHydrationLimit.Error()
	}
	return e.stage + ": " + reason
}
func (e *hydrationError) Unwrap() error { return e.cause }
func hydrationFailure(stage string, err error) error {
	return &hydrationError{stage: stage, cause: err}
}

func (h *Hydrator) reserveRequest() error {
	h.requests++
	if h.requests > maxHydrationRequests {
		return errHydrationLimit
	}
	return nil
}

func (h *Hydrator) reserveBytes(raw []byte) error {
	h.bytes += len(raw)
	if h.bytes > maxHydrationBytes {
		return errHydrationLimit
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

func parseNotionTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return parsed
}
