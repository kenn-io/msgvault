// Package delivery provides owner-authorized policy operations and an explicit
// draft/send boundary. It does not implement provider transport or an outbox.
package delivery

import (
	"context"
	"errors"
	"go.kenn.io/msgvault/internal/store"
)

var ErrPolicyForbidden = errors.New("delivery policy capability required")

type PolicyStore interface {
	GetDeliveryPolicyContext(ctx context.Context, query store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error)
	SetDeliveryPolicyContext(ctx context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error)
	ClearDeliveryPolicyContext(ctx context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error)
}

// Authority must be derived from trusted authentication, never request fields.
// PolicyWrite is distinct from ordinary drafting or sending capabilities.
type Authority struct {
	Actor                   string
	PolicyRead, PolicyWrite bool
}
type Service struct{ Store PolicyStore }

func (s Service) Read(ctx context.Context, a Authority, q store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error) {
	if !a.PolicyRead {
		return nil, ErrPolicyForbidden
	}
	return s.Store.GetDeliveryPolicyContext(ctx, q)
}
func (s Service) Write(ctx context.Context, a Authority, w store.DeliveryPolicyWrite, reset bool) (*store.DeliveryPolicyReceipt, error) {
	if !a.PolicyWrite || a.Actor == "" {
		return nil, ErrPolicyForbidden
	}
	w.Actor = a.Actor
	if reset {
		return s.Store.ClearDeliveryPolicyContext(ctx, w)
	}
	return s.Store.SetDeliveryPolicyContext(ctx, w)
}

type Mode string

const (
	ModeDraft Mode = "draft"
	ModeSend  Mode = "send"
)

type AdmissionStore interface {
	WithDeliveryAdmissionContext(ctx context.Context, recipients []store.DeliveryRecipient, attempt func(context.Context) error) error
}
type DispatchRequest struct {
	Mode       Mode
	Recipients []store.DeliveryRecipient
	Content    []byte
}
type DispatchReceipt struct {
	Mode              Mode
	Code              string
	Content           []byte
	ProviderAttempted bool
}

// Execute keeps drafts available in both policies. A blocked explicit send
// returns the original recoverable content, never silently creates/sends a
// draft, and performs zero provider calls. Each retry invokes admission anew.
func Execute(ctx context.Context, st AdmissionStore, r DispatchRequest, draft, send func(context.Context) error) (*DispatchReceipt, error) {
	receipt := &DispatchReceipt{Mode: r.Mode, Content: append([]byte(nil), r.Content...)}
	switch r.Mode {
	case ModeDraft:
		if draft == nil {
			return receipt, store.ErrDeliveryPolicyInvalid
		}
		receipt.Code = "draft"
		return receipt, draft(ctx)
	case ModeSend:
		err := st.WithDeliveryAdmissionContext(ctx, r.Recipients, send)
		if err != nil {
			receipt.Code = store.DeliveryErrorCode(err)
			if admission, ok := errors.AsType[*store.DeliveryAdmissionError](err); ok {
				receipt.ProviderAttempted = admission.ProviderAttempted
			}
			return receipt, err
		}
		receipt.Code = "sent"
		receipt.ProviderAttempted = true
		return receipt, nil
	default:
		return receipt, store.ErrDeliveryPolicyInvalid
	}
}
