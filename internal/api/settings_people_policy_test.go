package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestPeopleInferencePolicyUpdateKeepsCredentialsAndRevokesConsent(t *testing.T) {
	if !peoplesweep.StoredCredentialsSupported() {
		t.Skip("stored credentials require Unix permissions")
	}
	for _, scenario := range []struct {
		name      string
		failCheck bool
		change    string
	}{
		{name: "checked update"}, {name: "failed check rolls back", failCheck: true},
		{name: "credential changes during negotiation", change: "credential"},
		{name: "config changes during negotiation", change: "config"},
		{name: "concurrent host edit prevents rollback", failCheck: true, change: "check-config"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			failCheck := scenario.failCheck
			var duringRequest func(int32)
			var calls atomic.Int32
			gate := NewSerialOperationGate()
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _, held := gate.Holder()
				assertions.False(held, "external provider requests must not hold the archive operation gate")
				assertions.Equal("Bearer synthetic-key", r.Header.Get("Authorization"))
				var body struct {
					Model string `json:"model"`
				}
				data, err := io.ReadAll(r.Body)
				if !assertions.NoError(err) {
					return
				}
				if !assertions.NoError(json.Unmarshal(data, &body)) {
					return
				}
				assertions.NotContains(string(data), "archive-private-canary")
				call := calls.Add(1)
				if duringRequest != nil {
					duringRequest(call)
				}
				if call > 1 && failCheck {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				content := `{"ok":true}`
				if strings.Contains(string(data), `"claims"`) {
					content = `{"claims":[]}`
				}
				response := map[string]any{"model": body.Model, "choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 3}}
				assertions.NoError(json.NewEncoder(w).Encode(response))
			}))
			t.Cleanup(remote.Close)
			target, err := url.Parse(remote.URL)
			requirements.NoError(err)
			srv, path := newSettingsTestServer(t, "")
			st := testutil.NewTestStore(t)
			srv = NewServerWithOptions(ServerOptions{Config: srv.cfg, Store: st, Logger: srv.logger, OperationGate: gate})
			srv.peopleInferenceHTTPClient = &http.Client{Transport: peopleInferenceRewriteTransport{target: target}}
			before, err := config.ReadConfigFile(path)
			requirements.NoError(err)
			provider, err := peoplesweep.PresetProviderConfig("openrouter", "first-model")
			requirements.NoError(err)
			provider.RetentionPosture = "operator-confirmed"
			provider.TrainingPosture = "operator-confirmed"
			provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
			provider.SourceSince = "2025-01-01"
			provider.AllowSensitive = true
			created, err := personenrollment.NewService(path, st).CreateProfile(before.ETag, "remote", provider)
			requirements.NoError(err)
			credentials := peoplesweep.NewFileCredentialStore(srv.cfg.TokensDir())
			requirements.NoError(credentials.Save("remote", peoplesweep.NewCredential(peoplesweep.AuthBearer, "synthetic-key")))
			credentialRevision, _, err := credentials.Revision("remote")
			requirements.NoError(err)
			loaded, err := config.Load(path, srv.cfg.HomeDir)
			requirements.NoError(err)
			selected := loaded.People.Sweep
			selected.Enabled = true
			selected.Provider.Name = "remote"
			old, err := selected.Profile()
			requirements.NoError(err)
			_, err = st.EnsurePersonInferenceProfile(t.Context(), old)
			requirements.NoError(err)
			requirements.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{ProfileFingerprint: old.Fingerprint, CheckedAt: time.Now(), DriverVersion: old.DriverVersion, OutputMode: old.OutputMode, ModelVersion: old.Model}))
			_, _, err = st.GrantPersonInferenceConsent(t.Context(), old.Fingerprint, "synthetic-test")
			requirements.NoError(err)
			endpoint := peopleInferenceSettingsPath + "/providers/remote/policy"
			stale := performSettingsRequest(t, srv, http.MethodPatch, endpoint, []byte(`{"model":"second-model"}`), before.ETag, "")
			requirements.Equal(http.StatusPreconditionFailed, stale.Code, stale.Body.String())
			assertions.Equal(int32(0), calls.Load())
			rejected := performSettingsRequest(t, srv, http.MethodPatch, endpoint, []byte(`{"credential_env":"OTHER_KEY"}`), created.ETag, "")
			requirements.Equal(http.StatusBadRequest, rejected.Code, rejected.Body.String())
			assertions.Equal(int32(0), calls.Load())
			duringRequest = func(call int32) {
				if call == 1 && scenario.change == "credential" {
					requirements.NoError(credentials.Save("remote", peoplesweep.NewCredential(peoplesweep.AuthBearer, "replacement-key")))
				}
				if (call == 1 && scenario.change == "config") || (call == 2 && scenario.change == "check-config") {
					current, err := config.ReadConfigFile(path)
					requirements.NoError(err)
					_, err = config.EditConfigTables(path, current.ETag, []config.TableEdit{{Path: []string{"people", "sweep", "providers", "remote"}, Values: map[string]any{"model": "host-model"}}})
					requirements.NoError(err)
				}
			}
			response := performSettingsRequest(t, srv, http.MethodPatch, endpoint, []byte(`{"model":"second-model","allow_sensitive":false}`), created.ETag, "")
			if scenario.change == "credential" || scenario.change == "config" {
				expected := http.StatusConflict
				if scenario.change == "config" {
					expected = http.StatusPreconditionFailed
				}
				requirements.Equal(expected, response.Code, response.Body.String())
			} else if failCheck {
				expected := http.StatusBadGateway
				if scenario.change == "check-config" {
					expected = http.StatusInternalServerError
				}
				requirements.Equal(expected, response.Code, response.Body.String())
				var failure struct {
					ConsentRemainsRevoked     bool `json:"consent_remains_revoked"`
					RolledBack                bool `json:"rolled_back"`
					OperationMayHaveCompleted bool `json:"operation_may_have_completed"`
				}
				requirements.NoError(json.Unmarshal(response.Body.Bytes(), &failure))
				assertions.True(failure.ConsentRemainsRevoked)
				assertions.Equal(scenario.change != "check-config", failure.RolledBack)
				assertions.Equal(scenario.change == "check-config", failure.OperationMayHaveCompleted)
			} else {
				requirements.Equal(http.StatusOK, response.Code, response.Body.String())
				assertions.NotEmpty(response.Header().Get("ETag"))
			}
			current, err := config.Load(path, srv.cfg.HomeDir)
			requirements.NoError(err)
			updated := current.People.Sweep.Providers["remote"]
			assertions.Equal(provider.Credential, updated.Credential)
			assertions.Equal(provider.Endpoint, updated.Endpoint)
			assertions.Equal(provider.AllowedSources, updated.AllowedSources)
			if scenario.change == "config" || scenario.change == "check-config" {
				assertions.Equal("host-model", updated.Model)
			} else if failCheck || scenario.change == "credential" {
				assertions.Equal("first-model", updated.Model)
				assertions.True(updated.AllowSensitive)
			} else {
				assertions.Equal("second-model", updated.Model)
				assertions.False(updated.AllowSensitive)
			}
			afterRevision, _, err := credentials.Revision("remote")
			requirements.NoError(err)
			if scenario.change == "credential" {
				assertions.NotEqual(credentialRevision, afterRevision)
			} else {
				assertions.Equal(credentialRevision, afterRevision)
			}
			active, err := st.HasActivePersonInferenceConsent(t.Context(), old.Fingerprint)
			requirements.NoError(err)
			preflightConflict := scenario.change == "credential" || scenario.change == "config"
			assertions.Equal(preflightConflict, active)
			if preflightConflict {
				assertions.Equal(int32(1), calls.Load())
			} else {
				assertions.GreaterOrEqual(calls.Load(), int32(2))
			}
			assertions.NotContains(response.Body.String(), "synthetic-key")
		})
	}
}

func TestPeopleInferencePolicyUpdatePreservesCodexReleaseGate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	if peoplesweep.CodexReleaseAvailable() {
		t.Skip("release gate is open")
	}
	srv, path := newSettingsTestServer(t, "")
	st := testutil.NewTestStore(t)
	srv.store = st
	before, err := config.ReadConfigFile(path)
	requirements.NoError(err)
	provider := peoplesweep.ProviderConfig{Protocol: peoplesweep.ProtocolCodexAppServer, Model: "gpt-test", ReasoningEffort: "high", Auth: peoplesweep.AuthNone, Credential: peoplesweep.CredentialNone, OutputMode: peoplesweep.OutputModeNativeJSONSchema, Executable: "codex", ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1, RetentionPosture: "operator-confirmed", TrainingPosture: "operator-confirmed", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01", RequestTimeout: time.Minute}
	created, err := personenrollment.NewService(path, st).CreateProfile(before.ETag, "subscription", provider)
	requirements.NoError(err)
	response := performSettingsRequest(t, srv, http.MethodPatch, peopleInferenceSettingsPath+"/providers/subscription/policy", []byte(`{"model":"other-model"}`), created.ETag, "")
	requirements.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), "codex_unavailable")
	after, err := config.ReadConfigFile(path)
	requirements.NoError(err)
	assertions.Equal(created.ETag, after.ETag)
}
