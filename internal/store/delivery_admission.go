package store

import (
	"context"
	"errors"
)

type DeliveryAdmissionError struct {
	Code              string                `json:"code"`
	Reason            string                `json:"reason"`
	ProviderAttempted bool                  `json:"provider_attempted"`
	Policies          []DeliveryPolicyState `json:"policies"`
	cause             error
}

func (e *DeliveryAdmissionError) Error() string { return e.Code + ": " + e.Reason }
func (e *DeliveryAdmissionError) Unwrap() error { return e.cause }

// WithDeliveryAdmissionContext is the check a sender runs before each provider
// attempt. One read snapshot reads every recipient's current policy, then
// attempt runs once with no lock held. Each recipient is one exact envelope
// recipient (person and address), including cc and bcc. A revocation committed before the check
// blocks the attempt; one that lands later blocks the next attempt. The callback
// is never called on denial.
func (s *Store) WithDeliveryAdmissionContext(ctx context.Context, recipients []DeliveryPolicyQuery, attempt func(context.Context) error) error {
	blocked := func(reason string, cause error, states []DeliveryPolicyState) *DeliveryAdmissionError {
		code := "draft_required"
		if err := ctx.Err(); err != nil {
			code, reason, cause = "admission_cancelled", "admission_cancelled", err
		}
		return &DeliveryAdmissionError{Code: code, Reason: reason, Policies: states, cause: cause}
	}
	if len(recipients) == 0 || attempt == nil {
		return blocked("exact_recipients_required", ErrDeliveryPolicyInvalid, nil)
	}
	// A snapshot the caller opened earlier can predate a revocation.
	if snapshot, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot); ok && snapshot.db == s.db.DB {
		return blocked("admission_snapshot_inherited", ErrDeliveryPolicyInvalid, nil)
	}
	states := make([]DeliveryPolicyState, 0, len(recipients))
	var denied *DeliveryAdmissionError
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		holders := newDeliveryHolderCache() // dies with the snapshot
		for _, recipient := range recipients {
			q, err := resolveDeliveryQuery(recipient)
			if err != nil || recipient.Endpoint == "" {
				if err == nil {
					err = ErrDeliveryPolicyInvalid
				}
				if denied == nil {
					denied = blocked("unresolved_recipient", err, nil)
				}
				continue
			}
			state, _, err := s.deliveryStateTx(ctx, tx, q, false, false, holders)
			if err != nil {
				if !errors.Is(err, ErrDeliveryPersonUnknown) && !errors.Is(err, ErrDeliveryPolicyInvalid) {
					return err
				}
				if denied == nil {
					denied = blocked("unresolved_recipient", err, nil)
				}
				continue
			}
			states = append(states, *state)
			if state.EffectivePolicy != DeliverySendAllowed && denied == nil {
				denied = blocked(state.Reason, nil, nil)
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			return &DeliveryAdmissionError{Code: "admission_unavailable", Reason: "admission_unavailable", cause: err}
		}
		return blocked("admission_cancelled", err, nil)
	}
	if denied != nil {
		denied.Policies = states
		return denied
	}
	if err = ctx.Err(); err != nil {
		return blocked("admission_cancelled", err, states)
	}
	// From here no error can assert the provider was not called. The caller
	// reconciles uncertainty with provider-native idempotency.
	if err = attempt(ctx); err != nil {
		return &DeliveryAdmissionError{Code: "delivery_uncertain", Reason: "provider_attempt_failed", ProviderAttempted: true, Policies: states, cause: err}
	}
	return nil
}

// DeliveryReceipt is the outcome of ExecuteDeliveryContext. A blocked send
// returns the original Content so the caller can keep the draft.
type DeliveryReceipt struct {
	Code              string
	Content           []byte
	ProviderAttempted bool
}

// ExecuteDeliveryContext admits every recipient, then makes one send attempt. A
// blocked send returns the original content and makes zero provider calls. Each
// retry calls it again so admission reads current policy.
func (s *Store) ExecuteDeliveryContext(ctx context.Context, recipients []DeliveryPolicyQuery, content []byte, send func(context.Context) error) (*DeliveryReceipt, error) {
	receipt := &DeliveryReceipt{Content: append([]byte(nil), content...)}
	if err := s.WithDeliveryAdmissionContext(ctx, recipients, send); err != nil {
		receipt.Code = DeliveryErrorCode(err)
		if admission, ok := errors.AsType[*DeliveryAdmissionError](err); ok {
			receipt.ProviderAttempted = admission.ProviderAttempted
		}
		return receipt, err
	}
	receipt.Code, receipt.ProviderAttempted = "sent", true
	return receipt, nil
}

// DeliveryErrorCode is shared by typed transport adapters.
func DeliveryErrorCode(err error) string {
	var admission *DeliveryAdmissionError
	switch {
	case errors.As(err, &admission):
		return admission.Code
	case errors.Is(err, ErrDeliveryPolicyRevisionConflict):
		return "revision_conflict"
	case errors.Is(err, ErrDeliveryPersonUnknown):
		return "unknown_person"
	case errors.Is(err, ErrDeliveryPolicyInvalid):
		if cause, ok := errors.AsType[*deliveryInvalidError](err); ok {
			return cause.reason
		}
		return "invalid_policy"
	default:
		return "delivery_policy_unavailable"
	}
}
