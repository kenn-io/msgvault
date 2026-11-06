package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrYouTubeCaptionCursorChanged = errors.New("YouTube caption cursor changed concurrently")

// YouTubeCaptionRoot is one persisted video root eligible for caption polling.
type YouTubeCaptionRoot struct {
	MessageID int64
	VideoID   string
}

// MessageYouTubeCaptionAttachmentsContext includes stored versions and
// unavailable observations owned by the YouTube caption importer.
func (s *Store) MessageYouTubeCaptionAttachmentsContext(ctx context.Context, messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachmentsContext(ctx, messageID, "youtube:caption:")
}

// ClearYouTubeCaptionObservationContext removes only the importer's obsolete
// unavailable marker once a current transcript is stored.
func (s *Store) ClearYouTubeCaptionObservationContext(ctx context.Context, messageID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE message_id=? AND source_part_key='youtube:captions:status'`, messageID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows == 0 {
			return err
		}
		if err := recomputeMessageAttachmentStatsWith(tx, messageID); err != nil {
			return err
		}
		return s.bumpDerivedDataRevision(tx)
	})
}

func youtubeCaptionStateKey(sourceID int64) string {
	return "youtube.captions:" + strconv.FormatInt(sourceID, 10)
}

// YouTubeCaptionStateContext reads the source's independent caption cursor.
func (s *Store) YouTubeCaptionStateContext(ctx context.Context, sourceID int64) (string, error) {
	source, err := s.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return "", err
	}
	if source.SourceType != "youtube" {
		return "", errors.New("caption source must be YouTube")
	}
	var value string
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key=?`), youtubeCaptionStateKey(sourceID)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// CompareAndSwapYouTubeCaptionStateContext advances only the cursor read by
// this worker. Attachment writes precede this operation, so a crash replays
// an idempotent attachment instead of skipping it.
func (s *Store) CompareAndSwapYouTubeCaptionStateContext(ctx context.Context, sourceID int64, previous, next string) error {
	if _, err := s.YouTubeCaptionStateContext(ctx, sourceID); err != nil {
		return err
	}
	key := youtubeCaptionStateKey(sourceID)
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, `UPDATE archive_metadata SET value = ? WHERE key = ? AND value = ?`, next, key, previous)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 && previous == "" {
			result, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, key, next)
			if err != nil {
				return err
			}
			changed, err = result.RowsAffected()
			if err != nil {
				return err
			}
		}
		if changed != 1 {
			return ErrYouTubeCaptionCursorChanged
		}
		return nil
	})
}

// YouTubeCaptionRootBoundContext freezes a finite cycle despite new uploads.
func (s *Store) YouTubeCaptionRootBoundContext(ctx context.Context, sourceID int64) (int64, error) {
	var upper int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(MAX(m.id),0) FROM messages m
		WHERE m.source_id = ? AND m.source_message_id LIKE 'video:%'
		AND EXISTS (SELECT 1 FROM sources s WHERE s.id=m.source_id AND s.source_type='youtube')`), sourceID).Scan(&upper)
	return upper, err
}

// NextYouTubeCaptionRootContext uses persisted roots as the durable queue,
// including roots archived before caption opt-in or during partial syncs.
func (s *Store) NextYouTubeCaptionRootContext(ctx context.Context, sourceID, after, upper int64) (*YouTubeCaptionRoot, error) {
	root := &YouTubeCaptionRoot{}
	var external string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT m.id,m.source_message_id FROM messages m
		WHERE m.source_id=? AND m.id>? AND m.id<=? AND m.source_message_id LIKE 'video:%'
		AND EXISTS (SELECT 1 FROM sources s WHERE s.id=m.source_id AND s.source_type='youtube')
		ORDER BY m.id LIMIT 1`), sourceID, after, upper).Scan(&root.MessageID, &external)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // An exhausted caption queue has no root and no error.
	}
	if err != nil {
		return nil, fmt.Errorf("read YouTube caption root: %w", err)
	}
	root.VideoID = strings.TrimPrefix(external, "video:")
	return root, nil
}
