package peoplesweep_test

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	configpkg "go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestProviderModesEndToEndConfigLoadedNativeAndPromptRepair(t *testing.T) {
	for _, test := range []struct {
		name      string
		mode      peoplesweep.OutputMode
		wantCalls int
		repair    bool
	}{
		{name: "native schema", mode: peoplesweep.OutputModeNativeJSONSchema, wantCalls: 1},
		{name: "prompt JSON repairs", mode: peoplesweep.OutputModePromptJSON, wantCalls: 2, repair: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			provider := &openAISweepServer{t: t}
			server := httptest.NewServer(provider)
			defer server.Close()

			loaded := loadProviderModeEndToEndConfig(t, server.URL, test.mode)
			profile, err := loaded.People.Sweep.Profile()
			must.NoError(err)
			checks.Equal(test.mode, profile.OutputMode)

			var validResponse string
			provider.responseFor = func(raw []byte, call int) (openAISweepResponse, error) {
				if call == 0 {
					var responseErr error
					validResponse, responseErr = extractionResponseFromWire(raw, sweepResponseModel,
						peoplesweep.TokenUsage{InputTokens: 23, OutputTokens: 7}, nil)
					if responseErr != nil {
						return openAISweepResponse{}, responseErr
					}
					if test.repair {
						return openAISweepResponse{Body: staticOpenAIEnvelope(
							t, sweepResponseModel,
							`{"claims":[{"target_key":"unknown-target","relation":"support","value":"synthetic","evidence_ids":["unknown-evidence"],"valid_from":null,"valid_until":null,"confidence_basis_points":900}]}`,
							11, 2,
						)}, nil
					}
				}
				return openAISweepResponse{Body: validResponse}, nil
			}
			fixture := newOpenAISweepFixture(t, server, provider, 1,
				func(config *peoplesweep.Config) { *config = loaded.People.Sweep })

			result, err := runOpenAISweep(t, fixture)
			must.NoError(err)
			checks.Equal(1, result.PeopleSucceeded)
			checks.Equal(1, result.ProjectedWrites)
			provider.mu.Lock()
			calls := provider.calls
			wireHashes := append([]string(nil), provider.wireHashes...)
			provider.mu.Unlock()
			checks.Equal(test.wantCalls, calls)
			checks.Len(wireHashes, test.wantCalls)
			if test.repair {
				checks.NotEqual(wireHashes[0], wireHashes[1],
					"the repair must reserve and send its distinct exact wire request")
			}
		})
	}
}

func loadProviderModeEndToEndConfig(
	t *testing.T, endpoint string, mode peoplesweep.OutputMode,
) *configpkg.Config {
	t.Helper()
	homeDir := t.TempDir()
	path := filepath.Join(homeDir, "config.toml")
	content := fmt.Sprintf(`[data]
data_dir = %q

[people.sweep]
enabled = true
provider = "loaded-profile"

[people.sweep.providers.loaded-profile]
protocol = "openai_chat"
endpoint = %q
model = "model-request-2026"
auth = "bearer"
credential = "env"
credential_env = "SYNTHETIC_OPENAI_KEY"
output_mode = %q
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2026-01-01"
request_timeout = "2s"
allow_sensitive = true
`, homeDir, endpoint+"/compatible/custom", mode)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	loaded, err := configpkg.Load(path, "")
	require.NoError(t, err)
	return loaded
}
