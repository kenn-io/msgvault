package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

const emailPushIMAPSourceType = "imap"

func emailPushBindingKey(sourceID int64) string {
	return "email_push.incarnation:" + strconv.FormatInt(sourceID, 10)
}

// BindEmailPushSource atomically reads an active email source and its durable
// incarnation token, creating the token lazily. The source write lock fences
// concurrent removal across processes on SQLite and PostgreSQL. Ordinary sync
// never rotates this token; removal/retirement invalidates it in its own tx.
// No listener may install a token based on a source read before this lock.
func (s *Store) BindEmailPushSource(ctx context.Context, sourceID int64) (*Source, string, error) {
	var source *Source
	var token string
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		// SQLite's UPDATE acquires the writer lock; PostgreSQL locks the exact
		// source row, with the existing sync/maintenance lock order.
		if err := s.lockSyncSourceTx(ctx, tx, sourceID); err != nil {
			return err
		}
		var err error
		source, err = scanSource(tx.QueryRowContext(ctx, `SELECT `+sourceCatalogColumns+` FROM sources WHERE id = ?`, sourceID))
		if err != nil {
			return err
		}
		if (source.SourceType != emailPushIMAPSourceType && source.SourceType != "gmail") || source.HistoryOnly || source.MergedIntoSourceID != 0 {
			return errors.New("email push requires an active email source")
		}
		key := emailPushBindingKey(sourceID)
		err = tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, key).Scan(&token)
		if errors.Is(err, sql.ErrNoRows) {
			var random [32]byte
			if _, err := rand.Read(random[:]); err != nil {
				return fmt.Errorf("generate email push source binding: %w", err)
			}
			token = hex.EncodeToString(random[:])
			_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata (key, value) VALUES (?, ?)`, key, token)
		}
		if err != nil {
			return fmt.Errorf("install email push source binding: %w", err)
		}
		if token == "" {
			return errors.New("email push source binding is empty")
		}
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("bind email push source: %w", err)
	}
	return source, token, nil
}

// EmailPushBoundSource reads the source and token from one database snapshot.
// A missing token fails closed: only explicit listener startup may bind again.
func (s *Store) EmailPushBoundSource(ctx context.Context, sourceID int64, token string) (*Source, error) {
	if token == "" {
		return nil, errors.New("email push source binding is missing")
	}
	source, err := scanSource(s.db.QueryRowContext(ctx, `SELECT `+sourceCatalogColumns+`
 FROM sources WHERE id = ? AND EXISTS (
 SELECT 1 FROM archive_metadata WHERE key = ? AND value = ?
 )`, sourceID, emailPushBindingKey(sourceID), token))
	if err != nil {
		return nil, fmt.Errorf("read email push source binding: %w", err)
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return nil, ErrSourceRetired
	}
	return source, nil
}

func (s *Store) invalidateEmailPushBinding(ctx context.Context, q contextStatementQuerier, sourceID int64) error {
	if _, err := q.ExecContext(ctx, s.Rebind(`DELETE FROM archive_metadata WHERE key = ?`), emailPushBindingKey(sourceID)); err != nil {
		return fmt.Errorf("invalidate email push source binding: %w", err)
	}
	return nil
}
