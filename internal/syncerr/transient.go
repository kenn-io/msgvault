// Package syncerr provides shared error classification for sync flows.
package syncerr

import (
	"errors"
	"net"
	"strings"

	"github.com/emersion/go-imap/v2"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/omi"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/twilio"
	"golang.org/x/oauth2"
	"maunium.net/go/mautrix"
)

// IsTransientNetwork reports whether err (or anything it wraps) indicates a
// transient network problem rather than a logical or auth failure. It catches
// DNS lookup failures, dial timeouts, and connection refused — common after
// laptop sleep/wake or Wi-Fi flaps. Callers should retry on the next schedule
// instead of alerting the user; the underlying credentials are not invalid.
func IsTransientNetwork(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Some libraries (notably golang.org/x/oauth2) wrap transport errors
	// with fmt.Errorf without %w, hiding the typed *url.Error / *net.OpError
	// from errors.As. Fall back to substring matching for those cases.
	s := err.Error()
	switch {
	case strings.Contains(s, "dial tcp"):
		return true
	case strings.Contains(s, "no such host"):
		return true
	case strings.Contains(s, "i/o timeout"):
		return true
	case strings.Contains(s, "connection refused"):
		return true
	case strings.Contains(s, "network is unreachable"):
		return true
	case strings.Contains(s, "no route to host"):
		return true
	}
	return false
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
	} else if e, ok := errors.AsType[*carddav.StatusError](err); ok {
		status = e.StatusCode
	} else if e, ok := errors.AsType[mautrix.HTTPError](err); ok && e.Response != nil {
		status = e.Response.StatusCode
	}
	imapErr, _ := errors.AsType[*imap.Error](err)
	notionErr, _ := errors.AsType[*notionmeetings.APIError](err)
	_, gmailThrottled := errors.AsType[*gmail.ThrottledError](err)
	_, graphThrottled := errors.AsType[*msgraph.ThrottledError](err)
	retrieveError, _ := errors.AsType[*oauth2.RetrieveError](err)
	switch {
	case errors.Is(err, granola.ErrUnauthorized) || errors.Is(err, omi.ErrUnauthorized) || (retrieveError != nil && retrieveError.ErrorCode == "invalid_grant") || errors.Is(err, msgraph.ErrUnauthorized) || errors.Is(err, gcal.ErrUnauthorized) || errors.Is(err, slack.ErrAuth) || errors.Is(err, notionmeetings.ErrUnauthorized) || errors.Is(err, oauth.ErrTokenUnrefreshable) || errors.Is(err, oauth.ErrInvalidTokenJSON) || errors.Is(err, beeper.ErrUnauthorized) || errors.Is(err, chatwoot.ErrUnauthorized) || imapErr != nil && imapErr.Code == imap.ResponseCodeAuthenticationFailed || status == 401:
		return "auth"
	case gmailThrottled || graphThrottled || errors.Is(err, gcal.ErrRateLimited) || notionErr != nil && notionErr.Status == 429 || errors.Is(err, granola.ErrRateLimited) || status == 429:
		return "rate_limited"
	case status == 0 && retrieveError == nil && IsTransientNetwork(err):
		return "network"
	}
	return "other"
}
