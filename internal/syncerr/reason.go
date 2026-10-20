package syncerr

import (
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/slack"
	"golang.org/x/oauth2"
)

// IsAuthInvalidError reports an expired or revoked OAuth refresh token.
func IsAuthInvalidError(err error) bool {
	e, ok := errors.AsType[*oauth2.RetrieveError](err)
	return ok && e.ErrorCode == "invalid_grant"
}

// Reason classifies sync errors into the fixed telemetry vocabulary.
func Reason(err error) string {
	gmailStatus, _ := errors.AsType[*gmail.StatusError](err)
	graphStatus, _ := errors.AsType[*msgraph.StatusError](err)
	_, gmailThrottled := errors.AsType[*gmail.ThrottledError](err)
	_, graphThrottled := errors.AsType[*msgraph.ThrottledError](err)
	_, retrieveError := errors.AsType[*oauth2.RetrieveError](err)
	switch {
	case IsAuthInvalidError(err) || errors.Is(err, msgraph.ErrUnauthorized) || errors.Is(err, gcal.ErrUnauthorized) ||
		errors.Is(err, slack.ErrAuth) || errors.Is(err, notionmeetings.ErrUnauthorized) || errors.Is(err, oauth.ErrTokenUnrefreshable) || errors.Is(err, oauth.ErrInvalidTokenJSON) ||
		gmailStatus != nil && gmailStatus.StatusCode == http.StatusUnauthorized || graphStatus != nil && graphStatus.StatusCode == http.StatusUnauthorized:
		return "auth"
	case gmailThrottled || graphThrottled || errors.Is(err, notionmeetings.ErrRateLimited) ||
		gmailStatus != nil && gmailStatus.StatusCode == http.StatusTooManyRequests || graphStatus != nil && graphStatus.StatusCode == http.StatusTooManyRequests:
		return "rate_limited"
	case gmailStatus != nil || graphStatus != nil || retrieveError:
		return "other"
	case IsTransientNetwork(err):
		return "network"
	default:
		return "other"
	}
}
