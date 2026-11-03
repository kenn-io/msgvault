package meta

import (
	"context"
	"errors"
)

// ErrorClass is a stable caller-facing outcome without provider message text.
type ErrorClass string

const (
	ClassRateLimited            ErrorClass = "rate_limited"
	ClassTokenInvalid           ErrorClass = "token_invalid"
	ClassPermissionMissing      ErrorClass = "permission_missing"
	ClassWindowClosed           ErrorClass = "messaging_window_closed"
	ClassPrivateReplyNotAllowed ErrorClass = "private_reply_not_allowed"
	ClassThreadOwned            ErrorClass = "thread_owned_by_other_app"
	ClassRecipientUnavailable   ErrorClass = "recipient_unavailable"
	ClassAppAccess              ErrorClass = "app_access_insufficient"
	ClassRejected               ErrorClass = "provider_rejected"
	ClassCancelled              ErrorClass = "cancelled_before_dispatch"
	ClassUnknown                ErrorClass = "outcome_unknown"
)

// Classify distinguishes a definitive rejection from a possibly applied write.
// Known code/subcode pairs take precedence over the broad code predicates.
func Classify(err error, dispatched bool) ErrorClass {
	if !dispatched && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return ClassCancelled
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status >= 500 {
		return ClassUnknown
	}
	switch {
	case api.Code == 200 && api.Subcode == 2018028:
		return ClassAppAccess
	case api.Code == 10 && (api.Subcode == 2534022 || api.Subcode == 2018278 || api.Subcode == 2018065):
		return ClassWindowClosed
	case api.Code == 100 && api.Subcode == 2534025:
		return ClassPrivateReplyNotAllowed
	case (api.Code == 10 && api.Subcode == 2018300) || (api.Code == 100 && api.Subcode == 2534037):
		return ClassThreadOwned
	case api.Code == 551 || (api.Code == 10 && api.Subcode == 2018108):
		return ClassRecipientUnavailable
	case api.Throttled():
		return ClassRateLimited
	case api.TokenExpired():
		return ClassTokenInvalid
	case api.Permission() || api.Code == 3:
		return ClassPermissionMissing
	default:
		return ClassRejected
	}
}

// AbuseWarning tells callers to stop sending when Meta flags abusive traffic.
func AbuseWarning(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Code == 613 && api.Subcode == 2018338
}
