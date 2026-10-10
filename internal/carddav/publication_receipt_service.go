package carddav

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

// ReconcileReviewedPersonWithReceipt observes only the pending update admitted
// for this original request. Settled outcomes require no provider transport.
// Recovery never sends another PUT or admits a new publication.
func (s *Service) ReconcileReviewedPersonWithReceipt(ctx context.Context, personID int64, token, principal, key string, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReceipt, error) {
	if s == nil || s.store == nil || authorize == nil {
		return nil, store.ErrCardDAVInvalidPlan
	}
	bound, err := s.store.WithReviewedCardDAVPublicationReceipt(ctx, principal, key, personID, token)
	if err != nil {
		return nil, err
	}
	release, err := s.store.AcquireCardDAVPersonOperation(bound, personID)
	if err != nil {
		return nil, err
	}
	defer release()
	receipt, pending, err := s.store.ReviewedCardDAVPublicationRecoveryContext(bound, authorize)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return receipt, nil
	}
	if err := s.validateScopedPublication(authorize); err != nil {
		return nil, err
	}
	operationErr := s.recoverPublicationAuthorizedUnderLease(bound, *pending, authorize)
	receipt, err = s.store.ReviewedCardDAVPublicationReceiptContext(bound, authorize)
	if err != nil {
		return nil, err
	}
	return receipt, operationErr
}

// PublishReviewedPersonWithReceipt returns an exact saved outcome after current
// authorization, before generating a preview or requiring provider transport.
// Native callers derive principal from current authentication. Repeated requests
// never dispatch again, including receipts that still need GET-only recovery.
func (s *Service) PublishReviewedPersonWithReceipt(ctx context.Context, personID int64, token, principal, key string, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReceipt, error) {
	if s == nil || s.store == nil || authorize == nil {
		return nil, store.ErrCardDAVInvalidPlan
	}
	bound, err := s.store.WithReviewedCardDAVPublicationReceipt(ctx, principal, key, personID, token)
	if err != nil {
		return nil, err
	}
	release, err := s.store.AcquireCardDAVPersonOperation(bound, personID)
	if err != nil {
		return nil, err
	}
	defer release()
	receipt, err := s.store.ReviewedCardDAVPublicationReceiptContext(bound, authorize)
	if err == nil {
		return receipt, nil
	}
	if !errors.Is(err, store.ErrCardDAVPublicationReceiptNotFound) {
		return nil, err
	}
	if err := s.validateScopedPublication(authorize); err != nil {
		return nil, err
	}
	operationErr := s.publishReviewedPersonAuthorizedUnderLease(bound, personID, token, authorize, func(ctx context.Context, pending store.CardDAVPublication) error {
		return s.store.AdmitCardDAVPublicationReceiptContext(ctx, pending, authorize)
	})
	receipt, err = s.store.ReviewedCardDAVPublicationReceiptContext(bound, authorize)
	if err != nil {
		if operationErr != nil && errors.Is(err, store.ErrCardDAVPublicationReceiptNotFound) {
			return nil, operationErr
		}
		return nil, err
	}
	return receipt, operationErr
}
