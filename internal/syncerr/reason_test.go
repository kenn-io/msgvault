package syncerr

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/slack"
	"golang.org/x/oauth2"
)

func TestReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&net.DNSError{Err: "synthetic"}, "network"},
		{&oauth2.RetrieveError{ErrorCode: "invalid_grant"}, "auth"},
		{msgraph.ErrUnauthorized, "auth"},
		{&gmail.StatusError{StatusCode: 401}, "auth"},
		{&gmail.StatusError{StatusCode: 429}, "rate_limited"},
		{slack.ErrAuth, "auth"},
		{notionmeetings.ErrUnauthorized, "auth"},
		{notionmeetings.ErrRateLimited, "rate_limited"},
		{oauth.ErrTokenUnrefreshable, "auth"},
		{oauth.ErrInvalidTokenJSON, "auth"},
		{&gmail.ThrottledError{}, "rate_limited"},
		{&msgraph.ThrottledError{}, "rate_limited"},
		{errors.New("invalid_grant status 401 429"), "other"},
		{&gmail.StatusError{StatusCode: 500}, "other"},
		{fmt.Errorf("dial tcp: %w", &gmail.StatusError{StatusCode: 500}), "other"},
	} {
		assert.Equal(t, tc.want, Reason(fmt.Errorf("sync: %w", tc.err)))
	}
}

func TestCalendarUnauthorizedReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	client := gcal.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}), gcal.WithBaseURL(server.URL))
	_, err := client.ListCalendars(t.Context(), "")
	assert.Equal(t, "auth", Reason(err))
}
