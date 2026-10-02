package cmd

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestMCPProviderPresetSelectionPreservesExactETagAndCredentialsBoundary(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte(""), 0600))
	cfg, err := config.Load(path, "")
	requirements.NoError(err)
	st, _ := mcpSQLiteStoreWithPath(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_people_provider_settings", nil)
	requirements.NoError(err)
	settings := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	requirements.NotEmpty(settings.ETag)
	args := map[string]any{"name": "remote", "etag": settings.ETag, "preset_id": "openrouter", "model": "synthetic-model", "retention_posture": "operator-confirmed", "training_posture": "operator-confirmed", "allowed_sources": []string{"conversation_text"}, "source_since": "2025-01-01", "allow_sensitive": false}
	disclosure, err := backend.OperationDisclosure(t.Context(), "create_people_provider_preset", args)
	requirements.NoError(err)
	assertions.Contains(disclosure, "openrouter")
	result, err = backend.ExecuteOperation(t.Context(), "create_people_provider_preset", args)
	requirements.NoError(err)
	created := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	requirements.Len(created.Profiles, 1)
	assertions.False(created.Profiles[0].CredentialConfigured)
	assertions.NotEqual(settings.ETag, created.ETag)
	result, err = backend.ExecuteOperation(t.Context(), "select_people_provider", map[string]any{"name": "remote", "etag": settings.ETag})
	requirements.NoError(err)
	requirements.True(result.IsError)
	loaded, err := config.Load(path, cfg.HomeDir)
	requirements.NoError(err)
	profileConfig := loaded.People.Sweep
	profileConfig.Enabled = true
	profileConfig.Provider.Name = "remote"
	profile, err := profileConfig.Profile()
	requirements.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)
	requirements.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(), DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: profile.Model}))
	result, err = backend.ExecuteOperation(t.Context(), "consent_people_provider", map[string]any{"name": "remote", "etag": created.ETag, "fingerprint": profile.Fingerprint})
	requirements.NoError(err)
	consented := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	assertions.True(consented.Profiles[0].ConsentActive)
	result, err = backend.ExecuteOperation(t.Context(), "select_people_provider", map[string]any{"name": "remote", "etag": created.ETag})
	requirements.NoError(err)
	selected := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	assertions.Equal("remote", selected.ConfiguredName)
	assertions.True(selected.PendingRestart)
	assertions.False(selected.RunningEnabled)
	for _, key := range []string{"api_key", "endpoint", "credential_env", "env", "command"} {
		invalid := map[string]any{"name": "remote", "etag": selected.ETag, "model": "other", key: "synthetic-secret"}
		result, err := backend.ExecuteOperation(t.Context(), "update_people_provider_policy", invalid)
		requirements.NoError(err)
		requirements.True(result.IsError)
	}
	result, err = backend.ExecuteOperation(t.Context(), "revoke_people_provider_consent", map[string]any{"name": "remote", "etag": selected.ETag})
	requirements.NoError(err)
	revoked := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	assertions.False(revoked.Profiles[0].ConsentActive)
	result, err = backend.ExecuteOperation(t.Context(), "disable_people_inference", map[string]any{"etag": revoked.ETag})
	requirements.NoError(err)
	disabled := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	assertions.False(disabled.ConfiguredEnabled)
	args["name"], args["etag"] = "other", disabled.ETag
	result, err = backend.ExecuteOperation(t.Context(), "create_people_provider_preset", args)
	requirements.NoError(err)
	added := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	result, err = backend.ExecuteOperation(t.Context(), "remove_people_provider", map[string]any{"name": "remote", "etag": added.ETag})
	requirements.NoError(err)
	removed := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	requirements.Len(removed.Profiles, 1)
	assertions.Equal("other", removed.Profiles[0].Name)
}

func TestMCPProviderStatusAndHistoryUseRealCLIAndStore(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.People.Sweep = personProviderTestConfig()
	requirements.NoError(cfg.Save())
	st, _ := mcpSQLiteStoreWithPath(t)
	requests := make(chan api.CLIRunRequest, 4)
	adapter := &inProcessPersonProviderDaemonStore{storeAPIAdapter: &storeAPIAdapter{store: st, mcpCommands: registeredMCPCommandDescriptors()}, config: cfg.People.Sweep, requests: requests}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_people_provider_status", map[string]any{"name": cfg.People.Sweep.Provider.Name})
	requirements.NoError(err)
	status := operationOutput[mcpserver.PeopleProviderStatus](t, result)
	assertions.NotEmpty(status.Policy.Fingerprint)
	assertions.Equal("bearer", status.Policy.Auth)
	assertions.Equal(status.Policy.Fingerprint, status.Consent.Fingerprint)
	assertions.NotEmpty(status.Settings.ETag)
	requirements.Len(requests, 1)
	request := <-requests
	assertions.Empty(request.Env)
	assertions.Empty(request.Cwd)
	assertions.False(request.GrantDecided)
	result, err = backend.ExecuteOperation(t.Context(), "list_people_provider_history", map[string]any{"name": cfg.People.Sweep.Provider.Name, "person_id": int64(42), "limit": 5})
	requirements.NoError(err)
	history := operationOutput[mcpserver.PeopleProviderHistory](t, result)
	assertions.Empty(history.Runs)
	assertions.Empty(history.Attempts)
	requirements.Len(requests, 1)
	request = <-requests
	assertions.Contains(request.Args, "--person=42")
	assertions.Contains(request.Args, "--limit=5")
	session := operationMCPSession(t, backend, nil, nil)
	for _, name := range []string{"get_people_provider_status", "list_people_provider_history"} {
		called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{"name": cfg.People.Sweep.Provider.Name}})
		requirements.NoError(err)
		requirements.False(called.IsError)
	}
}

func TestMCPProviderSDKApprovalDeclinePreventsPolicyCreation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, nil, 0600))
	cfg, err := config.Load(path, "")
	requirements.NoError(err)
	st, _ := mcpSQLiteStoreWithPath(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_people_provider_settings", nil)
	requirements.NoError(err)
	settings := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	args := map[string]any{"name": "remote", "etag": settings.ETag, "preset_id": "openrouter", "model": "synthetic-model", "retention_posture": "operator-confirmed", "training_posture": "operator-confirmed", "allowed_sources": []string{"conversation_text"}, "source_since": "2025-01-01", "allow_sensitive": false}
	action := "decline"
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyProviders}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assertions.Contains(request.Params.Message, "openrouter.ai")
		approvals++
		return &sdkmcp.ElicitResult{Action: action, Content: map[string]any{"confirm": true}}, nil
	}})
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_people_provider_preset", Arguments: args})
	requirements.NoError(err)
	assertions.True(called.IsError)
	current, err := config.Load(path, cfg.HomeDir)
	requirements.NoError(err)
	assertions.NotContains(current.People.Sweep.Providers, "remote")
	unchanged, err := config.ReadConfigFile(path)
	requirements.NoError(err)
	assertions.Equal(settings.ETag, unchanged.ETag)
	action = "accept"
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_people_provider_preset", Arguments: args})
	requirements.NoError(err)
	requirements.False(called.IsError)
	assertions.Equal(2, approvals)
	current, err = config.Load(path, cfg.HomeDir)
	requirements.NoError(err)
	assertions.Contains(current.People.Sweep.Providers, "remote")
}

func TestMCPProviderPolicyFailureRetainsRollbackFactsAndHidesDiagnostics(t *testing.T) {
	for _, body := range []string{
		`{"error":"provider_check_failed","consent_remains_revoked":true,"rolled_back":true,"operation_may_have_completed":false,"message":"private-provider-canary"}`,
		`{"error":"provider_policy_update_failed","consent_remains_revoked":true,"rolled_back":false,"operation_may_have_completed":true,"message":"private-provider-canary"}`,
		`{"error":"private-provider-canary","message":"private-provider-canary"}`,
	} {
		t.Run(body, func(t *testing.T) {
			// A daemon failure receipt is a stable external contract. No CLI or policy
			// transaction is stubbed; their behavior is exercised by the owning tests.
			requirements := require.New(t)
			assertions := assert.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertions.Equal(http.MethodPatch, r.Method)
				assertions.Equal(peopleProviderSettingsPath+"/providers/remote/policy", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
				_, err := io.WriteString(w, body)
				assertions.NoError(err)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			backend := &daemonMCPOperations{client: client, supported: []string{"update_people_provider_policy"}}
			result, err := backend.ExecuteOperation(t.Context(), "update_people_provider_policy", map[string]any{"name": "remote", "etag": `"revision"`, "model": "synthetic-model"})
			requirements.NoError(err)
			requirements.True(result.IsError)
			data, err := json.Marshal(result.Output)
			requirements.NoError(err)
			assertions.NotContains(string(data), "private-provider-canary")
			var output map[string]any
			requirements.NoError(json.Unmarshal(data, &output))
			if strings.Contains(body, `"rolled_back":true`) {
				assertions.Equal(false, output["operation_may_have_completed"])
				assertions.Equal(true, output["consent_remains_revoked"])
			} else {
				assertions.Equal(true, output["operation_may_have_completed"])
			}
		})
	}
}

func FuzzMCPProviderNameUsesProductionCobraParser(f *testing.F) {
	for _, name := range []string{"remote", "default", "--json", "-x", "", "source.example", "hello\x00"} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		input, err := mcpProviderArguments("get_people_provider_status", map[string]any{"name": name})
		command := newPersonProviderStatusCommand(personProviderCommandDeps{})
		if err != nil {
			assert.Error(t, optionalPersonProviderNameArgs(command, []string{name}))
			return
		}
		argv := []string{input.Name, "--json"}
		require.NoError(t, command.ParseFlags(argv))
		parsed := command.Flags().Args()
		require.Equal(t, []string{name}, parsed)
		require.NoError(t, command.Args(command, parsed))
		jsonOutput, err := command.Flags().GetBool("json")
		require.NoError(t, err)
		assert.True(t, jsonOutput)
	})
}

func TestMCPProviderPolicyUpdateCheckAndConsentUseRealProviderProtocol(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	if !peoplesweep.StoredCredentialsSupported() {
		t.Skip("stored credentials require Unix permissions")
	}
	gate := api.NewSerialOperationGate()
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, held := gate.Holder()
		assertions.False(held)
		assertions.Equal("Bearer synthetic-key", r.Header.Get("Authorization"))
		data, err := io.ReadAll(r.Body)
		if !assertions.NoError(err) {
			return
		}
		content := `{"ok":true}`
		if strings.Contains(string(data), `"claims"`) {
			content = `{"claims":[]}`
		}
		var request struct {
			Model string `json:"model"`
		}
		if !assertions.NoError(json.Unmarshal(data, &request)) {
			return
		}
		assertions.Equal("next-model", request.Model)
		calls.Add(1)
		assertions.NoError(json.MarshalWrite(w, map[string]any{"model": request.Model, "choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 3}}))
	}))
	t.Cleanup(provider.Close)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.People.Sweep = personProviderTestConfig()
	hosted := cfg.People.Sweep.Providers["default"]
	hosted.Endpoint = provider.URL + "/v1"
	hosted.Credential = peoplesweep.CredentialStored
	hosted.CredentialEnv = ""
	cfg.People.Sweep.Providers["default"] = hosted
	requirements.NoError(cfg.Save())
	credentials := peoplesweep.NewFileCredentialStore(cfg.TokensDir())
	requirements.NoError(credentials.Save("default", peoplesweep.NewCredential(peoplesweep.AuthBearer, "synthetic-key")))
	revision, _, err := credentials.Revision("default")
	requirements.NoError(err)
	st, _ := mcpSQLiteStoreWithPath(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, OperationGate: gate, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_people_provider_settings", nil)
	requirements.NoError(err)
	before := operationOutput[mcpserver.PeopleProviderSettings](t, result)
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyProviders}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assertions.Contains(request.Params.Message, provider.URL)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_people_provider_policy", Arguments: map[string]any{"name": "default", "etag": before.ETag, "model": "next-model", "allow_sensitive": false}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	data, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var after mcpserver.PeopleProviderSettings
	requirements.NoError(json.Unmarshal(data, &after))
	requirements.Len(after.Profiles, 1)
	assertions.Equal("next-model", after.Profiles[0].Model)
	assertions.True(after.Profiles[0].Checked)
	assertions.False(after.Profiles[0].ConsentActive)
	assertions.True(after.PendingRestart)
	assertions.NotEqual(before.ETag, after.ETag)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "check_people_provider", Arguments: map[string]any{"name": "default", "etag": after.ETag}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "consent_people_provider", Arguments: map[string]any{"name": "default", "etag": after.ETag, "fingerprint": after.Profiles[0].Fingerprint}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	active, err := st.HasActivePersonInferenceConsent(t.Context(), after.Profiles[0].Fingerprint)
	requirements.NoError(err)
	assertions.True(active)
	updatedRevision, _, err := credentials.Revision("default")
	requirements.NoError(err)
	assertions.Equal(revision, updatedRevision)
	assertions.Equal(int32(3), calls.Load())
	assertions.NotContains(string(data), "synthetic-key")
	assertions.NotContains(string(data), "credential_env")
}
