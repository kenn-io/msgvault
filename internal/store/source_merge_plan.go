package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	msgmime "go.kenn.io/msgvault/internal/mime"
)

type mergeConversation struct {
	id          int64
	providerID  sql.NullString
	metadata    sql.NullString
	destination int64
	archivalID  string
}
type mergeMessage struct {
	id, conversationID                                         int64
	providerID, rfcID, listID, kind, chatID, subject, metadata sql.NullString
	sender                                                     sql.NullInt64
	sent, deleted, removed                                     sql.NullTime
	archiveOnly                                                bool
	fingerprint                                                string
	survivor                                                   int64
	archivalID                                                 string
}
type sourceMergePlan struct {
	conversations []mergeConversation
	messages      []mergeMessage
}

// Source-local provider IDs are never treated as global identities. A body
// fingerprint needs the sender, timestamp, conversation and complete media
// hashes; snippets and incomplete media are insufficient evidence.
func mergeMessageFingerprint(ctx context.Context, tx *loggedTx, m mergeMessage) (string, error) {
	if !m.sender.Valid || !m.sent.Valid || (m.kind.String != MessageTypeEmail && !stableMergeConversationID(m.chatID.String)) {
		return "", nil
	}
	var recipients, senderEvidence []string
	if m.kind.String == MessageTypeEmail {
		var complete bool
		var err error
		recipients, complete, err = mergeEmailRecipients(ctx, tx, m.id)
		if err != nil {
			return "", err
		}
		if !complete {
			return "", nil
		}
		senderEvidence, err = mergeEmailSenderEvidence(ctx, tx, m.id)
		if err != nil {
			return "", err
		}
	}
	var text, html sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`, m.id).Scan(&text, &html)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(a.content_hash, ''), COALESCE(a.attachment_role, ''), COALESCE(a.content_id, '')
		FROM attachments a
		WHERE a.message_id = ?
		  AND NOT EXISTS (SELECT 1 FROM source_merge_preserved_attachments spa WHERE spa.attachment_id = a.id)
		ORDER BY a.content_hash, a.attachment_role, a.content_id`, m.id)
	if err != nil {
		return "", err
	}
	var media []string
	incomplete := false
	for rows.Next() {
		var hash, role, cid string
		if err := rows.Scan(&hash, &role, &cid); err != nil {
			_ = rows.Close()
			return "", err
		}
		if hash == "" {
			incomplete = true
		}
		media = append(media, hash+":"+role+":"+cid)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return "", err
	}
	if incomplete || (text.String == "" && html.String == "" && len(media) == 0) {
		return "", nil
	}
	payload, err := json.Marshal(struct {
		Chat, Kind, Subject, Text, HTML, Metadata, Sent string
		RFC822, ListID                                  string
		Sender                                          int64
		SenderEvidence, Recipients                      []string
		Media                                           []string
	}{
		mergeFingerprintConversation(m), m.kind.String, m.subject.String, text.String, html.String,
		m.metadata.String, m.sent.Time.UTC().Format(time.RFC3339Nano),
		strings.ToLower(strings.TrimSpace(m.rfcID.String)), strings.ToLower(strings.TrimSpace(m.listID.String)),
		m.sender.Int64, senderEvidence, recipients, media})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func mergeMessageContentMissingFromSurvivor(
	ctx context.Context,
	tx *loggedTx,
	historicalID, survivorID int64,
) (bool, error) {
	var historicalText, historicalHTML sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`, historicalID).
		Scan(&historicalText, &historicalHTML)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var survivorText, survivorHTML sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`, survivorID).
		Scan(&survivorText, &survivorHTML)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if (historicalText.String != "" && survivorText.String == "") ||
		(historicalHTML.String != "" && survivorHTML.String == "") {
		return true, nil
	}
	var historicalMIMEAbsent bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM message_raw historical_raw
		WHERE historical_raw.message_id = ? AND historical_raw.raw_format = 'mime'
		  AND NOT EXISTS (
			SELECT 1 FROM message_raw survivor_raw
			WHERE survivor_raw.message_id = ? AND survivor_raw.raw_format = 'mime'
		  )
	)`, historicalID, survivorID).Scan(&historicalMIMEAbsent)
	return historicalMIMEAbsent, err
}

// mergeEmailRecipients returns the complete envelope identity needed when an
// email has no RFC822 Message-ID. The message_recipients email_address is the
// immutable address captured from the envelope; mutable participant aliases
// cannot prove which recipient the original message named.
func mergeEmailRecipients(ctx context.Context, tx *loggedTx, messageID int64) ([]string, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT LOWER(TRIM(recipient_type)), email_address
		FROM message_recipients
		WHERE message_id = ? AND recipient_type IN ('to', 'cc', 'bcc')
		ORDER BY LOWER(TRIM(recipient_type)), LOWER(TRIM(COALESCE(email_address, ''))), id`, messageID)
	if err != nil {
		return nil, false, err
	}
	var recipients []string
	complete := true
	for rows.Next() {
		var recipientType string
		var address sql.NullString
		if err := rows.Scan(&recipientType, &address); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		canonicalAddress := strings.ToLower(strings.TrimSpace(address.String))
		if recipientType == "" || !address.Valid || canonicalAddress == "" {
			complete = false
		}
		recipients = append(recipients, recipientType+":"+canonicalAddress)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	sort.Strings(recipients)
	return recipients, complete && len(recipients) > 0, nil
}

func mergeEmailSenderEvidence(ctx context.Context, tx *loggedTx, messageID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(participant_id, 0), LOWER(TRIM(COALESCE(email_address, '')))
		FROM message_recipients
		WHERE message_id = ? AND LOWER(TRIM(recipient_type)) = 'from'
		ORDER BY LOWER(TRIM(COALESCE(email_address, ''))), COALESCE(participant_id, 0), id`, messageID)
	if err != nil {
		return nil, err
	}
	var senders []string
	for rows.Next() {
		var participantID int64
		var address string
		if err := rows.Scan(&participantID, &address); err != nil {
			_ = rows.Close()
			return nil, err
		}
		senders = append(senders, fmt.Sprintf("%d:%s", participantID, address))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return senders, nil
}

type mergePayloadFingerprintKey struct {
	senderID int64
	sentAt   string
}

func mergePayloadFingerprintKeyFor(m mergeMessage) (mergePayloadFingerprintKey, bool) {
	if !m.sender.Valid || !m.sent.Valid || (m.kind.String != MessageTypeEmail && !stableMergeConversationID(m.chatID.String)) {
		return mergePayloadFingerprintKey{}, false
	}
	return mergePayloadFingerprintKey{
		senderID: m.sender.Int64,
		sentAt:   m.sent.Time.UTC().Format(time.RFC3339Nano),
	}, true
}

func mergePayloadFingerprintCandidateKeys(messages []mergeMessage) map[mergePayloadFingerprintKey]struct{} {
	candidates := make(map[mergePayloadFingerprintKey]struct{}, len(messages))
	for _, m := range messages {
		if m.deleted.Valid || m.removed.Valid {
			continue
		}
		if key, ok := mergePayloadFingerprintKeyFor(m); ok {
			candidates[key] = struct{}{}
		}
	}
	return candidates
}

func mergeRFC822FingerprintCandidateIDs(messages []mergeMessage) map[string]struct{} {
	candidates := make(map[string]struct{}, len(messages))
	for _, m := range messages {
		if m.deleted.Valid || m.removed.Valid || m.kind.String != MessageTypeEmail || m.rfcID.String == "" {
			continue
		}
		candidates[m.rfcID.String] = struct{}{}
	}
	return candidates
}

func mergePayloadFingerprintCandidate(m mergeMessage, candidates map[mergePayloadFingerprintKey]struct{}) bool {
	key, ok := mergePayloadFingerprintKeyFor(m)
	if !ok {
		return false
	}
	_, ok = candidates[key]
	return ok
}

func mergeReadMessages(
	ctx context.Context,
	tx *loggedTx,
	sourceID int64,
	includeArchiveOnlyFingerprints bool,
	fingerprintCandidates map[mergePayloadFingerprintKey]struct{},
	rfc822FingerprintCandidates map[string]struct{},
) ([]mergeMessage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.conversation_id, m.source_message_id, m.rfc822_message_id,
 m.message_type, COALESCE((SELECT original_conversation_id FROM source_merge_conversations smc WHERE smc.conversation_id = c.id), c.source_conversation_id), m.subject, CAST(m.metadata AS TEXT), m.sender_id,
 m.sent_at, m.received_at, m.internal_date, m.deleted_at, m.deleted_from_source_at, m.list_id,
 EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)
 FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE m.source_id = ? ORDER BY m.id`, sourceID)
	if err != nil {
		return nil, err
	}
	var messages []mergeMessage
	for rows.Next() {
		var m mergeMessage
		var received, internal sql.NullTime
		if err := rows.Scan(&m.id, &m.conversationID, &m.providerID, &m.rfcID, &m.kind, &m.chatID, &m.subject, &m.metadata, &m.sender, &m.sent, &received, &internal, &m.deleted, &m.removed, &m.listID, &m.archiveOnly); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if m.rfcID.Valid {
			m.rfcID.String = msgmime.NormalizeMessageID(m.rfcID.String)
			if m.rfcID.String == "" {
				m.rfcID = sql.NullString{}
			}
		}
		if !m.sent.Valid {
			m.sent = received
		}
		if !m.sent.Valid {
			m.sent = internal
		}
		messages = append(messages, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if err := mergeFingerprintMessages(ctx, tx, messages, includeArchiveOnlyFingerprints, fingerprintCandidates, rfc822FingerprintCandidates); err != nil {
		return nil, err
	}
	return messages, nil
}

func mergeFingerprintMessages(
	ctx context.Context,
	tx *loggedTx,
	messages []mergeMessage,
	includeArchiveOnlyFingerprints bool,
	fingerprintCandidates map[mergePayloadFingerprintKey]struct{},
	rfc822FingerprintCandidates map[string]struct{},
) error {
	for i := range messages {
		if messages[i].archiveOnly && !includeArchiveOnlyFingerprints {
			continue
		}
		_, matchingRFC822ID := rfc822FingerprintCandidates[messages[i].rfcID.String]
		if !mergePayloadFingerprintCandidate(messages[i], fingerprintCandidates) &&
			(!matchingRFC822ID || messages[i].kind.String != MessageTypeEmail) {
			continue
		}
		fingerprint, err := mergeMessageFingerprint(ctx, tx, messages[i])
		if err != nil {
			return err
		}
		messages[i].fingerprint = fingerprint
	}
	return nil
}

// Matrix room IDs include the room and homeserver identity. Account-local
// mailbox/UID strings never establish a shared conversation namespace.
func stableMergeConversationID(id string) bool {
	return strings.HasPrefix(id, "!") && strings.Contains(id[1:], ":") && !strings.HasSuffix(id, ":")
}

func mergeFingerprintConversation(m mergeMessage) string {
	if m.kind.String == MessageTypeEmail {
		return m.rfcID.String
	}
	return m.chatID.String
}

func buildSourceMergePlan(ctx context.Context, tx *loggedTx, from, into int64, result *SourceMergeResult) (sourceMergePlan, error) {
	var plan sourceMergePlan
	var sourceType string
	if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, from).Scan(&sourceType); err != nil {
		return plan, err
	}
	if sourceType == "beeper" {
		if err := validateMergeBeeperFamilies(ctx, tx, from, into); err != nil {
			return plan, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id, COALESCE((SELECT original_conversation_id FROM source_merge_conversations smc WHERE smc.conversation_id = c.id), c.source_conversation_id), CAST(c.metadata AS TEXT) FROM conversations c WHERE c.source_id = ? ORDER BY c.id`, from)
	if err != nil {
		return plan, err
	}
	for rows.Next() {
		var c mergeConversation
		if err := rows.Scan(&c.id, &c.providerID, &c.metadata); err != nil {
			_ = rows.Close()
			return plan, err
		}
		plan.conversations = append(plan.conversations, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return plan, err
	}
	for i := range plan.conversations {
		c := &plan.conversations[i]
		c.destination = c.id
		if sourceType == "beeper" && stableMergeConversationID(c.providerID.String) {
			var metadata sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT c.id, CAST(c.metadata AS TEXT) FROM conversations c WHERE c.source_id = ? AND (c.source_conversation_id = ? OR EXISTS (SELECT 1 FROM source_merge_conversations smc WHERE smc.conversation_id = c.id AND smc.original_conversation_id = ?)) ORDER BY CASE WHEN c.source_conversation_id = ? THEN 0 ELSE 1 END, c.id LIMIT 1`, into, c.providerID.String, c.providerID.String, c.providerID.String).Scan(&c.destination, &metadata)
			if errors.Is(err, sql.ErrNoRows) {
				c.destination = c.id
			} else if err != nil {
				return plan, err
			}
			// A shared chat ID with conflicting recorded network identity cannot be consolidated.
			var left, right map[string]any
			_ = json.Unmarshal([]byte(c.metadata.String), &left)
			_ = json.Unmarshal([]byte(metadata.String), &right)
			for _, key := range []string{"network", "service"} {
				if l, lok := left[key].(string); lok && l != "" {
					if r, rok := right[key].(string); rok && r != "" && l != r {
						return plan, invalidSourceMerge("shared conversation has incompatible provider identity")
					}
				}
			}
		}
		if c.destination == c.id {
			c.archivalID = fmt.Sprintf("msgvault-archive:%d:conversation:%d", from, c.id)
			for {
				var used bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE source_id = ? AND source_conversation_id = ?)`, into, c.archivalID).Scan(&used); err != nil {
					return plan, err
				}
				if !used {
					break
				}
				c.archivalID += ":archive"
			}
		}
	}
	result.ConversationsMoved = int64(len(plan.conversations))
	// Only unique live pairs can prove duplicate occurrence identity.
	plan.messages, err = mergeReadMessages(ctx, tx, from, false, nil, nil)
	if err != nil {
		return plan, err
	}
	// The destination scan reads every row's lightweight identity for RFC822
	// matching and collision-free archival IDs. It loads body, recipient, and
	// attachment fingerprints only for shared sender/timestamp pairs or matching
	// RFC822 IDs.
	fingerprintCandidates := mergePayloadFingerprintCandidateKeys(plan.messages)
	rfc822FingerprintCandidates := mergeRFC822FingerprintCandidateIDs(plan.messages)
	existing, err := mergeReadMessages(ctx, tx, into, false, fingerprintCandidates, rfc822FingerprintCandidates)
	if err != nil {
		return plan, err
	}
	// Source-side payloads only need fingerprints when their sender/timestamp
	// or RFC822 ID can match a live destination message. The lightweight source
	// rows remain in plan.messages for transfer and ambiguity counting.
	if err := mergeFingerprintMessages(
		ctx,
		tx,
		plan.messages,
		true,
		mergePayloadFingerprintCandidateKeys(existing),
		mergeRFC822FingerprintCandidateIDs(existing),
	); err != nil {
		return plan, err
	}
	fingerprints := map[string][]int64{}
	rfcIDs := map[string][]int64{}
	sourceFingerprints := map[string]int{}
	sourceRFCIDs := map[string]int{}
	usedIDs := map[string]bool{}
	for _, m := range existing {
		usedIDs[m.providerID.String] = true
		if m.archiveOnly || m.deleted.Valid || m.removed.Valid {
			continue
		}
		if m.fingerprint != "" {
			fingerprints[m.fingerprint] = append(fingerprints[m.fingerprint], m.id)
		}
		if m.kind.String == MessageTypeEmail && m.rfcID.String != "" {
			rfcIDs[m.rfcID.String] = append(rfcIDs[m.rfcID.String], m.id)
		}
	}
	for _, m := range plan.messages {
		if m.deleted.Valid || m.removed.Valid {
			continue
		}
		if m.fingerprint != "" {
			sourceFingerprints[m.fingerprint]++
		}
		if m.kind.String == MessageTypeEmail && m.rfcID.String != "" {
			sourceRFCIDs[m.rfcID.String]++
		}
	}
	usedSurvivors := map[int64]bool{}
	for i := range plan.messages {
		m := &plan.messages[i]
		m.archivalID = fmt.Sprintf("msgvault-archive:%d:%d", from, m.id)
		for usedIDs[m.archivalID] {
			m.archivalID += ":archive"
		}
		usedIDs[m.archivalID] = true
		if m.deleted.Valid || m.removed.Valid {
			continue
		}
		candidates := fingerprints[m.fingerprint]
		incoming := sourceFingerprints[m.fingerprint]
		if m.kind.String == MessageTypeEmail && m.rfcID.String != "" && len(rfcIDs[m.rfcID.String]) > 0 {
			rfcCandidates := rfcIDs[m.rfcID.String]
			if sourceRFCIDs[m.rfcID.String] != 1 || len(rfcCandidates) != 1 || m.fingerprint == "" ||
				len(candidates) != 1 || incoming != 1 || candidates[0] != rfcCandidates[0] {
				result.AmbiguousMatches++
				continue
			}
		}
		if len(candidates) == 1 && incoming == 1 && !usedSurvivors[candidates[0]] {
			contentMissing, err := mergeMessageContentMissingFromSurvivor(ctx, tx, m.id, candidates[0])
			if err != nil {
				return plan, err
			}
			if contentMissing {
				continue
			}
			m.survivor = candidates[0]
			usedSurvivors[m.survivor] = true
			result.DuplicatesHidden++
			var count int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments a WHERE a.message_id = ? AND (
 a.source_part_key IS NOT NULL OR a.content_hash IS NULL OR a.content_hash = '' OR
 NOT EXISTS (SELECT 1 FROM attachments b WHERE b.message_id = ? AND b.source_part_key IS NULL AND b.content_hash = a.content_hash))`, m.id, m.survivor).Scan(&count); err != nil {
				return plan, err
			}
			result.AttachmentsCopied += count
		} else if len(candidates) > 0 {
			result.AmbiguousMatches++
		}
	}
	result.MessagesMoved = int64(len(plan.messages))
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_identities WHERE source_id = ?`, from).Scan(&result.IdentitiesMerged); err != nil {
		return plan, err
	}
	// Completed execution and checkpoint rows deliberately stay on their original
	// source; count destination key conflicts rather than merging resume state.
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_checkpoints a WHERE a.source_id = ? AND EXISTS (SELECT 1 FROM sync_checkpoints b WHERE b.source_id = ? AND b.checkpoint_type = a.checkpoint_type)`, from, into).Scan(&result.CheckpointConflicts)
	return plan, err
}

// Provider-recorded families are stronger evidence than mutable display names.
func validateMergeBeeperFamilies(ctx context.Context, tx *loggedTx, from, into int64) error {
	read := func(sourceID int64) (map[string]bool, error) {
		families := map[string]bool{}
		rows, err := tx.QueryContext(ctx, `SELECT CAST(metadata AS TEXT) FROM conversations WHERE source_id = ? AND metadata IS NOT NULL`, sourceID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return nil, err
			}
			var metadata map[string]any
			if json.Unmarshal([]byte(raw), &metadata) == nil {
				for _, key := range []string{"network", "service"} {
					if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
						families[strings.ToLower(strings.TrimSpace(value))] = true
					}
				}
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		rows, err = tx.QueryContext(ctx, `SELECT slug FROM communication_services cs WHERE EXISTS (SELECT 1 FROM participant_contact_observations o WHERE o.source_id = ? AND o.service_id = cs.id AND o.source = 'archive_observation' AND o.source_ref LIKE 'beeper:%')`, sourceID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var slug string
			if err := rows.Scan(&slug); err != nil {
				_ = rows.Close()
				return nil, err
			}
			families[slug] = true
		}
		return families, errors.Join(rows.Err(), rows.Close())
	}
	left, err := read(from)
	if err != nil {
		return err
	}
	right, err := read(into)
	if err != nil {
		return err
	}
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	for family := range left {
		if right[family] {
			return nil
		}
	}
	return invalidSourceMerge("sources have incompatible recorded Beeper networks")
}
