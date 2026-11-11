package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Protection is an archive-wide privacy fact. Keep its numeric identity and
// timestamp when the observing source or its current profile is replaced.
const xProtectedAuthorKeyPrefix = "x_author_protected_at:"
const xProtectedAuthorTimeFormat = "2006-01-02T15:04:05.000000000Z"

func xProtectedAuthorFloor(ctx context.Context, q contextRowQuerier, rebind func(string) string, author string) (sql.NullTime, error) {
	var value string
	err := q.QueryRowContext(ctx, rebind(`SELECT value FROM archive_metadata WHERE key=?`), xProtectedAuthorKeyPrefix+author).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullTime{}, nil
	}
	if err != nil {
		return sql.NullTime{}, err
	}
	at, err := time.Parse(xProtectedAuthorTimeFormat, value)
	if err != nil {
		return sql.NullTime{}, fmt.Errorf("parse X author protection time: %w", err)
	}
	return sql.NullTime{Time: at, Valid: true}, nil
}

func (s *Store) preserveXAuthorFloor(ctx context.Context, exec sourceIdentityExec, author string, at time.Time) error {
	_, err := exec(ctx, s.Rebind(`INSERT INTO archive_metadata(key,value) VALUES(?,?)
 ON CONFLICT(key) DO UPDATE SET value=CASE WHEN excluded.value>archive_metadata.value THEN excluded.value ELSE archive_metadata.value END`), xProtectedAuthorKeyPrefix+author, at.UTC().Format(xProtectedAuthorTimeFormat))
	return err
}

// Old profile observations are upgraded lazily before their source is removed.
func (s *Store) preserveXSourceAuthorFloors(ctx context.Context, query sourceIdentityQuery, exec sourceIdentityExec, sourceID int64) error {
	rows, err := query(ctx, s.Rebind(`SELECT o.source_resource_uid,o.observed_at FROM participant_contact_observations o
 WHERE o.source_id=? AND o.address_kind='provider_identity' AND o.source='archive_observation'
 AND o.source_id IN (SELECT id FROM sources WHERE source_type='x')
 AND (',' || COALESCE(o.type_tokens,'') || ',') LIKE '%,x_author_protected,%'`), sourceID)
	if err != nil {
		return err
	}
	type fact struct {
		id string
		at sql.NullTime
	}
	var facts []fact
	for rows.Next() {
		var f fact
		if err := rows.Scan(&f.id, &f.at); err != nil {
			_ = rows.Close()
			return err
		}
		f.id = strings.TrimPrefix(f.id, "x:author:")
		if validXAuthorID(f.id) && f.at.Valid {
			facts = append(facts, f)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, f := range facts {
		if err := s.preserveXAuthorFloor(ctx, exec, f.id, f.at.Time); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) markXAuthorProfileCleanupTx(ctx context.Context, tx *loggedTx, observationID int64, author string) error {
	protectedAt, publicAt, err := s.xAuthorProtectionTimesAcrossSourcesTx(ctx, tx, author)
	if err != nil || !protectedAt.Valid {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT u.observed_at,EXISTS(SELECT 1 FROM x_post_versions v WHERE v.source_id=u.source_id AND v.author_id=? AND (v.suppressed_at IS NULL OR v.cleanup_pending=1))
 FROM participant_contact_observations u WHERE u.provider_user_id=(SELECT provider_user_id FROM participant_contact_observations WHERE id=?)
 AND u.address_kind='username' AND u.source='archive_observation' AND u.source_id IN (SELECT id FROM sources WHERE source_type='x')`, author, observationID)
	if err != nil {
		return err
	}
	pending := false
	for rows.Next() {
		var at sql.NullTime
		var hasPost bool
		if err := rows.Scan(&at, &hasPost); err != nil {
			_ = rows.Close()
			return err
		}
		if (!at.Valid || !at.Time.After(protectedAt.Time)) && (!hasPost || (publicAt.Valid && publicAt.Time.After(protectedAt.Time))) {
			pending = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if !pending {
		return nil
	}
	var tokens string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(type_tokens,'') FROM participant_contact_observations WHERE id=?`, observationID).Scan(&tokens); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE participant_contact_observations SET type_tokens=? WHERE id=?`, joinTypeTokens(append(strings.Split(tokens, ","), xAuthorProfilePending)), observationID)
	return err
}
