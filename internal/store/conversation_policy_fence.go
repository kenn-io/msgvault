package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

type ConversationPolicyTx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
type ConversationPolicyLockMode uint8

const (
	ConversationPolicyShared ConversationPolicyLockMode = iota
	ConversationPolicyExclusive
)

func (s *Store) initializeConversationPolicyFence(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return err
	}
	var main string
	for rows.Next() {
		var seq int
		var name, file string
		if err = rows.Scan(&seq, &name, &file); err != nil {
			break
		}
		if name == "main" {
			main = file
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	if main == "" {
		return nil
	}
	path, err := filepath.Abs(main)
	if err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("resolve conversation policy archive fence: %w", err)
	}
	s.conversationPolicyLockPath = path + ".conversation-policy.lock"
	return nil
}

// AcquireSQLiteConversationPolicyFence must precede cache and SQL locks. Each
// disk acquisition owns a separate open-file description until release.
func (s *Store) AcquireSQLiteConversationPolicyFence(ctx context.Context, mode ConversationPolicyLockMode) (func(), error) {
	if s.IsPostgreSQL() {
		return nil, errors.New("SQLite conversation policy fence requested for PostgreSQL")
	}
	if mode != ConversationPolicyShared && mode != ConversationPolicyExclusive {
		return nil, errors.New("invalid conversation policy lock mode")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := s.withoutSyncScope()
	var release func()
	if root.conversationPolicyLockPath != "" {
		lock := flock.New(root.conversationPolicyLockPath)
		var ok bool
		var err error
		if mode == ConversationPolicyShared {
			ok, err = lock.TryRLockContext(ctx, 50*time.Millisecond)
		} else {
			ok, err = lock.TryLockContext(ctx, 50*time.Millisecond)
		}
		if err != nil || !ok {
			_ = lock.Close()
			if err == nil {
				err = ctx.Err()
			}
			if err == nil {
				err = errors.New("fence not acquired")
			}
			return nil, fmt.Errorf("acquire conversation policy fence: %w", err)
		}
		release = func() { _ = lock.Unlock(); _ = lock.Close() }
	} else {
		for {
			var ok bool
			if mode == ConversationPolicyShared {
				ok = root.conversationPolicyMu.TryRLock()
			} else {
				ok = root.conversationPolicyMu.TryLock()
			}
			if ok {
				break
			}
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		if mode == ConversationPolicyShared {
			release = root.conversationPolicyMu.RUnlock
		} else {
			release = root.conversationPolicyMu.Unlock
		}
	}
	var once sync.Once
	return func() { once.Do(release) }, nil
}

// LockPostgresConversationPolicyTx uses the caller's connection and transaction.
func (s *Store) LockPostgresConversationPolicyTx(ctx context.Context, tx ConversationPolicyTx, mode ConversationPolicyLockMode) error {
	if !s.IsPostgreSQL() {
		return errors.New("PostgreSQL conversation policy fence requested for SQLite")
	}
	name := "pg_advisory_xact_lock_shared"
	if mode == ConversationPolicyExclusive {
		name = "pg_advisory_xact_lock"
	} else if mode != ConversationPolicyShared {
		return errors.New("invalid conversation policy lock mode")
	}
	_, err := tx.ExecContext(ctx, `SELECT `+name+`(hashtextextended('msgvault.conversation_policy', 0))`)
	return err
}

func (s *Store) withConversationPolicyWrite(ctx context.Context, fn func(*loggedTx) error) error {
	if !s.IsPostgreSQL() {
		release, err := s.AcquireSQLiteConversationPolicyFence(ctx, ConversationPolicyExclusive)
		if err != nil {
			return err
		}
		defer release()
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error {
		if s.IsPostgreSQL() {
			if err := s.LockPostgresConversationPolicyTx(ctx, tx.Tx, ConversationPolicyExclusive); err != nil {
				return err
			}
		}
		return s.lockIdentityMutationTxContext(ctx, tx)
	}, fn)
}
