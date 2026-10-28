package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

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
	"maunium.net/go/mautrix"
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
		{mautrix.HTTPError{Response: &http.Response{StatusCode: http.StatusTooManyRequests}}, "rate_limited"},
		{mautrix.HTTPError{Response: &http.Response{StatusCode: http.StatusInternalServerError}}, "other"},
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
		{oauth.ErrTokenUnrefreshable, "auth"},
		{oauth.ErrInvalidTokenJSON, "auth"},
		{&gmail.ThrottledError{}, "rate_limited"},
		{&msgraph.ThrottledError{}, "rate_limited"},
		{errors.New("invalid_grant status 401 429"), "other"},
		{&gmail.StatusError{StatusCode: 500}, "other"},
		{context.DeadlineExceeded, "network"},
	} {
		assert.Equal(t, tc.want, reason(fmt.Errorf("sync: %w", tc.err)))
	}
}

func TestProviderFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
		run    func(context.Context, string) error
	}{
		{"notion exhausted throttling", 429, "", "rate_limited", func(ctx context.Context, url string) error {
			_, err := notionmeetings.NewClient(url, "synthetic").ListUsers(ctx, "")
			return err
		}},
		{"notion exhausted server failures", 503, "", "other", func(ctx context.Context, url string) error {
			_, err := notionmeetings.NewClient(url, "synthetic").ListUsers(ctx, "")
			return err
		}},
		{"calendar unauthorized", 401, "", "auth", func(ctx context.Context, url string) error {
			_, err := gcal.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}), gcal.WithBaseURL(url)).ListCalendars(ctx, "")
			return err
		}},
		{"calendar throttled", 429, "", "rate_limited", func(ctx context.Context, url string) error {
			_, err := gcal.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}), gcal.WithBaseURL(url)).ListCalendars(ctx, "")
			return err
		}},
		{"calendar quota", 403, `{"error":{"errors":[{"reason":"quotaExceeded"}]}}`, "rate_limited", func(ctx context.Context, url string) error {
			_, err := gcal.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic"}), gcal.WithBaseURL(url)).ListCalendars(ctx, "")
			return err
		}},
		{"matrix", 401, `{"errcode":"M_UNKNOWN_TOKEN","error":"Expired token"}`, "auth", func(ctx context.Context, url string) error {
			client, err := mautrix.NewClient(url, "@archive:example.com", "synthetic")
			if err != nil {
				return fmt.Errorf("matrix request: %w", err)
			}
			_, err = client.Whoami(ctx)
			return fmt.Errorf("matrix request: %w", err)
		}},
		{"omi", 401, "", "auth", func(ctx context.Context, url string) error {
			_, err := omi.NewPacedClient(url, "synthetic", t.TempDir()).ListConversations(ctx, omi.ListParams{})
			return err
		}},
		{"omi request timeout", 0, "", "network", func(ctx context.Context, url string) error {
			_, err := omi.NewPacedClient(url, "synthetic", t.TempDir()).ListConversations(ctx, omi.ListParams{})
			return err
		}},
		{"granola", 401, "", "auth", func(ctx context.Context, url string) error {
			_, err := granola.NewClient(url, "synthetic").ListNotes(ctx, granola.ListNotesParams{})
			return err
		}},
		{"granola exhausted server failures", 503, "", "other", func(ctx context.Context, url string) error {
			_, err := granola.NewClient(url, "synthetic").ListNotes(ctx, granola.ListNotesParams{})
			return err
		}},
		{"beeper", 401, "", "auth", func(ctx context.Context, url string) error {
			_, err := beeper.NewClient(url, func(context.Context) (string, error) { return "synthetic", nil }, 1000).ListAccounts(ctx)
			return err
		}},
		{"chatwoot", 401, "", "auth", func(ctx context.Context, url string) error {
			client, err := chatwoot.NewClient(url, 9, "synthetic")
			if err != nil {
				return err
			}
			_, err = client.ListInboxes(ctx)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.status == 0 {
						// Returning on cancellation can send an empty 200 before
						// the client reports its timeout. Hold until the call ends.
						<-release
						return
					}
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				transport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = transport })
				err := tc.run(t.Context(), "https://example.com")
				require.Error(t, err)
				if tc.status == 0 {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
				assert.Equal(t, tc.want, reason(err), "provider error: %v", err)
			})
		})
	}
}
