package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	configpkg "go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPeopleSweepSchedulerRegistersOnlyWhenEnabled(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		want    bool
	}{
		{name: "enabled", enabled: true, want: true},
		{name: "disabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			sched := scheduler.New(nil)
			cfg := peoplesweep.Config{Enabled: test.enabled, Schedule: "15 2 * * *"}
			must.NoError(addPeopleSweepJob(sched, cfg, func(context.Context) error { return nil }))
			checks.Equal(test.want, sched.IsJobScheduled(peopleSweepJobName))
			if test.want {
				must.Len(sched.JobStatus(), 1)
				checks.Equal("15 2 * * *", sched.JobStatus()[0].Schedule)
			}
		})
	}
}

// TestProductionPersonSweepCodexUsesReleasedIsolationGate catches scheduled
// and manual worker construction passing nil launch dependencies for Codex.
func TestProductionPersonSweepCodexUsesReleasedIsolationGate(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	var marker string
	var executable string
	if runtime.GOOS == "windows" {
		var err error
		executable, err = os.Executable()
		must.NoError(err)
	} else {
		marker = filepath.Join(t.TempDir(), "app-server-started")
		executable = filepath.Join(t.TempDir(), "codex-production-fixture")
		must.NoError(os.WriteFile(executable, []byte(
			"#!/bin/sh\nif test \"$1\" = '--version'; then printf 'codex-cli 0.149.0\\n'; exit 0; fi\n"+
				"printf started > '"+marker+"'\n",
		), 0o700))
	}
	config := commandCodexConfig()
	mutateConfiguredPersonProvider(&config, func(provider *peoplesweep.ProviderConfig) {
		provider.Executable = executable
	})
	fullConfig := configpkg.NewDefaultConfig()
	fullConfig.Data.DataDir = t.TempDir()
	fullConfig.People.Sweep = config
	st := testutil.NewTestStore(t)

	worker, err := newProductionPersonSweepWorker(fullConfig, st)
	must.ErrorIs(err, peoplesweep.ErrCodexIsolationUnreleased)
	checks.Nil(worker)
	checks.NoDirExists(fullConfig.TokensDir())
	if marker != "" {
		checks.NoFileExists(marker)
	}

	err = newPeopleSweepScheduledRun(fullConfig, st)(t.Context())
	must.ErrorIs(err, peoplesweep.ErrCodexIsolationUnreleased)
	if marker != "" {
		checks.NoFileExists(marker)
	}
}

func TestProductionPersonSweepConstructsEveryHTTPProtocolAndCredentialSource(t *testing.T) {
	tests := []struct {
		name       string
		provider   peoplesweep.ProviderConfig
		credential peoplesweep.Credential
	}{
		{
			name: "OpenAI Chat environment",
			provider: productionPersonSweepProvider(
				peoplesweep.ProtocolOpenAIChat, peoplesweep.AuthBearer,
				peoplesweep.CredentialEnv, peoplesweep.OutputModeNativeJSONSchema,
			),
		},
		{
			name: "OpenAI Responses stored",
			provider: productionPersonSweepProvider(
				peoplesweep.ProtocolOpenAIResponses, peoplesweep.AuthBearer,
				peoplesweep.CredentialStored, peoplesweep.OutputModeNativeJSONSchema,
			),
			credential: peoplesweep.NewCredential(peoplesweep.AuthBearer, "stored-test-value"),
		},
		{
			name: "Anthropic Messages environment",
			provider: productionPersonSweepProvider(
				peoplesweep.ProtocolAnthropicMessages, peoplesweep.AuthXAPIKey,
				peoplesweep.CredentialEnv, peoplesweep.OutputModePromptJSON,
			),
		},
		{
			name: "Google Generate Content environment",
			provider: productionPersonSweepProvider(
				peoplesweep.ProtocolGoogleGenerateContent, peoplesweep.AuthGoogleAPIKey,
				peoplesweep.CredentialEnv, peoplesweep.OutputModePromptJSON,
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.provider.Credential == peoplesweep.CredentialStored {
				requireStoredCredentialStorePlatform(t)
			}
			must := require.New(t)
			checks := assert.New(t)
			st := testutil.NewTestStore(t)
			fullConfig := configpkg.NewDefaultConfig()
			fullConfig.Data.DataDir = t.TempDir()
			fullConfig.People.Sweep = productionPersonSweepConfig(test.provider)
			if test.provider.Credential == peoplesweep.CredentialEnv {
				t.Setenv(test.provider.CredentialEnv, "environment-test-value")
			}
			if test.provider.Credential == peoplesweep.CredentialStored {
				must.NoError(peoplesweep.NewFileCredentialStore(fullConfig.TokensDir()).
					Save("production", test.credential))
			}

			runner, err := newProductionStructuredRunner(fullConfig, st)
			must.NoError(err)
			profile, err := fullConfig.People.Sweep.Profile()
			must.NoError(err)
			_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
			must.NoError(err)
			must.NoError(st.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
				ProfileFingerprint: profile.Fingerprint,
				CheckedAt:          time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC),
				DriverVersion:      profile.DriverVersion,
				OutputMode:         profile.OutputMode,
				ModelVersion:       "production-test-model",
			}))
			_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "production-test")
			must.NoError(err)
			prepared, err := runner.PrepareStructured(t.Context(), peoplesweep.StructuredRequest{
				ProgramID: "production-construction", ProgramVersion: "1",
				InputText: "Synthetic input only.", SchemaName: "production_construction",
				JSONSchema:      json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
				MaxOutputTokens: 16,
				Sources: []peoplesweep.SourceDescriptor{{
					Class: peoplesweep.SourceConversationText, ObservedOn: "2026-08-26",
				}},
			})
			must.NoError(err)
			session, err := runner.BeginStructuredExecution(t.Context(), prepared)
			must.NoError(err)
			checks.NotNil(session)
			if test.provider.Credential != peoplesweep.CredentialStored {
				checks.NoDirExists(fullConfig.TokensDir())
			}
		})
	}
}

func productionPersonSweepConfig(provider peoplesweep.ProviderConfig) peoplesweep.Config {
	config := peoplesweep.Config{
		Enabled:   true,
		Provider:  peoplesweep.ProviderSelection{Name: "production"},
		Providers: map[string]peoplesweep.ProviderConfig{"production": provider},
	}
	config.ApplyDefaults()
	return config
}

func productionPersonSweepProvider(
	protocol peoplesweep.Protocol,
	auth peoplesweep.AuthScheme,
	credential peoplesweep.CredentialSource,
	mode peoplesweep.OutputMode,
) peoplesweep.ProviderConfig {
	credentialEnv := ""
	if credential == peoplesweep.CredentialEnv {
		credentialEnv = "PRODUCTION_SWEEP_TEST_KEY"
	}
	tokenLimitParameter := ""
	if protocol == peoplesweep.ProtocolOpenAIChat {
		tokenLimitParameter = "max_completion_tokens"
	}
	return peoplesweep.ProviderConfig{
		Protocol: protocol, Endpoint: "https://provider.example.test/v1", Model: "test-model",
		Auth: auth, Credential: credential, CredentialEnv: credentialEnv, OutputMode: mode,
		TokenLimitParameter: tokenLimitParameter,
		RetentionPosture:    "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2026-01-01", RequestTimeout: time.Second,
	}
}

func TestPeopleSweepSchedulerRejectsInvalidCron(t *testing.T) {
	sched := scheduler.New(nil)
	err := addPeopleSweepJob(sched, peoplesweep.Config{
		Enabled: true, Schedule: "not a cron",
	}, func(context.Context) error { return nil })
	require.ErrorContains(t, err, "invalid cron expression")
	assert.False(t, sched.IsJobScheduled(peopleSweepJobName))
}

func TestPeopleSweepSchedulerRecoversJournalGap(t *testing.T) {
	must := require.New(t)
	st, personID, config := newPersonSweepCommandStore(t, true)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), false)
	must.NoError(err)
	profile, err := config.Profile()
	must.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	must.NoError(err)
	_, err = st.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{{
		PersonID: personID, SourceLane: profile.AllowedSources[0],
		ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint,
	}})
	must.NoError(err)
	tx, err := st.DB().BeginTx(t.Context(), nil)
	must.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var journalSequence int64
	err = tx.QueryRowContext(t.Context(), `
		UPDATE person_sweep_change_clock
		SET sequence = sequence + 1
		WHERE singleton = TRUE AND enabled = TRUE
		RETURNING sequence`).Scan(&journalSequence)
	must.NoError(err)
	_, err = tx.ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_sweep_changes
			(sequence, person_id, source_lane, change_kind, evidence_effect, recorded_at)
		VALUES (?, ?, ?, 'upsert', '', CURRENT_TIMESTAMP)`),
		journalSequence, personID, profile.AllowedSources[0])
	must.NoError(err)
	must.NoError(tx.Commit())
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		DELETE FROM person_sweep_work WHERE person_id = ?`), personID)
	must.NoError(err)
	var journalHighWater int64
	must.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT MAX(sequence) FROM person_sweep_changes WHERE person_id = ?`), personID).
		Scan(&journalHighWater))

	fullConfig := configpkg.NewDefaultConfig()
	fullConfig.Data.DataDir = t.TempDir()
	fullConfig.People.Sweep = config
	run := newPeopleSweepScheduledRun(fullConfig, st)
	must.NoError(run(t.Context()))
	var cursorHighWater int64
	must.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT optimistic_sequence FROM person_sweep_cursors
		WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
		  AND catalog_fingerprint = ?`), personID, profile.AllowedSources[0],
		peoplesweep.ProgramFingerprint(), catalog.Fingerprint).Scan(&cursorHighWater))
	assert.Equal(t, journalHighWater, cursorHighWater)
}
