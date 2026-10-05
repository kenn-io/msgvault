package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mscontacts"
	"golang.org/x/oauth2"
)

func TestCardDAVMicrosoftAccountSelection(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	required := require.New(t)
	req := normalizeCardDAVAccountRequest(CardDAVAccountRequest{Provider: "microsoft", Username: "Person@Example.com", Enabled: new(true)})
	required.NoError(validateCardDAVAccountRequest(req))
	assertions.Equal(mscontacts.GraphBaseURL, req.BaseURL)
	assertions.Equal("person@example.com", req.Username)
	controller := &CardDAVController{}
	credential, err := controller.credentialForRequest(t.Context(), req)
	required.NoError(err)
	assertions.True(credential.Microsoft)
	assertions.False(credential.Google)
	assertions.Empty(credential.Password)
	credential.ConnectionGeneration = 1
	dir := t.TempDir()
	required.NoError(carddav.SaveCredential(dir, credential))
	saved, err := carddav.LoadCredential(dir)
	required.NoError(err)
	assertions.True(cardDAVCredentialMatchesConfig(saved, config.CardDAVConfig{Provider: "microsoft"}))
	assertions.False(cardDAVCredentialMatchesConfig(saved, config.CardDAVConfig{Provider: "google"}))
	assertions.False(cardDAVCredentialMatchesConfig(saved, config.CardDAVConfig{}))

	req.Password = "synthetic-password"
	required.Error(validateCardDAVAccountRequest(req))
	req.Password, req.OAuthApp = "", "contacts"
	required.Error(validateCardDAVAccountRequest(req), "an OAuth app is a Google setting")
}

func TestCardDAVMicrosoftServiceNeedsContactsToken(t *testing.T) {
	t.Parallel()
	controller := &CardDAVController{cfg: &config.Config{Microsoft: config.MicrosoftConfig{ClientID: "synthetic-client"}}}
	controller.cfg.Data.DataDir = t.TempDir()
	_, err := controller.microsoftContactsManager("person@example.com")
	require.ErrorIs(t, err, errMicrosoftContactsAuthorization)
}

func TestMicrosoftContactsTokenErrorMarksRevokedTokenUnauthorized(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	revoked := microsoftContactsTokenError(&oauth2.RetrieveError{ErrorCode: "invalid_grant"})
	require.ErrorIs(revoked, errMicrosoftContactsAuthorization)
	status, ok := errors.AsType[*carddav.StatusError](revoked)
	require.True(ok)
	require.Equal(http.StatusUnauthorized, status.StatusCode)

	unavailable := microsoftContactsTokenError(&oauth2.RetrieveError{ErrorCode: "temporarily_unavailable"})
	require.NotErrorIs(unavailable, errMicrosoftContactsAuthorization)
	_, ok = errors.AsType[*carddav.StatusError](unavailable)
	require.False(ok)
}
