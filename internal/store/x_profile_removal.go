package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

// Existing protected evidence also discovers interrupted/pre-marker erasures.
// Historical usernames need cleanup when no post work can retire their source.
const xProfileCleanupCondition = `o.address_kind='provider_identity' AND o.source='archive_observation'
 AND o.active_until IS NULL AND o.superseded_at IS NULL
 AND ((',' || COALESCE(o.type_tokens,'') || ',') LIKE '%,x_author_profile_cleanup_pending,%'
 OR ((',' || COALESCE(o.type_tokens,'') || ',') LIKE '%,x_author_protected,%'
 AND NOT EXISTS (SELECT 1 FROM participant_contact_observations p
 WHERE p.provider_user_id=o.provider_user_id AND p.address_kind='provider_identity'
 AND p.source='archive_observation' AND p.active_until IS NULL AND p.superseded_at IS NULL
 AND p.source_id IN (SELECT id FROM sources WHERE source_type='x')
 AND (',' || COALESCE(p.type_tokens,'') || ',') NOT LIKE '%,x_author_protected,%'
 AND (',' || COALESCE(p.type_tokens,'') || ',') NOT LIKE '%,x_author_unknown,%'
 AND p.observed_at>o.observed_at)
 AND EXISTS (SELECT 1 FROM participant_contact_observations u
 WHERE u.provider_user_id=o.provider_user_id AND u.address_kind='username'
 AND u.source='archive_observation' AND u.source_id IN (SELECT id FROM sources WHERE source_type='x')
 AND NOT EXISTS (SELECT 1 FROM x_post_versions v WHERE v.source_id=u.source_id
 AND ('x:author:' || v.author_id)=o.source_resource_uid
 AND (v.suppressed_at IS NULL OR v.cleanup_pending=1)))))`

type XAuthorProfileRemovalWork struct {
	ObservationID, SourceID int64
	AuthorID                string
	DerivedComplete         bool
}

func (s *Store) PendingXAuthorProfileRemovalsContext(ctx context.Context) ([]XAuthorProfileRemovalWork, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id,o.source_id,o.provider_user_id,pi.identifier_value
 FROM participant_contact_observations o JOIN participant_identifiers pi
 ON pi.participant_id=o.participant_id AND pi.identifier_type='x_user_id'
 WHERE o.source_id IN (SELECT id FROM sources WHERE source_type='x') AND `+xProfileCleanupCondition+`
 ORDER BY o.source_id,o.id,pi.identifier_value`)
	if err != nil {
		return nil, err
	}
	var out []XAuthorProfileRemovalWork
	for rows.Next() {
		var work XAuthorProfileRemovalWork
		var anchor string
		if err := rows.Scan(&work.ObservationID, &work.SourceID, &anchor, &work.AuthorID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if validXAuthorID(work.AuthorID) && anchor == beeperidentity.ProviderIdentityKey("x", "service", "", work.AuthorID) {
			out = append(out, work)
		}
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func (s *Store) RemoveXAuthorProfileContext(ctx context.Context, input XAuthorProfileRemovalWork) (work XRemovalWork, err error) {
	if err := s.requireSyncSource(input.SourceID); err != nil {
		return work, err
	}
	if input.SourceID <= 0 || input.ObservationID <= 0 || !validXAuthorID(input.AuthorID) {
		return work, errors.New("invalid X author profile cleanup")
	}
	work.SourceID, work.Reason, work.Pending = input.SourceID, "protected", true
	anchor := beeperidentity.ProviderIdentityKey("x", "service", "", input.AuthorID)
	err = s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, input.SourceID); err != nil {
			return err
		}
		var tokens string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(type_tokens,'') FROM participant_contact_observations
 WHERE id=? AND source_id=? AND provider_user_id=? AND address_kind='provider_identity' AND source='archive_observation'`, input.ObservationID, input.SourceID, anchor).Scan(&tokens); err != nil {
			return err
		}
		values := strings.Split(tokens, ",")
		if !slices.Contains(values, xAuthorProfilePending) {
			values = append(values, xAuthorProfilePending)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE participant_contact_observations SET type_tokens=? WHERE id=?`, joinTypeTokens(values), input.ObservationID); err != nil {
			return err
		}
		protectedAt, _, err := s.xAuthorProtectionTimesAcrossSourcesTx(ctx, tx, input.AuthorID)
		if err != nil || !protectedAt.Valid {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT source_id FROM participant_contact_observations
 WHERE provider_user_id=? AND address_kind='username' AND source='archive_observation'
 AND source_id IN (SELECT id FROM sources WHERE source_type='x') GROUP BY source_id ORDER BY source_id`, anchor)
		if err != nil {
			return err
		}
		var sources []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			sources = append(sources, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, source := range sources {
			ids, err := s.removeUnsupportedXProfileTx(ctx, tx, source, input.AuthorID, protectedAt.Time)
			if err != nil {
				return err
			}
			work.EvidenceIDs = append(work.EvidenceIDs, ids...)
		}
		if len(work.EvidenceIDs) > 0 {
			if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, `SELECT CAST(value AS BIGINT) FROM archive_metadata WHERE key=?`, derivedDataRevisionKey).Scan(&work.CacheRevision)
		}
		return nil
	})
	return work, err
}

func (s *Store) FinishXAuthorProfileRemovalContext(ctx context.Context, work XAuthorProfileRemovalWork) error {
	if err := s.requireSyncSource(work.SourceID); err != nil {
		return err
	}
	if !work.DerivedComplete || work.SourceID <= 0 || work.ObservationID <= 0 || !validXAuthorID(work.AuthorID) {
		return errors.New("x profile derived cleanup is incomplete")
	}
	return s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		var tokens string
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(type_tokens,'') FROM participant_contact_observations
 WHERE id=? AND source_id=? AND provider_user_id=?`, work.ObservationID, work.SourceID, beeperidentity.ProviderIdentityKey("x", "service", "", work.AuthorID)).Scan(&tokens)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		values := slices.DeleteFunc(strings.Split(tokens, ","), func(value string) bool { return value == xAuthorProfilePending })
		_, err = tx.ExecContext(ctx, `UPDATE participant_contact_observations SET type_tokens=? WHERE id=?`, joinTypeTokens(values), work.ObservationID)
		return err
	})
}
