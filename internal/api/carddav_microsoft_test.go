package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mscontacts"
	"golang.org/x/oauth2"
)

func TestCardDAVMicrosoftServiceNeedsContactsToken(t *testing.T) {
	t.Parallel()
	controller := &CardDAVController{cfg: &config.Config{Microsoft: config.MicrosoftConfig{ClientID: "synthetic-client"}}}
	controller.cfg.Data.DataDir = t.TempDir()
	_, err := controller.microsoftContactsManager("person@example.com")
	require.ErrorIs(t, err, carddav.ErrMicrosoftAuthorizationRequired)
}

func TestMicrosoftContactsTokenErrorMarksRevokedTokenUnauthorized(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	revoked := microsoftContactsTokenError(&oauth2.RetrieveError{ErrorCode: "invalid_grant"})
	require.ErrorIs(revoked, carddav.ErrMicrosoftAuthorizationRequired)
	_, ok := errors.AsType[*carddav.StatusError](revoked)
	require.False(ok, "a revoked sign-in rejects no single write")
	require.ErrorIs(microsoftContactsTokenError(&oauth2.RetrieveError{ErrorCode: "interaction_required"}), carddav.ErrMicrosoftAuthorizationRequired)

	throttled := microsoftContactsTokenError(&oauth2.RetrieveError{Response: &http.Response{
		StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"30"}},
	}})
	status, ok := errors.AsType[*carddav.StatusError](throttled)
	require.True(ok)
	require.Equal(http.StatusTooManyRequests, status.StatusCode)
	require.Equal(30*time.Second, status.RetryAfter)
	code, _ := carddav.SyncFailure(throttled)
	require.Equal("retry_after", code)

	unavailable := microsoftContactsTokenError(&oauth2.RetrieveError{ErrorCode: "temporarily_unavailable"})
	require.NotErrorIs(unavailable, carddav.ErrMicrosoftAuthorizationRequired)
	_, ok = errors.AsType[*carddav.StatusError](unavailable)
	require.False(ok)
	require.ErrorIs(unavailable, carddav.ErrMicrosoftTokenUnavailable, "one token failure stops the whole sync")
}

// A Microsoft connection keeps its schedule without a token, because carddav
// authorize-microsoft signs in outside the daemon.
func TestCardDAVMicrosoftScheduleSurvivesMissingToken(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	cfg := &config.Config{Microsoft: config.MicrosoftConfig{ClientID: "synthetic-client"}}
	cfg.Data.DataDir = t.TempDir()
	cfg.CardDAV = config.CardDAVConfig{Provider: "microsoft", BaseURL: mscontacts.GraphBaseURL, Username: "person@example.com", Schedule: "0 3 * * *"}
	controller := &CardDAVController{cfg: cfg, service: cardDAVListFixture{}}
	var scheduled CardDAVOperations
	controller.SetScheduleReconciler(func(_ config.CardDAVConfig, service CardDAVOperations) error {
		scheduled = service
		return nil
	})
	require.NoError(controller.ReconcileSchedule())
	require.NotNil(scheduled)
}
