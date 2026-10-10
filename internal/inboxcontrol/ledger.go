package inboxcontrol

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// ErrConflict means an intent key or receipt transition conflicts with durable evidence.
var ErrConflict = errors.New("inbox operation conflict")

type Status string

const (
	StatusPrepared      Status = "prepared"
	StatusDispatching   Status = "dispatching"
	StatusVerified      Status = "verified"
	StatusPartial       Status = "partial"
	StatusUnknown       Status = "unknown"
	StatusReconcileOnly Status = "reconcile-only"
	StatusFailed        Status = "failed"
)

// Receipt retains dispatch evidence indefinitely. Intent contains only operation
// metadata, never message bodies, credentials, or preview tokens. Principal and
// original source/action authorization must be checked before returning it.
type Receipt struct {
	ID             string     `json:"id"`
	PrincipalID    string     `json:"principal_id"`
	SourceID       int64      `json:"source_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	IntentHash     string     `json:"intent_hash"`
	StateHash      string     `json:"state_hash"`
	Intent         Request    `json:"intent"`
	Before         State      `json:"before"`
	Projected      *State     `json:"projected,omitempty"`
	After          *State     `json:"after,omitempty"`
	Status         Status     `json:"status"`
	FailureCode    string     `json:"failure_code,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	DispatchedAt   *time.Time `json:"dispatched_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

func (r Receipt) ValidatePrepared() error {
	if !validIdentity(r.ID) || !validIdentity(r.PrincipalID) || len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 128 || !utf8.ValidString(r.IdempotencyKey) || r.SourceID <= 0 || r.Status != StatusPrepared || r.CreatedAt.IsZero() || r.DispatchedAt != nil || r.FinishedAt != nil || r.After != nil || r.FailureCode != "" {
		return fmt.Errorf("%w: invalid prepared receipt", ErrInvalid)
	}
	if !r.Intent.DryRun || r.Intent.Expected != nil || r.Intent.PreviewToken != "" || r.Intent.IdempotencyKey != "" || r.Intent.ReceiptID != "" {
		return fmt.Errorf("%w: receipt intent must omit execution credentials", ErrInvalid)
	}
	if !r.Intent.Operation.IsMutation() {
		return fmt.Errorf("%w: receipt requires a mutation intent", ErrInvalid)
	}
	intent, err := IntentFingerprint(r.Intent)
	if err != nil {
		return err
	}
	state, err := SemanticFingerprint(r.Before)
	if err != nil {
		return err
	}
	if intent != r.IntentHash || state != r.StateHash || r.Before.ObservedAt.IsZero() {
		return fmt.Errorf("%w: receipt fingerprints do not match evidence", ErrInvalid)
	}
	if r.Projected != nil {
		if _, err := SemanticFingerprint(*r.Projected); err != nil {
			return err
		}
		if r.Before.Target != r.Projected.Target || r.Before.Source != r.Projected.Source {
			return fmt.Errorf("%w: projection changes canonical binding", ErrInvalid)
		}
	}
	if r.Intent.Target != nil {
		if *r.Intent.Target != r.Before.Target || r.Intent.Target.SourceID != r.SourceID {
			return fmt.Errorf("%w: receipt target does not match evidence", ErrInvalid)
		}
	} else if r.Intent.Source == nil || *r.Intent.Source != r.Before.Source || r.Intent.Source.SourceID != r.SourceID {
		return fmt.Errorf("%w: receipt source does not match evidence", ErrInvalid)
	}
	return nil
}

// Ledger's atomic claim prevents duplicate dispatch even across processes.
// A returned existing receipt must be reauthorized and compared by intent.
type Ledger interface {
	LookupInboxReceipt(ctx context.Context, principalID string, sourceID int64, idempotencyKey string) (*Receipt, error)
	GetInboxReceipt(ctx context.Context, receiptID string) (*Receipt, error)
	PrepareInboxReceipt(ctx context.Context, receipt Receipt) (*Receipt, bool, error)
	MarkInboxDispatching(ctx context.Context, receiptID string) error
	RecordInboxReceiptMapping(ctx context.Context, receiptID string, mapping State) error
	FinishInboxReceipt(ctx context.Context, receiptID string, status Status, after *State, failureCode string) error
}
