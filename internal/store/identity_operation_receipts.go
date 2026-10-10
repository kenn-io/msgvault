package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identitycontrol"
)

var (
	ErrIdentityOperationIdempotency     = errors.New("identity operation key already used for a different request")
	ErrIdentityOperationStale           = errors.New("identity operation evidence changed; preview again")
	ErrIdentityOperationBlocked         = errors.New("identity operation blocked; use the existing identity or publication recovery")
	ErrIdentityOperationReceiptNotFound = errors.New("identity operation receipt not found")
	errIdentityOperationReplay          = errors.New("identity operation receipt already committed")
)

type IdentityOperationRequest struct {
	Principal           string                         `json:"principal"`
	IdempotencyKey      string                         `json:"idempotency_key"`
	Operation           identitycontrol.Operation      `json:"operation"`
	Target              identitycontrol.IdentityTarget `json:"target"`
	ExpectedFingerprint string                         `json:"expected_fingerprint"`
}

// IdentityOperationReceiptContext reads one exact principal/key outcome without
// replaying a write or requiring an unexpired preview. The native service must
// derive principal from current authentication and enforce receipt read scope.
func (s *Store) IdentityOperationReceiptContext(ctx context.Context, principal, key string) (*IdentityReceipt, error) {
	if strings.TrimSpace(principal) == "" || len(principal) > 256 || strings.TrimSpace(key) == "" || len(key) > 256 {
		return nil, fmt.Errorf("%w: bounded principal and receipt key required", identitycontrol.ErrInvalidRequest)
	}
	return s.identityReceiptReadContext(ctx, `SELECT receipt_json FROM identity_operation_receipts WHERE principal=? AND idempotency_key=?`, principal, key)
}

// IdentityReceiptByIDContext is the trusted owner's reconciliation primitive.
// Native HTTP admission must require current owner authority; this method does
// not issue a grant or replay the recorded operation.
func (s *Store) IdentityReceiptByIDContext(ctx context.Context, id string) (*IdentityReceipt, error) {
	if strings.TrimSpace(id) == "" || len(id) > 256 {
		return nil, fmt.Errorf("%w: bounded receipt ID required", identitycontrol.ErrInvalidRequest)
	}
	return s.identityReceiptReadContext(ctx, `SELECT receipt_json FROM identity_operation_receipts WHERE receipt_id=?`, id)
}

func (s *Store) identityReceiptReadContext(ctx context.Context, query string, args ...any) (*IdentityReceipt, error) {
	var receipt IdentityReceipt
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var encoded string
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&encoded); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrIdentityOperationReceiptNotFound
			}
			return fmt.Errorf("read identity receipt: %w", err)
		}
		if err := json.Unmarshal([]byte(encoded), &receipt); err != nil {
			return fmt.Errorf("decode identity receipt: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

// IdentityReceipt records one atomic archive identity outcome. It retains exact
// identifiers and digests, without storing names, contact bodies or grant keys.
type IdentityReceipt struct {
	ID                string                         `json:"id"`
	Operation         identitycontrol.Operation      `json:"operation"`
	Target            identitycontrol.IdentityTarget `json:"target"`
	BeforeFingerprint string                         `json:"before_fingerprint"`
	AfterFingerprint  string                         `json:"after_fingerprint"`
	BeforeRevision    int64                          `json:"before_revision"`
	AfterRevision     int64                          `json:"after_revision"`
	Changed           bool                           `json:"changed"`
	Participants      []int64                        `json:"participants"`
	Persons           []int64                        `json:"persons"`
	Edge              *IdentityReceiptEdgeChange     `json:"edge,omitzero"`
	CreatedAt         time.Time                      `json:"created_at"`
}

// IdentityReceiptEdgeChange distinguishes a new or removed edge from a manual
// confirmation that preserves the edge while clearing automated ownership.
type IdentityReceiptEdgeChange struct {
	BeforePresent     bool  `json:"before_present"`
	AfterPresent      bool  `json:"after_present"`
	BeforeCandidateID int64 `json:"before_candidate_id,omitzero"`
	AfterCandidateID  int64 `json:"after_candidate_id,omitzero"`
}

// ApplyIdentityOperationContext uses the same native transaction and identity
// fence for scope authorization, evidence checking, mutation and receipt. The
// trusted native caller supplies authorization; HTTP/MCP never omit it.
func (s *Store) ApplyIdentityOperationContext(ctx context.Context, request IdentityOperationRequest, authorize func(context.Context, *IdentitySnapshot) error) (*IdentityReceipt, error) {
	return s.applyIdentityOperationContext(ctx, request, authorize, nil)
}

// ApplyIdentityOperationWithPreviewContext separates current scope admission
// from fresh-write preview verification. Both run under the identity fence.
// An exact committed retry still needs current admission, but does not require
// a live preview and never replays a mutation.
func (s *Store) ApplyIdentityOperationWithPreviewContext(ctx context.Context, request IdentityOperationRequest, authorize, verifyPreview func(context.Context, *IdentitySnapshot) error) (*IdentityReceipt, error) {
	if authorize == nil || verifyPreview == nil {
		return nil, fmt.Errorf("%w: current authorization and preview verifier required", identitycontrol.ErrInvalidRequest)
	}
	return s.applyIdentityOperationContext(ctx, request, authorize, verifyPreview)
}

func (s *Store) applyIdentityOperationContext(ctx context.Context, request IdentityOperationRequest, authorize, verifyPreview func(context.Context, *IdentitySnapshot) error) (*IdentityReceipt, error) {
	if err := (identitycontrol.PreviewRequest{Operation: request.Operation, Target: request.Target}).Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Principal) == "" || len(request.Principal) > 256 || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 256 || len(request.ExpectedFingerprint) != 64 {
		return nil, fmt.Errorf("%w: principal, bounded key and preview fingerprint required", identitycontrol.ErrInvalidRequest)
	}
	request.Target = request.Target.Canonical(request.Operation)
	requestHash, err := identityEvidenceHash(request)
	if err != nil {
		return nil, err
	}
	var before *IdentitySnapshot
	var receipt *IdentityReceipt
	guard := func(ctx context.Context, tx *loggedTx) error {
		var err error
		before, err = s.identityOperationSnapshotTx(ctx, tx, request.Operation, request.Target)
		if err != nil {
			return err
		}
		if authorize != nil {
			if err = authorize(ctx, before); err != nil {
				return err
			}
		}
		var existingHash, encoded string
		err = tx.QueryRowContext(ctx, `SELECT request_hash,receipt_json FROM identity_operation_receipts WHERE principal=? AND idempotency_key=?`, request.Principal, request.IdempotencyKey).Scan(&existingHash, &encoded)
		if err == nil {
			if existingHash != requestHash {
				return ErrIdentityOperationIdempotency
			}
			receipt = new(IdentityReceipt)
			if err = json.Unmarshal([]byte(encoded), receipt); err != nil {
				return fmt.Errorf("decode identity receipt: %w", err)
			}
			// Return through rollback even on a fresh metadata row. Exact retries must
			// not seed metadata or commit anything merely to return a saved outcome.
			return errIdentityOperationReplay
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lookup identity receipt: %w", err)
		}
		if verifyPreview != nil {
			if err := verifyPreview(ctx, before); err != nil {
				return err
			}
		}
		if before.Fingerprint != request.ExpectedFingerprint {
			return ErrIdentityOperationStale
		}
		if len(before.Blockers) > 0 {
			return fmt.Errorf("%w: %s", ErrIdentityOperationBlocked, strings.Join(before.Blockers, ", "))
		}
		return nil
	}
	after := func(ctx context.Context, tx *loggedTx) error {
		snapshot, err := s.identityOperationSnapshotTx(ctx, tx, request.Operation, request.Target)
		if err != nil {
			return err
		}
		id, err := newVCardUID()
		if err != nil {
			return err
		}
		receipt = &IdentityReceipt{
			ID: id, Operation: request.Operation, Target: request.Target,
			BeforeFingerprint: before.Fingerprint, AfterFingerprint: snapshot.Fingerprint,
			BeforeRevision: before.IdentityRevision, AfterRevision: snapshot.IdentityRevision,
			Changed:      before.Fingerprint != snapshot.Fingerprint,
			Participants: canonicalIdentityIDs(append(append([]int64{}, before.Members...), snapshot.Members...)),
			Persons:      []int64{}, CreatedAt: time.Now().UTC(),
		}
		for _, p := range before.Persons {
			receipt.Persons = append(receipt.Persons, p.ID)
		}
		for _, p := range snapshot.Persons {
			receipt.Persons = append(receipt.Persons, p.ID)
		}
		receipt.Persons = canonicalIdentityIDs(receipt.Persons)
		if request.Operation == identitycontrol.OperationGraphLink || request.Operation == identitycontrol.OperationGraphUnlink {
			receipt.Edge = &IdentityReceiptEdgeChange{}
			receipt.Edge.BeforePresent, receipt.Edge.BeforeCandidateID = identityReceiptTargetEdge(before)
			receipt.Edge.AfterPresent, receipt.Edge.AfterCandidateID = identityReceiptTargetEdge(snapshot)
		}
		encoded, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO identity_operation_receipts (receipt_id,principal,idempotency_key,request_hash,receipt_json) VALUES (?,?,?,?,?)`, receipt.ID, request.Principal, request.IdempotencyKey, requestHash, string(encoded))
		if err != nil {
			return fmt.Errorf("persist identity receipt: %w", err)
		}
		return nil
	}
	switch request.Operation {
	case identitycontrol.OperationGraphLink:
		_, _, err = s.linkParticipantsContextGuardedOwned(ctx, request.Target.ParticipantID, request.Target.OtherParticipantID, 0, guard, after)
	case identitycontrol.OperationGraphUnlink:
		_, err = s.unlinkParticipantsContextGuardedReceipt(ctx, request.Target.ParticipantID, request.Target.OtherParticipantID, guard, after)
	case identitycontrol.OperationPersonLink, identitycontrol.OperationPersonUnlink:
		err = s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
				return err
			}
			if err := guard(ctx, tx); err != nil {
				return err
			}
			var changed bool
			if request.Operation == identitycontrol.OperationPersonLink {
				var err error
				changed, err = s.bindPersonParticipantsTx(ctx, tx, request.Target.PersonID, before.ComponentMembers)
				if err != nil {
					return err
				}
			} else {
				ph, args := sortedIDPlaceholders(before.ComponentMembers)
				args = append([]any{request.Target.PersonID}, args...)
				result, err := tx.ExecContext(ctx, `DELETE FROM person_participants WHERE person_id=? AND participant_id IN (`+ph+`)`, args...)
				if err != nil {
					return fmt.Errorf("detach explicit person binding: %w", err)
				}
				count, err := result.RowsAffected()
				if err != nil {
					return err
				}
				changed = count > 0
				if changed {
					if err := s.markContactStateDirtyTx(ctx, tx, request.Target.PersonID); err != nil {
						return err
					}
				}
			}
			if changed {
				if err := s.bumpPersonRevisionsTx(ctx, tx, request.Target.PersonID); err != nil {
					return err
				}
				if err := s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(ctx, tx, request.Target.PersonID); err != nil {
					return err
				}
				if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
					return err
				}
				if err := s.publishPersonIdentityScopeChangesTx(ctx, tx, []int64{request.Target.PersonID}); err != nil {
					return err
				}
			}
			return after(ctx, tx)
		})
	}
	if errors.Is(err, errIdentityOperationReplay) {
		return receipt, nil
	}
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

func identityReceiptTargetEdge(snapshot *IdentitySnapshot) (bool, int64) {
	for _, edge := range snapshot.Links {
		if edge.ParticipantID == snapshot.Target.ParticipantID && edge.OtherParticipantID == snapshot.Target.OtherParticipantID {
			return true, edge.CandidateID
		}
	}
	return false, 0
}
