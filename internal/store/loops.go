package store

import (
	"context"
	"database/sql"
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

// LoopQuery selects the conversations a loop request reads. A zero Since
// reads conversations of any age.
type LoopQuery struct {
	Since          time.Time
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
	OrderID        int64
	ConversationID int64
	OccurredAt     time.Time
	Direction      ActivityDirection
	Sender         LoopParticipant
	Recipients     []LoopRecipient
	ListID         string
	Subject        string
	Snippet        string
	// HasFileAttachment is true for any attachment that is not inline content.
	HasFileAttachment bool
	// MayHaveFileAttachment means messages.has_attachments is true but the
	// archive has no file-attachment rows. Detection checks the stored MIME
	// lazily only when an otherwise empty or acknowledgement-only body needs it.
	MayHaveFileAttachment bool
}

// LoopConversation is an email thread or direct chat with its newest messages.
type LoopConversation struct {
	ConversationID   int64
	SourceID         int64
	ConversationType string
	// FirstAt is the time of the conversation's oldest live message. No loop
	// in it can start earlier.
	FirstAt time.Time
	// Messages are the newest live messages, newest first.
	Messages []LoopMessage

	earlier iter.Seq2[LoopMessage, error]
}

// HasEarlier is true when the conversation holds live messages older than
// Messages, which Walk reads on demand.
func (c LoopConversation) HasEarlier() bool { return c.earlier != nil }

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

// LoopCutoff is the covered question boundary for a conversation. MessageID
// and OrderID identify the latest question; CoveredOrderIDs preserve the
// timestamp/ID coverage snapshot if an older question's date later changes.
type LoopCutoff struct {
	MessageID       int64
	OccurredAt      time.Time
	OrderID         int64
	CoveredOrderIDs []int64
}

// loopMessageFilterSQL keeps what the archive still holds. A message deleted
// at the source stays readable here, so it still anchors and settles loops.
const loopMessageFilterSQL = `%[1]s.sent_at IS NOT NULL AND %[1]s.deleted_at IS NULL
	AND %[1]s.message_type NOT IN ('calendar_event', 'meeting_transcript', 'synctech_sms_call')
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

// loopTimeParam binds a bound instant using the same representation as
// loopTimeKey on SQLite.
func (s *Store) loopTimeParam(instant time.Time) any {
	if s.IsPostgreSQL() {
		return s.dialect.TimestampParam(instant)
	}
	return sqliteutil.TimestampKey(instant)
}

// LoopConversationsContext yields the email threads and direct chats with a
// live message from a person rather than a list since q.Since, the one with
// the oldest first message first. A caller that wants the oldest loops can
// stop once FirstAt passes the oldest loop it needs, since no later
// conversation can start a loop earlier. It loads loopChunkConversations conversations at a time,
// each with its newest messages classified for the owner, and reads nothing
// past a chunk the caller doesn't ask for.
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
			chunk, err := s.loopChunk(ctx, heads[start:min(start+loopChunkConversations, len(heads))], owners)
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

// loopConversationHeads picks the conversations to read, newest first. With a
// PersonID, it keeps conversations where one of the person's participants
// sent, received, or belongs.
func (s *Store) loopConversationHeads(ctx context.Context, q LoopQuery) ([]LoopConversation, error) {
	query, args := s.loopConversationQuery(q)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list loop conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var heads []LoopConversation
	for rows.Next() {
		var c LoopConversation
		var first nullableTimestamp
		if err := rows.Scan(&c.ConversationID, &c.SourceID, &c.ConversationType, &first); err != nil {
			return nil, err
		}
		c.FirstAt = first.Time
		heads = append(heads, c)
	}
	return heads, rows.Err()
}

func (s *Store) loopConversationQuery(q LoopQuery) (string, []any) {
	firstAt := `(SELECT MIN(x.sent_at) FROM messages x JOIN sources xs ON xs.id = x.source_id
				WHERE x.conversation_id = m.conversation_id AND ` + loopLiveMessageSQL("x", "xs") + `)`
	if !s.IsPostgreSQL() {
		firstAt = `(SELECT x.sent_at FROM messages x JOIN sources xs ON xs.id = x.source_id
				WHERE x.conversation_id = m.conversation_id AND ` + loopLiveMessageSQL("x", "xs") + `
				ORDER BY ` + s.loopTimeKey("x.sent_at") + `, x.id LIMIT 1)`
	}
	query := `SELECT m.conversation_id, c.source_id, c.conversation_type,
			` + firstAt + ` AS first_at
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE c.conversation_type IN ('email_thread', 'direct_chat')
			AND (m.list_id IS NULL OR m.list_id = '') AND ` + loopLiveMessageSQL("m", "src")
	var args []any
	if !q.Since.IsZero() {
		query += ` AND ` + s.loopTimeKey("m.sent_at") + ` >= ?`
		args = append(args, s.loopTimeParam(q.Since))
	}
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
	query += ` GROUP BY m.conversation_id, c.source_id, c.conversation_type
		ORDER BY ` + s.loopTimeKey("first_at") + `, m.conversation_id`
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
		`SELECT m.id, COALESCE(lmo.id, m.id), COALESCE(m.sender_id, 0), COALESCE(m.subject, ''), COALESCE(m.snippet, ''), COALESCE(m.list_id, ''),
			EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id
				AND a.attachment_role IN ('standalone', 'unknown')),
			COALESCE(m.has_attachments, FALSE), m.message_type
		FROM messages m LEFT JOIN loop_message_order lmo ON lmo.message_id = m.id WHERE m.id IN (%s)`,
		func(rows *loggedRows) error {
			var id int64
			var m LoopMessage
			var hasAttachments bool
			var messageType string
			if err := rows.Scan(&id, &m.OrderID, &m.Sender.ParticipantID, &m.Subject, &m.Snippet, &m.ListID,
				&m.HasFileAttachment, &hasAttachments, &messageType); err != nil {
				return err
			}
			m.MayHaveFileAttachment = messageType == MessageTypeEmail && hasAttachments && !m.HasFileAttachment
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

// LoopAutomationSignalsContext is kept for callers interested only in the
// original's automation evidence.
func (s *Store) LoopAutomationSignalsContext(ctx context.Context, messageID int64) (mime.AutomationSignals, bool, error) {
	signals, _, unreadable, err := s.LoopOriginalEvidenceContext(ctx, messageID)
	return signals, unreadable, err
}

// LoopMarksContext returns the decisions in force at now: done marks and
// snoozes that have not lapsed. Each decision keeps its question coverage
// attached to the conversation as message identities and timestamps change.
func (s *Store) LoopMarksContext(ctx context.Context, now time.Time) (map[int64]LoopCutoff, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.conversation_id, d.through_at, d.through_message_id, d.through_order_id FROM loop_conversation_decisions d
		WHERE d.state = ? OR d.snoozed_until > ? ORDER BY d.through_at, d.through_order_id`, LoopStateDone, s.dialect.TimestampParam(now))
	if err != nil {
		return nil, fmt.Errorf("load loop marks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	marks := map[int64]LoopCutoff{}
	for rows.Next() {
		var id, messageID, orderID int64
		var through nullableTimestamp
		if err := rows.Scan(&id, &through, &messageID, &orderID); err != nil {
			return nil, err
		}
		marks[id] = LoopCutoff{MessageID: messageID, OccurredAt: through.Time, OrderID: orderID}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	coveredRows, err := s.db.QueryContext(ctx, `SELECT c.conversation_id, c.order_id
		FROM loop_decision_covered_orders c JOIN loop_conversation_decisions d ON d.conversation_id = c.conversation_id
		WHERE d.state = ? OR d.snoozed_until > ? ORDER BY c.conversation_id, c.order_id`, LoopStateDone, s.dialect.TimestampParam(now))
	if err != nil {
		return nil, fmt.Errorf("load covered loop questions: %w", err)
	}
	defer func() { _ = coveredRows.Close() }()
	for coveredRows.Next() {
		var conversationID, orderID int64
		if err := coveredRows.Scan(&conversationID, &orderID); err != nil {
			return nil, err
		}
		cutoff := marks[conversationID]
		cutoff.CoveredOrderIDs = append(cutoff.CoveredOrderIDs, orderID)
		marks[conversationID] = cutoff
	}
	return marks, coveredRows.Err()
}

// MarkLoopDoneContext records that the owner finished the loop anchored at
// the message, through the question through. It replaces any earlier mark.
func (s *Store) MarkLoopDoneContext(ctx context.Context, anchorID int64, through LoopMessage) (LoopDecision, error) {
	return s.putLoopDecision(ctx, LoopDecision{AnchorMessageID: anchorID, State: LoopStateDone}, through)
}

// SnoozeLoopContext hides the loop anchored at the message until the instant,
// through the question through. A done mark that covers that question stays,
// and the result reports it.
func (s *Store) SnoozeLoopContext(ctx context.Context, anchorID int64, until time.Time, through LoopMessage) (LoopDecision, error) {
	until = until.UTC()
	return s.putLoopDecision(ctx, LoopDecision{AnchorMessageID: anchorID, State: LoopStateSnoozed, SnoozedUntil: &until}, through)
}

func (s *Store) putLoopDecision(ctx context.Context, d LoopDecision, through LoopMessage) (LoopDecision, error) {
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		return s.putLoopDecisionTx(ctx, tx, &d, through)
	})
	if err != nil {
		return LoopDecision{}, fmt.Errorf("store loop decision: %w", err)
	}
	return d, nil
}

func (s *Store) putLoopDecisionTx(ctx context.Context, tx *loggedTx, d *LoopDecision, through LoopMessage) error {
	var conversationID int64
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages WHERE id = ?`, d.AnchorMessageID).Scan(&conversationID); err != nil {
		return fmt.Errorf("find loop decision conversation: %w", err)
	}
	if through.OrderID == 0 {
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(
			`INSERT OR IGNORE INTO loop_message_order (message_id) VALUES (?)`), through.ID); err != nil {
			return fmt.Errorf("allocate loop message ordering token: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM loop_message_order WHERE message_id = ?`, through.ID).Scan(&through.OrderID); err != nil {
			return fmt.Errorf("read loop message ordering token: %w", err)
		}
	}
	throughOrderID := through.OrderID
	if d.State == LoopStateSnoozed {
		var covered nullableTimestamp
		var throughID, orderID int64
		if err := tx.QueryRowContext(ctx, `SELECT through_at, through_message_id, through_order_id FROM loop_conversation_decisions
			WHERE conversation_id = ? AND state = ?`, conversationID, LoopStateDone).
			Scan(&covered, &throughID, &orderID); err == nil {
			cutoff := LoopCutoff{MessageID: throughID, OccurredAt: covered.Time, OrderID: orderID}
			coverageRows, err := tx.QueryContext(ctx, `SELECT order_id FROM loop_decision_covered_orders WHERE conversation_id = ?`, conversationID)
			if err != nil {
				return fmt.Errorf("read done loop coverage: %w", err)
			}
			for coverageRows.Next() {
				var coveredOrderID int64
				if err := coverageRows.Scan(&coveredOrderID); err != nil {
					_ = coverageRows.Close()
					return fmt.Errorf("scan done loop coverage: %w", err)
				}
				cutoff.CoveredOrderIDs = append(cutoff.CoveredOrderIDs, coveredOrderID)
			}
			if err := coverageRows.Err(); err != nil {
				_ = coverageRows.Close()
				return fmt.Errorf("iterate done loop coverage: %w", err)
			}
			if err := coverageRows.Close(); err != nil {
				return fmt.Errorf("close done loop coverage: %w", err)
			}
			if LoopCutoffCovers(cutoff, through) {
				d.State, d.SnoozedUntil = LoopStateDone, nil
				return nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read done loop mark: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM loop_conversation_decisions WHERE conversation_id = ?`, conversationID); err != nil {
		return fmt.Errorf("replace loop decision: %w", err)
	}
	var until any
	if d.SnoozedUntil != nil {
		until = s.dialect.TimestampParam(*d.SnoozedUntil)
	}
	var stored sql.NullTime
	err := tx.QueryRowContext(ctx, `INSERT INTO loop_conversation_decisions
		(conversation_id, state, snoozed_until, through_at, through_message_id, through_order_id)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING state, snoozed_until`,
		conversationID, d.State, until, s.operationTimestampParam(through.OccurredAt), through.ID, throughOrderID).Scan(&d.State, &stored)
	d.SnoozedUntil = utcNullTimePointer(stored)
	if err != nil {
		return err
	}
	cutoffAt := s.loopTimeParam(through.OccurredAt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO loop_decision_covered_orders (conversation_id, order_id)
		SELECT ?, lmo.id FROM messages m JOIN loop_message_order lmo ON lmo.message_id = m.id
		WHERE m.conversation_id = ? AND (`+s.loopTimeKey("m.sent_at")+` < ?
			OR (`+s.loopTimeKey("m.sent_at")+` = ? AND lmo.id <= ?) OR m.id = ?)`,
		conversationID, conversationID, cutoffAt, cutoffAt, throughOrderID, through.ID); err != nil {
		return fmt.Errorf("store covered loop question order tokens: %w", err)
	}
	return nil
}

// LoopCutoffCovers reports whether a stored decision covers ask. The snapshot
// keeps questions covered if timestamps change; the original time/order cutoff
// still covers late-arriving historical messages and lets newer questions list.
func LoopCutoffCovers(cutoff LoopCutoff, ask LoopMessage) bool {
	if ask.OrderID > 0 && slices.Contains(cutoff.CoveredOrderIDs, ask.OrderID) {
		return true
	}
	// A merge can remap identity to an earlier-token survivor, but a later-token
	// survivor is a new question beyond the original boundary. GC and subset
	// export clear MessageID when that exact row is absent.
	sameIdentity := cutoff.MessageID != 0 && cutoff.MessageID == ask.ID &&
		cutoff.OrderID > 0 && ask.OrderID > 0 && ask.OrderID <= cutoff.OrderID
	byOrder := ask.OccurredAt.Before(cutoff.OccurredAt) ||
		(ask.OccurredAt.Equal(cutoff.OccurredAt) && ask.OrderID <= cutoff.OrderID)
	return sameIdentity || byOrder
}

// copyLoopDecisionTx brings a source conversation's decision into the
// survivor's conversation when a duplicate crosses conversation boundaries.
// The source mark stays with its original conversation while the destination
// resolves conflicts: done beats snooze, two dones keep the later question,
// and two snoozes keep the later deadline.
func copyLoopDecisionTx(ctx context.Context, tx *loggedTx, from, into int64) error {
	if from == into {
		return nil
	}
	var fromConversation, intoConversation int64
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages WHERE id = ?`, from).Scan(&fromConversation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("find source loop decision conversation: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages WHERE id = ?`, into).Scan(&intoConversation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("find survivor loop decision conversation: %w", err)
	}
	if fromConversation == intoConversation {
		if _, err := tx.ExecContext(ctx, `UPDATE loop_conversation_decisions SET through_message_id = ?
			WHERE conversation_id = ? AND through_message_id = ?`, into, fromConversation, from); err != nil {
			return fmt.Errorf("remap loop decision question from %d to %d: %w", from, into, err)
		}
		return nil
	}
	source, err := loadLoopDecision(ctx, tx, fromConversation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read loop decision from conversation %d: %w", fromConversation, err)
	}
	if source.ThroughMessageID == from {
		source.ThroughMessageID = into
	}
	target, err := loadLoopDecision(ctx, tx, intoConversation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read loop decision from survivor conversation %d: %w", intoConversation, err)
	}
	if errors.Is(err, sql.ErrNoRows) || loopDecisionReplaces(target, source) {
		var until any
		if source.SnoozedUntil != nil {
			until = source.SnoozedUntil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_conversation_decisions
			(conversation_id, state, snoozed_until, through_at, through_message_id, through_order_id)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (conversation_id) DO UPDATE SET state = excluded.state, snoozed_until = excluded.snoozed_until,
				through_at = excluded.through_at, through_message_id = excluded.through_message_id, through_order_id = excluded.through_order_id`,
			intoConversation, source.State, until, source.ThroughAt, source.ThroughMessageID, source.ThroughOrderID); err != nil {
			return fmt.Errorf("copy loop decision from conversation %d to %d: %w", fromConversation, intoConversation, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM loop_decision_covered_orders WHERE conversation_id = ?`, intoConversation); err != nil {
			return fmt.Errorf("replace survivor loop coverage: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_decision_covered_orders (conversation_id, order_id)
			SELECT ?, order_id FROM loop_decision_covered_orders WHERE conversation_id = ?`, intoConversation, fromConversation); err != nil {
			return fmt.Errorf("copy loop decision coverage from %d to %d: %w", fromConversation, intoConversation, err)
		}
	}
	return nil
}

type loopDecisionRecord struct {
	State            string
	SnoozedUntil     *time.Time
	ThroughAt        time.Time
	ThroughMessageID int64
	ThroughOrderID   int64
}

func loadLoopDecision(ctx context.Context, q contextRowQuerier, conversationID int64) (loopDecisionRecord, error) {
	var d loopDecisionRecord
	var until sql.NullTime
	var through nullableTimestamp
	err := q.QueryRowContext(ctx, `SELECT state, snoozed_until, through_at, through_message_id, through_order_id
		FROM loop_conversation_decisions WHERE conversation_id = ?`, conversationID).
		Scan(&d.State, &until, &through, &d.ThroughMessageID, &d.ThroughOrderID)
	if err != nil {
		return loopDecisionRecord{}, err
	}
	d.SnoozedUntil, d.ThroughAt = utcNullTimePointer(until), through.Time
	return d, nil
}

func loopDecisionReplaces(existing, candidate loopDecisionRecord) bool {
	if candidate.State == LoopStateDone {
		if existing.State != LoopStateDone {
			return true
		}
		return candidate.ThroughAt.After(existing.ThroughAt) ||
			(candidate.ThroughAt.Equal(existing.ThroughAt) && candidate.ThroughOrderID > existing.ThroughOrderID)
	}
	return existing.State == LoopStateSnoozed && candidate.SnoozedUntil.After(*existing.SnoozedUntil)
}
