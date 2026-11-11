package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
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
)

func TestPeopleProviderNativeApprovalAndConsent(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	r.NoError(os.WriteFile(path, []byte(`[people.sweep]
enabled = false
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
`), 0o600))
	cfg, err := config.Load(path, dir)
	r.NoError(err)
	cfg.Data.DataDir = dir
	st := testutil.NewTestStore(t)
	candidate := cfg.People.Sweep
	candidate.Enabled = true
	profile, err := candidate.Profile()
	r.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	r.NoError(err)
	r.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(), DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: profile.Model}))
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test")
	r.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	approved := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyProviders}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		if approved == 0 {
			a.Contains(req.Params.Message, "Enable people inference with the selected provider")
		} else if approved == 1 {
			a.Contains(req.Params.Message, "revoke active consent for both configured and running providers")
			a.Contains(req.Params.Message, "re-consent")
		}
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
	a.NotContains(string(data), "fingerprint")
	a.NotContains(string(data), "credential_")
	var view mcpserver.PeopleProviderSettings
	r.NoError(json.Unmarshal(data, &view))
	r.NotEmpty(view.ETag)
	a.False(view.ConfiguredEnabled)
	selected, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "select_people_provider", Arguments: map[string]any{"etag": view.ETag, "name": "primary"}})
	r.NoError(err)
	r.False(selected.IsError)
	data, err = json.Marshal(selected.StructuredContent)
	r.NoError(err)
	var selectedView mcpserver.PeopleProviderSettings
	r.NoError(json.Unmarshal(data, &selectedView))
	a.True(selectedView.ConfiguredEnabled)
	a.True(selectedView.PendingRestart)
	r.NotEmpty(selectedView.ETag)
	disabled, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "disable_people_inference", Arguments: map[string]any{"etag": selectedView.ETag}})
	r.NoError(err)
	r.False(disabled.IsError)
	data, err = json.Marshal(disabled.StructuredContent)
	r.NoError(err)
	var disabledView mcpserver.PeopleProviderSettings
	r.NoError(json.Unmarshal(data, &disabledView))
	a.True(disabledView.PendingRestart)
	a.Equal(2, approved)
	active, err := st.HasActivePersonInferenceConsent(t.Context(), profile.Fingerprint)
	r.NoError(err)
	a.False(active)
	stale, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "disable_people_inference", Arguments: map[string]any{"etag": selectedView.ETag}})
	r.NoError(err)
	r.True(stale.IsError)
	data, err = json.Marshal(stale.StructuredContent)
	r.NoError(err)
	a.Contains(string(data), "settings_conflict")
	a.NotContains(string(data), "operation_may_have_completed") // Disclosure refused before any mutation.
	invalid, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "select_people_provider", Arguments: map[string]any{"etag": disabledView.ETag, "name": " "}})
	r.NoError(err)
	r.True(invalid.IsError)
	data, err = json.Marshal(invalid.StructuredContent)
	r.NoError(err)
	a.Contains(string(data), "invalid_arguments")
	refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "select_people_provider", Arguments: map[string]any{"etag": disabledView.ETag, "name": "primary"}})
	r.NoError(err)
	r.True(refused.IsError)
	data, err = json.Marshal(refused.StructuredContent)
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
	a.Len(newDaemonMCPOperations(nil, c).capabilities(), 3)
	c.Routes[1].RequestProperties = nil
	c.Routes[2].Path = "/wrong"
	a.Equal([]string{"get_people_provider_settings"}, newDaemonMCPOperations(nil, c).capabilities())
	c.Routes[0].Path = "/wrong"
	a.Empty(newDaemonMCPOperations(nil, c).capabilities())
}

func TestPeopleProviderPostCommitFailureIsUncertain(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	var code string
	var postCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"broken":`))
			return
		}
		postCount.Add(1)
		a.Equal(`"revision"`, req.Header.Get("If-Match"))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"` + code + `","message":"synthetic private diagnostic"}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	r.NoError(err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_people_provider_settings", "disable_people_inference", "select_people_provider"}}
	_, err = backend.OperationDisclosure(t.Context(), "disable_people_inference", map[string]any{"etag": `"revision"`})
	var refusal *mcpserver.OperationRefusalError
	r.ErrorAs(err, &refusal)
	a.Equal("operation_context_unavailable", refusal.Code)
	a.Zero(postCount.Load())
	for _, tc := range []struct {
		name, code string
		uncertain  bool
	}{
		{"disable_people_inference", "settings_conflict", true},
		{"disable_people_inference", "settings_edit_rejected", true},
		{"select_people_provider", "settings_conflict", true},
		{"select_people_provider", "settings_edit_rejected", true},
		{"disable_people_inference", "settings_read_failed", true},
		{"select_people_provider", "server_busy", false},
	} {
		code = tc.code
		args := map[string]any{"etag": `"revision"`}
		if tc.name == "select_people_provider" {
			args["name"] = "primary"
		}
		result, err := backend.ExecuteOperation(t.Context(), tc.name, args)
		r.NoError(err)
		r.True(result.IsError)
		failure, ok := result.Output.(operationFailureOutput)
		r.True(ok)
		if tc.code != "settings_read_failed" {
			a.Equal(tc.code, failure.Error)
		}
		a.Equal(tc.uncertain, failure.OperationMayHaveCompleted)
		data, err := json.Marshal(result.Output)
		r.NoError(err)
		a.NotContains(string(data), "synthetic private diagnostic")
	}
}
