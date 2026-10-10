package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrCardDAVPublicationReceiptNotFound = errors.New("CardDAV publication receipt not found")

// CardDAVPublicationReceipt retains one exact reviewed intent and its outcome.
// It contains native identifiers and digests, without contact bodies or keys.
type CardDAVPublicationReceipt struct {
	ID                   string     `json:"id"`
	State                string     `json:"state"`
	Noop                 bool       `json:"noop"`
	PersonID             int64      `json:"person_id"`
	PersonUID            string     `json:"person_uid"`
	PersonRevision       int64      `json:"person_revision"`
	AccountID            int64      `json:"account_id"`
	AddressBookID        int64      `json:"address_book_id"`
	ConnectionGeneration int64      `json:"connection_generation"`
	MutationRevision     int64      `json:"mutation_revision"`
	BodySHA256           string     `json:"body_sha256"`
	RemoteETag           string     `json:"remote_etag"`
	ErrorCode            string     `json:"error_code,omitzero"`
	RetryAfter           *time.Time `json:"retry_after,omitzero"`
	CreatedAt            time.Time  `json:"created_at"`
	VerifiedAt           *time.Time `json:"verified_at,omitzero"`
}

// CardDAVPublicationReceiptContext reads the saved principal/key outcome with
// current native resource identities and authorization. Later provider state,
// person revisions and preview expiry do not invalidate historical receipts.
// Native HTTP callers derive principal from current authentication. The trusted
// authorizer must not open another Store transaction inside this read snapshot.
func (s *Store) CardDAVPublicationReceiptContext(ctx context.Context, principal, key string, authorize PersonEditAuthorizer) (*CardDAVPublicationReceipt, error) {
	if authorize == nil {
		return nil, ErrCardDAVInvalidPlan
	}
	bound, err := s.WithCardDAVPublicationReceipt(ctx, principal, key)
	if err != nil {
		return nil, err
	}
	binding, ok, err := s.cardDAVReceiptBinding(bound)
	if err != nil || !ok {
		return nil, ErrCardDAVInvalidPlan
	}
	return s.cardDAVPublicationReceiptWithBindingContext(ctx, binding, authorize)
}

// ReviewedCardDAVPublicationReceiptContext matches the original posted request
// before preview or provider work. Current resource authority is still required.
func (s *Store) ReviewedCardDAVPublicationReceiptContext(ctx context.Context, authorize PersonEditAuthorizer) (*CardDAVPublicationReceipt, error) {
	binding, bound, err := s.cardDAVReceiptBinding(ctx)
	if err != nil || !bound || binding.postedRequestHash == "" || authorize == nil {
		return nil, ErrCardDAVInvalidPlan
	}
	return s.cardDAVPublicationReceiptWithBindingContext(ctx, binding, authorize)
}

func (s *Store) cardDAVPublicationReceiptWithBindingContext(ctx context.Context, binding cardDAVReceiptBinding, authorize PersonEditAuthorizer) (*CardDAVPublicationReceipt, error) {
	var saved *cardDAVStoredPublicationReceipt
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		saved, err = s.readCardDAVPublicationReceiptTx(ctx, tx, "principal_hash=? AND idempotency_key_hash=?", binding.principalHash, binding.keyHash)
		if err != nil {
			return err
		}
		return s.authorizeCardDAVStoredReceiptTx(ctx, tx, binding, saved, authorize)
	})
	if err != nil {
		return nil, err
	}
	return &saved.value, nil
}

func (s *Store) authorizeCardDAVStoredReceiptTx(ctx context.Context, tx *loggedTx, binding cardDAVReceiptBinding, saved *cardDAVStoredPublicationReceipt, authorize PersonEditAuthorizer) error {
	if binding.postedRequestHash != "" && (saved.postedRequestHash != binding.postedRequestHash || saved.value.PersonID != binding.personID) {
		return ErrCardDAVPublicationMismatch
	}
	scope, err := s.identityGrantSelectionTx(ctx, tx, []int64{saved.value.PersonID}, []int64{saved.value.AddressBookID})
	if err != nil {
		return err
	}
	currentFingerprint, err := cardDAVReceiptBookFingerprint(scope.AddressBooks[0])
	if err != nil {
		return err
	}
	if scope.Persons[0].UID != saved.value.PersonUID || scope.AddressBooks[0].AccountID != saved.value.AccountID || currentFingerprint != saved.bookFingerprint {
		return ErrCardDAVStalePlan
	}
	return authorize(ctx, scope)
}

// ReviewedCardDAVPublicationRecoveryContext selects the original receipt and
// exact native pending update in one currently authorized read snapshot.
// Settled receipts need no recovery. Selection performs no provider work and
// grants no authority; callers must recheck authority at each provider attempt.
func (s *Store) ReviewedCardDAVPublicationRecoveryContext(ctx context.Context, authorize PersonEditAuthorizer) (*CardDAVPublicationReceipt, *CardDAVPublication, error) {
	binding, bound, err := s.cardDAVReceiptBinding(ctx)
	if err != nil || !bound || binding.postedRequestHash == "" || authorize == nil {
		return nil, nil, ErrCardDAVInvalidPlan
	}
	var saved *cardDAVStoredPublicationReceipt
	var pending *CardDAVPublication
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		saved, err = s.readCardDAVPublicationReceiptTx(ctx, tx, "principal_hash=? AND idempotency_key_hash=?", binding.principalHash, binding.keyHash)
		if err != nil {
			return err
		}
		if err := s.authorizeCardDAVStoredReceiptTx(ctx, tx, binding, saved, authorize); err != nil {
			return err
		}
		if saved.value.State != "dispatching" {
			return nil
		}
		pending, err = getCardDAVPublicationFrom(ctx, tx, saved.value.PersonID, "")
		if errors.Is(err, ErrCardDAVPublicationNotFound) {
			return ErrCardDAVPublicationMismatch
		}
		if err != nil {
			return err
		}
		digest, err := cardDAVReceiptIntentHash(*pending)
		if err != nil {
			return err
		}
		if saved.value.Noop || pending.PendingOperation != CardDAVMutationUpdate || pending.ConflictOwned || pending.ResolutionConflictID != 0 || pending.PendingIntentID == "" || pending.PendingIntentID != saved.pendingIntentID || digest != saved.requestHash || pending.AddressBookID != saved.value.AddressBookID || pending.ConnectionGeneration != saved.value.ConnectionGeneration || pending.MutationRevision != saved.value.MutationRevision || cardDAVReceiptHash(pending.OutgoingBody) != saved.value.BodySHA256 {
			return ErrCardDAVPublicationMismatch
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return &saved.value, pending, nil
}

// CardDAVPublicationReceiptByIDContext is a trusted current-owner historical
// read. Native HTTP admission must reject delegates and recheck current owner
// authority through authorize inside this read snapshot. Resource cleanup does
// not destroy receipts or require recreating native resources. No work replays.
func (s *Store) CardDAVPublicationReceiptByIDContext(ctx context.Context, id string, authorize func(context.Context) error) (*CardDAVPublicationReceipt, error) {
	if authorize == nil || strings.TrimSpace(id) == "" || len(id) > 256 || !utf8.ValidString(id) {
		return nil, ErrCardDAVInvalidPlan
	}
	var saved *cardDAVStoredPublicationReceipt
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		saved, err = s.readCardDAVPublicationReceiptTx(ctx, tx, "receipt_id=?", id)
		if err != nil {
			return err
		}
		return authorize(ctx)
	})
	if err != nil {
		return nil, err
	}
	return &saved.value, nil
}

type cardDAVStoredPublicationReceipt struct {
	value             CardDAVPublicationReceipt
	bookFingerprint   string
	postedRequestHash string
	requestHash       string
	pendingIntentID   string
}

func (s *Store) readCardDAVPublicationReceiptTx(ctx context.Context, tx *loggedTx, selector string, args ...any) (*cardDAVStoredPublicationReceipt, error) {
	var saved cardDAVStoredPublicationReceipt
	receipt := &saved.value
	var created string
	var verified, retryAfter, pendingID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT receipt_id,state,is_noop,person_id,person_uid,person_revision,account_id,address_book_id,book_fingerprint,posted_request_hash,request_hash,pending_intent_id,connection_generation,mutation_revision,body_sha256,remote_etag,error_code,retry_after,created_at,verified_at FROM carddav_publication_receipts WHERE `+selector, args...).Scan(&receipt.ID, &receipt.State, &receipt.Noop, &receipt.PersonID, &receipt.PersonUID, &receipt.PersonRevision, &receipt.AccountID, &receipt.AddressBookID, &saved.bookFingerprint, &saved.postedRequestHash, &saved.requestHash, &pendingID, &receipt.ConnectionGeneration, &receipt.MutationRevision, &receipt.BodySHA256, &receipt.RemoteETag, &receipt.ErrorCode, &retryAfter, &created, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVPublicationReceiptNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read CardDAV publication receipt: %w", err)
	}
	if pendingID.Valid {
		saved.pendingIntentID = pendingID.String
	}
	receipt.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return nil, fmt.Errorf("read CardDAV publication receipt creation time: %w", err)
	}
	if verified.Valid {
		when, err := time.Parse(time.RFC3339Nano, verified.String)
		if err != nil {
			return nil, fmt.Errorf("read CardDAV publication receipt verification time: %w", err)
		}
		receipt.VerifiedAt = &when
	}
	if retryAfter.Valid {
		deadline, err := time.Parse(time.RFC3339Nano, retryAfter.String)
		if err != nil {
			return nil, fmt.Errorf("read CardDAV publication receipt retry deadline: %w", err)
		}
		receipt.RetryAfter = &deadline
	}
	return &saved, nil
}

func cardDAVReceiptBookFingerprint(book IdentityGrantAddressBook) (string, error) {
	encoded, err := json.Marshal(struct {
		Version              int
		AccountID            int64
		BookID               int64
		CanonicalURL         string
		OwnershipFingerprint string
	}{1, book.AccountID, book.BookID, book.CanonicalURL, book.OwnershipFingerprint})
	if err != nil {
		return "", fmt.Errorf("hash CardDAV receipt book identity: %w", err)
	}
	return cardDAVReceiptHash(encoded), nil
}
