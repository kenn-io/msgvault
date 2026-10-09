package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DeliveryRecipient is one exact envelope recipient, including cc and bcc.
// A container/group cannot stand in for its members. Each attempt, including
// retries and queued work, must supply a fresh reviewed binding and revision.
type DeliveryRecipient struct {
	PersonUID      string         `json:"person_uid"`
	Target         DeliveryTarget `json:"target"`
	Role           string         `json:"role"`
	PolicyRevision int64          `json:"policy_revision"`
	PersonRevision int64          `json:"person_revision"`
	BindingDigest  string         `json:"binding_digest"`
}
type DeliveryAdmissionError struct {
	Code              string                `json:"code"`
	Reason            string                `json:"reason"`
	ProviderAttempted bool                  `json:"provider_attempted"`
	Policies          []DeliveryPolicyState `json:"policies"`
	cause             error
}

func (e *DeliveryAdmissionError) Error() string { return e.Code + ": " + e.Reason }
func (e *DeliveryAdmissionError) Unwrap() error { return e.cause }

// WithDeliveryAdmissionContext defines the application dispatch boundary.
// The synchronous callback MUST perform exactly one provider attempt using this
// immutable recipient snapshot, honor its context, and return after the attempt
// ends. It must not enqueue work, spawn a send goroutine, change the envelope,
// reenter Store, or perform its own retry. Provider/platform/user authorization
// remains the caller's responsibility. The callback is never called on denial.
//
// Native identity/source writers and policy changes wait until this attempt
// exits. Revocation that commits before admission blocks the attempt; revocation
// arriving after admission takes effect on the next attempt. Holding this gate
// through callback completion prevents stale preflight checks authorizing sends.
func (s *Store) WithDeliveryAdmissionContext(ctx context.Context, recipients []DeliveryRecipient, attempt func(context.Context) error) error {
	blocked := func(reason string, cause error, states []DeliveryPolicyState) error {
		code := "draft_required"
		if err := ctx.Err(); err != nil {
			code, reason, cause = "admission_cancelled", "admission_cancelled", err
		} else if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
			code, reason = "admission_cancelled", "admission_cancelled"
		}
		return &DeliveryAdmissionError{Code: code, Reason: reason, Policies: states, cause: cause}
	}
	if len(recipients) == 0 || len(recipients) > 100 || attempt == nil {
		return blocked("exact_recipients_required", ErrDeliveryPolicyInvalid, nil)
	}
	if s.readOnly {
		return &DeliveryAdmissionError{
			Code:   "admission_unavailable",
			Reason: "read_only_store",
			cause:  errors.New("delivery admission requires a writable Store"),
		}
	}
	snapshot := append([]DeliveryRecipient(nil), recipients...)
	// Pool acquisition remains cancellable. Once a connection is acquired, the
	// transaction lifetime is detached so cancellation cannot release the native
	// fence while the synchronous provider callback is still in flight.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return blocked("admission_unavailable", err, nil)
	}
	defer func() { _ = conn.Close() }()
	rawTx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		return blocked("admission_unavailable", err, nil)
	}
	tx := &loggedTx{Tx: rawTx, rebind: s.db.rebind}
	defer func() { _ = tx.Rollback() }()
	if s.IsPostgreSQL() {
		if err = s.enterDeliveryAdmissionExclusiveFenceContext(ctx, tx); err != nil {
			return blocked("admission_unavailable", err, nil)
		}
	} else if _, err = tx.ExecContext(ctx,
		`UPDATE delivery_admission_lock SET singleton=singleton WHERE singleton=1`,
	); err != nil {
		return blocked("admission_unavailable", err, nil)
	}
	states := make([]DeliveryPolicyState, 0, len(snapshot))
	seen := map[deliveryRecipientEndpoint]bool{}
	for _, recipient := range snapshot {
		q := DeliveryPolicyQuery{PersonUID: recipient.PersonUID, Target: &recipient.Target}
		if err = ValidateDeliveryPolicyQuery(q); err != nil {
			return blocked("unresolved_recipient", err, states)
		}
		if recipient.Role != "to" && recipient.Role != "cc" && recipient.Role != "bcc" {
			return blocked("unsupported_recipient_role", ErrDeliveryPolicyInvalid, states)
		}
		// Native anchors can describe the same mailbox. Reject a repeated
		// canonical route key so an endpoint cannot be admitted twice through
		// different anchors. Keep the original snapshot for binding checks.
		endpoint := recipient.Target
		endpoint.ContactPointID, endpoint.ParticipantID = 0, 0
		endpoint.Endpoint = deliveryEndpointRouteKey(endpoint)
		key := deliveryRecipientEndpoint{personUID: recipient.PersonUID, target: endpoint}
		if seen[key] {
			return blocked("duplicate_recipient", ErrDeliveryPolicyInvalid, states)
		}
		seen[key] = true
		state, e := s.deliveryStateTx(ctx, tx, q)
		if e != nil {
			return blocked("unresolved_recipient", e, states)
		}
		states = append(states, *state)
		if state.EffectivePolicy != DeliverySendAllowed {
			return blocked(state.Reason, nil, states)
		}
		if recipient.PolicyRevision != state.PolicyRevision || recipient.PersonRevision != state.PersonRevision || recipient.BindingDigest == "" || recipient.BindingDigest != state.BindingDigest {
			return blocked("stale_delivery_binding", ErrDeliveryTargetChanged, states)
		}
	}
	if err = ctx.Err(); err != nil {
		return blocked("admission_cancelled", err, states)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	attemptCtx = context.WithValue(attemptCtx, deliveryAdmissionContextKey{}, snapshot)
	// From here onward no error can assert that the provider was not called.
	// The caller must reconcile uncertainty with provider-native idempotency.
	if err = attempt(attemptCtx); err != nil {
		return &DeliveryAdmissionError{Code: "delivery_uncertain", Reason: "provider_attempt_failed", ProviderAttempted: true, Policies: states, cause: err}
	}
	if err = tx.Commit(); err != nil {
		return &DeliveryAdmissionError{Code: "delivery_uncertain", Reason: "admission_completion_failed", ProviderAttempted: true, Policies: states, cause: err}
	}
	return nil
}

type deliveryRecipientEndpoint struct {
	personUID string
	target    DeliveryTarget
}

type deliveryAdmissionContextKey struct{}

// AdmittedDeliveryRecipients returns a copy of the approved envelope snapshot.
// Future provider adapters must use this snapshot rather than mutable input.
func AdmittedDeliveryRecipients(ctx context.Context) ([]DeliveryRecipient, error) {
	recipients, ok := ctx.Value(deliveryAdmissionContextKey{}).([]DeliveryRecipient)
	if !ok {
		return nil, fmt.Errorf("missing delivery admission: %w", ErrDeliveryPolicyInvalid)
	}
	return append([]DeliveryRecipient(nil), recipients...), nil
}

// DeliveryErrorCode is shared by typed transport adapters.
func DeliveryErrorCode(err error) string {
	var admission *DeliveryAdmissionError
	switch {
	case errors.As(err, &admission):
		return admission.Code
	case errors.Is(err, ErrDeliveryPolicyRevisionConflict):
		return "revision_conflict"
	case errors.Is(err, ErrDeliveryTargetChanged):
		return "target_changed"
	case errors.Is(err, ErrDeliveryPersonUnknown):
		return "unknown_person"
	case errors.Is(err, ErrDeliveryPolicyInvalid):
		return "invalid_policy"
	default:
		return "delivery_policy_unavailable"
	}
}
