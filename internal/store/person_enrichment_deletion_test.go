package store

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestDeletePersonWithEnrichmentSuppressionsCopiesDigestsAndCascades(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentDeletionFixture(t)

	err := f.store.DeletePersonWithEnrichmentSuppressionsContext(t.Context(), DeletePersonEnrichmentInput{
		PersonID: f.person.ID, ExpectedRevision: f.person.Revision,
		Actor: "test", Reason: PersonEnrichmentSuppressionDeletion,
		CurrentIdentifiers: []PersonEnrichmentSuppressionInput{f.returned[1]},
	})
	requirements.NoError(err)

	_, err = f.store.GetPersonContext(t.Context(), f.person.ID)
	requirements.ErrorIs(err, ErrPersonNotFound)
	for _, digest := range append([]PersonEnrichmentSuppressionInput{f.disclosed}, f.returned...) {
		found, lookupErr := f.store.HasPersonEnrichmentSuppressionContext(
			t.Context(), deletionSuppressionLookup(digest))
		requirements.NoError(lookupErr)
		checks.True(found, "%s", digest.IdentifierClass)
	}
	for _, table := range []string{
		"person_enrichment_attempts", "person_enrichment_attempt_identifiers",
		"person_enrichment_provider_identities", "person_enrichment_citations",
		"person_fact_claims", "person_fact_decisions",
	} {
		checks.Equal(int64(0), enrichmentTableCount(t, f.store, table), table)
	}
	checks.Equal(int64(3), enrichmentTableCount(t, f.store, "person_enrichment_suppressions"))
}

func TestDeletePersonWithEnrichmentSuppressionsRollsBackOnSuppressionFailure(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentDeletionFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), `
		CREATE TRIGGER fail_person_enrichment_suppression
		BEFORE INSERT ON person_enrichment_suppressions
		BEGIN SELECT RAISE(ABORT, 'synthetic suppression failure'); END`)
	requirements.NoError(err)

	err = f.store.DeletePersonWithEnrichmentSuppressionsContext(t.Context(), DeletePersonEnrichmentInput{
		PersonID: f.person.ID, ExpectedRevision: f.person.Revision,
		Actor: "test", Reason: PersonEnrichmentSuppressionDeletion,
		CurrentIdentifiers: []PersonEnrichmentSuppressionInput{f.returned[1]},
	})
	requirements.ErrorContains(err, "synthetic suppression failure")

	person, getErr := f.store.GetPersonContext(t.Context(), f.person.ID)
	requirements.NoError(getErr)
	checks.Equal(f.person.Revision, person.Revision)
	checks.Positive(enrichmentTableCount(t, f.store, "person_enrichment_attempts"))
	checks.Positive(enrichmentTableCount(t, f.store, "person_enrichment_provider_identities"))
	checks.Positive(enrichmentTableCount(t, f.store, "person_fact_claims"))
	checks.Zero(enrichmentTableCount(t, f.store, "person_enrichment_suppressions"))
}

func TestDeletePersonContextCopiesPreviouslyRecordedEnrichmentDigests(t *testing.T) {
	f := newEnrichmentDeletionFixture(t)
	require.NoError(t, f.store.DeletePersonContext(t.Context(), f.person.ID, f.person.Revision))

	for _, digest := range append([]PersonEnrichmentSuppressionInput{f.disclosed}, f.returned...) {
		found, err := f.store.HasPersonEnrichmentSuppressionContext(t.Context(), deletionSuppressionLookup(digest))
		require.NoError(t, err)
		assert.True(t, found, "%s", digest.IdentifierClass)
	}
}

func TestPersonEnrichmentReimportRemainsSuppressedAfterDeletion(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentDeletionFixture(t)
	requirements.NoError(f.store.DeletePersonWithEnrichmentSuppressionsContext(
		t.Context(), DeletePersonEnrichmentInput{
			PersonID: f.person.ID, ExpectedRevision: f.person.Revision,
			Actor: "test", Reason: PersonEnrichmentSuppressionDeletion,
		}))

	participantID, err := f.store.EnsureParticipantContext(
		t.Context(), "PERSON@example.com", "Reimported Person", "example.com")
	requirements.NoError(err)
	person, created, err := f.store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	checks.True(created)
	checks.NotEqual(f.person.ID, person.ID)

	gate, err := personenrichment.NewEgressGate(f.store, f.store, f.hasher,
		func(string) (string, bool) { return "unused-test-credential", true })
	requirements.NoError(err)
	_, err = gate.Authorize(t.Context(), personenrichment.EgressInput{
		Profile: f.profile,
		Request: personenrichment.Request{Identity: personenrichment.Identity{
			Email: "person@example.com",
		}},
	})
	requirements.ErrorIs(err, personenrichment.ErrSuppressed)
}

type enrichmentDeletionFixture struct {
	store     *Store
	person    *Person
	profile   personenrichment.ProviderProfile
	hasher    *personenrichment.SuppressionHasher
	disclosed PersonEnrichmentSuppressionInput
	returned  []PersonEnrichmentSuppressionInput
}

func newEnrichmentDeletionFixture(t *testing.T) enrichmentDeletionFixture {
	t.Helper()
	f := newEnrichmentResultFixture(t)
	outcome, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	require.NoError(t, err)
	require.Equal(t, personenrichment.ClaimApplied, outcome.Status)

	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x6a}, 32))
	require.NoError(t, err)
	disclosedDigest := hasher.Digest(f.profile.ProviderNamespace,
		personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
		"person@example.com")
	disclosed := deletionSuppressionInput(disclosedDigest)

	person, err := f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(t, err)
	run, created, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "deletion-disclosed-attempt", RequestedAt: f.now.Add(time.Minute),
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, f.store.PutPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkInput{
		PersonID: person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "manual:deletion"},
		DueAt:   f.now.Add(time.Minute),
	}))
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "deletion-worker", ProviderName: f.profile.Name,
		Now: f.now.Add(time.Minute), LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	_, created, err = f.store.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: person.ID, ProfileFingerprint: f.profile.Fingerprint,
		PayloadHash: strings.Repeat("7", 64), RequestHash: strings.Repeat("8", 64),
		PersonRevision: person.Revision, Trigger: lease.Trigger,
		DisclosedIdentifiers: []personenrichment.SuppressionDigest{disclosedDigest},
	})
	require.NoError(t, err)
	require.True(t, created)

	sealed, err := f.commit.VerifiedReturnedIdentifierDigests()
	require.NoError(t, err)
	returned := make([]PersonEnrichmentSuppressionInput, len(sealed))
	for i := range sealed {
		returned[i] = deletionSuppressionInput(sealed[i])
	}
	person, err = f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(t, err)
	return enrichmentDeletionFixture{
		store: f.store, person: person, profile: f.profile, hasher: hasher,
		disclosed: disclosed, returned: returned,
	}
}

func deletionSuppressionInput(digest personenrichment.SuppressionDigest) PersonEnrichmentSuppressionInput {
	return PersonEnrichmentSuppressionInput{
		ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
		NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID,
		Digest: append([]byte(nil), digest.Digest...),
		Reason: PersonEnrichmentSuppressionDeletion, Actor: "test",
	}
}

func deletionSuppressionLookup(input PersonEnrichmentSuppressionInput) PersonEnrichmentSuppressionLookup {
	return PersonEnrichmentSuppressionLookup{
		ProviderNamespace: input.ProviderNamespace, IdentifierClass: input.IdentifierClass,
		NormalizationVersion: input.NormalizationVersion, KeyID: input.KeyID,
		Digest: append([]byte(nil), input.Digest...),
	}
}

func TestDeletePersonWithEnrichmentSuppressionsRejectsInvalidInputWithoutMutation(t *testing.T) {
	f := newEnrichmentDeletionFixture(t)
	input := f.returned[0]
	input.Digest = []byte("not-a-digest")
	err := f.store.DeletePersonWithEnrichmentSuppressionsContext(t.Context(), DeletePersonEnrichmentInput{
		PersonID: f.person.ID, ExpectedRevision: f.person.Revision,
		Actor: "test", Reason: PersonEnrichmentSuppressionDeletion,
		CurrentIdentifiers: []PersonEnrichmentSuppressionInput{input},
	})
	require.Error(t, err)
	_, err = f.store.GetPersonContext(t.Context(), f.person.ID)
	assert.NotErrorIs(t, err, ErrPersonNotFound)
}

func TestDeletePersonWithEnrichmentSuppressionsRejectsRecordedAttemptKeyMismatch(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentDeletionFixture(t)
	configuredHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x4e}, 32))
	requirements.NoError(err)
	configuredKeyID, err := configuredHasher.KeyID()
	requirements.NoError(err)
	err = f.store.DeletePersonWithEnrichmentSuppressionsContext(t.Context(), DeletePersonEnrichmentInput{
		PersonID: f.person.ID, ExpectedRevision: f.person.Revision,
		Actor: "test", Reason: PersonEnrichmentSuppressionDeletion,
		ConfiguredKeyID: configuredKeyID,
	})
	requirements.ErrorIs(err, personenrichment.ErrSuppressionKeyMismatch)
	person, getErr := f.store.GetPersonContext(t.Context(), f.person.ID)
	requirements.NoError(getErr)
	checks.Equal(f.person.Revision, person.Revision)
	checks.Positive(enrichmentTableCount(t, f.store, "person_enrichment_attempts"))
	checks.Positive(enrichmentTableCount(t, f.store, "person_enrichment_provider_identities"))
	checks.Zero(enrichmentTableCount(t, f.store, "person_enrichment_suppressions"))
}
