package syncerr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{&carddav.StatusError{StatusCode: 401}, "auth"},
		{&carddav.StatusError{StatusCode: 429}, "rate_limited"},
		{&gmail.StatusError{StatusCode: 429}, "rate_limited"},
		{slack.ErrAuth, "auth"},
		{&twilio.APIError{StatusCode: 401}, "auth"},
		{&twilio.APIError{StatusCode: 429}, "rate_limited"},
		{&discord.APIError{StatusCode: 401}, "auth"},
		{&discord.APIError{StatusCode: 429}, "rate_limited"},
		{&imap.Error{Code: imap.ResponseCodeAuthenticationFailed}, "auth"},
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

func TestProviderUnauthorizedReasons(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	t.Run("twilio", func(t *testing.T) {
		client, err := twilio.NewClient(twilio.Options{AccountSID: "ACaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AuthToken: "synthetic", Endpoints: map[string]string{"voice": server.URL, "intelligence": server.URL}})
		require.NoError(t, err)
		_, _, err = client.ListRecordingsPage(t.Context(), time.Time{}, 1000, "")
		require.Error(t, err)
		assert.Equal(t, "auth", Reason(err))
	})
	t.Run("omi", func(t *testing.T) {
		_, err := omi.NewPacedClient(server.URL, "synthetic", t.TempDir()).ListConversations(t.Context(), omi.ListParams{})
		require.Error(t, err)
		assert.Equal(t, "auth", Reason(err))
	})
	t.Run("granola", func(t *testing.T) {
		_, err := granola.NewClient(server.URL, "synthetic").ListNotes(t.Context(), granola.ListNotesParams{})
		require.Error(t, err)
		assert.Equal(t, "auth", Reason(err))
	})
	t.Run("beeper", func(t *testing.T) {
		client := beeper.NewClient(server.URL, func(context.Context) (string, error) { return "synthetic", nil }, 1000)
		_, err := client.ListAccounts(t.Context())
		require.Error(t, err)
		assert.Equal(t, "auth", Reason(err))
	})
	t.Run("chatwoot", func(t *testing.T) {
		client, err := chatwoot.NewClient(server.URL, 9, "synthetic")
		require.NoError(t, err)
		_, err = client.ListInboxes(t.Context())
		require.Error(t, err)
		assert.Equal(t, "auth", Reason(err))
	})
}
