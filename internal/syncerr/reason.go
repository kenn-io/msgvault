package syncerr

import (
	"errors"

	"github.com/emersion/go-imap/v2"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/twilio"
	"golang.org/x/oauth2"
)

func IsAuthInvalidError(err error) bool {
	e, ok := errors.AsType[*oauth2.RetrieveError](err)
	return ok && e.ErrorCode == "invalid_grant"
}
func Reason(err error) string {
	status := 0
	if e, ok := errors.AsType[*gmail.StatusError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[*msgraph.StatusError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[*twilio.APIError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[*discord.APIError](err); ok {
		status = e.StatusCode
	}
	imapErr, _ := errors.AsType[*imap.Error](err)
	_, gmailThrottled := errors.AsType[*gmail.ThrottledError](err)
	_, graphThrottled := errors.AsType[*msgraph.ThrottledError](err)
	_, retrieveError := errors.AsType[*oauth2.RetrieveError](err)
	switch {
	case IsAuthInvalidError(err) || errors.Is(err, msgraph.ErrUnauthorized) || errors.Is(err, gcal.ErrUnauthorized) || errors.Is(err, slack.ErrAuth) || errors.Is(err, notionmeetings.ErrUnauthorized) || errors.Is(err, oauth.ErrTokenUnrefreshable) || errors.Is(err, oauth.ErrInvalidTokenJSON) || errors.Is(err, beeper.ErrUnauthorized) || errors.Is(err, chatwoot.ErrUnauthorized) || imapErr != nil && imapErr.Code == imap.ResponseCodeAuthenticationFailed || status == 401:
		return "auth"
	case gmailThrottled || graphThrottled || errors.Is(err, notionmeetings.ErrRateLimited) || status == 429:
		return "rate_limited"
	case status == 0 && !retrieveError && IsTransientNetwork(err):
		return "network"
	default:
		return "other"
	}
}
