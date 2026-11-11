package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestPeopleProviderProjectionAndRefusals(t *testing.T) {
	a, r := assert.New(t), require.New(t)
	secret := "synthetic-private"
	result := projectPeopleProviderSettings(&mcpserver.OperationResult{ETag: `"exact"`, Output: generated.PeopleInferenceSettingsResponse{Profiles: []generated.PeopleInferenceProfileSetting{{Name: "primary", Endpoint: &secret, CredentialEnv: &secret, Fingerprint: &secret, CredentialSource: secret, Checked: true}}}}, false)
	data, err := json.Marshal(result.Output)
	r.NoError(err)
	a.NotContains(string(data), secret)
	a.Contains(string(data), `"etag":"\"exact\""`)
	a.Contains(string(data), `"checked":true`)
	for _, code := range []string{"check_required", "consent_required", "provider_not_found", "invalid_provider", "people_inference_unavailable", "if_match_required", "settings_conflict"} {
		a.True(peopleProviderDefinite(code), code)
	}
	a.False(peopleProviderDefinite("settings_read_failed"))
}

func TestPeopleProviderNativeApprovalAndConsent(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	r.NoError(os.WriteFile(path, []byte(`[people.sweep]
enabled = true
provider = "primary"
[people.sweep.providers.primary]
protocol = "openai_chat"
endpoint = "https://api.example.test/v1"
model = "synthetic-model"
auth = "bearer"
credential = "env"
credential_env = "SYNTHETIC_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
request_timeout = "45s"
`), 0600))
	cfg, err := config.Load(path, dir)
	r.NoError(err)
	cfg.Data.DataDir = dir
	st := testutil.NewTestStore(t)
	profile, err := cfg.People.Sweep.Profile()
	r.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	r.NoError(err)
	r.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(), DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: profile.Model}))
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test")
	r.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	a.Subset(backend.capabilities(), []string{"get_people_provider_settings", "select_people_provider", "disable_people_inference"})
	approved := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyProviders}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		a.Contains(req.Params.Message, "revoke active consent")
		a.Contains(req.Params.Message, "re-consent")
		approved++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_people_provider_settings"})
	r.NoError(err)
	r.False(read.IsError)
	data, err := json.Marshal(read.StructuredContent)
	r.NoError(err)
	a.NotContains(string(data), "api.example.test")
	a.NotContains(string(data), "SYNTHETIC_KEY")
	var view mcpserver.PeopleProviderSettings
	r.NoError(json.Unmarshal(data, &view))
	r.NotEmpty(view.ETag)
	disabled, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "disable_people_inference", Arguments: map[string]any{"etag": view.ETag}})
	r.NoError(err)
	a.False(disabled.IsError)
	a.Equal(1, approved)
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	r.NoError(err)
	a.False(active)
	stale, err := backend.ExecuteOperation(t.Context(), "disable_people_inference", map[string]any{"etag": view.ETag})
	r.NoError(err)
	r.True(stale.IsError)
	data, err = json.Marshal(stale.Output)
	r.NoError(err)
	a.Contains(string(data), "settings_conflict")
	current, err := backend.ExecuteOperation(t.Context(), "get_people_provider_settings", nil)
	r.NoError(err)
	newView := operationOutput[mcpserver.PeopleProviderSettings](t, current)
	a.True(newView.PendingRestart)
	refused, err := backend.ExecuteOperation(t.Context(), "select_people_provider", map[string]any{"etag": newView.ETag, "name": "primary"})
	r.NoError(err)
	r.True(refused.IsError)
	data, err = json.Marshal(refused.Output)
	r.NoError(err)
	a.Contains(string(data), "consent_required")
	a.Contains(string(data), `"operation_may_have_completed":false`)
}

func TestPeopleProviderDiscoveryRequiresEachNativeContract(t *testing.T) {
	a := assert.New(t)
	c := &apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{
		{OperationID: "getSettingsPeopleInference", Method: "GET", Path: "/api/v1/settings/people-inference"},
		{OperationID: "selectSettingsPeopleInference", Method: "POST", Path: "/api/v1/settings/people-inference/select", RequestProperties: []string{"name"}},
		{OperationID: "disableSettingsPeopleInference", Method: "POST", Path: "/api/v1/settings/people-inference/disable"},
	}}
	a.Len(peopleProviderMCPCapabilities(c), 3)
	c.Routes[1].RequestProperties = nil
	c.Routes[2].Path = "/wrong"
	a.Equal([]string{"get_people_provider_settings"}, peopleProviderMCPCapabilities(c))
	c.Routes[0].Path = "/wrong"
	a.Empty(peopleProviderMCPCapabilities(c))
}

func TestPeopleProviderPostCommitFailureIsUncertain(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"settings_read_failed","message":"synthetic private diagnostic"}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	r.NoError(err)
	backend := &daemonMCPOperations{client: client, supported: []string{"disable_people_inference"}}
	result, err := backend.ExecuteOperation(t.Context(), "disable_people_inference", map[string]any{"etag": `"revision"`})
	r.NoError(err)
	r.True(result.IsError)
	data, err := json.Marshal(result.Output)
	r.NoError(err)
	a.Contains(string(data), `"operation_may_have_completed":true`)
	a.NotContains(string(data), "synthetic private diagnostic")
}
