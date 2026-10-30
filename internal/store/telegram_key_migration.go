package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/constant"
)

// TelegramExportKeyEvidence identifies an archived message also present in a
// typed Desktop export. Two distinct matches are required to migrate a legacy
// unmarked group, because old group_chat rows did not retain the peer kind.
type TelegramExportKeyEvidence struct {
	MessageID int64
	SentAt    time.Time
	Text      string
}

// ErrTelegramEvidenceInsufficient means fewer than two distinct archived
// message snapshots match the provided peer evidence.
var ErrTelegramEvidenceInsufficient = errors.New("insufficient matching message evidence for legacy Telegram conversation; archive was not changed")

// TelegramExportReplyEvidence identifies an exported reply and its archived snapshot.
type TelegramExportReplyEvidence struct {
	ReplyID int64
	SentAt  time.Time
	Text    string
}

// TelegramExportKeyChange describes a peer-kind mapping established by an export.
type TelegramExportKeyChange struct {
	LegacyID              int64
	MarkedID              int64
	ConversationType      string
	AllowRetype           bool
	HasCrossChatReplies   bool
	ReplyEvidence         map[int64]TelegramExportReplyEvidence
	ReplyEvidenceOverflow bool
	Evidence              []TelegramExportKeyEvidence
}

type telegramKeyPlan struct {
	change         TelegramExportKeyChange
	conversationID int64
}

// VerifyTelegramReplyContext verifies an old deferred reply against its original
// message snapshot and the typed peer in an export before permitting recovery.
func (s *Store) VerifyTelegramReplyContext(ctx context.Context, sourceID int64, key, parent string, sent time.Time, body string) (bool, error) {
	if parent == "" || sent.IsZero() {
		return false, nil
	}
	verified := false
	err := s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error {
		return s.lockSyncSourceTx(ctx, tx, sourceID)
	}, func(tx *loggedTx) error {
		var id int64
		var storedTime sql.NullTime
		var metadata sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, sent_at, metadata FROM messages WHERE source_id = ? AND source_message_id = ?`+s.dialect.SelectForUpdate(), sourceID, key).Scan(&id, &storedTime, &metadata)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !storedTime.Valid || !storedTime.Time.Equal(sent) || !metadata.Valid {
			return nil
		}
		fields := map[string]jsontext.Value{}
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
			return errors.New("invalid Telegram recovery metadata")
		}
		var reference string
		if raw, ok := fields["referenced_message_id"]; !ok || json.Unmarshal(raw, &reference) != nil || reference != parent {
			return nil
		}
		var storedBody sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`+s.dialect.SelectForUpdate(), id).Scan(&storedBody)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		storedText := ""
		if storedBody.Valid {
			storedText = storedBody.String
		}
		if storedText != body {
			return nil
		}
		fields["reply_peer_verified"] = jsontext.Value("true")
		encoded, err := json.Marshal(fields, json.Deterministic(true))
		if err != nil {
			return err
		}
		query := fmt.Sprintf(`UPDATE messages SET metadata = %s WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
		if _, err := tx.ExecContext(ctx, query, string(encoded), id, sourceID); err != nil {
			return err
		}
		verified = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("verify legacy Telegram reply: %w", err)
	}
	return verified, nil
}

// MigrateTelegramExportKeysContext atomically rewrites verified legacy keys.
// Ambiguous conversations, conflicting target keys and malformed recovery
// metadata abort the entire transaction. Attachment part keys are preserved.
func (s *Store) MigrateTelegramExportKeysContext(ctx context.Context, sourceID int64, changes []TelegramExportKeyChange) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error {
		return s.lockSyncSourceTx(ctx, tx, sourceID)
	}, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return fmt.Errorf("load Telegram migration source: %w", err)
		}
		if sourceType != "telegram" {
			return errors.New("Telegram key migration requires a Telegram source")
		}
		plans := make([]telegramKeyPlan, 0, len(changes))
		planned := make(map[int64]int64)
		var directClaims []TelegramExportKeyChange
		for _, change := range changes {
			if change.ConversationType == "direct_chat" {
				if change.LegacyID <= 0 || change.MarkedID != change.LegacyID {
					return errors.New("invalid Telegram direct peer claim")
				}
				directClaims = append(directClaims, change)
				continue
			}
			marked := constant.TDLibPeerID(change.MarkedID)
			if change.LegacyID <= 0 || marked.ToPlain() != change.LegacyID || (change.ConversationType == "group_chat" && !marked.IsChat() && !marked.IsChannel()) || (change.ConversationType == "channel" && !marked.IsChannel()) || (change.ConversationType != "group_chat" && change.ConversationType != "channel") {
				return errors.New("invalid Telegram key migration")
			}
			var convID int64
			var convType string
			err := tx.QueryRowContext(ctx, `SELECT id, conversation_type FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(change.LegacyID, 10)).Scan(&convID, &convType)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("find legacy Telegram conversation: %w", err)
			}
			if convType != change.ConversationType {
				continue
			}
			if change.HasCrossChatReplies {
				return errors.New("legacy Telegram cross-chat replies need verified repair before migration; archive was not changed")
			}
			if marked, ok := planned[convID]; ok {
				if marked != change.MarkedID {
					return errors.New("ambiguous Telegram peer kind; matching export evidence is required")
				}
				continue
			}
			var target int64
			err = tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`, sourceID, strconv.FormatInt(change.MarkedID, 10)).Scan(&target)
			if err == nil {
				return errors.New("Telegram conversation key collision; archive was not changed")
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("check Telegram conversation collision: %w", err)
			}
			if err := verifyTelegramExportEvidence(ctx, tx, sourceID, convID, change, s.dialect.SelectForUpdate()); err != nil {
				return err
			}
			if err := s.verifyLegacyTelegramRepliesTx(ctx, tx, sourceID, convID, change); err != nil {
				return err
			}
			planned[convID] = change.MarkedID
			plans = append(plans, telegramKeyPlan{change: change, conversationID: convID})
		}
		retypes := make(map[int64]bool)
		type unresolvedDirect struct{ id int64 }
		var unresolved []unresolvedDirect
		for _, claim := range directClaims {
			var convID int64
			var convType string
			err := tx.QueryRowContext(ctx, `SELECT id, conversation_type FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(claim.LegacyID, 10)).Scan(&convID, &convType)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("check Telegram direct peer claim: %w", err)
			}
			if convType == "direct_chat" {
				continue
			}
			if _, migrating := planned[convID]; migrating {
				continue
			}
			if claim.AllowRetype && (convType == "group_chat" || convType == "chat") {
				if claim.HasCrossChatReplies {
					return errors.New("legacy Telegram cross-chat replies need verified repair before migration; archive was not changed")
				}
				if err := verifyTelegramExportEvidence(ctx, tx, sourceID, convID, claim, s.dialect.SelectForUpdate()); err != nil {
					return err
				}
				if err := s.verifyLegacyTelegramRepliesTx(ctx, tx, sourceID, convID, claim); err != nil {
					return err
				}
				retypes[convID] = true
			}
			unresolved = append(unresolved, unresolvedDirect{id: convID})
		}
		for _, claim := range unresolved {
			if !retypes[claim.id] {
				return errors.New("Telegram direct peer key collision with unresolved legacy conversation; archive was not changed")
			}
		}
		for convID := range retypes {
			if _, err := tx.ExecContext(ctx, `UPDATE conversations SET conversation_type = ? WHERE id = ? AND source_id = ?`, "direct_chat", convID, sourceID); err != nil {
				return fmt.Errorf("retype Telegram user conversation: %w", err)
			}
		}
		for _, plan := range plans {
			if err := s.rekeyTelegramConversationTx(ctx, tx, sourceID, plan); err != nil {
				return err
			}
		}
		if len(plans) > 0 {
			if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
				return fmt.Errorf("invalidate Telegram message caches after key migration: %w", err)
			}
		}
		return nil
	})
}

func verifyTelegramExportEvidence(ctx context.Context, tx *loggedTx, sourceID, convID int64, change TelegramExportKeyChange, lock string) error {
	matched := make(map[int64]bool)
	for _, item := range change.Evidence {
		if item.MessageID == 0 || item.SentAt.IsZero() || item.Text == "" || matched[item.MessageID] {
			continue
		}
		var id int64
		var sent sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT id, sent_at FROM messages WHERE source_id = ? AND conversation_id = ? AND source_message_id = ?`+lock, sourceID, convID, fmt.Sprintf("%d:%d", change.LegacyID, item.MessageID)).Scan(&id, &sent)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("find Telegram migration evidence: %w", err)
		}
		var body sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`+lock, id).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read Telegram migration evidence: %w", err)
		}
		if sent.Valid && sent.Time.Equal(item.SentAt) && body.Valid && body.String == item.Text {
			matched[item.MessageID] = true
		}
		if len(matched) >= 2 {
			return nil
		}
	}
	return ErrTelegramEvidenceInsufficient
}

// verifyLegacyTelegramRepliesTx refuses to reinterpret an old reply unless the
// matching export identifies that same in-chat parent. Old importers ignored
// cross-chat peer IDs, so an absent export message cannot establish the peer.
func (s *Store) verifyLegacyTelegramRepliesTx(ctx context.Context, tx *loggedTx, sourceID, convID int64, change TelegramExportKeyChange) error {
	if change.ReplyEvidenceOverflow {
		return errors.New("too many Telegram reply references to verify safely")
	}
	prefix := strconv.FormatInt(change.LegacyID, 10) + ":"
	after := int64(0)
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id, source_message_id, metadata, reply_to_message_id, sent_at FROM messages WHERE source_id = ? AND conversation_id = ? AND id > ? ORDER BY id LIMIT 200`+s.dialect.SelectForUpdate(), sourceID, convID, after)
		if err != nil {
			return fmt.Errorf("list legacy Telegram replies: %w", err)
		}
		type row struct {
			id       int64
			key      string
			metadata sql.NullString
			parent   sql.NullInt64
			sent     sql.NullTime
		}
		var page []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.key, &r.metadata, &r.parent, &r.sent); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan legacy Telegram reply: %w", err)
			}
			page = append(page, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, r := range page {
			after = r.id
			var metadata struct {
				ReferencedMessageID string `json:"referenced_message_id"`
			}
			if r.metadata.Valid && r.metadata.String != "" {
				if err := json.Unmarshal([]byte(r.metadata.String), &metadata); err != nil {
					return errors.New("invalid legacy Telegram reply metadata")
				}
			}
			if metadata.ReferencedMessageID == "" && !r.parent.Valid {
				continue
			}
			suffix, ok := strings.CutPrefix(r.key, prefix)
			id, err := strconv.ParseInt(suffix, 10, 64)
			if !ok || err != nil || id == 0 || strconv.FormatInt(id, 10) != suffix {
				return errors.New("invalid legacy Telegram reply key")
			}
			evidence, verified := change.ReplyEvidence[id]
			if !verified || evidence.ReplyID == 0 || !r.sent.Valid || !r.sent.Time.Equal(evidence.SentAt) {
				return errors.New("legacy Telegram reply needs matching export evidence; archive was not changed")
			}
			var body sql.NullString
			err = tx.QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = ?`+s.dialect.SelectForUpdate(), r.id).Scan(&body)
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("legacy Telegram reply snapshot conflicts with export; archive was not changed")
			}
			if err != nil {
				return fmt.Errorf("read legacy Telegram reply evidence: %w", err)
			}
			if (body.Valid && body.String != evidence.Text) || (!body.Valid && evidence.Text != "") {
				return errors.New("legacy Telegram reply snapshot conflicts with export; archive was not changed")
			}
			expected := prefix + strconv.FormatInt(evidence.ReplyID, 10)
			if metadata.ReferencedMessageID != "" && metadata.ReferencedMessageID != expected {
				return errors.New("legacy Telegram reply reference conflicts with export; archive was not changed")
			}
			if r.parent.Valid {
				var parentKey string
				var parentSourceID int64
				if err := tx.QueryRowContext(ctx, `SELECT source_id, source_message_id FROM messages WHERE id = ?`+s.dialect.SelectForUpdate(), r.parent.Int64).Scan(&parentSourceID, &parentKey); err != nil {
					return fmt.Errorf("read legacy Telegram reply parent: %w", err)
				}
				if parentSourceID != sourceID || parentKey != expected {
					return errors.New("legacy Telegram reply link conflicts with export; archive was not changed")
				}
			}
		}
		if len(page) < 200 {
			return nil
		}
	}
}

type telegramLegacyMessage struct {
	id       int64
	key      string
	metadata sql.NullString
}

func (s *Store) rekeyTelegramConversationTx(ctx context.Context, tx *loggedTx, sourceID int64, plan telegramKeyPlan) error {
	change := plan.change
	oldPrefix := strconv.FormatInt(change.LegacyID, 10) + ":"
	newPrefix := strconv.FormatInt(change.MarkedID, 10) + ":"
	after := int64(0)
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id, source_message_id, metadata FROM messages WHERE source_id = ? AND conversation_id = ? AND id > ? ORDER BY id LIMIT 200`+s.dialect.SelectForUpdate(), sourceID, plan.conversationID, after)
		if err != nil {
			return fmt.Errorf("list legacy Telegram messages: %w", err)
		}
		var page []telegramLegacyMessage
		for rows.Next() {
			var m telegramLegacyMessage
			if err := rows.Scan(&m.id, &m.key, &m.metadata); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan legacy Telegram message: %w", err)
			}
			page = append(page, m)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read legacy Telegram messages: %w", err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, m := range page {
			suffix, ok := strings.CutPrefix(m.key, oldPrefix)
			id, err := strconv.ParseInt(suffix, 10, 64)
			if !ok || err != nil || id == 0 || strconv.FormatInt(id, 10) != suffix {
				return errors.New("invalid legacy Telegram message key; archive was not changed")
			}
			metadata, err := migrateTelegramMessageMetadata(m.metadata, change.LegacyID, oldPrefix, newPrefix)
			if err != nil {
				return err
			}
			query := fmt.Sprintf(`UPDATE messages SET source_message_id = ?, metadata = %s WHERE id = ? AND source_id = ? AND conversation_id = ?`, s.dialect.JSONBindExpr())
			if _, err := tx.ExecContext(ctx, query, newPrefix+suffix, metadata, m.id, sourceID, plan.conversationID); err != nil {
				return fmt.Errorf("rewrite Telegram message key: %w", err)
			}
			after = m.id
		}
		if len(page) < 200 {
			break
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET source_conversation_id = ? WHERE id = ? AND source_id = ?`, strconv.FormatInt(change.MarkedID, 10), plan.conversationID, sourceID); err != nil {
		return fmt.Errorf("rewrite Telegram conversation key: %w", err)
	}
	return nil
}

func migrateTelegramMessageMetadata(value sql.NullString, legacyID int64, oldPrefix, newPrefix string) (string, error) {
	fields := map[string]jsontext.Value{}
	if value.Valid && value.String != "" {
		if err := json.Unmarshal([]byte(value.String), &fields); err != nil || fields == nil {
			return "", errors.New("invalid Telegram recovery metadata; archive was not changed")
		}
	}
	if raw, exists := fields["legacy_media_chat_id"]; exists {
		var prior int64
		if err := json.Unmarshal(raw, &prior); err != nil || (prior != 0 && prior != legacyID) {
			return "", errors.New("conflicting Telegram media identity; archive was not changed")
		}
	}
	fields["legacy_media_chat_id"] = jsontext.Value(strconv.FormatInt(legacyID, 10))
	if raw, exists := fields["referenced_message_id"]; exists {
		var reference string
		if err := json.Unmarshal(raw, &reference); err != nil {
			return "", errors.New("invalid Telegram reply metadata; archive was not changed")
		}
		if suffix, ok := strings.CutPrefix(reference, oldPrefix); ok {
			id, err := strconv.ParseInt(suffix, 10, 64)
			if err != nil || id == 0 || strconv.FormatInt(id, 10) != suffix {
				return "", errors.New("invalid Telegram reply key; archive was not changed")
			}
			encoded, err := json.Marshal(newPrefix + suffix)
			if err != nil {
				return "", err
			}
			fields["referenced_message_id"] = encoded
			fields["reply_peer_verified"] = jsontext.Value("true")
		}
	}
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode Telegram recovery metadata: %w", err)
	}
	return string(encoded), nil
}

// HasLegacyTelegramConversationKeysContext reports whether this source has
// positive group/channel keys written by the older Desktop importer.
func (s *Store) HasLegacyTelegramConversationKeysContext(ctx context.Context, sourceID int64) (bool, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return false, err
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`SELECT source_conversation_id FROM conversations WHERE source_id = ? AND conversation_type IN ('group_chat', 'channel', 'chat')`), sourceID)
	if err != nil {
		return false, fmt.Errorf("list legacy Telegram conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key sql.NullString
		if err := rows.Scan(&key); err != nil {
			return false, fmt.Errorf("scan legacy Telegram conversation: %w", err)
		}
		if key.Valid {
			id, err := strconv.ParseInt(key.String, 10, 64)
			if err == nil && id > 0 && strconv.FormatInt(id, 10) == key.String {
				return true, nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read legacy Telegram conversations: %w", err)
	}
	return false, nil
}

// HasLegacyTelegramConversationKeyContext reports whether a positive peer ID
// is still used by a legacy group-like conversation for this source.
func (s *Store) HasLegacyTelegramConversationKeyContext(ctx context.Context, sourceID, peerID int64) (bool, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return false, err
	}
	if peerID <= 0 {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`SELECT EXISTS (
		SELECT 1 FROM conversations
		WHERE source_id = ? AND source_conversation_id = ?
		AND conversation_type IN ('group_chat', 'channel', 'chat')
	)`), sourceID, strconv.FormatInt(peerID, 10)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check legacy Telegram conversation key: %w", err)
	}
	return exists, nil
}

// HasAmbiguousTelegramDirectKeysContext finds legacy direct keys which may
// instead be Desktop's unmarked non-admin monoforum channel IDs.
func (s *Store) HasAmbiguousTelegramDirectKeysContext(ctx context.Context, sourceID int64) (bool, error) {
	ids, err := s.AmbiguousTelegramDirectKeysContext(ctx, sourceID)
	return len(ids) != 0, err
}

// AmbiguousTelegramDirectKeysContext lists unverified overlapping peer IDs.
func (s *Store) AmbiguousTelegramDirectKeysContext(ctx context.Context, sourceID int64) ([]int64, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`SELECT source_conversation_id, metadata FROM conversations WHERE source_id = ? AND conversation_type = 'direct_chat'`), sourceID)
	if err != nil {
		return nil, fmt.Errorf("list Telegram direct conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var key, metadata sql.NullString
		if err := rows.Scan(&key, &metadata); err != nil {
			return nil, err
		}
		if !key.Valid {
			continue
		}
		id, err := strconv.ParseInt(key.String, 10, 64)
		if err == nil && strconv.FormatInt(id, 10) == key.String && id >= constant.MinMTProtoMonoforumID && id <= constant.MaxMTProtoMonoforumID {
			if metadata.Valid && metadata.String != "" {
				var fields map[string]jsontext.Value
				if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
					return nil, errors.New("invalid Telegram conversation metadata")
				}
				if raw, ok := fields["telegram_verified_user"]; ok {
					var verified bool
					if err := json.Unmarshal(raw, &verified); err != nil {
						return nil, errors.New("invalid Telegram user verification")
					}
					if verified {
						continue
					}
				}
			}
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// HasAmbiguousTelegramDirectKeyContext reports whether an unverified legacy
// direct conversation still occupies a positive peer ID in the monoforum range.
func (s *Store) HasAmbiguousTelegramDirectKeyContext(ctx context.Context, sourceID, peerID int64) (bool, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return false, err
	}
	if peerID < constant.MinMTProtoMonoforumID || peerID > constant.MaxMTProtoMonoforumID {
		return false, nil
	}
	var metadata sql.NullString
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`SELECT metadata FROM conversations
		WHERE source_id = ? AND source_conversation_id = ? AND conversation_type = 'direct_chat'`),
		sourceID, strconv.FormatInt(peerID, 10)).Scan(&metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check ambiguous Telegram direct key: %w", err)
	}
	if !metadata.Valid || metadata.String == "" {
		return true, nil
	}
	fields := map[string]jsontext.Value{}
	if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
		return false, errors.New("invalid Telegram conversation metadata")
	}
	if raw, ok := fields["telegram_verified_user"]; ok {
		var verified bool
		if err := json.Unmarshal(raw, &verified); err != nil {
			return false, errors.New("invalid Telegram user verification")
		}
		return !verified, nil
	}
	return true, nil
}

// HasTelegramShiftedHistoryContext identifies Desktop's shifted message keys
// and reply references which need former-group linkage before live sync.
func (s *Store) HasTelegramShiftedHistoryContext(ctx context.Context, sourceID int64) (bool, error) {
	channels, err := s.TelegramShiftedHistoryChannelsContext(ctx, sourceID)
	return len(channels) != 0, err
}
