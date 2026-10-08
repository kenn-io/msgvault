package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

func (s *Store) ReplaceMessageChatwootAttachments(messageID int64, refs []AttachmentRef) error {
	return s.withTx(func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		if err := s.replaceMessageAttachmentsWhereTx(tx, messageID, `source_attachment_id LIKE ?`, false, refs, "chatwoot:%"); err != nil {
			return err
		}
		return recomputeMessageAttachmentStatsWith(tx, messageID)
	})
}

// ChatwootConversationHead returns the highest archived Chatwoot message ID in
// one conversation, or zero when none exist.
func (s *Store) ChatwootConversationHead(ctx context.Context, sourceID int64, sourceConversationID string) (int64, error) {
	var head int64
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(MAX(CAST(m.source_message_id AS BIGINT)), 0)
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ? AND c.source_id = ? AND c.source_conversation_id = ?`),
		sourceID, sourceID, sourceConversationID).Scan(&head); err != nil {
		return 0, fmt.Errorf("load Chatwoot conversation head: %w", err)
	}
	return head, nil
}

// SyncChatwootSelfAgents applies configuration and ownership under the same identity lock.
func (s *Store) SyncChatwootSelfAgents(ctx context.Context, sourceID int64, wanted []string) error {
	const signal = "chatwoot_self_agent"
	wanted = slices.Clone(wanted)
	slices.Sort(wanted)
	wanted = slices.Compact(wanted)
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var raw sql.NullString
		var imported bool
		if err := tx.QueryRowContext(ctx, `SELECT sync_config,
   EXISTS(SELECT 1 FROM sync_runs WHERE source_id = sources.id AND status = 'completed') OR
   EXISTS(SELECT 1 FROM messages WHERE source_id = sources.id)
   FROM sources WHERE id = ? AND source_type = 'chatwoot'`, sourceID).Scan(&raw, &imported); err != nil {
			return err
		}
		config := map[string]jsontext.Value{}
		if raw.Valid && strings.TrimSpace(raw.String) != "" {
			if err := json.Unmarshal([]byte(raw.String), &config); err != nil {
				return err
			}
		}
		if config == nil {
			config = map[string]jsontext.Value{}
		}
		priorRaw, applied := config["chatwoot_self_agents"]
		var prior []string
		if applied {
			if err := json.Unmarshal(priorRaw, &prior); err != nil {
				return err
			}
		}
		var optedOut bool
		if value := config["no_default_identity"]; len(value) > 0 {
			if err := json.Unmarshal(value, &optedOut); err != nil {
				return err
			}
		}
		changed := false
		for _, address := range wanted {
			if slices.Contains(prior, address) {
				continue
			}
			added, _, err := s.mergeAccountIdentitySignalsTxWith(ctx, tx, sourceID, address, []string{signal}, newIdentifierMatch(address), applied || (!imported && !optedOut))
			if err != nil {
				return err
			}
			changed = changed || added
		}
		rows, err := tx.QueryContext(ctx, `SELECT address, source_signal FROM account_identities WHERE source_id = ?`, sourceID)
		if err != nil {
			return err
		}
		var identities []AccountIdentity
		for rows.Next() {
			var identity AccountIdentity
			if err = rows.Scan(&identity.Address, &identity.SourceSignal); err != nil {
				_ = rows.Close()
				return err
			}
			identities = append(identities, identity)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, identity := range identities {
			if slices.Contains(wanted, identity.Address) {
				continue
			}
			signals := strings.Split(identity.SourceSignal, ",")
			if !slices.Contains(signals, signal) {
				continue
			}
			signals = slices.DeleteFunc(signals, func(value string) bool { return value == signal })
			if len(signals) == 0 {
				if _, err = tx.ExecContext(ctx, `DELETE FROM account_identities WHERE source_id = ? AND address = ?`, sourceID, identity.Address); err != nil {
					return err
				}
				changed = true
			} else if _, err = tx.ExecContext(ctx, `UPDATE account_identities SET source_signal = ? WHERE source_id = ? AND address = ?`, strings.Join(signals, ","), sourceID, identity.Address); err != nil {
				return err
			}
		}
		config["chatwoot_self_agents"], err = json.Marshal(wanted)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(config, json.Deterministic(true))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE sources SET sync_config = %s, updated_at = %s WHERE id = ?`, s.dialect.JSONBindExpr(), s.dialect.Now()), string(encoded), sourceID); err != nil {
			return err
		}
		if !changed {
			return nil
		}
		if _, err = s.bumpIdentityRevisionContext(ctx, tx); err != nil {
			return err
		}
		if err = s.bumpAccountIdentityRevisionContext(ctx, tx); err != nil {
			return err
		}
		return refreshSourceMessageAttributionContext(ctx, tx, sourceID, "")
	})
}
