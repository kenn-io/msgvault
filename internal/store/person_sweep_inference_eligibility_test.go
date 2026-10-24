package store_test

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonSweepProviderPacketOmitsIneligibleTargets(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newProductionPersonSweepParityFixture(t)
	enableSensitiveTargetsForPersonSweepTest(t, f)
	requirements.True(f.profile.AllowSensitive)
	personalityIncludedInSourceCatalog := false
	for _, target := range f.catalog.Targets {
		if target.Slug == store.AttributeSlugPersonality {
			personalityIncludedInSourceCatalog = true
		}
	}
	requirements.True(personalityIncludedInSourceCatalog,
		"fixture must include personality before inference-eligibility filtering")
	result := parityRunWholeWorker(t, f, peoplesweep.RunIncremental)
	requirements.Equal(1, result.PeopleSucceeded)
	requirements.NotEmpty(f.provider.packets)
	for _, packet := range f.provider.packets {
		requirements.NotEmpty(packet.Catalog.Targets)
		for _, target := range packet.Catalog.Targets {
			assertions.NotEqual(store.AttributeSlugContactFrequency, target.Slug)
			assertions.NotEqual(store.AttributeSlugPersonality, target.Slug)
		}
	}
}

// The shared parity fixture uses the least-privileged profile. Rebuild its
// worker with sensitive targets enabled so this test isolates inference
// eligibility as the reason personality is omitted.
func enableSensitiveTargetsForPersonSweepTest(t *testing.T, f *personSweepParityFixture) {
	t.Helper()
	provider := f.config.Providers["default"]
	provider.AllowSensitive = true
	f.config.Providers["default"] = provider
	profile, err := f.config.Profile()
	require.NoError(t, err)
	f.profile = profile
	_, err = f.store.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(t, err)
	require.NoError(t, f.store.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint,
		CheckedAt:          f.now,
		DriverVersion:      profile.DriverVersion,
		OutputMode:         profile.OutputMode,
		ModelVersion:       "parity-reported-model",
	}))
	_, _, err = f.store.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "parity")
	require.NoError(t, err)
	f.catalog, err = f.store.BuildPersonFactCatalogContext(t.Context(), profile.AllowSensitive)
	require.NoError(t, err)
	f.key.CatalogFingerprint = f.catalog.Fingerprint
	_, err = f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{f.key})
	require.NoError(t, err)
	targetURL, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	httpClient := f.server.Client()
	httpClient.Transport = parityRewriteTransport{base: httpClient.Transport, target: targetURL}
	registry, err := peoplesweep.NewDriverRegistry(httpClient, nil, nil)
	require.NoError(t, err)
	runner, err := peoplesweep.NewRunner(f.config, f.store, registry,
		peoplesweep.NewCredentialResolver(nil, func(name string) (string, bool) {
			assert.Equal(t, "PARITY_SYNTHETIC_KEY", name)
			return "synthetic-parity-key", true
		}))
	require.NoError(t, err)
	f.worker.Config = f.config
	f.worker.Runner = parityRedactedRunner{StructuredRunner: runner}
}

func TestPersonSweepCursorKeyUnchangedByInferenceEligibility(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	config := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "default"}, Providers: map[string]peoplesweep.ProviderConfig{"default": {
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1", Model: "gpt-test", Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY", OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
		RetentionPosture: "zero_retention", TrainingPosture: "no_training", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince: "2027-01-01", RequestTimeout: time.Second,
	}}}
	config.ApplyDefaults()
	profile, err := config.Profile()
	requirements.NoError(err)
	_, err = f.store.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)
	runner := &noProviderSweepRunner{}
	nextID := 0
	worker := peoplesweep.Worker{Config: config, Store: f.store, Source: f.store, Context: peoplesweep.NewContextRetriever(f.store), Sink: f.store, Runner: runner, Catalog: f.store,
		Clock: func() time.Time { return time.Date(2026, 8, 23, 13, 0, 0, 0, time.UTC) }, NewID: func() string { nextID++; return fmt.Sprintf("inference-cursor-%d", nextID) }, WorkerID: "worker-inference-cursor"}
	var initial []string
	for i, eligible := range []bool{false, true, false} {
		definition, err := f.store.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, store.AttributeSlugContactFrequency)
		requirements.NoError(err)
		_, err = f.store.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision, store.AttributeDefinitionUpdate{InferenceEligible: &eligible})
		requirements.NoError(err)
		f.insertMessage(t, fmt.Sprintf("inference-filtered-%d", i), "email", f.aliceID, time.Date(2026, 8, 23, 12, i, 0, 0, time.UTC))
		result, err := worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1})
		requirements.NoError(err)
		requirements.Equal(1, result.PeopleSucceeded)
		rows, err := f.store.DB().QueryContext(t.Context(), f.store.Rebind(`SELECT source_lane,program_fingerprint,catalog_fingerprint FROM person_sweep_cursors WHERE person_id = ? ORDER BY source_lane,program_fingerprint,catalog_fingerprint`), f.alicePersonID)
		requirements.NoError(err)
		defer rows.Close()
		keys := make([]string, 0)
		for rows.Next() {
			var lane, program, catalog string
			requirements.NoError(rows.Scan(&lane, &program, &catalog))
			keys = append(keys, lane+":"+program+":"+catalog)
		}
		requirements.NoError(rows.Err())
		requirements.NotEmpty(keys)
		if i == 0 {
			initial = keys
		} else {
			assertions.Equal(initial, keys)
		}
	}
	assertions.Zero(runner.calls)
}
