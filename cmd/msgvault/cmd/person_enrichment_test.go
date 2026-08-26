package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonEnrichmentSuppressHashesStdinBeforeDaemonRequest(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	raw := "Person@Example.com"
	normalized := "person@example.com"
	key := strings.Repeat("k", 32)
	var proxied []string
	deps := personEnrichmentCommandDeps{
		config: func() personenrichment.Config {
			return personEnrichmentCLIConfig("TEST_PERSON_ENRICHMENT_SUPPRESSION_KEY")
		},
		lookupEnv: func(name string) (string, bool) {
			assert.Equal(t, "TEST_PERSON_ENRICHMENT_SUPPRESSION_KEY", name)
			return key, true
		},
		isDaemonSubprocess: func() bool { return false },
		proxyArgs: func(_ *cobra.Command, args []string, _ map[string]string) error {
			proxied = append([]string(nil), args...)
			return nil
		},
	}
	stdout, stderr, err := executePersonEnrichmentCommand(t, deps, raw+"\n",
		"suppress", "--provider", "exa-default", "--identifier-class", "email",
		"--reason", "opt_out")
	requirements.NoError(err)
	combined := stdout + stderr + strings.Join(proxied, " ")
	checks.NotContains(combined, raw)
	checks.NotContains(combined, normalized)
	checks.Contains(proxied, "--normalization-version="+personenrichment.EmailNormalizationV1)
	checks.Contains(proxied, "--reason=opt_out")
	checks.Contains(proxied, "--actor=cli")
	hasher, hashErr := personenrichment.NewSuppressionHasher([]byte(key))
	requirements.NoError(hashErr)
	provider, ok := personEnrichmentProviderConfig(deps.config(), "exa-default")
	requirements.True(ok)
	namespace, namespaceErr := provider.ProviderNamespace()
	requirements.NoError(namespaceErr)
	want := hasher.Digest(namespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, normalized)
	checks.Contains(proxied, "--provider-namespace="+namespace)
	checks.Contains(proxied, "--key-id="+want.KeyID)
	checks.Contains(proxied, "--digest="+hex.EncodeToString(want.Digest))
	checks.NotContains(strings.Join(proxied, " "), "--provider=exa-default")
}

func TestPersonEnrichmentDaemonProxyForwardsOnlyRequiredConfiguredCredentials(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	config := personEnrichmentCLIConfig("TEST_PERSON_ENRICHMENT_SUPPRESSION_KEY")
	provider, ok := personEnrichmentProviderConfig(config, "exa-default")
	requirements.True(ok)
	values := map[string]string{
		config.SuppressionKeyEnv: "suppression-secret",
		provider.APIKeyEnv:       "provider-secret",
		"UNRELATED_KEY":          "must-not-forward",
	}
	var proxiedEnv map[string]string
	deps := personEnrichmentCommandDeps{
		config:             func() personenrichment.Config { return config },
		lookupEnv:          func(name string) (string, bool) { value, found := values[name]; return value, found },
		isDaemonSubprocess: func() bool { return false },
		proxyArgs: func(_ *cobra.Command, _ []string, env map[string]string) error {
			proxiedEnv = env
			return nil
		},
	}

	_, _, err := executePersonEnrichmentCommand(t, deps, "", "run",
		"--person=7", "--provider=exa-default", "--idempotency-key=manual-1")
	requirements.NoError(err)
	checks.Equal(map[string]string{
		config.SuppressionKeyEnv: "suppression-secret",
		provider.APIKeyEnv:       "provider-secret",
	}, proxiedEnv)

	proxiedEnv = nil
	_, _, err = executePersonEnrichmentCommand(t, deps, "", "suppress",
		"--person=7", "--reason=opt_out")
	requirements.NoError(err)
	checks.Equal(map[string]string{config.SuppressionKeyEnv: "suppression-secret"}, proxiedEnv)
}

func TestPersonEnrichmentSuppressInputModesAndReasons(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{name: "provider person id", stdin: "Opaque-ID\n", args: []string{
			"suppress", "--provider", "exa-default", "--identifier-class", "provider_person_id", "--reason", "data_subject_request"}},
		{name: "name company", stdin: "Alice Example\nExample Labs\n", args: []string{
			"suppress", "--provider", "exa-default", "--identifier-class", "name_company", "--reason", "opt_out"}},
		{name: "name only rejected", stdin: "Alice Example\n", args: []string{
			"suppress", "--provider", "exa-default", "--identifier-class", "name_company", "--reason", "opt_out"}, want: "name_company"},
		{name: "blank company rejected", stdin: "Alice Example\n \n", args: []string{
			"suppress", "--provider", "exa-default", "--identifier-class", "name_company", "--reason", "opt_out"}, want: "non-empty"},
		{name: "raw argv rejected", args: []string{
			"suppress", "person@example.com", "--provider", "exa-default", "--identifier-class", "email", "--reason", "opt_out"}, want: "unknown command"},
		{name: "both modes rejected", args: []string{
			"suppress", "--person", "7", "--provider", "exa-default", "--identifier-class", "email", "--reason", "opt_out"}, want: "exactly one"},
		{name: "class without provider rejected", stdin: "person@example.com\n", args: []string{
			"suppress", "--identifier-class", "email", "--reason", "opt_out"}, want: "exactly one"},
		{name: "bad reason", stdin: "person@example.com\n", args: []string{
			"suppress", "--provider", "exa-default", "--identifier-class", "email", "--reason", "deletion"}, want: "reason"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			var proxied []string
			deps := personEnrichmentCommandDeps{
				config: func() personenrichment.Config {
					return personEnrichmentCLIConfig("TEST_SUPPRESSION_KEY")
				},
				lookupEnv:          func(string) (string, bool) { return strings.Repeat("s", 32), true },
				isDaemonSubprocess: func() bool { return false },
				proxyArgs: func(_ *cobra.Command, args []string, _ map[string]string) error {
					proxied = append([]string(nil), args...)
					return nil
				},
			}
			stdout, stderr, err := executePersonEnrichmentCommand(t, deps, test.stdin, test.args...)
			if test.want == "" {
				requirements.NoError(err)
				checks.NotEmpty(proxied)
				checks.NotContains(stdout+stderr+strings.Join(proxied, " "), strings.TrimSpace(test.stdin))
				return
			}
			requirements.ErrorContains(err, test.want)
			checks.Empty(proxied)
		})
	}
}

func TestPersonEnrichmentSuppressSanitizesMalformedRawInputErrors(t *testing.T) {
	tests := []struct {
		name      string
		class     string
		stdin     string
		forbidden []string
	}{
		{name: "phone", class: "phone", stdin: "RAWPHONE++not-a-number++\n",
			forbidden: []string{"RAWPHONE", "rawphone", "not-a-number"}},
		{name: "public URL", class: "public_profile_url", stdin: "https://RAWURL.invalid/%zz\n",
			forbidden: []string{"RAWURL", "rawurl", "%zz"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var daemonArgs []string
			deps := personEnrichmentCommandDeps{
				config:             func() personenrichment.Config { return personEnrichmentCLIConfig("TEST_SUPPRESSION_KEY") },
				lookupEnv:          func(string) (string, bool) { return strings.Repeat("s", 32), true },
				isDaemonSubprocess: func() bool { return false },
				proxyArgs: func(_ *cobra.Command, args []string, _ map[string]string) error {
					daemonArgs = append([]string(nil), args...)
					return nil
				},
			}
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			stdout, stderr, err := executePersonEnrichmentCommand(t, deps, test.stdin,
				"suppress", "--provider", "exa-default", "--identifier-class", test.class,
				"--reason", "opt_out")
			require.Error(t, err)
			combined := err.Error() + stdout + stderr + logs.String() + strings.Join(daemonArgs, " ")
			for _, marker := range test.forbidden {
				if strings.Contains(combined, marker) {
					assert.Fail(t, "privacy boundary leaked a forbidden input marker")
				}
			}
			assert.Empty(t, daemonArgs)
		})
	}
}

func TestPersonEnrichmentSuppressPersonModeRejectsProviderMetadataSmuggling(t *testing.T) {
	marker := "RAW-MARKER"
	fields := []struct {
		name string
		args []string
	}{
		{name: "provider", args: []string{"--provider", marker}},
		{name: "provider namespace", args: []string{"--provider-namespace", marker}},
		{name: "identifier class", args: []string{"--identifier-class", marker}},
		{name: "normalization version", args: []string{"--normalization-version", marker}},
		{name: "key ID", args: []string{"--key-id", marker}},
		{name: "digest", args: []string{"--digest", marker}},
		{name: "actor", args: []string{"--actor", marker}},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			var daemonArgs []string
			deps := personEnrichmentCommandDeps{
				isDaemonSubprocess: func() bool { return false },
				proxyArgs: func(_ *cobra.Command, args []string, _ map[string]string) error {
					daemonArgs = append([]string(nil), args...)
					return nil
				},
			}
			args := []string{"suppress", "--person", "7", "--reason", "opt_out"}
			args = append(args, field.args...)
			stdout, stderr, err := executePersonEnrichmentCommand(t, deps, "", args...)
			require.Error(t, err)
			assert.Empty(t, daemonArgs)
			if strings.Contains(err.Error()+stdout+stderr+strings.Join(daemonArgs, " "), marker) {
				assert.Fail(t, "person-mode rejection leaked smuggled metadata")
			}
		})
	}
}

func TestPersonEnrichmentSuppressDaemonPersistsOnlyDigestMetadata(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	config := personEnrichmentCLIConfig("TEST_SUPPRESSION_KEY")
	provider := config.Providers[0]
	namespace, err := provider.ProviderNamespace()
	requirements.NoError(err)
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{'z'}, 32))
	requirements.NoError(err)
	digest := hasher.Digest(namespace, personenrichment.SuppressionProviderPersonID,
		personenrichment.ProviderPersonIDNormalizationV1, "Opaque-ID")
	deps := personEnrichmentCommandDeps{
		config:             func() personenrichment.Config { return config },
		openStore:          func() (*store.Store, func(), error) { return f.Store, func() {}, nil },
		lookupEnv:          func(string) (string, bool) { return strings.Repeat("z", 32), true },
		isDaemonSubprocess: func() bool { return true },
	}
	stdout, stderr, err := executePersonEnrichmentCommand(t, deps, "",
		"suppress", "--provider-namespace", namespace,
		"--identifier-class", "provider_person_id",
		"--normalization-version", personenrichment.ProviderPersonIDNormalizationV1,
		"--key-id", digest.KeyID, "--digest", hex.EncodeToString(digest.Digest),
		"--reason", "data_subject_request", "--actor", "cli")
	requirements.NoError(err)
	checks.NotContains(stdout+stderr, "Opaque-ID")
	found, err := f.Store.HasPersonEnrichmentSuppressionContext(t.Context(), digest)
	requirements.NoError(err)
	checks.True(found)
	rows, err := f.Store.ListPersonEnrichmentSuppressionsContext(t.Context(),
		store.PersonEnrichmentSuppressionFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	checks.Equal(hex.EncodeToString(digest.Digest[:6]), rows[0].DigestPrefix)
}

func TestPersonEnrichmentSuppressDaemonRejectsMismatchedDurableKey(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	config := personEnrichmentCLIConfig("TEST_SUPPRESSION_KEY")
	namespace, err := config.Providers[0].ProviderNamespace()
	requirements.NoError(err)
	oldHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{'o'}, 32))
	requirements.NoError(err)
	oldDigest := oldHasher.Digest(namespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "old@example.test")
	requirements.NoError(f.Store.InsertPersonEnrichmentSuppressionsContext(t.Context(),
		[]store.PersonEnrichmentSuppressionInput{{
			ProviderNamespace: oldDigest.ProviderNamespace, IdentifierClass: oldDigest.IdentifierClass,
			NormalizationVersion: oldDigest.NormalizationVersion, KeyID: oldDigest.KeyID,
			Digest: oldDigest.Digest, Reason: store.PersonEnrichmentSuppressionOptOut, Actor: "test",
		}}))
	newHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{'n'}, 32))
	requirements.NoError(err)
	newDigest := newHasher.Digest(namespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "new@example.test")
	deps := localPersonEnrichmentCommandDeps(config, f.Store)
	deps.lookupEnv = func(string) (string, bool) { return strings.Repeat("n", 32), true }
	_, _, err = executePersonEnrichmentCommand(t, deps, "",
		"suppress", "--provider-namespace", namespace, "--identifier-class", "email",
		"--normalization-version", personenrichment.EmailNormalizationV1,
		"--key-id", newDigest.KeyID, "--digest", hex.EncodeToString(newDigest.Digest),
		"--reason", "opt_out", "--actor", "cli")
	requirements.ErrorIs(err, personenrichment.ErrSuppressionKeyMismatch)
	rows, err := f.Store.ListPersonEnrichmentSuppressionsContext(t.Context(),
		store.PersonEnrichmentSuppressionFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assert.Equal(t, oldDigest.KeyID, rows[0].KeyID)
}

func TestPersonEnrichmentSuppressDaemonRejectsWrongConfiguredKeyOnEmptyLedger(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	config := personEnrichmentCLIConfig("TEST_SUPPRESSION_KEY")
	namespace, err := config.Providers[0].ProviderNamespace()
	requirements.NoError(err)
	wrongHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{'w'}, 32))
	requirements.NoError(err)
	wrong := wrongHasher.Digest(namespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "wrong@example.test")
	deps := localPersonEnrichmentCommandDeps(config, f.Store)
	deps.lookupEnv = func(string) (string, bool) { return strings.Repeat("c", 32), true }
	_, _, err = executePersonEnrichmentCommand(t, deps, "",
		"suppress", "--provider-namespace", namespace, "--identifier-class", "email",
		"--normalization-version", personenrichment.EmailNormalizationV1,
		"--key-id", wrong.KeyID, "--digest", hex.EncodeToString(wrong.Digest),
		"--reason", "opt_out", "--actor", "cli")
	requirements.ErrorIs(err, personenrichment.ErrSuppressionKeyMismatch)
	rows, err := f.Store.ListPersonEnrichmentSuppressionsContext(t.Context(),
		store.PersonEnrichmentSuppressionFilter{Limit: 10})
	requirements.NoError(err)
	assert.Empty(t, rows)
}

func TestPersonEnrichmentSuppressDaemonConcurrentDifferingKeysPersistOnlyConfiguredKey(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	config := personEnrichmentCLIConfig("TEST_SUPPRESSION_KEY")
	namespace, err := config.Providers[0].ProviderNamespace()
	requirements.NoError(err)
	configuredKey := strings.Repeat("c", 32)
	configuredHasher, err := personenrichment.NewSuppressionHasher([]byte(configuredKey))
	requirements.NoError(err)
	wrongHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{'w'}, 32))
	requirements.NoError(err)
	configured := configuredHasher.Digest(namespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "configured@example.test")
	wrong := wrongHasher.Digest(namespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "wrong@example.test")
	deps := localPersonEnrichmentCommandDeps(config, f.Store)
	deps.lookupEnv = func(string) (string, bool) { return configuredKey, true }
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, digest := range []personenrichment.SuppressionDigest{configured, wrong} {
		go func() {
			<-start
			_, _, commandErr := executePersonEnrichmentCommand(t, deps, "",
				"suppress", "--provider-namespace", namespace, "--identifier-class", "email",
				"--normalization-version", personenrichment.EmailNormalizationV1,
				"--key-id", digest.KeyID, "--digest", hex.EncodeToString(digest.Digest),
				"--reason", "opt_out", "--actor", "cli")
			errs <- commandErr
		}()
	}
	close(start)
	gotErrs := []error{<-errs, <-errs}
	checks.Equal(1, countNilErrors(gotErrs))
	checks.Equal(1, countMatchingErrors(gotErrs, personenrichment.ErrSuppressionKeyMismatch))
	rows, err := f.Store.ListPersonEnrichmentSuppressionsContext(t.Context(),
		store.PersonEnrichmentSuppressionFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	checks.Equal(configured.KeyID, rows[0].KeyID)
}

func countNilErrors(errs []error) int {
	count := 0
	for _, err := range errs {
		if err == nil {
			count++
		}
	}
	return count
}

func countMatchingErrors(errs []error, target error) int {
	count := 0
	for _, err := range errs {
		if errors.Is(err, target) {
			count++
		}
	}
	return count
}

func TestPersonEnrichmentConsentProfilesStatusAndRevoke(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	provider, profile, _ := scheduleWorkerProfile(t, f, "cli-controls", "TEST_CLI_PROVIDER_KEY")
	config := personenrichment.Config{Enabled: true, SuppressionKeyEnv: "TEST_SUPPRESSION_KEY", Providers: []personenrichment.ProviderConfig{provider}}
	deps := localPersonEnrichmentCommandDeps(config, f.Store)

	profiles, _, err := executePersonEnrichmentCommand(t, deps, "", "profiles", "--json")
	requirements.NoError(err)
	checks.Contains(profiles, profile.Fingerprint)
	_, _, err = executePersonEnrichmentCommand(t, deps, "", "consent", profile.Fingerprint)
	requirements.NoError(err)
	status, _, err := executePersonEnrichmentCommand(t, deps, "", "status", "--limit", "1", "--json")
	requirements.NoError(err)
	checks.Contains(status, `"active":true`)
	_, _, err = executePersonEnrichmentCommand(t, deps, "", "revoke", profile.Fingerprint)
	requirements.NoError(err)
	status, _, err = executePersonEnrichmentCommand(t, deps, "", "status", "--limit", "1", "--json")
	requirements.NoError(err)
	checks.Contains(status, `"active":false`)
}

func TestPersonEnrichmentManualRunPersistsAndReusesRunIDBeforeWork(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("manual@example.test", "Manual Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	requirements.NoError(err)
	provider, profile, _ := scheduleWorkerProfile(t, f, "manual-provider", "TEST_MANUAL_PROVIDER_KEY")
	_, _, err = f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	requirements.NoError(err)
	config := personenrichment.Config{Enabled: true, SuppressionKeyEnv: "TEST_SUPPRESSION_KEY", Providers: []personenrichment.ProviderConfig{provider}}
	var observed []int64
	deps := localPersonEnrichmentCommandDeps(config, f.Store)
	now := time.Date(2026, 8, 23, 20, 0, 0, 0, time.UTC)
	deps.clock = func() time.Time { return now }
	deps.newManualWorker = func(_ context.Context, st *store.Store, _ personenrichment.Config) (personEnrichmentScheduleWorker, error) {
		return personEnrichmentManualWorkerFunc(func(ctx context.Context, runID int64) (bool, error) {
			run, getErr := st.GetPersonEnrichmentRunContext(ctx, runID)
			require.NoError(t, getErr)
			assert.Equal(t, "manual-key", run.RequestedBy)
			observed = append(observed, runID)
			if len(observed) == 1 {
				work, workErr := st.ListPersonEnrichmentWorkContext(ctx, store.PersonEnrichmentWorkFilter{
					PersonID: person.ID, ProfileFingerprint: profile.Fingerprint, Limit: 10,
				})
				require.NoError(t, workErr)
				require.NotEmpty(t, work)
				lease, claimErr := st.ClaimWork(ctx, personenrichment.ClaimOptions{
					RunID: runID, Owner: "manual-test-worker", ProviderName: profile.Name,
					Now: now, LeaseDuration: time.Minute,
				})
				require.NoError(t, claimErr)
				require.NotNil(t, lease, "work=%+v now=%s", work, now)
				assert.Equal(t, runID, lease.RunID)
			}
			return false, nil
		}), nil
	}

	first, _, err := executePersonEnrichmentCommand(t, deps, "", "run",
		"--person", strconv.FormatInt(person.ID, 10), "--provider", "manual-provider", "--idempotency-key", "manual-key", "--json")
	requirements.NoError(err)
	second, _, err := executePersonEnrichmentCommand(t, deps, "", "run",
		"--person", strconv.FormatInt(person.ID, 10), "--provider", "manual-provider", "--idempotency-key", "manual-key", "--json")
	requirements.NoError(err)
	requirements.Len(observed, 2)
	checks.Equal(observed[0], observed[1])
	checks.Contains(first, `"state":"running"`)
	checks.Contains(second, `"state":"running"`)
	run, err := f.Store.GetPersonEnrichmentRunContext(t.Context(), observed[0])
	requirements.NoError(err)
	checks.Equal("running", run.State)
}

func TestPersonEnrichmentManualRunRejectsUntrackedPersonWithoutRunOrWork(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("untracked@example.test", "Untracked Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	provider, profile, _ := scheduleWorkerProfile(t, f, "manual-untracked", "TEST_MANUAL_PROVIDER_KEY")
	_, _, err = f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	requirements.NoError(err)
	config := personenrichment.Config{Enabled: true, SuppressionKeyEnv: "TEST_SUPPRESSION_KEY", Providers: []personenrichment.ProviderConfig{provider}}
	deps := localPersonEnrichmentCommandDeps(config, f.Store)
	deps.newManualWorker = func(context.Context, *store.Store, personenrichment.Config) (personEnrichmentScheduleWorker, error) {
		require.FailNow(t, "untracked manual run must not construct a worker")
		return nil, errors.New("unreachable test worker construction")
	}
	_, _, err = executePersonEnrichmentCommand(t, deps, "", "run",
		"--person", strconv.FormatInt(person.ID, 10), "--provider", provider.Name,
		"--idempotency-key", "untracked-key", "--json")
	requirements.ErrorContains(err, "tracked")
	var runs, work int64
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_enrichment_runs WHERE kind = 'manual' AND requested_by = 'untracked-key'`).Scan(&runs))
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT COUNT(*) FROM person_enrichment_work WHERE person_id = ?`), person.ID).Scan(&work))
	checks.Zero(runs)
	checks.Zero(work)
}

func TestPersonEnrichmentManualRunKeepsRunIDOnLeaseAndAttemptAndReportsFinalCounts(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("finished@example.test", "Finished Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	requirements.NoError(err)
	provider, profile, _ := scheduleWorkerProfile(t, f, "manual-finished", "TEST_MANUAL_PROVIDER_KEY")
	_, _, err = f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	requirements.NoError(err)
	config := personenrichment.Config{Enabled: true, SuppressionKeyEnv: "TEST_SUPPRESSION_KEY", Providers: []personenrichment.ProviderConfig{provider}}
	now := time.Date(2026, 8, 23, 21, 0, 0, 0, time.UTC)
	completionClock := now
	deps := localPersonEnrichmentCommandDeps(config, f.Store)
	deps.clock = func() time.Time {
		current := completionClock
		completionClock = completionClock.Add(time.Minute)
		return current
	}
	var attemptID, durableRunID int64
	var calls int
	deps.newManualWorker = func(_ context.Context, st *store.Store, _ personenrichment.Config) (personEnrichmentScheduleWorker, error) {
		return personEnrichmentManualWorkerFunc(func(ctx context.Context, runID int64) (bool, error) {
			calls++
			if calls > 1 {
				return false, nil
			}
			lease, claimErr := st.ClaimWork(ctx, personenrichment.ClaimOptions{
				RunID: runID, Owner: "manual-final-worker", ProviderName: profile.Name,
				Now: now, LeaseDuration: time.Minute,
			})
			require.NoError(t, claimErr)
			require.NotNil(t, lease)
			assert.Equal(t, runID, lease.RunID)
			current, getErr := st.GetPersonContext(ctx, person.ID)
			require.NoError(t, getErr)
			attempt, created, beginErr := st.BeginAttempt(ctx, lease.Token, personenrichment.AttemptStart{
				RunID: runID, PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
				PayloadHash: strings.Repeat("a", 64), RequestHash: strings.Repeat("b", 64),
				PersonRevision: current.Revision, Trigger: lease.Trigger,
			})
			require.NoError(t, beginErr)
			require.True(t, created)
			assert.Equal(t, runID, attempt.RunID)
			require.NoError(t, st.MarkTerminal(ctx, attempt.Token, personenrichment.SafeFailure{
				Class: personenrichment.FailureTerminal, Message: "synthetic terminal result",
			}))
			attemptID, durableRunID = attempt.ID, runID
			return true, nil
		}), nil
	}

	output, _, err := executePersonEnrichmentCommand(t, deps, "", "run",
		"--person", strconv.FormatInt(person.ID, 10), "--provider", provider.Name,
		"--idempotency-key", "finished-key", "--json")
	requirements.NoError(err)
	checks.Contains(output, `"state":"failed"`)
	checks.Contains(output, `"failed_count":1`)
	attempt, err := f.Store.GetPersonEnrichmentAttemptContext(t.Context(), attemptID)
	requirements.NoError(err)
	checks.Equal(durableRunID, attempt.RunID)
	run, err := f.Store.GetPersonEnrichmentRunContext(t.Context(), durableRunID)
	requirements.NoError(err)
	checks.Equal(int64(1), run.FailedCount)
	checks.Equal("failed", run.State)
	requirements.NotNil(run.CompletedAt)
	checks.True(run.CompletedAt.After(run.RequestedAt),
		"manual runs must record the post-drain completion time, not the pre-run clock read")
}

func executePersonEnrichmentCommand(
	t *testing.T, deps personEnrichmentCommandDeps, stdin string, args ...string,
) (string, string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonEnrichmentCommand(deps))
	root.AddCommand(person)
	root.SetArgs(append([]string{"person", "enrichment"}, args...))
	root.SetIn(strings.NewReader(stdin))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func personEnrichmentCLIConfig(suppressionEnv string) personenrichment.Config {
	provider := personenrichment.ProviderConfig{
		Name: "exa-default", Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://exa.example.test/search", APIKeyEnv: "TEST_PROVIDER_KEY",
		Mode: "people", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		TargetKeys:         []string{"attribute:bio"}, RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", RefreshInterval: time.Hour,
		RequestTimeout: time.Minute, PollInterval: time.Minute, MaxJobAge: time.Hour,
		MaxRetries: 2, MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
	}
	return personenrichment.Config{Enabled: true, SuppressionKeyEnv: suppressionEnv, Providers: []personenrichment.ProviderConfig{provider}}
}
