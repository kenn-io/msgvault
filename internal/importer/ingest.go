package importer

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// IngestRawMessage parses and stores a raw MIME message into the database.
// This is the shared ingestion path for all importers (mbox, emlx, etc.).
//
// Parameters:
//   - sourceID: the source row ID
//   - identifier: the source identifier (e.g. email address), used for is_from_me
//   - attachmentsDir: directory for attachment files (empty = skip disk storage)
//   - labelIDs: label IDs to apply to the message
//   - sourceMsgID: stable dedup key for source_message_id column
//   - rawHash: sha256 hex of raw bytes, used as thread fallback
//   - raw: the raw RFC 5322 MIME bytes
//   - fallbackDate: used when MIME Date header is missing/unparseable
//   - log: logger (must not be nil)
func IngestRawMessage(
	ctx context.Context, st *store.Store,
	sourceID int64, identifier, attachmentsDir string,
	labelIDs []int64, sourceMsgID, rawHash string,
	raw []byte, fallbackDate time.Time,
	log *slog.Logger,
) error {
	parsed, parseErr := mime.Parse(raw)
	if parseErr != nil {
		errMsg := textutil.FirstLine(parseErr.Error())
		parsed = &mime.Message{
			Subject:  "(MIME parse error)",
			BodyText: fmt.Sprintf("[MIME parsing failed: %s]\n\nRaw MIME data is preserved in message_raw table.", errMsg),
		}
		if !fallbackDate.IsZero() {
			parsed.Date = fallbackDate
		}
	}

	subject := textutil.EnsureUTF8(parsed.Subject)
	bodyText := textutil.EnsureUTF8(parsed.GetBodyText())
	bodyHTML := textutil.EnsureUTF8(parsed.BodyHTML)

	// Sanitize address fields in place so all downstream consumers
	// (participantMap, senderID lookup, buildRecipientSet) use consistent keys.
	for _, addrs := range [][]mime.Address{
		parsed.From, parsed.To, parsed.Cc, parsed.Bcc,
	} {
		for i := range addrs {
			addrs[i].Email = textutil.SanitizeUTF8(addrs[i].Email)
			addrs[i].Name = textutil.SanitizeUTF8(addrs[i].Name)
			addrs[i].Domain = textutil.SanitizeUTF8(addrs[i].Domain)
		}
	}

	allAddresses := make(
		[]mime.Address, 0,
		len(parsed.From)+len(parsed.To)+
			len(parsed.Cc)+len(parsed.Bcc),
	)
	allAddresses = append(allAddresses, parsed.From...)
	allAddresses = append(allAddresses, parsed.To...)
	allAddresses = append(allAddresses, parsed.Cc...)
	allAddresses = append(allAddresses, parsed.Bcc...)
	participantMap, err := st.EnsureParticipantsBatch(allAddresses)
	if err != nil {
		return fmt.Errorf("ensure participants: %w", err)
	}

	var senderID sql.NullInt64
	if len(parsed.From) > 0 && parsed.From[0].Email != "" {
		if id, ok := participantMap[parsed.From[0].Email]; ok {
			senderID = sql.NullInt64{Int64: id, Valid: true}
		}
	}

	isFromMe := false
	if len(parsed.From) > 0 {
		if strings.EqualFold(parsed.From[0].Email, identifier) {
			isFromMe = true
		}
	}

	threadID := threadKey(parsed, rawHash)

	convSubject := subject
	if convSubject == "" {
		convSubject = "(no subject)"
	}
	conversationID, err := st.EnsureConversation(
		sourceID, threadID, convSubject,
	)
	if err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}

	var sentAt sql.NullTime
	if !parsed.Date.IsZero() {
		sentAt = sql.NullTime{Time: parsed.Date.UTC(), Valid: true}
	} else if !fallbackDate.IsZero() {
		sentAt = sql.NullTime{Time: fallbackDate.UTC(), Valid: true}
	}

	snippet := snippetFromBody(bodyText)
	hasAttachments := len(parsed.Attachments) > 0
	attachmentCount := len(parsed.Attachments)

	rec := &store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: sourceMsgID,
		MessageType:     "email",
		SentAt:          sentAt,
		SenderID:        senderID,
		IsFromMe:        isFromMe,
		Subject: sql.NullString{
			String: subject, Valid: subject != "",
		},
		Snippet: sql.NullString{
			String: snippet, Valid: snippet != "",
		},
		SizeEstimate:    int64(len(raw)),
		HasAttachments:  hasAttachments,
		AttachmentCount: attachmentCount,
	}

	// Build recipient sets
	recipientSets := []store.RecipientSet{
		buildRecipientSet("from", parsed.From, participantMap),
		buildRecipientSet("to", parsed.To, participantMap),
		buildRecipientSet("cc", parsed.Cc, participantMap),
		buildRecipientSet("bcc", parsed.Bcc, participantMap),
	}

	// Persist atomically
	messageID, err := st.PersistMessage(&store.MessagePersistData{
		Message:    rec,
		BodyText:   sql.NullString{String: bodyText, Valid: bodyText != ""},
		BodyHTML:   sql.NullString{String: bodyHTML, Valid: bodyHTML != ""},
		RawMIME:    raw,
		Recipients: recipientSets,
		LabelIDs:   labelIDs,
	})
	if err != nil {
		return err
	}

	// Attachments: best-effort outside the transaction (file I/O).
	// Failures are logged but don't fail the ingest — the message,
	// body, and raw MIME are already committed. The attachment count
	// correction below updates metadata to reflect what stored.
	for i := range parsed.Attachments {
		att := &parsed.Attachments[i]
		if err := storeAttachment(
			st, attachmentsDir, messageID, att,
		); err != nil {
			log.Warn("failed to store attachment",
				"message", messageID,
				"filename", att.Filename,
				"error", err,
			)
		}
	}

	// Correct attachment count if disk storage filtered some out
	if attachmentsDir != "" && len(parsed.Attachments) > 0 {
		var storedCount int
		if err := st.DB().QueryRow(
			st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`),
			messageID,
		).Scan(&storedCount); err != nil {
			log.Warn("failed to count stored attachments",
				"message", messageID, "error", err,
			)
		} else if storedCount != attachmentCount {
			if _, err := st.DB().Exec(
				st.Rebind(`UPDATE messages SET has_attachments = ?, attachment_count = ? WHERE id = ?`),
				storedCount > 0, storedCount, messageID,
			); err != nil {
				log.Warn("failed to update attachment metadata",
					"message", messageID, "error", err,
				)
			}
		}
	}

	// FTS: best-effort outside the transaction
	if st.FTS5Available() {
		fromAddr := joinEmails(parsed.From)
		toAddrs := joinEmails(parsed.To)
		ccAddrs := joinEmails(parsed.Cc)
		if err := st.UpsertFTS(
			messageID, subject, bodyText,
			fromAddr, toAddrs, ccAddrs,
		); err != nil {
			log.Warn("failed to upsert FTS",
				"message", messageID, "error", err,
			)
		}
	}

	return nil
}

func normalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.Trim(id, "<>")
	return textutil.SanitizeUTF8(id)
}

func threadKey(parsed *mime.Message, rawHash string) string {
	if len(parsed.References) > 0 {
		if root := normalizeMessageID(parsed.References[0]); root != "" {
			return root
		}
	}
	if irt := normalizeMessageID(parsed.InReplyTo); irt != "" {
		return irt
	}
	if mid := normalizeMessageID(parsed.MessageID); mid != "" {
		return mid
	}
	return rawHash
}

func snippetFromBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	line := textutil.FirstLine(body)
	return textutil.TruncateRunes(line, 200)
}

func joinEmails(addrs []mime.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	emails := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Email != "" {
			emails = append(emails, a.Email)
		}
	}
	return strings.Join(emails, " ")
}

// buildRecipientSet deduplicates addresses and returns a RecipientSet
// ready for store.PersistMessage.
func buildRecipientSet(recipientType string, addresses []mime.Address, participantMap map[string]int64) store.RecipientSet {
	rs := store.RecipientSet{Type: recipientType}
	if len(addresses) == 0 {
		return rs
	}

	idToName := make(map[int64]string)
	var orderedIDs []int64

	for _, addr := range addresses {
		if addr.Email == "" {
			continue
		}
		id, ok := participantMap[addr.Email]
		if !ok {
			continue
		}
		name := textutil.EnsureUTF8(addr.Name)
		if _, seen := idToName[id]; !seen {
			orderedIDs = append(orderedIDs, id)
			idToName[id] = name
			continue
		}
		if idToName[id] == "" && name != "" {
			idToName[id] = name
		}
	}

	rs.ParticipantIDs = orderedIDs
	rs.DisplayNames = make([]string, len(orderedIDs))
	for i, id := range orderedIDs {
		rs.DisplayNames[i] = idToName[id]
	}
	return rs
}

func storeAttachment(
	st *store.Store, attachmentsDir string,
	messageID int64, att *mime.Attachment,
) error {
	storagePath, err := export.StoreAttachmentFile(attachmentsDir, att)
	if err != nil || storagePath == "" {
		return err
	}
	return st.UpsertAttachment(
		messageID, att.Filename, att.ContentType,
		storagePath, att.ContentHash, len(att.Content),
	)
}
