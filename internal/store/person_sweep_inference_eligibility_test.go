package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonSweepProviderPacketOmitsIneligibleTargets(t *testing.T) {
	f := newProductionPersonSweepParityFixture(t)
	result := parityRunWholeWorker(t, f, peoplesweep.RunIncremental)
	require.Equal(t, 1, result.PeopleSucceeded)
	require.NotEmpty(t, f.provider.packets)
	for _, packet := range f.provider.packets {
		require.NotEmpty(t, packet.Catalog.Targets)
		for _, target := range packet.Catalog.Targets {
			assert.NotEqual(t, store.AttributeSlugContactFrequency, target.Slug)
			assert.NotEqual(t, store.AttributeSlugPersonality, target.Slug)
		}
	}
}

func TestPersonSweepCursorKeyUnchangedByInferenceEligibility(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	config := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "default"}, Providers: map[string]peoplesweep.ProviderConfig{"default": {
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1", Model: "gpt-test", Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY", OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
		RetentionPosture: "zero_retention", TrainingPosture: "no_training", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince: "2027-01-01", RequestTimeout: time.Second,
	}}}
	config.ApplyDefaults()
	profile, err := config.Profile()
	require.NoError(t, err)
	_, err = f.store.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(t, err)
	runner := &noProviderSweepRunner{}
	nextID := 0
	worker := peoplesweep.Worker{Config: config, Store: f.store, Source: f.store, Context: peoplesweep.NewContextRetriever(f.store), Sink: f.store, Runner: runner, Catalog: f.store,
		Clock: func() time.Time { return time.Date(2026, 8, 23, 13, 0, 0, 0, time.UTC) }, NewID: func() string { nextID++; return fmt.Sprintf("inference-cursor-%d", nextID) }, WorkerID: "worker-inference-cursor"}
	var initial []string
	for i, eligible := range []bool{false, true, false} {
		definition, err := f.store.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, store.AttributeSlugContactFrequency)
		require.NoError(t, err)
		_, err = f.store.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision, store.AttributeDefinitionUpdate{InferenceEligible: &eligible})
		require.NoError(t, err)
		f.insertMessage(t, fmt.Sprintf("inference-filtered-%d", i), "email", f.aliceID, time.Date(2026, 8, 23, 12, i, 0, 0, time.UTC))
		result, err := worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1})
		require.NoError(t, err)
		require.Equal(t, 1, result.PeopleSucceeded)
		rows, err := f.store.DB().QueryContext(t.Context(), f.store.Rebind(`SELECT source_lane,program_fingerprint,catalog_fingerprint FROM person_sweep_cursors WHERE person_id = ? ORDER BY source_lane,program_fingerprint,catalog_fingerprint`), f.alicePersonID)
		require.NoError(t, err)
		keys := make([]string, 0)
		for rows.Next() {
			var lane, program, catalog string
			require.NoError(t, rows.Scan(&lane, &program, &catalog))
			keys = append(keys, lane+":"+program+":"+catalog)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.NotEmpty(t, keys)
		if i == 0 {
			initial = keys
		} else {
			assert.Equal(t, initial, keys)
		}
	}
	assert.Zero(t, runner.calls)
}
