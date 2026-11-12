package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const voiceSearchSetting = "voice_transcript_search_enabled"

func voiceSearchSQL(pg bool, messageID string) string {
	aggregate := `GROUP_CONCAT(transcript_text, char(10))`
	if pg {
		aggregate = `STRING_AGG(transcript_text, E'\n' ORDER BY attachment_id)`
	}
	return `COALESCE((SELECT ` + aggregate + ` FROM (SELECT transcript_text, attachment_id FROM voice_transcripts WHERE message_id=` + messageID + ` ORDER BY attachment_id)),'')`
}

// SetVoiceTranscriptSearch changes the search setting and removes saved speech when disabled.
func (s *Store) SetVoiceTranscriptSearch(ctx context.Context, enabled bool) error {
	return s.withSourceMaintenanceGateContext(ctx, func(tx *loggedTx) error {
		value := "0"
		if enabled {
			value = "1"
		}
		if err := setArchiveMetadataValueTx(ctx, tx, voiceSearchSetting, value); err != nil {
			return err
		}
		if enabled {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `DELETE FROM voice_transcripts RETURNING message_id`)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		return s.reindexVoiceMessages(ctx, tx, ids)
	})
}

// VoiceTranscriptMappings returns a bounded page of retained Beeper recordings.
func (s *Store) VoiceTranscriptMappings(ctx context.Context, destination string, after int64, limit int) ([]BeeperMediaMapping, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT `+beeperMediaOccurrenceColumns("o")+` FROM beeper_media_occurrences o`+beeperMediaCurrentJoin+`
		AND o.destination_key=? AND o.source_type='beeper' AND a.media_type='voice_note' AND o.retention_state='retained'
		AND o.transcript_sha256='' AND o.attachment_id>?
		AND EXISTS (SELECT 1 FROM beeper_media_deliveries d WHERE d.destination_key=o.destination_key AND d.processing_key=o.processing_key AND d.phase='done' AND d.coverage_state='transcribed')
		AND NOT EXISTS (SELECT 1 FROM voice_transcripts v WHERE v.message_id=o.message_id AND v.attachment_id=o.attachment_id) ORDER BY o.attachment_id LIMIT ?`), destination, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []BeeperMediaMapping
	for rows.Next() {
		var m BeeperMediaMapping
		var next sql.NullTime
		targets := beeperMediaOccurrenceScanTargets(&m)
		targets[beeperMediaNextActionIndex] = &next
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// SaveVoiceTranscript checks the setting and retained version in the same transaction as the index update.
func (s *Store) SaveVoiceTranscript(ctx context.Context, m BeeperMediaMapping, text string) error {
	return s.withSourceMaintenanceGateContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, `UPDATE archive_metadata SET value=value WHERE key=? AND value='1'`, voiceSearchSetting)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		if err := s.lockMeetingEvidenceWith(ctx, tx, m.MessageID); err != nil {
			return err
		}
		var current int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM beeper_media_occurrences o`+beeperMediaCurrentJoin+`
			AND o.destination_key=? AND o.occurrence_ref=? AND o.revision=? AND o.retention_state='retained'
			AND o.transcript_sha256='' AND o.message_id=? AND o.attachment_id=? AND a.id=o.attachment_id AND m.id=o.message_id
			AND o.vault_uid=? AND o.source_id=? AND o.source_version_id=? AND o.content_version_id=?`, m.DestinationKey, m.OccurrenceRef, m.Revision, m.MessageID, m.AttachmentID, m.VaultUID, m.DocbankSourceID, m.SourceVersionID, m.ContentVersionID).Scan(&current)
		if err != nil || current != 1 {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO voice_transcripts(message_id,attachment_id,transcript_text) VALUES(?,?,?)
			ON CONFLICT(message_id,attachment_id) DO UPDATE SET transcript_text=excluded.transcript_text`, m.MessageID, m.AttachmentID, text)
		if err != nil {
			return err
		}
		return s.reindexVoiceMessages(ctx, tx, []int64{m.MessageID})
	})
}

func (s *Store) reindexVoiceMessages(ctx context.Context, tx *loggedTx, ids []int64) error {
	if !s.fts5Available {
		return nil
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT derived_fts"); err != nil {
			return err
		}
		var err error
		if s.IsPostgreSQL() {
			q := boundQuerier{ctx: ctx, q: tx}
			var row derivedEmailText
			row, err = s.loadDerivedEmailText(q, id)
			if err == nil {
				row.fts.Body = row.body
				err = s.dialect.FTSUpsert(q, row.fts)
			}
		} else {
			_, err = tx.ExecContext(ctx, s.dialect.FTSBackfillBatchSQL(), id, id+1)
		}
		if s.dialect.IsFTSValueTooLargeError(err) {
			_, err = markFTSOverflowUnsearchable(boundQuerier{ctx: ctx, q: tx}, id)
		}
		if err != nil {
			return fmt.Errorf("index voice transcript: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT derived_fts"); err != nil {
			return err
		}
	}
	return nil
}
