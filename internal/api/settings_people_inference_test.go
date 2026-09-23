package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrollment"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const peopleInferenceSettingsPath = "/api/v1/settings/people-inference"

func TestPeopleInferenceSettingsReportsConfiguredAndRunningState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("TEST_KEY_ENV_NAME", "synthetic-private-api-key")
	initial := peopleInferenceSettingsConfig("gpt-first", true)
	srv, path := newSettingsTestServer(t, initial)
	require.NoError(os.WriteFile(path, []byte(peopleInferenceSettingsConfig("gpt-second", false)), 0o600))

	resp := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	assert.Equal("no-store", resp.Header().Get("Cache-Control"))
	var body PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assert.False(body.ConfiguredEnabled)
	assert.True(body.RunningEnabled)
	assert.True(body.PendingRestart)
	assert.Equal("primary", body.ConfiguredName)
	assert.Equal("primary", body.RunningName)
	require.Len(body.Profiles, 1)
	assert.Equal("gpt-second", body.Profiles[0].Model)
	assert.Equal("TEST_KEY_ENV_NAME", body.Profiles[0].CredentialEnv)
	assert.True(body.Profiles[0].CredentialConfigured)
	assert.True(body.Profiles[0].Selected)
	assert.NotEmpty(body.ConfiguredFingerprint)
	assert.NotEmpty(body.RunningFingerprint)
	assert.NotEqual(body.ConfiguredFingerprint, body.RunningFingerprint)
	assert.NotContains(resp.Body.String(), "synthetic-private-api-key")
}

func TestPeopleInferenceSelectionRequiresCheckAndConsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t, "")
	st := testutil.NewTestStore(t)
	srv.store = st
	before, err := config.ReadConfigFile(path)
	require.NoError(err)
	service := personenrollment.NewService(path, st)
	created, err := service.CreateProfile(before.ETag, "remote", completeAPIProvider("EXAMPLE_KEY", "example-model"))
	require.NoError(err)
	request := []byte(`{"name":"remote"}`)
	selectPath := peopleInferenceSettingsPath + "/select"
	resp := performSettingsRequest(t, srv, http.MethodPost, selectPath, request, created.ETag, "")
	assert.Equal(http.StatusConflict, resp.Code, resp.Body.String())

	configured, err := config.Load(path, "")
	require.NoError(err)
	selected := configured.People.Sweep
	selected.Enabled = true
	selected.Provider.Name = "remote"
	profile, err := selected.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(context.Background(), profile)
	require.NoError(err)
	require.NoError(st.RecordPersonInferenceCheck(context.Background(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(),
		DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode,
		ModelVersion: profile.Model,
	}))
	resp = performSettingsRequest(t, srv, http.MethodPost, selectPath, request, created.ETag, "")
	assert.Equal(http.StatusConflict, resp.Code, resp.Body.String())
	_, _, err = st.GrantPersonInferenceConsent(context.Background(), profile.Fingerprint, "test")
	require.NoError(err)
	resp = performSettingsRequest(t, srv, http.MethodPost, selectPath, request, created.ETag, "")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assert.Equal("remote", body.ConfiguredName)
	assert.True(body.ConfiguredEnabled)
	assert.Equal(profile.Fingerprint, body.ConfiguredFingerprint)
	assert.True(body.PendingRestart)
	assert.NotEmpty(resp.Header().Get("ETag"))
}

func TestPeopleInferencePresetCreationUsesBoundEndpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newSettingsTestServer(t, "")
	read := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	request := []byte(`{"preset_id":"openrouter","model":"example/model","retention_posture":"operator-confirmed","training_posture":"operator-confirmed","allowed_sources":["conversation_text"],"source_since":"2025-01-01","allow_sensitive":false}`)
	created := performSettingsRequest(t, srv, http.MethodPut,
		peopleInferenceSettingsPath+"/providers/remote", request, read.Header().Get("ETag"), "")
	require.Equal(http.StatusOK, created.Code, created.Body.String())
	var body PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(created.Body.Bytes(), &body))
	assert.False(body.ConfiguredEnabled)
	assert.True(body.PendingRestart)
	require.Len(body.Profiles, 1)
	var found *PeopleInferenceProfileSetting
	for index := range body.Profiles {
		if body.Profiles[index].Name == "remote" {
			found = &body.Profiles[index]
		}
	}
	require.NotNil(found)
	assert.Equal("https://openrouter.ai/api/v1", found.Endpoint)
	assert.Equal("openrouter", found.PresetID)
	assert.Equal("stored", found.CredentialSource)
	assert.NotEmpty(found.Fingerprint)
	assert.NotContains(created.Body.String(), "api_key")
	stale := performSettingsRequest(t, srv, http.MethodPut,
		peopleInferenceSettingsPath+"/providers/other", request, read.Header().Get("ETag"), "")
	assert.Equal(http.StatusPreconditionFailed, stale.Code)
}

func TestPeopleInferencePresetAcceptsDaemonEnvironmentReference(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Setenv("TEST_PEOPLE_PROVIDER_KEY", "synthetic-key")
	srv, _ := newSettingsTestServer(t, "")
	read := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, read.Code)
	request := []byte(`{"preset_id":"venice","model":"example/model","credential_env":"TEST_PEOPLE_PROVIDER_KEY","retention_posture":"operator-confirmed","training_posture":"operator-confirmed","allowed_sources":["conversation_text"],"source_since":"2025-01-01","allow_sensitive":false}`)
	created := performSettingsRequest(t, srv, http.MethodPut,
		peopleInferenceSettingsPath+"/providers/from-env", request, read.Header().Get("ETag"), "")
	require.Equal(http.StatusOK, created.Code, created.Body.String())
	var body PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(created.Body.Bytes(), &body))
	require.Len(body.Profiles, 1)
	assert.Equal("env", body.Profiles[0].CredentialSource)
	assert.Equal("TEST_PEOPLE_PROVIDER_KEY", body.Profiles[0].CredentialEnv)
	assert.True(body.Profiles[0].CredentialConfigured)
	assert.Empty(body.Profiles[0].CredentialRevision)
	assert.NotContains(created.Body.String(), "synthetic-key")
	key := performSettingsRequest(t, srv, http.MethodPut,
		peopleInferenceSettingsPath+"/providers/from-env/key", []byte(`{"value":"other"}`), `"revision"`, "")
	assert.Equal(http.StatusNotFound, key.Code)
}

type failingPeopleInferenceStore struct{ *store.Store }

func (f failingPeopleInferenceStore) HasSuccessfulPersonInferenceCheck(context.Context, string) (bool, error) {
	return false, errors.New("database unavailable")
}

func TestPeopleInferenceSelectionReportsStoreFailureAsServerError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t, "")
	srv.store = failingPeopleInferenceStore{Store: testutil.NewTestStore(t)}
	before, err := config.ReadConfigFile(path)
	require.NoError(err)
	service := personenrollment.NewService(path, nil)
	created, err := service.CreateProfile(before.ETag, "remote", completeAPIProvider("EXAMPLE_KEY", "example-model"))
	require.NoError(err)
	resp := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/select", []byte(`{"name":"remote"}`), created.ETag, "")
	assert.Equal(http.StatusInternalServerError, resp.Code)
	assert.NotContains(resp.Body.String(), "database unavailable")
}

func TestPeopleInferenceKeyWriteUsesSeparateRevisionAndInvalidatesAuthority(t *testing.T) {
	if !peoplesweep.StoredCredentialsSupported() {
		t.Skip("stored people provider credentials are unsupported on this platform")
	}
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t, "")
	st := testutil.NewTestStore(t)
	srv.store = st
	before, err := config.ReadConfigFile(path)
	require.NoError(err)
	service := personenrollment.NewService(path, st)
	provider, err := peoplesweep.PresetProviderConfig("openrouter", "example-model")
	require.NoError(err)
	provider.RetentionPosture = "operator-confirmed"
	provider.TrainingPosture = "operator-confirmed"
	provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
	provider.SourceSince = "2025-01-01"
	created, err := service.CreateProfile(before.ETag, "remote", provider)
	require.NoError(err)
	get := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	var status PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(get.Body.Bytes(), &status))
	require.Len(status.Profiles, 1)
	initialRevision := status.Profiles[0].CredentialRevision
	assert.NotEmpty(initialRevision)
	assert.False(status.Profiles[0].CredentialConfigured)
	assert.Equal(created.ETag, get.Header().Get("ETag"))

	pathKey := peopleInferenceSettingsPath + "/providers/remote/key"
	write := func(revision, secret string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, pathKey, strings.NewReader(`{"value":"`+secret+`"}`))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("If-Match", revision)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		return response
	}
	first := write(initialRevision, "first-secret")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	assert.NotContains(first.Body.String(), "first-secret")
	var firstStatus PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(first.Body.Bytes(), &firstStatus))
	firstRevision := firstStatus.Profiles[0].CredentialRevision
	assert.NotEqual(initialRevision, firstRevision)
	assert.True(firstStatus.Profiles[0].CredentialConfigured)
	stale := write(initialRevision, "stale-secret")
	assert.Equal(http.StatusPreconditionFailed, stale.Code)
	credential, err := peoplesweep.NewFileCredentialStore(srv.cfg.TokensDir()).Load("remote")
	require.NoError(err)
	assert.Equal("first-secret", credential.Value())

	configured, err := config.Load(path, "")
	require.NoError(err)
	selected := configured.People.Sweep
	selected.Enabled = true
	selected.Provider.Name = "remote"
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
	second := write(firstRevision, "second-secret")
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	checked, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(checked)
	consented, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(consented)
	var secondStatus PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(second.Body.Bytes(), &secondStatus))
	secondRevision := secondStatus.Profiles[0].CredentialRevision
	require.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(), DriverVersion: profile.DriverVersion,
		OutputMode: profile.OutputMode, ModelVersion: profile.Model,
	}))
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	removed := performSettingsRequest(t, srv, http.MethodDelete, pathKey, nil, secondRevision, "")
	require.Equal(http.StatusOK, removed.Code, removed.Body.String())
	var removedStatus PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(removed.Body.Bytes(), &removedStatus))
	assert.False(removedStatus.Profiles[0].CredentialConfigured)
	assert.NotEqual(secondRevision, removedStatus.Profiles[0].CredentialRevision)
	_, err = peoplesweep.NewFileCredentialStore(srv.cfg.TokensDir()).Load("remote")
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
	checked, err = st.HasSuccessfulPersonInferenceCheck(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(checked)
	consented, err = st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(consented)
}

type peopleInferenceRewriteTransport struct{ target *url.URL }

func (t peopleInferenceRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	copyURL := *request.URL
	copyURL.Scheme = t.target.Scheme
	copyURL.Host = t.target.Host
	cloned.URL = &copyURL
	return http.DefaultTransport.RoundTrip(cloned)
}

func TestPeopleInferenceAPICheckConsentAndSelectUsesSyntheticProviderPath(t *testing.T) {
	if !peoplesweep.StoredCredentialsSupported() {
		t.Skip("stored people provider credentials are unsupported on this platform")
	}
	assert := assert.New(t)
	require := require.New(t)
	var seen atomic.Bool
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(true)
		assert.Equal("/api/v1/chat/completions", r.URL.Path)
		assert.Equal("Bearer synthetic-key", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(err) {
			return
		}
		assert.Contains(string(body), "Return an object with ok set to true")
		assert.NotContains(string(body), "archive-private-canary")
		_, err = io.WriteString(w, `{"model":"example-model","choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
		assert.NoError(err)
	}))
	defer providerServer.Close()
	target, err := url.Parse(providerServer.URL)
	require.NoError(err)

	srv, path := newSettingsTestServer(t, "")
	st := testutil.NewTestStore(t)
	srv.store = st
	srv.peopleInferenceHTTPClient = &http.Client{Transport: peopleInferenceRewriteTransport{target: target}}
	before, err := config.ReadConfigFile(path)
	require.NoError(err)
	provider, err := peoplesweep.PresetProviderConfig("openrouter", "example-model")
	require.NoError(err)
	provider.RetentionPosture = "operator-confirmed"
	provider.TrainingPosture = "operator-confirmed"
	provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
	provider.SourceSince = "2025-01-01"
	created, err := personenrollment.NewService(path, st).CreateProfile(before.ETag, "remote", provider)
	require.NoError(err)
	get := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, get.Code)
	var status PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(get.Body.Bytes(), &status))
	require.Len(status.Profiles, 1)
	revision := status.Profiles[0].CredentialRevision
	keyRequest := httptest.NewRequest(http.MethodPut,
		peopleInferenceSettingsPath+"/providers/remote/key", strings.NewReader(`{"value":"synthetic-key"}`))
	keyRequest.RemoteAddr = "127.0.0.1:12345"
	keyRequest.Header.Set("Content-Type", "application/json")
	keyRequest.Header.Set("If-Match", revision)
	keyResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(keyResponse, keyRequest)
	require.Equal(http.StatusOK, keyResponse.Code, keyResponse.Body.String())

	check := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/providers/remote/check", nil, created.ETag, "")
	require.Equal(http.StatusOK, check.Code, check.Body.String())
	assert.True(seen.Load())
	var checked PeopleInferenceCheckResponse
	require.NoError(json.Unmarshal(check.Body.Bytes(), &checked))
	assert.True(checked.OK)
	assert.NotEmpty(checked.Fingerprint)
	assert.Equal("example-model", checked.Model)
	consent := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/providers/remote/consent",
		[]byte(`{"fingerprint":"`+checked.Fingerprint+`","confirmed":true}`), created.ETag, "")
	require.Equal(http.StatusOK, consent.Code, consent.Body.String())
	var consentStatus PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(consent.Body.Bytes(), &consentStatus))
	require.Len(consentStatus.Profiles, 1)
	assert.True(consentStatus.Profiles[0].Checked)
	assert.True(consentStatus.Profiles[0].ConsentActive)
	selected := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/select", []byte(`{"name":"remote"}`), created.ETag, "")
	require.Equal(http.StatusOK, selected.Code, selected.Body.String())
	selectedETag := selected.Header().Get("ETag")
	runningConfig, err := config.Load(path, "")
	require.NoError(err)
	srv.cfg.People.Sweep = runningConfig.People.Sweep
	runningProvider := srv.cfg.People.Sweep.Providers["remote"]
	runningProvider.Model = "older-model"
	srv.cfg.People.Sweep.Providers["remote"] = runningProvider
	runningProfile, err := srv.cfg.People.Sweep.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), runningProfile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), runningProfile.Fingerprint, "test")
	require.NoError(err)
	revoked := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/providers/remote/revoke", nil, selectedETag, "")
	require.Equal(http.StatusOK, revoked.Code, revoked.Body.String())
	var revokedStatus PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(revoked.Body.Bytes(), &revokedStatus))
	assert.False(revokedStatus.Profiles[0].ConsentActive)
	runningConsent, err := st.HasActivePersonInferenceConsent(t.Context(), runningProfile.Fingerprint)
	require.NoError(err)
	assert.False(runningConsent)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), checked.Fingerprint, "test")
	require.NoError(err)
	disabled := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/disable", nil, selectedETag, "")
	require.Equal(http.StatusOK, disabled.Code, disabled.Body.String())
	var disabledStatus PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(disabled.Body.Bytes(), &disabledStatus))
	assert.False(disabledStatus.ConfiguredEnabled)
	consented, err := st.HasActivePersonInferenceConsent(t.Context(), checked.Fingerprint)
	require.NoError(err)
	assert.False(consented)
	blockedRemove := performSettingsRequest(t, srv, http.MethodDelete,
		peopleInferenceSettingsPath+"/providers/remote", nil, disabled.Header().Get("ETag"), "")
	assert.Equal(http.StatusConflict, blockedRemove.Code)
	invalidRemove := performSettingsRequest(t, srv, http.MethodDelete,
		peopleInferenceSettingsPath+"/providers/bad%20name", nil, disabled.Header().Get("ETag"), "")
	assert.Equal(http.StatusBadRequest, invalidRemove.Code)
	backup, err := personenrollment.NewService(path, st).CreateProfile(disabled.Header().Get("ETag"), "backup", provider)
	require.NoError(err)
	removed := performSettingsRequest(t, srv, http.MethodDelete,
		peopleInferenceSettingsPath+"/providers/remote", nil, backup.ETag, "")
	require.Equal(http.StatusOK, removed.Code, removed.Body.String())
	var removedStatus PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(removed.Body.Bytes(), &removedStatus))
	require.Len(removedStatus.Profiles, 1)
	assert.Equal("backup", removedStatus.Profiles[0].Name)
	credentials := peoplesweep.NewFileCredentialStore(srv.cfg.TokensDir())
	_, err = credentials.Load("remote")
	assert.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
}

func TestPeopleInferenceKeyMutationDoesNotRequireRestart(t *testing.T) {
	if !peoplesweep.StoredCredentialsSupported() {
		t.Skip("stored people provider credentials are unsupported on this platform")
	}
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newSettingsTestServer(t, `[people.sweep]
enabled = false
provider = "remote"

[people.sweep.providers.remote]
preset_id = "openrouter"
protocol = "openai_chat"
endpoint = "https://openrouter.ai/api/v1"
model = "example-model"
auth = "bearer"
credential = "stored"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "operator-confirmed"
training_posture = "operator-confirmed"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
request_timeout = "1m"
`)
	srv.store = testutil.NewTestStore(t)
	get := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, get.Code, get.Body.String())
	var initial PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(get.Body.Bytes(), &initial))
	require.Len(initial.Profiles, 1)
	assert.False(initial.PendingRestart)
	pathKey := peopleInferenceSettingsPath + "/providers/remote/key"
	request := httptest.NewRequest(http.MethodPut, pathKey, strings.NewReader(`{"value":"synthetic-key"}`))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", initial.Profiles[0].CredentialRevision)
	written := httptest.NewRecorder()
	srv.Router().ServeHTTP(written, request)
	require.Equal(http.StatusOK, written.Code, written.Body.String())
	var afterWrite PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(written.Body.Bytes(), &afterWrite))
	assert.False(afterWrite.PendingRestart)
	removed := performSettingsRequest(t, srv, http.MethodDelete, pathKey, nil,
		afterWrite.Profiles[0].CredentialRevision, "")
	require.Equal(http.StatusOK, removed.Code, removed.Body.String())
	var afterDelete PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(removed.Body.Bytes(), &afterDelete))
	assert.False(afterDelete.PendingRestart)
}

func peopleInferenceSettingsConfig(model string, enabled bool) string {
	status := "false"
	if enabled {
		status = "true"
	}
	content := strings.ReplaceAll(`[people.sweep]
enabled = ENABLED
provider = "primary"

[people.sweep.providers.primary]
protocol = "openai_chat"
endpoint = "https://api.example.test/v1"
model = "MODEL"
auth = "bearer"
credential = "env"
credential_env = "TEST_KEY_ENV_NAME"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
request_timeout = "45s"
`, "ENABLED", status)
	return strings.ReplaceAll(content, "MODEL", model)
}
