package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// recordImportDecisionTx belongs to the ready canonical snapshot transaction.
// Neither a silent import nor a retained decision becomes a new arrival later.
// The ledger has no message FK: deleting a message cannot erase its decision.
func (s *Store) recordImportDecisionTx(ctx context.Context, tx *loggedTx, id int64, newlyCreated bool, raw []byte, rawFormat string) error {
	if err := s.validateImportWriteTx(ctx, tx); err != nil {
		return err
	}
	run := s.mcpIngest.importRun
	var source, conversation int64
	var nativeID, messageType string
	var fromMe bool
	var sent sql.NullTime
	var archived time.Time
	if err := tx.QueryRowContext(ctx, `SELECT source_id,source_message_id,conversation_id,message_type,is_from_me,sent_at,archived_at FROM messages WHERE id=?`, id).Scan(&source, &nativeID, &conversation, &messageType, &fromMe, &sent, &archived); err != nil {
		return mcpSafeError(err)
	}
	if source != run.SourceID {
		return mcpStoreError("invalid_import_run")
	}
	var retainedIdentity bool
	if run.Format == "synctech_sms" && messageType == "mms" {
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT identifier FROM sources WHERE id=?`, source).Scan(&owner); err != nil {
			return err
		}
		var err error
		retainedIdentity, err = s.retainSynctechMMSImportIdentityTx(ctx, tx.Tx, run.SourceGeneration, source, nativeID, owner, raw, rawFormat)
		if err != nil {
			return fmt.Errorf("retain native import identity: %w", err)
		}
	}
	runtime := s.mcpRoot().mcpConfig.Load()
	notify := newlyCreated && !retainedIdentity && messageType != "calendar_event" && run.NotifyImports && runtime != nil && runtime.config.Enabled && runtime.config.ImportNotifications && runtime.kinds["msgvault.import_message_archived"+"\x00"+run.Format][string(RefKindMessage)]
	var decisionRun, decisionTime any
	if newlyCreated {
		decisionRun, decisionTime = run.ID, mcpTime(archived)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO import_message_decisions (source_generation,source_message_id,source_id,import_run_id,archived_at,notified) VALUES (?,?,?,?,?,?) ON CONFLICT (source_generation,source_message_id) DO NOTHING`, run.SourceGeneration, nativeID, source, decisionRun, decisionTime, notify)
	if err != nil {
		return mcpSafeError(err)
	}
	added, err := result.RowsAffected()
	if err != nil {
		return mcpSafeError(err)
	}
	if !notify || added == 0 {
		return nil
	}
	var sentAt any
	if sent.Valid {
		sentAt = sent.Time.UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(map[string]any{
		"kind": string(RefKindMessage), "message_id": strconv.FormatInt(id, 10),
		"conversation_id": strconv.FormatInt(conversation, 10), "source_id": strconv.FormatInt(source, 10),
		"from_me": fromMe, "sent_at": sentAt, "archived_at": archived.UTC().Format(time.RFC3339Nano),
		"ingestion_mode": "import", "import_run_id": run.ID, "import_format": run.Format,
	})
	if err != nil {
		return err
	}
	return s.appendMCPEventTx(ctx, tx, MCPEvent{Family: "msgvault.import_message_archived", Kind: string(RefKindMessage), ScopeKind: "source", ScopeID: source, SourceID: source, ConversationID: conversation, MessageID: id, ItemKey: fmt.Sprintf("message:%d", id), FromMe: fromMe, OccurredAt: archived, Data: data})
}

// adoptImportIdentitiesTx preserves only native identity, before enabling capture
// or destroying the row that proves it existed. Callers hold the identity fence.
// Predicates are fixed internal SQL and arguments are bound, never parser input.
func (s *Store) adoptImportIdentitiesTx(ctx context.Context, tx *sql.Tx, predicate string, args ...any) error {
	if err := s.adoptLegacyImazingOccurrencesTx(ctx, tx, predicate, args...); err != nil {
		return err
	}
	if err := s.adoptLegacySynctechMMSIdentitiesTx(ctx, tx, predicate, args...); err != nil {
		return err
	}
	return s.adoptImportIdentityDecisionsTx(ctx, tx, predicate, args...)
}

// Disabled capture keeps opaque silent decisions without requiring raw recovery.
func (s *Store) adoptImportIdentityDecisionsTx(ctx context.Context, tx *sql.Tx, predicate string, args ...any) error {
	var after int64
	for {
		identities, err := s.importIdentityPage(ctx, tx, predicate, args, after)
		if err != nil {
			return err
		}
		for _, identity := range identities {
			lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, "source", identity.source, identity.source)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO import_message_decisions (source_generation,source_message_id,source_id,notified) VALUES (?,?,?,FALSE) ON CONFLICT (source_generation,source_message_id) DO NOTHING`), lifetime.source, identity.nativeID, identity.source)
			if err != nil {
				return err
			}
			after = identity.id
		}
		if len(identities) < 128 {
			return nil
		}
	}
}

type importLegacyIdentity struct {
	id, source int64
	nativeID   string
}

func (s *Store) importIdentityPage(ctx context.Context, tx *sql.Tx, predicate string, args []any, after int64) (page []importLegacyIdentity, returnErr error) {
	parameters := append(append([]any(nil), args...), after)
	rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT m.id,m.source_id,m.source_message_id FROM messages m LEFT JOIN mcp_source_resources sr ON sr.source_id=m.source_id WHERE (`+predicate+`) AND m.id>? AND NOT EXISTS (SELECT 1 FROM import_message_decisions d WHERE d.source_generation=sr.generation AND d.source_message_id=m.source_message_id) ORDER BY m.id LIMIT 128`), parameters...)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	for rows.Next() {
		var identity importLegacyIdentity
		if err := rows.Scan(&identity.id, &identity.source, &identity.nativeID); err != nil {
			return nil, err
		}
		page = append(page, identity)
	}
	return page, rows.Err()
}

// retainImportAliasTx links native key reconciliation to its original durable
// decision. Neither the old key nor its replacement may become a new arrival.
func (s *Store) retainImportAliasTx(ctx context.Context, tx *loggedTx, messageID int64, replacement string) error {
	if err := s.adoptImportIdentitiesTx(ctx, tx.Tx, "m.id=?", messageID); err != nil {
		return err
	}
	var sourceID int64
	var nativeID string
	if err := tx.QueryRowContext(ctx, `SELECT source_id,source_message_id FROM messages WHERE id=?`, messageID).Scan(&sourceID, &nativeID); err != nil {
		return err
	}
	lifetime, err := s.ensureMCPScopeLifetime(ctx, tx.Tx, "source", sourceID, sourceID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO import_message_decisions (source_generation,source_message_id,source_id,import_run_id,archived_at,notified) SELECT source_generation,?,source_id,import_run_id,archived_at,notified FROM import_message_decisions WHERE source_generation=? AND source_message_id=? ON CONFLICT (source_generation,source_message_id) DO NOTHING`, replacement, lifetime.source, nativeID)
	if err != nil {
		return err
	}
	// Multiple retained occurrence allocations may intentionally converge on
	// one archive key through the native duplicate-merge path.
	_, err = tx.ExecContext(ctx, `UPDATE import_occurrence_allocations SET source_message_id=? WHERE source_generation=? AND import_format='imazing_csv' AND source_message_id=?`, replacement, lifetime.source, nativeID)
	return err
}

// RetainExistingImportMessageContext preserves a silent decision for a native
// unchanged-row fast path. It does not rewrite the message, invent a historical
// run/time or publish an occurrence. Non-import archiver callers are unchanged.
func (s *Store) RetainExistingImportMessageContext(ctx context.Context, expected MessageIdentityGuard) error {
	if s.mcpIngest.Mode != IngestImport {
		return nil
	}
	run := s.mcpIngest.importRun
	if run == nil || expected.ID <= 0 || expected.SourceID != run.SourceID || expected.SourceMessageID == "" {
		return mcpStoreError("invalid_import_run")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var actual MessageIdentityGuard
		err := tx.QueryRowContext(ctx, `SELECT id,source_id,source_message_id FROM messages WHERE id=?`+s.dialect.SelectForUpdate(), expected.ID).Scan(&actual.ID, &actual.SourceID, &actual.SourceMessageID)
		if err != nil {
			return fmt.Errorf("read existing import identity: %w", err)
		}
		if actual != expected {
			return errors.New("existing import message identity changed")
		}
		return s.adoptImportIdentitiesTx(ctx, tx.Tx, "m.id=? AND m.source_id=?", expected.ID, expected.SourceID)
	})
}
