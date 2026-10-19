package syncerr

import (
	"errors"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/msgraph"
	"golang.org/x/oauth2"
)

// Reason classifies sync errors into the fixed telemetry vocabulary.
func Reason(err error) string {
	if IsTransientNetwork(err) {
		return "network"
	}
	if e, ok := errors.AsType[*oauth2.RetrieveError](err); errors.Is(err, msgraph.ErrUnauthorized) || ok && e.ErrorCode == "invalid_grant" {
		return "auth"
	}
	if _, ok := errors.AsType[*gmail.ThrottledError](err); ok {
		return "rate_limited"
	}
	if _, ok := errors.AsType[*msgraph.ThrottledError](err); ok {
		return "rate_limited"
	}
	return "other"
}
