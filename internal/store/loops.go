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
	Questions      []LoopMessage
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
	GroupChat        bool
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

// LoopCutoff resolves exact question states and historical fallback boundaries.
type LoopCutoff struct {
	MessageID  int64
	OccurredAt time.Time
	OrderID    int64
	now        time.Time
	fallbacks  []loopFallback
	questions  map[int64]loopQuestionState
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
	// Importers can label group MMS as direct_chat. Read membership from all
	// message senders and recipients, including older pages.
	for i := range chunk {
		if chunk[i].ConversationType != "direct_chat" {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM messages WHERE conversation_id = ?`, chunk[i].ConversationID)
		if err != nil {
			return nil, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		members, err := s.loopMessages(ctx, ids, owners)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		ownerKnown := false
		for _, m := range members {
			people := []LoopParticipant{m.Sender}
			for _, r := range m.Recipients {
				people = append(people, r.LoopParticipant)
			}
			for _, p := range people {
				ownerKnown = ownerKnown || p.IsOwner
				if p.ParticipantID == 0 {
					continue
				}
				key := "participant:" + strconv.FormatInt(p.ParticipantID, 10)
				if p.PersonID != 0 {
					key = "person:" + strconv.FormatInt(p.PersonID, 10)
				}
				seen[key] = seen[key] || p.IsOwner
			}
		}
		limit := 2
		if ownerKnown {
			limit = 1
		}
		external := 0
		for _, owner := range seen {
			if !owner {
				external++
			}
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

// LoopMarksContext loads exact question states, including expired snoozes.
// A known open question must never inherit a different question's fallback.
func (s *Store) LoopMarksContext(ctx context.Context, now time.Time) (map[int64]LoopCutoff, error) {
	marks, err := loadLoopMarks(ctx, s.db)
	for id, mark := range marks {
		mark.now = now
		marks[id] = mark
	}
	return marks, err
}

type loopQuestionState struct {
	State string
	Until *time.Time
}

type loopFallback struct {
	Cutoff LoopCutoff
	Result loopQuestionState
}

func loadLoopMarks(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*loggedRows, error)
}) (map[int64]LoopCutoff, error) {
	marks := map[int64]LoopCutoff{}
	rows, err := q.QueryContext(ctx, `SELECT conversation_id, through_at, through_message_id, through_order_id, state, snoozed_until
		FROM loop_conversation_decisions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("load loop marks: %w", err)
	}
	for rows.Next() {
		var id int64
		var cutoff LoopCutoff
		var at, until nullableTimestamp
		var state string
		if err := rows.Scan(&id, &at, &cutoff.MessageID, &cutoff.OrderID, &state, &until); err != nil {
			_ = rows.Close()
			return nil, err
		}
		cutoff.OccurredAt = at.Time
		result := loopQuestionState{State: state}
		if until.Valid {
			result.Until = &until.Time
		}
		mark := marks[id]
		mark.MessageID, mark.OrderID, mark.OccurredAt = cutoff.MessageID, cutoff.OrderID, cutoff.OccurredAt
		mark.fallbacks = append(mark.fallbacks, loopFallback{Cutoff: cutoff, Result: result})
		marks[id] = mark
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, `SELECT conversation_id, order_id, state, snoozed_until FROM loop_question_states`)
	if err != nil {
		return nil, fmt.Errorf("load loop question states: %w", err)
	}
	for rows.Next() {
		var id, order int64
		var state loopQuestionState
		var until nullableTimestamp
		if err := rows.Scan(&id, &order, &state.State, &until); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if until.Valid {
			state.Until = &until.Time
		}
		mark := marks[id]
		if mark.questions == nil {
			mark.questions = map[int64]loopQuestionState{}
		}
		mark.questions[order] = state
		marks[id] = mark
	}
	return marks, errors.Join(rows.Err(), rows.Close())
}

func loopStateFor(mark LoopCutoff, ask LoopMessage) loopQuestionState {
	if state, ok := mark.questions[ask.OrderID]; ok {
		return state
	}
	state := loopQuestionState{State: "open"}
	for _, fallback := range mark.fallbacks {
		if loopBoundaryCovers(fallback.Cutoff, ask) {
			state = strongerLoopState(state, fallback.Result)
		}
	}
	return state
}

func strongerLoopState(a, b loopQuestionState) loopQuestionState {
	if a.State == LoopStateDone {
		return a
	}
	if b.State == LoopStateDone {
		return b
	}
	if b.State == LoopStateSnoozed && (a.State != LoopStateSnoozed || b.Until.After(*a.Until)) {
		return b
	}
	return a
}

func (s *Store) MarkLoopDoneContext(ctx context.Context, anchorID int64, through LoopMessage) (LoopDecision, error) {
	return s.putLoopDecision(ctx, LoopDecision{AnchorMessageID: anchorID, State: LoopStateDone}, through)
}

func (s *Store) SnoozeLoopContext(ctx context.Context, anchorID int64, until time.Time, through LoopMessage) (LoopDecision, error) {
	until = until.UTC()
	return s.putLoopDecision(ctx, LoopDecision{AnchorMessageID: anchorID, State: LoopStateSnoozed, SnoozedUntil: &until}, through)
}

func (s *Store) putLoopDecision(ctx context.Context, d LoopDecision, through LoopMessage) (LoopDecision, error) {
	err := s.withTxContext(ctx, func(tx *loggedTx) error { return s.putLoopDecisionTx(ctx, tx, &d, through) })
	if err != nil {
		return LoopDecision{}, fmt.Errorf("store loop decision: %w", err)
	}
	return d, nil
}

func (s *Store) putLoopDecisionTx(ctx context.Context, tx *loggedTx, d *LoopDecision, through LoopMessage) error {
	var conversationID int64
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM messages WHERE id = ?`, d.AnchorMessageID).Scan(&conversationID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM loop_message_order WHERE message_id = ?`, through.ID).Scan(&through.OrderID); err != nil {
		return err
	}
	marks, err := loadLoopMarks(ctx, tx)
	if err != nil {
		return err
	}
	mark := marks[conversationID]
	if d.State == LoopStateSnoozed {
		asks := through.Questions
		if len(asks) == 0 {
			asks = []LoopMessage{through}
		}
		allDone := true
		for _, ask := range asks {
			allDone = allDone && loopStateFor(mark, ask).State == LoopStateDone
		}
		if allDone {
			d.State, d.SnoozedUntil = LoopStateDone, nil
			return nil
		}
	}
	if err := s.snapshotLoopQuestionsTx(ctx, tx, conversationID, mark); err != nil {
		return err
	}
	cutoff := LoopCutoff{MessageID: through.ID, OrderID: through.OrderID, OccurredAt: through.OccurredAt}
	questions, err := loopKnownMessages(ctx, tx, conversationID)
	if err != nil {
		return err
	}
	for _, ask := range questions {
		if loopBoundaryCovers(cutoff, ask) {
			if err := s.writeLoopQuestionTx(ctx, tx, conversationID, ask.OrderID, loopQuestionState{State: d.State, Until: d.SnoozedUntil}); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM loop_conversation_decisions WHERE conversation_id = ?`, conversationID); err != nil {
		return err
	}
	var until any
	if d.SnoozedUntil != nil {
		until = s.dialect.TimestampParam(*d.SnoozedUntil)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO loop_conversation_decisions
		(conversation_id, state, snoozed_until, through_at, through_message_id, through_order_id) VALUES (?, ?, ?, ?, ?, ?)`,
		conversationID, d.State, until, s.operationTimestampParam(through.OccurredAt), through.ID, through.OrderID)
	return err
}

func loopKnownMessages(ctx context.Context, tx *loggedTx, conversationID int64) ([]LoopMessage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.id, lmo.id, m.sent_at FROM messages m
		JOIN loop_message_order lmo ON lmo.message_id = m.id WHERE m.conversation_id = ?`, conversationID)
	if err != nil {
		return nil, err
	}
	var messages []LoopMessage
	for rows.Next() {
		var m LoopMessage
		var at nullableTimestamp
		if err := rows.Scan(&m.ID, &m.OrderID, &at); err != nil {
			_ = rows.Close()
			return nil, err
		}
		m.OccurredAt = at.Time
		messages = append(messages, m)
	}
	return messages, errors.Join(rows.Err(), rows.Close())
}

func (s *Store) writeLoopQuestionTx(ctx context.Context, tx *loggedTx, conversationID, orderID int64, state loopQuestionState) error {
	var until any
	if state.Until != nil {
		until = s.dialect.TimestampParam(*state.Until)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO loop_question_states (conversation_id, order_id, state, snoozed_until)
		VALUES (?, ?, ?, ?) ON CONFLICT (conversation_id, order_id) DO UPDATE SET state = excluded.state, snoozed_until = excluded.snoozed_until`,
		conversationID, orderID, state.State, until)
	return err
}

func (s *Store) snapshotLoopQuestionsTx(ctx context.Context, tx *loggedTx, conversationID int64, mark LoopCutoff) error {
	messages, err := loopKnownMessages(ctx, tx, conversationID)
	if err != nil {
		return err
	}
	for _, ask := range messages {
		if err := s.writeLoopQuestionTx(ctx, tx, conversationID, ask.OrderID, loopStateFor(mark, ask)); err != nil {
			return err
		}
	}
	return nil
}

// LoopCutoffCovers uses exact state for known questions and fallbacks for late imports.
func LoopCutoffCovers(cutoff LoopCutoff, ask LoopMessage) bool {
	state := loopStateFor(cutoff, ask)
	return state.State == LoopStateDone || (state.State == LoopStateSnoozed && cutoff.now.Before(*state.Until))
}

func loopBoundaryCovers(cutoff LoopCutoff, ask LoopMessage) bool {
	return ask.OccurredAt.Before(cutoff.OccurredAt) ||
		(ask.OccurredAt.Equal(cutoff.OccurredAt) && ask.OrderID <= cutoff.OrderID)
}

// copyLoopDecisionTx follows a confirmed duplicate to whichever copy survives.
// Whole-conversation merges move fallbacks separately; other callers copy them.
func (s *Store) copyLoopDecisionTx(ctx context.Context, tx *loggedTx, from, into int64, copyFallbacks bool) error {
	if from == into {
		return nil
	}
	var fromConversation, intoConversation, fromOrder, intoOrder int64
	for _, row := range []struct {
		id                  int64
		conversation, order *int64
	}{{from, &fromConversation, &fromOrder}, {into, &intoConversation, &intoOrder}} {
		err := tx.QueryRowContext(ctx, `SELECT m.conversation_id, lmo.id FROM messages m JOIN loop_message_order lmo ON lmo.message_id = m.id WHERE m.id = ?`, row.id).Scan(row.conversation, row.order)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	marks, err := loadLoopMarks(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range []int64{fromConversation, intoConversation} {
		if err := s.snapshotLoopQuestionsTx(ctx, tx, id, marks[id]); err != nil {
			return err
		}
	}
	marks, err = loadLoopMarks(ctx, tx)
	if err != nil {
		return err
	}
	state := strongerLoopState(marks[fromConversation].questions[fromOrder], marks[intoConversation].questions[intoOrder])
	if err := s.writeLoopQuestionTx(ctx, tx, intoConversation, intoOrder, state); err != nil {
		return err
	}
	if copyFallbacks && fromConversation != intoConversation {
		if _, err := tx.ExecContext(ctx, `INSERT INTO loop_conversation_decisions
			(conversation_id, state, snoozed_until, through_at, through_message_id, through_order_id)
			SELECT ?, d.state, d.snoozed_until, d.through_at, d.through_message_id, d.through_order_id
			FROM loop_conversation_decisions d WHERE d.conversation_id = ?
			AND NOT EXISTS (SELECT 1 FROM loop_conversation_decisions target
				WHERE target.conversation_id = ? AND target.state = d.state
				AND target.through_order_id = d.through_order_id AND target.through_at = d.through_at
				AND (target.snoozed_until = d.snoozed_until OR (target.snoozed_until IS NULL AND d.snoozed_until IS NULL)))`,
			intoConversation, fromConversation, intoConversation); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE loop_conversation_decisions SET through_message_id = ? WHERE through_message_id = ?`, into, from)
	return err
}

// mergeLoopConversationsTx snapshots both sides before messages change conversations.
func (s *Store) mergeLoopConversationsTx(ctx context.Context, tx *loggedTx, from, into int64) error {
	if from == into {
		return nil
	}
	marks, err := loadLoopMarks(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range []int64{from, into} {
		if err := s.snapshotLoopQuestionsTx(ctx, tx, id, marks[id]); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE loop_question_states SET conversation_id = ? WHERE conversation_id = ?`, into, from); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE loop_conversation_decisions SET conversation_id = ? WHERE conversation_id = ?`, into, from)
	return err
}
