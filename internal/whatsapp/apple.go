package whatsapp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
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

const appleRawFormat = "whatsapp_apple_json"

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

	ownershipCtx := context.WithoutCancel(ctx)
	run, err := imp.store.BeginNativeImportAttempt(ownershipCtx, source.ID, "whatsapp", opts.NotifyImports, syncID, 0)
	if err != nil {
		return nil, fmt.Errorf("begin Apple WhatsApp import: %w", err)
	}
	summary.ImportRunID = run.ID
	importStore, err := imp.store.WithImportRun(ownershipCtx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("bind Apple WhatsApp import: %w", err)
	}
	if importStore == nil {
		return nil, errors.New("bind Apple WhatsApp import: WithImportRun returned a nil Store")
	}
	imp.store = importStore

	imp.progress.OnStart()

	lidMap, err := loadAppleLIDMap(ctx, chatDBPath)
	if err != nil {
		return nil, fmt.Errorf("load Apple LID mapping: %w", err)
	}
	pushNames, err := loadApplePushNames(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("load Apple push names: %w", err)
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

	var selfParticipantID int64
	if opts.NotifyImports {
		selfParticipantID, _, err = imp.store.ParticipantByIdentifier("whatsapp", opts.Phone)
	} else {
		selfParticipantID, err = imp.store.EnsureParticipantByPhone(opts.Phone, opts.DisplayName, "whatsapp")
	}
	if err != nil {
		return nil, fmt.Errorf("ensure self participant: %w", err)
	}
	participantIDs := map[string]int64{opts.Phone: selfParticipantID}
	summary.Participants = 1

	chats, err := fetchAppleChats(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("fetch Apple chats: %w", err)
	}

	aggregates, markersAvailable, err := fetchAppleChatAggregates(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("read Apple chat change markers: %w", err)
	}
	// Without a store identity, markers cannot tell one database from another,
	// so every chat is read.
	storeIdentity, err := fetchAppleStoreIdentity(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("read Apple chat change markers: %w", err)
	}
	if storeIdentity == "" {
		markersAvailable, aggregates = false, nil
	}
	importContext, err := appleImportContext(
		storeIdentity, selfParticipantID, lidMap, pushNames, duplicateStanzas,
	)
	if err != nil {
		return nil, err
	}
	var previousMarkers map[int64]string
	if !opts.Full && markersAvailable {
		previousMarkers, err = imp.previousAppleChatMarkers(ctx, source, syncID, importContext)
		if err != nil {
			return nil, err
		}
	}
	nextMarkers := make(map[int64]string, len(chats))
	for _, chat := range chats {
		if marker, ok := previousMarkers[chat.RowID]; ok {
			nextMarkers[chat.RowID] = marker
		}
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}
	totalLimit := int64(opts.Limit)
	var totalAdded int64
	pendingPushNames := make(map[string]string)

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
		plan := appleConversationPlan{}
		plan.add(opts.Phone, opts.DisplayName, "member")
		conversationID, err := imp.store.ConversationIDContext(ctx, source.ID, canonicalChatJID)
		if err != nil {
			return summary, fmt.Errorf("find Apple conversation: %w", err)
		}
		summary.ChatsProcessed++
		imp.progress.OnChatStart(canonicalChatJID, appleChatTitle(chat, lidMap), 0)
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
				name := firstNonEmptyApple(member.ContactName, member.FirstName)
				if name == "" {
					if pushName := applePushNameFor(pushNames, member.JID); pushName != "" {
						pendingPushNames[phone] = pushName
						if opts.NotifyImports {
							name = pushName
						}
					}
				}
				role := "member"
				if member.IsAdmin {
					role = "admin"
				}
				plan.add(phone, name, role)
			}
		} else if phone := applePhoneForJID(chat.RawJID, lidMap); phone != "" {
			name := chat.Name
			if strings.TrimSpace(name) == "" {
				if pushName := applePushNameFor(pushNames, chat.RawJID); pushName != "" {
					pendingPushNames[phone] = pushName
					if opts.NotifyImports {
						name = pushName
					}
				}
			}
			plan.add(phone, name, "member")
		}
		if !opts.NotifyImports {
			conversationID, err = imp.applyAppleConversationPlan(ctx, source.ID, canonicalChatJID, conversationType, conversationTitle, plan, participantIDs, summary)
			if err != nil {
				return summary, err
			}
		}

		marker := appleChatMarker(aggregates, chat, conversationID)
		if marker != "" && previousMarkers[chat.RowID] == marker {
			if opts.NotifyImports {
				if _, err := imp.applyAppleConversationPlan(ctx, source.ID, canonicalChatJID, conversationType, conversationTitle, plan, participantIDs, summary); err != nil {
					return summary, err
				}
			}
			imp.progress.OnChatComplete(canonicalChatJID, 0)
			continue
		}
		delete(nextMarkers, chat.RowID)

		var afterRowID int64
		var chatAdded int64
		for {
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			if totalLimit > 0 && totalAdded >= totalLimit {
				break
			}
			// Fetch full pages even under --limit: unchanged rows don't count toward it.
			messages, err := fetchAppleMessages(
				ctx, db, chat.RowID, afterRowID, batchSize,
			)
			if err != nil {
				return summary, fmt.Errorf("fetch Apple messages: %w", err)
			}
			if len(messages) == 0 {
				break
			}

			var stanzaIDs []string
			for _, sourceMessage := range messages {
				if appleMessageInWindow(sourceMessage, opts) &&
					isImportableAppleMessage(sourceMessage, duplicateStanzas) {
					stanzaIDs = append(stanzaIDs, sourceMessage.StanzaID)
				}
			}
			stored, err := imp.store.StoredMessagesContext(
				ctx, source.ID, appleRawFormat, stanzaIDs,
			)
			if err != nil {
				return summary, fmt.Errorf("load stored Apple messages: %w", err)
			}

			for _, sourceMessage := range messages {
				if totalLimit > 0 && totalAdded >= totalLimit {
					break
				}
				afterRowID = sourceMessage.RowID
				if !appleMessageInWindow(sourceMessage, opts) {
					continue
				}
				summary.MessagesProcessed++
				if !isImportableAppleMessage(sourceMessage, duplicateStanzas) {
					summary.MessagesSkipped++
					continue
				}

				var senderID sql.NullInt64
				var senderPhone string
				messagePlan := plan.clone()
				senderIndex := -1
				if opts.NotifyImports {
					phone, name := appleMessageSenderIdentity(sourceMessage, chat, lidMap)
					if sourceMessage.FromMe != 0 {
						phone, name = opts.Phone, opts.DisplayName
					}
					if phone != "" {
						if name == "" && sourceMessage.GroupMemberJID != "" {
							name = applePushNameFor(pushNames, sourceMessage.GroupMemberJID)
						}
						senderIndex = messagePlan.add(phone, name, "member")
						senderPhone = phone
						id := participantIDs[phone]
						if id == 0 {
							id, _, err = imp.store.ParticipantByIdentifier("whatsapp", phone)
							if err != nil {
								return summary, fmt.Errorf("find Apple sender: %w", err)
							}
						}
						senderID = sql.NullInt64{Int64: id, Valid: id != 0}
					}
				} else {
					senderID, senderPhone, err = resolveAppleMessageSender(imp.store, sourceMessage, chat, lidMap, selfParticipantID, participantIDs, summary)
					if err != nil {
						return summary, err
					}
					if senderID.Valid {
						if err := imp.store.EnsureConversationParticipant(conversationID, senderID.Int64, "member"); err != nil {
							return summary, fmt.Errorf("add Apple message sender: %w", err)
						}
					}
				}
				if sourceMessage.FromMe == 0 && senderPhone != "" && sourceMessage.GroupMemberJID != "" && firstNonEmptyApple(sourceMessage.GroupContact, sourceMessage.GroupFirstName) == "" {
					if pushName := applePushNameFor(pushNames, sourceMessage.GroupMemberJID); pushName != "" {
						pendingPushNames[senderPhone] = pushName
					}
				}
				if sourceMessage.FromMe != 0 {
					senderPhone = opts.Phone
				}

				message := mapAppleMessage(
					sourceMessage, conversationID, source.ID, senderID,
				)
				rawJSON, err := json.Marshal(sourceMessage, json.Deterministic(true))
				if err != nil {
					return summary, fmt.Errorf("encode Apple message raw data: %w", err)
				}
				data := &store.MessagePersistData{
					Message:        &message,
					BodyText:       sourceMessage.Text,
					RawMIME:        rawJSON,
					RawFormat:      appleRawFormat,
					PreserveLabels: true,
					FTSBestEffort:  opts.NotifyImports,
					FTS: &store.FTSDoc{
						Body:     sourceMessage.Text.String,
						FromAddr: senderPhone,
					},
				}
				if existing, ok := stored[sourceMessage.StanzaID]; ok && appleMessageStored(existing, data) {
					unchanged, err := imp.store.MessageContentMatchesContext(
						ctx, existing.ID, data.BodyText, *data.FTS,
					)
					if err != nil {
						return summary, fmt.Errorf("compare stored Apple message: %w", err)
					}
					if unchanged {
						if opts.NotifyImports {
							if _, err := imp.applyAppleConversationPlan(ctx, source.ID, canonicalChatJID, conversationType, conversationTitle, messagePlan, participantIDs, summary); err != nil {
								return summary, err
							}
						}
						summary.MessagesSkipped++
						continue
					}
				}
				if opts.NotifyImports {
					var resolved []int64
					_, err = imp.store.PersistMessageWithParticipantsContext(ctx, messagePlan.participants, func(ids []int64) *store.MessagePersistData {
						resolved = append([]int64(nil), ids...)
						snapshot := *data
						msg := *data.Message
						msg.SenderID = sql.NullInt64{}
						if senderIndex >= 0 {
							msg.SenderID = sql.NullInt64{Int64: ids[senderIndex], Valid: true}
						}
						snapshot.Message = &msg
						snapshot.Conversation = messagePlan.snapshot(canonicalChatJID, conversationType, conversationTitle, ids)
						return &snapshot
					})
					if err == nil {
						for i, id := range resolved {
							phone := messagePlan.participants[i].PhoneNumber
							if _, ok := participantIDs[phone]; !ok {
								summary.Participants++
							}
							participantIDs[phone] = id
						}
						selfParticipantID = participantIDs[opts.Phone]
						plan = messagePlan
						conversationID, err = imp.store.ConversationIDContext(ctx, source.ID, canonicalChatJID)
					}
				} else {
					_, err = imp.store.PersistMessageContext(ctx, data)
				}
				if err != nil {
					return summary, fmt.Errorf("persist Apple message: %w", err)
				}
				summary.MessagesAdded++
				chatAdded++
				totalAdded++
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

			if len(messages) < batchSize || (totalLimit > 0 && totalAdded >= totalLimit) {
				break
			}
		}
		if opts.NotifyImports && conversationID == 0 {
			// Native empty and filtered chats retain their archive trace without an event.
			conversationID, err = imp.applyAppleConversationPlan(ctx, source.ID, canonicalChatJID, conversationType, conversationTitle, plan, participantIDs, summary)
			if err != nil {
				return summary, err
			}
			selfParticipantID = participantIDs[opts.Phone]
		}
		marker = appleChatMarker(aggregates, chat, conversationID)
		// A chat cut short by --limit may still hold unread changes. A date
		// window leaves older messages unwritten on purpose, so those chats
		// stay unmarked and a later unfiltered run still imports them.
		windowed := !opts.After.IsZero() || !opts.Before.IsZero()
		if marker != "" && !windowed && (totalLimit == 0 || totalAdded < totalLimit) {
			nextMarkers[chat.RowID] = marker
		}
		imp.progress.OnChatComplete(canonicalChatJID, chatAdded)
	}
	for phone, pushName := range pendingPushNames {
		if _, err := ensureAppleParticipant(
			imp.store, phone, pushName, participantIDs, summary,
		); err != nil {
			return summary, err
		}
	}

	if err := imp.store.RecomputeConversationStats(source.ID); err != nil {
		return summary, fmt.Errorf("recompute Apple conversation stats: %w", err)
	}
	if markersAvailable {
		importContext, err = appleImportContext(storeIdentity, selfParticipantID, lidMap, pushNames, duplicateStanzas)
		if err != nil {
			return summary, err
		}
		if err := imp.saveAppleChatMarkers(
			ctx, source.ID, syncID, importContext, nextMarkers,
		); err != nil {
			return summary, err
		}
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
	// Match importApple's iOS text (0) and URL (7) message filter.
	rows, err := db.QueryContext(ctx, `
		SELECT m.ZSTANZAID, COUNT(*)
		FROM ZWAMESSAGE m
		JOIN ZWACHATSESSION c ON c.Z_PK = m.ZCHATSESSION
		WHERE COALESCE(m.ZMESSAGETYPE, 0) IN (0, 7)
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

func loadApplePushNames(
	ctx context.Context,
	db *sql.DB,
) (mapping map[string]string, retErr error) {
	var tableCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'ZWAPROFILEPUSHNAME'
	`).Scan(&tableCount); err != nil {
		return nil, fmt.Errorf("check ZWAPROFILEPUSHNAME availability: %w", err)
	}
	if tableCount == 0 {
		return map[string]string{}, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(ZJID, ''), COALESCE(ZPUSHNAME, '')
		FROM ZWAPROFILEPUSHNAME
		WHERE TRIM(COALESCE(ZJID, '')) <> ''
		  AND TRIM(COALESCE(ZPUSHNAME, '')) <> ''
		ORDER BY Z_PK ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query ZWAPROFILEPUSHNAME: %w", err)
	}
	defer func() {
		if err := rows.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close ZWAPROFILEPUSHNAME rows: %w", err)
		}
	}()

	mapping = make(map[string]string)
	for rows.Next() {
		var jid, pushName string
		if err := rows.Scan(&jid, &pushName); err != nil {
			return nil, fmt.Errorf("scan ZWAPROFILEPUSHNAME: %w", err)
		}
		jid = strings.ToLower(strings.TrimSpace(jid))
		pushName = strings.TrimSpace(pushName)
		if _, exists := mapping[jid]; !exists {
			mapping[jid] = pushName
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ZWAPROFILEPUSHNAME: %w", err)
	}
	return mapping, nil
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

	phone, name := appleMessageSenderIdentity(message, chat, lidMap)
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

func appleMessageSenderIdentity(message appleMessage, chat appleChat, lidMap map[string]string) (string, string) {
	jid := message.GroupMemberJID
	name := firstNonEmptyApple(message.GroupContact, message.GroupFirstName)
	if jid == "" && !isAppleGroupJID(chat.RawJID) {
		jid = firstNonEmptyApple(message.FromJID, chat.RawJID)
		name = firstNonEmptyApple(name, chat.Name)
	}
	phone := applePhoneForJID(jid, lidMap)
	return phone, name
}

func ensureAppleParticipant(
	s *store.Store,
	phone, displayName string,
	participantIDs map[string]int64,
	summary *ImportSummary,
) (int64, error) {
	if participantID, ok := participantIDs[phone]; ok && participantID != 0 {
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
	if _, counted := participantIDs[phone]; !counted {
		summary.Participants++
	}
	participantIDs[phone] = participantID
	return participantID, nil
}

// mapAppleMessage derives a stored message from an Apple row. Bump
// appleChatMarkerVersion when it or isImportableAppleMessage changes, or
// unchanged chats keep the old derivation.
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

// isImportableAppleMessage is the import's row filter. iOS uses 0 for text and
// 7 for URL messages; these codes differ from Android. Keep this filter in
// sync with fetchDuplicateAppleTextStanzas.
func isImportableAppleMessage(message appleMessage, duplicateStanzas map[string]struct{}) bool {
	if (message.MessageType != 0 && message.MessageType != 7) ||
		!message.Text.Valid || strings.TrimSpace(message.Text.String) == "" ||
		strings.TrimSpace(message.StanzaID) == "" {
		return false
	}
	_, duplicate := duplicateStanzas[message.StanzaID]
	return !duplicate
}

// appleMessageStored reports whether the stored message columns and raw
// payload already equal the derived message. Body and search document are
// checked separately because they need a per-message read.
func appleMessageStored(stored store.StoredMessage, data *store.MessagePersistData) bool {
	message := data.Message
	return stored.ConversationID == message.ConversationID &&
		stored.SenderID == message.SenderID &&
		stored.SourceIsFromMe.Valid && stored.SourceIsFromMe.Bool == message.IsFromMe &&
		stored.MessageType == message.MessageType &&
		sameAppleInstant(stored.SentAt, message.SentAt) &&
		sameAppleInstant(stored.InternalDate, message.InternalDate) &&
		stored.Snippet == message.Snippet &&
		stored.SizeEstimate == message.SizeEstimate &&
		bytes.Equal(stored.Raw, data.RawMIME)
}

// sameAppleInstant ignores differences below the microsecond PostgreSQL keeps.
func sameAppleInstant(stored, derived sql.NullTime) bool {
	if stored.Valid != derived.Valid {
		return false
	}
	return !stored.Valid || stored.Time.Sub(derived.Time).Abs() < time.Microsecond
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

// appleMessageInWindow reports whether the message falls within the optional
// [After, Before) window. A message without a valid timestamp is outside any
// bounded window.
func appleMessageInWindow(message appleMessage, opts ImportOptions) bool {
	if opts.After.IsZero() && opts.Before.IsZero() {
		return true
	}
	sent := appleMessageTimestamp(message.MessageDate)
	if !sent.Valid {
		return false
	}
	if !opts.After.IsZero() && sent.Time.Before(opts.After) {
		return false
	}
	if !opts.Before.IsZero() && !sent.Time.Before(opts.Before) {
		return false
	}
	return true
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

func applePushNameFor(pushNames map[string]string, jid string) string {
	if len(pushNames) == 0 {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(jid))
	if key == "" {
		return ""
	}
	return pushNames[key]
}

// appleConversationPlan carries native identities without publishing them before a message commits.
type appleConversationPlan struct {
	participants []store.ParticipantPersistData
	roles        []string
}

func (p *appleConversationPlan) add(phone, name, role string) int {
	for i := range p.participants {
		if p.participants[i].PhoneNumber == phone {
			if name != "" {
				p.participants[i].DisplayName = name
			}
			return i
		}
	}
	i := len(p.participants)
	p.participants = append(p.participants, store.ParticipantPersistData{PhoneNumber: phone, IdentifierType: "whatsapp", DisplayName: name})
	p.roles = append(p.roles, role)
	return i
}

func (p *appleConversationPlan) clone() appleConversationPlan {
	return appleConversationPlan{participants: append([]store.ParticipantPersistData(nil), p.participants...), roles: append([]string(nil), p.roles...)}
}

func (p *appleConversationPlan) snapshot(key, kind, title string, ids []int64) *store.ConversationPersistData {
	members := make([]store.ConversationParticipantRef, len(ids))
	for i, id := range ids {
		members[i] = store.ConversationParticipantRef{ParticipantID: id, Role: p.roles[i]}
	}
	return &store.ConversationPersistData{SourceConversationID: key, ConversationType: kind, Title: title, Participants: members, PreserveExistingParticipants: true, PreserveExistingParticipantRoles: true}
}

// applyAppleConversationPlan preserves the native refresh path for silent,
// unchanged and empty chats. New notifying messages instead persist the plan atomically.
func (imp *Importer) applyAppleConversationPlan(ctx context.Context, sourceID int64, key, kind, title string, plan appleConversationPlan, cache map[string]int64, summary *ImportSummary) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	id, err := imp.store.EnsureConversationWithType(sourceID, key, kind, title)
	if err != nil {
		return 0, fmt.Errorf("ensure Apple conversation: %w", err)
	}
	for i, participant := range plan.participants {
		pid, err := ensureAppleParticipant(imp.store, participant.PhoneNumber, participant.DisplayName, cache, summary)
		if err != nil {
			return 0, err
		}
		if err := imp.store.EnsureConversationParticipant(id, pid, plan.roles[i]); err != nil {
			return 0, fmt.Errorf("add Apple conversation participant: %w", err)
		}
	}
	return id, nil
}
