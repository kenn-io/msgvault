package imessage

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (database/sql)
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const defaultPageSize = 500

// Client reads from macOS's iMessage chat.db and imports messages
// directly into the msgvault store.
type Client struct {
	db                *sql.DB
	afterDate         time.Time // only import messages after this date
	beforeDate        time.Time // only import messages before this date
	limit             int       // max total messages to import (0 = unlimited)
	useNanoseconds    bool      // whether chat.db uses nanosecond timestamps
	ownerHandle       string    // phone/email of device owner (from --me flag)
	logger            *slog.Logger
	pageSize          int
	attachmentsDir    string
	messagesRoot      string
	rebaseAttachments bool
	mediaPolicy       attachmentpolicy.Policy
	attachmentQuery   string
	participantCounts map[int64]int
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
	if err := c.mediaPolicy.Validate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if c.mediaPolicy.MaxBytes == 0 {
		c.mediaPolicy.MaxBytes = attachmentpolicy.DefaultChatMaxBytes
	}
	if c.attachmentsDir != "" && c.rebaseAttachments {
		info, err := os.Stat(c.messagesRoot)
		if err == nil && !info.IsDir() {
			err = errors.New("not a directory")
		}
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("messages root %q: %w", c.messagesRoot, err)
		}
	}
	if c.messagesRoot == "" && c.attachmentsDir != "" {
		home, err := os.UserHomeDir()
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		c.messagesRoot = filepath.Join(home, "Library", "Messages")
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
		"SELECT MAX(date) FROM message WHERE date > 0 AND date < ?",
		int64(math.MaxInt64),
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
	var args []any

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
	return c.ImportWithProgress(ctx, s, sourceID, nil)
}

// ImportWithProgress reports cumulative progress after each page.
func (c *Client) ImportWithProgress(ctx context.Context, s *store.Store, sourceID int64, onProgress func(ImportSummary)) (*ImportSummary, error) {
	summary := &ImportSummary{}
	if err := c.prepareAttachments(ctx); err != nil {
		return summary, fmt.Errorf("read attachment schema: %w", err)
	}

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
		if err != nil {
			return nil, fmt.Errorf("resolve default owner: %w", err)
		}
		ownerPID = pidMap["me@imessage.local"]
	}

	// Track resolved participants to avoid repeated DB calls
	phoneCache := map[string]int64{} // phone -> participantID
	emailCache := map[string]int64{} // email -> participantID
	convCache := map[string]int64{}  // chatGUID -> conversationID
	imported := 0
	lastROWID := int64(0)
	// Committed messages need their conversation counts however the run ends.
	finalize := func(err error) error {
		if statsErr := s.RecomputeConversationStats(sourceID); statsErr != nil {
			return errors.Join(err, fmt.Errorf("recompute stats: %w", statsErr))
		}
		return err
	}

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

		messageIDs := make([]string, 0, len(rows))
		for _, msg := range rows {
			messageIDs = append(messageIDs, strconv.FormatInt(msg.ROWID, 10))
		}
		stored, err := s.StoredMessagesContext(ctx, sourceID, "imessage_json", messageIDs)
		if err != nil {
			return summary, fmt.Errorf("load stored messages: %w", err)
		}
		storedIDs := make([]int64, 0, len(stored))
		for _, message := range stored {
			storedIDs = append(storedIDs, message.ID)
		}
		labels, err := s.MessageLabelIDsBatchContext(ctx, storedIDs)
		if err != nil {
			return summary, err
		}

		for i, msg := range rows {
			// Rows are ordered by message ID. Keep the last chat placement once.
			if i+1 < len(rows) && msg.ROWID == rows[i+1].ROWID {
				continue
			}
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			if c.limit > 0 && imported >= c.limit {
				break
			}

			lastROWID = msg.ROWID
			if err := c.importMessage(
				ctx, s, sourceID, &msg, stored, labels,
				imessageLabelID, smsLabelID,
				phoneCache, emailCache, convCache,
				ownerPID, summary,
			); err != nil {
				if fatal := fatalImportError(ctx, err); fatal != nil {
					return summary, finalize(fatal)
				}
				if errors.Is(err, ErrAttachmentImport) {
					// Keep scanning later messages and report the count once the scan ends.
					// importMessage has already persisted this message row.
					imported++
					// The first failure explains the run; the count covers the rest.
					level := slog.LevelDebug
					if summary.AttachmentFailures == 1 {
						level = slog.LevelWarn
					}
					c.logger.Log(ctx, level, "failed to import message attachments", "rowid", msg.ROWID, "error", err)
					continue
				}
				c.logger.Warn(
					"failed to import message",
					"rowid", msg.ROWID,
					"error", err,
				)
				summary.Skipped++
				continue
			}

			imported++
		}

		if onProgress != nil {
			onProgress(*summary)
		}
		if pageCount < c.pageSize {
			break
		}
	}

	var scanErr error
	if summary.AttachmentFailures > 0 {
		scanErr = fmt.Errorf("%d messages had attachment errors: %w", summary.AttachmentFailures, ErrAttachmentImport)
	}
	return summary, finalize(scanErr)
}

// fatalImportError returns the error that ends the run: cancellation or a failed archive write.
func fatalImportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errAttachmentStore) {
		return err
	}
	return nil
}

// fetchPage queries the next batch of messages from chat.db.
func (c *Client) fetchPage(
	ctx context.Context,
	afterROWID int64,
) ([]messageRow, int, error) {
	sqlQuery := `
		WITH page AS (
			SELECT m.ROWID FROM message m
			WHERE m.ROWID > ?`
	args := []any{afterROWID}

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
	sqlQuery += ` ORDER BY m.ROWID ASC LIMIT ?
		)
		SELECT
			m.ROWID, m.guid, m.text, m.attributedBody,
			m.date, m.is_from_me, m.service,
			m.cache_has_attachments,
			h.id,
			c.ROWID, c.guid, c.display_name, c.chat_identifier
		FROM page p
		JOIN message m ON m.ROWID = p.ROWID
		LEFT JOIN handle h ON h.ROWID = m.handle_id
		LEFT JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
		LEFT JOIN chat c ON c.ROWID = cmj.chat_id
		ORDER BY m.ROWID ASC, cmj.chat_id ASC`
	args = append(args, pageSize)

	dbRows, err := c.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = dbRows.Close() }()

	var result []messageRow
	pageCount := 0
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
		if len(result) == 0 || result[len(result)-1].ROWID != msg.ROWID {
			pageCount++
		}
		result = append(result, msg)
	}
	if err := dbRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate messages: %w", err)
	}

	return result, pageCount, nil
}

// importMessage processes a single chat.db message row and writes it
// to the msgvault store.
func (c *Client) importMessage(
	ctx context.Context,
	s *store.Store,
	sourceID int64,
	msg *messageRow,
	stored map[string]store.StoredMessage,
	storedLabels map[int64][]int64,
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
		if err != nil {
			return fmt.Errorf("resolve message sender: %w", err)
		}
		if pid > 0 {
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
	bodyDecodeFailed := false
	if msg.Text != nil {
		body = *msg.Text
	} else if len(msg.AttributedBody) > 0 {
		var decoded bool
		body, decoded = extractAttributedBodyTextWithStatus(msg.AttributedBody)
		bodyDecodeFailed = !decoded
	}

	msgDate := appleTimestampToTime(msg.Date)
	var sentAt sql.NullTime
	if !msgDate.IsZero() {
		sentAt = sql.NullTime{Time: msgDate, Valid: true}
	}
	sourceMessageID := strconv.FormatInt(msg.ROWID, 10)
	if bodyDecodeFailed {
		var existingMessageID int64
		err := s.DB().QueryRowContext(ctx, s.Rebind(`
			SELECT id FROM messages
			WHERE source_id = ? AND source_message_id = ?
		`), sourceID, sourceMessageID).Scan(&existingMessageID)
		if err == nil {
			body, err = s.GetMessageBodyText(existingMessageID)
			if err != nil {
				return fmt.Errorf("read archived body after attributed body decode failure: %w", err)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find existing message after attributed body decode failure: %w", err)
		}
		c.logger.Warn("failed to decode attributed body; preserving archived text")
	}
	message := &store.Message{
		SourceID:        sourceID,
		SourceMessageID: sourceMessageID,
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
		SizeEstimate:            int64(len(body)),
		HasAttachments:          msg.HasAttachments != 0,
		PreserveAttachmentStats: true,
	}
	raw, err := marshalMessageRaw(msg, body)
	if err != nil {
		return err
	}
	recipients, err := c.messageRecipients(s, msg, senderID, ownerPID, phoneCache, emailCache, summary)
	if err != nil {
		return fmt.Errorf("resolve message recipients: %w", err)
	}
	existing, exists := stored[sourceMessageID]
	labelID := imessageLabelID
	if msgType == "sms" {
		labelID = smsLabelID
	}
	labels := storedLabels[existing.ID]
	if exists && existing.Matches(message, raw) && slices.Contains(labels, labelID) {
		storedBody, err := s.GetMessageBodyTextContext(ctx, existing.ID)
		if err != nil {
			return fmt.Errorf("compare message body: %w", err)
		}
		matches, err := s.MessageRecipientsMatchContext(ctx, existing.ID, recipients)
		if err != nil {
			return fmt.Errorf("compare message recipients: %w", err)
		}
		if matches && storedBody == body {
			if err := s.IndexStoredMessageContext(ctx, existing.ID); err != nil {
				c.logger.Warn("failed to index message for search", "message_id", existing.ID, "error", err)
			}
			return c.finishMessageAttachments(ctx, s, existing.ID, msg, summary, true, true)
		}
	}
	if !slices.Contains(labels, labelID) {
		labels = append(labels, labelID)
	}
	msgID, err := s.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: message, BodyText: sql.NullString{String: body, Valid: true},
		RawMIME: raw, RawFormat: "imessage_json", Recipients: recipients,
		LabelIDs: labels,
	})
	if err != nil {
		return fmt.Errorf("persist message: %w", err)
	}
	if err := s.IndexStoredMessageContext(ctx, msgID); err != nil {
		c.logger.Warn("failed to index message for search", "message_id", msgID, "error", err)
	}
	if exists {
		summary.MessagesUpdated++
		if msgDate.IsZero() && (existing.SentAt.Valid || existing.InternalDate.Valid) {
			summary.DatesCleared++
		}
	} else {
		summary.MessagesAdded++
	}
	return c.finishMessageAttachments(ctx, s, msgID, msg, summary, exists, false)
}

func (c *Client) finishMessageAttachments(ctx context.Context, s *store.Store, msgID int64, msg *messageRow, summary *ImportSummary, existing, countUpdate bool) error {
	summary.MessagesImported++
	changed, err := c.importAttachments(ctx, s, msgID, msg, summary, existing)
	if countUpdate && changed {
		summary.MessagesUpdated++
	}
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		summary.AttachmentFailures++
		return fmt.Errorf("%w: %w", ErrAttachmentImport, err)
	}

	return nil
}

// messageRecipients builds the from/to snapshots for this import.
func (c *Client) messageRecipients(
	s *store.Store,
	msg *messageRow,
	senderID sql.NullInt64,
	ownerPID int64,
	phoneCache, emailCache map[string]int64,
	summary *ImportSummary,
) ([]store.RecipientSet, error) {
	sets := []store.RecipientSet{{Type: "from"}, {Type: "to"}}
	if senderID.Valid {
		sets[0].ParticipantIDs = []int64{senderID.Int64}
	}
	if msg.IsFromMe != 0 {
		pids, err := c.getChatParticipantIDs(s, msg, phoneCache, emailCache, summary)
		if err != nil {
			return nil, err
		}
		sets[1].ParticipantIDs = pids
	} else if ownerPID > 0 {
		sets[1].ParticipantIDs = []int64{ownerPID}
	}
	return sets, nil
}

// getChatParticipantIDs returns participant IDs for the chat members
// (excluding the owner). Used for "to" recipients on is_from_me messages.
func (c *Client) getChatParticipantIDs(
	s *store.Store,
	msg *messageRow,
	phoneCache map[string]int64,
	emailCache map[string]int64,
	summary *ImportSummary,
) ([]int64, error) {
	if msg.ChatROWID == nil {
		// No chat info; fall back to handle if available
		if msg.HandleID != nil {
			pid, err := c.resolveParticipant(
				s, *msg.HandleID,
				phoneCache, emailCache, summary,
			)
			if err != nil {
				return nil, err
			}
			if pid > 0 {
				return []int64{pid}, nil
			}
		}
		return nil, nil
	}

	rows, err := c.db.Query(`
		SELECT h.id
		FROM chat_handle_join chj
		JOIN handle h ON h.ROWID = chj.handle_id
		WHERE chj.chat_id = ?
	`, *msg.ChatROWID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var pids []int64
	for rows.Next() {
		var handleID string
		if err := rows.Scan(&handleID); err != nil {
			return nil, err
		}
		pid, err := c.resolveParticipant(
			s, handleID, phoneCache, emailCache, summary,
		)
		if err != nil {
			return nil, err
		}
		if pid > 0 {
			pids = append(pids, pid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pids, nil
}

// marshalMessageRaw serializes the source message for comparison and storage.
func marshalMessageRaw(msg *messageRow, body string) ([]byte, error) {
	raw := map[string]any{
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
	rawJSON, err := json.Marshal(raw, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("marshal raw JSON: %w", err)
	}
	return rawJSON, nil
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
	// Purge trusts the stored roster size, so refresh it for every chat the run sees.
	if msg.ChatROWID != nil {
		if _, err := c.chatParticipantCount(ctx, s, convID, *msg.ChatROWID); err != nil {
			return 0, false, err
		}
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
				s.Rebind("UPDATE conversations SET title = ? WHERE id = ?"),
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
	_ = s.DB().QueryRowContext(ctx, s.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ?
		  AND COALESCE(p.email_address, '') != 'me@imessage.local'
		  AND COALESCE(p.display_name, '') != 'Me'
	`), convID).Scan(&totalCount)

	// Get first few names for display. The literal '?' in COALESCE is
	// inside single quotes, so Rebind (which only converts ? outside
	// quoted strings) leaves it intact.
	rows, err := s.DB().QueryContext(ctx, s.Rebind(`
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
	`), convID)
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
	if err := rows.Err(); err != nil {
		c.logger.Warn("iterate chat title names", "error", err)
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
	if err := rows.Err(); err != nil {
		c.logger.Warn("iterate chat handles", "chat_id", chatROWID, "error", err)
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
