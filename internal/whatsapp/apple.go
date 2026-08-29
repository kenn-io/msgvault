package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

const appleEpochOffset = int64(978307200)

type appleChat struct {
	RowID  int64
	RawJID string
	Name   string
}

type appleMessage struct {
	RowID          int64               `json:"row_id"`
	ChatRowID      int64               `json:"chat_row_id"`
	StanzaID       string              `json:"stanza_id"`
	FromMe         int                 `json:"from_me"`
	MessageDate    appleTimestampValue `json:"message_date"`
	Text           sql.NullString      `json:"text"`
	MessageType    int                 `json:"message_type"`
	FromJID        string              `json:"from_jid"`
	GroupMemberJID string              `json:"group_member_jid"`
	GroupContact   string              `json:"group_contact_name"`
	GroupFirstName string              `json:"group_first_name"`
}

// appleTimestampValue accepts both numeric Core Data timestamp values and the
// time.Time values returned by go-sqlite3 for columns declared TIMESTAMP.
// time.Time.Unix recovers the original numeric seconds before the Apple epoch
// offset is applied by appleMessageTimestamp.
type appleTimestampValue struct {
	Seconds float64 `json:"seconds"`
	Valid   bool    `json:"valid"`
}

func (value *appleTimestampValue) Scan(source any) error {
	switch typed := source.(type) {
	case nil:
		*value = appleTimestampValue{}
		return nil
	case time.Time:
		value.Seconds = float64(typed.Unix()) +
			float64(typed.Nanosecond())/float64(time.Second)
		value.Valid = true
		return nil
	case int64:
		value.Seconds = float64(typed)
		value.Valid = true
		return nil
	case float64:
		value.Seconds = typed
		value.Valid = true
		return nil
	case []byte:
		return value.scanString(string(typed))
	case string:
		return value.scanString(typed)
	default:
		return fmt.Errorf("unsupported Apple timestamp value %T", source)
	}
}

func (value *appleTimestampValue) scanString(source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		*value = appleTimestampValue{}
		return nil
	}
	seconds, err := strconv.ParseFloat(source, 64)
	if err != nil {
		return fmt.Errorf("parse Apple timestamp: %w", err)
	}
	value.Seconds = seconds
	value.Valid = true
	return nil
}

type appleGroupMember struct {
	JID         string
	ContactName string
	FirstName   string
	IsAdmin     bool
}

func (imp *Importer) importApple(
	ctx context.Context,
	db *sql.DB,
	chatDBPath string,
	opts ImportOptions,
) (summary *ImportSummary, retErr error) {
	startedAt := time.Now()
	summary = &ImportSummary{}

	source, err := imp.store.GetOrCreateSource("whatsapp", opts.Phone)
	if err != nil {
		return nil, fmt.Errorf("get or create source: %w", err)
	}
	if opts.DisplayName != "" {
		_ = imp.store.UpdateSourceDisplayName(source.ID, opts.DisplayName)
	}
	summary.SourceID = source.ID

	syncID, err := imp.store.StartSync(source.ID, "whatsapp_apple_import")
	if err != nil {
		return nil, fmt.Errorf("start sync: %w", err)
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	imp = &scoped
	defer func() {
		if retErr != nil {
			_ = imp.store.FailSync(syncID, retErr.Error())
			return
		}
		if err := imp.store.CompleteSync(syncID, ""); err != nil {
			retErr = fmt.Errorf("complete sync: %w", err)
		}
	}()

	imp.progress.OnStart()

	lidMap, err := loadAppleLIDMap(ctx, chatDBPath)
	if err != nil {
		return nil, fmt.Errorf("load Apple LID mapping: %w", err)
	}
	duplicateStanzas, duplicateRows, err := fetchDuplicateAppleTextStanzas(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("find duplicate Apple stanza IDs: %w", err)
	}
	if duplicateRows > 0 {
		summary.Errors++
		imp.progress.OnError(fmt.Errorf(
			"skipping %d Apple text messages whose stanza IDs are duplicated",
			duplicateRows,
		))
	}

	selfParticipantID, err := imp.store.EnsureParticipantByPhone(
		opts.Phone, opts.DisplayName, "whatsapp",
	)
	if err != nil {
		return nil, fmt.Errorf("ensure self participant: %w", err)
	}
	participantIDs := map[string]int64{opts.Phone: selfParticipantID}
	summary.Participants = 1

	chats, err := fetchAppleChats(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("fetch Apple chats: %w", err)
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}
	totalLimit := int64(opts.Limit)
	var totalAdded int64

	for _, chat := range chats {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if totalLimit > 0 && totalAdded >= totalLimit {
			break
		}
		if !isImportableAppleChat(chat.RawJID) {
			continue
		}

		canonicalChatJID := canonicalAppleJID(chat.RawJID, lidMap)
		conversationType := "direct_chat"
		conversationTitle := ""
		if isAppleGroupJID(chat.RawJID) {
			conversationType = "group_chat"
			conversationTitle = chat.Name
		}
		conversationID, err := imp.store.EnsureConversationWithType(
			source.ID, canonicalChatJID, conversationType, conversationTitle,
		)
		if err != nil {
			return summary, fmt.Errorf("ensure Apple conversation: %w", err)
		}
		summary.ChatsProcessed++
		imp.progress.OnChatStart(canonicalChatJID, appleChatTitle(chat, lidMap), 0)

		if err := imp.store.EnsureConversationParticipant(
			conversationID, selfParticipantID, "member",
		); err != nil {
			return summary, fmt.Errorf("add self to Apple conversation: %w", err)
		}

		if isAppleGroupJID(chat.RawJID) {
			members, err := fetchAppleGroupMembers(ctx, db, chat.RowID)
			if err != nil {
				return summary, fmt.Errorf("fetch Apple group members: %w", err)
			}
			for _, member := range members {
				phone := applePhoneForJID(member.JID, lidMap)
				if phone == "" {
					continue
				}
				participantID, err := ensureAppleParticipant(
					imp.store, phone, firstNonEmptyApple(member.ContactName, member.FirstName),
					participantIDs, summary,
				)
				if err != nil {
					return summary, err
				}
				role := "member"
				if member.IsAdmin {
					role = "admin"
				}
				if err := imp.store.EnsureConversationParticipant(
					conversationID, participantID, role,
				); err != nil {
					return summary, fmt.Errorf("add Apple group participant: %w", err)
				}
			}
		} else if phone := applePhoneForJID(chat.RawJID, lidMap); phone != "" {
			participantID, err := ensureAppleParticipant(
				imp.store, phone, chat.Name, participantIDs, summary,
			)
			if err != nil {
				return summary, err
			}
			if err := imp.store.EnsureConversationParticipant(
				conversationID, participantID, "member",
			); err != nil {
				return summary, fmt.Errorf("add Apple direct participant: %w", err)
			}
		}

		var afterRowID int64
		var chatAdded int64
		for {
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			remaining := batchSize
			if totalLimit > 0 {
				left := totalLimit - totalAdded
				if left <= 0 {
					break
				}
				if left < int64(remaining) {
					remaining = int(left)
				}
			}

			messages, err := fetchAppleMessages(
				ctx, db, chat.RowID, afterRowID, remaining,
			)
			if err != nil {
				return summary, fmt.Errorf("fetch Apple messages: %w", err)
			}
			if len(messages) == 0 {
				break
			}

			for _, sourceMessage := range messages {
				afterRowID = sourceMessage.RowID
				summary.MessagesProcessed++
				if sourceMessage.MessageType != 0 ||
					!sourceMessage.Text.Valid || strings.TrimSpace(sourceMessage.Text.String) == "" ||
					strings.TrimSpace(sourceMessage.StanzaID) == "" {
					summary.MessagesSkipped++
					continue
				}
				if _, duplicate := duplicateStanzas[sourceMessage.StanzaID]; duplicate {
					summary.MessagesSkipped++
					continue
				}

				senderID, senderPhone, err := resolveAppleMessageSender(
					imp.store, sourceMessage, chat, lidMap,
					selfParticipantID, participantIDs, summary,
				)
				if err != nil {
					return summary, err
				}
				if sourceMessage.FromMe != 0 {
					senderPhone = opts.Phone
				}
				if senderID.Valid {
					if err := imp.store.EnsureConversationParticipant(
						conversationID, senderID.Int64, "member",
					); err != nil {
						return summary, fmt.Errorf("add Apple message sender: %w", err)
					}
				}

				message := mapAppleMessage(
					sourceMessage, conversationID, source.ID, senderID,
				)
				messageID, err := imp.store.UpsertMessage(&message)
				if err != nil {
					return summary, fmt.Errorf("upsert Apple message: %w", err)
				}
				if err := imp.store.UpsertMessageBody(
					messageID, sourceMessage.Text, sql.NullString{},
				); err != nil {
					return summary, fmt.Errorf("store Apple message body: %w", err)
				}
				rawJSON, err := json.Marshal(sourceMessage)
				if err != nil {
					return summary, fmt.Errorf("encode Apple message raw data: %w", err)
				}
				if err := imp.store.UpsertMessageRawWithFormat(
					messageID, rawJSON, "whatsapp_apple_json",
				); err != nil {
					return summary, fmt.Errorf("store Apple message raw data: %w", err)
				}
				if err := imp.store.UpsertFTS(
					messageID, "", sourceMessage.Text.String, senderPhone, "", "",
				); err != nil {
					return summary, fmt.Errorf("index Apple message: %w", err)
				}

				summary.MessagesAdded++
				chatAdded++
				totalAdded++
				if totalLimit > 0 && totalAdded >= totalLimit {
					break
				}
			}

			if err := imp.store.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
				MessagesProcessed: summary.MessagesProcessed,
				MessagesAdded:     summary.MessagesAdded,
			}); err != nil {
				return summary, fmt.Errorf("checkpoint Apple import: %w", err)
			}
			imp.progress.OnProgress(
				summary.MessagesProcessed,
				summary.MessagesAdded,
				summary.MessagesSkipped,
			)

			if len(messages) < remaining || (totalLimit > 0 && totalAdded >= totalLimit) {
				break
			}
		}
		imp.progress.OnChatComplete(canonicalChatJID, chatAdded)
	}

	if err := imp.store.RecomputeConversationStats(source.ID); err != nil {
		return summary, fmt.Errorf("recompute Apple conversation stats: %w", err)
	}
	summary.Duration = time.Since(startedAt)
	imp.progress.OnComplete(summary)
	return summary, nil
}

func fetchAppleChats(ctx context.Context, db *sql.DB) ([]appleChat, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT Z_PK, COALESCE(ZCONTACTJID, ''), COALESCE(ZPARTNERNAME, '')
		FROM ZWACHATSESSION
		WHERE TRIM(COALESCE(ZCONTACTJID, '')) <> ''
		ORDER BY COALESCE(ZLASTMESSAGEDATE, 0) DESC, Z_PK DESC
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var chats []appleChat
	for rows.Next() {
		var chat appleChat
		if err := rows.Scan(&chat.RowID, &chat.RawJID, &chat.Name); err != nil {
			return nil, err
		}
		chats = append(chats, chat)
	}
	return chats, rows.Err()
}

func fetchAppleMessages(
	ctx context.Context,
	db *sql.DB,
	chatRowID, afterRowID int64,
	limit int,
) ([]appleMessage, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.Z_PK, m.ZCHATSESSION, COALESCE(m.ZSTANZAID, ''),
		       COALESCE(m.ZISFROMME, 0), m.ZMESSAGEDATE, m.ZTEXT,
		       COALESCE(m.ZMESSAGETYPE, 0), COALESCE(m.ZFROMJID, ''),
		       COALESCE(gm.ZMEMBERJID, ''), COALESCE(gm.ZCONTACTNAME, ''),
		       COALESCE(gm.ZFIRSTNAME, '')
		FROM ZWAMESSAGE m
		LEFT JOIN ZWAGROUPMEMBER gm ON gm.Z_PK = m.ZGROUPMEMBER
		WHERE m.ZCHATSESSION = ? AND m.Z_PK > ?
		ORDER BY m.Z_PK ASC
		LIMIT ?
	`, chatRowID, afterRowID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []appleMessage
	for rows.Next() {
		var message appleMessage
		if err := rows.Scan(
			&message.RowID, &message.ChatRowID, &message.StanzaID,
			&message.FromMe, &message.MessageDate, &message.Text,
			&message.MessageType, &message.FromJID,
			&message.GroupMemberJID, &message.GroupContact,
			&message.GroupFirstName,
		); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func fetchAppleGroupMembers(
	ctx context.Context,
	db *sql.DB,
	chatRowID int64,
) ([]appleGroupMember, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(ZMEMBERJID, ''), COALESCE(ZCONTACTNAME, ''),
		       COALESCE(ZFIRSTNAME, ''), COALESCE(ZISADMIN, 0)
		FROM ZWAGROUPMEMBER
		WHERE ZCHATSESSION = ? AND TRIM(COALESCE(ZMEMBERJID, '')) <> ''
		ORDER BY Z_PK ASC
	`, chatRowID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var members []appleGroupMember
	for rows.Next() {
		var member appleGroupMember
		var isAdmin int
		if err := rows.Scan(
			&member.JID, &member.ContactName, &member.FirstName, &isAdmin,
		); err != nil {
			return nil, err
		}
		member.IsAdmin = isAdmin != 0
		members = append(members, member)
	}
	return members, rows.Err()
}

func fetchDuplicateAppleTextStanzas(
	ctx context.Context,
	db *sql.DB,
) (map[string]struct{}, int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.ZSTANZAID, COUNT(*)
		FROM ZWAMESSAGE m
		JOIN ZWACHATSESSION c ON c.Z_PK = m.ZCHATSESSION
		WHERE COALESCE(m.ZMESSAGETYPE, 0) = 0
		  AND TRIM(COALESCE(m.ZSTANZAID, '')) <> ''
		  AND TRIM(COALESCE(m.ZTEXT, '')) <> ''
		  AND (
		    LOWER(COALESCE(c.ZCONTACTJID, '')) LIKE '%@s.whatsapp.net'
		    OR LOWER(COALESCE(c.ZCONTACTJID, '')) LIKE '%@lid'
		    OR LOWER(COALESCE(c.ZCONTACTJID, '')) LIKE '%@g.us'
		  )
		GROUP BY m.ZSTANZAID
		HAVING COUNT(*) > 1
	`)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	duplicates := make(map[string]struct{})
	var duplicateRows int64
	for rows.Next() {
		var stanzaID string
		var count int64
		if err := rows.Scan(&stanzaID, &count); err != nil {
			return nil, 0, err
		}
		duplicates[stanzaID] = struct{}{}
		duplicateRows += count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return duplicates, duplicateRows, nil
}

func loadAppleLIDMap(ctx context.Context, chatDBPath string) (map[string]string, error) {
	lidPath := filepath.Join(filepath.Dir(chatDBPath), "LID.sqlite")
	if _, err := os.Stat(lidPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}

	db, err := openReadOnlyDatabase(ctx, lidPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	var tableCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'ZWAZACCOUNT'
	`).Scan(&tableCount); err != nil {
		return nil, err
	}
	if tableCount == 0 {
		return map[string]string{}, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(ZIDENTIFIER, ''), COALESCE(ZPHONENUMBER, '')
		FROM ZWAZACCOUNT
		WHERE TRIM(COALESCE(ZIDENTIFIER, '')) <> ''
		  AND TRIM(COALESCE(ZPHONENUMBER, '')) <> ''
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	mapping := make(map[string]string)
	for rows.Next() {
		var identifier, phoneValue string
		if err := rows.Scan(&identifier, &phoneValue); err != nil {
			return nil, err
		}
		phoneValue = strings.TrimSpace(phoneValue)
		phone := normalizePhone(strings.TrimPrefix(phoneValue, "+"), "s.whatsapp.net")
		if phone == "" {
			continue
		}
		identifier = strings.ToLower(strings.TrimSpace(identifier))
		mapping[identifier] = phone
		mapping[strings.TrimSuffix(identifier, "@lid")] = phone
	}
	return mapping, rows.Err()
}

func resolveAppleMessageSender(
	s *store.Store,
	message appleMessage,
	chat appleChat,
	lidMap map[string]string,
	selfParticipantID int64,
	participantIDs map[string]int64,
	summary *ImportSummary,
) (sql.NullInt64, string, error) {
	if message.FromMe != 0 {
		return sql.NullInt64{Int64: selfParticipantID, Valid: true}, "", nil
	}

	jid := message.GroupMemberJID
	name := firstNonEmptyApple(message.GroupContact, message.GroupFirstName)
	if jid == "" && !isAppleGroupJID(chat.RawJID) {
		jid = firstNonEmptyApple(message.FromJID, chat.RawJID)
		name = firstNonEmptyApple(name, chat.Name)
	}
	phone := applePhoneForJID(jid, lidMap)
	if phone == "" {
		return sql.NullInt64{}, "", nil
	}
	participantID, err := ensureAppleParticipant(
		s, phone, name, participantIDs, summary,
	)
	if err != nil {
		return sql.NullInt64{}, "", err
	}
	return sql.NullInt64{Int64: participantID, Valid: true}, phone, nil
}

func ensureAppleParticipant(
	s *store.Store,
	phone, displayName string,
	participantIDs map[string]int64,
	summary *ImportSummary,
) (int64, error) {
	if participantID, ok := participantIDs[phone]; ok {
		if displayName != "" {
			if _, err := s.EnsureParticipantByPhone(phone, displayName, "whatsapp"); err != nil {
				return 0, fmt.Errorf("update Apple participant: %w", err)
			}
		}
		return participantID, nil
	}
	participantID, err := s.EnsureParticipantByPhone(phone, displayName, "whatsapp")
	if err != nil {
		return 0, fmt.Errorf("ensure Apple participant: %w", err)
	}
	participantIDs[phone] = participantID
	summary.Participants++
	return participantID, nil
}

func mapAppleMessage(
	message appleMessage,
	conversationID, sourceID int64,
	senderID sql.NullInt64,
) store.Message {
	sentAt := appleMessageTimestamp(message.MessageDate)
	return store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: message.StanzaID,
		MessageType:     "whatsapp",
		SentAt:          sentAt,
		InternalDate:    sentAt,
		SenderID:        senderID,
		IsFromMe:        message.FromMe != 0,
		Snippet:         appleMessageSnippet(message.Text),
		SizeEstimate:    int64(len(message.Text.String)),
		ArchivedAt:      time.Now(),
	}
}

func appleMessageTimestamp(value appleTimestampValue) sql.NullTime {
	if !value.Valid || value.Seconds <= 0 ||
		math.IsNaN(value.Seconds) || math.IsInf(value.Seconds, 0) {
		return sql.NullTime{}
	}
	seconds := math.Floor(value.Seconds)
	nanoseconds := int64((value.Seconds - seconds) * float64(time.Second))
	return sql.NullTime{
		Time:  time.Unix(int64(seconds)+appleEpochOffset, nanoseconds).UTC(),
		Valid: true,
	}
}

func appleMessageSnippet(text sql.NullString) sql.NullString {
	if !text.Valid || text.String == "" {
		return sql.NullString{}
	}
	snippet := text.String
	if utf8.RuneCountInString(snippet) > 100 {
		snippet = string([]rune(snippet)[:100])
	}
	return sql.NullString{String: snippet, Valid: true}
}

func isImportableAppleChat(jid string) bool {
	jid = strings.ToLower(strings.TrimSpace(jid))
	return strings.HasSuffix(jid, "@s.whatsapp.net") ||
		strings.HasSuffix(jid, "@lid") ||
		strings.HasSuffix(jid, "@g.us")
}

func isAppleGroupJID(jid string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(jid)), "@g.us")
}

func applePhoneForJID(jid string, lidMap map[string]string) string {
	jid = strings.ToLower(strings.TrimSpace(jid))
	if jid == "" {
		return ""
	}
	if strings.HasSuffix(jid, "@lid") {
		if phone := lidMap[jid]; phone != "" {
			return phone
		}
		return lidMap[strings.TrimSuffix(jid, "@lid")]
	}
	if !strings.HasSuffix(jid, "@s.whatsapp.net") {
		return ""
	}
	return normalizePhone(strings.TrimSuffix(jid, "@s.whatsapp.net"), "s.whatsapp.net")
}

func canonicalAppleJID(jid string, lidMap map[string]string) string {
	if phone := applePhoneForJID(jid, lidMap); phone != "" {
		return strings.TrimPrefix(phone, "+") + "@s.whatsapp.net"
	}
	return strings.ToLower(strings.TrimSpace(jid))
}

func appleChatTitle(chat appleChat, lidMap map[string]string) string {
	if chat.Name != "" {
		return chat.Name
	}
	if phone := applePhoneForJID(chat.RawJID, lidMap); phone != "" {
		return phone
	}
	return chat.RawJID
}

func firstNonEmptyApple(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
