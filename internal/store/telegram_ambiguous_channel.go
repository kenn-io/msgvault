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

	"github.com/gotd/td/constant"
)

// EnsureTelegramTypedMonoforumContext records a typed API monoforum before its
// first message, or marks an existing canonical group key as verified.
func (s *Store) EnsureTelegramTypedMonoforumContext(ctx context.Context, sourceID, markedID int64, title string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if !constant.TDLibPeerID(markedID).IsMonoforum() {
		return errors.New("invalid Telegram monoforum peer")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram channel verification requires a Telegram source")
		}
		var convID int64
		var kind string
		var metadata sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, conversation_type, metadata FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(markedID, 10)).Scan(&convID, &kind, &metadata)
		if errors.Is(err, sql.ErrNoRows) {
			query := fmt.Sprintf(`INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title, metadata) VALUES (?, ?, ?, ?, %s) ON CONFLICT (source_id, source_conversation_id) DO NOTHING RETURNING id`, s.dialect.JSONBindExpr())
			err = tx.QueryRowContext(ctx, query, sourceID, strconv.FormatInt(markedID, 10), "group_chat", title, `{"telegram_verified_monoforum":true}`).Scan(&convID)
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("Telegram monoforum changed during verification")
			}
			return err
		}
		if err != nil {
			return err
		}
		if kind != "group_chat" {
			return errors.New("Telegram monoforum has conflicting conversation type")
		}
		fields := map[string]jsontext.Value{}
		if metadata.Valid && metadata.String != "" {
			if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
				return errors.New("invalid Telegram conversation metadata")
			}
		}
		if raw, exists := fields["telegram_verified_monoforum"]; exists {
			var verified bool
			if err := json.Unmarshal(raw, &verified); err != nil || !verified {
				return errors.New("conflicting Telegram monoforum verification")
			}
			return nil
		}
		fields["telegram_verified_monoforum"] = jsontext.Value("true")
		encoded, err := json.Marshal(fields, json.Deterministic(true))
		if err != nil {
			return err
		}
		query := fmt.Sprintf(`UPDATE conversations SET metadata = %s WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
		_, err = tx.ExecContext(ctx, query, string(encoded), convID, sourceID)
		return err
	})
}

// VerifyTelegramAmbiguousChannelContext moves a legacy personal-chat key to
// its authenticated monoforum peer only after matching archived snapshots and
// checking every legacy reply. The caller supplies verified API history.
func (s *Store) VerifyTelegramAmbiguousChannelContext(ctx context.Context, sourceID, bareID, markedID int64, evidence []TelegramExportKeyEvidence, replies map[int64]TelegramExportReplyEvidence, hasCrossChatReplies bool) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	marked := constant.TDLibPeerID(markedID)
	if bareID < constant.MinMTProtoMonoforumID || bareID > constant.MaxMTProtoMonoforumID || !marked.IsMonoforum() || marked.ToPlain() != bareID {
		return errors.New("invalid ambiguous Telegram channel peer")
	}
	if hasCrossChatReplies {
		return errors.New("legacy Telegram cross-chat replies require verified repair")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram channel verification requires a Telegram source")
		}
		var convID int64
		var kind string
		var metadata sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, conversation_type, metadata FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(bareID, 10)).Scan(&convID, &kind, &metadata)
		if err != nil {
			return fmt.Errorf("find ambiguous Telegram conversation: %w", err)
		}
		if kind != "direct_chat" {
			return errors.New("ambiguous Telegram channel is not a direct conversation")
		}
		fields := map[string]jsontext.Value{}
		if metadata.Valid && metadata.String != "" {
			if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
				return errors.New("invalid Telegram conversation metadata")
			}
		}
		if raw, ok := fields["telegram_verified_user"]; ok {
			var verified bool
			if err := json.Unmarshal(raw, &verified); err != nil || verified {
				return errors.New("Telegram conversation already has verified user identity")
			}
		}
		var target int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(markedID, 10)).Scan(&target)
		if err == nil {
			return errors.New("Telegram channel conversation key collision; archive was not changed")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		change := TelegramExportKeyChange{LegacyID: bareID, MarkedID: markedID, ConversationType: "group_chat", Evidence: evidence, ReplyEvidence: replies}
		if err := verifyTelegramExportEvidence(ctx, tx, sourceID, convID, change, s.dialect.SelectForUpdate()); err != nil {
			return err
		}
		if err := s.verifyLegacyTelegramRepliesTx(ctx, tx, sourceID, convID, change); err != nil {
			return err
		}
		if err := s.rejectExternalTelegramReferencesTx(ctx, tx, sourceID, convID, bareID); err != nil {
			return err
		}
		if err := s.rekeyTelegramConversationTx(ctx, tx, sourceID, telegramKeyPlan{change: change, conversationID: convID}); err != nil {
			return err
		}
		fields["telegram_verified_monoforum"] = jsontext.Value("true")
		encoded, err := json.Marshal(fields, json.Deterministic(true))
		if err != nil {
			return err
		}
		query := fmt.Sprintf(`UPDATE conversations SET conversation_type = ?, metadata = %s WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
		if _, err := tx.ExecContext(ctx, query, "group_chat", string(encoded), convID, sourceID); err != nil {
			return err
		}
		if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
			return fmt.Errorf("invalidate Telegram message caches after ambiguous channel repair: %w", err)
		}
		return nil
	})
}

// References from other conversations may name a different typed peer with the
// same bare ID. Do not rewrite or leave them attached without peer evidence.
func (s *Store) rejectExternalTelegramReferencesTx(ctx context.Context, tx *loggedTx, sourceID, convID, bareID int64) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE source_id = ? AND conversation_id <> ? AND reply_to_message_id IN (SELECT id FROM messages WHERE source_id = ? AND conversation_id = ?) LIMIT 1`, sourceID, convID, sourceID, convID).Scan(&exists)
	if err == nil {
		return errors.New("external Telegram reply requires verified repair")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check external Telegram reply links: %w", err)
	}
	prefix := strconv.FormatInt(bareID, 10) + ":"
	rows, err := tx.QueryContext(ctx, `SELECT metadata FROM messages WHERE source_id = ? AND conversation_id <> ? AND CAST(metadata AS TEXT) LIKE ?`, sourceID, convID, "%"+prefix+"%")
	if err != nil {
		return fmt.Errorf("check external Telegram reply metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var metadata sql.NullString
		if err := rows.Scan(&metadata); err != nil {
			return err
		}
		var fields struct {
			Reference string `json:"referenced_message_id"`
		}
		if !metadata.Valid || json.Unmarshal([]byte(metadata.String), &fields) != nil {
			return errors.New("invalid external Telegram reply metadata")
		}
		if strings.HasPrefix(fields.Reference, prefix) {
			return errors.New("external Telegram reply requires verified repair")
		}
	}
	return rows.Err()
}

// CheckVerifiedTelegramChannelExportContext matches an ambiguous Desktop
// dialog to an API-verified monoforum and its archived message snapshots.
func (s *Store) CheckVerifiedTelegramChannelExportContext(ctx context.Context, sourceID, bareID, markedID int64, evidence []TelegramExportKeyEvidence) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	marked := constant.TDLibPeerID(markedID)
	if bareID < constant.MinMTProtoMonoforumID || bareID > constant.MaxMTProtoMonoforumID || !marked.IsMonoforum() || marked.ToPlain() != bareID {
		return errors.New("invalid ambiguous Telegram channel peer")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram channel verification requires a Telegram source")
		}
		var convID int64
		var kind string
		var metadata sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, conversation_type, metadata FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(markedID, 10)).Scan(&convID, &kind, &metadata)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTelegramPeerUnverified
		}
		if err != nil {
			return err
		}
		if kind != "group_chat" || !metadata.Valid {
			return ErrTelegramPeerUnverified
		}
		var fields struct {
			Verified bool `json:"telegram_verified_monoforum"`
		}
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return errors.New("invalid Telegram conversation metadata")
		}
		if !fields.Verified {
			return ErrTelegramPeerUnverified
		}
		return verifyTelegramExportEvidence(ctx, tx, sourceID, convID, TelegramExportKeyChange{LegacyID: markedID, Evidence: evidence}, s.dialect.SelectForUpdate())
	})
}
