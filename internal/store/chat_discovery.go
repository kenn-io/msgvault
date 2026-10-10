package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

// ErrInvalidChatDiscoveryQuery wraps every chat discovery input rejection.
var ErrInvalidChatDiscoveryQuery = errors.New("invalid chat discovery query")

// DefaultChatDiscoveryLimit is the result count when a query sets no limit.
const DefaultChatDiscoveryLimit = 20

// ChatDiscoveryQuery finds chats whose names or titles contain any whole
// query token. Limit 0 selects DefaultChatDiscoveryLimit; SourceID 0 searches
// every source.
type ChatDiscoveryQuery struct {
	Query    string
	Limit    int
	SourceID int64
}

// ChatDiscoveryResult is one ranked chat with the names that matched it.
// MessageID is the chat's newest visible message.
type ChatDiscoveryResult struct {
	ConversationID       int64    `json:"conversation_id"`
	MessageID            int64    `json:"message_id"`
	SourceConversationID string   `json:"source_conversation_id"`
	SourceID             int64    `json:"source_id"`
	SourceType           string   `json:"source_type"`
	SourceIdentifier     string   `json:"source_identifier"`
	SourceDisplayName    string   `json:"source_display_name"`
	Network              string   `json:"network"`
	Title                string   `json:"title"`
	ConversationType     string   `json:"conversation_type"`
	MatchedTokens        []string `json:"matched_tokens"`
	MatchedNames         []string `json:"matched_names"`
	EvidenceTruncated    bool     `json:"evidence_truncated"`
	MatchKind            string   `json:"match_kind"`
}

// ChatDiscoveryPage holds ranked results; HasMore reports matches beyond the
// limit.
type ChatDiscoveryPage struct {
	Results []ChatDiscoveryResult `json:"results"`
	HasMore bool                  `json:"has_more"`
}

// ValidateChatDiscoveryQuery shares the CLI, API and Store input contract.
func ValidateChatDiscoveryQuery(q ChatDiscoveryQuery) error {
	if !utf8.ValidString(q.Query) || len(q.Query) > 256 {
		return fmt.Errorf("query must be valid UTF-8 and at most 256 bytes: %w", ErrInvalidChatDiscoveryQuery)
	}
	tokens := uniqueChatTokens(q.Query)
	if len(tokens) == 0 || len(tokens) > 16 {
		return fmt.Errorf("query must contain between 1 and 16 unique name tokens: %w", ErrInvalidChatDiscoveryQuery)
	}
	if q.Limit < 0 || q.Limit > 100 {
		return fmt.Errorf("limit must be between 1 and 100: %w", ErrInvalidChatDiscoveryQuery)
	}
	if q.SourceID < 0 {
		return fmt.Errorf("source_id must be positive: %w", ErrInvalidChatDiscoveryQuery)
	}
	return nil
}

func uniqueChatTokens(value string) []string {
	tokens := []string{}
	for _, token := range directoryTokens(value) {
		if !slices.Contains(tokens, token) {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

// maxChatEvidence bounds the matching names returned for one chat.
const maxChatEvidence = 8

type rankedChat struct {
	result   ChatDiscoveryResult
	last     time.Time
	bits     uint16
	best     int
	bestName string
	names    map[string]struct{}
}

func (c *rankedChat) match(value string, query, exactQuery []string) {
	tokens := directoryTokens(value)
	matched := 0
	for index, token := range query {
		if slices.Contains(tokens, token) {
			c.bits |= 1 << index
			matched++
		}
	}
	if matched == 0 {
		return
	}
	c.names[value] = struct{}{}
	exact := slices.Equal(tokens, exactQuery)
	if c.bestName == "" || (exact && c.result.MatchKind != "exact") ||
		(exact == (c.result.MatchKind == "exact") && (matched > c.best || (matched == c.best && value < c.bestName))) {
		c.bestName = value
	}
	c.best = max(c.best, matched)
	if exact {
		c.result.MatchKind = "exact"
	}
}

// evidence returns the smallest matching names in sorted order, keeping the
// name that decided the rank when the list is truncated.
func (c *rankedChat) evidence() ([]string, bool) {
	names := slices.Sorted(maps.Keys(c.names))
	if len(names) <= maxChatEvidence {
		return names, false
	}
	names = names[:maxChatEvidence]
	if !slices.Contains(names, c.bestName) {
		names[maxChatEvidence-1] = c.bestName
	}
	return names, true
}

func compareRankedChats(a, b rankedChat) int {
	if a.result.MatchKind != b.result.MatchKind {
		if a.result.MatchKind == "exact" {
			return -1
		}
		return 1
	}
	if a.best != b.best {
		return b.best - a.best
	}
	if comparison := b.last.Compare(a.last); comparison != 0 {
		return comparison
	}
	if a.result.ConversationID < b.result.ConversationID {
		return -1
	}
	if a.result.ConversationID > b.result.ConversationID {
		return 1
	}
	return 0
}

// SearchChatsContext discovers archived chats without reading message bodies.
// Matching uses Go's shared Unicode normalization, rather than database LOWER
// functions with different semantics. Membership comes from the chat_members
// projection, so the scan covers chat metadata, not messages. The stream is
// not pre-limited: every candidate receives its complete score before bounded
// top-N admission.
func (s *Store) SearchChatsContext(ctx context.Context, q ChatDiscoveryQuery) (*ChatDiscoveryPage, error) {
	if err := ValidateChatDiscoveryQuery(q); err != nil {
		return nil, err
	}
	if err := s.refreshChatMembersContext(ctx); err != nil {
		return nil, err
	}
	if q.Limit == 0 {
		q.Limit = DefaultChatDiscoveryLimit
	}
	tokens := uniqueChatTokens(q.Query)
	exactTokens := directoryTokens(q.Query)
	top := make([]rankedChat, 0, q.Limit+2)
	statement := `WITH eligible AS (
		SELECT c.id, c.source_id, c.source_conversation_id, c.title,
			c.conversation_type, anchor.id AS message_id, anchor.sent_at AS last_at
		FROM conversations c
		JOIN messages anchor ON anchor.id = COALESCE((
			SELECT m.id FROM messages m WHERE m.conversation_id = c.id AND ` + LiveMessagesWhere("m", false) + `
			AND m.sent_at IS NOT NULL ORDER BY m.sent_at DESC, m.id DESC LIMIT 1
		), (
			SELECT m.id FROM messages m WHERE m.conversation_id = c.id AND ` + LiveMessagesWhere("m", false) + `
			ORDER BY m.sent_at DESC, m.id DESC LIMIT 1
		))
		WHERE c.conversation_type IN ` + chatConversationTypesSQL
	args := []any{}
	if q.SourceID != 0 {
		statement += ` AND c.source_id = ?`
		args = append(args, q.SourceID)
	}
	statement += `), raw_members AS (
		SELECT cp.conversation_id, cp.participant_id
		FROM conversation_participants cp JOIN eligible e ON e.id = cp.conversation_id
		UNION
		SELECT cm.conversation_id, cm.participant_id
		FROM chat_members cm JOIN eligible e ON e.id = cm.conversation_id
	), members AS (
		SELECT b.conversation_id, b.participant_id, e.source_id FROM raw_members b
		JOIN eligible e ON e.id = b.conversation_id
		WHERE NOT (` + senderOwnerFallback("b.participant_id", "e.source_id") + `)
		AND NOT EXISTS (
			SELECT 1 FROM account_identities ai
			JOIN sources source ON source.id = ai.source_id AND source.source_type = 'beeper'
			JOIN participant_identifiers pi ON pi.participant_id = b.participant_id AND pi.identifier_type = 'beeper'
			WHERE ai.source_id = e.source_id
			AND pi.identifier_value = ` + beeperidentity.FallbackSQL(s.dialect.DriverName(), "source.identifier", "ai.address") + `
		)
		AND NOT EXISTS (
			SELECT 1 FROM messages owner
			WHERE owner.sender_id = b.participant_id AND owner.source_id = e.source_id
			AND owner.source_is_from_me = TRUE
		)
	), names AS (
		SELECT id AS conversation_id, title AS value FROM eligible
		UNION ALL
		SELECT b.conversation_id, p.display_name FROM members b JOIN participants p ON p.id = b.participant_id
		UNION ALL
		SELECT b.conversation_id, p.email_address FROM members b JOIN participants p ON p.id = b.participant_id
		UNION ALL
		SELECT b.conversation_id, p.phone_number FROM members b JOIN participants p ON p.id = b.participant_id
		UNION ALL
		SELECT b.conversation_id, i.identifier_value FROM members b JOIN participant_identifiers i ON i.participant_id = b.participant_id
		UNION ALL
		SELECT b.conversation_id, o.original_value FROM members b
		JOIN participant_contact_observations o ON o.participant_id = b.participant_id
		WHERE o.active_until IS NULL AND o.superseded_at IS NULL
		AND (o.source_id IS NULL OR o.source_id = b.source_id)
		UNION ALL
		SELECT b.conversation_id, p.display_name FROM members b
		JOIN person_participants pp ON pp.participant_id = b.participant_id
		JOIN persons p ON p.id = pp.person_id
		UNION ALL
		SELECT b.conversation_id, n.formatted FROM members b
		JOIN person_participants pp ON pp.participant_id = b.participant_id
		JOIN person_names n ON n.person_id = pp.person_id
		WHERE n.active_until IS NULL AND n.superseded_at IS NULL
		UNION ALL
		SELECT b.conversation_id, n.original_value FROM members b
		JOIN person_participants pp ON pp.participant_id = b.participant_id
		JOIN person_names n ON n.person_id = pp.person_id
		WHERE n.active_until IS NULL AND n.superseded_at IS NULL
		UNION ALL
		SELECT b.conversation_id, COALESCE(n.honorific_prefixes, '') || ' ' || COALESCE(n.given_name, '') || ' ' ||
			COALESCE(n.additional_names, '') || ' ' || COALESCE(n.family_name, '') || ' ' ||
			COALESCE(n.secondary_surname, '') || ' ' || COALESCE(n.generation, '') || ' ' ||
			COALESCE(n.honorific_suffixes, '') FROM members b
		JOIN person_participants pp ON pp.participant_id = b.participant_id
		JOIN person_names n ON n.person_id = pp.person_id
		WHERE n.active_until IS NULL AND n.superseded_at IS NULL
		UNION ALL
		SELECT b.conversation_id, n.sort_as FROM members b
		JOIN person_participants pp ON pp.participant_id = b.participant_id
		JOIN person_names n ON n.person_id = pp.person_id
		WHERE n.active_until IS NULL AND n.superseded_at IS NULL
		UNION ALL
		SELECT b.conversation_id, cm.alias FROM members b
		JOIN chat_members cm ON cm.conversation_id = b.conversation_id AND cm.participant_id = b.participant_id
		WHERE cm.alias <> ''
	)
	SELECT e.id, e.message_id, e.source_id, COALESCE(e.source_conversation_id, ''),
		COALESCE(e.title, ''), e.conversation_type, e.last_at,
		s.source_type, s.identifier, COALESCE(s.display_name, ''),
		COALESCE(n.value, '')
	FROM eligible e JOIN sources s ON s.id = e.source_id
	JOIN names n ON n.conversation_id = e.id
	ORDER BY e.id`
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(statement), args...)
	if err != nil {
		return nil, fmt.Errorf("stream chat discovery metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var current *rankedChat
	admit := func() {
		if current == nil || current.bits == 0 {
			return
		}
		for i, token := range tokens {
			if current.bits&(1<<i) != 0 {
				current.result.MatchedTokens = append(current.result.MatchedTokens, token)
			}
		}
		current.result.MatchedNames, current.result.EvidenceTruncated = current.evidence()
		top = append(top, *current)
		slices.SortFunc(top, compareRankedChats)
		if len(top) > q.Limit+1 {
			top = top[:q.Limit+1]
		}
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var result ChatDiscoveryResult
		var last nullableTimestamp
		var value string
		if err := rows.Scan(&result.ConversationID, &result.MessageID, &result.SourceID, &result.SourceConversationID,
			&result.Title, &result.ConversationType, &last, &result.SourceType,
			&result.SourceIdentifier, &result.SourceDisplayName, &value); err != nil {
			return nil, fmt.Errorf("scan chat discovery metadata: %w", err)
		}
		if current == nil || current.result.ConversationID != result.ConversationID {
			admit()
			result.Network = chatNetworkLabel(result.SourceType, result.SourceDisplayName, result.SourceIdentifier)
			result.MatchKind = "partial"
			result.MatchedTokens = []string{}
			current = &rankedChat{result: result, last: last.Time, names: map[string]struct{}{}}
		}
		current.match(value, tokens, exactTokens)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read chat discovery metadata: %w", err)
	}
	admit()
	page := &ChatDiscoveryPage{Results: []ChatDiscoveryResult{}, HasMore: len(top) > q.Limit}
	for _, chat := range top[:min(len(top), q.Limit)] {
		page.Results = append(page.Results, chat.result)
	}
	return page, nil
}

func chatNetworkLabel(sourceType, displayName, sourceIdentifier string) string {
	if sourceType != "beeper" {
		return sourceType
	}
	if label, ok := strings.CutPrefix(displayName, "Beeper "); ok {
		label = strings.TrimSpace(label)
		if label != "" && label != strings.TrimSpace(sourceIdentifier) {
			return label
		}
	}
	return "unknown"
}
