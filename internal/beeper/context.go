package beeper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ContextAnchor binds a provider cursor to one observed message and its content.
type ContextAnchor struct {
	Cursor    string    `json:"cursor"`
	MessageID string    `json:"message_id"`
	Timestamp time.Time `json:"timestamp"`
	Digest    string    `json:"message_digest"`
}

// ContextBounds limits the selected live scope, including linked reply context.
type ContextBounds struct {
	InitialMessages int
	MaxEvents       int
	MaxBytes        int
	MaxRequests     int
	Timeout         time.Duration
}

// Validate refuses invalid bounds before any provider request.
func (b ContextBounds) Validate() error {
	if b.InitialMessages < 1 || b.InitialMessages > b.MaxEvents || b.MaxEvents < 1 || b.MaxEvents > 200 || b.MaxBytes < 1 || b.MaxBytes > 1<<20 || b.MaxRequests < 1 || b.MaxRequests > 128 || b.Timeout <= 0 || b.Timeout > 2*time.Minute {
		return errors.New("invalid context bounds")
	}
	return nil
}

// DefaultContextBounds authorizes a recent 50-event window within fixed caps.
func DefaultContextBounds() ContextBounds {
	return ContextBounds{50, 200, 128 << 10, 64, 30 * time.Second}
}

// ContextSnapshot states completeness only for its declared bounded scope.
type ContextSnapshot struct {
	Complete            bool           `json:"complete"`
	Changed             bool           `json:"changed"`
	Reason              string         `json:"reason"`
	Scope               string         `json:"scope"`
	ChatID              string         `json:"chat_id"`
	AccountID           string         `json:"account_id"`
	Title               string         `json:"title"`
	Activity            string         `json:"activity"`
	LowerAnchor         *ContextAnchor `json:"lower_anchor"`
	Head                ContextAnchor  `json:"head"`
	Messages            []Message      `json:"messages"`
	OlderHistoryOmitted bool           `json:"older_history_omitted"`
	Stamp               string         `json:"stamp"`
	Requests            int            `json:"requests"`
}

// Valid checks the required identity and digest shape without provider access.
func (a ContextAnchor) Valid() bool {
	digest, err := hex.DecodeString(a.Digest)
	return a.Cursor != "" && a.MessageID != "" && !a.Timestamp.IsZero() && err == nil && len(digest) == sha256.Size
}

// contextProjection is exactly the message supplied to classification. It
// excludes volatile transport locations and never downloads an attachment.
func contextProjection(m Message) Message {
	m.Raw = nil
	m.Attachments = append([]Attachment(nil), m.Attachments...)
	for i := range m.Attachments {
		m.Attachments[i].SrcURL = ""
	}
	return m
}
func contextDigest(value any) (string, error) {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// CaptureContext reads one bounded scope. The caller recaptures that same
// scope after inference; this is not an atomic provider snapshot or a sync
// completeness certificate. Reads here have one HTTP attempt and no redirect.
func (c *Client) CaptureContext(ctx context.Context, accountID, chatID string, lower *ContextAnchor, b ContextBounds) (snapshot ContextSnapshot, err error) {
	snapshot = ContextSnapshot{ChatID: chatID, AccountID: accountID, LowerAnchor: lower, Scope: "recent_messages", Messages: []Message{}}
	if err := b.Validate(); err != nil {
		return snapshot, err
	}
	// Measure the actual result after stamp/completeness and every other field
	// are final. Held results also fit the cap or return an ordinary error.
	defer func() {
		encoded, encodeErr := json.Marshal(snapshot)
		if encodeErr != nil {
			err = fmt.Errorf("encode context: %w", encodeErr)
			return
		}
		if len(encoded) <= b.MaxBytes {
			return
		}
		snapshot = ContextSnapshot{Reason: "context_byte_limit", Scope: snapshot.Scope, ChatID: chatID, AccountID: accountID, Messages: []Message{}, Requests: snapshot.Requests}
		encoded, encodeErr = json.Marshal(snapshot)
		if encodeErr != nil {
			err = fmt.Errorf("encode context: %w", encodeErr)
		} else if len(encoded) > b.MaxBytes {
			err = errors.New("context byte limit cannot fit result metadata")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	reader := c
	if c.requestBudget == nil {
		var closeScope func()
		reader, closeScope, err = c.ForBoundedOperations(b.MaxRequests)
		if err != nil {
			return snapshot, err
		}
		defer closeScope()
	}
	beforeRequests := reader.Requests()
	defer func() { snapshot.Requests = reader.Requests() - beforeRequests }()
	boundedReader := *reader
	boundedReader.readMaxBytes = int64(b.MaxBytes)
	reader = &boundedReader
	failed := func(reason string) (ContextSnapshot, error) { snapshot.Reason = reason; return snapshot, nil }
	take := func() bool {
		if snapshot.Requests >= b.MaxRequests {
			return false
		}
		snapshot.Requests++
		return true
	}
	getChat := func() (*Chat, error) {
		if !take() {
			return nil, errors.New("context request limit")
		}
		return reader.GetChat(ctx, chatID)
	}
	getMessage := func(id string) (*Message, error) {
		if !take() {
			return nil, errors.New("context request limit")
		}
		return reader.GetMessage(ctx, chatID, id)
	}
	page := func(cursor, direction string) (*ListMessagesOutput, error) {
		if !take() {
			return nil, errors.New("context request limit")
		}
		return reader.ListMessagesPage(ctx, chatID, cursor, direction)
	}
	validMessage := func(m Message) bool {
		return m.ID != "" && m.ChatID == chatID && m.AccountID == accountID && !m.Timestamp.IsZero()
	}
	validChat := func(ch *Chat) bool {
		return ch != nil && ch.ID == chatID && ch.AccountID == accountID && !ch.LastActivity.IsZero() && ch.Merge == nil && ch.MergedIntoChatID == ""
	}
	before, e := getChat()
	if e != nil {
		return failed("provider_unavailable")
	}
	if !validChat(before) {
		return failed("provider_identity_mismatch")
	}
	snapshot.Title = before.Title
	snapshot.Activity = before.LastActivity.UTC().Format(time.RFC3339Nano)
	seen := map[string]bool{}
	projectedBytes := len(before.Title) + len(accountID) + len(chatID)
	add := func(m Message) error {
		if !validMessage(m) || seen[m.ID] {
			return errors.New("invalid or repeated event identity")
		}
		if len(snapshot.Messages) >= b.MaxEvents {
			return errors.New("context event limit")
		}
		projected := contextProjection(m)
		encoded, e := json.Marshal(projected)
		if e != nil {
			return e
		}
		projectedBytes += len(encoded) + 1
		if projectedBytes > b.MaxBytes {
			return errors.New("context byte limit")
		}
		seen[m.ID] = true
		snapshot.Messages = append(snapshot.Messages, projected)
		return nil
	}
	setHead := func(cursor string, m Message) error {
		digest, e := contextDigest(contextProjection(m))
		if e != nil {
			return e
		}
		snapshot.Head = ContextAnchor{cursor, m.ID, m.Timestamp, digest}
		return nil
	}
	empty := func(p *ListMessagesOutput) bool {
		return p != nil && p.Items != nil && len(p.Items) == 0 && p.OldestCursor == "" && p.NewestCursor == ""
	}
	if lower != nil {
		snapshot.Scope = "since_reviewed_head"
		if !lower.Valid() {
			return failed("invalid_reviewed_head")
		}
		anchor, e := getMessage(lower.MessageID)
		if e != nil || anchor == nil || !validMessage(*anchor) {
			return failed("context_anchor_changed")
		}
		digest, e := contextDigest(contextProjection(*anchor))
		if e != nil || digest != lower.Digest || !anchor.Timestamp.Equal(lower.Timestamp) {
			return failed("context_anchor_changed")
		}
		if e = add(*anchor); e != nil {
			return failed("context_identity_mismatch")
		}
		snapshot.Head = *lower
		cursor := lower.Cursor
		seenCursors := map[string]bool{cursor: true}
		for {
			p, e := page(cursor, "after")
			if e != nil {
				return failed("context_unavailable_or_limit")
			}
			if empty(p) {
				break
			}
			if len(p.Items) == 0 {
				return failed("context_incomplete")
			}
			if p.NewestCursor == "" || seenCursors[p.NewestCursor] {
				return failed("context_nonadvancing")
			}
			for _, m := range p.Items {
				if e = add(m); e != nil {
					return failed("context_identity_or_limit")
				}
			}
			if e = setHead(p.NewestCursor, p.Items[0]); e != nil {
				return failed("context_invalid")
			}
			snapshot.Changed = true
			cursor = p.NewestCursor
			seenCursors[cursor] = true
		}
	} else {
		cursor, direction := "", ""
		seenCursors := map[string]bool{}
		for {
			p, e := page(cursor, direction)
			if e != nil {
				return failed("context_unavailable_or_limit")
			}
			if empty(p) {
				break
			}
			if len(p.Items) == 0 {
				return failed("context_incomplete")
			}
			if snapshot.Head.Cursor == "" {
				if p.NewestCursor == "" {
					return failed("context_missing_head")
				}
				if e = setHead(p.NewestCursor, p.Items[0]); e != nil {
					return failed("context_invalid")
				}
			}
			remaining := b.InitialMessages - len(snapshot.Messages)
			for _, m := range p.Items[:min(remaining, len(p.Items))] {
				if e = add(m); e != nil {
					return failed("context_identity_or_limit")
				}
			}
			if len(p.Items) > remaining {
				snapshot.OlderHistoryOmitted = true
				break
			}
			if p.OldestCursor == "" || p.OldestCursor == cursor || seenCursors[p.OldestCursor] {
				return failed("context_nonadvancing")
			}
			cursor, direction = p.OldestCursor, "before"
			seenCursors[cursor] = true
			if len(snapshot.Messages) == b.InitialMessages {
				probe, e := page(cursor, direction)
				if e != nil {
					return failed("context_unavailable_or_limit")
				}
				if empty(probe) {
					break
				}
				if len(probe.Items) == 0 {
					return failed("context_incomplete")
				}
				for _, m := range probe.Items {
					if !validMessage(m) || seen[m.ID] {
						return failed("context_nonadvancing")
					}
				}
				snapshot.OlderHistoryOmitted = true
				break
			}
		}
		snapshot.Changed = len(snapshot.Messages) > 0
	}
	if len(snapshot.Messages) == 0 {
		return failed("no_content")
	}
	// Linked replies/reactions need their target context; never silently omit a
	// missing parent or turn a cyclic chain into a complete packet.
	for i := 0; i < len(snapshot.Messages); i++ { //nolint:intrange // Appended reply ancestors must also be visited; integer range would freeze the initial length.
		parent := snapshot.Messages[i].LinkedMessageID
		if parent == "" || seen[parent] {
			continue
		}
		m, e := getMessage(parent)
		if e != nil || m == nil || m.ID != parent {
			return failed("context_parent_missing")
		}
		if e = add(*m); e != nil {
			return failed("context_parent_identity_or_limit")
		}
	}
	links := map[string]string{}
	for _, m := range snapshot.Messages {
		links[m.ID] = m.LinkedMessageID
	}
	for id := range links {
		walk := map[string]bool{}
		for p := id; p != ""; p = links[p] {
			if walk[p] {
				return failed("context_parent_cycle")
			}
			walk[p] = true
		}
	}
	probe, e := page(snapshot.Head.Cursor, "after")
	if e != nil {
		return failed("context_unavailable_or_limit")
	}
	if !empty(probe) {
		return failed("context_changed")
	}
	after, e := getChat()
	if e != nil {
		return failed("context_unavailable_or_limit")
	}
	if !validChat(after) || !after.LastActivity.Equal(before.LastActivity) || after.Title != before.Title {
		return failed("context_changed")
	}
	sort.Slice(snapshot.Messages, func(i, j int) bool {
		a, b := snapshot.Messages[i], snapshot.Messages[j]
		if a.Timestamp.Equal(b.Timestamp) {
			return a.ID < b.ID
		}
		return a.Timestamp.Before(b.Timestamp)
	})
	snapshot.Stamp, e = contextDigest(struct {
		Account, Chat, Title, Scope string
		Lower                       *ContextAnchor
		Head                        ContextAnchor
		Messages                    []Message
	}{accountID, chatID, before.Title, snapshot.Scope, lower, snapshot.Head, snapshot.Messages})
	if e != nil {
		return snapshot, e
	}
	snapshot.Complete = true
	return snapshot, nil
}
