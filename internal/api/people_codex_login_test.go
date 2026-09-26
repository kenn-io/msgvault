package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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

type pendingCodexLoginClient struct {
	started    chan struct{}
	ended      chan struct{}
	cancelSeen chan struct{}
	finish     chan struct{}
}

type completedCodexLoginClient struct{}

func (completedCodexLoginClient) StartDeviceLogin(_ context.Context, present func(peoplesweep.DeviceLogin) error) error {
	return present(peoplesweep.DeviceLogin{
		VerificationURL: "https://example.test/device", UserCode: "ABCD-EFGH",
		ExpiresAt: time.Now().Add(time.Minute),
	})
}

func (completedCodexLoginClient) ListModels(context.Context) ([]peoplesweep.CodexModel, error) {
	return []peoplesweep.CodexModel{{ID: "gpt-test", SupportedEfforts: []string{"medium", "high"}}}, nil
}

func (c pendingCodexLoginClient) StartDeviceLogin(ctx context.Context, present func(peoplesweep.DeviceLogin) error) error {
	defer close(c.ended)
	if err := present(peoplesweep.DeviceLogin{
		VerificationURL: "https://example.test/device", UserCode: "ABCD-EFGH",
		ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		return err
	}
	close(c.started)
	<-ctx.Done()
	if c.cancelSeen != nil {
		close(c.cancelSeen)
	}
	if c.finish != nil {
		<-c.finish
	}
	return ctx.Err()
}

func (pendingCodexLoginClient) ListModels(context.Context) ([]peoplesweep.CodexModel, error) {
	return nil, nil
}

func TestPeopleCodexLoginsBindOwnerAndCancelActiveDeviceFlow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client := pendingCodexLoginClient{started: make(chan struct{}), ended: make(chan struct{})}
	manager := newPeopleCodexLogins(client, time.Now)
	draft, err := manager.Start("browser-a", "codex-main", nil)
	require.NoError(err)
	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		require.FailNow("device code was not presented")
	}
	_, err = manager.Get("browser-b", draft.ID)
	require.ErrorIs(err, peoplesweep.ErrEnrollmentDraftNotFound)
	preparationCalls := 0
	_, err = manager.Start("browser-b", "codex-other", func() error {
		preparationCalls++
		return nil
	})
	require.ErrorIs(err, errPeopleCodexLoginBusy)
	_, err = manager.Start("browser-b", "bad name", func() error {
		preparationCalls++
		return nil
	})
	require.Error(err)
	assert.Zero(preparationCalls)
	status, err := manager.Get("browser-a", draft.ID)
	require.NoError(err)
	assert.Equal("pending", status.state)
	assert.Equal("ABCD-EFGH", status.login.UserCode)
	require.ErrorIs(manager.Cancel("browser-b", draft.ID), peoplesweep.ErrEnrollmentDraftNotFound)
	require.NoError(manager.Cancel("browser-a", draft.ID))
	select {
	case <-client.ended:
	case <-time.After(5 * time.Second):
		require.FailNow("device login did not stop after cancellation")
	}
	_, err = manager.Get("browser-a", draft.ID)
	require.ErrorIs(err, peoplesweep.ErrEnrollmentDraftNotFound)
}

func TestPeopleCodexLoginsBlockSecondLoginWhileCompletedSessionUnconsumed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manager := newPeopleCodexLogins(completedCodexLoginClient{}, time.Now)
	draft, err := manager.Start("browser-a", "codex-main", nil)
	require.NoError(err)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := manager.Get("browser-a", draft.ID)
		require.NoError(err)
		if status.state == "complete" {
			break
		}
		if time.Now().After(deadline) {
			require.FailNow("device login did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	preparationCalls := 0
	_, err = manager.Start("browser-b", "codex-other", func() error {
		preparationCalls++
		return nil
	})
	require.ErrorIs(err, errPeopleCodexLoginBusy)
	assert.Zero(preparationCalls)
	// Consuming the completed session by cancelling its draft unblocks a
	// fresh ceremony.
	require.NoError(manager.Cancel("browser-a", draft.ID))
	second, err := manager.Start("browser-b", "codex-other", nil)
	require.NoError(err)
	require.NoError(manager.Cancel("browser-b", second.ID))
}

func TestPeopleCodexLoginCancellationWaitsForCredentialCommitToStop(t *testing.T) {
	require := require.New(t)
	client := pendingCodexLoginClient{
		started: make(chan struct{}), ended: make(chan struct{}),
		cancelSeen: make(chan struct{}), finish: make(chan struct{}),
	}
	manager := newPeopleCodexLogins(client, time.Now)
	draft, err := manager.Start("browser-a", "codex-main", nil)
	require.NoError(err)
	<-client.started
	result := make(chan error, 1)
	go func() { result <- manager.Cancel("browser-a", draft.ID) }()
	select {
	case <-client.cancelSeen:
	case <-time.After(5 * time.Second):
		require.FailNow("login cancellation did not reach client")
	}
	select {
	case err := <-result:
		require.FailNow("cancellation returned before client stopped", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(client.finish)
	require.NoError(<-result)
}

func TestPeopleCodexLoginAPIRoutesKeepDeviceCodeOutOfPollAndCancel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newSettingsTestServer(t, "")
	client := pendingCodexLoginClient{started: make(chan struct{}), ended: make(chan struct{})}
	srv.peopleCodexLogins = newPeopleCodexLogins(client, time.Now)
	started := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/codex/login", []byte(`{"name":"codex-main"}`), "", "")
	require.Equal(http.StatusOK, started.Code, started.Body.String())
	var login PeopleCodexLoginResponse
	require.NoError(json.Unmarshal(started.Body.Bytes(), &login))
	assert.Equal("https://example.test/device", login.VerificationURL)
	assert.Equal("ABCD-EFGH", login.UserCode)
	assert.NotEmpty(login.SessionID)
	poll := performSettingsRequest(t, srv, http.MethodGet,
		peopleInferenceSettingsPath+"/codex/login/"+login.SessionID, nil, "", "")
	require.Equal(http.StatusOK, poll.Code, poll.Body.String())
	assert.Contains(poll.Body.String(), `"state":"pending"`)
	assert.NotContains(poll.Body.String(), login.UserCode)
	models := performSettingsRequest(t, srv, http.MethodGet,
		peopleInferenceSettingsPath+"/codex/login/"+login.SessionID+"/models", nil, "", "")
	assert.Equal(http.StatusConflict, models.Code)
	cancelled := performSettingsRequest(t, srv, http.MethodDelete,
		peopleInferenceSettingsPath+"/codex/login/"+login.SessionID, nil, "", "")
	require.Equal(http.StatusOK, cancelled.Code, cancelled.Body.String())
	assert.NotContains(cancelled.Body.String(), login.UserCode)
	select {
	case <-client.ended:
	case <-time.After(5 * time.Second):
		require.FailNow("device login did not stop after API cancellation")
	}
}

func TestPeopleCodexLoginAPICreatesProfileOnlyAfterExactModelDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("the private codex auth home layout requires Unix permission bits")
	}
	srv, _ := newSettingsTestServer(t, "")
	srv.peopleCodexLogins = newPeopleCodexLogins(completedCodexLoginClient{}, time.Now)
	started := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/codex/login", []byte(`{"name":"subscription"}`), "", "")
	require.Equal(http.StatusOK, started.Code, started.Body.String())
	var login PeopleCodexLoginResponse
	require.NoError(json.Unmarshal(started.Body.Bytes(), &login))
	settings := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, settings.Code)
	badModel := performSettingsRequest(t, srv, http.MethodPut,
		peopleInferenceSettingsPath+"/codex/login/"+login.SessionID+"/profile",
		[]byte(`{"model":"unknown","reasoning_effort":"high","retention_posture":"operator-confirmed","training_posture":"operator-confirmed","allowed_sources":["conversation_text"],"source_since":"2025-01-01","allow_sensitive":false}`),
		settings.Header().Get("ETag"), "")
	assert.Equal(http.StatusUnprocessableEntity, badModel.Code)
	created := performSettingsRequest(t, srv, http.MethodPut,
		peopleInferenceSettingsPath+"/codex/login/"+login.SessionID+"/profile",
		[]byte(`{"model":"gpt-test","reasoning_effort":"high","retention_posture":"operator-confirmed","training_posture":"operator-confirmed","allowed_sources":["conversation_text"],"source_since":"2025-01-01","allow_sensitive":false}`),
		settings.Header().Get("ETag"), "")
	require.Equal(http.StatusOK, created.Code, created.Body.String())
	var response PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(created.Body.Bytes(), &response))
	var found bool
	for _, profile := range response.Profiles {
		if profile.Name == "subscription" {
			found = true
			assert.Equal(string(peoplesweep.ProtocolCodexAppServer), profile.Protocol)
			assert.Equal("gpt-test", profile.Model)
			assert.False(profile.CredentialConfigured)
			assert.False(profile.ConsentActive)
		}
	}
	assert.True(found)
	authHome := filepath.Join(srv.cfg.TokensDir(), "people-codex")
	require.NoError(os.MkdirAll(authHome, 0o700))
	require.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), []byte(`{"synthetic":true}`), 0o600))
	read := performSettingsRequest(t, srv, http.MethodGet, peopleInferenceSettingsPath, nil, "", "")
	require.Equal(http.StatusOK, read.Code)
	var authenticated PeopleInferenceSettingsResponse
	require.NoError(json.Unmarshal(read.Body.Bytes(), &authenticated))
	for _, profile := range authenticated.Profiles {
		if profile.Name == "subscription" {
			assert.True(profile.CredentialConfigured)
		}
	}
}

func TestPeopleCodexLoginRevokesPriorAccountAuthorityBeforeDeviceFlow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, path := newSettingsTestServer(t, "")
	st := testutil.NewTestStore(t)
	srv.store = st
	before, err := config.ReadConfigFile(path)
	require.NoError(err)
	provider := peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		Auth: peoplesweep.AuthNone, Credential: peoplesweep.CredentialNone,
		OutputMode: peoplesweep.OutputModeNativeJSONSchema, Executable: "codex",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
		RetentionPosture:  "operator-confirmed", TrainingPosture: "operator-confirmed",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Minute,
	}
	_, err = personenrollment.NewService(path, st).CreateProfile(before.ETag, "subscription", provider)
	require.NoError(err)
	configured, err := config.Load(path, "")
	require.NoError(err)
	profileConfig := configured.People.Sweep
	profileConfig.Provider = peoplesweep.ProviderSelection{Name: "subscription"}
	profileConfig.Enabled = true
	profile, err := profileConfig.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(),
		DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode,
		ModelVersion: profile.Model,
	}))
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	srv.peopleCodexLogins = newPeopleCodexLogins(completedCodexLoginClient{}, time.Now)
	invalid := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/codex/login", []byte(`{"name":"bad name"}`), "", "")
	assert.Equal(http.StatusBadRequest, invalid.Code)
	stillConsented, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(stillConsented)
	started := performSettingsRequest(t, srv, http.MethodPost,
		peopleInferenceSettingsPath+"/codex/login", []byte(`{"name":"replacement"}`), "", "")
	require.Equal(http.StatusOK, started.Code, started.Body.String())
	consented, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(consented)
	checked, err := st.HasSuccessfulPersonInferenceCheck(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.False(checked)
}

func TestPeopleCodexLoginManagerUsesPrivateDaemonAuthHome(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if runtime.GOOS != "linux" {
		t.Skip("Codex enrollment launcher is Linux-only")
	}
	srv, _ := newSettingsTestServer(t, "")
	manager, err := srv.codexLoginManager()
	require.NoError(err)
	require.NotNil(manager)
	authHome := filepath.Join(srv.cfg.TokensDir(), "people-codex")
	info, err := os.Lstat(authHome)
	require.NoError(err)
	assert.True(info.IsDir())
	assert.Equal(os.FileMode(0o700), info.Mode().Perm())
	_, err = os.Lstat(filepath.Join(authHome, "auth.json"))
	assert.ErrorIs(err, os.ErrNotExist)
}
