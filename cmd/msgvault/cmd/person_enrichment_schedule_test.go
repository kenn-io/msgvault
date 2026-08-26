package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type scheduleEnrichmentWorkerFunc func(context.Context, int64) (bool, error)

func (f scheduleEnrichmentWorkerFunc) RunOnce(ctx context.Context, runID int64) (bool, error) {
	return f(ctx, runID)
}

func TestPersonEnrichmentScheduleResumesRunningRunsAcrossSecondPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	occurrence := time.Date(2026, 8, 23, 15, 42, 37, 0, time.FixedZone("offset", 2*60*60))
	oldIDs := make([]int64, 0, 201)
	for i := range 201 {
		run, created, err := f.Store.StartRun(t.Context(), personenrichment.RunStart{
			Kind: "manual", RequestedBy: "old-" + formatScheduleTestID(i),
			RequestedAt: occurrence.Add(-time.Duration(i+1) * time.Minute),
		})
		require.NoError(err)
		require.True(created)
		oldIDs = append(oldIDs, run.ID)
	}
	slices.Reverse(oldIDs)
	calls := make([]int64, 0, 202)
	runner := &personEnrichmentSchedule{
		Store: f.Store,
		Worker: scheduleEnrichmentWorkerFunc(func(_ context.Context, runID int64) (bool, error) {
			calls = append(calls, runID)
			return false, nil
		}),
		CatchUpLimit: 200,
	}

	require.NoError(runner.Wake(t.Context(), occurrence))
	require.GreaterOrEqual(len(calls), 202)
	assert.Equal(oldIDs, calls[:201], "all captured old runs must resume oldest-first")
	current, err := f.Store.ListRunningRuns(t.Context(), personenrichment.RunningRunFilter{Limit: 200})
	require.NoError(err)
	assert.Empty(current)

	before := len(calls)
	require.NoError(runner.Wake(t.Context(), occurrence))
	assert.Len(calls, before, "the same canonical occurrence must not create another spend scope")
	var scheduled int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT COUNT(*) FROM person_enrichment_runs
		WHERE kind = 'scheduled' AND requested_by = ?`),
		canonicalPersonEnrichmentOccurrence(occurrence)).Scan(&scheduled))
	assert.Equal(1, scheduled)
}

func TestPersonEnrichmentScheduleResumesRunningRunAfterCrash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile := scheduleTestEnrichmentProfile(t)
	_, err := f.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)

	first := scheduleTestPerson(t, f, "pending-person@example.test")
	second := scheduleTestPerson(t, f, "retry-person@example.test")
	for _, person := range []*store.Person{first, second} {
		_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
		require.NoError(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	now := base.Add(time.Second)
	oldRun, _, err := f.Store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "crashed-run", RequestedAt: base.Add(-time.Hour),
	})
	require.NoError(err)

	pending := scheduleTestAttempt(t, f.Store, profile, oldRun.ID, first, now, "pending-owner", "a")
	require.NoError(f.Store.AuthorizeAttemptDispatch(t.Context(), pending.Token))
	require.NoError(f.Store.RecordProviderStarted(t.Context(), pending.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: "pending-job", StartedAt: now,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		ProgramFingerprint: strings.Repeat("c", 64),
	}))
	require.NoError(f.Store.SchedulePoll(t.Context(), pending.Token, personenrichment.Result{
		State: personenrichment.ResultPending, JobID: "pending-job", PollAfter: time.Minute,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
	}))

	retry := scheduleTestAttempt(t, f.Store, profile, oldRun.ID, second, now, "retry-owner", "d")
	futureRetry := base.Add(30 * time.Minute)
	require.NoError(f.Store.ScheduleRetry(t.Context(), retry.Token, personenrichment.RetryUpdate{
		Failure:      personenrichment.SafeFailure{Class: personenrichment.FailureTransient, Message: "safe retry"},
		NextActionAt: futureRetry,
	}))

	calls := make([]int64, 0)
	worker := scheduleEnrichmentWorkerFunc(func(ctx context.Context, runID int64) (bool, error) {
		calls = append(calls, runID)
		lease, claimErr := f.Store.ClaimWork(ctx, personenrichment.ClaimOptions{
			RunID: runID, Owner: "recovery-worker", ProviderName: profile.Name,
			Now: now, LeaseDuration: time.Minute,
		})
		if claimErr != nil || lease == nil {
			return false, claimErr
		}
		if lease.ActiveAttempt != nil {
			return true, f.Store.MarkTerminal(ctx, lease.Token, personenrichment.SafeFailure{
				Class: personenrichment.FailureTerminal, Message: "synthetic terminal recovery",
			})
		}
		return true, f.Store.ReleaseWork(ctx, lease.Token, personenrichment.WorkRelease{Outcome: "complete"})
	})
	runner := &personEnrichmentSchedule{Store: f.Store, Worker: worker, CatchUpLimit: 200}

	now = base.Add(2 * time.Minute)
	require.NoError(runner.Wake(t.Context(), now))
	old, err := f.Store.GetPersonEnrichmentRunContext(t.Context(), oldRun.ID)
	require.NoError(err)
	assert.Equal("running", old.State, "future retry keeps the crashed run durable")
	storedRetry, err := f.Store.GetPersonEnrichmentAttemptContext(t.Context(), retry.ID)
	require.NoError(err)
	require.NotNil(storedRetry.NextActionAt)
	assert.Equal(futureRetry, storedRetry.NextActionAt.UTC())
	assert.Equal(oldRun.ID, calls[0], "old run resumes before the current occurrence")

	now = futureRetry.Add(time.Second)
	require.NoError(runner.Wake(t.Context(), now))
	old, err = f.Store.GetPersonEnrichmentRunContext(t.Context(), oldRun.ID)
	require.NoError(err)
	assert.Equal("failed", old.State,
		"a recovered run whose every attempt ended terminal-failed derives failed")
}

func TestPersonEnrichmentScheduleRegistersWithoutResolvingProviderCredentials(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	catalog, err := f.Store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(err)
	require.NotEmpty(catalog.Targets)
	targetKey := catalog.Targets[0].Key
	t.Setenv("TEST_PERSON_ENRICHMENT_SUPPRESSION_KEY", strings.Repeat("s", 32))
	t.Setenv("TEST_EXA_CREDENTIAL", "")
	t.Setenv("TEST_SIXTYFOUR_CREDENTIAL", "")
	config := personenrichment.Config{
		Enabled: true, Schedule: "*/15 * * * *", BatchSize: 25,
		LeaseDuration: time.Minute, SuppressionKeyEnv: "TEST_PERSON_ENRICHMENT_SUPPRESSION_KEY",
		Providers: []personenrichment.ProviderConfig{
			{
				Name: "exa-scheduled", Kind: personenrichment.ProviderExa, Enabled: true,
				Endpoint: "https://exa.example.test/search", APIKeyEnv: "TEST_EXA_CREDENTIAL",
				Mode: "people", NumResults: 1,
				AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
				TargetKeys:         []string{targetKey}, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
				RefreshInterval: time.Hour, RequestTimeout: time.Minute, PollInterval: time.Minute,
				MaxJobAge: time.Hour, MaxRetries: 2, MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
			},
			{
				Name: "sixtyfour-scheduled", Kind: personenrichment.ProviderSixtyfour, Enabled: true,
				Endpoint: "https://sixtyfour.example.test/start", PollEndpoint: "https://sixtyfour.example.test/poll",
				APIKeyEnv: "TEST_SIXTYFOUR_CREDENTIAL", Tier: "medium",
				AllowedIdentifiers: []personenrichment.IdentifierClass{
					personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
				},
				TargetKeys: []string{targetKey}, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
				RefreshInterval: time.Hour, RequestTimeout: time.Minute, PollInterval: time.Minute,
				MaxJobAge: time.Hour, MaxRetries: 2, MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
			},
		},
	}
	sched := scheduler.New(nil)
	require.NoError(registerPersonEnrichmentJob(t.Context(), sched, f.Store, config))
	assert.True(sched.IsJobScheduled(personEnrichmentJob))

	var profiles int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_enrichment_profiles`).Scan(&profiles))
	assert.Equal(2, profiles)
}

type scheduleFunctionProvider struct {
	start func(context.Context, personenrichment.Request) (personenrichment.Attempt, error)
	poll  func(context.Context, personenrichment.Attempt) (personenrichment.Result, error)
}

func (p *scheduleFunctionProvider) Start(
	ctx context.Context, request personenrichment.Request,
) (personenrichment.Attempt, error) {
	return p.start(ctx, request)
}

func (p *scheduleFunctionProvider) Poll(
	ctx context.Context, attempt personenrichment.Attempt,
) (personenrichment.Result, error) {
	return p.poll(ctx, attempt)
}

func TestPersonEnrichmentScheduleRealWorkerPollsAndRetriesAcrossRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Second)
	config, profile, target := scheduleWorkerProfile(t, f, "a-restart", "RESTART_PROVIDER_KEY")
	_, _, err := f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	for _, email := range []string{"pending-person@example.test", "retry-person@example.test"} {
		person := scheduleTestPerson(t, f, email)
		_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
		require.NoError(err)
	}
	oldRun, _, err := f.Store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "real-worker-crash", RequestedAt: now.Add(-time.Hour),
	})
	require.NoError(err)

	requests := make(map[string]personenrichment.Request)
	polls := make(map[string]int)
	retryRequestID := ""
	provider := &scheduleFunctionProvider{
		start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
			jobID := "job:" + request.RequestHash[:16]
			requestID := "request:" + request.RequestHash[:16]
			requests[jobID] = request
			if strings.Contains(request.Identity.Email, "retry-person") {
				retryRequestID = requestID
			}
			return personenrichment.Attempt{
				State: personenrichment.AttemptPending, RequestID: requestID,
				JobID: jobID, PollAfter: time.Minute, StartedAt: now,
				AdapterVersion: "schedule-adapter-v1", SchemaVersion: "schedule-wire-v1",
				ProgramFingerprint: scheduleWorkerProgramFingerprint(t),
			}, nil
		},
		poll: func(_ context.Context, attempt personenrichment.Attempt) (personenrichment.Result, error) {
			polls[attempt.JobID]++
			if attempt.RequestID == retryRequestID && polls[attempt.JobID] == 1 {
				return personenrichment.Result{}, &personenrichment.ProviderError{
					Provider: config.Name, RequestID: attempt.RequestID,
					Class: personenrichment.FailureTransient, RetryAfter: "1800",
				}
			}
			return scheduleWorkerResult(t, requests[attempt.JobID], target,
				attempt.RequestID, attempt.JobID), nil
		},
	}
	factory := personenrichment.ProviderFactory(func(
		personenrichment.ProviderConfig, string,
	) (personenrichment.Provider, error) {
		return provider, nil
	})
	first := scheduleWorker(t, f.Store, now, map[string]personenrichment.ProviderFactory{
		config.Name: factory,
	}, map[string]personenrichment.ProviderConfig{config.Name: config}, func(string) (string, bool) {
		return "restart-secret", true
	})
	for range 2 {
		processed, runErr := first.RunOnce(t.Context(), oldRun.ID)
		require.NoError(runErr)
		require.True(processed)
	}

	now = now.Add(2 * time.Minute)
	restarted := scheduleWorker(t, f.Store, now, map[string]personenrichment.ProviderFactory{
		config.Name: factory,
	}, map[string]personenrichment.ProviderConfig{config.Name: config}, func(string) (string, bool) {
		return "restart-secret", true
	})
	runner := &personEnrichmentSchedule{Store: f.Store, Worker: restarted, CatchUpLimit: 200}
	require.NoError(runner.Wake(t.Context(), now))
	old, err := f.Store.GetPersonEnrichmentRunContext(t.Context(), oldRun.ID)
	require.NoError(err)
	assert.Equal("running", old.State)
	retryAttempt := scheduleAttemptByProviderRequestID(t, f.Store, retryRequestID)
	require.NotNil(retryAttempt.NextActionAt)
	futureRetry := *retryAttempt.NextActionAt
	assert.True(futureRetry.After(now))
	for _, count := range polls {
		assert.Equal(1, count)
	}

	now = futureRetry.Add(time.Second)
	restarted = scheduleWorker(t, f.Store, now, map[string]personenrichment.ProviderFactory{
		config.Name: factory,
	}, map[string]personenrichment.ProviderConfig{config.Name: config}, func(string) (string, bool) {
		return "restart-secret", true
	})
	runner = &personEnrichmentSchedule{Store: f.Store, Worker: restarted, CatchUpLimit: 200}
	require.NoError(runner.Wake(t.Context(), now))
	old, err = f.Store.GetPersonEnrichmentRunContext(t.Context(), oldRun.ID)
	require.NoError(err)
	assert.Equal("succeeded", old.State)
	var retryPolls int
	for jobID, request := range requests {
		if strings.Contains(request.Identity.Email, "retry-person") {
			retryPolls = polls[jobID]
		}
	}
	assert.Equal(2, retryPolls)
}

func TestPersonEnrichmentScheduleRealWorkerIsolatesProviderCredentialsAndFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Second)
	missingConfig, missingProfile, _ := scheduleWorkerProfile(t, f, "a-missing", "MISSING_PROVIDER_KEY")
	failingConfig, failingProfile, _ := scheduleWorkerProfile(t, f, "b-failing", "FAILING_PROVIDER_KEY")
	goodConfig, goodProfile, goodTarget := scheduleWorkerProfile(t, f, "c-good", "GOOD_PROVIDER_KEY")
	for _, profile := range []personenrichment.ProviderProfile{missingProfile, failingProfile, goodProfile} {
		_, _, err := f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
		require.NoError(err)
	}
	person := scheduleTestPerson(t, f, "provider-isolation@example.test")
	_, err := f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(err)
	t.Setenv("UNRELATED_CHILD_SECRET", "must-not-forward")
	lookups := make([]string, 0, 2)
	missingFactories, failingFactories, goodFactories, goodStarts := 0, 0, 0, 0
	worker := scheduleWorker(t, f.Store, now, map[string]personenrichment.ProviderFactory{
		missingConfig.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			missingFactories++
			return nil, errors.New("missing-credential provider factory must not run")
		},
		failingConfig.Name: func(_ personenrichment.ProviderConfig, credential string) (personenrichment.Provider, error) {
			failingFactories++
			assert.Equal("failing-provider-secret", credential)
			return nil, errors.New("synthetic provider construction failure")
		},
		goodConfig.Name: func(_ personenrichment.ProviderConfig, credential string) (personenrichment.Provider, error) {
			goodFactories++
			assert.Equal("good-provider-secret", credential)
			assert.NotContains(credential, "must-not-forward")
			return &scheduleFunctionProvider{
				start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
					goodStarts++
					result := scheduleWorkerResult(t, request, goodTarget, "good-request", "")
					return personenrichment.Attempt{
						State: personenrichment.AttemptComplete, RequestID: result.RequestID,
						AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
						ProgramFingerprint: scheduleWorkerProgramFingerprint(t), Result: &result,
					}, nil
				},
				poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
					return personenrichment.Result{}, nil
				},
			}, nil
		},
	}, map[string]personenrichment.ProviderConfig{
		missingConfig.Name: missingConfig, failingConfig.Name: failingConfig,
		goodConfig.Name: goodConfig,
	}, func(name string) (string, bool) {
		lookups = append(lookups, name)
		if name == failingConfig.APIKeyEnv {
			return "failing-provider-secret", true
		}
		if name == goodConfig.APIKeyEnv {
			return "good-provider-secret", true
		}
		return "", false
	})
	runner := &personEnrichmentSchedule{Store: f.Store, Worker: worker, CatchUpLimit: 200}
	require.NoError(runner.Wake(t.Context(), now))
	assert.Zero(missingFactories)
	assert.Equal(1, failingFactories)
	assert.Equal(1, goodFactories)
	assert.Equal(1, goodStarts)
	assert.ElementsMatch([]string{
		missingConfig.APIKeyEnv, failingConfig.APIKeyEnv, goodConfig.APIKeyEnv,
	}, lookups)
	assert.NotContains(lookups, "UNRELATED_CHILD_SECRET")
}

func scheduleWorkerProfile(
	t *testing.T, f *storetest.Fixture, name, credentialEnv string,
) (personenrichment.ProviderConfig, personenrichment.ProviderProfile, personfacts.TargetDescriptor) {
	t.Helper()
	catalog, err := f.Store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	var target personfacts.TargetDescriptor
	for _, candidate := range catalog.Targets {
		if candidate.ValueType == personfacts.ValueText && candidate.Cardinality == personfacts.CardinalitySingle {
			target = candidate
			break
		}
	}
	require.NotEmpty(t, target.Key)
	config := personenrichment.ProviderConfig{
		Name: name, Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://" + name + ".example.test/search", APIKeyEnv: credentialEnv,
		Mode: "people", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName,
			personenrichment.IdentifierEmail,
			personenrichment.IdentifierCurrentCompany,
		},
		TargetKeys: []string{target.Key}, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		RefreshInterval: time.Hour, RequestTimeout: time.Minute, PollInterval: time.Minute,
		MaxJobAge: time.Hour, MaxRetries: 2, MaxRequestsPerRun: 100, MaxRequestsPerDay: 1000,
	}
	profile, err := config.Profile(catalog)
	require.NoError(t, err)
	_, err = f.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	return config, profile, target
}

func scheduleWorker(
	t *testing.T, st *store.Store, now time.Time,
	factories map[string]personenrichment.ProviderFactory,
	configs map[string]personenrichment.ProviderConfig,
	credential personenrichment.CredentialLookup,
) *personenrichment.Worker {
	t.Helper()
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x73}, 32))
	require.NoError(t, err)
	gate, err := personenrichment.NewEgressGate(st, st, hasher, credential)
	require.NoError(t, err)
	worker, err := personenrichment.NewWorker(st, st, *gate, factories, personenrichment.WorkerOptions{
		Owner: "schedule-real-worker", LeaseDuration: time.Minute, RenewEvery: 10 * time.Second,
		Clock: func() time.Time { return now }, Jitter: func(delay time.Duration) time.Duration { return delay },
		ProviderConfigs: configs,
	})
	require.NoError(t, err)
	return worker
}

func scheduleWorkerProgramFingerprint(t *testing.T) string {
	t.Helper()
	fingerprint, err := personenrichment.ProgramFingerprint(personenrichment.ProgramDescriptor{
		HostMappingVersion: personenrichment.HostClaimMappingVersion,
		AdapterVersion:     "schedule-adapter-v1", WireSchemaVersion: "schedule-wire-v1",
	})
	require.NoError(t, err)
	return fingerprint
}

func scheduleWorkerResult(
	t *testing.T, request personenrichment.Request, target personfacts.TargetDescriptor,
	requestID, jobID string,
) personenrichment.Result {
	t.Helper()
	return personenrichment.Result{
		State: personenrichment.ResultComplete, RequestID: requestID, JobID: jobID,
		AdapterVersion: "schedule-adapter-v1", SchemaVersion: "schedule-wire-v1",
		ProviderVersion: "schedule-provider-v1", FreshAsOf: time.Now().UTC(),
		IdentityMatches: []personenrichment.IdentityMatch{{
			Class: personenrichment.IdentifierEmail, Value: request.Identity.Email, Confidence: 1000,
		}},
		Claims: []personfacts.ProposedClaim{{
			Target: target, Relation: personfacts.RelationSupport,
			SubmittedValue: json.RawMessage(`"Synthetic schedule fact"`),
			Origin:         personfacts.OriginEnrichment,
			Confidence:     personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{
				SourceClass: personfacts.EvidenceProviderAssertion,
				Directness:  personfacts.Indirect, Authority: personfacts.AuthorityAggregator,
				Excerpt: "Synthetic scheduler provider assertion.",
			}},
		}},
	}
}

func scheduleAttemptByProviderRequestID(
	t *testing.T, st *store.Store, requestID string,
) store.PersonEnrichmentAttempt {
	t.Helper()
	attempts, err := st.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		Limit: 200,
	})
	require.NoError(t, err)
	for _, attempt := range attempts {
		if attempt.ProviderRequestID != nil && *attempt.ProviderRequestID == requestID {
			return attempt
		}
	}
	require.FailNow(t, "provider request attempt not found", requestID)
	return store.PersonEnrichmentAttempt{}
}

func scheduleTestPerson(t *testing.T, f *storetest.Fixture, email string) *store.Person {
	t.Helper()
	participant := f.EnsureParticipant(email, "Schedule Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participant)
	require.NoError(t, err)
	organization, err := f.Store.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: "Synthetic Schedule Company", Kind: store.OrganizationKindCompany,
	})
	require.NoError(t, err)
	_, err = f.Store.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID, Source: store.ProvenanceUser,
	})
	require.NoError(t, err)
	return person
}

func scheduleTestEnrichmentProfile(t *testing.T) personenrichment.ProviderProfile {
	t.Helper()
	target := personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: "attribute:bio", Revision: "revision-1",
		UniversalID: "attribute:bio", Slug: "bio", Description: "Synthetic biography",
		ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
		Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{},
	}
	profile, err := (personenrichment.ProviderConfig{
		Name: "schedule-provider", Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://schedule.example.test/search", APIKeyEnv: "SCHEDULE_PROVIDER_KEY",
		Mode: "people", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		TargetKeys:         []string{target.Key}, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		RefreshInterval: 24 * time.Hour, RequestTimeout: time.Minute, PollInterval: time.Minute,
		MaxJobAge: time.Hour, MaxRetries: 2, MaxRequestsPerRun: 1000, MaxRequestsPerDay: 10000,
	}).Profile(personfacts.Catalog{Version: "schedule-v1", Targets: []personfacts.TargetDescriptor{target}})
	require.NoError(t, err)
	return profile
}

func scheduleTestAttempt(
	t *testing.T, st *store.Store, profile personenrichment.ProviderProfile, runID int64,
	person *store.Person, now time.Time, owner, hashByte string,
) *personenrichment.DurableAttempt {
	t.Helper()
	lease, err := st.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: runID, Owner: owner, ProviderName: profile.Name,
		Now: now, LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, person.ID, lease.PersonID)
	attempt, _, err := st.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: runID, PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
		PayloadHash: strings.Repeat(hashByte, 64), RequestHash: strings.Repeat(hashByte, 64),
		PersonRevision: person.Revision, Trigger: lease.Trigger,
	})
	require.NoError(t, err)
	return attempt
}

func formatScheduleTestID(value int) string { return strconv.Itoa(value) }
