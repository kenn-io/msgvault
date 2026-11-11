package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// Preserve uncertainty for whole edit chains, including aliases a snapshot
// knew before the latest version was discovered. Old aliases' default status
// cannot by itself make a currently verified chain uncertain.
func (s *Store) xRestoreTargetHeadTx(ctx context.Context, tx *loggedTx, sourceID int64, removed map[string]bool) (string, error) {
	h := sha256.New()
	_, _ = h.Write([]byte("msgvault-x-restore-targets-v1\n"))
	rows, err := tx.QueryContext(ctx, `SELECT p.post_id FROM x_post_versions p
 WHERE p.source_id=? AND p.suppressed_at IS NULL
 AND EXISTS (SELECT 1 FROM x_post_versions v
 WHERE v.source_id=p.source_id AND v.canonical_id=p.canonical_id
 AND v.suppressed_at IS NULL AND v.availability='unverified' AND `+xLatestRefreshAlias+`)
 ORDER BY LENGTH(p.post_id),p.post_id`, sourceID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return "", err
		}
		// Complete current removals will be replayed in private scratch. A
		// snapshot's older uncertainty for those IDs must not block that cleanup.
		if removed[id] {
			continue
		}
		if err := writeXRestoreFingerprint(h, id); err != nil {
			_ = rows.Close()
			return "", err
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
