package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestProviderCredentialRouteRejectsPeopleProviderID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t, storedRemotePeopleProviderTOML)
	st := testutil.NewTestStore(t)
	srv.store = st
	get := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	var initial PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(get.Body.Bytes(), &initial))
	require.Len(initial.Profiles, 1)
	saved := performSettingsRequest(t, srv, http.MethodPut, peopleInferenceSettingsPath+"/providers/remote/key",
		[]byte(`{"value":"synthetic-people-key"}`), initial.Profiles[0].CredentialRevision, "")
	require.Equal(http.StatusOK, saved.Code, saved.Body.String())

	configured, err := config.Load(path, "")
	require.NoError(err)
	selected := configured.People.Sweep
	selected.Enabled = true
	profile, err := selected.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(), DriverVersion: profile.DriverVersion,
		OutputMode: profile.OutputMode, ModelVersion: profile.Model,
	}))
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)

	credentials, err := providercredentials.Read(srv.cfg.TokensDir())
	require.NoError(err)
	route := "/api/v1/settings/provider-credentials/" + url.PathEscape(providercredentials.PeopleProviderID("remote"))
	put := performSettingsRequest(t, srv, http.MethodPut, route, []byte(`{"value":"replacement"}`), credentials.ETag, "")
	assert.Equal(http.StatusBadRequest, put.Code, put.Body.String())
	assert.Contains(put.Body.String(), "invalid_credential_id")
	deleted := performSettingsRequest(t, srv, http.MethodDelete, route, nil, credentials.ETag, "")
	assert.Equal(http.StatusBadRequest, deleted.Code, deleted.Body.String())
	assert.Contains(deleted.Body.String(), "invalid_credential_id")

	value, err := peoplesweep.NewStoredCredentials(srv.cfg.TokensDir()).Load("remote", profile.Endpoint)
	require.NoError(err)
	assert.Equal("synthetic-people-key", value)
	consented, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(consented)
}
