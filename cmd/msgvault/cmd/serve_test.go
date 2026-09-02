package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/discord"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestStoreAPIAdapterDeletePersonSuppressesCurrentIdentifiers(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("Delete.Person@Example.test", "Delete Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)
	_, profile, _ := scheduleWorkerProfile(t, f, "deletion-provider", "TEST_DELETE_PROVIDER_KEY")
	key := strings.Repeat("d", 32)
	_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(err)
	_, _, err = f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE participants SET display_name = NULL WHERE id = ?`), participantID)
	require.NoError(err)
	displayName := "Delete Profile"
	person, err = f.Store.UpdatePersonDisplayNameContext(
		t.Context(), person.ID, person.Revision, &displayName)
	require.NoError(err)
	organization, err := f.Store.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: "Example Labs", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	_, err = f.Store.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		IsCurrent: new(true), IsPrimary: new(true), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	person, err = f.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	run, _, err := f.Store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "disabled-deletion-history", RequestedAt: now,
	})
	require.NoError(err)
	require.NoError(f.Store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "manual:disabled-delete"},
		DueAt:   now,
	}))
	lease, err := f.Store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "disabled-deletion-worker", ProviderName: profile.Name,
		Now: now, LeaseDuration: time.Minute,
	})
	require.NoError(err)
	require.NotNil(lease)
	hasher, err := personenrichment.NewSuppressionHasher([]byte(key))
	require.NoError(err)
	oldDigest := hasher.Digest(profile.ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "old-delete@example.test")
	_, _, err = f.Store.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
		PayloadHash: strings.Repeat("a", 64), RequestHash: strings.Repeat("b", 64),
		PersonRevision: person.Revision, Trigger: lease.Trigger,
		CheckedIdentifiers: []personenrichment.SuppressionDigest{oldDigest},
	})
	require.NoError(err)
	adapter := &storeAPIAdapter{
		store: f.Store,
		personEnrichmentConfig: personenrichment.Config{
			Enabled: false, SuppressionKeyEnv: "TEST_DELETE_SUPPRESSION_KEY",
		},
		lookupEnv: func(name string) (string, bool) {
			assert.Equal(t, "TEST_DELETE_SUPPRESSION_KEY", name)
			return key, true
		},
	}

	require.NoError(adapter.DeletePersonContext(t.Context(), person.ID, person.Revision))
	digest := hasher.Digest(profile.ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "delete.person@example.test")
	found, err := f.Store.HasPersonEnrichmentSuppressionContext(t.Context(), digest)
	require.NoError(err)
	assert.True(t, found)
	nameCompany, err := personenrichment.NormalizeSuppressionIdentifier(
		personenrichment.SuppressionNameCompany, []string{"Delete Profile", "Example Labs"})
	require.NoError(err)
	digest = hasher.Digest(profile.ProviderNamespace, nameCompany.Class,
		nameCompany.NormalizationVersion, nameCompany.Value)
	found, err = f.Store.HasPersonEnrichmentSuppressionContext(t.Context(), digest)
	require.NoError(err)
	assert.True(t, found)
}

func TestStoreAPIAdapterDeletePersonWithoutEnrichmentHistoryNeedsNoSuppressionKey(t *testing.T) {
	f := storetest.New(t)
	participantID := f.EnsureParticipant("unenriched-delete@example.test", "Unenriched Delete", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	adapter := &storeAPIAdapter{
		store: f.Store,
		personEnrichmentConfig: personenrichment.Config{
			Enabled: false, SuppressionKeyEnv: "TEST_UNUSED_SUPPRESSION_KEY",
		},
		lookupEnv: func(string) (string, bool) {
			require.FailNow(t, "deleting a person without enrichment history must not load a suppression key")
			return "", false
		},
	}

	require.NoError(t, adapter.DeletePersonContext(t.Context(), person.ID, person.Revision))
	_, err = f.Store.GetPersonContext(t.Context(), person.ID)
	require.ErrorIs(t, err, store.ErrPersonNotFound)
}

func TestStoreAPIAdapterDeletePersonFailsClosedWithoutMatchingSuppressionKey(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *storetest.Fixture, personenrichment.ProviderProfile)
		key   func(string) (string, bool)
	}{
		{
			name: "missing key",
			key:  func(string) (string, bool) { return "", false },
		},
		{
			name: "mismatched durable key",
			setup: func(t *testing.T, f *storetest.Fixture, profile personenrichment.ProviderProfile) {
				t.Helper()
				hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{'o'}, 32))
				require.NoError(t, err)
				digest := hasher.Digest(profile.ProviderNamespace, personenrichment.SuppressionEmail,
					personenrichment.EmailNormalizationV1, "other@example.test")
				require.NoError(t, f.Store.InsertPersonEnrichmentSuppressionsContext(t.Context(),
					[]store.PersonEnrichmentSuppressionInput{{
						ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
						NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID,
						Digest: digest.Digest, Reason: store.PersonEnrichmentSuppressionOptOut, Actor: "test",
					}}))
			},
			key: func(string) (string, bool) { return strings.Repeat("n", 32), true },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := storetest.New(t)
			participantID := f.EnsureParticipant("closed@example.test", "Closed Person", "example.test")
			person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
			require.NoError(t, err)
			_, profile, _ := scheduleWorkerProfile(t, f, "closed-provider", "TEST_CLOSED_PROVIDER_KEY")
			if test.setup != nil {
				test.setup(t, f, profile)
			}
			adapter := &storeAPIAdapter{
				store: f.Store,
				personEnrichmentConfig: personenrichment.Config{
					Enabled: true, SuppressionKeyEnv: "TEST_CLOSED_SUPPRESSION_KEY",
				},
				lookupEnv: test.key,
			}
			err = adapter.DeletePersonContext(t.Context(), person.ID, person.Revision)
			require.Error(t, err)
			_, getErr := f.Store.GetPersonContext(t.Context(), person.ID)
			require.NoError(t, getErr)
		})
	}
}

func TestStoreAPIAdapterDeletePersonRejectsRecordedAttemptKeyMismatch(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("attempt-key@example.test", "Attempt Key", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)
	_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(err)
	_, profile, _ := scheduleWorkerProfile(t, f, "attempt-key-provider", "TEST_ATTEMPT_PROVIDER_KEY")
	now := time.Date(2026, 8, 23, 22, 0, 0, 0, time.UTC)
	_, _, err = f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	run, _, err := f.Store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "attempt-key-run", RequestedAt: now,
	})
	require.NoError(err)
	require.NoError(f.Store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "manual:attempt-key"},
		DueAt:   now,
	}))
	lease, err := f.Store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "attempt-key-worker", ProviderName: profile.Name,
		Now: now, LeaseDuration: time.Minute,
	})
	require.NoError(err)
	require.NotNil(lease)
	oldHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{'o'}, 32))
	require.NoError(err)
	oldDigest := oldHasher.Digest(profile.ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "attempt-key@example.test")
	_, _, err = f.Store.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
		PayloadHash: strings.Repeat("a", 64), RequestHash: strings.Repeat("b", 64),
		PersonRevision: person.Revision, Trigger: lease.Trigger,
		CheckedIdentifiers: []personenrichment.SuppressionDigest{oldDigest},
	})
	require.NoError(err)
	adapter := &storeAPIAdapter{
		store: f.Store,
		personEnrichmentConfig: personenrichment.Config{
			Enabled: true, SuppressionKeyEnv: "TEST_DELETE_SUPPRESSION_KEY",
		},
		lookupEnv: func(string) (string, bool) { return strings.Repeat("n", 32), true },
	}
	err = adapter.DeletePersonContext(t.Context(), person.ID, person.Revision)
	require.ErrorIs(err, personenrichment.ErrSuppressionKeyMismatch)
	_, err = f.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	attempts, err := f.Store.ListPersonEnrichmentAttemptsContext(t.Context(),
		store.PersonEnrichmentAttemptFilter{PersonID: person.ID, Limit: 10})
	require.NoError(err)
	assert.NotEmpty(t, attempts)
}

// serveLifecycleTestTimeout bounds waits for daemon-startup milestones (API
// seam entered, analytics build started, health ready). Every use is a
// positive wait, so the value only stretches the failure path — passing runs
// are unaffected. It must absorb a full InitSchema on the slowest CI
// environment: the sharded Windows runner has been observed taking over two
// minutes to execute schema.sql under filesystem load.
const serveLifecycleTestTimeout = 180 * time.Second

func TestServeConfigParsing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Create temp config with scheduled accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"

[server]
api_port = 9090
api_key = "test-key"

[[accounts]]
email = "user1@gmail.com"
schedule = "0 2 * * *"
enabled = true

[[accounts]]
email = "user2@gmail.com"
schedule = "0 3 * * *"
enabled = true

[[accounts]]
email = "disabled@gmail.com"
schedule = "0 4 * * *"
enabled = false
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "write config")

	cfg, err := config.Load(configPath, "")
	require.NoError(err, "Load")

	// Verify server config
	assert.Equal(9090, cfg.Server.APIPort, "APIPort")
	assert.Equal("test-key", cfg.Server.APIKey, "APIKey")

	// Verify scheduled accounts
	scheduled := cfg.ScheduledAccounts()
	assert.Len(scheduled, 2, "len(ScheduledAccounts())")

	// Verify specific accounts
	acc := cfg.GetAccountSchedule("user1@gmail.com")
	require.NotNil(acc, "GetAccountSchedule(user1)")
	assert.Equal("0 2 * * *", acc.Schedule, "user1 schedule")

	// Disabled account should still be retrievable but not in scheduled list
	disabled := cfg.GetAccountSchedule("disabled@gmail.com")
	assert.NotNil(disabled, "GetAccountSchedule(disabled)")
}

func TestSchedulerWithConfig(t *testing.T) {
	cfg := &config.Config{
		Accounts: []config.AccountSchedule{
			{Email: "test1@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "test2@gmail.com", Schedule: "0 3 * * *", Enabled: true},
			{Email: "test3@gmail.com", Schedule: "invalid", Enabled: true},
		},
	}

	var syncCalls []string
	sched := scheduler.New(func(ctx context.Context, email string) error {
		syncCalls = append(syncCalls, email)
		return nil
	})

	count, errs := sched.AddAccountsFromConfig(cfg)

	// Should schedule 2 valid accounts
	assert.Equal(t, 2, count, "scheduled count")

	// Should have 1 error for invalid cron
	assert.Len(t, errs, 1, "len(errs)")

	// Verify status
	statuses := sched.Status()
	assert.Len(t, statuses, 2, "len(Status())")
}

func TestServeCmdNoAccounts(t *testing.T) {
	// Create temp config without accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644), "write config")

	cfg, err := config.Load(configPath, "")
	require.NoError(t, err, "Load")

	scheduled := cfg.ScheduledAccounts()
	assert.Empty(t, scheduled, "expected no scheduled accounts")
}

func TestServeOAuthValidationAllowsMicrosoftOnly(t *testing.T) {
	assert.True(t, hasServeOAuthConfig(&config.Config{
		Microsoft: config.MicrosoftConfig{ClientID: "azure-client-id"},
	}))
}

func TestServeOAuthValidationReportsNoProviders(t *testing.T) {
	assert.False(t, hasServeOAuthConfig(&config.Config{}))
}

func TestRunServeStartsReadOnlyWithoutOAuthConfig(t *testing.T) {
	oldCfg := cfg
	dataDir := t.TempDir()
	cfg = lifecycleTestConfig(dataDir)
	cfg.Server.APIPort = freeTCPPort(t)
	t.Cleanup(func() { cfg = oldCfg })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{Use: serveCmd.Use}
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServe(cmd, nil)
	}()

	waitForServeHealth(t, cfg.Server.APIPort, errCh)
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err, "runServe")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "runServe did not stop after context cancellation")
	}
}

func TestRunServeImmediateCancellationWaitsForAPIStart(t *testing.T) {
	require := require.New(t)
	oldCfg := cfg
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Server.APIPort = freeTCPPort(t)
	c.Vector.Enabled = false
	c.Analytics.Engine = config.AnalyticsEngineSQL
	cfg = c
	t.Cleanup(func() { cfg = oldCfg })

	started := make(chan struct{})
	release := make(chan struct{})
	oldStart := startServeAPIServer
	startServeAPIServer = func(server *api.Server, listener net.Listener) error {
		close(started)
		<-release
		return server.StartOnListener(listener)
	}
	t.Cleanup(func() {
		startServeAPIServer = oldStart
		select {
		case <-release:
		default:
			close(release)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()

	select {
	case <-started:
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("API startup seam was not entered")
	}
	cancel()
	select {
	case err := <-errCh:
		require.FailNow("runServe returned before listener-start barrier", "error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-errCh:
		require.NoError(err, "runServe")
	case <-time.After(10 * time.Second):
		require.FailNow("runServe did not stop after listener startup was released")
	}
}

func TestRunServeAutoSelectsAPIPortWhenUnconfigured(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldCfg := cfg
	dataDir := t.TempDir()
	cfg = lifecycleTestConfig(dataDir)
	cfg.Server.APIPort = 0 // auto-select an open port
	t.Cleanup(func() { cfg = oldCfg })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{Use: serveCmd.Use}
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		errCh <- runServe(cmd, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(30 * time.Second):
			require.Fail("runServe did not stop during test cleanup")
		}
	})

	// Discover the auto-selected port the same way clients do: through the
	// daemon runtime record, not the configured port (which is 0).
	// A fresh Windows runner can need more than 15 seconds to initialize the
	// full schema while the CLI package shards compete for CPU and disk I/O.
	// This test checks port discovery, not startup performance.
	rt, ready, err := waitForDaemonRuntime(ctx, dataDir, 45*time.Second, daemonRuntimeReady, errCh)
	require.NoError(err, "wait for daemon runtime record")
	require.True(ready, "daemon runtime record did not become ready")
	assert.NotZero(rt.Port, "runtime record must record the bound ephemeral port")

	url := fmt.Sprintf("http://%s/health", net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port)))
	resp, err := http.Get(url) //nolint:gosec // local test server
	require.NoError(err, "GET /health on discovered address")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusOK, resp.StatusCode, "/health status")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(err, "runServe")
	case <-time.After(5 * time.Second):
		require.FailNow("runServe did not stop after context cancellation")
	}
}

func TestRunServeServesHealthWhileAnalyticsBuildBlocked(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldCfg := cfg
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Server.APIPort = freeTCPPort(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	c.Vector.Enabled = false
	cfg = c
	t.Cleanup(func() { cfg = oldCfg })

	buildStarted := make(chan struct{})
	stubBuildCacheSubprocess(t, func(ctx context.Context, _ bool) error {
		close(buildStarted)
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(cancel)
	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServe(cmd, nil)
	}()

	select {
	case <-buildStarted:
	case err := <-errCh:
		require.NoError(err, "runServe exited before analytics build was blocked")
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("analytics cache build did not start")
	}
	waitForServeHealthBounded(t, c.Server.APIPort, errCh)

	healthClient := &http.Client{Timeout: time.Second}
	resp, err := healthClient.Get(fmt.Sprintf("http://127.0.0.1:%d/health", c.Server.APIPort))
	require.NoError(err, "GET /health")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusOK, resp.StatusCode)
	var health api.HealthResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&health))
	assert.Equal(api.AnalyticsModeSQLFallback, health.AnalyticsEngine,
		"auto mode must report live-SQL fallback while cache initialization is blocked")

	aggregateResp, err := healthClient.Get(fmt.Sprintf(
		"http://127.0.0.1:%d/api/v1/aggregates?view_type=senders",
		c.Server.APIPort,
	))
	require.NoError(err, "GET aggregate through SQL fallback")
	defer func() { _ = aggregateResp.Body.Close() }()
	assert.Equal(http.StatusOK, aggregateResp.StatusCode,
		"read-only aggregate must bypass the cache-build mutation gate")
	var aggregates api.AggregateResponse
	require.NoError(json.NewDecoder(aggregateResp.Body).Decode(&aggregates))

	cancel()
	select {
	case err := <-errCh:
		require.NoError(err, "runServe")
	case <-time.After(10 * time.Second):
		require.FailNow("runServe did not stop after context cancellation")
	}
}

func TestRunServeDuckDBReportsInitializingWithoutSQLFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldCfg := cfg
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Server.APIPort = freeTCPPort(t)
	c.Analytics.Engine = config.AnalyticsEngineDuckDB
	c.Analytics.AutoBuildCache = true
	c.Vector.Enabled = false
	cfg = c
	t.Cleanup(func() { cfg = oldCfg })

	buildStarted := make(chan struct{})
	stubBuildCacheSubprocess(t, func(ctx context.Context, _ bool) error {
		close(buildStarted)
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(cancel)
	cmd := &cobra.Command{Use: serveCmd.Use}
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()

	select {
	case <-buildStarted:
	case err := <-errCh:
		require.NoError(err, "runServe exited before analytics build was blocked")
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("analytics cache build did not start")
	}
	waitForServeHealthBounded(t, c.Server.APIPort, errCh)

	healthClient := &http.Client{Timeout: time.Second}
	resp, err := healthClient.Get(fmt.Sprintf("http://127.0.0.1:%d/health", c.Server.APIPort))
	require.NoError(err, "GET /health")
	var health api.HealthResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&health))
	_ = resp.Body.Close()
	assert.Equal(api.AnalyticsModeInitializing, health.AnalyticsEngine)

	resp, err = healthClient.Get(fmt.Sprintf(
		"http://127.0.0.1:%d/api/v1/messages/filter?limit=1",
		c.Server.APIPort,
	))
	require.NoError(err, "GET general archive route")
	assert.Equal(http.StatusOK, resp.StatusCode,
		"SQLite-backed detail routes must remain available while DuckDB initializes")
	_ = resp.Body.Close()

	resp, err = healthClient.Get(fmt.Sprintf(
		"http://127.0.0.1:%d/api/v1/text/conversations",
		c.Server.APIPort,
	))
	require.NoError(err, "GET text conversations")
	assert.Equal(http.StatusOK, resp.StatusCode,
		"SQLite-backed text routes must remain available while DuckDB initializes")
	_ = resp.Body.Close()

	resp, err = healthClient.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/aggregates?view_type=senders", c.Server.APIPort))
	require.NoError(err, "GET analytics route")
	assert.Equal(http.StatusServiceUnavailable, resp.StatusCode)
	_ = resp.Body.Close()

	resp, err = healthClient.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/query", c.Server.APIPort),
		"application/json",
		strings.NewReader(`{"sql":"SELECT 1"}`),
	)
	require.NoError(err, "POST SQL query")
	assert.Equal(http.StatusServiceUnavailable, resp.StatusCode,
		"initializing DuckDB must report SQL engine unavailable")
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-errCh:
		require.NoError(err, "runServe")
	case <-time.After(10 * time.Second):
		require.FailNow("runServe did not stop after context cancellation")
	}
}

func TestRunServeAutoSwitchesToDuckDBAfterBackgroundBuild(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldCfg := cfg
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	c.Server.APIPort = freeTCPPort(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	c.Vector.Enabled = false
	cfg = c
	t.Cleanup(func() { cfg = oldCfg })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(cancel)

	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	stubBuildCacheSubprocess(t, func(ctx context.Context, fullRebuild bool) error {
		close(buildStarted)
		select {
		case <-releaseBuild:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), fullRebuild)
		return err
	})

	cmd := &cobra.Command{Use: serveCmd.Use}
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()

	select {
	case <-buildStarted:
	case err := <-errCh:
		require.NoError(err, "runServe exited before analytics build was blocked")
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("analytics cache build did not start")
	}
	waitForServeHealthBounded(t, c.Server.APIPort, errCh)

	healthClient := &http.Client{Timeout: time.Second}
	readAnalyticsMode := func() string {
		resp, err := healthClient.Get(fmt.Sprintf("http://127.0.0.1:%d/health", c.Server.APIPort))
		if err != nil {
			return ""
		}
		defer func() { _ = resp.Body.Close() }()
		var health api.HealthResponse
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&health) != nil {
			return ""
		}
		return health.AnalyticsEngine
	}
	assert.Equal(api.AnalyticsModeSQLFallback, readAnalyticsMode())

	close(releaseBuild)
	assert.Eventually(func() bool {
		return readAnalyticsMode() == api.AnalyticsModeDuckDB
	}, 10*time.Second, 25*time.Millisecond, "auto mode should switch to DuckDB after cache build")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(err, "runServe")
	case <-time.After(10 * time.Second):
		require.FailNow("runServe did not stop after context cancellation")
	}
}

func TestListenServeAPIHonorsAvailableExplicitPort(t *testing.T) {
	require := require.New(t)
	probe, err := net.Listen("tcp", net.JoinHostPort(defaultDaemonBindAddr, "0"))
	require.NoError(err)
	explicitPort, err := listenerPort(probe)
	require.NoError(err)
	require.NoError(probe.Close())

	ln, err := listenServeAPI(defaultDaemonBindAddr, explicitPort)
	require.NoError(err)
	t.Cleanup(func() { _ = ln.Close() })
	actualPort, err := listenerPort(ln)
	require.NoError(err)
	assert.Equal(t, explicitPort, actualPort)
}

func TestRunServeFailsBeforeArchiveWorkWhenAPIPortInUse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ln, err := net.Listen("tcp", net.JoinHostPort(defaultDaemonBindAddr, "0"))
	require.NoError(err, "reserve API port")
	t.Cleanup(func() { _ = ln.Close() })
	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(ok, "listener address must be TCP")

	oldCfg := cfg
	dataDir := t.TempDir()
	cfg = lifecycleTestConfig(dataDir)
	cfg.Server.APIPort = addr.Port
	t.Cleanup(func() { cfg = oldCfg })

	cmd := &cobra.Command{Use: "serve"}
	cmd.SetContext(context.Background())
	err = runServe(cmd, nil)

	require.Error(err, "runServe")
	assert.Contains(err.Error(), "API server address unavailable")
	assert.Contains(err.Error(), net.JoinHostPort(defaultDaemonBindAddr, strconv.Itoa(addr.Port)))
	assert.NoFileExists(filepath.Join(dataDir, "msgvault.db"), "serve must not touch the archive when the API port is unavailable")
}

type recordingServeAPIServer struct {
	events *[]string
}

func (s recordingServeAPIServer) Shutdown(context.Context) error {
	*s.events = append(*s.events, "api-shutdown")
	return nil
}

type recordingServeScheduler struct {
	events *[]string
	ctx    context.Context
}

func (s recordingServeScheduler) Stop() context.Context {
	*s.events = append(*s.events, "scheduler-stop")
	return s.ctx
}

type recordingServeGate struct {
	events *[]string
}

func (g recordingServeGate) StartDrain() {
	*g.events = append(*g.events, "gate-start-drain")
}

func (g recordingServeGate) Wait(context.Context) error {
	*g.events = append(*g.events, "gate-wait")
	return nil
}

func TestShutdownServeRuntimeDrainsGateAroundHTTPAndScheduler(t *testing.T) {
	doneCtx, done := context.WithCancel(context.Background())
	done()
	events := []string{}

	err := shutdownServeRuntime(
		context.Background(),
		io.Discard,
		recordingServeAPIServer{events: &events},
		recordingServeScheduler{events: &events, ctx: doneCtx},
		recordingServeGate{events: &events},
	)

	require.NoError(t, err, "shutdownServeRuntime")
	assert.Equal(t, []string{
		"gate-start-drain",
		"api-shutdown",
		"scheduler-stop",
		"gate-wait",
	}, events, "shutdown order")
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen on free port")
	defer func() { require.NoError(t, ln.Close(), "close listener") }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "listener address must be TCP")
	return addr.Port
}

func waitForServeHealth(t *testing.T, port int, errCh <-chan error) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(serveLifecycleTestTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			require.NoError(t, err, "runServe exited before health was ready")
			require.FailNow(t, "runServe exited before health was ready")
		default:
		}
		resp, err := http.Get(url) //nolint:gosec // local test server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.FailNow(t, "serve health endpoint did not become ready")
}

func waitForServeHealthBounded(t *testing.T, port int, errCh <-chan error) {
	t.Helper()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(serveLifecycleTestTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			require.NoError(t, err, "runServe exited before health was ready")
			require.FailNow(t, "runServe exited before health was ready")
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.FailNow(t, "serve health endpoint did not become ready")
}

func TestRunDaemonSQLQueryRebuildsStaleCacheOutOfProcess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	c := lifecycleTestConfig(dataDir)
	s, err := store.Open(c.DatabaseDSN())
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")
	engine := query.NewEngine(s.DB(), false)
	defer func() { _ = engine.Close() }()

	sentinel := errors.New("subprocess sentinel")
	var called bool
	var gotFullRebuild bool
	old := buildCacheSubprocessForRun
	buildCacheSubprocessForRun = func(_ context.Context, fullRebuild bool) error {
		called = true
		gotFullRebuild = fullRebuild
		return sentinel
	}
	t.Cleanup(func() { buildCacheSubprocessForRun = old })

	_, err = runDaemonSQLQuery(context.Background(), c, s, engine, "select 1")

	require.Error(err, "query should fail with subprocess sentinel")
	require.ErrorIs(err, sentinel, "error")
	assert.True(called, "subprocess rebuild should be called")
	assert.True(gotFullRebuild, "missing cache should request full rebuild")
}

func TestOpenDaemonAnalyticsEngineForceSQLSkipsCacheBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineSQL
	c.Analytics.AutoBuildCache = true
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		require.FailNow("engine=sql must not build analytics cache")
		return nil
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentNone,
	)
	require.NoError(err, "openDaemonAnalyticsEngine")
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQL, mode, "engine=sql is a deliberate live-SQL choice")
	assert.Equal(startupCacheBuildOutcomeNone, outcome, "no explicit intent has no outcome")
}

func TestOpenDaemonAnalyticsEngineSkipsCacheBuildWhenDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = false
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		require.FailNow("auto_build_cache=false must not build analytics cache")
		return nil
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentNone,
	)
	require.NoError(err, "openDaemonAnalyticsEngine")
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQLFallback, mode, "auto mode without a cache is a fallback")
	assert.Equal(startupCacheBuildOutcomeNone, outcome, "no explicit intent has no outcome")
}

func TestOpenDaemonAnalyticsEngineWarnsWhenDuckDBRefreshDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = false
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(err, "build ready analytics cache")
	staleness := cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir())
	require.False(staleness.NeedsBuild, "test cache must be ready: %+v", staleness)
	var logs bytes.Buffer
	oldLogger := logger
	logger = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { logger = oldLogger })

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentNone,
	)
	require.NoError(err, "openDaemonAnalyticsEngine")
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.DuckDBEngine{}, engine,
		"auto mode must keep using a usable cache")
	assert.Equal(api.AnalyticsModeDuckDB, mode,
		"usable cache selects DuckDB even when automatic refresh is disabled")
	assert.Equal(startupCacheBuildOutcomeNone, outcome, "no explicit intent has no outcome")
	assert.Contains(logs.String(),
		"automatic analytics cache refresh disabled",
		"startup warning explains the live-SQL opt-out")
	assert.Contains(logs.String(),
		"auto_build_cache=false",
		"startup warning records the disabled setting")
}

func TestDaemonCacheRefreshErrorReportsDaemonCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	subprocessErr := errors.New("signal: killed")

	require.ErrorIs(t, daemonCacheRefreshError(ctx, subprocessErr), context.Canceled)
	assert.Same(t, subprocessErr, daemonCacheRefreshError(context.Background(), subprocessErr))
	assert.NoError(t, daemonCacheRefreshError(ctx, nil))
}

func TestOpenDaemonAnalyticsEngineAutoBuildsCacheAtStartup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	_, err := s.DB().Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'user@example.com');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
			VALUES (1, 1, 'thread1', 'email_thread', 'Hello');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet)
			VALUES (1, 1, 1, 'msg1', 'email', '2024-01-15 10:00:00', 'Hello', 'Preview');
	`)
	require.NoError(err, "insert test data")

	builds := 0
	stubBuildCacheSubprocess(t, func(_ context.Context, fullRebuild bool) error {
		builds++
		_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), fullRebuild)
		return err
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentNone,
	)
	require.NoError(err, "openDaemonAnalyticsEngine")
	defer func() { _ = engine.Close() }()

	assert.Equal(1, builds, "a stale cache must be built synchronously at startup")
	assert.Equal(api.AnalyticsModeDuckDB, mode,
		"the daemon must serve DuckDB over the fresh cache, not live-SQL fallback")
	assert.Equal(startupCacheBuildOutcomeNone, outcome, "automatic builds have no explicit outcome")
}

func TestDaemonDuckDBEnginesUseIsolatedSpillDirectories(t *testing.T) {
	requirements := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	requirements.NoError(err)

	longLived, err := openDaemonDuckDBEngine(c, s)
	requirements.NoError(err)
	defer func() { _ = longLived.Close() }()
	temporary, err := openDaemonDuckDBEngine(c, s)
	requirements.NoError(err)

	spillParent := filepath.Join(c.HomeDir, "tmp",
		fmt.Sprintf("duckdb-query-%d", os.Getpid()))
	entries, err := os.ReadDir(spillParent)
	requirements.NoError(err)
	requirements.Len(entries, 2, "each engine owns its own spill subdirectory")

	requirements.NoError(temporary.Close())
	_, err = longLived.QuerySQL(context.Background(), "SELECT 1")
	requirements.NoError(err,
		"closing a temporary engine must not disturb the live engine")
	entries, err = os.ReadDir(spillParent)
	requirements.NoError(err)
	assert.Len(t, entries, 1,
		"a closing engine removes only its own spill subdirectory")
	requirements.NoError(longLived.Close())
}

func TestOpenDaemonAnalyticsEngineAutoFallsBackWhenStartupBuildFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = true
	var logs bytes.Buffer
	oldLogger := logger
	logger = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { logger = oldLogger })
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		return errors.New("simulated build failure")
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentNone,
	)
	require.NoError(err, "a failed auto-mode build must not fail daemon startup")
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQLFallback, mode,
		"a failed build falls back to live SQL for engine=auto")
	assert.Equal(startupCacheBuildOutcomeNone, outcome, "automatic failures have no explicit outcome")
	assert.Contains(logs.String(), `msg="daemon startup step failed"`)
	assert.Contains(logs.String(), "step=build_analytics_cache")
}

func TestOpenDaemonAnalyticsEngineDuckDBRequiresCacheBuild(t *testing.T) {
	require := require.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineDuckDB
	c.Analytics.AutoBuildCache = true
	sentinel := errors.New("build failed")
	stubBuildCacheSubprocess(t, func(context.Context, bool) error {
		return sentinel
	})

	engine, _, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentNone,
	)
	if engine != nil {
		_ = engine.Close()
	}

	require.Error(err, "duckdb mode should fail when the required cache build fails")
	require.ErrorIs(err, sentinel, "error")
	require.Equal(startupCacheBuildOutcomeNone, outcome, "automatic failure has no explicit outcome")
}

func TestOpenDaemonAnalyticsEngineExplicitIntentOverridesDisabledAutoBuild(t *testing.T) {
	assert := assert.New(t)

	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	c.Analytics.AutoBuildCache = false
	builds := 0
	stubStartupCacheBuild(t, func(_ context.Context, intent startupCacheBuildIntent) error {
		builds++
		assert.Equal(startupCacheBuildIntentDefault, intent)
		_, err := buildCacheAuto(c.DatabaseDSN(), c.AnalyticsDir())
		return err
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentDefault,
	)
	require.NoError(t, err)
	defer func() { _ = engine.Close() }()

	assert.Equal(1, builds)
	assert.Equal(api.AnalyticsModeDuckDB, mode)
	assert.Equal(startupCacheBuildOutcomeFulfilled, outcome)
}

func TestOpenDaemonAnalyticsEngineExplicitFullBuildRunsWhenCacheIsFresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c, s := openTestDaemonAnalyticsStore(t)
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(err)
	builds := 0
	stubStartupCacheBuild(t, func(_ context.Context, intent startupCacheBuildIntent) error {
		builds++
		assert.Equal(startupCacheBuildIntentFull, intent)
		_, buildErr := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
		return buildErr
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentFull,
	)
	require.NoError(err)
	defer func() { _ = engine.Close() }()

	assert.Equal(1, builds)
	assert.Equal(api.AnalyticsModeDuckDB, mode)
	assert.Equal(startupCacheBuildOutcomeFulfilled, outcome)
}

func TestOpenDaemonAnalyticsEngineExplicitFailureKeepsAutoFallback(t *testing.T) {
	assert := assert.New(t)

	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	stubStartupCacheBuild(t, func(context.Context, startupCacheBuildIntent) error {
		return errors.New("simulated explicit build failure")
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentDefault,
	)
	require.NoError(t, err)
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQLFallback, mode)
	assert.Equal(startupCacheBuildOutcomeFailed, outcome)
}

func TestOpenDaemonAnalyticsEngineExplicitSuccessWithoutUsableCacheIsFailed(t *testing.T) {
	assert := assert.New(t)

	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	stubStartupCacheBuild(t, func(context.Context, startupCacheBuildIntent) error {
		return nil
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentDefault,
	)
	require.NoError(t, err)
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQLFallback, mode)
	assert.Equal(startupCacheBuildOutcomeFailed, outcome,
		"a successful child exit does not fulfill intent without a usable cache")
}

func TestOpenDaemonAnalyticsEngineExplicitFullFailureDoesNotReuseOldCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(err)
	stubStartupCacheBuild(t, func(context.Context, startupCacheBuildIntent) error {
		return errors.New("simulated full rebuild failure")
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentFull,
	)
	require.NoError(err)
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQLFallback, mode,
		"explicit auto-mode failure must preserve the documented live-SQL fallback")
	assert.Equal(startupCacheBuildOutcomeFailed, outcome)
}

func TestOpenDaemonAnalyticsEngineExplicitDuckDBOpenFailureMarksIntentFailed(t *testing.T) {
	assert := assert.New(t)

	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineAuto
	stubStartupCacheBuild(t, func(context.Context, startupCacheBuildIntent) error {
		_, err := buildCache(c.DatabaseDSN(), c.AnalyticsDir(), true)
		return err
	})
	sentinel := errors.New("simulated DuckDB open failure")
	oldOpen := openDaemonDuckDBEngineForRun
	openDaemonDuckDBEngineForRun = func(*config.Config, *store.Store) (*query.DuckDBEngine, error) {
		return nil, sentinel
	}
	t.Cleanup(func() { openDaemonDuckDBEngineForRun = oldOpen })

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentDefault,
	)
	require.NoError(t, err, "auto mode must preserve live-SQL fallback")
	defer func() { _ = engine.Close() }()

	assert.IsType(&query.SQLiteEngine{}, engine)
	assert.Equal(api.AnalyticsModeSQLFallback, mode)
	assert.Equal(startupCacheBuildOutcomeFailed, outcome,
		"the CLI must not claim the daemon is using a cache it could not open")
}

func TestOpenDaemonAnalyticsEngineSQLLeavesExplicitIntentUnconsumed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineSQL
	stubStartupCacheBuild(t, func(context.Context, startupCacheBuildIntent) error {
		require.FailNow("SQL engine must leave cache intent for the HTTP path")
		return nil
	})

	engine, mode, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentDefault,
	)
	require.NoError(err)
	defer func() { _ = engine.Close() }()

	assert.Equal(api.AnalyticsModeSQL, mode)
	assert.Equal(startupCacheBuildOutcomeUnconsumed, outcome)
}

func TestOpenDaemonAnalyticsEngineExplicitDuckDBFailureIsFatal(t *testing.T) {
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.Engine = config.AnalyticsEngineDuckDB
	sentinel := errors.New("explicit build failed")
	stubStartupCacheBuild(t, func(context.Context, startupCacheBuildIntent) error {
		return sentinel
	})

	engine, _, outcome, err := openDaemonAnalyticsEngine(
		context.Background(), c, s, startupCacheBuildIntentDefault,
	)
	if engine != nil {
		_ = engine.Close()
	}

	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, startupCacheBuildOutcomeFatal, outcome)
}

func openTestDaemonAnalyticsStore(t *testing.T) (*config.Config, *store.Store) {
	t.Helper()
	c := lifecycleTestConfig(t.TempDir())
	s, err := store.Open(c.DatabaseDSN())
	require.NoError(t, err, "open store")
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.InitSchema(), "init schema")
	return c, s
}

func stubBuildCacheSubprocess(
	t *testing.T,
	fn func(context.Context, bool) error,
) {
	t.Helper()
	old := buildCacheSubprocessForRun
	buildCacheSubprocessForRun = fn
	t.Cleanup(func() { buildCacheSubprocessForRun = old })
}

func stubStartupCacheBuild(
	t *testing.T,
	fn func(context.Context, startupCacheBuildIntent) error,
) {
	t.Helper()
	old := buildStartupCacheSubprocessForRun
	buildStartupCacheSubprocessForRun = fn
	t.Cleanup(func() { buildStartupCacheSubprocessForRun = old })
}

func TestStoreAPIAdapterServesSourceStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	source, err := s.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	require.NoError(s.UpdateSourceDisplayName(source.ID, "Alice"), "set display name")
	require.NoError(s.UpdateSourceSyncCursor(source.ID, "history-1"), "set sync cursor")

	completedID, err := s.StartSync(source.ID, "full")
	require.NoError(err, "start sync")
	require.NoError(s.UpdateSyncCheckpoint(completedID, &store.Checkpoint{
		MessagesProcessed: 3,
		MessagesAdded:     2,
		MessagesUpdated:   1,
	}), "update checkpoint")
	require.NoError(s.CompleteSync(completedID, "history-2"), "complete sync")

	adapter := &storeAPIAdapter{store: s}
	srv := api.NewServer(
		&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		adapter,
		nil,
		slog.New(slog.DiscardHandler),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources/status?source_type=gmail", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp api.SourceStatusResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	require.Len(resp.Sources, 1, "sources")

	got := resp.Sources[0]
	assert.Equal(source.ID, got.ID, "ID")
	assert.Equal("gmail", got.SourceType, "SourceType")
	assert.Equal("alice@example.com", got.Identifier, "Identifier")
	require.NotNil(got.DisplayName, "DisplayName")
	assert.Equal("Alice", *got.DisplayName, "DisplayName")
	assert.Nil(got.ActiveSync, "ActiveSync")
	require.NotNil(got.LatestSync, "LatestSync")
	assert.Equal(completedID, got.LatestSync.ID, "LatestSync.ID")
	require.NotNil(got.LastSuccessfulSync, "LastSuccessfulSync")
	assert.Equal(completedID, got.LastSuccessfulSync.ID, "LastSuccessfulSync.ID")
	assert.Equal(store.SyncStatusCompleted, got.LastSuccessfulSync.Status, "LastSuccessfulSync.Status")
	require.NotNil(got.LastSuccessfulSync.CursorAfter, "LastSuccessfulSync.CursorAfter")
	assert.Equal("history-2", *got.LastSuccessfulSync.CursorAfter, "LastSuccessfulSync.CursorAfter")
}

func TestStoreAPIAdapterRunCLISyncPacksOnlyAfterSubprocessSuccess(t *testing.T) {
	tests := []struct {
		name           string
		predecessorErr error
		wantPacked     bool
		wantAttempts   int
	}{
		{name: "success", wantPacked: true, wantAttempts: 1},
		{name: "failure", predecessorErr: errors.New("sync subprocess failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newAttachmentMaintenanceFixture(t)
			hash := f.addLoose([]byte("CLI sync attachment payload"))
			adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
			var events []api.CLISyncEvent
			runnerCalls := 0

			err := adapter.runCLISyncWithRunner(
				context.Background(),
				api.CLISyncRequest{Email: "alice@example.com"},
				func(event api.CLISyncEvent) error {
					events = append(events, event)
					return nil
				},
				func(_ context.Context, args []string, _ func(string, string) error) error {
					runnerCalls++
					assert.Equal([]string{"sync", "alice@example.com"}, args)
					assert.Nil(f.packedEntry(hash), "packing must follow subprocess success")
					return tt.predecessorErr
				},
			)

			if tt.predecessorErr != nil {
				require.ErrorIs(err, tt.predecessorErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, runnerCalls)
			assert.Equal(tt.wantPacked, f.packedEntry(hash) != nil)
			assert.Equal(tt.wantAttempts,
				strings.Count(f.logs.String(), "automatic attachment maintenance complete"))
			assert.Empty(events, "successful automatic maintenance writes no normal CLI output")
		})
	}
}

func TestCLISyncSubprocessArgsIncrementalIncludesFolderFilters(t *testing.T) {
	assert.Equal(t,
		[]string{"sync", "--folder", "INBOX", "--folder", "Archive", "--skip-folder", "Trash", "alice@example.com"},
		cliSyncSubprocessArgs(api.CLISyncRequest{
			Email:       "alice@example.com",
			Folders:     []string{"INBOX", "Archive"},
			SkipFolders: []string{"Trash"},
		}),
	)
	assert.Equal(t,
		[]string{"sync", "--folder", "Folder,With,Comma", "alice@example.com"},
		cliSyncSubprocessArgs(api.CLISyncRequest{
			Email:   "alice@example.com",
			Folders: []string{"Folder,With,Comma"},
		}),
	)
	assert.Equal(t,
		[]string{"sync", "--folder", "Path\\To\\File", "--skip-folder", "Fold,er", "alice@example.com"},
		cliSyncSubprocessArgs(api.CLISyncRequest{
			Email:       "alice@example.com",
			Folders:     []string{"Path\\To\\File"},
			SkipFolders: []string{"Fold,er"},
		}),
	)
}

func TestCLISyncSubprocessArgsIncludesExactSourceID(t *testing.T) {
	assert.Equal(t,
		[]string{"sync", "--source-id", "42"},
		cliSyncSubprocessArgs(api.CLISyncRequest{SourceID: 42, SourceIDSet: true}),
	)
	assert.Equal(t,
		[]string{"sync-full", "--source-id", "42"},
		cliSyncSubprocessArgs(api.CLISyncRequest{Full: true, SourceID: 42, SourceIDSet: true}),
	)
}

func TestDaemonCLIRunCannotUseServerRemoteDeleteConfigOrEnvironment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(err)
	binaryName := "msgvault"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(t.TempDir(), binaryName)
	build := exec.Command("go", "build", "-tags", "fts5 sqlite_vec", "-o", binaryPath, "./cmd/msgvault")
	build.Dir = repoRoot
	buildOutput, err := build.CombinedOutput()
	require.NoError(err, "build real msgvault binary: %s", buildOutput)

	savedResolver := daemonCLIExecutableResolver
	daemonCLIExecutableResolver = func() (string, error) { return binaryPath, nil }
	t.Cleanup(func() { daemonCLIExecutableResolver = savedResolver })

	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.toml")
	configContents := fmt.Sprintf(`[data]
data_dir = %q

[server]
api_key = %q

[deletion]
remote_enabled = true
`, dataDir, "boundary-secret")
	require.NoError(os.WriteFile(configPath, []byte(configContents), 0o600))
	serverCfg, err := config.Load(configPath, "")
	require.NoError(err)

	savedCfg, savedCfgFile, savedHomeDir, savedUseLocal := cfg, cfgFile, homeDir, useLocal
	cfg, cfgFile, homeDir, useLocal = serverCfg, configPath, "", false
	t.Cleanup(func() {
		cfg, cfgFile, homeDir, useLocal = savedCfg, savedCfgFile, savedHomeDir, savedUseLocal
	})
	t.Setenv(remoteDeleteEnvVar, "1")

	st, err := store.Open(serverCfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "boundary@example.invalid")
	require.NoError(err)

	manager, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	manifest := deletion.NewManifestForSource("daemon consent boundary", []string{"remote-1"}, deletion.SourceReference{
		ID: source.ID, Type: source.SourceType, Identifier: source.Identifier,
	})
	require.NoError(manager.SaveManifest(manifest))

	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: serverCfg,
		Store:  &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	})
	body, err := json.Marshal(api.CLIRunRequest{
		Args: []string{"delete-staged", "--yes", manifest.ID},
	})
	require.NoError(err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "boundary-secret")
	response := httptest.NewRecorder()

	daemon.Router().ServeHTTP(response, request)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var events []api.CLIRunEvent
	decoder := json.NewDecoder(response.Body)
	for decoder.More() {
		var event api.CLIRunEvent
		require.NoError(decoder.Decode(&event))
		events = append(events, event)
	}
	require.Len(events, 3, response.Body.String())
	blocked := "remote deletion is gated; set [deletion] remote_enabled = true in the invoking CLI's config.toml for durable consent; one-command alternative: " +
		remoteDeleteEnvVar + "=1"
	var stdout, stderr, subprocessError string
	for _, event := range events {
		switch event.Type {
		case "stdout":
			stdout += event.Data
		case "stderr":
			stderr += event.Data
		case "error":
			subprocessError = event.Error
		}
	}
	assert.Contains(stdout, "Deletion Summary:\n")
	assert.Equal("Error: "+blocked+"\n", stderr)
	assert.Equal(cliSubprocessExitSentinel, subprocessError)
	assert.FileExists(filepath.Join(manager.PendingDir(), manifest.ID+".json"))
	assert.NoFileExists(filepath.Join(manager.InProgressDir(), manifest.ID+".json"))
}

func TestStoreAPIAdapterRunCLICommandPacksOnlyAllowlistedSuccess(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		predecessorErr error
		wantPacked     bool
		wantAttempts   int
	}{
		{
			name:         "allowlisted success",
			args:         []string{importMboxCommand, "archive.mbox"},
			wantPacked:   true,
			wantAttempts: 1,
		},
		{
			name: "unrelated success",
			args: []string{"remove-account", "alice@example.com"},
		},
		{
			name:           "allowlisted failure",
			args:           []string{"sync-teams", "alice@example.com"},
			predecessorErr: errors.New("command subprocess failed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newAttachmentMaintenanceFixture(t)
			hash := f.addLoose([]byte("generic CLI attachment payload"))
			adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
			var events []api.CLIRunEvent
			runnerCalls := 0

			err := adapter.runCLICommandWithRunner(
				context.Background(),
				api.CLIRunRequest{Args: tt.args, Env: map[string]string{"TEST": "value"}, Cwd: "/tmp"},
				func(event api.CLIRunEvent) error {
					events = append(events, event)
					return nil
				},
				func(
					_ context.Context,
					args []string,
					env map[string]string,
					cwd string,
					_ func(string, string) error,
				) error {
					runnerCalls++
					assert.Equal(tt.args, args)
					assert.Equal(map[string]string{"TEST": "value"}, env)
					assert.Equal("/tmp", cwd)
					assert.Nil(f.packedEntry(hash), "packing must follow subprocess success")
					return tt.predecessorErr
				},
			)

			if tt.predecessorErr != nil {
				require.ErrorIs(err, tt.predecessorErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, runnerCalls)
			assert.Equal(tt.wantPacked, f.packedEntry(hash) != nil)
			assert.Equal(tt.wantAttempts,
				strings.Count(f.logs.String(), "automatic attachment maintenance complete"))
			assert.Empty(events, "successful automatic maintenance writes no normal CLI output")
		})
	}
}

func TestStoreAPIAdapterAppendsServerOwnedGrantDecision(t *testing.T) {
	adapter := &storeAPIAdapter{}
	req := api.CLIRunRequest{
		Args:         []string{"add-account", "user@example.com", "--readonly"},
		GrantDecided: true,
	}
	var gotArgs []string

	err := adapter.runCLICommandWithRunner(
		context.Background(), req, nil,
		func(
			_ context.Context,
			args []string,
			_ map[string]string,
			_ string,
			_ func(string, string) error,
		) error {
			gotArgs = args
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{
		"add-account", "user@example.com", "--readonly", "--grant-decided=true",
	}, gotArgs)
	assert.Equal(t, []string{"add-account", "user@example.com", "--readonly"}, req.Args,
		"server injection must not mutate caller-owned args")
}

func TestStoreAPIAdapterInterceptsExplicitRepackInDaemonParent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	oldPackID := f.makeZeroLivePack([]byte("explicit parent repack dead bytes"))
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
	var events []api.CLIRunEvent

	err := adapter.runCLICommandWithRunner(
		context.Background(), api.CLIRunRequest{Args: []string{"repack-attachments"}},
		func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		},
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			require.FailNow("explicit repack must never spawn a child process")
			return nil
		},
	)

	require.NoError(err)
	has, err := f.store.HasPackRecord(oldPackID)
	require.NoError(err)
	assert.False(has)
	require.Len(events, 1)
	assert.Equal("stdout", events[0].Type)
	assert.Contains(events[0].Data, "removed 1 old pack(s)")
}

func TestStoreAPIAdapterRejectsExplicitRepackInLooseAttachmentMode(t *testing.T) {
	f := newAttachmentMaintenanceFixture(t)
	f.maintenance.packCreationEnabled = false
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}

	err := adapter.runCLICommandWithRunner(
		context.Background(), api.CLIRunRequest{Args: []string{"repack-attachments"}},
		nil,
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			require.FailNow(t, "disabled repack must never spawn a child process")
			return nil
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "[data].loose_attachments")
}

func TestStoreAPIAdapterExplicitRepackAcceptsLoggingPassthroughFlags(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	oldPackID := f.makeZeroLivePack([]byte("explicit repack with root logging flags"))
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}

	err := adapter.runCLICommandWithRunner(
		context.Background(), api.CLIRunRequest{Args: []string{
			"repack-attachments", "--log-level=debug", "--log-sql",
			"--log-sql-slow-ms=250", "--verbose",
		}}, nil,
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			require.FailNow("explicit repack with root flags must still run in the daemon parent")
			return nil
		},
	)

	require.NoError(err)
	has, err := f.store.HasPackRecord(oldPackID)
	require.NoError(err)
	assert.False(has)
}

func TestRepackAttachmentsParentArgsAllowed(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "none", want: true},
		{name: "forwarded logging", args: []string{
			"--log-level=debug", "--log-sql", "--log-sql-slow-ms=250", "--verbose",
		}, want: true},
		{name: "non-forwarded root flag", args: []string{"--config=/tmp/config.toml"}},
		{name: "command-specific flag", args: []string{"--target-size=1024"}},
		{name: "positional argument", args: []string{"unexpected"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, repackAttachmentsParentArgsAllowed(tt.args))
		})
	}
}

func TestStoreAPIAdapterRepackAfterSuccessfulRemovalOnly(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		predecessorErr error
		wantRemoved    bool
	}{
		{name: "successful account removal", args: []string{"remove-account", "alice@example.com", "--yes"}, wantRemoved: true},
		{name: "failed account removal", args: []string{"remove-account", "alice@example.com", "--yes"}, predecessorErr: errors.New("remove failed")},
		{name: "successful excluded media purge", args: []string{"purge-excluded-media", "--yes"}, wantRemoved: true},
		{name: "successful garbage collection", args: []string{"gc", "--yes"}, wantRemoved: true},
		{name: "dry run is not a removal", args: []string{"purge-excluded-media", "--dry-run"}},
		{name: "unconfirmed purge is not a removal", args: []string{"purge-excluded-media"}},
		{name: "failed excluded media purge", args: []string{"purge-excluded-media", "--yes"}, predecessorErr: errors.New("purge failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newAttachmentMaintenanceFixture(t)
			oldPackID := f.makeZeroLivePack([]byte("post removal dead bytes"))
			adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
			runnerCalls := 0

			err := adapter.runCLICommandWithRunner(
				context.Background(),
				api.CLIRunRequest{Args: tt.args},
				nil,
				func(context.Context, []string, map[string]string, string, func(string, string) error) error {
					runnerCalls++
					return tt.predecessorErr
				},
			)
			if tt.predecessorErr != nil {
				require.ErrorIs(err, tt.predecessorErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, runnerCalls)
			has, hasErr := f.store.HasPackRecord(oldPackID)
			require.NoError(hasErr)
			assert.Equal(!tt.wantRemoved, has)
		})
	}
}

func TestStoreAPIAdapterPostRemovalRepackWarningPreservesSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, _ := newFailingAttachmentMaintenance(t)
	adapter := &storeAPIAdapter{store: maintenance.store, attachmentMaintenance: maintenance}
	var events []api.CLIRunEvent

	err := adapter.runCLICommandWithRunner(
		context.Background(), api.CLIRunRequest{Args: []string{"remove-account", "alice@example.com", "--yes"}},
		func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		},
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			return nil
		},
	)

	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("stderr", events[0].Type)
	assert.Contains(events[0].Data, "repack-attachments")
}

func TestStoreAPIAdapterPostRemovalRepackCancellationPreservesSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	oldPackID := f.makeZeroLivePack([]byte("canceled post-removal repack remains retryable"))
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
	ctx, cancel := context.WithCancel(context.Background())
	var events []api.CLIRunEvent

	err := adapter.runCLICommandWithRunner(
		ctx, api.CLIRunRequest{Args: []string{"remove-account", "alice@example.com", "--yes"}},
		func(event api.CLIRunEvent) error {
			events = append(events, event)
			return nil
		},
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			cancel()
			return nil
		},
	)

	require.NoError(err, "maintenance cancellation cannot erase committed removal success")
	assert.Empty(events, "cancellation is informational, not a streamed warning")
	has, err := f.store.HasPackRecord(oldPackID)
	require.NoError(err)
	assert.True(has, "canceled cleanup remains inventoried for retry")
	assert.Contains(f.logs.String(), "automatic attachment repack canceled")
}

func TestStoreAPIAdapterExplicitRepackCancellationFailsFast(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	oldPackID := f.makeZeroLivePack([]byte("explicit canceled repack is fail-fast"))
	adapter := &storeAPIAdapter{store: f.store, attachmentMaintenance: f.maintenance}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := adapter.runCLICommandWithRunner(
		ctx, api.CLIRunRequest{Args: []string{"repack-attachments"}}, nil,
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			require.FailNow("explicit repack must never spawn a child process")
			return nil
		},
	)

	require.ErrorIs(err, context.Canceled)
	has, getErr := f.store.HasPackRecord(oldPackID)
	require.NoError(getErr)
	assert.True(has, "fail-fast cancellation leaves physical inventory untouched")
}

func TestStoreAPIAdapterMaintenanceWarningStreamsWithoutChangingSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, logs := newFailingAttachmentMaintenance(t)
	adapter := &storeAPIAdapter{store: maintenance.store, attachmentMaintenance: maintenance}
	var events []api.CLISyncEvent

	err := adapter.runCLISyncWithRunner(
		context.Background(),
		api.CLISyncRequest{Email: "alice@example.com"},
		func(event api.CLISyncEvent) error {
			events = append(events, event)
			return nil
		},
		func(context.Context, []string, func(string, string) error) error { return nil },
	)

	require.NoError(err, "automatic maintenance failure must preserve sync success")
	require.Len(events, 1, "one concise warning event")
	assert.Equal("stderr", events[0].Type)
	assert.Contains(events[0].Data, "pack-attachments")
	assert.Contains(events[0].Data, "retry")
	assert.Contains(logs.String(), "automatic attachment maintenance failed")
}

func TestStoreAPIAdapterMaintenanceWarningStreamFailurePreservesSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, logs := newFailingAttachmentMaintenance(t)
	adapter := &storeAPIAdapter{store: maintenance.store, attachmentMaintenance: maintenance}
	warningErr := errors.New("client disconnected")

	err := adapter.runCLICommandWithRunner(
		context.Background(),
		api.CLIRunRequest{Args: []string{importMboxCommand, "archive.mbox"}},
		func(api.CLIRunEvent) error { return warningErr },
		func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			return nil
		},
	)

	require.NoError(err, "warning stream failure must preserve command success")
	assert.Contains(logs.String(), "failed to emit automatic attachment maintenance warning")
	assert.Contains(logs.String(), warningErr.Error())
}

func TestStoreAPIAdapterServesCLIInitDB(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	adapter := &storeAPIAdapter{store: s}
	srv := api.NewServer(
		&config.Config{
			Identity: config.IdentityConfig{Addresses: []string{"alice@example.com"}},
			Server:   config.ServerConfig{APIPort: 8080},
		},
		adapter,
		nil,
		slog.New(slog.DiscardHandler),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/init-db", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Notice string `json:"notice"`
		Stats  struct {
			TotalMessages int64 `json:"total_messages"`
			TotalAccounts int64 `json:"total_accounts"`
		} `json:"stats"`
	}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode response")
	assert.Contains(resp.Notice, "legacy [identity] config", "migration notice")
	assert.Equal(int64(0), resp.Stats.TotalMessages, "messages")
	assert.Equal(int64(0), resp.Stats.TotalAccounts, "accounts")
}

func TestStoreAPIAdapterServesCLIDeleteDeduped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	keepID := f.CreateMessage("keep")
	dropID := f.CreateMessage("drop")
	_, err := f.Store.MergeDuplicates(keepID, []int64{dropID}, "batch-a")
	require.NoError(err, "merge duplicate")

	adapter := &storeAPIAdapter{store: f.Store}
	srv := api.NewServer(
		&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		adapter,
		nil,
		slog.New(slog.DiscardHandler),
	)

	planReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped/plan",
		strings.NewReader(`{"batch_ids":["batch-a"]}`),
	)
	planReq.Header.Set("Content-Type", "application/json")
	planResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(planResp, planReq)

	require.Equal(http.StatusOK, planResp.Code, "plan body: %s", planResp.Body.String())
	var plan struct {
		Total      int64 `json:"total"`
		BatchCount int64 `json:"batch_count"`
		Batches    []struct {
			ID    string `json:"id"`
			Count int64  `json:"count"`
		} `json:"batches"`
	}
	require.NoError(json.NewDecoder(planResp.Body).Decode(&plan), "decode plan")
	assert.Equal(int64(1), plan.Total, "plan total")
	assert.Equal(int64(1), plan.BatchCount, "plan batch count")
	require.Len(plan.Batches, 1, "plan batches")

	executeReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped",
		strings.NewReader(`{
			"batch_ids":["batch-a"],
			"no_backup": true,
			"expected_total": 1,
			"expected_batch_count": 1,
			"expected_batches": [{"id":"batch-a", "count":1}]
		}`),
	)
	executeReq.Header.Set("Content-Type", "application/json")
	executeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(executeResp, executeReq)

	require.Equal(http.StatusOK, executeResp.Code, "execute body: %s", executeResp.Body.String())
	var executed struct {
		Deleted    int64 `json:"deleted"`
		BatchCount int64 `json:"batch_count"`
	}
	require.NoError(json.NewDecoder(executeResp.Body).Decode(&executed), "decode execute")
	assert.Equal(int64(1), executed.Deleted, "deleted")
	assert.Equal(int64(1), executed.BatchCount, "execute batch count")
}

// TestSetupVectorFeatures_Disabled verifies that when
// cfg.Vector.Enabled is false, setupVectorFeatures returns (nil, nil)
// regardless of build tag. Runs under both tagged and untagged builds.
func TestSetupVectorFeatures_Disabled(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Vector.Enabled = false

	vf, err := setupVectorFeatures(context.Background(), nil, "", false)
	require.NoError(t, err, "setupVectorFeatures")
	assert.Nil(t, vf, "setupVectorFeatures should be nil when disabled")
}

// TestRunScheduledIMAPSync_NoCredentials verifies that the IMAP path
// in runScheduledSync is reachable — i.e. an IMAP source row makes the
// dispatcher build an IMAP client and surface a credentials error,
// rather than the misleading "oauth2: token expired and refresh token
// is not set" message reported in #329.
func TestRunScheduledIMAPSync_NoCredentials(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	const imapID = "imaps://user@example.com@imap.example.com:993"
	_, err = s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")

	// getOAuthMgr is only invoked on the Gmail path; fail loudly so
	// any wrong-path dispatch is obvious.
	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested for IMAP source", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	err = runScheduledSync(context.Background(), imapID, s, getOAuthMgr)
	require.Error(err, "runScheduledSync(imap, no creds) want credentials error")
	msg := err.Error()
	assert.False(strings.Contains(msg, "refresh token") || strings.Contains(msg, "token may be expired"),
		"IMAP path produced Gmail-flavoured error %q — dispatch is still Gmail-only", msg)
	assert.True(strings.Contains(msg, "no credentials") || strings.Contains(msg, "IMAP"),
		"error %q does not mention IMAP credentials", msg)
}

// TestRunScheduledIMAPSync_DispatchByDisplayName verifies the daemon
// resolves IMAP sources when config.toml lists the account as a plain
// email — i.e. the lookup key matches the source's display_name rather
// than its imaps:// identifier. Regression: a previous version only
// matched against identifier, so config-driven scheduled syncs fell
// through to the Gmail OAuth path (#329).
func TestRunScheduledIMAPSync_DispatchByDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	const (
		imapID    = "imaps://user@example.com@imap.example.com:993"
		imapEmail = "user@example.com"
	)
	src, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(src.ID, imapEmail), "set display_name")

	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested for IMAP source", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	// Pass the email (as config.toml `email = "..."` would supply it),
	// not the imaps:// identifier. Dispatch must still land on the
	// IMAP path; absence of credentials produces an IMAP-shaped error.
	err = runScheduledSync(context.Background(), imapEmail, s, getOAuthMgr)
	require.Error(err, "runScheduledSync(email, no creds) want IMAP credentials error")
	msg := err.Error()
	assert.False(strings.Contains(msg, "refresh token") || strings.Contains(msg, "token may be expired"),
		"dispatch fell through to Gmail path: %q", msg)
	assert.Contains(msg, "IMAP", "error %q does not mention IMAP — dispatch likely missed the source", msg)
}

// TestRunScheduledIMAPSync_DefaultIdentityIsDisplayName verifies the
// IMAP dispatch path writes the source's display_name (the email) as
// the default account identity — never the raw imaps:// identifier
// URL. Regression: a previous version passed src.Identifier, which
// would inject e.g. "imaps://user@host:993" into account_identities
// when the user had cleared their identities.
func TestRunScheduledIMAPSync_DefaultIdentityIsDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	// Use a closed port on loopback so buildAPIClient succeeds (the
	// client doesn't dial in its constructor) and confirmDefaultIdentity
	// fires before syncer.Full hits ECONNREFUSED.
	const (
		imapID    = "imaps://user@example.com@127.0.0.1:1"
		imapEmail = "user@example.com"
	)
	src, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(src.ID, imapEmail), "set display_name")
	require.NoError(s.UpdateSourceSyncConfig(src.ID,
		`{"host":"127.0.0.1","port":1,"username":"user@example.com","tls":true}`,
	), "set sync_config")
	require.NoError(imaplib.SaveCredentials(cfg.TokensDir(), imapID, "unused"), "save credentials")

	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	// Expected to fail at the IMAP connection; what matters is that
	// confirmDefaultIdentity ran first with the display_name.
	_ = runScheduledSync(context.Background(), imapID, s, getOAuthMgr)

	identities, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.NotEmpty(identities, "no identities written — confirmDefaultIdentity did not fire on the IMAP path")
	for _, id := range identities {
		if strings.HasPrefix(id.Address, "imaps://") ||
			strings.HasPrefix(id.Address, "imap://") ||
			strings.HasPrefix(id.Address, "imap+starttls://") {
			assert.Fail("identity is an IMAP URL — daemon polluted account_identities",
				"address=%q", id.Address)
		}
	}
	var foundEmail bool
	for _, id := range identities {
		if id.Address == imapEmail {
			foundEmail = true
			break
		}
	}
	assert.True(foundEmail, "identities = %+v, want one with Address=%q", identities, imapEmail)
}

// TestFindScheduledSyncSources verifies that the plural resolver returns
// ALL syncable source types for an identifier (imap + teams together),
// only the matching type for single-type identifiers, and an empty slice
// for unknown identifiers.
func TestFindScheduledSyncSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	// Unknown identifier returns empty slice (not nil), enabling the
	// Gmail token-first fallback in runScheduledSync.
	got, err := findScheduledSyncSources(s, "missing@example.com")
	require.NoError(err, "findScheduledSyncSources(missing)")
	assert.Empty(got, "findScheduledSyncSources(missing) should be empty")

	// An address that has BOTH an IMAP source (display_name lookup) and
	// a Teams source must return both, in stable order imap then teams.
	const (
		imapID      = "imaps://nat@host@imap.example.com:993"
		sharedEmail = "nat@x.com"
	)
	imapSrc, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(imapSrc.ID, sharedEmail), "set imap display_name")

	teamsSrc, err := s.GetOrCreateSource("teams", sharedEmail)
	require.NoError(err, "create teams source")

	got, err = findScheduledSyncSources(s, sharedEmail)
	require.NoError(err, "findScheduledSyncSources(imap+teams)")
	require.Len(got, 2, "findScheduledSyncSources(imap+teams) should return 2 sources")
	assert.Equal("imap", got[0].SourceType, "first source should be imap")
	assert.Equal(imapSrc.ID, got[0].ID, "first source ID")
	assert.Equal("teams", got[1].SourceType, "second source should be teams")
	assert.Equal(teamsSrc.ID, got[1].ID, "second source ID")

	// A gmail-only identifier returns exactly one gmail source.
	const gmailAddr = "g@x.com"
	gmailSrc, err := s.GetOrCreateSource("gmail", gmailAddr)
	require.NoError(err, "create gmail source")

	got, err = findScheduledSyncSources(s, gmailAddr)
	require.NoError(err, "findScheduledSyncSources(gmail)")
	require.Len(got, 1, "findScheduledSyncSources(gmail) should return 1 source")
	assert.Equal("gmail", got[0].SourceType, "source should be gmail")
	assert.Equal(gmailSrc.ID, got[0].ID, "gmail source ID")

	// Non-syncable types (mbox) are ignored; returns empty.
	const mboxAddr = "mbox-only@example.com"
	_, err = s.GetOrCreateSource("mbox", mboxAddr)
	require.NoError(err, "create mbox source")

	got, err = findScheduledSyncSources(s, mboxAddr)
	require.NoError(err, "findScheduledSyncSources(mbox-only)")
	assert.Empty(got, "findScheduledSyncSources(mbox-only) should be empty")

	// Discord schedules are keyed by exact guild ID. Display names are not
	// accepted because separate guilds may legitimately have the same name.
	discordA, err := s.GetOrCreateSource(sourceTypeDiscord, "113456789012345678")
	require.NoError(err, "create first Discord source")
	require.NoError(s.UpdateSourceDisplayName(discordA.ID, "Shared Guild"), "name first Discord source")
	discordB, err := s.GetOrCreateSource(sourceTypeDiscord, "223456789012345678")
	require.NoError(err, "create second Discord source")
	require.NoError(s.UpdateSourceDisplayName(discordB.ID, "Shared Guild"), "name second Discord source")

	got, err = findScheduledSyncSources(s, discordB.Identifier)
	require.NoError(err, "find Discord source by guild ID")
	require.Len(got, 1, "exact guild ID selects one Discord source")
	assert.Equal(discordB.ID, got[0].ID)

	got, err = findScheduledSyncSources(s, "Shared Guild")
	require.NoError(err, "do not resolve Discord source by display name")
	assert.Empty(got, "duplicate guild display names must not select an arbitrary source")
}

func TestScheduledTeamsImportOptionsApplyMediaPolicy(t *testing.T) {
	oldConfig := cfg
	t.Cleanup(func() { cfg = oldConfig })
	enabled := true
	cfg = &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
		Teams: config.TeamsConfig{
			MediaScope: "direct",
			AccountsConfig: map[string]config.MediaAccountConfig{
				"user@example.com": {Media: &enabled, MaxMediaMB: 7},
			},
		},
	}

	opts := scheduledTeamsImportOptions("user@example.com")
	assert.Equal(t, cfg.Teams.MediaPolicy("user@example.com"), opts.MediaPolicy)
	assert.Equal(t, cfg.AttachmentsDir(), opts.AttachmentsDir)
	assert.True(t, opts.IncludeChannels)
}

func TestRunScheduledSyncUsesSharedDiscordImporterAndRebuildsOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t)
	source, err := st.Store.GetOrCreateSource(sourceTypeDiscord, "113456789012345678")
	require.NoError(err)

	originalImport := importDiscordSourceForScheduledRun
	originalRebuild := rebuildCacheAfterScheduledSourceRun
	t.Cleanup(func() {
		importDiscordSourceForScheduledRun = originalImport
		rebuildCacheAfterScheduledSourceRun = originalRebuild
	})

	var imported []int64
	importDiscordSourceForScheduledRun = func(
		_ context.Context, gotStore *store.Store, gotSource *store.Source,
		deps discordCommandDeps, full bool, after time.Time, progress func(string),
	) (*discord.ImportSummary, error) {
		assert.Same(st.Store, gotStore)
		assert.False(full)
		assert.True(after.IsZero())
		assert.Nil(progress)
		assert.NotNil(deps.tokenManager)
		imported = append(imported, gotSource.ID)
		return nil, errors.New("synthetic Discord import failure")
	}
	rebuilds := 0
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error {
		rebuilds++
		return nil
	}

	err = runScheduledSync(context.Background(), source.Identifier, st.Store, func(string) (*oauth.Manager, error) {
		require.FailNow("Discord scheduled sync must not resolve Gmail OAuth")
		return nil, errors.New("unreachable Gmail OAuth resolution")
	})
	require.ErrorContains(err, "synthetic Discord import failure")
	assert.Equal([]int64{source.ID}, imported)
	assert.Equal(1, rebuilds)
}

func TestRunScheduledSyncLogsDiscordImportIssues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t)
	source, err := st.Store.GetOrCreateSource(sourceTypeDiscord, "113456789012345678")
	require.NoError(err)

	originalImport := importDiscordSourceForScheduledRun
	originalRebuild := rebuildCacheAfterScheduledSourceRun
	originalLogger := logger
	t.Cleanup(func() {
		importDiscordSourceForScheduledRun = originalImport
		rebuildCacheAfterScheduledSourceRun = originalRebuild
		logger = originalLogger
	})
	importDiscordSourceForScheduledRun = func(
		context.Context, *store.Store, *store.Source,
		discordCommandDeps, bool, time.Time, func(string),
	) (*discord.ImportSummary, error) {
		return &discord.ImportSummary{
			CatalogIssues: []discord.CatalogIssue{{
				Scope: discord.CatalogScopePrivateArchive, Kind: discord.CatalogIssueForbidden,
				GuildID: source.Identifier, ParentID: "300000000000000001",
				StatusCode: http.StatusForbidden, DiscordCode: 50013,
				Err: errors.New("private-response-secret"),
			}},
			ContainerIssues: []discord.ContainerIssue{{
				ContainerID: "400000000000000001", Kind: discord.ContainerIssueUnknownChannel,
				StatusCode: http.StatusNotFound, DiscordCode: 10003,
			}},
		}, nil
	}
	rebuildCacheAfterScheduledSourceRun = func(context.Context, string) error { return nil }
	var logs bytes.Buffer
	logger = slog.New(slog.NewTextHandler(&logs, nil))

	require.NoError(runScheduledSync(
		context.Background(), source.Identifier, st.Store,
		func(string) (*oauth.Manager, error) {
			require.FailNow("Discord scheduled sync must not resolve Gmail OAuth")
			return nil, errors.New("unreachable")
		},
	))
	output := logs.String()
	assert.Contains(output, "discord catalog issue")
	assert.Contains(output, "scope=private_archive")
	assert.Contains(output, "parent_id=300000000000000001")
	assert.Contains(output, "status_code=403")
	assert.Contains(output, "discord_code=50013")
	assert.Contains(output, "discord container issue")
	assert.Contains(output, "container_id=400000000000000001")
	assert.Contains(output, "kind=unknown_channel")
	assert.Contains(output, "status_code=404")
	assert.NotContains(output, "private-response-secret")
}

func TestScheduledDiscordGuildFailureDoesNotBlockLaterGuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t)
	first, err := st.Store.GetOrCreateSource(sourceTypeDiscord, "113456789012345678")
	require.NoError(err)
	second, err := st.Store.GetOrCreateSource(sourceTypeDiscord, "223456789012345678")
	require.NoError(err)
	require.Less(first.ID, second.ID)

	originalImport := importDiscordSourceForScheduledRun
	originalRebuild := rebuildCacheAfterScheduledSourceRun
	t.Cleanup(func() {
		importDiscordSourceForScheduledRun = originalImport
		rebuildCacheAfterScheduledSourceRun = originalRebuild
	})
	var imported []int64
	importDiscordSourceForScheduledRun = func(
		_ context.Context, _ *store.Store, source *store.Source,
		_ discordCommandDeps, _ bool, _ time.Time, _ func(string),
	) (*discord.ImportSummary, error) {
		imported = append(imported, source.ID)
		if source.ID == first.ID {
			return nil, errors.New("synthetic first guild failure")
		}
		return &discord.ImportSummary{}, nil
	}
	var rebuilt []string
	rebuildCacheAfterScheduledSourceRun = func(_ context.Context, identifier string) error {
		rebuilt = append(rebuilt, identifier)
		return nil
	}

	completed := make(chan string, 2)
	sched := scheduler.New(func(ctx context.Context, identifier string) error {
		err := runScheduledSync(ctx, identifier, st.Store, func(string) (*oauth.Manager, error) {
			return nil, errors.New("unreachable Gmail OAuth resolution")
		})
		completed <- identifier
		return err
	})
	for _, source := range []*store.Source{first, second} {
		require.NoError(sched.AddAccount(source.Identifier, "0 0 1 1 *"))
	}
	sched.Start()
	t.Cleanup(func() { <-sched.Stop().Done() })
	awaitCompletion := func() string {
		select {
		case identifier := <-completed:
			return identifier
		case <-time.After(5 * time.Second):
			require.FailNow("timed out waiting for scheduled Discord guild")
			return ""
		}
	}

	require.NoError(sched.TriggerSync(first.Identifier))
	assert.Equal(first.Identifier, awaitCompletion())
	require.NoError(sched.TriggerSync(second.Identifier))
	assert.Equal(second.Identifier, awaitCompletion())

	assert.Equal([]int64{first.ID, second.ID}, imported)
	assert.Equal([]string{first.Identifier, second.Identifier}, rebuilt,
		"each scheduled guild invocation rebuilds its cache exactly once")
}

func TestCronExpressionValidation(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"daily at 2am", "0 2 * * *", false},
		{"every 15 min", "*/15 * * * *", false},
		{"weekly sunday", "0 0 * * 0", false},
		{"monthly first", "0 0 1 * *", false},
		{"twice daily", "0 8,18 * * *", false},
		{"invalid", "not a cron", true},
		{"empty", "", true},
		{"too many fields", "* * * * * *", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := scheduler.ValidateCronExpr(tt.expr)
			if tt.wantErr {
				assert.Error(t, err, "ValidateCronExpr(%q)", tt.expr)
			} else {
				assert.NoError(t, err, "ValidateCronExpr(%q)", tt.expr)
			}
		})
	}
}
