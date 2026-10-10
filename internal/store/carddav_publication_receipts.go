package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type cardDAVReceiptContextKey struct{}

type cardDAVReceiptBinding struct {
	db                *sql.DB
	principalHash     string
	keyHash           string
	postedRequestHash string
	personID          int64
}

// WithCardDAVPublicationReceipt binds a trusted native principal and opaque key
// to this Store. It grants no authority and does not prepare or dispatch work.
// Native callers must derive the principal from current authentication.
func (s *Store) WithCardDAVPublicationReceipt(ctx context.Context, principal, key string) (context.Context, error) {
	if strings.TrimSpace(principal) == "" || len(principal) > 256 || !utf8.ValidString(principal) || strings.TrimSpace(key) == "" || len(key) > 256 || !utf8.ValidString(key) {
		return nil, ErrCardDAVInvalidPlan
	}
	binding := cardDAVReceiptBinding{db: s.DB(), principalHash: cardDAVReceiptHash([]byte(principal)), keyHash: cardDAVReceiptHash([]byte(key))}
	return context.WithValue(ctx, cardDAVReceiptContextKey{}, binding), nil
}

// WithReviewedCardDAVPublicationReceipt binds the original posted target and
// approval token without granting authority. Use this context for reviewed
// preparation, admission, provider attempts, and receipt lookup.
func (s *Store) WithReviewedCardDAVPublicationReceipt(ctx context.Context, principal, key string, personID int64, token string) (context.Context, error) {
	if personID <= 0 || strings.TrimSpace(token) == "" || len(token) > 256 || !utf8.ValidString(token) {
		return nil, ErrCardDAVInvalidPlan
	}
	bound, err := s.WithCardDAVPublicationReceipt(ctx, principal, key)
	if err != nil {
		return nil, err
	}
	binding, _, err := s.cardDAVReceiptBinding(bound)
	if err != nil {
		return nil, err
	}
	binding.personID = personID
	binding.postedRequestHash = cardDAVPostedPublicationHash(personID, token)
	return context.WithValue(bound, cardDAVReceiptContextKey{}, binding), nil
}

func cardDAVPostedPublicationHash(personID int64, token string) string {
	return cardDAVReceiptHash(fmt.Appendf(nil, "reviewed-publication-v1:%d:%s", personID, cardDAVReceiptHash([]byte(token))))
}

func (s *Store) cardDAVReceiptBinding(ctx context.Context) (cardDAVReceiptBinding, bool, error) {
	binding, ok := ctx.Value(cardDAVReceiptContextKey{}).(cardDAVReceiptBinding)
	if !ok {
		return cardDAVReceiptBinding{}, false, nil
	}
	if binding.db != s.DB() {
		return cardDAVReceiptBinding{}, false, ErrCardDAVInvalidPlan
	}
	return binding, true, nil
}

// AdmitCardDAVPublicationReceiptContext commits exact intent evidence before
// provider work. It checks current native authority inside the same transaction
// as receipt admission. The existing service owns the person-operation lease.
func (s *Store) AdmitCardDAVPublicationReceiptContext(ctx context.Context, expected CardDAVPublication, authorize PersonEditAuthorizer) error {
	binding, bound, err := s.cardDAVReceiptBinding(ctx)
	if err != nil || !bound || authorize == nil || expected.PendingOperation != CardDAVMutationUpdate || expected.PendingIntentID == "" || expected.PersonID <= 0 || expected.AddressBookID <= 0 || expected.ConflictOwned || expected.ResolutionConflictID != 0 {
		return ErrCardDAVInvalidPlan
	}
	if binding.postedRequestHash != "" && binding.personID != expected.PersonID {
		return ErrCardDAVPublicationMismatch
	}
	digest, err := cardDAVReceiptIntentHash(expected)
	if err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var person IdentityPersonEvidence
		var book IdentityGrantAddressBook
		capture := func(ctx context.Context, scope *IdentityGrantSelection) error {
			if scope == nil || len(scope.Persons) != 1 || len(scope.AddressBooks) != 1 {
				return ErrCardDAVInvalidPlan
			}
			person, book = scope.Persons[0], scope.AddressBooks[0]
			return authorize(ctx, scope)
		}
		if err := s.authorizeCardDAVPublicationRequestTxContext(ctx, tx, expected, http.MethodPut, capture); err != nil {
			return err
		}
		bookFingerprint, err := cardDAVReceiptBookFingerprint(book)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO carddav_publication_receipts (receipt_id,principal_hash,idempotency_key_hash,request_hash,posted_request_hash,pending_intent_id,person_id,person_uid,person_revision,account_id,address_book_id,book_fingerprint,connection_generation,mutation_revision,body_sha256,state,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'dispatching',?) ON CONFLICT (principal_hash,idempotency_key_hash) DO NOTHING`, uuid.NewString(), binding.principalHash, binding.keyHash, digest, binding.postedRequestHash, expected.PendingIntentID, expected.PersonID, person.UID, person.Revision, book.AccountID, expected.AddressBookID, bookFingerprint, expected.ConnectionGeneration, expected.MutationRevision, cardDAVReceiptHash(expected.OutgoingBody), time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return fmt.Errorf("admit CardDAV publication receipt: %w", err)
		}
		var savedHash, savedPostedHash, state string
		if err := tx.QueryRowContext(ctx, `SELECT request_hash,posted_request_hash,state FROM carddav_publication_receipts WHERE principal_hash=? AND idempotency_key_hash=?`+s.dialect.SelectForUpdate(), binding.principalHash, binding.keyHash).Scan(&savedHash, &savedPostedHash, &state); err != nil {
			return fmt.Errorf("read admitted CardDAV publication receipt: %w", err)
		}
		if savedHash != digest || savedPostedHash != binding.postedRequestHash || state != "dispatching" {
			return ErrCardDAVPublicationMismatch
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count admitted CardDAV publication receipts: %w", err)
		}
		if affected != 1 {
			return ErrCardDAVPublicationPending
		}
		return nil
	})
}

// Owner recovery may observe an already dispatched update. Its original
// receipt must settle in the same transaction that clears the native intent.
// Read the binding from that exact persisted intent, never from caller input.
func (s *Store) cardDAVOwnerRecoveryReceiptTx(ctx context.Context, tx *loggedTx, pending CardDAVPublication) (*cardDAVReceiptBinding, error) {
	if pending.PendingIntentID == "" {
		return nil, ErrCardDAVPublicationReceiptNotFound
	}
	binding := &cardDAVReceiptBinding{db: s.DB()}
	var savedHash, state string
	err := tx.QueryRowContext(ctx, `SELECT principal_hash,idempotency_key_hash,request_hash,state FROM carddav_publication_receipts WHERE pending_intent_id=?`, pending.PendingIntentID).Scan(&binding.principalHash, &binding.keyHash, &savedHash, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVPublicationReceiptNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read CardDAV owner recovery receipt: %w", err)
	}
	digest, err := cardDAVReceiptIntentHash(pending)
	if err != nil {
		return nil, err
	}
	if pending.PendingOperation != CardDAVMutationUpdate || state != "dispatching" || savedHash != digest {
		return nil, ErrCardDAVPublicationPending
	}
	return binding, nil
}

func (s *Store) settleCardDAVPublicationReceiptTx(ctx context.Context, tx *loggedTx, binding *cardDAVReceiptBinding, pending CardDAVPublication, etag string) error {
	digest, err := cardDAVReceiptIntentHash(pending)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE carddav_publication_receipts SET state='verified',remote_etag=?,verified_at=? WHERE principal_hash=? AND idempotency_key_hash=? AND request_hash=? AND pending_intent_id=? AND state='dispatching'`, etag, time.Now().UTC().Format(time.RFC3339Nano), binding.principalHash, binding.keyHash, digest, pending.PendingIntentID)
	if err != nil {
		return fmt.Errorf("settle CardDAV publication receipt: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count settled CardDAV publication receipts: %w", err)
	}
	if affected != 1 {
		return ErrCardDAVPublicationMismatch
	}
	return nil
}

func cardDAVReceiptHash(value []byte) string { return fmt.Sprintf("%x", sha256.Sum256(value)) }

// Version 1 projects immutable native identity explicitly. Adding a field to
// CardDAVPublication must not change the digest of already admitted receipts.
func cardDAVReceiptIntentHash(pending CardDAVPublication) (string, error) {
	encoded, err := json.Marshal(struct {
		Version                   int
		PendingIntentID           string
		PersonID                  int64
		AddressBookID             int64
		Desired                   bool
		Href                      string
		Operation                 CardDAVMutationOperation
		MutationRevision          int64
		ConnectionGeneration      int64
		StartedAt                 *time.Time
		BodySHA256                string
		EnvelopeSHA256            string
		SemanticHash              string
		LocalHash                 string
		RemoteETag                string
		ApprovedBodySHA256        *string
		ApprovedInferenceRevision *int64
		ApprovedMutationRevision  *int64
	}{1, pending.PendingIntentID, pending.PersonID, pending.AddressBookID, pending.Desired, pending.Href, pending.PendingOperation, pending.MutationRevision, pending.ConnectionGeneration, pending.PendingStartedAt, cardDAVReceiptHash(pending.OutgoingBody), cardDAVReceiptHash(pending.OutgoingEnvelopeMetadata), pending.OutgoingSemanticHash, pending.LocalHash, pending.RemoteETag, pending.ApprovedBodySHA256, pending.ApprovedInferenceRevision, pending.ApprovedMutationRevision})
	if err != nil {
		return "", fmt.Errorf("hash CardDAV publication receipt intent: %w", err)
	}
	return cardDAVReceiptHash(encoded), nil
}

// Receipt-bound provider attempts require the exact independently committed
// admission, including GET-only recovery after a restart. Admission itself uses
// the native request validator before inserting this evidence.
func (s *Store) authorizeCardDAVReceiptRequestTx(ctx context.Context, tx *loggedTx, binding cardDAVReceiptBinding, expected CardDAVPublication) error {
	digest, err := cardDAVReceiptIntentHash(expected)
	if err != nil {
		return err
	}
	var savedHash, savedPostedHash, intent, state string
	err = tx.QueryRowContext(ctx, `SELECT request_hash,posted_request_hash,pending_intent_id,state FROM carddav_publication_receipts WHERE principal_hash=? AND idempotency_key_hash=?`+s.dialect.SelectForUpdate(), binding.principalHash, binding.keyHash).Scan(&savedHash, &savedPostedHash, &intent, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCardDAVPublicationReceiptNotFound
	}
	if err != nil {
		return fmt.Errorf("authorize CardDAV publication receipt request: %w", err)
	}
	if (binding.postedRequestHash != "" && savedPostedHash != binding.postedRequestHash) || savedHash != digest || intent != expected.PendingIntentID || state != "dispatching" {
		return ErrCardDAVPublicationMismatch
	}
	return nil
}

// No-op receipts commit with native approval and envelope updates. They have no
// pending provider nonce, and therefore can never admit a provider attempt.
func (s *Store) commitCardDAVNoopReceiptTx(ctx context.Context, tx *loggedTx, binding cardDAVReceiptBinding, plan CardDAVPublicationPlan, review *CardDAVReviewedPublicationPlan, prepared *CardDAVPublication, authorize PersonEditAuthorizer) error {
	if review == nil || authorize == nil || !plan.Desired || !prepared.Noop || prepared.PendingIntentID != "" || review.Fence.RemoteETag == nil {
		return ErrCardDAVInvalidPlan
	}
	scope, err := s.identityGrantSelectionTx(ctx, tx, []int64{plan.PersonID}, []int64{plan.AddressBookID})
	if err != nil {
		return err
	}
	person, book := scope.Persons[0], scope.AddressBooks[0]
	if err := authorize(ctx, scope); err != nil {
		return err
	}
	bookFingerprint, err := cardDAVReceiptBookFingerprint(book)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(struct {
		Version        int
		Noop           bool
		PersonID       int64
		AddressBookID  int64
		ApprovalToken  string
		BodySHA256     string
		EnvelopeSHA256 string
	}{1, true, plan.PersonID, plan.AddressBookID, review.ApprovalToken, cardDAVReceiptHash(plan.OutgoingBody), cardDAVReceiptHash(plan.OutgoingEnvelopeMetadata)})
	if err != nil {
		return fmt.Errorf("hash CardDAV no-op receipt: %w", err)
	}
	digest := cardDAVReceiptHash(encoded)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO carddav_publication_receipts (receipt_id,principal_hash,idempotency_key_hash,request_hash,posted_request_hash,pending_intent_id,is_noop,person_id,person_uid,person_revision,account_id,address_book_id,book_fingerprint,connection_generation,mutation_revision,body_sha256,state,remote_etag,created_at,verified_at) VALUES (?,?,?,?,?,NULL,TRUE,?,?,?,?,?,?,?,?,?,'verified',?,?,?) ON CONFLICT (principal_hash,idempotency_key_hash) DO NOTHING`, uuid.NewString(), binding.principalHash, binding.keyHash, digest, binding.postedRequestHash, plan.PersonID, person.UID, person.Revision, book.AccountID, plan.AddressBookID, bookFingerprint, review.Fence.ConnectionGeneration, prepared.MutationRevision, cardDAVReceiptHash(plan.OutgoingBody), *review.Fence.RemoteETag, now, now)
	if err != nil {
		return fmt.Errorf("record CardDAV no-op receipt: %w", err)
	}
	var savedHash, savedPostedHash, state string
	var noop bool
	if err := tx.QueryRowContext(ctx, `SELECT request_hash,posted_request_hash,state,is_noop FROM carddav_publication_receipts WHERE principal_hash=? AND idempotency_key_hash=?`+s.dialect.SelectForUpdate(), binding.principalHash, binding.keyHash).Scan(&savedHash, &savedPostedHash, &state, &noop); err != nil {
		return fmt.Errorf("read CardDAV no-op receipt: %w", err)
	}
	if savedHash != digest || savedPostedHash != binding.postedRequestHash || state != "verified" || !noop {
		return ErrCardDAVPublicationMismatch
	}
	return nil
}

// Rejection and native intent removal share a transaction. Failed persistence
// retains the dispatch receipt and native evidence for observation-only recovery.
func (s *Store) rejectCardDAVPublicationReceiptTx(ctx context.Context, tx *loggedTx, binding cardDAVReceiptBinding, pending CardDAVPublication, throttled bool, retryAfter *time.Time) error {
	digest, err := cardDAVReceiptIntentHash(pending)
	if err != nil {
		return err
	}
	code := "provider_rejected"
	var deadline any
	if throttled {
		code = "retry_after"
		if retryAfter == nil {
			return ErrCardDAVInvalidPlan
		}
		deadline = retryAfter.UTC().Format(time.RFC3339Nano)
	}
	result, err := tx.ExecContext(ctx, `UPDATE carddav_publication_receipts SET state='rejected',error_code=?,retry_after=? WHERE principal_hash=? AND idempotency_key_hash=? AND request_hash=? AND pending_intent_id=? AND state='dispatching'`, code, deadline, binding.principalHash, binding.keyHash, digest, pending.PendingIntentID)
	if err != nil {
		return fmt.Errorf("reject CardDAV publication receipt: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count rejected CardDAV publication receipts: %w", err)
	}
	if affected != 1 {
		return ErrCardDAVPublicationMismatch
	}
	return nil
}
