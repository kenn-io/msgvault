package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

const (
	LoopStateOpen    = "open"
	LoopStateDone    = "done"
	LoopStateSnoozed = "snoozed"

	// loopFirstMessages is how many of a conversation's newest messages come
	// with it. Most turns settle inside them; the walk pages back only when
	// they don't.
	loopFirstMessages = 4
	// loopChunkConversations is how many conversations load at once.
	loopChunkConversations = 200
	// loopPageMessages is the size of each page read past the first messages.
	loopPageMessages = 20
)

// LoopQuery selects the conversations a loop request reads.
type LoopQuery struct {
	PersonID       int64
	ConversationID int64
}

// LoopParticipant is one side of a message with the identity fields a loop
// shows.
type LoopParticipant struct {
	ParticipantID int64
	PersonID      int64
	DisplayName   string
	Address       string
	IsOwner       bool
}

type LoopRecipient struct {
	LoopParticipant

	Type string
}

// LoopMessage is one live, non-draft email or chat message with its
// owner-relative direction.
type LoopMessage struct {
	ID             int64
	ConversationID int64
	OccurredAt     time.Time
	Direction      ActivityDirection
	Sender         LoopParticipant
	Recipients     []LoopRecipient
	ListID         string
	Subject        string
	Snippet        string
	// HasFileAttachment is true only when the archive confirms a non-inline file.
	HasFileAttachment bool
}

// LoopConversation is an email thread or direct chat with its newest messages.
type LoopConversation struct {
	ConversationID   int64
	SourceID         int64
	ConversationType string
	GroupChat        bool
	// Messages are the newest live messages, newest first.
	Messages []LoopMessage

	earlier iter.Seq2[LoopMessage, error]
}

// Walk yields the conversation's live messages newest first: Messages, then
// older pages read only as far as the caller asks.
func (c LoopConversation) Walk() iter.Seq2[LoopMessage, error] {
	return func(yield func(LoopMessage, error) bool) {
		for _, m := range c.Messages {
			if !yield(m, nil) {
				return
			}
		}
		if c.earlier == nil {
			return
		}
		for m, err := range c.earlier {
			if !yield(m, err) {
				return
			}
		}
	}
}

// LoopDecision is the owner's done or snooze mark on a loop's anchor message.
type LoopDecision struct {
	AnchorMessageID int64
	State           string
	SnoozedUntil    *time.Time
}

// LoopMark is a decision attached to one exact question.
type LoopMark struct {
	State        string
	SnoozedUntil *time.Time
}

// loopMessageFilterSQL keeps what the archive still holds. A message deleted
// at the source stays readable here, so it still anchors and settles loops.
const loopMessageFilterSQL = `%[1]s.sent_at IS NOT NULL AND %[1]s.deleted_at IS NULL
	AND COALESCE(%[1]s.message_type, '') NOT IN ('calendar_event', 'meeting_transcript', 'synctech_sms_call')
	AND COALESCE(%[1]s.is_sent, TRUE)
	AND NOT (%[1]s.draft_authored OR %[2]s)`

func loopLiveMessageSQL(m, src string) string {
	return fmt.Sprintf(loopMessageFilterSQL, m, draftEvidenceSQL(m, src))
}

// loopTimeKey orders instants chronologically on SQLite, whose timestamp
// columns may contain strings with different UTC offsets. PostgreSQL stores
// these values as timestamptz and already compares them by instant.
func (s *Store) loopTimeKey(expression string) string {
	if s.IsPostgreSQL() {
		return expression
	}
	return sqliteutil.TimestampKeyFunction + "(" + expression + ")"
}

// LoopConversationsContext yields conversations oldest-activity first. It
// retains the small head rows, then hydrates messages in bounded chunks. Each
// conversation walks messages newest first on demand.
func (s *Store) LoopConversationsContext(ctx context.Context, q LoopQuery) iter.Seq2[LoopConversation, error] {
	return func(yield func(LoopConversation, error) bool) {
		owners, err := s.loopOwnerIdentities(ctx)
		var heads []LoopConversation
		if err == nil {
			heads, err = s.loopConversationHeads(ctx, q)
		}
		if err != nil {
			yield(LoopConversation{}, err)
			return
		}
		for start := 0; start < len(heads); start += loopChunkConversations {
			end := min(start+loopChunkConversations, len(heads))
			chunk, err := s.loopChunk(ctx, heads[start:end], owners)
			if err != nil {
				yield(LoopConversation{}, err)
				return
			}
			for _, conv := range chunk {
				if !yield(conv, nil) {
					return
				}
			}
		}
	}
}

// loopConversationHeads picks conversations in oldest-activity order. With a
// PersonID, it keeps conversations where one of that person's participants
// sent, received, or belongs.
func (s *Store) loopConversationHeads(ctx context.Context, q LoopQuery) ([]LoopConversation, error) {
	query, args := s.loopConversationQuery(q)
	query += ` ORDER BY MAX(` + s.loopTimeKey("m.sent_at") + `), m.conversation_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list loop conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var heads []LoopConversation
	for rows.Next() {
		var c LoopConversation
		if err := rows.Scan(&c.ConversationID, &c.SourceID, &c.ConversationType); err != nil {
			return nil, err
		}
		heads = append(heads, c)
	}
	return heads, rows.Err()
}

func (s *Store) loopConversationQuery(q LoopQuery) (string, []any) {
	query := `SELECT m.conversation_id, c.source_id, c.conversation_type
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE c.conversation_type IN ('email_thread', 'direct_chat')
			AND (m.list_id IS NULL OR m.list_id = '') AND ` + loopLiveMessageSQL("m", "src")
	var args []any
	if q.ConversationID > 0 {
		query += ` AND m.conversation_id = ?`
		args = append(args, q.ConversationID)
	}
	if q.PersonID > 0 {
		query += ` AND EXISTS (SELECT 1 FROM person_participants pp WHERE pp.person_id = ? AND (
			EXISTS (SELECT 1 FROM messages pm WHERE pm.conversation_id = c.id AND pm.sender_id = pp.participant_id)
			OR EXISTS (SELECT 1 FROM message_recipients pr JOIN messages pm ON pm.id = pr.message_id
				WHERE pm.conversation_id = c.id AND pr.participant_id = pp.participant_id)
			OR EXISTS (SELECT 1 FROM conversation_participants cp
				WHERE cp.conversation_id = c.id AND cp.participant_id = pp.participant_id)))`
		args = append(args, q.PersonID)
	}
	query += ` GROUP BY m.conversation_id, c.source_id, c.conversation_type`
	return query, args
}

// loopChunk attaches the newest messages to each conversation in the chunk,
// in one classification pass.
func (s *Store) loopChunk(ctx context.Context, chunk []LoopConversation, owners loopOwners) ([]LoopConversation, error) {
	ids := make([]int64, len(chunk))
	for i, c := range chunk {
		ids[i] = c.ConversationID
	}
	type windowRow struct{ messageID, conversationID, rank int64 }
	var window []windowRow
	err := queryInChunksContext(ctx, s.db, ids, nil,
		`SELECT id, conversation_id, rn FROM (
			SELECT m.id, m.conversation_id,
				ROW_NUMBER() OVER (PARTITION BY m.conversation_id ORDER BY `+s.loopTimeKey("m.sent_at")+` DESC, m.id DESC) AS rn
			FROM messages m JOIN sources src ON src.id = m.source_id
			WHERE m.conversation_id IN (%s) AND `+strings.ReplaceAll(loopLiveMessageSQL("m", "src"), "%", "%%")+`
		) w WHERE rn <= `+strconv.Itoa(loopFirstMessages),
		func(rows *loggedRows) error {
			var r windowRow
			if err := rows.Scan(&r.messageID, &r.conversationID, &r.rank); err != nil {
				return err
			}
			window = append(window, r)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("load loop window: %w", err)
	}
	slices.SortFunc(window, func(a, b windowRow) int {
		if a.conversationID != b.conversationID {
			return int(a.conversationID - b.conversationID)
		}
		return int(a.rank - b.rank)
	})
	messageIDs := make([]int64, len(window))
	lastID := map[int64]int64{}
	count := map[int64]int{}
	for i, r := range window {
		messageIDs[i] = r.messageID
		lastID[r.conversationID] = r.messageID
		count[r.conversationID]++
	}
	messages, err := s.loopMessages(ctx, messageIDs, owners)
	if err != nil {
		return nil, err
	}
	byConversation := map[int64][]LoopMessage{}
	for _, m := range messages {
		byConversation[m.ConversationID] = append(byConversation[m.ConversationID], m)
	}
	// Importer membership snapshots avoid hydrating message history for groups.
	var directIDs []int64
	for _, c := range chunk {
		if c.ConversationType == "direct_chat" {
			directIDs = append(directIDs, c.ConversationID)
		}
	}
	members := map[int64]map[string]bool{}
	err = queryInChunksContext(ctx, s.db, directIDs, nil,
		`SELECT cp.conversation_id, cp.participant_id, COALESCE(pp.person_id,0)
   FROM conversation_participants cp LEFT JOIN person_participants pp ON pp.participant_id = cp.participant_id
   WHERE cp.conversation_id IN (%s)`, func(rows *loggedRows) error {
			var conversationID, participantID, personID int64
			if err := rows.Scan(&conversationID, &participantID, &personID); err != nil {
				return err
			}
			if members[conversationID] == nil {
				members[conversationID] = map[string]bool{}
			}
			_, owner := owners.participants[participantID]
			if _, ok := owners.persons[personID]; ok {
				owner = true
			}
			key := "participant:" + strconv.FormatInt(participantID, 10)
			if personID != 0 {
				key = "person:" + strconv.FormatInt(personID, 10)
			}
			members[conversationID][key] = members[conversationID][key] || owner
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("load loop chat membership: %w", err)
	}
	for i := range chunk {
		external, ownerKnown := 0, false
		for _, owner := range members[chunk[i].ConversationID] {
			if owner {
				ownerKnown = true
			} else {
				external++
			}
		}
		limit := 2
		if ownerKnown {
			limit = 1
		}
		chunk[i].GroupChat = external > limit
	}
	out := chunk[:0:0]
	for _, c := range chunk {
		if count[c.ConversationID] == 0 {
			continue
		}
		c.Messages = byConversation[c.ConversationID]
		if count[c.ConversationID] == loopFirstMessages {
			c.earlier = s.loopEarlier(ctx, c.ConversationID, lastID[c.ConversationID], owners)
		}
		out = append(out, c)
	}
	return out, nil
}

// loopEarlier yields a conversation's live messages older than the one with
// ID after, newest first, a page at a time.
func (s *Store) loopEarlier(ctx context.Context, conversationID, after int64, owners loopOwners) iter.Seq2[LoopMessage, error] {
	return func(yield func(LoopMessage, error) bool) {
		for {
			ids, err := s.loopMessagePageIDs(ctx, conversationID, after, loopPageMessages)
			if err == nil && len(ids) == 0 {
				return
			}
			var page []LoopMessage
			if err == nil {
				page, err = s.loopMessages(ctx, ids, owners)
			}
			if err != nil {
				yield(LoopMessage{}, err)
				return
			}
			for _, m := range page {
				if !yield(m, nil) {
					return
				}
			}
			if len(ids) < loopPageMessages {
				return
			}
			after = ids[len(ids)-1]
		}
	}
}

func (s *Store) loopMessagePageIDs(ctx context.Context, conversationID, after int64, limit int) ([]int64, error) {
	query, args := s.loopMessagePageQuery(conversationID, after, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read loop messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// loopMessagePageQuery reads the page after ID after. It derives the cursor
// timestamp in SQL so values never round-trip through Go.
func (s *Store) loopMessagePageQuery(conversationID, after int64, limit int) (string, []any) {
	timestamp := s.loopTimeKey("m.sent_at")
	cursor := s.loopTimeKey("(SELECT sent_at FROM messages WHERE id = ?)")
	query := `SELECT m.id FROM messages m JOIN sources src ON src.id = m.source_id
		WHERE m.conversation_id = ? AND ` + loopLiveMessageSQL("m", "src") + `
			AND (` + timestamp + ` < ` + cursor + `
				OR (` + timestamp + ` = ` + cursor + ` AND m.id < ?))
		ORDER BY ` + timestamp + ` DESC, m.id DESC LIMIT ?`
	return query, []any{conversationID, after, after, after, limit}
}

// loopOwners is who counts as you: every participant that holds an address of
// any account, and the people those participants belong to.
type loopOwners struct {
	participants map[int64]struct{}
	persons      map[int64]struct{}
}

// loopOwnerIdentities loads the owners once per request. A participant that
// matches any account address is you across the whole archive, whether or not
// a person links it to your other addresses, so mail between your own
// accounts is never a loop. The match starts from the few account addresses,
// so the lower-case indexes on participants and identifiers serve it.
func (s *Store) loopOwnerIdentities(ctx context.Context) (loopOwners, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id FROM account_identities ai
		JOIN participants p ON `+ownerEmailMatch("p.email_address", "ai.address")+`
		UNION ALL
		SELECT pi.participant_id FROM account_identities ai
		JOIN participant_identifiers pi ON `+ownerIdentifierMatch("pi.identifier_type", "pi.identifier_value", "ai.address", identifierWithoutPrimaryEmail))
	if err != nil {
		return loopOwners{}, fmt.Errorf("load owner identities: %w", err)
	}
	owners := loopOwners{participants: map[int64]struct{}{}, persons: map[int64]struct{}{}}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return loopOwners{}, err
		}
		owners.participants[id] = struct{}{}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return loopOwners{}, err
	}
	err = queryInChunksContext(ctx, s.db, slices.Collect(maps.Keys(owners.participants)), nil,
		`SELECT person_id FROM person_participants WHERE participant_id IN (%s)`,
		func(rows *loggedRows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			owners.persons[id] = struct{}{}
			return nil
		})
	if err != nil {
		return loopOwners{}, fmt.Errorf("load owner persons: %w", err)
	}
	return owners, nil
}

// loopMessages loads and classifies messages, keeping the order of ids and
// leaving out those the activity rules do not count.
//
// ClassifyActivityCandidate sees you only through an account address of the
// message's own source. Here any participant that holds an address of any of
// your accounts, and any person who holds one, is you. A question sent to an
// alias you linked in the Directory reads as inbound, a reply from one reads
// as outbound, and mail between two of your accounts reads as yours on both
// ends. In a direct chat, a message that is not yours is theirs.
func (s *Store) loopMessages(ctx context.Context, ids []int64, owners loopOwners) ([]LoopMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	candidates, err := s.LoadActivityCandidatesByIDContext(ctx, ids)
	if err != nil {
		return nil, err
	}
	details, err := s.loopMessageDetails(ctx, ids)
	if err != nil {
		return nil, err
	}
	participants, err := s.loopParticipants(ctx, candidates)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]LoopMessage, len(candidates))
	for _, c := range candidates {
		// Eligibility is the activity feed's rule, which drops mail deleted at
		// the source. The ids came from the loop selection instead.
		if c.ConversationID == nil {
			continue
		}
		m := details[c.MessageID]
		m.ID, m.ConversationID = c.MessageID, *c.ConversationID
		m.Sender.ParticipantID = resolvedLoopSenderID(m.Sender.ParticipantID, c.Counterparts)
		m.Direction = ClassifyActivityCandidate(c, DefaultMaxDirectActivityCounterparts).Direction
		for _, at := range []*time.Time{c.SentAt, c.ReceivedAt, c.InternalDate} {
			if activityUsableTime(at) {
				m.OccurredAt = *at
				break
			}
		}
		ownerLinked := activityOwnerLinked(c.Counterparts)
		recipientIsOwner := false
		for _, cp := range c.Counterparts {
			p := participants[cp.ParticipantID]
			p.ParticipantID = cp.ParticipantID
			if cp.PersonID != nil {
				p.PersonID = *cp.PersonID
			}
			_, ownerParticipant := owners.participants[cp.ParticipantID]
			_, ownerPerson := owners.persons[p.PersonID]
			p.IsOwner = ownerLinked(cp) || ownerParticipant || ownerPerson
			switch {
			case cp.RecipientType == meetingSenderRole && cp.ParticipantID == m.Sender.ParticipantID:
				m.Sender = p
			case cp.RecipientType != meetingSenderRole:
				recipientIsOwner = recipientIsOwner || p.IsOwner
				m.Recipients = append(m.Recipients, LoopRecipient{LoopParticipant: p, Type: cp.RecipientType})
			}
		}
		switch {
		case m.Sender.IsOwner:
			m.Direction = DirectionOutbound
		case recipientIsOwner && m.Direction == DirectionObserved:
			m.Direction = DirectionInbound
		case c.ConversationType == "direct_chat" && m.Direction == DirectionObserved:
			// A direct chat holds you and one other side, and your own
			// messages are outbound already. Importers that don't know your
			// handle, such as iMessage without --me, leave no owner address
			// to match.
			m.Direction = DirectionInbound
		}
		byID[m.ID] = m
	}
	out := make([]LoopMessage, 0, len(byID))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// resolvedLoopSenderID uses a lone From snapshot when an older or partially
// repaired email has no messages.sender_id. Multiple distinct From
// participants remain ambiguous and do not name a sender.
func resolvedLoopSenderID(senderID int64, counterparts []ActivityCounterpart) int64 {
	if senderID != 0 {
		return senderID
	}
	var fromID int64
	for _, counterpart := range counterparts {
		if counterpart.RecipientType != meetingSenderRole {
			continue
		}
		if fromID != 0 && fromID != counterpart.ParticipantID {
			return 0
		}
		fromID = counterpart.ParticipantID
	}
	return fromID
}

func (s *Store) loopMessageDetails(ctx context.Context, messageIDs []int64) (map[int64]LoopMessage, error) {
	out := make(map[int64]LoopMessage, len(messageIDs))
	err := queryInChunksContext(ctx, s.db, messageIDs, nil,
		`SELECT m.id, COALESCE(m.sender_id, 0), COALESCE(m.subject, ''), COALESCE(m.snippet, ''), COALESCE(m.list_id, ''),
			EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id
				AND a.attachment_role = 'standalone')
		FROM messages m WHERE m.id IN (%s)`,
		func(rows *loggedRows) error {
			var id int64
			var m LoopMessage
			if err := rows.Scan(&id, &m.Sender.ParticipantID, &m.Subject, &m.Snippet, &m.ListID,
				&m.HasFileAttachment); err != nil {
				return err
			}
			out[id] = m
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("load loop message details: %w", err)
	}
	return out, nil
}

func (s *Store) loopParticipants(ctx context.Context, candidates []ActivityCandidate) (map[int64]LoopParticipant, error) {
	seen := map[int64]struct{}{}
	var ids []int64
	for _, c := range candidates {
		for _, cp := range c.Counterparts {
			if _, ok := seen[cp.ParticipantID]; !ok {
				seen[cp.ParticipantID] = struct{}{}
				ids = append(ids, cp.ParticipantID)
			}
		}
	}
	out := make(map[int64]LoopParticipant, len(ids))
	err := queryInChunksContext(ctx, s.db, ids, nil,
		`SELECT id, COALESCE(display_name, ''), COALESCE(NULLIF(email_address, ''), phone_number, '')
		FROM participants WHERE id IN (%s)`,
		func(rows *loggedRows) error {
			var p LoopParticipant
			if err := rows.Scan(&p.ParticipantID, &p.DisplayName, &p.Address); err != nil {
				return err
			}
			out[p.ParticipantID] = p
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("load loop participants: %w", err)
	}
	return out, nil
}

// LoopOriginalEvidenceContext reads the automation and file-attachment
// evidence of an email's stored original with the parser sync uses. A message
// with no readable original has no evidence. unreadable is true when
// decompression or MIME parsing fails, so it says nothing about the sender.
func (s *Store) LoopOriginalEvidenceContext(ctx context.Context, messageID int64) (signals mime.AutomationSignals, hasFileAttachment bool, unreadable bool, err error) {
	raw, err := s.GetMessageRawContext(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrInvalidMessageRaw) {
		return mime.AutomationSignals{}, false, errors.Is(err, ErrInvalidMessageRaw), nil
	}
	if err != nil {
		return mime.AutomationSignals{}, false, false, err
	}
	msg, err := mime.ParseWithRecovery(raw, "")
	if err != nil {
		return mime.AutomationSignals{}, false, true, nil //nolint:nilerr // A parse failure is a result the caller counts, not a read failure.
	}
	hasFileAttachment = slices.ContainsFunc(msg.Attachments, func(a mime.Attachment) bool { return !a.IsInline })
	return msg.AutomationSignals(), hasFileAttachment, false, nil
}

// LoopMarksContext loads exact decisions, including expired snoozes.
func (s *Store) LoopMarksContext(ctx context.Context) (map[int64]LoopMark, error) {
	marks := map[int64]LoopMark{}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id,state,snoozed_until FROM loop_decisions`)
	if err != nil {
		return nil, fmt.Errorf("load loop decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var mark LoopMark
		var until nullableTimestamp
		if err := rows.Scan(&id, &mark.State, &until); err != nil {
			return nil, err
		}
		if until.Valid {
			mark.SnoozedUntil = &until.Time
		}
		marks[id] = mark
	}
	return marks, rows.Err()
}

func (s *Store) MarkLoopDoneContext(ctx context.Context, anchorID int64, questions []LoopMessage) (LoopDecision, error) {
	return s.putLoopDecision(ctx, LoopDecision{AnchorMessageID: anchorID, State: LoopStateDone}, questions)
}

func (s *Store) SnoozeLoopContext(ctx context.Context, anchorID int64, until time.Time, questions []LoopMessage) (LoopDecision, error) {
	until = until.UTC()
	return s.putLoopDecision(ctx, LoopDecision{AnchorMessageID: anchorID, State: LoopStateSnoozed, SnoozedUntil: &until}, questions)
}

// ClearLoopDecisionContext removes the done or snooze decision on one exact question.
func (s *Store) ClearLoopDecisionContext(ctx context.Context, messageID int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM loop_decisions WHERE message_id = ?`, messageID)
	if err != nil {
		return false, fmt.Errorf("clear loop decision: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count cleared loop decisions: %w", err)
	}
	return count > 0, nil
}

func (s *Store) putLoopDecision(ctx context.Context, d LoopDecision, questions []LoopMessage) (LoopDecision, error) {
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		allDone := len(questions) > 0
		for _, question := range questions {
			var until any
			if d.SnoozedUntil != nil {
				until = s.dialect.TimestampParam(*d.SnoozedUntil)
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO loop_decisions(message_id,state,snoozed_until) VALUES (?,?,?)
    ON CONFLICT (message_id) DO UPDATE SET
     state = CASE WHEN loop_decisions.state = 'done' THEN 'done' ELSE excluded.state END,
     snoozed_until = CASE WHEN loop_decisions.state = 'done' OR excluded.state = 'done' THEN NULL ELSE excluded.snoozed_until END`,
				question.ID, d.State, until)
			if err != nil {
				return err
			}
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM loop_decisions WHERE message_id = ?`, question.ID).Scan(&state); err != nil {
				return err
			}
			allDone = allDone && state == LoopStateDone
		}
		if allDone {
			d.State, d.SnoozedUntil = LoopStateDone, nil
		}
		return nil
	})
	if err != nil {
		return LoopDecision{}, fmt.Errorf("store loop decision: %w", err)
	}
	return d, nil
}

// copyLoopDecisionTx copies a decision to a confirmed duplicate survivor.
// Hidden messages retain their own decisions so undo can restore them.
func (s *Store) copyLoopDecisionTx(ctx context.Context, tx *loggedTx, from, into int64) error {
	if from == into {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO loop_decisions(message_id,state,snoozed_until)
  SELECT ?,state,snoozed_until FROM loop_decisions WHERE message_id = ?
  ON CONFLICT (message_id) DO UPDATE SET
   state = CASE WHEN loop_decisions.state = 'done' OR excluded.state = 'done' THEN 'done' ELSE 'snoozed' END,
   snoozed_until = CASE WHEN loop_decisions.state = 'done' OR excluded.state = 'done' THEN NULL
    WHEN loop_decisions.snoozed_until > excluded.snoozed_until THEN loop_decisions.snoozed_until ELSE excluded.snoozed_until END`, into, from)
	return err
}

// LoopMessageEvidence retains classification facts, never message text.
type LoopMessageEvidence struct {
	Empty           bool `json:"empty"`
	Acknowledgement bool `json:"acknowledgement"`
	Asks            bool `json:"asks"`
	Automated       bool `json:"automated"`
	Attachment      bool `json:"attachment"`
	Unreadable      bool `json:"unreadable"`
}

// LoopMessageEvidenceContext returns cached facts only for the requested rules.
// A version mismatch replaces derived evidence so every reader can make progress.
func (s *Store) LoopMessageEvidenceContext(ctx context.Context, id int64, version int) (LoopMessageEvidence, string, bool, error) {
	var e LoopMessageEvidence
	var token string
	var facts sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT token, facts FROM loop_message_evidence
 WHERE message_id = ? AND rule_version = ?`, id, version).Scan(&token, &facts)
	if err == nil && facts.Valid {
		err = json.Unmarshal([]byte(facts.String), &e)
		return e, token, err == nil, err
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return e, "", false, err
	}
	token = rand.Text()
	_, err = s.db.ExecContext(ctx, `INSERT INTO loop_message_evidence(message_id,rule_version,token,facts)
 SELECT id, ?, ?, NULL FROM messages WHERE id = ?
 ON CONFLICT(message_id) DO UPDATE SET
  rule_version = excluded.rule_version,
  token = CASE WHEN loop_message_evidence.rule_version <> excluded.rule_version THEN excluded.token ELSE loop_message_evidence.token END,
  facts = CASE WHEN loop_message_evidence.rule_version <> excluded.rule_version THEN NULL ELSE loop_message_evidence.facts END`, version, token, id)
	if err != nil {
		return e, "", false, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT token, facts FROM loop_message_evidence WHERE message_id = ?`, id).Scan(&token, &facts)
	if errors.Is(err, sql.ErrNoRows) {
		return e, "", false, nil
	}
	if err != nil {
		return e, "", false, err
	}
	if facts.Valid {
		err = json.Unmarshal([]byte(facts.String), &e)
		return e, token, err == nil, err
	}
	return e, token, false, nil
}

// SaveLoopMessageEvidenceContext saves only while the computation's token is
// current. Body and original edits delete it, preventing a stale computation
// from restoring invalidated facts while unchanged imports retain their tokens.
func (s *Store) SaveLoopMessageEvidenceContext(ctx context.Context, id int64, version int, token string, e LoopMessageEvidence) error {
	facts, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE loop_message_evidence SET facts = ?
 WHERE message_id = ? AND rule_version = ? AND token = ?`, string(facts), id, version, token)
	return err
}
