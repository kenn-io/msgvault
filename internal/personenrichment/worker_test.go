package personenrichment_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type workerFixture struct {
	store   *store.Store
	person  *store.Person
	config  personenrichment.ProviderConfig
	profile personenrichment.ProviderProfile
	target  personfacts.TargetDescriptor
	hasher  *personenrichment.SuppressionHasher
	run     *personenrichment.DurableRun
	now     time.Time
}

func newWorkerFixture(t *testing.T, name string, mutate func(*personenrichment.ProviderConfig)) *workerFixture {
	t.Helper()
	fixture := storetest.New(t)
	participantID := fixture.EnsureParticipant("worker-person@example.test", "Worker Person", "example.test")
	person, _, err := fixture.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	_, err = fixture.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	person, err = fixture.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(t, err)
	_, err = fixture.Store.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressURL, OriginalValue: "https://profiles.example.test/worker-person",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	person, err = fixture.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(t, err)
	catalog, err := fixture.Store.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	var target personfacts.TargetDescriptor
	for _, candidate := range catalog.Targets {
		if candidate.Kind == personfacts.TargetAttribute && candidate.ValueType == personfacts.ValueText &&
			candidate.Cardinality == personfacts.CardinalitySingle && !candidate.Sensitive {
			target = candidate
			break
		}
	}
	require.NotEmpty(t, target.Key)
	cfg := personenrichment.ProviderConfig{
		Name: name, Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://api.example.test/search", APIKeyEnv: "TEST_PROVIDER_KEY",
		Mode: "deep", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierPublicProfileURL},
		TargetKeys:         []string{target.Key},
		RetentionPosture:   "zero_retention", TrainingPosture: "no_training",
		RefreshInterval: 24 * time.Hour, RequestTimeout: 5 * time.Second,
		PollInterval: time.Nanosecond, MaxJobAge: 15 * time.Minute, MaxRetries: 3,
		MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	profile, err := cfg.Profile(catalog)
	require.NoError(t, err)
	_, err = fixture.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = fixture.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(t, err)
	now := time.Now().UTC().Add(-time.Second)
	run, created, err := fixture.Store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "scheduled", RequestedBy: "worker:" + name, RequestedAt: now,
	})
	require.NoError(t, err)
	require.True(t, created)
	hasher, err := personenrichment.NewSuppressionHasher([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	return &workerFixture{
		store: fixture.Store, person: person, config: cfg, profile: profile,
		target: target, hasher: hasher, run: run, now: now,
	}
}

func (f *workerFixture) enqueue(t *testing.T) {
	t.Helper()
	require.NoError(t, f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"},
		DueAt:   f.now,
	}))
}

func (f *workerFixture) gate(t *testing.T, credential func(string) (string, bool)) personenrichment.EgressGate {
	t.Helper()
	gate, err := personenrichment.NewEgressGate(f.store, f.store, f.hasher, credential)
	require.NoError(t, err)
	return *gate
}

func (f *workerFixture) options(configs map[string]personenrichment.ProviderConfig) personenrichment.WorkerOptions {
	return personenrichment.WorkerOptions{
		Owner: "worker-test", LeaseDuration: time.Minute, RenewEvery: 20 * time.Second,
		Clock: time.Now, Jitter: func(time.Duration) time.Duration { return 0 },
		ProviderConfigs: configs,
	}
}

func (f *workerFixture) newWorker(
	t *testing.T,
	factories map[string]personenrichment.ProviderFactory,
	configs map[string]personenrichment.ProviderConfig,
	credential func(string) (string, bool),
) *personenrichment.Worker {
	t.Helper()
	worker, err := personenrichment.NewWorker(f.store, f.store, f.gate(t, credential), factories, f.options(configs))
	require.NoError(t, err)
	return worker
}

type functionProvider struct {
	start func(context.Context, personenrichment.Request) (personenrichment.Attempt, error)
	poll  func(context.Context, personenrichment.Attempt) (personenrichment.Result, error)
}

func (p *functionProvider) Start(ctx context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
	return p.start(ctx, request)
}

func (p *functionProvider) Poll(ctx context.Context, attempt personenrichment.Attempt) (personenrichment.Result, error) {
	return p.poll(ctx, attempt)
}

type guaranteedFunctionProvider struct {
	*functionProvider

	bound func(context.Context, personenrichment.Request) (personenrichment.Cost, error)
}

func (p *guaranteedFunctionProvider) GuaranteedMaxCharge(
	ctx context.Context, request personenrichment.Request,
) (personenrichment.Cost, error) {
	return p.bound(ctx, request)
}

func workerProgramFingerprint(t *testing.T, generated bool, generatedHash string) string {
	t.Helper()
	fingerprint, err := personenrichment.ProgramFingerprint(personenrichment.ProgramDescriptor{
		HostMappingVersion: personenrichment.HostClaimMappingVersion,
		AdapterVersion:     "test-adapter-v1", WireSchemaVersion: "test-wire-v1",
		GeneratedSchema: generated, GeneratedSchemaHash: generatedHash,
	})
	require.NoError(t, err)
	return fingerprint
}

func workerResult(
	t *testing.T, request personenrichment.Request, target personfacts.TargetDescriptor,
	requestID, jobID string, generated bool, generatedHash string, cost personenrichment.Cost,
) personenrichment.Result {
	t.Helper()
	return personenrichment.Result{
		State: personenrichment.ResultComplete, RequestID: requestID, JobID: jobID,
		AdapterVersion: "test-adapter-v1", SchemaVersion: "test-wire-v1",
		GeneratedSchema: generated, GeneratedSchemaHash: generatedHash,
		ProviderVersion: "test-provider-v1", FreshAsOf: time.Now().UTC(), Cost: cost,
		IdentityMatches: []personenrichment.IdentityMatch{{
			Class: personenrichment.IdentifierPublicProfileURL,
			Value: request.Identity.PublicProfileURLs[0], Confidence: 1000,
		}},
		Claims: []personfacts.ProposedClaim{{
			Target: target, Relation: personfacts.RelationSupport,
			SubmittedValue: json.RawMessage(`"Synthetic biography"`),
			Origin:         personenrichmentClaimOrigin(),
			Confidence:     personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{
				SourceClass: personfacts.EvidenceProviderAssertion,
				Directness:  personfacts.Indirect, Authority: personfacts.AuthorityAggregator,
				Excerpt: "Synthetic provider assertion.",
			}},
		}},
	}
}

func personenrichmentClaimOrigin() personfacts.ClaimOrigin {
	return personfacts.OriginEnrichment
}

func TestWorkerResumesExactBoundAsyncAttemptAfterRestart(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "async-restart", nil)
	f.enqueue(t)
	var starts, polls atomic.Int64
	var polledJob atomic.Value
	const schemaHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	factory := func(_ personenrichment.ProviderConfig, _ string) (personenrichment.Provider, error) {
		return &functionProvider{
			start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
				starts.Add(1)
				return personenrichment.Attempt{
					State: personenrichment.AttemptPending, JobID: "opaque-job-42",
					PollAfter: time.Nanosecond, StartedAt: time.Now().UTC(),
					AdapterVersion: "test-adapter-v1", SchemaVersion: "test-wire-v1",
					GeneratedSchema: true, GeneratedSchemaHash: schemaHash,
					ProgramFingerprint: workerProgramFingerprint(t, true, schemaHash),
					Targets:            request.Targets,
				}, nil
			},
			poll: func(_ context.Context, attempt personenrichment.Attempt) (personenrichment.Result, error) {
				polls.Add(1)
				polledJob.Store(attempt.JobID)
				requestInput, err := f.store.LoadRequestInput(t.Context(), personenrichment.WorkLease{
					PersonID: f.person.ID, Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"},
				})
				require.NoError(t, err)
				request, _, err := personenrichment.BuildRequest(requestInput, f.profile)
				require.NoError(t, err)
				return workerResult(t, request, f.target, "", attempt.JobID, true, schemaHash, personenrichment.Cost{}), nil
			},
		}, nil
	}
	providers := map[string]personenrichment.ProviderFactory{f.config.Name: factory}
	configs := map[string]personenrichment.ProviderConfig{f.config.Name: f.config}
	credential := func(string) (string, bool) { return "test-key", true }

	first := f.newWorker(t, providers, configs, credential)
	processed, err := first.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(work, 1)
	requirements.NotNil(work[0].ActiveAttemptID)
	boundID := *work[0].ActiveAttemptID
	attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), boundID)
	requirements.NoError(err)
	requirements.NotNil(attempt.ProviderJobID)
	checks.Equal("opaque-job-42", *attempt.ProviderJobID)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_enrichment_attempts
			(run_id, person_id, profile_fingerprint, trigger_kind, trigger_generation,
			 person_revision, payload_hash, request_hash, state, lease_fence,
			 hard_cost_cap_enforced, reserved_cost_usd_micros, completed_at)
		VALUES (?, ?, ?, 'tracked', 'diagnostic', ?, ?, ?, 'terminal', 0, FALSE, 0, ?)`),
		f.run.ID, f.person.ID, f.profile.Fingerprint, f.person.Revision,
		strings.Repeat("b", 64), strings.Repeat("c", 64), time.Now().UTC())
	requirements.NoError(err)

	second := f.newWorker(t, providers, configs, credential)
	processed, err = second.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Equal(int64(1), starts.Load())
	checks.Equal(int64(1), polls.Load())
	checks.Equal("opaque-job-42", polledJob.Load())
	attempt, err = f.store.GetPersonEnrichmentAttemptContext(t.Context(), boundID)
	requirements.NoError(err)
	checks.Equal("succeeded", attempt.State)
	work, err = f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(work, 1)
	checks.Nil(work[0].ActiveAttemptID)
	checks.Nil(work[0].RunID)
}

func TestWorkerCompletesSynchronousResultAndIsolatesProviderFailure(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	failed := newWorkerFixture(t, "a-failing-provider", nil)
	goodConfig := failed.config
	goodConfig.Name = "b-working-provider"
	goodConfig.Endpoint = "https://working.example.test/search"
	goodProfile, err := goodConfig.Profile(personfacts.Catalog{Targets: failed.profile.Targets})
	requirements.NoError(err)
	_, err = failed.store.EnsurePersonEnrichmentProfile(t.Context(), goodProfile)
	requirements.NoError(err)
	_, _, err = failed.store.GrantPersonEnrichmentConsent(t.Context(), goodProfile.Fingerprint, "test")
	requirements.NoError(err)
	requirements.NoError(failed.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: failed.person.ID, ProfileFingerprint: failed.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"}, DueAt: failed.now,
	}))
	requirements.NoError(failed.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: failed.person.ID, ProfileFingerprint: goodProfile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"}, DueAt: failed.now,
	}))
	var goodStarts atomic.Int64
	factories := map[string]personenrichment.ProviderFactory{
		failed.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return &functionProvider{
				start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
					return personenrichment.Attempt{}, &personenrichment.ProviderError{Class: personenrichment.FailureTerminal}
				},
				poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
					return personenrichment.Result{}, errors.New("unexpected poll")
				},
			}, nil
		},
		goodConfig.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return &functionProvider{
				start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
					goodStarts.Add(1)
					result := workerResult(t, request, failed.target, "opaque-request-good", "", false, "", personenrichment.Cost{})
					return personenrichment.Attempt{
						State: personenrichment.AttemptComplete, RequestID: result.RequestID,
						AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
						ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
					}, nil
				},
				poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
					return personenrichment.Result{}, errors.New("unexpected poll")
				},
			}, nil
		},
	}
	configs := map[string]personenrichment.ProviderConfig{
		failed.config.Name: failed.config, goodConfig.Name: goodConfig,
	}
	worker := failed.newWorker(t, factories, configs, func(string) (string, bool) { return "test-key", true })

	processed, err := worker.RunOnce(t.Context(), failed.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	processed, err = worker.RunOnce(t.Context(), failed.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Equal(int64(1), goodStarts.Load())
	attempts, err := failed.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: failed.person.ID, RunID: failed.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 2)
	states := []string{attempts[0].State, attempts[1].State}
	checks.ElementsMatch([]string{"succeeded", "terminal"}, states)
}

func TestWorkerConcurrentRunOnceStartsProviderOnlyOnce(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "concurrent-single-start", nil)
	f.enqueue(t)
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseStart) }) }
	t.Cleanup(release)
	var starts atomic.Int64
	fingerprint := workerProgramFingerprint(t, false, "")
	provider := &functionProvider{
		start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
			if starts.Add(1) > 1 {
				return personenrichment.Attempt{}, errors.New("duplicate provider start")
			}
			close(startEntered)
			select {
			case <-releaseStart:
			case <-time.After(10 * time.Second):
				return personenrichment.Attempt{}, errors.New("provider start was never released")
			}
			// workerResult is pure construction; fatal-capable helpers stay on
			// the test goroutine because this closure runs on a worker goroutine.
			result := workerResult(t, request, f.target, "single-start-request", "", false, "", personenrichment.Cost{})
			return personenrichment.Attempt{
				State: personenrichment.AttemptComplete, RequestID: result.RequestID,
				AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
				ProgramFingerprint: fingerprint, Result: &result,
			}, nil
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	factories := map[string]personenrichment.ProviderFactory{
		f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		},
	}
	configs := map[string]personenrichment.ProviderConfig{f.config.Name: f.config}
	newWorker := func(owner string) *personenrichment.Worker {
		t.Helper()
		options := f.options(configs)
		options.Owner = owner
		worker, err := personenrichment.NewWorker(
			f.store, f.store,
			f.gate(t, func(string) (string, bool) { return "test-key", true }),
			factories, options,
		)
		requirements.NoError(err)
		return worker
	}
	first := newWorker("concurrent-worker-a")
	second := newWorker("concurrent-worker-b")
	type runResult struct {
		processed bool
		err       error
	}
	firstDone := make(chan runResult, 1)
	go func() {
		processed, err := first.RunOnce(t.Context(), f.run.ID)
		firstDone <- runResult{processed: processed, err: err}
	}()
	select {
	case <-startEntered:
	case <-time.After(10 * time.Second):
		t.Fatalf("first worker never entered provider start")
	}

	processed, err := second.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.False(processed, "the leased work must not be started by a second worker")
	release()
	var result runResult
	select {
	case result = <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("first worker never finished after release")
	}
	requirements.NoError(result.err)
	checks.True(result.processed)
	checks.Equal(int64(1), starts.Load())

	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("succeeded", attempts[0].State)
}

func TestWorkerSuppressesBeforeCredentialLookupOrProviderConstruction(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "suppressed-provider", nil)
	f.enqueue(t)
	normalized, err := personenrichment.NormalizeSuppressionIdentifier(
		personenrichment.SuppressionPublicProfileURL, []string{"https://profiles.example.test/worker-person"})
	requirements.NoError(err)
	digest := f.hasher.Digest(f.profile.ProviderNamespace, normalized.Class, normalized.NormalizationVersion, normalized.Value)
	requirements.NoError(f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{{
		ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
		NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID, Digest: digest.Digest,
		Reason: store.PersonEnrichmentSuppressionOptOut, Actor: "test",
	}}))
	var credentials, factories, starts atomic.Int64
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			factories.Add(1)
			return &functionProvider{start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
				starts.Add(1)
				return personenrichment.Attempt{}, nil
			}, poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
				return personenrichment.Result{}, nil
			}}, nil
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { credentials.Add(1); return "test-key", true },
	)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Zero(credentials.Load())
	checks.Zero(factories.Load())
	checks.Zero(starts.Load())
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("suppressed", attempts[0].State)
}

type consentFailureChecker struct {
	personenrichment.ConsentChecker

	err error
}

func (c consentFailureChecker) HasActivePersonEnrichmentConsent(context.Context, string) (bool, error) {
	return false, c.err
}

type suppressionFailureChecker struct {
	personenrichment.SuppressionChecker

	keyIDsErr error
	lookupErr error
}

func (s suppressionFailureChecker) ListPersonEnrichmentSuppressionKeyIDsContext(
	ctx context.Context,
) ([]string, error) {
	if s.keyIDsErr != nil {
		return nil, s.keyIDsErr
	}
	return s.SuppressionChecker.ListPersonEnrichmentSuppressionKeyIDsContext(ctx)
}

func (s suppressionFailureChecker) HasPersonEnrichmentSuppressionContext(
	ctx context.Context, lookup personenrichment.SuppressionLookup,
) (bool, error) {
	if s.lookupErr != nil {
		return false, s.lookupErr
	}
	return s.SuppressionChecker.HasPersonEnrichmentSuppressionContext(ctx, lookup)
}

func TestWorkerRetriesWrappedConsentAndSuppressionInfrastructureFailures(t *testing.T) {
	tests := []struct {
		name string
		gate func(*workerFixture, error) *personenrichment.EgressGate
	}{
		{
			name: "consent lookup failure",
			gate: func(f *workerFixture, injected error) *personenrichment.EgressGate {
				gate, err := personenrichment.NewEgressGate(
					consentFailureChecker{ConsentChecker: f.store, err: injected}, f.store, f.hasher,
					func(string) (string, bool) { return "unexpected-credential", true })
				require.NoError(t, err)
				return gate
			},
		},
		{
			name: "suppression key lookup failure",
			gate: func(f *workerFixture, injected error) *personenrichment.EgressGate {
				gate, err := personenrichment.NewEgressGate(
					f.store, suppressionFailureChecker{SuppressionChecker: f.store, keyIDsErr: injected},
					f.hasher, func(string) (string, bool) { return "unexpected-credential", true })
				require.NoError(t, err)
				return gate
			},
		},
		{
			name: "suppression digest lookup failure",
			gate: func(f *workerFixture, injected error) *personenrichment.EgressGate {
				gate, err := personenrichment.NewEgressGate(
					f.store, suppressionFailureChecker{SuppressionChecker: f.store, lookupErr: injected},
					f.hasher, func(string) (string, bool) { return "unexpected-credential", true })
				require.NoError(t, err)
				return gate
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newWorkerFixture(t, "gate-infrastructure", nil)
			f.enqueue(t)
			injected := errors.New("synthetic policy store unavailable")
			var factory atomic.Int64
			worker, err := personenrichment.NewWorker(
				f.store, f.store, *tt.gate(f, injected),
				map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
					factory.Add(1)
					return nil, errors.New("provider must not be constructed")
				}}, f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config}))
			requirements.NoError(err)

			processed, err := worker.RunOnce(t.Context(), f.run.ID)
			requirements.NoError(err)
			checks.True(processed)
			checks.Zero(factory.Load())
			workRows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
				PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
			})
			requirements.NoError(err)
			requirements.Len(workRows, 1)
			checks.Nil(workRows[0].ActiveAttemptID)
			attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
				PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
			})
			requirements.NoError(err)
			checks.Empty(attempts)
		})
	}
}

type requestInputOverrideStore struct {
	personenrichment.WorkStore

	mutate  func(*personenrichment.RequestInput)
	loadErr error
}

type beginAttemptErrorStore struct {
	personenrichment.WorkStore

	err      error
	releases atomic.Int64
}

type workerHookStore struct {
	personenrichment.WorkStore

	afterBegin     func() error
	afterAuthorize func() error
}

func (s *workerHookStore) AuthorizeAttemptDispatch(
	ctx context.Context, token personenrichment.LeaseToken,
) error {
	if err := s.WorkStore.AuthorizeAttemptDispatch(ctx, token); err != nil {
		return err
	}
	if s.afterAuthorize != nil {
		return s.afterAuthorize()
	}
	return nil
}

func (s *workerHookStore) BeginAttempt(
	ctx context.Context, token personenrichment.LeaseToken, start personenrichment.AttemptStart,
) (*personenrichment.DurableAttempt, bool, error) {
	attempt, created, err := s.WorkStore.BeginAttempt(ctx, token, start)
	if err != nil {
		return attempt, created, err
	}
	if s.afterBegin != nil {
		if err := s.afterBegin(); err != nil {
			return nil, false, err
		}
	}
	return attempt, created, nil
}

func TestWorkerDoesNotStartProviderAfterAuthorityRemovalCommits(t *testing.T) {
	for _, removal := range []string{"tracking", "suppression", "provider-id-suppression"} {
		t.Run(removal, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newWorkerFixture(t, "authority-removal-"+removal, nil)
			providerPersonID := "known-provider-person-id"
			if removal == "provider-id-suppression" {
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
					INSERT INTO person_enrichment_provider_identities
						(person_id, provider_namespace, provider_person_id, confidence, verified_at)
					VALUES (?, ?, ?, ?, ?)`), f.person.ID, f.profile.ProviderNamespace,
					providerPersonID, 1000, f.now)
				require.NoError(err)
			}
			f.enqueue(t)
			remove := func() error {
				_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, false)
				return err
			}
			if removal == "suppression" {
				normalized, err := personenrichment.NormalizeSuppressionIdentifier(
					personenrichment.SuppressionPublicProfileURL,
					[]string{"https://profiles.example.test/worker-person"})
				require.NoError(err)
				digest := f.hasher.Digest(f.profile.ProviderNamespace, normalized.Class,
					normalized.NormalizationVersion, normalized.Value)
				remove = func() error {
					return f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(),
						[]store.PersonEnrichmentSuppressionInput{{
							ProviderNamespace:    digest.ProviderNamespace,
							IdentifierClass:      digest.IdentifierClass,
							NormalizationVersion: digest.NormalizationVersion,
							KeyID:                digest.KeyID, Digest: digest.Digest,
							Reason: store.PersonEnrichmentSuppressionOptOut, Actor: "privacy-test",
						}})
				}
			}
			if removal == "provider-id-suppression" {
				normalized, err := personenrichment.NormalizeSuppressionIdentifier(
					personenrichment.SuppressionProviderPersonID, []string{providerPersonID})
				require.NoError(err)
				digest := f.hasher.Digest(f.profile.ProviderNamespace, normalized.Class,
					normalized.NormalizationVersion, normalized.Value)
				remove = func() error {
					return f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(),
						[]store.PersonEnrichmentSuppressionInput{{
							ProviderNamespace:    digest.ProviderNamespace,
							IdentifierClass:      digest.IdentifierClass,
							NormalizationVersion: digest.NormalizationVersion,
							KeyID:                digest.KeyID, Digest: digest.Digest,
							Reason: store.PersonEnrichmentSuppressionOptOut, Actor: "privacy-test",
						}})
				}
			}
			work := &workerHookStore{WorkStore: f.store, afterBegin: remove}
			var starts atomic.Int64
			factory := func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
				return &functionProvider{
					start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
						starts.Add(1)
						return personenrichment.Attempt{}, errors.New("provider must not start")
					},
					poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
						return personenrichment.Result{}, errors.New("unexpected poll")
					},
				}, nil
			}
			worker, err := personenrichment.NewWorker(
				work, f.store, f.gate(t, func(string) (string, bool) { return "test-key", true }),
				map[string]personenrichment.ProviderFactory{f.config.Name: factory},
				f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config}),
			)
			require.NoError(err)

			processed, err := worker.RunOnce(t.Context(), f.run.ID)
			assert.True(processed)
			require.ErrorIs(err, store.ErrStaleLease)
			assert.Zero(starts.Load())
		})
	}
}

func TestWorkerAuthorizedDispatchPreventsConcurrentPersonDeletion(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newWorkerFixture(t, "dispatch-delete-race", nil)
	f.enqueue(t)
	authorized := make(chan struct{})
	resume := make(chan struct{})
	work := &workerHookStore{
		WorkStore: f.store,
		afterAuthorize: func() error {
			close(authorized)
			<-resume
			return nil
		},
	}
	var starts atomic.Int64
	provider := &functionProvider{
		start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
			starts.Add(1)
			return personenrichment.Attempt{}, errors.New("synthetic uncertain provider start")
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	worker, err := personenrichment.NewWorker(
		work, f.store, f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}}, f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config}),
	)
	requirements.NoError(err)
	done := make(chan error, 1)
	go func() {
		_, runErr := worker.RunOnce(t.Context(), f.run.ID)
		done <- runErr
	}()
	<-authorized
	deleteErr := f.store.DeletePersonContext(t.Context(), f.person.ID, f.person.Revision)
	close(resume)
	runErr := <-done
	requirements.ErrorIs(deleteErr, store.ErrPersonEnrichmentDispatchInProgress)
	requirements.NoError(runErr)
	checks.Equal(int64(1), starts.Load())
	person, err := f.store.GetPersonContext(t.Context(), f.person.ID)
	requirements.NoError(err)
	requirements.NoError(f.store.DeletePersonContext(t.Context(), person.ID, person.Revision))
}

func (s *beginAttemptErrorStore) BeginAttempt(
	ctx context.Context, token personenrichment.LeaseToken, start personenrichment.AttemptStart,
) (*personenrichment.DurableAttempt, bool, error) {
	if s.err != nil {
		return nil, false, s.err
	}
	return s.WorkStore.BeginAttempt(ctx, token, start)
}

func (s *beginAttemptErrorStore) ReleaseWork(
	ctx context.Context, token personenrichment.LeaseToken, release personenrichment.WorkRelease,
) error {
	s.releases.Add(1)
	return s.WorkStore.ReleaseWork(ctx, token, release)
}

func TestWorkerRetriesSameRevisionInLaterRunAfterBudgetExhaustion(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "budget-next-run", nil)
	f.enqueue(t)
	work := &beginAttemptErrorStore{WorkStore: f.store, err: personenrichment.ErrRequestBudgetExceeded}
	var starts atomic.Int64
	provider := &functionProvider{
		start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
			starts.Add(1)
			result := workerResult(t, request, f.target, "budget-next-run-request", "", false, "", personenrichment.Cost{})
			return personenrichment.Attempt{
				State: personenrichment.AttemptComplete, RequestID: result.RequestID,
				StartedAt: time.Now().UTC(), AdapterVersion: result.AdapterVersion,
				SchemaVersion: result.SchemaVersion, ProgramFingerprint: workerProgramFingerprint(t, false, ""),
				Result: &result,
			}, nil
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	options := f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config})
	options.Clock = func() time.Time { return f.now }
	worker, err := personenrichment.NewWorker(
		work, f.store, f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: func(
			personenrichment.ProviderConfig, string,
		) (personenrichment.Provider, error) {
			return provider, nil
		}}, options,
	)
	requirements.NoError(err)

	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Zero(starts.Load())
	requirements.NoError(f.store.CompleteRun(t.Context(), f.run.ID, personenrichment.RunCompletion{
		State: "succeeded", CompletedAt: f.now,
	}))
	workRows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(workRows, 1)
	requirements.Nil(workRows[0].RunID)

	work.err = nil
	f.now = workRows[0].DueAt.Add(time.Second)
	nextRun, created, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "scheduled", RequestedBy: "worker:budget-next-run:retry", RequestedAt: f.now,
	})
	requirements.NoError(err)
	requirements.True(created)
	processed, err = worker.RunOnce(t.Context(), nextRun.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Equal(int64(1), starts.Load())
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: nextRun.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("succeeded", attempts[0].State)
}

func TestWorkerDoesNotConsumeWorkOnBeginAttemptInfrastructureFailure(t *testing.T) {
	tests := []struct {
		name         string
		injected     error
		wantError    bool
		wantReleases int64
		wantState    string
	}{
		{name: "infrastructure", injected: errors.New("synthetic database failure"), wantError: true},
		{name: "request budget policy", injected: personenrichment.ErrRequestBudgetExceeded,
			wantReleases: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newWorkerFixture(t, "begin-attempt-error", nil)
			f.enqueue(t)
			work := &beginAttemptErrorStore{WorkStore: f.store, err: test.injected}
			var starts atomic.Int64
			worker, err := personenrichment.NewWorker(
				work, f.store, f.gate(t, func(string) (string, bool) { return "test-key", true }),
				map[string]personenrichment.ProviderFactory{f.config.Name: func(
					personenrichment.ProviderConfig, string,
				) (personenrichment.Provider, error) {
					return &functionProvider{
						start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
							starts.Add(1)
							return personenrichment.Attempt{}, errors.New("unexpected provider start")
						},
						poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
							return personenrichment.Result{}, errors.New("unexpected provider poll")
						},
					}, nil
				}},
				f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config}),
			)
			requirements.NoError(err)

			processed, err := worker.RunOnce(t.Context(), f.run.ID)
			checks.True(processed)
			if test.wantError {
				requirements.ErrorIs(err, test.injected)
			} else {
				requirements.NoError(err)
			}
			checks.Equal(test.wantReleases, work.releases.Load())
			checks.Zero(starts.Load())
			attempts, listErr := f.store.ListPersonEnrichmentAttemptsContext(
				t.Context(), store.PersonEnrichmentAttemptFilter{PersonID: f.person.ID, RunID: f.run.ID, Limit: 10},
			)
			requirements.NoError(listErr)
			if test.wantState == "" {
				checks.Empty(attempts)
				if errors.Is(test.injected, personenrichment.ErrRequestBudgetExceeded) {
					workRows, workErr := f.store.ListPersonEnrichmentWorkContext(t.Context(),
						store.PersonEnrichmentWorkFilter{PersonID: f.person.ID,
							ProfileFingerprint: f.profile.Fingerprint, Limit: 10})
					requirements.NoError(workErr)
					requirements.Len(workRows, 1)
					checks.Nil(workRows[0].ActiveAttemptID)
				}
				return
			}
			requirements.Len(attempts, 1)
			checks.Equal(test.wantState, attempts[0].State)
		})
	}
}

func (s requestInputOverrideStore) LoadRequestInput(
	ctx context.Context, lease personenrichment.WorkLease,
) (personenrichment.RequestInput, error) {
	if s.loadErr != nil {
		return personenrichment.RequestInput{}, s.loadErr
	}
	input, err := s.WorkStore.LoadRequestInput(ctx, lease)
	if err == nil && s.mutate != nil {
		s.mutate(&input)
	}
	return input, err
}

func TestWorkerRejectsRuntimeProfileAndCatalogDriftBeforeEgress(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*workerFixture, *personenrichment.WorkerOptions) personenrichment.WorkStore
	}{
		{
			name: "missing runtime config",
			configure: func(f *workerFixture, options *personenrichment.WorkerOptions) personenrichment.WorkStore {
				options.ProviderConfigs = map[string]personenrichment.ProviderConfig{}
				return f.store
			},
		},
		{
			name: "mismatched runtime profile",
			configure: func(f *workerFixture, options *personenrichment.WorkerOptions) personenrichment.WorkStore {
				mismatched := f.config
				mismatched.Endpoint = "https://different.example.test/search"
				options.ProviderConfigs = map[string]personenrichment.ProviderConfig{f.config.Name: mismatched}
				return f.store
			},
		},
		{
			name: "current catalog drift",
			configure: func(f *workerFixture, options *personenrichment.WorkerOptions) personenrichment.WorkStore {
				return requestInputOverrideStore{WorkStore: f.store, mutate: func(input *personenrichment.RequestInput) {
					for i := range input.Catalog.Targets {
						if input.Catalog.Targets[i].Key == f.target.Key {
							input.Catalog.Targets[i].Description += " changed"
							input.Catalog.Targets[i].Revision = "changed-revision"
						}
					}
				}}
			},
		},
		{
			name: "eligible identity disappeared",
			configure: func(f *workerFixture, _ *personenrichment.WorkerOptions) personenrichment.WorkStore {
				return requestInputOverrideStore{WorkStore: f.store, mutate: func(input *personenrichment.RequestInput) {
					input.Names = nil
					input.Emails = nil
					input.Phones = nil
					input.CurrentCompanies = nil
					input.PublicProfileURLs = nil
				}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newWorkerFixture(t, "drift-provider", nil)
			f.enqueue(t)
			var credential, factory, network atomic.Int64
			factories := map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
				factory.Add(1)
				return &functionProvider{start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
					network.Add(1)
					return personenrichment.Attempt{}, nil
				}, poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
					network.Add(1)
					return personenrichment.Result{}, nil
				}}, nil
			}}
			options := f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config})
			work := tt.configure(f, &options)
			worker, err := personenrichment.NewWorker(work, f.store, f.gate(t, func(string) (string, bool) {
				credential.Add(1)
				return "test-key", true
			}), factories, options)
			requirements.NoError(err)
			processed, err := worker.RunOnce(t.Context(), f.run.ID)
			requirements.NoError(err)
			checks.True(processed)
			checks.Zero(credential.Load())
			checks.Zero(factory.Load())
			checks.Zero(network.Load())
			workRows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
				PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
			})
			requirements.NoError(err)
			checks.Empty(workRows)
			requirements.NoError(f.store.CompleteRun(t.Context(), f.run.ID, personenrichment.RunCompletion{
				State: "succeeded", CompletedAt: time.Now().UTC(),
			}))
		})
	}
}

func TestWorkerTerminalizesActiveAttemptWhenRuntimeProfileDrifts(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "active-profile-drift", nil)
	f.enqueue(t)
	const schemaHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var polls atomic.Int64
	factory := personenrichment.ProviderFactory(func(
		personenrichment.ProviderConfig, string,
	) (personenrichment.Provider, error) {
		return &functionProvider{
			start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
				return personenrichment.Attempt{
					State: personenrichment.AttemptPending, JobID: "profile-drift-job",
					PollAfter: time.Nanosecond, StartedAt: time.Now().UTC(),
					AdapterVersion: "test-adapter-v1", SchemaVersion: "test-wire-v1",
					GeneratedSchema: true, GeneratedSchemaHash: schemaHash,
					ProgramFingerprint: workerProgramFingerprint(t, true, schemaHash), Targets: request.Targets,
				}, nil
			},
			poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
				polls.Add(1)
				return personenrichment.Result{}, errors.New("drifted attempt must not be polled")
			},
		}, nil
	})

	first := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: factory},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { return "test-key", true },
	)
	processed, err := first.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	requirements.True(processed)

	options := f.options(map[string]personenrichment.ProviderConfig{})
	second, err := personenrichment.NewWorker(
		f.store, f.store, f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: factory}, options,
	)
	requirements.NoError(err)
	processed, err = second.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Zero(polls.Load())

	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("terminal", attempts[0].State)
	checks.Equal(string(personenrichment.FailurePolicy), *attempts[0].FailureClass)
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	checks.Empty(work)
}

func TestWorkerRejectsModeIncompatibleIdentityBeforeEgress(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "incompatible-identity", func(config *personenrichment.ProviderConfig) {
		config.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierEmail}
	})
	f.enqueue(t)
	var credential, factory, network atomic.Int64
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			factory.Add(1)
			return &functionProvider{start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
				network.Add(1)
				return personenrichment.Attempt{}, nil
			}}, nil
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { credential.Add(1); return "test-key", true },
	)

	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Zero(credential.Load())
	checks.Zero(factory.Load())
	checks.Zero(network.Load())
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	checks.Empty(attempts)
}

func TestWorkerRetainsWorkForTransientRequestLoadFailure(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "transient-request-load", nil)
	f.enqueue(t)
	transientErr := errors.New("synthetic request store unavailable")
	work := requestInputOverrideStore{WorkStore: f.store, loadErr: transientErr}
	var credential, factory atomic.Int64
	worker, err := personenrichment.NewWorker(
		work, f.store, f.gate(t, func(string) (string, bool) {
			credential.Add(1)
			return "test-key", true
		}), map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			factory.Add(1)
			return nil, errors.New("provider must not be constructed")
		}}, f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config}))
	requirements.NoError(err)

	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Zero(credential.Load())
	checks.Zero(factory.Load())
	workRows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(workRows, 1)
	checks.Nil(workRows[0].ActiveAttemptID)
	checks.True(workRows[0].DueAt.After(time.Now().UTC().Add(-time.Second)))
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	checks.Empty(attempts)
}

type renewFailStore struct {
	personenrichment.WorkStore

	providerStarted <-chan struct{}
	renewed         chan struct{}
	once            sync.Once
}

func (s *renewFailStore) RenewLease(ctx context.Context, _ personenrichment.LeaseToken, _ time.Time) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.providerStarted:
	}
	s.once.Do(func() { close(s.renewed) })
	return errors.New("synthetic lease loss")
}

func TestWorkerLeaseLossCancelsInFlightProvider(t *testing.T) {
	requirements := require.New(t)
	f := newWorkerFixture(t, "lease-loss", nil)
	f.enqueue(t)
	providerStarted := make(chan struct{})
	wrapped := &renewFailStore{
		WorkStore: f.store, providerStarted: providerStarted, renewed: make(chan struct{}),
	}
	cancelled := make(chan struct{})
	provider := &functionProvider{
		start: func(ctx context.Context, _ personenrichment.Request) (personenrichment.Attempt, error) {
			close(providerStarted)
			<-ctx.Done()
			close(cancelled)
			return personenrichment.Attempt{}, ctx.Err()
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	options := f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config})
	options.LeaseDuration = 60 * time.Millisecond
	options.RenewEvery = 10 * time.Millisecond
	worker, err := personenrichment.NewWorker(
		wrapped, f.store, f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}}, options)
	requirements.NoError(err)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	assert.True(t, processed)
	requirements.ErrorContains(err, "lease")
	select {
	case <-wrapped.renewed:
	case <-time.After(time.Second):
		requirements.Fail("lease was not renewed")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		requirements.Fail("provider context was not cancelled")
	}
}

type renewalTrackingStore struct {
	personenrichment.WorkStore

	renewed chan personenrichment.LeaseToken
}

type bindingRenewalRaceStore struct {
	personenrichment.WorkStore

	renewStarted   chan struct{}
	bound          chan struct{}
	renewedCurrent chan personenrichment.LeaseToken
	renewOnce      sync.Once
	boundOnce      sync.Once
}

func (s *bindingRenewalRaceStore) LoadProviderProfile(
	ctx context.Context, fingerprint string,
) (personenrichment.ProviderProfile, error) {
	profile, err := s.WorkStore.LoadProviderProfile(ctx, fingerprint)
	if err != nil {
		return personenrichment.ProviderProfile{}, err
	}
	select {
	case <-s.renewStarted:
		return profile, nil
	case <-ctx.Done():
		return personenrichment.ProviderProfile{}, ctx.Err()
	}
}

func (s *bindingRenewalRaceStore) RenewLease(
	ctx context.Context, token personenrichment.LeaseToken, until time.Time,
) error {
	if token.AttemptID == 0 {
		s.renewOnce.Do(func() { close(s.renewStarted) })
		select {
		case <-s.bound:
			return errors.New("synthetic stale pre-attempt renewal")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := s.WorkStore.RenewLease(ctx, token, until); err != nil {
		return err
	}
	select {
	case s.renewedCurrent <- token:
	default:
	}
	return nil
}

func (s *bindingRenewalRaceStore) BeginAttempt(
	ctx context.Context, token personenrichment.LeaseToken, start personenrichment.AttemptStart,
) (*personenrichment.DurableAttempt, bool, error) {
	attempt, created, err := s.WorkStore.BeginAttempt(ctx, token, start)
	if err == nil {
		s.boundOnce.Do(func() { close(s.bound) })
	}
	return attempt, created, err
}

func TestWorkerRetriesOldTokenRenewalAfterAttemptBinding(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "renew-binding-race", nil)
	f.enqueue(t)
	wrapped := &bindingRenewalRaceStore{
		WorkStore: f.store, renewStarted: make(chan struct{}), bound: make(chan struct{}),
		renewedCurrent: make(chan personenrichment.LeaseToken, 1),
	}
	provider := &functionProvider{
		start: func(ctx context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
			select {
			case token := <-wrapped.renewedCurrent:
				assert.Positive(t, token.AttemptID)
			case <-ctx.Done():
				return personenrichment.Attempt{}, ctx.Err()
			}
			result := workerResult(t, request, f.target, "binding-race-request", "", false, "", personenrichment.Cost{})
			return personenrichment.Attempt{
				State: personenrichment.AttemptComplete, RequestID: result.RequestID,
				AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
				ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
			}, nil
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	options := f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config})
	options.LeaseDuration = 5 * time.Second
	options.RenewEvery = 250 * time.Millisecond
	worker, err := personenrichment.NewWorker(
		wrapped, f.store, f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}}, options)
	requirements.NoError(err)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	processed, err := worker.RunOnce(ctx, f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("succeeded", attempts[0].State)
}

func (s *renewalTrackingStore) RenewLease(
	ctx context.Context, token personenrichment.LeaseToken, until time.Time,
) error {
	if err := s.WorkStore.RenewLease(ctx, token, until); err != nil {
		return err
	}
	if token.AttemptID > 0 {
		select {
		case s.renewed <- token:
		default:
		}
	}
	return nil
}

func TestWorkerPublishesAttemptTokenToLeaseRenewalLoop(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "renew-after-begin", nil)
	f.enqueue(t)
	wrapped := &renewalTrackingStore{
		WorkStore: f.store, renewed: make(chan personenrichment.LeaseToken, 8),
	}
	provider := &functionProvider{
		start: func(ctx context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
			for range 3 {
				select {
				case token := <-wrapped.renewed:
					assert.Positive(t, token.AttemptID)
				case <-ctx.Done():
					return personenrichment.Attempt{}, ctx.Err()
				}
			}
			result := workerResult(t, request, f.target, "renewed-request", "", false, "", personenrichment.Cost{})
			return personenrichment.Attempt{
				State: personenrichment.AttemptComplete, RequestID: result.RequestID,
				AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
				ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
			}, nil
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	options := f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config})
	options.LeaseDuration = 5 * time.Second
	options.RenewEvery = 250 * time.Millisecond
	worker, err := personenrichment.NewWorker(
		wrapped, f.store, f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}}, options)
	requirements.NoError(err)

	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("succeeded", attempts[0].State)
}

func TestWorkerGuaranteedCostReservesBeforeStartAndReconcilesDownward(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "bounded-provider", func(cfg *personenrichment.ProviderConfig) {
		cfg.MaxCostUSDMicrosPerPersonPerDay = 1_000
		cfg.MaxCostUSDMicrosPerRun = 1_000
		cfg.MaxCostUSDMicrosPerDay = 1_000
	})
	f.enqueue(t)
	var reservedAtStart atomic.Int64
	provider := &guaranteedFunctionProvider{
		functionProvider: &functionProvider{
			start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
				counters, err := f.store.GetPersonEnrichmentRunCountersContext(t.Context(), f.run.ID)
				require.NoError(t, err)
				reservedAtStart.Store(counters.CostReservedUSDMicros)
				result := workerResult(t, request, f.target, "bounded-request", "", false, "", personenrichment.Cost{
					Currency: "USD", AmountMicros: 600,
				})
				return personenrichment.Attempt{
					State: personenrichment.AttemptComplete, RequestID: result.RequestID,
					AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
					ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
				}, nil
			},
			poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
				return personenrichment.Result{}, errors.New("unexpected poll")
			},
		},
		bound: func(context.Context, personenrichment.Request) (personenrichment.Cost, error) {
			return personenrichment.Cost{Currency: "USD", AmountMicros: 900}, nil
		},
	}
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { return "test-key", true },
	)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Equal(int64(900), reservedAtStart.Load())
	counters, err := f.store.GetPersonEnrichmentRunCountersContext(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.Equal(int64(0), counters.CostReservedUSDMicros)
	checks.Equal(int64(600), counters.CostChargedUSDMicros)
}

func TestWorkerRejectsInvalidGuaranteedCostBeforeStart(t *testing.T) {
	tests := []struct {
		name  string
		bound personenrichment.Cost
	}{
		{name: "missing", bound: personenrichment.Cost{}},
		{name: "estimated", bound: personenrichment.Cost{Currency: "USD", AmountMicros: 900, Estimated: true}},
		{name: "non USD", bound: personenrichment.Cost{Currency: "EUR", AmountMicros: 900}},
		{name: "negative", bound: personenrichment.Cost{Currency: "USD", AmountMicros: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newWorkerFixture(t, "invalid-bound", func(cfg *personenrichment.ProviderConfig) {
				cfg.MaxCostUSDMicrosPerRun = 1_000
			})
			f.enqueue(t)
			var starts atomic.Int64
			provider := &guaranteedFunctionProvider{
				functionProvider: &functionProvider{
					start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
						starts.Add(1)
						return personenrichment.Attempt{}, nil
					},
					poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
						return personenrichment.Result{}, nil
					},
				},
				bound: func(context.Context, personenrichment.Request) (personenrichment.Cost, error) { return tt.bound, nil },
			}
			worker := f.newWorker(t,
				map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
					return provider, nil
				}},
				map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
				func(string) (string, bool) { return "test-key", true },
			)
			processed, err := worker.RunOnce(t.Context(), f.run.ID)
			requirements.NoError(err)
			checks.True(processed)
			checks.Zero(starts.Load())
			attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
				PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
			})
			requirements.NoError(err)
			requirements.Len(attempts, 1)
			checks.Equal("terminal", attempts[0].State)
		})
	}
}

func TestWorkerRejectsProviderWithoutGuaranteedBoundUnderHardCost(t *testing.T) {
	f := newWorkerFixture(t, "missing-guarantee", func(cfg *personenrichment.ProviderConfig) {
		cfg.MaxCostUSDMicrosPerRun = 1_000
	})
	f.enqueue(t)
	var starts atomic.Int64
	provider := &functionProvider{
		start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
			starts.Add(1)
			return personenrichment.Attempt{}, nil
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, nil
		},
	}
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { return "test-key", true },
	)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	require.NoError(t, err)
	assert.True(t, processed)
	assert.Zero(t, starts.Load())
}

func TestWorkerMissingCredentialSchedulesRetryWithoutBudgetReservation(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "missing-credential", nil)
	f.enqueue(t)
	var factories atomic.Int64
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			factories.Add(1)
			return nil, errors.New("must not construct")
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { return "", false },
	)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Zero(factories.Load())
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(work, 1)
	checks.True(work[0].DueAt.After(time.Now().UTC().Add(-time.Second)))
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	checks.Empty(attempts)
}

func TestWorkerInvalidOutputWritesNoResultState(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "invalid-output", nil)
	f.enqueue(t)
	provider := &functionProvider{
		start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
			result := workerResult(t, request, f.target, "invalid-request", "", false, "", personenrichment.Cost{})
			result.IdentityConfidence = 1001
			return personenrichment.Attempt{
				State: personenrichment.AttemptComplete, RequestID: result.RequestID,
				AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
				ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
			}, nil
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { return "test-key", true },
	)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("terminal", attempts[0].State)
	var generations int64
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM person_fact_generations").Scan(&generations))
	checks.Zero(generations)
}

func TestWorkerUncertainStartIsNeverReplayed(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "uncertain-start", nil)
	f.enqueue(t)
	var starts atomic.Int64
	provider := &functionProvider{
		start: func(context.Context, personenrichment.Request) (personenrichment.Attempt, error) {
			starts.Add(1)
			return personenrichment.Attempt{}, errors.New("synthetic transport ambiguity")
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { return "test-key", true },
	)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	processed, err = worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.False(processed)
	checks.Equal(int64(1), starts.Load())
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("uncertain_start", attempts[0].State)
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	checks.Empty(work)
	requirements.NoError(f.store.CompleteRun(t.Context(), f.run.ID, personenrichment.RunCompletion{
		CompletedAt: f.now.Add(time.Minute),
	}))
	run, err := f.store.GetPersonEnrichmentRunContext(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.Equal("failed", run.State)
}

type noOpRenewStore struct{ personenrichment.WorkStore }

func (s noOpRenewStore) RenewLease(context.Context, personenrichment.LeaseToken, time.Time) error {
	return nil
}

func TestWorkerRejectsStaleCommitAfterLeaseReclaim(t *testing.T) {
	requirements := require.New(t)
	f := newWorkerFixture(t, "stale-commit", nil)
	f.enqueue(t)
	started := make(chan struct{})
	release := make(chan struct{})
	provider := &functionProvider{
		start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
			close(started)
			<-release
			result := workerResult(t, request, f.target, "stale-request", "", false, "", personenrichment.Cost{})
			return personenrichment.Attempt{
				State: personenrichment.AttemptComplete, RequestID: result.RequestID,
				AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
				ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
			}, nil
		},
		poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
			return personenrichment.Result{}, errors.New("unexpected poll")
		},
	}
	options := f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config})
	options.LeaseDuration = 60 * time.Millisecond
	options.RenewEvery = 10 * time.Millisecond
	worker, err := personenrichment.NewWorker(
		noOpRenewStore{WorkStore: f.store}, f.store,
		f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}}, options)
	requirements.NoError(err)
	done := make(chan error, 1)
	go func() {
		_, runErr := worker.RunOnce(t.Context(), f.run.ID)
		done <- runErr
	}()
	<-started
	time.Sleep(80 * time.Millisecond)
	reclaimed, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: f.run.ID, Owner: "replacement-worker", ProviderName: f.config.Name,
		Now: time.Now().UTC(), LeaseDuration: time.Minute,
	})
	requirements.NoError(err)
	requirements.NotNil(reclaimed)
	close(release)
	requirements.ErrorIs(<-done, store.ErrStaleLease)
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	assert.Equal(t, "uncertain_start", attempts[0].State)
}

func TestWorkerGuaranteedCostRejectsConcurrentCapBeforeNetwork(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "concurrent-cost", func(cfg *personenrichment.ProviderConfig) {
		cfg.MaxCostUSDMicrosPerRun = 1_000
	})
	f.enqueue(t)
	participantID, err := f.store.EnsureParticipant("second-worker@example.test", "Second Worker", "example.test")
	requirements.NoError(err)
	secondPerson, _, err := f.store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	_, err = f.store.SetPersonTrackingContext(t.Context(), secondPerson.ID, true)
	requirements.NoError(err)
	requirements.NoError(f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: secondPerson.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"}, DueAt: f.now,
	}))
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var starts atomic.Int64
	provider := &guaranteedFunctionProvider{
		functionProvider: &functionProvider{
			start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
				if starts.Add(1) == 1 {
					close(firstStarted)
					<-releaseFirst
				}
				result := workerResult(t, request, f.target, "concurrent-request", "", false, "", personenrichment.Cost{
					Currency: "USD", AmountMicros: 600,
				})
				return personenrichment.Attempt{
					State: personenrichment.AttemptComplete, RequestID: result.RequestID,
					AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
					ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
				}, nil
			},
			poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
				return personenrichment.Result{}, errors.New("unexpected poll")
			},
		},
		bound: func(context.Context, personenrichment.Request) (personenrichment.Cost, error) {
			return personenrichment.Cost{Currency: "USD", AmountMicros: 900}, nil
		},
	}
	factory := func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) { return provider, nil }
	configs := map[string]personenrichment.ProviderConfig{f.config.Name: f.config}
	firstOptions := f.options(configs)
	firstOptions.Owner = "cost-worker-a"
	first, err := personenrichment.NewWorker(f.store, f.store,
		f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: factory}, firstOptions)
	requirements.NoError(err)
	secondOptions := f.options(configs)
	secondOptions.Owner = "cost-worker-b"
	second, err := personenrichment.NewWorker(f.store, f.store,
		f.gate(t, func(string) (string, bool) { return "test-key", true }),
		map[string]personenrichment.ProviderFactory{f.config.Name: factory}, secondOptions)
	requirements.NoError(err)
	firstDone := make(chan error, 1)
	go func() {
		_, runErr := first.RunOnce(t.Context(), f.run.ID)
		firstDone <- runErr
	}()
	<-firstStarted
	processed, err := second.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Equal(int64(1), starts.Load())
	close(releaseFirst)
	requirements.NoError(<-firstDone)
	counters, err := f.store.GetPersonEnrichmentRunCountersContext(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.Equal(int64(1), counters.RequestsStarted)
	checks.Equal(int64(600), counters.CostChargedUSDMicros)
}

func TestWorkerActualCostAboveGuaranteedBoundFailsClosed(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "above-bound", func(cfg *personenrichment.ProviderConfig) {
		cfg.MaxCostUSDMicrosPerRun = 1_000
	})
	f.enqueue(t)
	provider := &guaranteedFunctionProvider{
		functionProvider: &functionProvider{
			start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
				result := workerResult(t, request, f.target, "above-bound-request", "", false, "", personenrichment.Cost{
					Currency: "USD", AmountMicros: 901,
				})
				result.ProviderPersonIDs = []personenrichment.ProviderPersonID{{ID: "opaque-overrun-person", Confidence: 1000}}
				result.CanonicalPublicURLs = []string{"https://profiles.example.test/overrun-person"}
				result.Citations = []personenrichment.Citation{{
					Key: "overrun-citation", URL: "https://sources.example.test/overrun-person",
					Title: "Synthetic overrun source", Publisher: "Example Publisher",
					Excerpt: "Synthetic public evidence.", RetrievedAt: time.Now().UTC(),
				}}
				result.SourceAttempts = []personenrichment.SourceAttempt{{
					URL: "https://sources.example.test/overrun-person", Outcome: "cited", ObservedAt: time.Now().UTC(),
				}}
				return personenrichment.Attempt{
					State: personenrichment.AttemptComplete, RequestID: result.RequestID,
					AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
					ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
				}, nil
			},
			poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
				return personenrichment.Result{}, errors.New("unexpected poll")
			},
		},
		bound: func(context.Context, personenrichment.Request) (personenrichment.Cost, error) {
			return personenrichment.Cost{Currency: "USD", AmountMicros: 900}, nil
		},
	}
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { return "test-key", true },
	)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	checks.True(processed)
	requirements.ErrorIs(err, store.ErrProviderCostBoundExceeded)
	attempts, listErr := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
	})
	requirements.NoError(listErr)
	requirements.Len(attempts, 1)
	checks.Equal("terminal", attempts[0].State)
	checks.Nil(attempts[0].FactGenerationKey)
	checks.Nil(attempts[0].LeaseOwner)
	checks.Equal(int64(0), attempts[0].ReservedCostUSDMicros)
	requirements.NotNil(attempts[0].ActualCostUSDMicros)
	checks.Equal(int64(901), *attempts[0].ActualCostUSDMicros)
	counters, counterErr := f.store.GetPersonEnrichmentRunCountersContext(t.Context(), f.run.ID)
	requirements.NoError(counterErr)
	checks.Equal(int64(901), counters.CostChargedUSDMicros)
	for _, table := range []string{
		"person_enrichment_provider_identities", "person_enrichment_citations",
		"person_enrichment_attempt_citations", "person_enrichment_attempt_sources",
		"person_fact_generations", "person_fact_evidence", "person_fact_claims",
		"person_fact_decisions", "person_attribute_values",
	} {
		var count int64
		requirements.NoError(f.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
		checks.Zero(count, table)
	}
	work, workErr := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(workErr)
	checks.Empty(work)
	var startsDisabled bool
	var safeError string
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT starts_disabled, safe_error FROM person_enrichment_profile_accounting
		WHERE profile_fingerprint = ?`), f.profile.Fingerprint).Scan(&startsDisabled, &safeError))
	checks.True(startsDisabled)
	checks.Equal("provider actual charge exceeded guaranteed maximum", safeError)
	requirements.NoError(f.store.CompleteRun(t.Context(), f.run.ID, personenrichment.RunCompletion{
		State: "failed", CompletedAt: time.Now().UTC(), Failure: &personenrichment.SafeFailure{
			Class: personenrichment.FailureTerminal, Message: "provider cost bound exceeded",
		},
	}))
}

func TestWorkerRateLimitedStartRetriesSameReservedAttempt(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newWorkerFixture(t, "rate-limit-retry", func(cfg *personenrichment.ProviderConfig) {
		cfg.MaxCostUSDMicrosPerRun = 1_000
	})
	f.enqueue(t)
	var starts, bounds atomic.Int64
	provider := &guaranteedFunctionProvider{
		functionProvider: &functionProvider{
			start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
				if starts.Add(1) == 1 {
					return personenrichment.Attempt{}, &personenrichment.ProviderError{
						Class: personenrichment.FailureRateLimited, Status: 429, RetryAfter: "0",
					}
				}
				result := workerResult(t, request, f.target, "retry-request", "", false, "", personenrichment.Cost{
					Currency: "USD", AmountMicros: 600,
				})
				return personenrichment.Attempt{
					State: personenrichment.AttemptComplete, RequestID: result.RequestID,
					AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
					ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
				}, nil
			},
			poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
				return personenrichment.Result{}, errors.New("unexpected poll")
			},
		},
		bound: func(context.Context, personenrichment.Request) (personenrichment.Cost, error) {
			bounds.Add(1)
			return personenrichment.Cost{Currency: "USD", AmountMicros: 900}, nil
		},
	}
	worker := f.newWorker(t,
		map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
			return provider, nil
		}},
		map[string]personenrichment.ProviderConfig{f.config.Name: f.config},
		func(string) (string, bool) { return "test-key", true },
	)
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	requirements.True(processed)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_work
		SET due_at = ? WHERE person_id = ? AND profile_fingerprint = ?`),
		time.Now().UTC().Add(-time.Second), f.person.ID, f.profile.Fingerprint)
	requirements.NoError(err)
	processed, err = worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.True(processed)
	checks.Equal(int64(2), starts.Load())
	checks.Equal(int64(1), bounds.Load())
	counters, err := f.store.GetPersonEnrichmentRunCountersContext(t.Context(), f.run.ID)
	requirements.NoError(err)
	checks.Equal(int64(1), counters.RequestsStarted)
	checks.Equal(int64(600), counters.CostChargedUSDMicros)
}

type recordingSink struct {
	inner    personenrichment.ClaimSink
	mu       sync.Mutex
	statuses []personenrichment.ClaimOutcomeStatus
}

func (s *recordingSink) CommitEnrichmentClaims(
	ctx context.Context, commit personenrichment.ClaimCommit,
) (*personenrichment.ClaimOutcome, error) {
	outcome, err := s.inner.CommitEnrichmentClaims(ctx, commit)
	if outcome != nil {
		s.mu.Lock()
		s.statuses = append(s.statuses, outcome.Status)
		s.mu.Unlock()
	}
	return outcome, err
}

func TestWorkerCommitRechecksConsentAndReturnedSuppression(t *testing.T) {
	tests := []struct {
		name       string
		want       personenrichment.ClaimOutcomeStatus
		invalidate func(*workerFixture, personenrichment.Result)
	}{
		{
			name: "consent revoked", want: personenrichment.ClaimPolicyRejected,
			invalidate: func(f *workerFixture, _ personenrichment.Result) {
				changed, err := f.store.RevokePersonEnrichmentConsent(t.Context(), f.profile.Fingerprint, "test")
				require.NoError(t, err)
				require.True(t, changed)
			},
		},
		{
			name: "returned identity suppressed", want: personenrichment.ClaimSuppressed,
			invalidate: func(f *workerFixture, result personenrichment.Result) {
				normalized, err := personenrichment.NormalizeSuppressionIdentifier(
					personenrichment.SuppressionPublicProfileURL, []string{result.CanonicalPublicURLs[0]})
				require.NoError(t, err)
				digest := f.hasher.Digest(f.profile.ProviderNamespace, normalized.Class, normalized.NormalizationVersion, normalized.Value)
				require.NoError(t, f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{{
					ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
					NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID, Digest: digest.Digest,
					Reason: store.PersonEnrichmentSuppressionOptOut, Actor: "test",
				}}))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newWorkerFixture(t, "commit-recheck", func(cfg *personenrichment.ProviderConfig) {
				cfg.MaxCostUSDMicrosPerRun = 1_000
			})
			f.enqueue(t)
			started := make(chan personenrichment.Result, 1)
			release := make(chan struct{})
			provider := &guaranteedFunctionProvider{
				functionProvider: &functionProvider{
					start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
						result := workerResult(t, request, f.target, "blocked-request", "", false, "", personenrichment.Cost{
							Currency: "USD", AmountMicros: 600,
						})
						result.CanonicalPublicURLs = []string{"https://profiles.example.test/synthetic-person"}
						started <- result
						<-release
						return personenrichment.Attempt{
							State: personenrichment.AttemptComplete, RequestID: result.RequestID,
							AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
							ProgramFingerprint: workerProgramFingerprint(t, false, ""), Result: &result,
						}, nil
					},
					poll: func(context.Context, personenrichment.Attempt) (personenrichment.Result, error) {
						return personenrichment.Result{}, errors.New("unexpected poll")
					},
				},
				bound: func(context.Context, personenrichment.Request) (personenrichment.Cost, error) {
					return personenrichment.Cost{Currency: "USD", AmountMicros: 900}, nil
				},
			}
			sink := &recordingSink{inner: f.store}
			worker, err := personenrichment.NewWorker(
				f.store, sink, f.gate(t, func(string) (string, bool) { return "test-key", true }),
				map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
					return provider, nil
				}}, f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config}))
			requirements.NoError(err)
			done := make(chan error, 1)
			go func() {
				processed, runErr := worker.RunOnce(t.Context(), f.run.ID)
				if !processed && runErr == nil {
					runErr = errors.New("worker did not process blocked result")
				}
				done <- runErr
			}()
			result := <-started
			tt.invalidate(f, result)
			close(release)
			requirements.NoError(<-done)
			sink.mu.Lock()
			checks.Equal([]personenrichment.ClaimOutcomeStatus{tt.want}, sink.statuses)
			sink.mu.Unlock()
			attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
				PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
			})
			requirements.NoError(err)
			requirements.Len(attempts, 1)
			requirements.NotNil(attempts[0].ActualCostUSDMicros)
			checks.Equal(int64(600), *attempts[0].ActualCostUSDMicros)
			checks.Nil(attempts[0].FactGenerationKey)
			checks.Nil(attempts[0].LeaseOwner)
			checks.Contains([]string{"terminal", "suppressed"}, attempts[0].State)
			for _, table := range []string{
				"person_enrichment_provider_identities", "person_enrichment_citations",
				"person_fact_generations", "person_fact_claims", "person_fact_decisions",
			} {
				var count int64
				requirements.NoError(f.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
				checks.Zero(count, table)
			}
			work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
				PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
			})
			requirements.NoError(err)
			checks.Empty(work)
		})
	}
}

func TestWorkerAsyncPollCommitRechecksConsentAndReturnedSuppression(t *testing.T) {
	tests := []struct {
		name       string
		want       personenrichment.ClaimOutcomeStatus
		invalidate func(*workerFixture, personenrichment.Result)
	}{
		{
			name: "consent revoked", want: personenrichment.ClaimPolicyRejected,
			invalidate: func(f *workerFixture, _ personenrichment.Result) {
				changed, err := f.store.RevokePersonEnrichmentConsent(t.Context(), f.profile.Fingerprint, "test")
				require.NoError(t, err)
				require.True(t, changed)
			},
		},
		{
			name: "returned identity suppressed", want: personenrichment.ClaimSuppressed,
			invalidate: func(f *workerFixture, result personenrichment.Result) {
				normalized, err := personenrichment.NormalizeSuppressionIdentifier(
					personenrichment.SuppressionPublicProfileURL, []string{result.CanonicalPublicURLs[0]})
				require.NoError(t, err)
				digest := f.hasher.Digest(f.profile.ProviderNamespace, normalized.Class, normalized.NormalizationVersion, normalized.Value)
				require.NoError(t, f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{{
					ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
					NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID, Digest: digest.Digest,
					Reason: store.PersonEnrichmentSuppressionOptOut, Actor: "test",
				}}))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newWorkerFixture(t, "async-commit-recheck", func(cfg *personenrichment.ProviderConfig) {
				cfg.MaxCostUSDMicrosPerRun = 1_000
			})
			f.enqueue(t)
			requestReady := make(chan personenrichment.Request, 1)
			pollBlocked := make(chan personenrichment.Result, 1)
			releasePoll := make(chan struct{})
			const schemaHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
			provider := &guaranteedFunctionProvider{
				functionProvider: &functionProvider{
					start: func(_ context.Context, request personenrichment.Request) (personenrichment.Attempt, error) {
						requestReady <- request
						return personenrichment.Attempt{
							State: personenrichment.AttemptPending, JobID: "async-policy-job",
							PollAfter: time.Nanosecond, StartedAt: time.Now().UTC(),
							AdapterVersion: "test-adapter-v1", SchemaVersion: "test-wire-v1",
							GeneratedSchema: true, GeneratedSchemaHash: schemaHash,
							ProgramFingerprint: workerProgramFingerprint(t, true, schemaHash),
							Targets:            request.Targets,
						}, nil
					},
					poll: func(_ context.Context, attempt personenrichment.Attempt) (personenrichment.Result, error) {
						request := <-requestReady
						result := workerResult(t, request, f.target, "", attempt.JobID, true, schemaHash, personenrichment.Cost{
							Currency: "USD", AmountMicros: 600,
						})
						result.CanonicalPublicURLs = []string{"https://profiles.example.test/synthetic-async-person"}
						pollBlocked <- result
						<-releasePoll
						return result, nil
					},
				},
				bound: func(context.Context, personenrichment.Request) (personenrichment.Cost, error) {
					return personenrichment.Cost{Currency: "USD", AmountMicros: 900}, nil
				},
			}
			sink := &recordingSink{inner: f.store}
			workerNow := time.Now().UTC()
			options := f.options(map[string]personenrichment.ProviderConfig{f.config.Name: f.config})
			options.Clock = func() time.Time { return workerNow }
			worker, err := personenrichment.NewWorker(
				f.store, sink, f.gate(t, func(string) (string, bool) { return "test-key", true }),
				map[string]personenrichment.ProviderFactory{f.config.Name: func(personenrichment.ProviderConfig, string) (personenrichment.Provider, error) {
					return provider, nil
				}}, options)
			requirements.NoError(err)
			processed, err := worker.RunOnce(t.Context(), f.run.ID)
			requirements.NoError(err)
			requirements.True(processed)
			workerNow = time.Now().UTC().Add(time.Minute)
			done := make(chan error, 1)
			go func() {
				processed, runErr := worker.RunOnce(t.Context(), f.run.ID)
				if !processed && runErr == nil {
					runErr = errors.New("worker did not process blocked poll")
				}
				done <- runErr
			}()
			result := <-pollBlocked
			tt.invalidate(f, result)
			close(releasePoll)
			requirements.NoError(<-done)
			sink.mu.Lock()
			checks.Equal([]personenrichment.ClaimOutcomeStatus{tt.want}, sink.statuses)
			sink.mu.Unlock()
			attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
				PersonID: f.person.ID, RunID: f.run.ID, Limit: 10,
			})
			requirements.NoError(err)
			requirements.Len(attempts, 1)
			requirements.NotNil(attempts[0].ProviderJobID)
			checks.Equal("async-policy-job", *attempts[0].ProviderJobID)
			requirements.NotNil(attempts[0].ActualCostUSDMicros)
			checks.Equal(int64(600), *attempts[0].ActualCostUSDMicros)
			checks.Nil(attempts[0].FactGenerationKey)
			checks.Nil(attempts[0].LeaseOwner)
			checks.Contains([]string{"terminal", "suppressed"}, attempts[0].State)
			for _, table := range []string{
				"person_enrichment_provider_identities", "person_enrichment_citations",
				"person_fact_generations", "person_fact_claims", "person_fact_decisions",
			} {
				var count int64
				requirements.NoError(f.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
				checks.Zero(count, table)
			}
			work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
				PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
			})
			requirements.NoError(err)
			checks.Empty(work)
		})
	}
}
