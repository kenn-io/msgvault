package store

import (
	"context"
	"database/sql"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

type conversationPolicyWorkKey struct{}

type conversationPolicyWork struct {
	store      *Store
	revision   int64
	messageIDs []int64
}

// CaptureConversationPolicyWork binds a work item to its original policy
// revision. Keep the returned context through requests, retries and publication.
// Reusing a work context never replaces its original revision.
func (s *Store) CaptureConversationPolicyWork(ctx context.Context) (context.Context, error) {
	if _, ok := ctx.Value(conversationPolicyWorkKey{}).(conversationPolicyWork); ok {
		return ctx, nil
	}
	revision, err := s.ConversationPolicyRevision(ctx)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, conversationPolicyWorkKey{}, conversationPolicyWork{store: s, revision: revision}), nil
}

// WithConversationPolicyMessageIDs binds the messages used by one provider
// request to captured work. Admission checks revalidate both the original
// policy revision and each message's current Full eligibility before the
// request, including retries that reuse this context.
func WithConversationPolicyMessageIDs(ctx context.Context, ids []int64) context.Context {
	work, ok := ctx.Value(conversationPolicyWorkKey{}).(conversationPolicyWork)
	if !ok {
		return ctx
	}
	seen := make(map[int64]struct{}, len(ids))
	work.messageIDs = make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		work.messageIDs = append(work.messageIDs, id)
	}
	return context.WithValue(ctx, conversationPolicyWorkKey{}, work)
}

// WithConversationPolicyWorkFrom carries already captured authority into a new
// caller context without replacing its cancellation or deadline. If both
// contexts carry authority, retain the earlier revision; never refresh old work.
func WithConversationPolicyWorkFrom(ctx, original context.Context) context.Context {
	if original != nil {
		if work, ok := original.Value(conversationPolicyWorkKey{}).(conversationPolicyWork); ok {
			if current, ok := ctx.Value(conversationPolicyWorkKey{}).(conversationPolicyWork); ok && current.revision <= work.revision {
				return ctx
			}
			return context.WithValue(ctx, conversationPolicyWorkKey{}, work)
		}
	}
	return ctx
}

// CheckConversationPolicyAdmission is a short admission check. It releases
// all fences and connections before returning to the provider caller.
func CheckConversationPolicyAdmission(ctx context.Context) error {
	work, ok := ctx.Value(conversationPolicyWorkKey{}).(conversationPolicyWork)
	if !ok {
		return nil
	}
	if _, ok := ctx.Value(personSweepPolicyKey{}).(personSweepPolicyWork); ok {
		return work.store.withConversationPolicyAdmissionLogged(ctx, func(*loggedTx) error { return nil })
	}
	if media, ok := ctx.Value(conversationPolicyMediaKey{}).(conversationPolicyMedia); ok {
		return work.store.withConversationPolicyAdmission(ctx, func(tx *sql.Tx) error {
			if media.visual {
				return work.store.CheckConversationPolicyVisualTokensTx(ctx, tx, media.tokens)
			}
			return work.store.CheckConversationPolicyDocumentTokensTx(ctx, tx, media.generationID, media.tokens)
		})
	}
	if persons, ok := ctx.Value(conversationPolicyPeopleKey{}).([]ConversationPolicyPersonVersion); ok {
		return work.store.withConversationPolicyAdmission(ctx, func(tx *sql.Tx) error {
			return work.store.CheckConversationPolicyPeopleTx(ctx, tx, work.revision, persons)
		})
	}
	return work.store.CheckConversationPolicyWork(ctx, work.revision, work.messageIDs)
}

// CheckConversationPolicyPublicationTx uses only the caller's connection.
// The caller must already own the policy fence for this transaction.
func (s *Store) CheckConversationPolicyPublicationTx(ctx context.Context, tx ConversationPolicyTx, ids []int64) error {
	if work, ok := ctx.Value(conversationPolicyWorkKey{}).(conversationPolicyWork); ok {
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key='conversation_policy_revision'`).Scan(&revision); err != nil {
			return err
		}
		if revision != work.revision {
			return ErrConversationPolicyStale
		}
	}
	return s.CheckConversationPolicyMessagesTx(ctx, tx, ids)
}

// CheckSQLiteConversationPolicyPublication checks without reacquiring the
// file fence. No SQL resource stays open when it returns.
func (s *Store) CheckSQLiteConversationPolicyPublication(ctx context.Context, ids []int64) error {
	return s.CheckConversationPolicyPublicationTx(ctx, s.DB(), ids)
}

func (s *Store) withConversationPolicyPublication(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.withConversationPolicyPublicationLogged(ctx, func(tx *loggedTx) error { return fn(tx.Tx) })
}

func (s *Store) withConversationPolicyPublicationLogged(ctx context.Context, fn func(*loggedTx) error) error {
	return s.withConversationPolicyTransaction(ctx, nil, fn)
}

func (s *Store) withConversationPolicyAdmission(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.withConversationPolicyAdmissionLogged(ctx, func(tx *loggedTx) error { return fn(tx.Tx) })
}

func (s *Store) withConversationPolicyAdmissionLogged(ctx context.Context, fn func(*loggedTx) error) error {
	opts := &sql.TxOptions{ReadOnly: true}
	if _, ok := ctx.Value(personSweepPolicyKey{}).(personSweepPolicyWork); ok {
		// Sweep admission takes identity and cited-evidence row locks on PostgreSQL.
		// Those reads need a short read/write transaction to acquire the locks.
		opts = nil
	}
	if refs, _ := ctx.Value(personSweepPublicationRefsKey{}).([]peoplesweep.EvidenceRef); len(refs) > 0 {
		opts = nil
	}
	return s.withConversationPolicyTransaction(ctx, opts, fn)
}

func (s *Store) withConversationPolicyTransaction(ctx context.Context, opts *sql.TxOptions, fn func(*loggedTx) error) error {
	if !s.IsPostgreSQL() {
		release, err := s.AcquireSQLiteConversationPolicyFence(ctx, ConversationPolicyShared)
		if err != nil {
			return err
		}
		defer release()
	}
	return s.withTxLockedContext(ctx, opts, func(tx *loggedTx) error {
		if s.IsPostgreSQL() {
			if err := s.LockPostgresConversationPolicyTx(ctx, tx.Tx, ConversationPolicyShared); err != nil {
				return err
			}
		}
		_, sweep := ctx.Value(personSweepPolicyKey{}).(personSweepPolicyWork)
		refs, _ := ctx.Value(personSweepPublicationRefsKey{}).([]peoplesweep.EvidenceRef)
		if sweep || len(refs) > 0 {
			// Match attribution's identity-before-sync-generation ordering.
			return s.shareIdentityLockTxContext(ctx, tx)
		}
		return nil
	}, func(tx *loggedTx) error {
		if err := s.lockPersonSweepPolicyInputsTx(ctx, tx); err != nil {
			return err
		}
		if err := s.CheckConversationPolicyPublicationTx(ctx, tx.Tx, nil); err != nil {
			return err
		}
		if err := s.checkPersonSweepPolicyInputsTx(ctx, tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

// PublishConversationPolicyProgress writes generation-side progress under the
// original work revision. The work context must be captured before discovery.
func PublishConversationPolicyProgress(ctx context.Context, db *sql.DB, write func(*sql.Tx) error) error {
	var s *Store
	if work, ok := ctx.Value(conversationPolicyWorkKey{}).(conversationPolicyWork); ok {
		s = work.store
	}
	return s.PublishConversationPolicyProgress(ctx, db, write)
}

// PublishConversationPolicyProgress serializes progress with policy reset on
// the actual generation database. A nil Store supports archive-free fixtures.
func (s *Store) PublishConversationPolicyProgress(ctx context.Context, db *sql.DB, write func(*sql.Tx) error) error {
	if s != nil && !s.IsPostgreSQL() {
		release, err := s.AcquireSQLiteConversationPolicyFence(ctx, ConversationPolicyShared)
		if err != nil {
			return err
		}
		defer release()
		if err := s.CheckSQLiteConversationPolicyPublication(ctx, nil); err != nil {
			return err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if s != nil && s.IsPostgreSQL() {
		if err := s.LockPostgresConversationPolicyTx(ctx, tx, ConversationPolicyShared); err != nil {
			return err
		}
		if err := s.CheckConversationPolicyPublicationTx(ctx, tx, nil); err != nil {
			return err
		}
	}
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit()
}
