package cmd

import (
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestStoreAPIAdapterReportsCommitmentDiscovery(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.People.Sweep.Enabled = true
	cfg.People.Sweep.Commitments.Enabled = true
	cfg.People.Sweep.Provider = peoplesweep.ProviderSelection{Name: "default"}
	cfg.People.Sweep.Providers = map[string]peoplesweep.ProviderConfig{"default": {
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://provider.example/v1",
		Model: "example-model", Auth: peoplesweep.AuthBearer, Credential: peoplesweep.CredentialEnv,
		CredentialEnv: "EXAMPLE_KEY", OutputMode: peoplesweep.OutputModeNativeJSONSchema,
		TokenLimitParameter: "max_completion_tokens", RetentionPosture: "zero_data_retention",
		TrainingPosture: "no_training", AllowSensitive: true,
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Second,
	}}
	cfg.People.Sweep.ApplyDefaults()
	profile, err := cfg.People.Sweep.Profile()
	r.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(ctx, profile)
	r.NoError(err)
	r.NoError(st.RecordPersonInferenceCheck(ctx, store.PersonInferenceCheck{ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now(), DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ModelVersion: profile.Model}))
	_, _, err = st.GrantPersonCommitmentConsent(ctx, profile.Fingerprint, "test")
	r.NoError(err)
	participant, err := st.EnsureParticipantByIdentifier("email", "alex@example.test", "Alex")
	r.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	r.NoError(err)
	_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
	r.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/loops?person_id=%d", person.ID), nil))
	r.Equal(http.StatusOK, w.Code, w.Body.String())
	var response api.LoopListResponse
	r.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	r.NotNil(response.CommitmentsComplete)
	r.False(*response.CommitmentsComplete)
}
