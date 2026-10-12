package imessage

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (database/sql)
	"go.kenn.io/msgvault/internal/store"
)

const defaultPageSize = 500

// Client reads from macOS's iMessage chat.db and imports messages
// directly into the msgvault store.
type Client struct {
	notifyImports  bool
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

// WithNotifyImports explicitly requests notifications for this offline call.
// The daemon must separately enable import notifications.
func WithNotifyImports(notify bool) ClientOption { return func(c *Client) { c.notifyImports = notify } }

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
func (c *Client) Import(ctx context.Context, s *store.Store, sourceID int64) (*ImportSummary, error) {
	// Offline calls own a fresh run and never inherit another ingestion mode.
	s = s.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	if err := s.ValidateNativeImportChoice(ctx, "apple_messages", c.notifyImports); err != nil {
		return nil, err
	}
	syncID, err := s.StartSync(sourceID, "apple_messages_import")
	if err != nil {
		return nil, fmt.Errorf("start Apple Messages import: %w", err)
	}
	scoped := s.ScopedToSync(sourceID, syncID)
	run, err := scoped.BeginNativeImportAttempt(ctx, sourceID, "apple_messages", c.notifyImports, syncID, 0)
	if err != nil {
		_ = s.FailSync(syncID, err.Error())
		return nil, fmt.Errorf("begin Apple Messages import: %w", err)
	}
	scoped, err = scoped.WithImportRun(ctx, run.ID)
	if err != nil {
		_ = s.FailSync(syncID, err.Error())
		return &ImportSummary{ImportRunID: run.ID}, err
	}
	summary, err := c.importMessages(ctx, scoped, sourceID)
	if summary == nil {
		summary = &ImportSummary{}
	}
	summary.ImportRunID = run.ID
	if err != nil {
		_ = s.FailSync(syncID, err.Error())
		return summary, err
	}
	if err = scoped.UpdateSyncCheckpoint(syncID, &store.Checkpoint{MessagesProcessed: int64(summary.MessagesImported), MessagesAdded: int64(summary.MessagesImported)}); err != nil {
		_ = s.FailSync(syncID, err.Error())
		return summary, fmt.Errorf("update Apple Messages checkpoint: %w", err)
	}
	if err = s.CompleteSync(syncID, ""); err != nil {
		return summary, fmt.Errorf("complete Apple Messages import: %w", err)
	}
	return summary, nil
}

func (c *Client) importMessages(ctx context.Context, s *store.Store, sourceID int64) (*ImportSummary, error) {
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

			// Per-message failures are skipped for this invocation, including a full page.
			lastROWID = msg.ROWID

			if err := c.importMessage(
				ctx, s, sourceID, &msg,
				imessageLabelID, smsLabelID,
				phoneCache, emailCache, convCache,
				summary,
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

// nativeParticipantPlan prepares the chat.db identities without archive writes.
type nativeParticipantPlan struct {
	participants []store.ParticipantPersistData
	keys         []string
	phone        []bool
	count        []bool
	indices      map[string]int
}

func (p *nativeParticipantPlan) add(handle string, fallbackOwner bool) int {
	phone, email, _ := resolveHandle(handle)
	if phone == "" && email == "" {
		return -1
	}
	key := email
	isPhone := phone != ""
	if isPhone {
		key = phone
	}
	if p.indices == nil {
		p.indices = make(map[string]int)
	}
	if index, ok := p.indices[key]; ok {
		return index
	}
	index := len(p.participants)
	p.indices[key] = index
	participant := store.ParticipantPersistData{EmailAddress: email}
	if isPhone {
		participant = store.ParticipantPersistData{PhoneNumber: phone, IdentifierType: "imessage"}
	}
	if fallbackOwner {
		participant.DisplayName = "Me"
	}
	p.participants = append(p.participants, participant)
	p.keys = append(p.keys, key)
	p.phone = append(p.phone, isPhone)
	p.count = append(p.count, !fallbackOwner)
	return index
}

func nativeParticipantID(ids []int64, index int) int64 {
	if index < 0 {
		return 0
	}
	return ids[index]
}

func (c *Client) chatHandles(ctx context.Context, msg *messageRow) ([]string, error) {
	if msg.ChatROWID == nil {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT h.id FROM chat_handle_join chj JOIN handle h ON h.ROWID=chj.handle_id WHERE chj.chat_id=?`, *msg.ChatROWID)
	if err != nil {
		return nil, fmt.Errorf("read native chat members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var handles []string
	for rows.Next() {
		var handle string
		if err := rows.Scan(&handle); err != nil {
			return nil, err
		}
		handles = append(handles, handle)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return handles, nil
}

// importMessage publishes one complete native projection. Reads from chat.db,
// body decoding and raw encoding finish before archive identity/writer fences.
func (c *Client) importMessage(ctx context.Context, s *store.Store, sourceID int64, msg *messageRow, imessageLabelID, smsLabelID int64, phoneCache, emailCache, convCache map[string]int64, summary *ImportSummary) error {
	chatGUID := "no-chat-" + strconv.FormatInt(msg.ROWID, 10)
	if msg.ChatGUID != nil {
		chatGUID = *msg.ChatGUID
	}
	convType := "direct_chat"
	if strings.Contains(chatGUID, ";+;") {
		convType = "group_chat"
	}
	title := ""
	if msg.ChatDisplayName != nil && *msg.ChatDisplayName != "" {
		title = *msg.ChatDisplayName
	} else if convType == "direct_chat" && msg.HandleID != nil {
		phone, email, name := resolveHandle(*msg.HandleID)
		switch {
		case name != "":
			title = name
		case phone != "":
			title = phone
		default:
			title = email
		}
	}
	handles, err := c.chatHandles(ctx, msg)
	if err != nil {
		return err
	}
	var plan nativeParticipantPlan
	ownerHandle := c.ownerHandle
	fallbackOwner := ownerHandle == ""
	if fallbackOwner {
		ownerHandle = "me@imessage.local"
	}
	owner := plan.add(ownerHandle, fallbackOwner)
	var members, recipients []int
	for _, handle := range handles {
		if index := plan.add(handle, false); index >= 0 {
			members = append(members, index)
			recipients = append(recipients, index)
		}
	}
	sender := owner
	if msg.IsFromMe == 0 {
		sender = -1
		if msg.HandleID != nil {
			sender = plan.add(*msg.HandleID, false)
			if sender >= 0 {
				members = append(members, sender)
			}
		}
	} else if msg.ChatROWID == nil && msg.HandleID != nil {
		if index := plan.add(*msg.HandleID, false); index >= 0 {
			recipients = append(recipients, index)
		}
	}
	body := ""
	if msg.Text != nil {
		body = *msg.Text
	} else if len(msg.AttributedBody) > 0 {
		body = extractAttributedBodyText(msg.AttributedBody)
	}
	raw, err := encodeMessageRaw(msg, body)
	if err != nil {
		return err
	}
	date := appleTimestampToTime(msg.Date)
	sentAt := sql.NullTime{Time: date, Valid: !date.IsZero()}
	sourceMessageID := strconv.FormatInt(msg.ROWID, 10)
	clearsDate := false
	if date.IsZero() {
		if err := s.DB().QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM messages WHERE source_id=? AND source_message_id=? AND (sent_at IS NOT NULL OR internal_date IS NOT NULL))`), sourceID, sourceMessageID).Scan(&clearsDate); err != nil {
			return fmt.Errorf("check existing message date: %w", err)
		}
	}
	msgType, labelID := "imessage", imessageLabelID
	if msg.Service != nil && strings.EqualFold(*msg.Service, "SMS") {
		msgType, labelID = "sms", smsLabelID
	}
	_, seenConversation := convCache[chatGUID]
	var participantTitle *store.ConversationParticipantTitle
	if !seenConversation && title == "" && convType == "group_chat" {
		participantTitle = &store.ConversationParticipantTitle{MaxNames: 3, ExcludeEmail: "me@imessage.local", ExcludeDisplayName: "Me"}
	}
	var committedIDs []int64
	_, err = s.PersistMessageWithParticipantsContext(ctx, plan.participants, func(ids []int64) *store.MessagePersistData {
		committedIDs = append([]int64(nil), ids...)
		conversationMembers := make([]store.ConversationParticipantRef, 0, len(members))
		for _, index := range members {
			conversationMembers = append(conversationMembers, store.ConversationParticipantRef{ParticipantID: ids[index], Role: "member"})
		}
		ownerID, senderID := nativeParticipantID(ids, owner), nativeParticipantID(ids, sender)
		var sets []store.RecipientSet
		if senderID > 0 {
			sets = append(sets, store.RecipientSet{Type: "from", ParticipantIDs: []int64{senderID}})
		}
		var to []int64
		if msg.IsFromMe != 0 {
			for _, index := range recipients {
				to = append(to, ids[index])
			}
		} else if ownerID > 0 {
			to = []int64{ownerID}
		}
		if len(to) > 0 {
			sets = append(sets, store.RecipientSet{Type: "to", ParticipantIDs: to})
		}
		return &store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: sourceMessageID, MessageType: msgType, SentAt: sentAt, InternalDate: sentAt, SenderID: sql.NullInt64{Int64: senderID, Valid: senderID > 0}, IsFromMe: msg.IsFromMe != 0, Snippet: sql.NullString{String: snippet(body, 100), Valid: body != ""}, SizeEstimate: int64(len(body)), HasAttachments: msg.HasAttachments != 0},
			Conversation: &store.ConversationPersistData{SourceConversationID: chatGUID, ConversationType: convType, Title: title, Participants: conversationMembers, PreserveExistingParticipants: true, ParticipantTitle: participantTitle},
			BodyText:     sql.NullString{String: body, Valid: body != ""}, PreserveBodyIfOmitted: true, RawMIME: raw, RawFormat: "imessage_json", Recipients: sets, PreserveLabels: true, AddLabelIDs: []int64{labelID},
		}
	})
	if err != nil {
		return fmt.Errorf("persist Apple Messages snapshot: %w", err)
	}
	for index, key := range plan.keys {
		cache := emailCache
		if plan.phone[index] {
			cache = phoneCache
		}
		if _, ok := cache[key]; !ok {
			cache[key] = committedIDs[index]
			if plan.count[index] {
				summary.ParticipantsResolved++
			}
		}
	}
	if !seenConversation {
		convCache[chatGUID] = 1
		summary.ConversationsImported++
	}
	if clearsDate {
		summary.DatesCleared++
	}
	if msg.HasAttachments != 0 {
		c.logger.Debug("message has attachments (extraction not yet implemented)", "rowid", msg.ROWID)
	}
	summary.MessagesImported++
	return nil
}

func encodeMessageRaw(msg *messageRow, body string) ([]byte, error) {
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
