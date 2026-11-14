package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type recordingPersonSweepRunner struct {
	requests []peoplesweep.RunRequest
}

func (r *recordingPersonSweepRunner) Run(
	_ context.Context, request peoplesweep.RunRequest,
) (peoplesweep.RunResult, error) {
	r.requests = append(r.requests, request)
	return peoplesweep.RunResult{RunID: "manual-run", PeopleAttempted: request.Limit}, nil
}

func newPersonSweepCommandStore(
	t *testing.T, tracked bool,
) (*store.Store, int64, peoplesweep.Config) {
	t.Helper()
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipant("sweep@example.test", "Sweep Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	if tracked {
		_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
		require.NoError(t, err)
	}
	config := personProviderTestConfig()
	mutateConfiguredPersonProvider(&config, func(provider *peoplesweep.ProviderConfig) {
		provider.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
		provider.SourceSince = "2027-01-01"
		provider.SourceUntil = ""
	})
	return st, person.ID, config
}

func executePersonSweepCommand(
	t *testing.T, deps personSweepCommandDeps, args ...string,
) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault", SilenceUsage: true, SilenceErrors: true}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonSweepCommand(deps))
	root.AddCommand(person)
	root.SetArgs(append([]string{"person", "sweep"}, args...))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	err := root.ExecuteContext(t.Context())
	return output.String(), err
}

func TestPersonSweepRunRejectsUntrackedPerson(t *testing.T) {
	st, personID, config := newPersonSweepCommandStore(t, false)
	constructed := 0
	deps := localPersonSweepCommandDeps(config, st)
	deps.newRunner = func(peoplesweep.Config, personSweepCommandStore) (personSweepRunner, error) {
		constructed++
		return &recordingPersonSweepRunner{}, nil
	}
	_, err := executePersonSweepCommand(t, deps, "run", "--person", strconv.FormatInt(personID, 10))
	require.ErrorContains(t, err, "not tracked")
	assert.Zero(t, constructed, "provider construction must happen after durable tracking validation")
}

func TestPersonSweepRunRejectsExplicitZeroPersonBeforeGlobalWork(t *testing.T) {
	t.Run("daemon", func(t *testing.T) {
		checks, must := assert.New(t), require.New(t)
		st, _, config := newPersonSweepCommandStore(t, true)
		runner := &recordingPersonSweepRunner{}
		opened, constructed := 0, 0
		deps := localPersonSweepCommandDeps(config, st)
		deps.openStore = func() (personSweepCommandStore, func(), error) {
			opened++
			return st, func() {}, nil
		}
		deps.newRunner = func(peoplesweep.Config, personSweepCommandStore) (personSweepRunner, error) {
			constructed++
			return runner, nil
		}

		_, err := executePersonSweepCommand(t, deps, "run", "--person", "0")
		must.ErrorContains(err, "positive")
		checks.Zero(opened, "invalid target must fail before opening the writer store")
		checks.Zero(constructed, "invalid target must fail before provider construction")
		checks.Empty(runner.requests, "invalid target must never become a global worker request")
	})

	t.Run("frontend", func(t *testing.T) {
		checks, must := assert.New(t), require.New(t)
		config := personProviderTestConfig()
		credentialLookups, proxies := 0, 0
		deps := personSweepCommandDeps{
			config: configCopy(config), isDaemonSubprocess: func() bool { return false },
			lookupEnv: func(string) (string, bool) {
				credentialLookups++
				return "credential-value", true
			},
			proxy: func(*cobra.Command, []string, map[string]string) error {
				proxies++
				return nil
			},
		}

		_, err := executePersonSweepCommand(t, deps, "run", "--person", "0")
		must.ErrorContains(err, "positive")
		checks.Zero(credentialLookups, "invalid target must not read provider credentials")
		checks.Zero(proxies, "invalid target must not reach the daemon writer")
	})
}

func TestPersonSweepRunSelectsBackstopMode(t *testing.T) {
	checks, must := assert.New(t), require.New(t)
	st, _, config := newPersonSweepCommandStore(t, true)
	runner := &recordingPersonSweepRunner{}
	deps := localPersonSweepCommandDeps(config, st)
	deps.newRunner = func(peoplesweep.Config, personSweepCommandStore) (personSweepRunner, error) {
		return runner, nil
	}
	output, err := executePersonSweepCommand(t, deps, "run", "--backstop", "--limit", "7", "--json")
	must.NoError(err)
	must.Len(runner.requests, 1)
	checks.Equal(peoplesweep.RunManual, runner.requests[0].Kind)
	checks.Equal(peoplesweep.RunBackstop, runner.requests[0].Mode)
	checks.Equal(7, runner.requests[0].Limit)
	checks.JSONEq(`{"run_id":"manual-run","people_attempted":7,"people_succeeded":0,"projected_writes":0,"usage":{"requests":0,"input_tokens":0,"output_tokens":0,"estimated_cost_microusd":0}}`, output)
}

func TestPersonSweepRunDefaultsLimitToConfiguredBound(t *testing.T) {
	st, _, config := newPersonSweepCommandStore(t, true)
	config.WorkBatchSize = 5
	runner := &recordingPersonSweepRunner{}
	deps := localPersonSweepCommandDeps(config, st)
	deps.newRunner = func(peoplesweep.Config, personSweepCommandStore) (personSweepRunner, error) {
		return runner, nil
	}
	_, err := executePersonSweepCommand(t, deps, "run")
	require.NoError(t, err)
	require.Len(t, runner.requests, 1)
	assert.Equal(t, 5, runner.requests[0].Limit)
}

func TestPersonSweepStatusReportsRedactedOperationalState(t *testing.T) {
	checks, must := assert.New(t), require.New(t)
	st, personID, config := newPersonSweepCommandStore(t, true)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	_, err := st.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: "status-run", Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: "program-safe", CatalogFingerprint: "catalog-safe",
		ProviderFingerprint: "provider-safe", StartedAt: now,
	})
	must.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_sweep_attempts
			(id, run_id, person_id, lease_fence, mode, status, failure_class,
			 cursor_envelope_json, envelope_hash, program_fingerprint,
			 catalog_fingerprint, provider_fingerprint, started_at, completed_at)
		VALUES (?, ?, ?, 1, 'incremental', 'failed', 'invalid_output',
			 '[]', 'envelope-safe', 'program-safe', 'catalog-safe', 'provider-safe', ?, ?)`),
		"status-attempt", "status-run", personID, now, now.Add(time.Second))
	must.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM person_sweep_work`)
	must.NoError(err)
	output, err := executePersonSweepCommand(
		t, localPersonSweepCommandDeps(config, st), "status", "--json",
	)
	must.NoError(err)
	var status personSweepStatusOutput
	must.NoError(json.Unmarshal([]byte(output), &status))
	checks.True(status.Enabled)
	checks.Equal("15 2 * * *", status.Schedule)
	checks.Zero(status.DirtyCount)
	checks.Equal(peoplesweep.FailureInvalidOutput, status.LastFailure)
	checks.NotEmpty(status.ProgramFingerprint)
	checks.NotEmpty(status.CatalogFingerprint)
	checks.NotEmpty(status.ProviderFingerprint)
	checks.NotContains(output, "credential-value")
	checks.NotContains(output, "evidence")
}

func TestPersonSweepStatusTableIncludesOldestDirtyTime(t *testing.T) {
	st, _, config := newPersonSweepCommandStore(t, true)
	output, err := executePersonSweepCommand(t, localPersonSweepCommandDeps(config, st), "status")
	require.NoError(t, err)
	assert.Contains(t, output, "Oldest dirty:")
}

func TestPersonSweepHistoryNeverPrintsEvidence(t *testing.T) {
	checks, must := assert.New(t), require.New(t)
	st, personID, config := newPersonSweepCommandStore(t, true)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	_, err := st.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{
		ID: "history-run", Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		ProgramFingerprint: "program-safe", CatalogFingerprint: "catalog-safe",
		ProviderFingerprint: "provider-safe", StartedAt: now,
	})
	must.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_sweep_attempts
			(id, run_id, person_id, lease_fence, mode, status, failure_class,
			 cursor_envelope_json, envelope_hash, program_fingerprint,
			 catalog_fingerprint, provider_fingerprint, generation_key,
			 provider_request_id, started_at, completed_at)
		VALUES (?, ?, ?, 1, 'incremental', 'failed', 'invalid_output',
			 '[]', 'envelope-safe', 'program-safe', 'catalog-safe', 'provider-safe',
			 'literal-secret-evidence', 'literal-secret-response-text', ?, ?)`),
		"history-attempt", "history-run", personID, now, now.Add(time.Second))
	must.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_sweep_runs SET status = 'failed', attempt_count = 1,
			failure_count = 1, completed_at = ? WHERE id = ?`), now.Add(time.Second), "history-run")
	must.NoError(err)
	deps := localPersonSweepCommandDeps(config, st)
	for _, args := range [][]string{{"history", "--person", strconv.FormatInt(personID, 10)}, {"history", "--person", strconv.FormatInt(personID, 10), "--json"}} {
		output, runErr := executePersonSweepCommand(t, deps, args...)
		must.NoError(runErr)
		checks.Contains(output, "invalid_output")
		checks.NotContains(output, "literal-secret-evidence")
		checks.NotContains(output, "literal-secret-response-text")
	}
}

func TestPersonSweepCommandsUseDaemonWriter(t *testing.T) {
	config := personProviderTestConfig()
	var got [][]string
	deps := personSweepCommandDeps{
		config: configCopy(config), isDaemonSubprocess: func() bool { return false },
		lookupEnv: func(string) (string, bool) { return "key", true },
		proxy: func(command *cobra.Command, args []string, _ map[string]string) error {
			forwarded, err := daemonCLIArgsFromCobra(command, args)
			require.NoError(t, err)
			got = append(got, forwarded)
			return nil
		},
	}
	for _, args := range [][]string{{"run", "--limit", "3"}, {"status", "--json"}, {"history", "--limit", "4"}} {
		_, err := executePersonSweepCommand(t, deps, args...)
		require.NoError(t, err)
	}
	assert.Equal(t, [][]string{
		{"person", "sweep", "run", "--limit=3"},
		{"person", "sweep", "status", "--json"},
		{"person", "sweep", "history", "--limit=4"},
	}, got)
}

func TestPersonSweepDaemonForwardsOnlyConfiguredOpenAIKey(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol peoplesweep.Protocol
		key      string
		want     map[string]string
	}{
		{name: "OpenAI configured key", protocol: peoplesweep.ProtocolOpenAIChat,
			key: "OPENAI_SWEEP_ONLY", want: map[string]string{"OPENAI_SWEEP_ONLY": "credential-value"}},
		{name: "Codex forwards none", protocol: peoplesweep.ProtocolCodexAppServer},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			config := personProviderTestConfig()
			mutateConfiguredPersonProvider(&config, func(provider *peoplesweep.ProviderConfig) {
				provider.Protocol = test.protocol
				provider.CredentialEnv = test.key
				if test.protocol == peoplesweep.ProtocolCodexAppServer {
					provider.Auth = peoplesweep.AuthNone
					provider.Credential = peoplesweep.CredentialNone
				}
			})
			var got map[string]string
			deps := personSweepCommandDeps{
				config: configCopy(config), isDaemonSubprocess: func() bool { return false },
				lookupEnv: func(name string) (string, bool) {
					checks.Equal(test.key, name)
					return "credential-value", true
				},
				proxy: func(_ *cobra.Command, _ []string, env map[string]string) error {
					got = env
					return nil
				},
			}
			_, err := executePersonSweepCommand(t, deps, "run")
			must.NoError(err)
			checks.Equal(test.want, got)
		})
	}
}

func localPersonSweepCommandDeps(config peoplesweep.Config, st *store.Store) personSweepCommandDeps {
	return personSweepCommandDeps{
		config:             configCopy(config),
		openStore:          func() (personSweepCommandStore, func(), error) { return st, func() {}, nil },
		isDaemonSubprocess: func() bool { return true },
	}
}

func configCopy(config peoplesweep.Config) func() peoplesweep.Config {
	return func() peoplesweep.Config { return config }
}
