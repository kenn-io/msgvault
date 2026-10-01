package cmd

import (
	"encoding/json/v2"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/providercredentials"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLaneReadinessFailedConsentLookupStaysUnknown(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Vector.Enabled = true
	cfg.Vector.People.Enabled = true
	cfg.Vector.Embeddings.Endpoint = "https://embedding.example.test/v1"
	cfg.Vector.Embeddings.Model = "synthetic-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.People.RetentionPosture = "zero_data_retention"
	cfg.Vector.People.TrainingPosture = "no_training"
	_, err := cfg.Vector.SemanticPersonEmbeddingProfile()
	require.NoError(t, err)
	require.NoError(t, st.Close())
	state := setupConsentFromStore(t.Context(), cfg, st)
	lane := personSearchLane(cfg, setupEnvironment{consent: state})
	assert.Equal(t, consentUnknown, lane.Consent, "a failed archive lookup cannot report missing authority")
}

func TestLaneReadinessUsesActualConfigConsentCredentialsAndPendingRestart(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Vector.Enabled, cfg.Vector.People.Enabled = true, true
	cfg.Vector.Embeddings.Endpoint = "https://embedding.example.test/v1"
	cfg.Vector.Embeddings.Model = "synthetic-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Embeddings.APIKeyEnv = "READINESS_SYNTHETIC_KEY"
	cfg.Vector.People.RetentionPosture, cfg.Vector.People.TrainingPosture = "zero_data_retention", "no_training"
	requirements.NoError(cfg.Save())
	t.Setenv(cfg.Vector.Embeddings.APIKeyEnv, "synthetic-readiness-private-key")
	profile, err := cfg.Vector.SemanticPersonEmbeddingProfile()
	requirements.NoError(err)
	_, err = st.EnsurePersonSemanticEmbeddingProfile(t.Context(), profile)
	requirements.NoError(err)
	reader := newDaemonLaneReadinessReader(cfg, st)
	runtime := api.LaneRuntimeSnapshot{Initialized: map[string]bool{}, VectorStatus: api.VectorStatusInitializing}
	read := func() api.LaneReadinessResponse {
		t.Helper()
		report, err := reader(t.Context(), runtime)
		requirements.NoError(err)
		return report
	}
	find := func(report api.LaneReadinessResponse, name string) api.LaneReadiness {
		t.Helper()
		for _, lane := range report.Lanes {
			if lane.Lane == name {
				return lane
			}
		}
		requirements.FailNow("missing lane")
		return api.LaneReadiness{}
	}
	report := read()
	requirements.Len(report.Lanes, 8)
	assertions.True(report.StoreAvailable)
	assertions.False(report.PendingRestart, "unchanged persisted configuration matches the daemon snapshot")
	text := find(report, laneTextSearch)
	assertions.True(text.Enabled)
	assertions.True(text.Configured)
	assertions.False(text.Initialized)
	assertions.Equal("available", text.CredentialState)
	assertions.Contains(text.Blockers, "uninitialized")
	person := find(report, lanePersonSearch)
	assertions.Equal(consentMissing, person.ConsentState)
	_, _, err = st.GrantPersonSemanticEmbeddingConsent(t.Context(), profile.Fingerprint, "cli")
	requirements.NoError(err)
	assertions.Equal(consentActive, find(read(), lanePersonSearch).ConsentState)
	cfg.Vector.Embeddings.Model = "changed-synthetic-model"
	requirements.NoError(cfg.Save())
	report = read()
	assertions.True(report.PendingRestart)
	assertions.Equal(consentStale, find(report, lanePersonSearch).ConsentState)
	assertions.Contains(find(report, lanePersonSearch).Blockers, "consent_stale")
	credentials, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), credentials.ETag, providercredentials.VectorEmbeddingsID, "https://different.example.test/v1", "synthetic-stored-private-key")
	requirements.NoError(err)
	report = read()
	assertions.Equal("unknown", find(report, laneTextSearch).CredentialState)
	wire, err := json.Marshal(report)
	requirements.NoError(err)
	for _, private := range []string{cfg.HomeDir, cfg.ConfigFilePath(), cfg.Vector.Embeddings.Endpoint, cfg.Vector.Embeddings.APIKeyEnv, "synthetic-readiness-private-key", "synthetic-stored-private-key", "provider credential", "msgvault "} {
		assertions.NotContains(string(wire), private)
	}
	runtime.VectorStatus = api.VectorStatusStale
	assertions.Contains(find(read(), laneTextSearch).Blockers, "index_stale")
	requirements.NoError(st.Close())
	report = read()
	assertions.False(report.StoreAvailable)
	assertions.Equal(consentUnknown, find(report, lanePersonSearch).ConsentState)
	assertions.Contains(find(report, lanePersonSearch).Blockers, "store_unavailable")
}

func TestLaneReadinessDisabledConfigurationAndRuntimeRemainDistinct(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	requirements.NoError(cfg.Save())
	reader := newDaemonLaneReadinessReader(cfg, nil)
	report, err := reader(t.Context(), api.LaneRuntimeSnapshot{Initialized: map[string]bool{laneMediaPolicy: true}, PendingRestart: true})
	requirements.NoError(err)
	assertions.False(report.StoreAvailable)
	assertions.True(report.PendingRestart)
	for _, lane := range report.Lanes {
		if lane.Lane == laneMediaPolicy {
			assertions.True(lane.Enabled)
			assertions.True(lane.Initialized)
			continue
		}
		if !lane.Enabled {
			assertions.Contains(lane.Blockers, "disabled")
		}
		assertions.Contains(lane.Blockers, "pending_restart")
	}
}

func TestLaneReadinessMissingStoredCredentialIsDistinctFromUnavailable(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.People.Sweep = productionPersonSweepConfig(productionPersonSweepProvider(peoplesweep.ProtocolOpenAIResponses, peoplesweep.AuthBearer, peoplesweep.CredentialStored, peoplesweep.OutputModeNativeJSONSchema))
	lane := peopleInferenceLane(cfg, setupEnvironment{consent: &setupConsentState{PersonInference: true}})
	assert.Equal(t, "missing", lane.readiness.credential)
	assert.NoDirExists(t, cfg.TokensDir(), "readiness does not create credential directories")
}
