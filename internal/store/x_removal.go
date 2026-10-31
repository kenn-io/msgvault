package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

// XRemovalWork is content-free, replayable derived cleanup. Source execution
// ownership spans Remove, external cleanup and Finish; no SQL transaction does.
type XRemovalWork struct {
	SourceID        int64   `json:"source_id"`
	CanonicalID     string  `json:"canonical_id"`
	Reason          string  `json:"reason"`
	MessageIDs      []int64 `json:"message_ids"`
	EvidenceIDs     []int64 `json:"evidence_ids,omitempty"`
	CacheRevision   int64   `json:"cache_revision"`
	Pending         bool    `json:"pending"`
	DerivedComplete bool    `json:"-"`
}

// RemoveXPostContentContext requires source execution ownership. Suppression
// commits before external I/O and includes every known alias of the native ID.
func (s *Store) RemoveXPostContentContext(ctx context.Context, sourceID int64, nativeID, reason string) (work XRemovalWork, err error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return work, err
	}
	if sourceID <= 0 || !validXControlID(nativeID) {
		return work, errors.New("invalid X removal identity")
	}
	switch reason {
	case "removed", "deleted", "protected", "withheld":
	default:
		return work, errors.New("invalid X removal reason")
	}
	err = s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id=?`, sourceID).Scan(&kind); err != nil {
			return err
		}
		if kind != "x" {
			return errors.New("X removal requires an X source")
		}
		if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
			return err
		}
		alias, err := s.xAliasTx(ctx, tx, sourceID, nativeID)
		if errors.Is(err, ErrXPostNotFound) {
			_, err = tx.ExecContext(ctx, `INSERT INTO x_post_versions(source_id,post_id,canonical_id,version_position,availability,suppressed_at,suppression_reason,cleanup_pending) VALUES(?,?,?,0,?,`+s.dialect.Now()+`,?,1)`, sourceID, nativeID, nativeID, reason, reason)
			if err != nil {
				return err
			}
			alias.Canonical = nativeID
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE x_post_versions SET suppressed_at=COALESCE(suppressed_at,`+s.dialect.Now()+`),suppression_reason=?,availability=?,cleanup_pending=1 WHERE source_id=? AND canonical_id=?`, reason, reason, sourceID, alias.Canonical); err != nil {
			return err
		}
		work = XRemovalWork{SourceID: sourceID, CanonicalID: alias.Canonical, Reason: reason, Pending: true}
		rows, err := tx.QueryContext(ctx, `SELECT message_id,author_id FROM x_post_versions WHERE source_id=? AND canonical_id=?`, sourceID, alias.Canonical)
		if err != nil {
			return err
		}
		authors := map[string]bool{}
		for rows.Next() {
			var mid sql.NullInt64
			var author sql.NullString
			if err := rows.Scan(&mid, &author); err != nil {
				_ = rows.Close()
				return err
			}
			if mid.Valid && !slices.Contains(work.MessageIDs, mid.Int64) {
				work.MessageIDs = append(work.MessageIDs, mid.Int64)
			}
			if author.Valid {
				authors[author.String] = true
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		slices.Sort(work.MessageIDs)
		for _, mid := range work.MessageIDs {
			old, err := s.loadXStoredPostTx(ctx, tx, sourceID, mid)
			if err != nil {
				return err
			}
			q := boundQuerier{ctx: ctx, q: tx}
			if err := s.dialect.InvalidateFTSForMessage(q, mid); err != nil {
				return err
			}
			if err := upsertMessageBody(q, s.dialect, s.fts5Available, mid, sql.NullString{}, sql.NullString{}); err != nil {
				return err
			}
			for _, table := range []string{"message_bodies", "message_raw"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE message_id=?`, mid); err != nil {
					return err
				}
			}
			tombstone := xPostMetadata{CanonicalID: alias.Canonical, CurrentID: old.Metadata.CurrentID, LatestKnownID: old.Metadata.LatestKnownID, AuthorID: old.Metadata.AuthorID, OriginalConversationID: old.Metadata.OriginalConversationID, ReferencedMessageID: old.Metadata.ReferencedMessageID, Availability: reason}
			if tombstone.CurrentID == "" {
				tombstone.CurrentID = old.Message.SourceMessageID
			}
			encoded, err := json.Marshal(tombstone)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET subject=NULL,snippet=NULL,metadata=?,embed_gen=NULL,size_estimate=0,has_attachments=FALSE,attachment_count=0 WHERE id=? AND source_id=?`, string(encoded), mid, sourceID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE message_recipients SET display_name=NULL,email_address=NULL WHERE message_id=?`, mid); err != nil {
				return err
			}
			// X conversations use ID-only titles. The denormalized last preview must
			// never retain a removed message after body/raw are gone.
			if _, err := tx.ExecContext(ctx, `UPDATE conversations SET last_message_preview=NULL WHERE id=? AND source_id=?`, old.Message.ConversationID, sourceID); err != nil {
				return err
			}
		}
		for author := range authors {
			ids, err := s.removeUnsupportedXProfileTx(ctx, tx, sourceID, author)
			if err != nil {
				return err
			}
			work.EvidenceIDs = append(work.EvidenceIDs, ids...)
		}
		if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT CAST(value AS BIGINT) FROM archive_metadata WHERE key=?`, derivedDataRevisionKey).Scan(&work.CacheRevision)
	})
	if err != nil {
		return XRemovalWork{}, fmt.Errorf("remove X content: %w", err)
	}
	return work, nil
}

// removeUnsupportedXProfileTx retains numeric anchors, curated observations and
// observations supported by other sources or remaining public X versions.
func (s *Store) removeUnsupportedXProfileTx(ctx context.Context, tx *loggedTx, sourceID int64, author string) ([]int64, error) {
	var supported bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM x_post_versions WHERE source_id=? AND author_id=? AND suppressed_at IS NULL AND availability='public' AND message_id IS NOT NULL)`, sourceID, author).Scan(&supported); err != nil || supported {
		return nil, err
	}
	anchor := beeperidentity.ProviderIdentityKey("x", "service", "", author)
	rows, err := tx.QueryContext(ctx, `SELECT id,participant_id,normalized_value FROM participant_contact_observations WHERE source_id=? AND provider_user_id=? AND address_kind='username' AND source='archive_observation'`, sourceID, anchor)
	if err != nil {
		return nil, err
	}
	type evidence struct {
		id, pid int64
		value   string
	}
	var records []evidence
	for rows.Next() {
		var e evidence
		if err := rows.Scan(&e.id, &e.pid, &e.value); err != nil {
			_ = rows.Close()
			return nil, err
		}
		records = append(records, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var ids []int64
	for _, e := range records {
		if _, err := tx.ExecContext(ctx, `DELETE FROM participant_contact_observations WHERE id=?`, e.id); err != nil {
			return nil, err
		}
		ids = append(ids, e.id)
		// Only a matching independent named snapshot or a curated person supports
		// this label. Numeric anchors alone do not support profile text.
		var independent bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM message_recipients mr JOIN messages m ON m.id=mr.message_id WHERE mr.participant_id=? AND m.source_id<>? AND mr.display_name<>'' AND mr.display_name=(SELECT display_name FROM participants WHERE id=?)) OR EXISTS(SELECT 1 FROM person_participants WHERE participant_id=?)`, e.pid, sourceID, e.pid, e.pid).Scan(&independent); err != nil {
			return nil, err
		}
		if !independent {
			if _, err := tx.ExecContext(ctx, `UPDATE participants SET display_name=NULL WHERE id=?`, e.pid); err != nil {
				return nil, err
			}
		}
	}
	if len(ids) > 0 {
		if err := s.bumpParticipantIdentifierRevision(ctx, tx); err != nil {
			return nil, err
		}
		if err := s.bumpParticipantDisplayNameRevisionContext(ctx, tx); err != nil {
			return nil, err
		}
		if err := s.reconcileCurrentObservationIdentityMatchesTxContext(ctx, tx); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// FinishXRemovalContext is called only after every daemon-managed derived copy
// has been deleted/rebuilt under the cleanup publication fence.
func (s *Store) FinishXRemovalContext(ctx context.Context, sourceID int64, work XRemovalWork) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if !work.DerivedComplete || sourceID != work.SourceID || work.CanonicalID == "" {
		return errors.New("X derived cleanup is incomplete")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE x_post_versions SET cleanup_pending=0 WHERE source_id=? AND canonical_id=? AND suppressed_at IS NOT NULL`, sourceID, work.CanonicalID)
		return err
	})
}

func (s *Store) PendingXRemovalsContext(ctx context.Context, sourceID int64) ([]XRemovalWork, error) {
	query := `SELECT source_id,canonical_id FROM x_post_versions WHERE cleanup_pending=1`
	var args []any
	if sourceID > 0 {
		query += ` AND source_id=?`
		args = append(args, sourceID)
	}
	query += ` GROUP BY source_id,canonical_id ORDER BY source_id,canonical_id`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	var out []XRemovalWork
	for rows.Next() {
		var w XRemovalWork
		if err := rows.Scan(&w.SourceID, &w.CanonicalID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		w.Pending = true
		out = append(out, w)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT message_id,availability FROM x_post_versions WHERE source_id=? AND canonical_id=?`), out[i].SourceID, out[i].CanonicalID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id sql.NullInt64
			var reason string
			if err := rows.Scan(&id, &reason); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[i].Reason = reason
			if id.Valid && !slices.Contains(out[i].MessageIDs, id.Int64) {
				out[i].MessageIDs = append(out[i].MessageIDs, id.Int64)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) XCleanupPendingContext(ctx context.Context) (bool, error) {
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM x_post_versions WHERE cleanup_pending=1)`).Scan(&pending)
	return pending, err
}

// XVectorIndexExistsContext checks an existing PG vector manifest without
// initializing a backend or enabling model work solely for cleanup.
func (s *Store) XVectorIndexExistsContext(ctx context.Context) (bool, error) {
	var present bool
	if s.IsPostgreSQL() {
		err := s.db.QueryRowContext(ctx, `SELECT to_regclass('index_generations') IS NOT NULL`).Scan(&present)
		return present, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='index_generations')`).Scan(&present)
	return present, err
}
