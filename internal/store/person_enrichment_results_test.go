package store

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

type enrichmentResultFixture struct {
	store   *Store
	person  *Person
	profile personenrichment.ProviderProfile
	lease   *personenrichment.WorkLease
	attempt *personenrichment.DurableAttempt
	result  personenrichment.Result
	commit  personenrichment.ClaimCommit
	now     time.Time
}

type enrichmentClaimResult struct {
	outcome *personenrichment.ClaimOutcome
	err     error
}

func newEnrichmentResultFixture(t *testing.T) *enrichmentResultFixture {
	t.Helper()
	st, personID, targets := newPersonFactProjectionStore(t)
	_, err := st.AddPersonContactPointContext(t.Context(), personID, PersonContactPointInput{
		AddressKind: ContactAddressURL, OriginalValue: "https://profiles.example.test/result-person",
		Envelope: ValueEnvelopeInput{Source: ProvenanceUser},
	})
	require.NoError(t, err)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(t, err)
	selected := []personfacts.TargetDescriptor{
		targets[AttributeSlugPrimaryChannel],
		targets[AttributeSlugAskMeAbout],
	}
	profile, err := (personenrichment.ProviderConfig{
		Name: "exa-results", Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://api.example.test/search", APIKeyEnv: "PROVIDER_API_KEY",
		Mode: "deep", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierEmail, personenrichment.IdentifierPublicProfileURL,
		},
		TargetKeys:       []string{selected[0].Key, selected[1].Key},
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		RefreshInterval: 24 * time.Hour, RequestTimeout: time.Minute,
		PollInterval: 30 * time.Second, MaxJobAge: 15 * time.Minute, MaxRetries: 5,
		MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
	}).Profile(personfacts.Catalog{Version: "fixture-v1", Targets: selected})
	require.NoError(t, err)
	_, err = st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = st.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(t, err)

	now := time.Date(2026, 8, 23, 12, 0, 0, 123000000, time.UTC)
	SetPersonEnrichmentClockForTest(st, func() time.Time { return now })
	run, created, err := st.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "result-fixture", RequestedAt: now,
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, st.PutPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkInput{
		PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "manual:result-fixture"},
		DueAt:   now,
	}))
	lease, err := st.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "result-worker", ProviderName: profile.Name,
		Now: now, LeaseDuration: 5 * time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	attempt, created, err := st.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
		PayloadHash: strings.Repeat("1", 64), RequestHash: strings.Repeat("2", 64),
		PersonRevision: person.Revision, Trigger: lease.Trigger,
	})
	require.NoError(t, err)
	require.True(t, created)

	schemaHash := strings.Repeat("a", 64)
	result := personenrichment.Result{
		State: personenrichment.ResultComplete, RequestID: " opaque-request\t",
		JobID: "opaque/job:Case?part=1", FreshAsOf: now.Add(-time.Hour),
		AdapterVersion: "exa-adapter-fixture-v1", SchemaVersion: "exa-wire-fixture-v1",
		ProviderVersion: "provider-fixture-v1", Model: "fixture-model", ModelVersion: "fixture-model-v1",
		GeneratedSchema: true, GeneratedSchemaHash: schemaHash,
		ProviderPersonIDs: []personenrichment.ProviderPersonID{{
			ID: "Opaque/Person:Case?part=1", Confidence: 975,
		}},
		CanonicalPublicURLs: []string{"https://profiles.example.test/alice%2Fstable"},
		Citations: []personenrichment.Citation{{
			Key: "citation-profile", URL: "https://sources.example.test/profile/alice",
			Title: "Synthetic public profile", Publisher: "Example Publisher",
			Excerpt:     "Synthetic public evidence shared by two claims.",
			PublishedAt: now.Add(-48 * time.Hour), RetrievedAt: now.Add(-time.Hour),
		}},
		SourceAttempts: []personenrichment.SourceAttempt{
			{URL: "https://sources.example.test/profile/alice", Outcome: "cited", ObservedAt: now.Add(-time.Hour)},
			{URL: "https://directory.example.test/visited", Outcome: "visited", ObservedAt: now.Add(-2 * time.Hour)},
		},
		Cost: personenrichment.Cost{Currency: "USD", AmountMicros: 600},
		Claims: []personfacts.ProposedClaim{
			enrichmentPublicClaim(selected[0], `"chat"`, 930),
			enrichmentPublicClaim(selected[1], `"sailing"`, 870),
		},
	}
	programFingerprint, err := personenrichment.ProgramFingerprint(personenrichment.ProgramDescriptor{
		HostMappingVersion: personenrichment.HostClaimMappingVersion,
		AdapterVersion:     result.AdapterVersion, WireSchemaVersion: result.SchemaVersion,
		GeneratedSchema: true, GeneratedSchemaHash: schemaHash,
	})
	require.NoError(t, err)
	require.NoError(t, st.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	require.NoError(t, st.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, RequestID: result.RequestID, JobID: result.JobID,
		StartedAt:      now,
		AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
		GeneratedSchema: true, GeneratedSchemaHash: schemaHash, Targets: selected,
		ProgramFingerprint: programFingerprint,
	}))
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x6a}, 32))
	require.NoError(t, err)
	commit, err := personenrichment.NewClaimCommit(personenrichment.ClaimCommitInput{
		AttemptID: attempt.ID, RunID: run.ID, PersonID: person.ID,
		LeaseFence: attempt.Token.Fence, ProfileFingerprint: profile.Fingerprint,
		ProviderNamespace: profile.ProviderNamespace, RequestHash: strings.Repeat("2", 64),
		IdentityAssessment: personenrichment.IdentityAssessment{
			Accepted: true, Score: 1000, Reason: "strong_identifier_match",
			MatchedClasses: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		},
	}, result, hasher)
	require.NoError(t, err)
	return &enrichmentResultFixture{
		store: st, person: person, profile: profile, lease: lease,
		attempt: attempt, result: result, commit: commit, now: now,
	}
}

func enrichmentPublicClaim(
	target personfacts.TargetDescriptor, submitted string, reportedScore int,
) personfacts.ProposedClaim {
	return personfacts.ProposedClaim{
		Target: target, Relation: personfacts.RelationSupport,
		SubmittedValue: json.RawMessage(submitted), Origin: personfacts.OriginEnrichment,
		Confidence: personfacts.ConfidenceInputs{ReportedScore: reportedScore},
		Evidence: []personfacts.EvidenceInput{{
			SourceClass: personfacts.EvidencePublic, SourceRef: "citation-profile",
			Directness: personfacts.DirectOther, Authority: personfacts.AuthorityAuthoritative,
		}},
	}
}

func enrichmentProviderAssertionClaim(
	target personfacts.TargetDescriptor, submitted, sourceRef string, reportedScore int,
) personfacts.ProposedClaim {
	return personfacts.ProposedClaim{
		Target: target, Relation: personfacts.RelationSupport,
		SubmittedValue: json.RawMessage(submitted), Origin: personfacts.OriginEnrichment,
		Confidence: personfacts.ConfidenceInputs{ReportedScore: reportedScore},
		Evidence: []personfacts.EvidenceInput{{
			SourceClass: personfacts.EvidenceProviderAssertion, SourceRef: sourceRef,
			Directness: personfacts.Indirect, Authority: personfacts.AuthorityAggregator,
			Excerpt: "Synthetic provider assertion.",
		}},
	}
}

func (f *enrichmentResultFixture) reseal(t *testing.T) {
	t.Helper()
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x6a}, 32))
	require.NoError(t, err)
	f.commit, err = personenrichment.NewClaimCommit(personenrichment.ClaimCommitInput{
		AttemptID: f.attempt.ID, RunID: f.attempt.RunID, PersonID: f.person.ID,
		LeaseFence: f.attempt.Token.Fence, ProfileFingerprint: f.profile.Fingerprint,
		ProviderNamespace: f.profile.ProviderNamespace, RequestHash: strings.Repeat("2", 64),
		IdentityAssessment: f.commit.IdentityAssessment,
	}, f.result, hasher)
	require.NoError(t, err)
}

func TestCommitEnrichmentClaimsAppliesAtomicallyAndReplaysRichResult(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	first, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	requirements.NoError(err)
	requirements.Equal(personenrichment.ClaimApplied, first.Status)
	requirements.NotNil(first.Generation)
	requirements.Len(first.Generation.Resolutions, 2)

	second, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	requirements.NoError(err)
	requirements.Equal(personenrichment.ClaimApplied, second.Status)
	requirements.NotNil(second.Generation)
	checks.Equal(first.Generation.GenerationID, second.Generation.GenerationID)
	checks.Equal(first.Generation.GenerationKey, second.Generation.GenerationKey)
	checks.Equal(first.Generation.Resolutions, second.Generation.Resolutions)

	claims, err := f.store.ListPersonFactClaimsContext(t.Context(), f.person.ID, personfacts.ClaimFilter{})
	requirements.NoError(err)
	checks.Len(claims, 2)
	decisions, err := f.store.ListPersonFactDecisionsContext(t.Context(), f.person.ID, personfacts.DecisionFilter{})
	requirements.NoError(err)
	checks.Len(decisions, 2)
	for _, decision := range decisions {
		checks.Equal(personfacts.DecisionApplied, decision.Action)
	}
	citations, err := f.store.ListPersonEnrichmentAttemptCitationsContext(t.Context(), f.attempt.ID)
	requirements.NoError(err)
	requirements.Len(citations, 1)
	checks.Equal("citation-profile", citations[0].CitationKey)
	checks.Equal("https://sources.example.test/profile/alice", citations[0].CanonicalURL)

	identities, err := f.store.LoadProviderPersonIDs(t.Context(), f.person.ID, f.profile.ProviderNamespace)
	requirements.NoError(err)
	checks.Equal([]string{"Opaque/Person:Case?part=1"}, identities)
	attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), f.attempt.ID)
	requirements.NoError(err)
	checks.Equal("succeeded", attempt.State)
	requirements.NotNil(attempt.CompletedAt)
	checks.Equal(f.now, *attempt.CompletedAt)
	requirements.NotNil(attempt.FactGenerationKey)
	checks.Equal(first.Generation.GenerationKey, *attempt.FactGenerationKey)
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(work, 1)
	checks.Nil(work[0].RunID)
	checks.Nil(work[0].ActiveAttemptID)
	checks.Equal(f.now.Add(f.profile.RefreshInterval), work[0].DueAt)
}

func TestPersonEnrichmentResultRejectsConcurrentConfiguredKeyWinner(t *testing.T) {
	for _, winner := range []string{"suppression", "deletion"} {
		t.Run(winner, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newEnrichmentResultFixture(t)
			newHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x4e}, 32))
			requirements.NoError(err)
			newDigest := newHasher.Digest(f.profile.ProviderNamespace,
				personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
				"configured-result-winner@example.test")
			newKeyID, err := newHasher.KeyID()
			requirements.NoError(err)
			input := PersonEnrichmentSuppressionInput{
				ProviderNamespace: newDigest.ProviderNamespace, IdentifierClass: newDigest.IdentifierClass,
				NormalizationVersion: newDigest.NormalizationVersion, KeyID: newDigest.KeyID,
				Digest: newDigest.Digest, Reason: PersonEnrichmentSuppressionDeletion,
				Actor: "privacy-test",
			}
			var deletePerson *Person
			if winner == "deletion" {
				participantID, createErr := f.store.EnsureParticipant(
					"delete-result-winner@example.test", "Delete Result Winner", "example.test")
				requirements.NoError(createErr)
				deletePerson, _, createErr = f.store.CreatePersonFromParticipantContext(
					t.Context(), participantID)
				requirements.NoError(createErr)
			}

			resultReached := make(chan struct{})
			releaseResult := make(chan struct{})
			SetPersonEnrichmentTxBarrierForTest(f.store, func(phase string) {
				if phase == "result_before_authority_lock" {
					close(resultReached)
					<-releaseResult
				}
			})
			type commitResult struct {
				outcome *personenrichment.ClaimOutcome
				err     error
			}
			result := make(chan commitResult, 1)
			go func() {
				outcome, commitErr := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
				result <- commitResult{outcome: outcome, err: commitErr}
			}()
			requireEnrichmentResultSignal(t, resultReached,
				"result did not reach its authority gate")
			if winner == "suppression" {
				requirements.NoError(f.store.InsertPersonEnrichmentSuppressionsForConfiguredKeyContext(
					t.Context(), newKeyID, []PersonEnrichmentSuppressionInput{input}))
			} else {
				requirements.NoError(f.store.DeletePersonWithEnrichmentSuppressionsContext(
					t.Context(), DeletePersonEnrichmentInput{
						PersonID: deletePerson.ID, ExpectedRevision: deletePerson.Revision,
						ConfiguredKeyID: newKeyID, Actor: "privacy-test",
						Reason:             PersonEnrichmentSuppressionDeletion,
						CurrentIdentifiers: []PersonEnrichmentSuppressionInput{input},
					}))
			}
			close(releaseResult)
			got := <-result
			requirements.ErrorIs(got.err, personenrichment.ErrSuppressionKeyMismatch)
			checks.Nil(got.outcome)
			assertNoEnrichmentResultSideEffects(t, f, true)
			attempt, getErr := f.store.GetPersonEnrichmentAttemptContext(t.Context(), f.attempt.ID)
			requirements.NoError(getErr)
			checks.Equal("pending", attempt.State)
			if winner == "deletion" {
				_, getErr = f.store.GetPersonContext(t.Context(), deletePerson.ID)
				checks.ErrorIs(getErr, ErrPersonNotFound)
			}
		})
	}
}

func TestPersonEnrichmentResultDeletionRevocationLockHierarchyDoesNotDeadlock(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x6a}, 32))
	requirements.NoError(err)
	keyID, err := hasher.KeyID()
	requirements.NoError(err)
	digest := hasher.Digest(f.profile.ProviderNamespace,
		personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
		"delete-lock-hierarchy@example.test")
	participantID, err := f.store.EnsureParticipant(
		"delete-lock-hierarchy@example.test", "Delete Lock Hierarchy", "example.test")
	requirements.NoError(err)
	deletePerson, _, err := f.store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)

	resultLocked := make(chan struct{})
	releaseResult := make(chan struct{})
	var resultOnce sync.Once
	SetPersonEnrichmentTxBarrierForTest(f.store, func(phase string) {
		if phase == "result_person_locked" {
			resultOnce.Do(func() {
				close(resultLocked)
				<-releaseResult
			})
		}
	})
	claimDone := make(chan enrichmentClaimResult, 1)
	go func() {
		outcome, commitErr := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
		claimDone <- enrichmentClaimResult{outcome: outcome, err: commitErr}
	}()
	requireEnrichmentResultSignal(t, resultLocked, "result did not acquire its person-first gate")
	revokeDone := make(chan error, 1)
	revokeStarted := make(chan struct{})
	go func() {
		close(revokeStarted)
		_, revokeErr := f.store.RevokePersonEnrichmentConsent(
			t.Context(), f.profile.Fingerprint, "privacy-test")
		revokeDone <- revokeErr
	}()
	requireEnrichmentResultSignal(t, revokeStarted, "revocation did not start")
	deleteDone := make(chan error, 1)
	deleteStarted := make(chan struct{})
	go func() {
		close(deleteStarted)
		deleteDone <- f.store.DeletePersonWithEnrichmentSuppressionsContext(
			t.Context(), DeletePersonEnrichmentInput{
				PersonID: deletePerson.ID, ExpectedRevision: deletePerson.Revision,
				ConfiguredKeyID: keyID, Actor: "privacy-test",
				Reason: PersonEnrichmentSuppressionDeletion,
				CurrentIdentifiers: []PersonEnrichmentSuppressionInput{{
					ProviderNamespace: digest.ProviderNamespace,
					IdentifierClass:   digest.IdentifierClass, NormalizationVersion: digest.NormalizationVersion,
					KeyID: digest.KeyID, Digest: digest.Digest,
					Reason: PersonEnrichmentSuppressionDeletion, Actor: "privacy-test",
				}},
			})
	}()
	requireEnrichmentResultSignal(t, deleteStarted, "deletion did not start")
	close(releaseResult)
	claim := requireEnrichmentClaimResult(t, claimDone)
	requirements.NoError(claim.err)
	requirements.NotNil(claim.outcome)
	checks.Equal(personenrichment.ClaimApplied, claim.outcome.Status)
	requirements.NoError(requireEnrichmentErrorResult(t, revokeDone, "revocation deadlocked"))
	requirements.NoError(requireEnrichmentErrorResult(t, deleteDone, "deletion deadlocked"))
	_, err = f.store.GetPersonContext(t.Context(), deletePerson.ID)
	checks.ErrorIs(err, ErrPersonNotFound)
}

func requireEnrichmentClaimResult(
	t *testing.T, result <-chan enrichmentClaimResult,
) enrichmentClaimResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		require.FailNow(t, "person enrichment result deadlocked")
		return enrichmentClaimResult{}
	}
}

func requireEnrichmentErrorResult(t *testing.T, result <-chan error, message string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		require.FailNow(t, message)
		return nil
	}
}

func requireEnrichmentResultSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		require.FailNow(t, message)
	}
}

func TestPersonEnrichmentResultPreparationHasNoDurableSideEffects(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
	requirements.NoError(err)
	requirements.NotNil(prepared)
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_enrichment_citations"))
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_fact_generations"))
	attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), f.attempt.ID)
	requirements.NoError(err)
	checks.Equal("pending", attempt.State)
}

func TestCommitEnrichmentClaimsRollsBackCitationAndProjectionFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger string
	}{
		{name: "after citation insertion", trigger: `
			CREATE TRIGGER fail_enrichment_citation AFTER INSERT ON person_enrichment_citations
			BEGIN SELECT RAISE(FAIL, 'injected citation failure'); END`},
		{name: "during projection", trigger: `
			CREATE TRIGGER fail_enrichment_projection BEFORE INSERT ON person_attribute_values
			BEGIN SELECT RAISE(FAIL, 'injected projection failure'); END`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEnrichmentResultFixture(t)
			if f.store.IsPostgreSQL() {
				t.Skip("SQLite trigger injection; PostgreSQL atomicity is covered by the shared transaction path")
			}
			_, err := f.store.DB().ExecContext(t.Context(), test.trigger)
			require.NoError(t, err)
			_, err = f.store.CommitEnrichmentClaims(t.Context(), f.commit)
			require.Error(t, err)
			assertNoEnrichmentResultSideEffects(t, f, true)
		})
	}
}

func TestCommitEnrichmentClaimsRejectsStaleFenceWithoutWrites(t *testing.T) {
	f := newEnrichmentResultFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_enrichment_attempts SET lease_fence = lease_fence + 1 WHERE id = ?`), f.attempt.ID)
	require.NoError(t, err)
	_, err = f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	require.ErrorIs(t, err, ErrStaleLease)
	assertNoEnrichmentResultSideEffects(t, f, true)
}

func TestPersonEnrichmentResultRejectsEnvelopeChangesAfterPreparation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *enrichmentResultFixture)
	}{
		{name: "request", mutate: func(t *testing.T, f *enrichmentResultFixture) {
			t.Helper()
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
				`UPDATE person_enrichment_attempts SET request_hash = ? WHERE id = ?`),
				strings.Repeat("3", 64), f.attempt.ID)
			require.NoError(t, err)
		}},
		{name: "profile", mutate: func(t *testing.T, f *enrichmentResultFixture) {
			t.Helper()
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
				`UPDATE person_enrichment_profiles SET endpoint = ? WHERE fingerprint = ?`),
				"https://changed.example.test/search", f.profile.Fingerprint)
			require.NoError(t, err)
		}},
		{name: "catalog", mutate: func(t *testing.T, f *enrichmentResultFixture) {
			t.Helper()
			description := "Changed inference semantics"
			definition, err := f.store.GetAttributeDefinitionBySlugContext(
				t.Context(), AttributeObjectPerson, AttributeSlugPrimaryChannel)
			require.NoError(t, err)
			descriptionPointer := &description
			_, err = f.store.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision,
				AttributeDefinitionUpdate{Description: &descriptionPointer})
			require.NoError(t, err)
		}},
		{name: "person revision", mutate: func(t *testing.T, f *enrichmentResultFixture) {
			t.Helper()
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
				`UPDATE persons SET revision = revision + 1 WHERE id = ?`), f.person.ID)
			require.NoError(t, err)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newEnrichmentResultFixture(t)
			prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
			require.NoError(t, err)
			test.mutate(t, f)
			_, err = f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
			require.Error(t, err)
			assertNoEnrichmentResultSideEffects(t, f, true)
		})
	}
}

func TestPersonEnrichmentResultSensitivePostureChangeAfterPreparationIsPolicyTerminal(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
	requirements.NoError(err)
	definition, err := f.store.GetAttributeDefinitionBySlugContext(
		t.Context(), AttributeObjectPerson, AttributeSlugPrimaryChannel)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE attribute_definitions SET is_sensitive = ?, revision = revision + 1 WHERE id = ?`),
		true, definition.ID)
	requirements.NoError(err)
	outcome, err := f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
	requirements.NoError(err)
	checks.Equal(personenrichment.ClaimPolicyRejected, outcome.Status)
	checks.Nil(outcome.Generation)
	assertNoEnrichmentResultSideEffects(t, f, false)
	attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), f.attempt.ID)
	requirements.NoError(err)
	checks.Equal("terminal", attempt.State)
	checks.Equal(int64(600), requireAttemptActualCost(t, attempt))
}

func TestPersonEnrichmentResultUsesOnePreparedCompletionTimestamp(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
	requirements.NoError(err)
	SetPersonEnrichmentClockForTest(f.store, func() time.Time { return f.now.Add(9 * time.Hour) })
	outcome, err := f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
	requirements.NoError(err)
	checks.Equal(personenrichment.ClaimApplied, outcome.Status)
	attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), f.attempt.ID)
	requirements.NoError(err)
	requirements.NotNil(attempt.CompletedAt)
	checks.Equal(f.now, *attempt.CompletedAt)
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	requirements.Len(work, 1)
	checks.Equal(f.now.Add(f.profile.RefreshInterval), work[0].DueAt)
}

func TestPersonEnrichmentResultDeduplicatesMetadataAndPreservesOpaqueIDs(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	f.result.Citations = append(f.result.Citations, f.result.Citations[0])
	f.result.SourceAttempts = append(f.result.SourceAttempts, f.result.SourceAttempts...)
	f.result.ProviderPersonIDs = []personenrichment.ProviderPersonID{{
		ID: "Opaque ID/Not-A-URL:MiXeD?x=1#fragment", Confidence: 901,
	}}
	f.reseal(t)
	outcome, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	requirements.NoError(err)
	checks.Equal(personenrichment.ClaimApplied, outcome.Status)
	checks.Equal(int64(1), enrichmentTableCount(t, f.store, "person_enrichment_citations"))
	checks.Equal(int64(1), enrichmentTableCount(t, f.store, "person_enrichment_attempt_citations"))
	checks.Equal(int64(2), enrichmentTableCount(t, f.store, "person_enrichment_attempt_sources"))
	identities, err := f.store.LoadProviderPersonIDs(t.Context(), f.person.ID, f.profile.ProviderNamespace)
	requirements.NoError(err)
	checks.Equal([]string{"Opaque ID/Not-A-URL:MiXeD?x=1#fragment"}, identities)
}

func TestCommitEnrichmentClaimsRanksUnsupportedAggregatorEvidenceBelowThreshold(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	f.result.Claims = []personfacts.ProposedClaim{enrichmentProviderAssertionClaim(
		f.profile.Targets[0], `"chat"`, "", 1000,
	)}
	f.result.Citations = nil
	f.reseal(t)
	outcome, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	requirements.NoError(err)
	requirements.NotNil(outcome.Generation)
	requirements.Len(outcome.Generation.Decisions, 1)
	checks.Equal(personfacts.DecisionRetained, outcome.Generation.Decisions[0].Action)
	checks.Equal(personfacts.ReasonBelowThreshold, outcome.Generation.Decisions[0].Reason)
	checks.Empty(outcome.Generation.Projections)
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_enrichment_citations"))
	evidence, err := f.store.ListPersonFactEvidenceContext(t.Context(), f.person.ID,
		personfacts.EvidenceFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(evidence, 1)
	checks.Equal("enrichment-attempt:"+strconv.FormatInt(f.attempt.ID, 10)+
		":job:"+f.result.JobID, evidence[0].Input.SourceRef)
	checks.Empty(evidence[0].Input.SourceURL)
}

func TestCommitEnrichmentClaimsRecordsMismatchedExternalEvidenceAsInvalid(t *testing.T) {
	tests := []struct {
		name   string
		claim  func(*enrichmentResultFixture) personfacts.ProposedClaim
		reason personfacts.DecisionReason
	}{
		{name: "forged provider assertion ref", claim: func(f *enrichmentResultFixture) personfacts.ProposedClaim {
			return enrichmentProviderAssertionClaim(
				f.profile.Targets[0], `"chat"`, "enrichment-attempt:999:job:forged", 1000)
		}, reason: personfacts.ReasonUnalignedEvidence},
		{name: "adapter supplied current-looking provider assertion ref", claim: func(f *enrichmentResultFixture) personfacts.ProposedClaim {
			return enrichmentProviderAssertionClaim(f.profile.Targets[0], `"chat"`,
				"enrichment-attempt:"+strconv.FormatInt(f.attempt.ID, 10)+":job:"+f.result.JobID, 1000)
		}, reason: personfacts.ReasonUnalignedEvidence},
		{name: "adapter supplied provider assertion URL", claim: func(f *enrichmentResultFixture) personfacts.ProposedClaim {
			claim := enrichmentProviderAssertionClaim(f.profile.Targets[0], `"chat"`, "", 1000)
			claim.Evidence[0].SourceURL = "https://provider.example.test/unsupported"
			return claim
		}, reason: personfacts.ReasonUnalignedEvidence},
		{name: "citation URL mismatch", claim: func(f *enrichmentResultFixture) personfacts.ProposedClaim {
			claim := enrichmentPublicClaim(f.profile.Targets[0], `"chat"`, 1000)
			claim.Evidence[0].SourceURL = "https://other.example.test/profile"
			return claim
		}, reason: personfacts.ReasonUnalignedEvidence},
		{name: "unknown citation key", claim: func(f *enrichmentResultFixture) personfacts.ProposedClaim {
			claim := enrichmentPublicClaim(f.profile.Targets[0], `"chat"`, 1000)
			claim.Evidence[0].SourceRef = "unknown-citation"
			claim.Evidence[0].SourceURL = "https://other.example.test/profile"
			return claim
		}, reason: personfacts.ReasonUnalignedEvidence},
		{name: "unsupported source class", claim: func(f *enrichmentResultFixture) personfacts.ProposedClaim {
			claim := enrichmentProviderAssertionClaim(f.profile.Targets[0], `"chat"`,
				"enrichment-attempt:"+strconv.FormatInt(f.attempt.ID, 10)+":job:"+f.result.JobID, 1000)
			claim.Evidence[0].SourceClass = personfacts.EvidenceSystem
			return claim
		}, reason: personfacts.ReasonMalformedValue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newEnrichmentResultFixture(t)
			f.result.Claims = []personfacts.ProposedClaim{test.claim(f)}
			f.reseal(t)
			outcome, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
			requirements.NoError(err)
			checks.Equal(personenrichment.ClaimApplied, outcome.Status)
			requirements.NotNil(outcome.Generation)
			requirements.Len(outcome.Generation.Decisions, 1)
			checks.Equal(personfacts.DecisionInvalid, outcome.Generation.Decisions[0].Action)
			checks.Equal(test.reason, outcome.Generation.Decisions[0].Reason)
			checks.Empty(outcome.Generation.Projections)
		})
	}
}

func TestCommitEnrichmentClaimsWeakIdentityIsAuditableAndCannotProject(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	f.commit.IdentityAssessment = personenrichment.IdentityAssessment{
		Accepted: false, Score: 0, Reason: "identity_not_verified",
	}
	f.result.Claims = []personfacts.ProposedClaim{
		enrichmentPublicClaim(f.profile.Targets[0], `"chat"`, 999),
		enrichmentProviderAssertionClaim(f.profile.Targets[1], `"provider-value"`, "", 998),
	}
	f.reseal(t)
	outcome, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	requirements.NoError(err)
	checks.Equal(personenrichment.ClaimIdentityRejected, outcome.Status)
	requirements.NotNil(outcome.Generation)
	requirements.Len(outcome.Generation.Decisions, 2)
	for _, decision := range outcome.Generation.Decisions {
		checks.Equal(personfacts.DecisionIdentityRejected, decision.Action)
	}
	checks.Empty(outcome.Generation.Projections)
	evidence, err := f.store.ListPersonFactEvidenceContext(t.Context(), f.person.ID,
		personfacts.EvidenceFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(evidence, 2)
	for _, item := range evidence {
		checks.Equal(0, item.Input.IdentityScore)
	}
	claims, err := f.store.ListPersonFactClaimsContext(t.Context(), f.person.ID,
		personfacts.ClaimFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(claims, 2)
	checks.ElementsMatch([]int{998, 999}, []int{
		claims[0].Confidence.ReportedScore, claims[1].Confidence.ReportedScore,
	})
	attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), f.attempt.ID)
	requirements.NoError(err)
	checks.Equal("identity_rejected", attempt.State)
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_enrichment_attempt_identifiers"))
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_enrichment_citations"))
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_enrichment_attempt_sources"))
	assertNoRefreshWork(t, f)
}

func TestCommitEnrichmentClaimsLateReturnedIdentifierSuppressionIsPrivateAndTerminal(t *testing.T) {
	for _, class := range []personenrichment.SuppressionIdentifierClass{
		personenrichment.SuppressionProviderPersonID,
		personenrichment.SuppressionPublicProfileURL,
	} {
		t.Run(string(class), func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newEnrichmentResultFixture(t)
			digests, err := f.commit.VerifiedReturnedIdentifierDigests()
			requirements.NoError(err)
			index := slices.IndexFunc(digests, func(item personenrichment.SuppressionDigest) bool {
				return item.IdentifierClass == class
			})
			requirements.NotEqual(-1, index)
			digest := digests[index]
			requirements.NoError(f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(),
				[]PersonEnrichmentSuppressionInput{{
					ProviderNamespace:    digest.ProviderNamespace,
					IdentifierClass:      digest.IdentifierClass,
					NormalizationVersion: digest.NormalizationVersion,
					KeyID:                digest.KeyID, Digest: digest.Digest,
					Reason: PersonEnrichmentSuppressionOptOut, Actor: "privacy-test",
				}}))
			outcome, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
			requirements.NoError(err)
			checks.Equal(personenrichment.ClaimSuppressed, outcome.Status)
			checks.Nil(outcome.Generation)
			assertNoEnrichmentResultSideEffects(t, f, false)
			attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), f.attempt.ID)
			requirements.NoError(err)
			checks.Equal("suppressed", attempt.State)
			checks.Equal(int64(600), requireAttemptActualCost(t, attempt))
			assertNoRefreshWork(t, f)
		})
	}
}

func TestCommitEnrichmentClaimsProviderIdentityOwnedByAnotherPersonIsAuditable(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	otherParticipant, err := f.store.EnsureParticipant("owner@example.com", "Other Owner", "example.com")
	requirements.NoError(err)
	other, _, err := f.store.CreatePersonFromParticipantContext(t.Context(), otherParticipant)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_enrichment_provider_identities
			(person_id, provider_namespace, provider_person_id, confidence, verified_at)
		VALUES (?, ?, ?, ?, ?)`), other.ID, f.profile.ProviderNamespace,
		f.result.ProviderPersonIDs[0].ID, 1000, f.now.Add(-time.Hour))
	requirements.NoError(err)

	outcome, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	requirements.NoError(err)
	checks.Equal(personenrichment.ClaimIdentityRejected, outcome.Status)
	requirements.NotNil(outcome.Generation)
	for _, decision := range outcome.Generation.Decisions {
		checks.Equal(personfacts.DecisionIdentityRejected, decision.Action)
		checks.Equal(personfacts.ReasonIdentityMismatch, decision.Reason)
	}
	owned, err := f.store.LoadProviderPersonIDs(t.Context(), f.person.ID, f.profile.ProviderNamespace)
	requirements.NoError(err)
	checks.Empty(owned)
	checks.Empty(outcome.Generation.Projections)
	firstGeneration := outcome.Generation
	replayed, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	requirements.NoError(err)
	checks.Equal(personenrichment.ClaimIdentityRejected, replayed.Status)
	requirements.NotNil(replayed.Generation)
	checks.Equal(firstGeneration.GenerationID, replayed.Generation.GenerationID)
	checks.Equal(firstGeneration.GenerationKey, replayed.Generation.GenerationKey)
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_enrichment_attempt_identifiers"))
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_enrichment_citations"))
	checks.Equal(int64(0), enrichmentTableCount(t, f.store, "person_enrichment_attempt_sources"))
	assertNoRefreshWork(t, f)
}

func TestCommitEnrichmentClaimsPostgresSerializesTwoPersonProviderIdentityOwnership(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	testDB := os.Getenv("MSGVAULT_TEST_DB")
	if !IsPostgresURL(testDB) {
		t.Skip("PostgreSQL ownership race requires MSGVAULT_TEST_DB")
	}
	first := newEnrichmentResultFixture(t)
	secondParticipant, err := first.store.EnsureParticipant("second@example.com", "Second", "example.com")
	requirements.NoError(err)
	secondPerson, _, err := first.store.CreatePersonFromParticipantContext(t.Context(), secondParticipant)
	requirements.NoError(err)
	secondStore := openPostgresStoreInCurrentSchema(t, first.store, testDB)
	requirements.NotSame(first.store.DB(), secondStore.DB())

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	type keyStage struct {
		backendPID int
		err        error
	}
	firstKeyHeld := make(chan keyStage, 1)
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	defer releaseFirstOnce.Do(func() { close(releaseFirst) })
	setPersonEnrichmentProviderIdentityBarrierForTest(first.store, func(phase string, tx *loggedTx) {
		if phase != "provider_identity_key_locked" {
			return
		}
		var backendPID int
		pidErr := tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&backendPID)
		firstKeyHeld <- keyStage{backendPID: backendPID, err: pidErr}
		<-releaseFirst
	})
	secondBeforeKey := make(chan struct{})
	secondKeyHeld := make(chan struct{})
	var secondBeforeKeyOnce sync.Once
	var secondKeyHeldOnce sync.Once
	setPersonEnrichmentProviderIdentityBarrierForTest(secondStore, func(phase string, _ *loggedTx) {
		switch phase {
		case "before_provider_identity_key":
			secondBeforeKeyOnce.Do(func() { close(secondBeforeKey) })
		case "provider_identity_key_locked":
			secondKeyHeldOnce.Do(func() { close(secondKeyHeld) })
		}
	})
	await := func(stage string, reached <-chan struct{}) {
		t.Helper()
		select {
		case <-reached:
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "await %s", stage)
		}
	}

	type ownershipResult struct {
		personID int64
		owner    int64
		claimed  bool
		err      error
	}
	results := make(chan ownershipResult, 2)
	claim := func(st *Store, personID int64) {
		item := ownershipResult{personID: personID}
		item.err = st.withTxContext(ctx, func(tx *loggedTx) error {
			owner, owned, lockErr := st.lockPersonEnrichmentProviderIdentityOwnershipTx(
				ctx, tx, first.profile.ProviderNamespace, first.result.ProviderPersonIDs[0].ID)
			if lockErr != nil {
				return lockErr
			}
			if owned {
				item.owner = owner
				return nil
			}
			item.claimed = true
			return st.attachPersonEnrichmentProviderIdentityTx(ctx, tx, personID,
				first.profile.ProviderNamespace, first.result.ProviderPersonIDs[0], first.now)
		})
		results <- item
	}
	go claim(first.store, first.person.ID)
	var firstStage keyStage
	select {
	case firstStage = <-firstKeyHeld:
	case <-ctx.Done():
		requirements.NoError(ctx.Err(), "await first provider identity key")
	}
	requirements.NoError(firstStage.err)
	go claim(secondStore, secondPerson.ID)
	await("second pre-provider-key stage", secondBeforeKey)
	requirements.Eventually(func() bool {
		var waiting bool
		queryErr := first.store.DB().QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity activity
				JOIN pg_locks waiting
				  ON waiting.pid = activity.pid
				 AND waiting.locktype = 'advisory'
				 AND NOT waiting.granted
				WHERE activity.datname = current_database()
				  AND activity.wait_event_type = 'Lock'
				  AND $1 = ANY(pg_blocking_pids(activity.pid))
			)`, firstStage.backendPID).Scan(&waiting)
		return queryErr == nil && waiting
	}, 3*time.Second, 10*time.Millisecond,
		"second transaction must wait on the first transaction's provider identity key")
	select {
	case <-secondKeyHeld:
		requirements.Fail("second transaction acquired provider identity key before release")
	default:
	}
	releaseFirstOnce.Do(func() { close(releaseFirst) })
	await("second post-provider-key stage", secondKeyHeld)

	claims := make(map[int64]ownershipResult, 2)
	for range 2 {
		var item ownershipResult
		select {
		case item = <-results:
		case <-ctx.Done():
			requirements.NoError(ctx.Err(), "await serialized commits")
		}
		requirements.NoError(item.err)
		claims[item.personID] = item
	}
	requirements.Len(claims, 2)
	checks.True(claims[first.person.ID].claimed)
	checks.False(claims[secondPerson.ID].claimed)
	checks.Equal(first.person.ID, claims[secondPerson.ID].owner)
	var owners int64
	requirements.NoError(first.store.DB().QueryRowContext(t.Context(), first.store.Rebind(`
		SELECT COUNT(*) FROM person_enrichment_provider_identities
		WHERE provider_namespace = ? AND provider_person_id = ?`),
		first.profile.ProviderNamespace, first.result.ProviderPersonIDs[0].ID).Scan(&owners))
	checks.Equal(int64(1), owners)

	var publicPathStages []string
	setPersonEnrichmentProviderIdentityBarrierForTest(first.store, func(phase string, _ *loggedTx) {
		publicPathStages = append(publicPathStages, phase)
	})
	outcome, err := first.store.CommitEnrichmentClaims(t.Context(), first.commit)
	requirements.NoError(err)
	requirements.NotNil(outcome)
	checks.Equal(personenrichment.ClaimApplied, outcome.Status)
	checks.Equal([]string{
		"before_provider_identity_key", "provider_identity_key_locked",
	}, publicPathStages)
}

func openPostgresStoreInCurrentSchema(t *testing.T, current *Store, testDB string) *Store {
	t.Helper()
	var schema string
	require.NoError(t, current.DB().QueryRowContext(t.Context(), "SELECT current_schema()").Scan(&schema))
	dsn, err := url.Parse(testDB)
	require.NoError(t, err)
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	st, err := OpenForTest(dsn.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st
}

func TestPersonEnrichmentResultRevokedConsentAfterPreparationIsPolicyTerminal(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentResultFixture(t)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
	requirements.NoError(err)
	changed, err := f.store.RevokePersonEnrichmentConsent(t.Context(), f.profile.Fingerprint, "privacy-test")
	requirements.NoError(err)
	requirements.True(changed)
	outcome, err := f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
	requirements.NoError(err)
	checks.Equal(personenrichment.ClaimPolicyRejected, outcome.Status)
	checks.Nil(outcome.Generation)
	assertNoEnrichmentResultSideEffects(t, f, false)
	attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), f.attempt.ID)
	requirements.NoError(err)
	checks.Equal("terminal", attempt.State)
	requirements.NotNil(attempt.FailureClass)
	checks.Equal(string(personenrichment.FailurePolicy), *attempt.FailureClass)
	checks.Equal(int64(600), requireAttemptActualCost(t, attempt))
	assertNoRefreshWork(t, f)
}

func assertNoEnrichmentResultSideEffects(t *testing.T, f *enrichmentResultFixture, workMustRemain bool) {
	t.Helper()
	for _, table := range []string{
		"person_enrichment_attempt_identifiers", "person_enrichment_provider_identities",
		"person_enrichment_citations", "person_enrichment_attempt_citations",
		"person_enrichment_attempt_sources", "person_fact_generations",
		"person_fact_evidence", "person_fact_claims", "person_fact_decisions",
		"person_attribute_values",
	} {
		assert.Equal(t, int64(0), enrichmentTableCount(t, f.store, table), table)
	}
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(t, err)
	if workMustRemain {
		require.Len(t, work, 1)
		require.NotNil(t, work[0].ActiveAttemptID)
		assert.Equal(t, f.attempt.ID, *work[0].ActiveAttemptID)
	} else {
		assert.Empty(t, work)
	}
}

func assertNoRefreshWork(t *testing.T, f *enrichmentResultFixture) {
	t.Helper()
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, work)
}

func enrichmentTableCount(t *testing.T, st *Store, table string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
	return count
}

func requireAttemptActualCost(t *testing.T, attempt *PersonEnrichmentAttempt) int64 {
	t.Helper()
	require.NotNil(t, attempt.ActualCostUSDMicros)
	return *attempt.ActualCostUSDMicros
}

func TestClaimsWithIdentityScoreCopiesEveryEvidenceAndRejectsEmptyClaims(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	target := personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: "attribute:test", Revision: strings.Repeat("1", 64),
		UniversalID: "attribute:test", Slug: "test", Description: "Synthetic target",
		ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
	}
	claims := []personfacts.ProposedClaim{{
		Target: target, Relation: personfacts.RelationSupport, SubmittedValue: json.RawMessage(`"value"`),
		Origin:   personfacts.OriginEnrichment,
		Evidence: []personfacts.EvidenceInput{{IdentityScore: 999}, {IdentityScore: 1}},
	}}
	got, err := claimsWithIdentityScore(claims, 400)
	requirements.NoError(err)
	requirements.Len(got, 1)
	checks.Equal([]int{400, 400}, []int{
		got[0].Evidence[0].IdentityScore, got[0].Evidence[1].IdentityScore,
	})
	got[0].Evidence[0].Excerpt = "changed"
	checks.Empty(claims[0].Evidence[0].Excerpt)
	_, err = claimsWithIdentityScore(claims, -1)
	requirements.Error(err)
	claims[0].Evidence = nil
	_, err = claimsWithIdentityScore(claims, 400)
	requirements.Error(err)
}

func TestExternalEvidenceAlignerRejectsMismatchedAndUnsafeEvidence(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	aligner := externalEvidenceAligner{
		AttemptID: 42, ProviderRequestID: "request/id", ProviderJobID: "job:id",
		CitationKeys: map[string]struct{}{"citation-profile": {}},
	}
	subject := int64(7)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	public := personfacts.EvidenceInput{
		PersonID: 7, SubjectPersonID: &subject, SourceClass: personfacts.EvidencePublic,
		SourceRef: "citation-profile", SourceURL: "https://example.test/profile",
		Directness: personfacts.DirectOther, Authority: personfacts.AuthorityAuthoritative,
		EventTime: now, RecordedTime: now,
	}
	accepted, err := aligner.Align(t.Context(), public)
	requirements.NoError(err)
	checks.True(accepted.Accepted)
	provider := public
	provider.SourceClass = personfacts.EvidenceProviderAssertion
	provider.SourceURL = ""
	provider.SourceRef = "enrichment-attempt:42:job:job:id"
	provider.Directness = personfacts.Indirect
	provider.Authority = personfacts.AuthorityAggregator
	accepted, err = aligner.Align(t.Context(), provider)
	requirements.NoError(err)
	checks.True(accepted.Accepted)

	for _, mutate := range []func(*personfacts.EvidenceInput){
		func(v *personfacts.EvidenceInput) { v.SourceRef = "missing" },
		func(v *personfacts.EvidenceInput) { v.SourceURL = "http://example.test/profile" },
		func(v *personfacts.EvidenceInput) { v.SourceClass = personfacts.EvidenceSystem },
	} {
		candidate := public
		mutate(&candidate)
		result, alignErr := aligner.Align(t.Context(), candidate)
		requirements.NoError(alignErr)
		checks.False(result.Accepted)
		requirements.NotNil(result.Failure)
		checks.Equal(personfacts.DecisionInvalid, result.Failure.Action)
	}
}

func TestPersonEnrichmentResultMetadataStructsDoNotExposeSecretsOrRawIdentifiers(t *testing.T) {
	citation := PersonEnrichmentCitation{}
	encoded, err := json.Marshal(citation)
	require.NoError(t, err)
	for _, forbidden := range []string{"credential", "suppression_key", "raw_identifier", "normalized_identifier"} {
		assert.NotContains(t, string(encoded), forbidden)
	}
}
