package imessage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/wesm/msgvault/internal/mime"
	"github.com/wesm/msgvault/internal/store"
)

const defaultPageSize = 500

// Client reads from macOS's iMessage chat.db and imports messages
// directly into the msgvault store.
type Client struct {
	db             *sql.DB
	afterDate      time.Time // only import messages after this date
	beforeDate     time.Time // only import messages before this date
	limit          int       // max total messages to import (0 = unlimited)
	useNanoseconds bool      // whether chat.db uses nanosecond timestamps
	ownerHandle    string    // phone/email of device owner (from --me flag)
	logger         *slog.Logger
	pageSize       int
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithAfterDate filters to messages on or after this date.
func WithAfterDate(t time.Time) ClientOption {
	return func(c *Client) { c.afterDate = t }
}

// WithBeforeDate filters to messages before this date.
func WithBeforeDate(t time.Time) ClientOption {
	return func(c *Client) { c.beforeDate = t }
}

// WithLimit sets the maximum number of messages to import.
func WithLimit(n int) ClientOption {
	return func(c *Client) { c.limit = n }
}

// WithOwnerHandle sets the device owner's phone or email for
// recipient tracking. Used to create message_recipients rows.
func WithOwnerHandle(handle string) ClientOption {
	return func(c *Client) { c.ownerHandle = handle }
}

// WithImessageLogger sets the logger for the client.
func WithImessageLogger(l *slog.Logger) ClientOption {
	return func(c *Client) { c.logger = l }
}

// NewClient opens a read-only connection to an iMessage chat.db file.
func NewClient(dbPath string, opts ...ClientOption) (*Client, error) {
	dsn := fmt.Sprintf(
		"file:%s?mode=ro&_journal_mode=WAL&_busy_timeout=5000",
		dbPath,
	)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open chat.db: %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf(
			"connect to chat.db: %w (check Full Disk Access permissions)",
			err,
		)
	}

	c := &Client{
		db:       db,
		logger:   slog.Default(),
		pageSize: defaultPageSize,
	}

	for _, opt := range opts {
		opt(c)
	}

	if err := c.detectTimestampFormat(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("detect timestamp format: %w", err)
	}

	return c, nil
}

// Close closes the database connection.
func (c *Client) Close() error {
	return c.db.Close()
}

// detectTimestampFormat checks whether chat.db uses nanosecond timestamps
// (macOS High Sierra+) or second timestamps (older macOS).
func (c *Client) detectTimestampFormat() error {
	var maxDate sql.NullInt64
	err := c.db.QueryRow(
		"SELECT MAX(date) FROM message WHERE date > 0",
	).Scan(&maxDate)
	if err != nil {
		return fmt.Errorf("query max date: %w", err)
	}
	if maxDate.Valid {
		c.useNanoseconds = maxDate.Int64 > 1_000_000_000_000
	}
	return nil
}

// CountFilteredMessages returns the total count of messages matching
// the date filters, for progress reporting.
func (c *Client) CountFilteredMessages(ctx context.Context) int64 {
	sqlQuery := "SELECT COUNT(*) FROM message WHERE 1=1"
	var args []interface{}

	if !c.afterDate.IsZero() {
		appleTS := timeToAppleTimestamp(c.afterDate, c.useNanoseconds)
		sqlQuery += " AND date >= ?"
		args = append(args, appleTS)
	}
	if !c.beforeDate.IsZero() {
		appleTS := timeToAppleTimestamp(c.beforeDate, c.useNanoseconds)
		sqlQuery += " AND date < ?"
		args = append(args, appleTS)
	}

	var count int64
	if err := c.db.QueryRowContext(ctx, sqlQuery, args...).Scan(&count); err != nil {
		return 0
	}
	return count
}

// Import reads all matching messages from chat.db and writes them
// into the msgvault store using direct store methods.
func (c *Client) Import(
	ctx context.Context,
	s *store.Store,
	sourceID int64,
) (*ImportSummary, error) {
	summary := &ImportSummary{}

	// Pre-create iMessage and SMS labels
	imessageLabelID, err := s.EnsureLabel(
		sourceID, "iMessage", "iMessage", "user",
	)
	if err != nil {
		return nil, fmt.Errorf("ensure iMessage label: %w", err)
	}
	smsLabelID, err := s.EnsureLabel(sourceID, "SMS", "SMS", "user")
	if err != nil {
		return nil, fmt.Errorf("ensure SMS label: %w", err)
	}

	// Resolve owner participant for sender attribution on is_from_me messages.
	// When --me is provided, resolve that handle (phone/email).
	// Otherwise, create a generic "Me" participant so outbound messages
	// aren't shown as "Unknown".
	var ownerPID int64
	if c.ownerHandle != "" {
		pid, err := c.resolveParticipant(
			s, c.ownerHandle,
			map[string]int64{}, map[string]int64{},
			summary,
		)
		if err != nil {
			return nil, fmt.Errorf("resolve owner handle %q: %w",
				c.ownerHandle, err)
		}
		ownerPID = pid
	} else {
		// No --me flag: create a "Me" participant by email convention
		pidMap, err := s.EnsureParticipantsBatch(
			[]mime.Address{{Email: "me@imessage.local", Name: "Me"}},
		)
		if err == nil {
			if id, ok := pidMap["me@imessage.local"]; ok {
				ownerPID = id
			}
		}
	}

	// Track resolved participants to avoid repeated DB calls
	phoneCache := map[string]int64{} // phone -> participantID
	emailCache := map[string]int64{} // email -> participantID
	convCache := map[string]int64{}  // chatGUID -> conversationID
	imported := 0
	lastROWID := int64(0)

	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}

		if c.limit > 0 && imported >= c.limit {
			break
		}

		rows, pageCount, err := c.fetchPage(ctx, lastROWID)
		if err != nil {
			return summary, fmt.Errorf("fetch page: %w", err)
		}
		if pageCount == 0 {
			break
		}

		for _, msg := range rows {
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			if c.limit > 0 && imported >= c.limit {
				break
			}

			if err := c.importMessage(
				ctx, s, sourceID, &msg,
				imessageLabelID, smsLabelID,
				phoneCache, emailCache, convCache,
				ownerPID, summary,
			); err != nil {
				c.logger.Warn(
					"failed to import message",
					"rowid", msg.ROWID,
					"error", err,
				)
				summary.Skipped++
				continue
			}

			imported++
			lastROWID = msg.ROWID
		}

		if pageCount < c.pageSize {
			break
		}
	}

	if err := s.RecomputeConversationStats(sourceID); err != nil {
		return summary, fmt.Errorf("recompute stats: %w", err)
	}

	return summary, nil
}

// fetchPage queries the next batch of messages from chat.db.
func (c *Client) fetchPage(
	ctx context.Context,
	afterROWID int64,
) ([]messageRow, int, error) {
	sqlQuery := `
		SELECT
			m.ROWID, m.guid, m.text, m.attributedBody,
			m.date, m.is_from_me, m.service,
			m.cache_has_attachments,
			h.id,
			c.ROWID, c.guid, c.display_name, c.chat_identifier
		FROM message m
		LEFT JOIN handle h ON h.ROWID = m.handle_id
		LEFT JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
		LEFT JOIN chat c ON c.ROWID = cmj.chat_id
		WHERE m.ROWID > ?`
	args := []interface{}{afterROWID}

	if !c.afterDate.IsZero() {
		appleTS := timeToAppleTimestamp(c.afterDate, c.useNanoseconds)
		sqlQuery += " AND m.date >= ?"
		args = append(args, appleTS)
	}
	if !c.beforeDate.IsZero() {
		appleTS := timeToAppleTimestamp(c.beforeDate, c.useNanoseconds)
		sqlQuery += " AND m.date < ?"
		args = append(args, appleTS)
	}

	pageSize := c.pageSize
	if c.limit > 0 {
		remaining := c.limit
		if remaining < pageSize {
			pageSize = remaining
		}
	}
	sqlQuery += " ORDER BY m.ROWID ASC LIMIT ?"
	args = append(args, pageSize)

	dbRows, err := c.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = dbRows.Close() }()

	var result []messageRow
	for dbRows.Next() {
		var msg messageRow
		if err := dbRows.Scan(
			&msg.ROWID, &msg.GUID, &msg.Text, &msg.AttributedBody,
			&msg.Date, &msg.IsFromMe, &msg.Service,
			&msg.HasAttachments,
			&msg.HandleID,
			&msg.ChatROWID, &msg.ChatGUID, &msg.ChatDisplayName,
			&msg.ChatIdentifier,
		); err != nil {
			return nil, 0, fmt.Errorf("scan message: %w", err)
		}
		result = append(result, msg)
	}
	if err := dbRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate messages: %w", err)
	}

	return result, len(result), nil
}

// importMessage processes a single chat.db message row and writes it
// to the msgvault store.
func (c *Client) importMessage(
	ctx context.Context,
	s *store.Store,
	sourceID int64,
	msg *messageRow,
	imessageLabelID, smsLabelID int64,
	phoneCache map[string]int64,
	emailCache map[string]int64,
	convCache map[string]int64,
	ownerPID int64,
	summary *ImportSummary,
) error {
	// Determine conversation
	chatGUID := "no-chat-" + strconv.FormatInt(msg.ROWID, 10)
	if msg.ChatGUID != nil {
		chatGUID = *msg.ChatGUID
	}

	convID, isNewConv, err := c.ensureConversation(
		ctx, s, sourceID, msg, chatGUID, convCache,
		phoneCache, emailCache, summary,
	)
	if err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}
	if isNewConv {
		summary.ConversationsImported++
	}

	// Resolve sender
	var senderID sql.NullInt64
	if msg.IsFromMe != 0 {
		// is_from_me: sender is the device owner
		if ownerPID > 0 {
			senderID = sql.NullInt64{Int64: ownerPID, Valid: true}
		}
	} else if msg.HandleID != nil {
		pid, err := c.resolveParticipant(
			s, *msg.HandleID, phoneCache, emailCache, summary,
		)
		if err == nil && pid > 0 {
			senderID = sql.NullInt64{Int64: pid, Valid: true}
			_ = s.EnsureConversationParticipant(convID, pid, "member")
		}
	}

	// Determine message type from service field
	msgType := "imessage"
	if msg.Service != nil && strings.EqualFold(*msg.Service, "SMS") {
		msgType = "sms"
	}

	// Extract body text
	body := ""
	if msg.Text != nil {
		body = *msg.Text
	} else if len(msg.AttributedBody) > 0 {
		body = extractAttributedBodyText(msg.AttributedBody)
	}

	msgDate := appleTimestampToTime(msg.Date)
	var sentAt sql.NullTime
	if !msgDate.IsZero() {
		sentAt = sql.NullTime{Time: msgDate, Valid: true}
	}

	// Upsert the message
	msgID, err := s.UpsertMessage(&store.Message{
		SourceID:        sourceID,
		SourceMessageID: strconv.FormatInt(msg.ROWID, 10),
		ConversationID:  convID,
		MessageType:     msgType,
		SentAt:          sentAt,
		InternalDate:    sentAt,
		SenderID:        senderID,
		IsFromMe:        msg.IsFromMe != 0,
		Snippet: sql.NullString{
			String: snippet(body, 100),
			Valid:  body != "",
		},
		SizeEstimate:   int64(len(body)),
		HasAttachments: msg.HasAttachments != 0,
	})
	if err != nil {
		return fmt.Errorf("upsert message: %w", err)
	}

	// Store body text directly (no MIME)
	if body != "" {
		if err := s.UpsertMessageBody(
			msgID,
			sql.NullString{String: body, Valid: true},
			sql.NullString{},
		); err != nil {
			return fmt.Errorf("upsert body: %w", err)
		}
	}

	// Write message_recipients rows
	if err := c.writeMessageRecipients(
		s, msgID, msg, senderID, ownerPID,
		phoneCache, emailCache, summary,
	); err != nil {
		return fmt.Errorf("write message recipients: %w", err)
	}

	// Store raw data as JSON for completeness
	if err := c.writeMessageRaw(s, msgID, msg, body); err != nil {
		return fmt.Errorf("write message raw: %w", err)
	}

	// Label: iMessage or SMS
	labelID := imessageLabelID
	if msgType == "sms" {
		labelID = smsLabelID
	}
	if err := s.LinkMessageLabel(msgID, labelID); err != nil {
		return fmt.Errorf("link label: %w", err)
	}

	// Warn about attachments
	if msg.HasAttachments != 0 {
		c.logger.Debug(
			"message has attachments (extraction not yet implemented)",
			"rowid", msg.ROWID,
		)
	}

	summary.MessagesImported++
	return nil
}

// writeMessageRecipients creates from/to rows in message_recipients.
// For is_from_me: from=owner, to=other chat participants.
// For !is_from_me: from=sender handle, to=owner.
func (c *Client) writeMessageRecipients(
	s *store.Store,
	msgID int64,
	msg *messageRow,
	senderID sql.NullInt64,
	ownerPID int64,
	phoneCache map[string]int64,
	emailCache map[string]int64,
	summary *ImportSummary,
) error {
	if msg.IsFromMe != 0 {
		// Sender is the device owner
		if ownerPID > 0 {
			if err := s.ReplaceMessageRecipients(
				msgID, "from", []int64{ownerPID}, nil,
			); err != nil {
				return err
			}
		}
		// Recipients are the other chat participants
		toPIDs := c.getChatParticipantIDs(
			s, msg, phoneCache, emailCache, summary,
		)
		if len(toPIDs) > 0 {
			if err := s.ReplaceMessageRecipients(
				msgID, "to", toPIDs, nil,
			); err != nil {
				return err
			}
		}
	} else {
		// Sender is the external handle
		if senderID.Valid {
			if err := s.ReplaceMessageRecipients(
				msgID, "from",
				[]int64{senderID.Int64}, nil,
			); err != nil {
				return err
			}
		}
		// Recipient is the device owner
		if ownerPID > 0 {
			if err := s.ReplaceMessageRecipients(
				msgID, "to", []int64{ownerPID}, nil,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// getChatParticipantIDs returns participant IDs for the chat members
// (excluding the owner). Used for "to" recipients on is_from_me messages.
func (c *Client) getChatParticipantIDs(
	s *store.Store,
	msg *messageRow,
	phoneCache map[string]int64,
	emailCache map[string]int64,
	summary *ImportSummary,
) []int64 {
	if msg.ChatROWID == nil {
		// No chat info; fall back to handle if available
		if msg.HandleID != nil {
			pid, err := c.resolveParticipant(
				s, *msg.HandleID,
				phoneCache, emailCache, summary,
			)
			if err == nil && pid > 0 {
				return []int64{pid}
			}
		}
		return nil
	}

	rows, err := c.db.Query(`
		SELECT h.id
		FROM chat_handle_join chj
		JOIN handle h ON h.ROWID = chj.handle_id
		WHERE chj.chat_id = ?
	`, *msg.ChatROWID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var pids []int64
	for rows.Next() {
		var handleID string
		if err := rows.Scan(&handleID); err != nil {
			continue
		}
		pid, err := c.resolveParticipant(
			s, handleID, phoneCache, emailCache, summary,
		)
		if err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// writeMessageRaw serializes the message data as JSON and stores it.
func (c *Client) writeMessageRaw(
	s *store.Store,
	msgID int64,
	msg *messageRow,
	body string,
) error {
	raw := map[string]interface{}{
		"rowid":           msg.ROWID,
		"guid":            msg.GUID,
		"date":            msg.Date,
		"is_from_me":      msg.IsFromMe,
		"body":            body,
		"attributed_body": msg.AttributedBody, // base64-encoded by json.Marshal; nil omitted
	}
	if msg.Service != nil {
		raw["service"] = *msg.Service
	}
	if msg.HandleID != nil {
		raw["handle_id"] = *msg.HandleID
	}
	if msg.ChatGUID != nil {
		raw["chat_guid"] = *msg.ChatGUID
	}
	if msg.ChatDisplayName != nil {
		raw["chat_display_name"] = *msg.ChatDisplayName
	}
	if msg.ChatIdentifier != nil {
		raw["chat_identifier"] = *msg.ChatIdentifier
	}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal raw JSON: %w", err)
	}
	return s.UpsertMessageRawWithFormat(msgID, rawJSON, "imessage_json")
}

// ensureConversation gets or creates a conversation for the chat,
// resolving participants and setting the title.
func (c *Client) ensureConversation(
	ctx context.Context,
	s *store.Store,
	sourceID int64,
	msg *messageRow,
	chatGUID string,
	convCache map[string]int64,
	phoneCache map[string]int64,
	emailCache map[string]int64,
	summary *ImportSummary,
) (int64, bool, error) {
	if id, ok := convCache[chatGUID]; ok {
		return id, false, nil
	}

	// Determine conversation type from chat GUID format.
	// iMessage uses "any;+;" for group chats, "any;-;" for 1:1 direct.
	convType := "direct_chat"
	if chatGUID != "" && strings.Contains(chatGUID, ";+;") {
		convType = "group_chat"
	}

	// Build conversation title.
	title := ""
	if msg.ChatDisplayName != nil && *msg.ChatDisplayName != "" {
		title = *msg.ChatDisplayName
	} else if convType == "direct_chat" && msg.HandleID != nil {
		// For 1:1 chats, use the other party's phone/email
		phone, email, name := resolveHandle(*msg.HandleID)
		if name != "" {
			title = name
		} else if phone != "" {
			title = phone
		} else if email != "" {
			title = email
		}
	}
	// Group chats without a display_name: title will be set after
	// participants are resolved (below).

	convID, err := s.EnsureConversationWithType(
		sourceID, chatGUID, convType, title,
	)
	if err != nil {
		return 0, false, err
	}
	convCache[chatGUID] = convID

	// Link chat participants to the conversation
	if msg.ChatROWID != nil {
		c.linkChatParticipants(
			ctx, s, *msg.ChatROWID, convID,
			phoneCache, emailCache, summary,
		)
	}

	// For group chats without a title, build one from participants
	if title == "" && convType == "group_chat" {
		title = c.buildGroupTitle(ctx, s, convID)
		if title != "" {
			_, _ = s.DB().Exec(
				"UPDATE conversations SET title = ? WHERE id = ?",
				title, convID,
			)
		}
	}

	return convID, true, nil
}

// buildGroupTitle builds a group chat title from participant names/phones.
// Returns something like "Alice, +15551234567, Bob" or "Alice, Bob +3 more".
func (c *Client) buildGroupTitle(
	ctx context.Context, s *store.Store, convID int64,
) string {
	// Get total non-self participant count
	var totalCount int
	_ = s.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ?
		  AND COALESCE(p.email_address, '') != 'me@imessage.local'
		  AND COALESCE(p.display_name, '') != 'Me'
	`, convID).Scan(&totalCount)

	// Get first few names for display
	rows, err := s.DB().QueryContext(ctx, `
		SELECT COALESCE(
			NULLIF(p.display_name, ''),
			NULLIF(p.phone_number, ''),
			NULLIF(p.email_address, ''),
			'?'
		)
		FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ?
		  AND COALESCE(p.email_address, '') != 'me@imessage.local'
		  AND COALESCE(p.display_name, '') != 'Me'
		ORDER BY p.id
		LIMIT 3
	`, convID)
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	if totalCount > len(names) {
		return strings.Join(names, ", ") +
			fmt.Sprintf(" +%d more", totalCount-len(names))
	}
	return strings.Join(names, ", ")
}

// linkChatParticipants resolves all handles in a chat and links them
// as conversation participants.
func (c *Client) linkChatParticipants(
	ctx context.Context,
	s *store.Store,
	chatROWID, convID int64,
	phoneCache map[string]int64,
	emailCache map[string]int64,
	summary *ImportSummary,
) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT h.id
		FROM chat_handle_join chj
		JOIN handle h ON h.ROWID = chj.handle_id
		WHERE chj.chat_id = ?
	`, chatROWID)
	if err != nil {
		c.logger.Warn(
			"failed to get chat participants",
			"chat_id", chatROWID, "error", err,
		)
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var handleID string
		if err := rows.Scan(&handleID); err != nil {
			continue
		}
		pid, err := c.resolveParticipant(
			s, handleID, phoneCache, emailCache, summary,
		)
		if err != nil || pid == 0 {
			continue
		}
		_ = s.EnsureConversationParticipant(convID, pid, "member")
	}
}

// resolveParticipant resolves a handle ID to a participant ID in the
// store, creating the participant if needed.
func (c *Client) resolveParticipant(
	s *store.Store,
	handleID string,
	phoneCache map[string]int64,
	emailCache map[string]int64,
	summary *ImportSummary,
) (int64, error) {
	phone, email, _ := resolveHandle(handleID)

	if phone != "" {
		if pid, ok := phoneCache[phone]; ok {
			return pid, nil
		}
		// Pass empty displayName: chat.db has no contact names, only handles.
		// Leaving display_name NULL lets later imports (Gmail, Google Voice,
		// WhatsApp --contacts, or import-contacts) populate the real name.
		pid, err := s.EnsureParticipantByPhone(phone, "", "imessage")
		if err != nil {
			return 0, fmt.Errorf("ensure participant by phone %s: %w", phone, err)
		}
		phoneCache[phone] = pid
		summary.ParticipantsResolved++
		return pid, nil
	}

	if email != "" {
		if pid, ok := emailCache[email]; ok {
			return pid, nil
		}
		result, err := s.EnsureParticipantsBatch([]mime.Address{
			{Email: email},
		})
		if err != nil {
			return 0, fmt.Errorf("ensure participant by email %s: %w", email, err)
		}
		pid := result[email]
		emailCache[email] = pid
		summary.ParticipantsResolved++
		return pid, nil
	}

	return 0, nil
}
