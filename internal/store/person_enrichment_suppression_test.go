package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func enrichmentSuppressionInput() store.PersonEnrichmentSuppressionInput {
	return store.PersonEnrichmentSuppressionInput{
		ProviderNamespace:    "exa:" + string(bytes.Repeat([]byte{'a'}, sha256.Size*2)),
		IdentifierClass:      personenrichment.SuppressionEmail,
		NormalizationVersion: personenrichment.EmailNormalizationV1,
		KeyID:                string(bytes.Repeat([]byte{'b'}, sha256.Size*2)),
		Digest:               bytes.Repeat([]byte{0x42}, sha256.Size),
		Reason:               store.PersonEnrichmentSuppressionDeletion,
		Actor:                "privacy-admin",
	}
}

func suppressionLookup(input store.PersonEnrichmentSuppressionInput) store.PersonEnrichmentSuppressionLookup {
	return store.PersonEnrichmentSuppressionLookup{
		ProviderNamespace:    input.ProviderNamespace,
		IdentifierClass:      input.IdentifierClass,
		NormalizationVersion: input.NormalizationVersion,
		KeyID:                input.KeyID,
		Digest:               append([]byte(nil), input.Digest...),
	}
}

func TestPersonEnrichmentSuppressionSurvivesProductionPersonDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	participantID, err := st.EnsureParticipantContext(
		t.Context(), "person@example.com", "Test Person", "example.com")
	require.NoError(err)
	person, created, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)
	assert.True(created)

	input := enrichmentSuppressionInput()
	require.NoError(st.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{input}))
	require.NoError(st.DeletePersonContext(t.Context(), person.ID, person.Revision))

	found, err := st.HasPersonEnrichmentSuppressionContext(t.Context(), suppressionLookup(input))
	require.NoError(err)
	assert.True(found)

	var people, suppressions int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM persons WHERE id = ?`), person.ID).Scan(&people))
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_enrichment_suppressions`).Scan(&suppressions))
	assert.Zero(people)
	assert.Equal(1, suppressions)
}

func TestPersonEnrichmentSuppressionIsProviderScopedAndMetadataImmutable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	input := enrichmentSuppressionInput()

	require.NoError(st.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{input}))
	require.NoError(st.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{input}))

	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_enrichment_suppressions`).Scan(&count))
	assert.Equal(1, count, "the exact unique tuple must be idempotent")

	differentProvider := suppressionLookup(input)
	differentProvider.ProviderNamespace = "sixtyfour:" + string(bytes.Repeat([]byte{'a'}, sha256.Size*2))
	found, err := st.HasPersonEnrichmentSuppressionContext(t.Context(), differentProvider)
	require.NoError(err)
	assert.False(found)

	otherNamespace := input
	otherNamespace.ProviderNamespace = differentProvider.ProviderNamespace
	require.NoError(st.InsertPersonEnrichmentSuppressionsContext(
		t.Context(), []store.PersonEnrichmentSuppressionInput{otherNamespace}))
	found, err = st.HasPersonEnrichmentSuppressionContext(t.Context(), differentProvider)
	require.NoError(err)
	assert.True(found)

	colliding := input
	colliding.Reason = store.PersonEnrichmentSuppressionOptOut
	err = st.InsertPersonEnrichmentSuppressionsContext(
		t.Context(), []store.PersonEnrichmentSuppressionInput{colliding})
	require.ErrorContains(err, "different metadata")

	colliding = input
	colliding.Actor = "other-admin"
	err = st.InsertPersonEnrichmentSuppressionsContext(
		t.Context(), []store.PersonEnrichmentSuppressionInput{colliding})
	require.ErrorContains(err, "different metadata")
}

func TestPersonEnrichmentSuppressionListRedactsAndBoundsPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	input := enrichmentSuppressionInput()
	require.NoError(st.InsertPersonEnrichmentSuppressionsContext(t.Context(), []store.PersonEnrichmentSuppressionInput{input}))

	rows, err := st.ListPersonEnrichmentSuppressionsContext(t.Context(), store.PersonEnrichmentSuppressionFilter{
		ProviderNamespace: input.ProviderNamespace,
		IdentifierClass:   input.IdentifierClass,
		KeyID:             input.KeyID,
		Limit:             1,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(input.ProviderNamespace, rows[0].ProviderNamespace)
	assert.Equal(input.IdentifierClass, rows[0].IdentifierClass)
	assert.Equal(input.NormalizationVersion, rows[0].NormalizationVersion)
	assert.Equal(input.KeyID, rows[0].KeyID)
	assert.Equal(hex.EncodeToString(input.Digest[:6]), rows[0].DigestPrefix)
	assert.Equal(input.Reason, rows[0].Reason)
	assert.Equal(input.Actor, rows[0].Actor)
	assert.False(rows[0].CreatedAt.IsZero())

	encoded, err := json.Marshal(rows[0])
	require.NoError(err)
	assert.NotContains(string(encoded), hex.EncodeToString(input.Digest))
	assert.NotContains(string(encoded), "person@example.com")

	for _, limit := range []int{0, 201, -1} {
		_, err = st.ListPersonEnrichmentSuppressionsContext(
			t.Context(), store.PersonEnrichmentSuppressionFilter{Limit: limit})
		require.ErrorContains(err, "limit")
	}
	for _, limit := range []int{1, 200} {
		_, err = st.ListPersonEnrichmentSuppressionsContext(
			t.Context(), store.PersonEnrichmentSuppressionFilter{Limit: limit})
		require.NoError(err)
	}
}

func TestPersonEnrichmentSuppressionRejectsUnsafeInputs(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	valid := enrichmentSuppressionInput()

	for name, mutate := range map[string]func(*store.PersonEnrichmentSuppressionInput){
		"namespace": func(input *store.PersonEnrichmentSuppressionInput) { input.ProviderNamespace = "" },
		"class": func(input *store.PersonEnrichmentSuppressionInput) {
			input.IdentifierClass = personenrichment.SuppressionIdentifierClass("name")
		},
		"normalization": func(input *store.PersonEnrichmentSuppressionInput) { input.NormalizationVersion = "" },
		"key ID":        func(input *store.PersonEnrichmentSuppressionInput) { input.KeyID = "secret-key" },
		"digest":        func(input *store.PersonEnrichmentSuppressionInput) { input.Digest = []byte{0x42} },
		"reason":        func(input *store.PersonEnrichmentSuppressionInput) { input.Reason = "unknown" },
		"actor":         func(input *store.PersonEnrichmentSuppressionInput) { input.Actor = " " },
	} {
		t.Run(name, func(t *testing.T) {
			input := valid
			mutate(&input)
			err := st.InsertPersonEnrichmentSuppressionsContext(
				t.Context(), []store.PersonEnrichmentSuppressionInput{input})
			require.Error(err)
		})
	}
}

var _ personenrichment.SuppressionChecker = (*store.Store)(nil)

func TestPersonEnrichmentSuppressionKeyIDsIncludeAttemptIdentifierKeys(t *testing.T) {
	requirements := require.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "gate-union-key-state")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "gate-union-worker")
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x33}, 32))
	requirements.NoError(err)
	digest := hasher.Digest(f.profile.ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "gate-union@example.test")
	start := testAttemptStart(&f, run.ID, "e")
	start.DisclosedIdentifiers = []personenrichment.SuppressionDigest{digest}
	_, _, err = f.store.BeginAttempt(t.Context(), lease.Token, start)
	requirements.NoError(err)

	// No suppression rows exist; the durable attempt identifiers under the
	// disclosed key are the whole durable key state and must be visible to
	// the pre-egress fail-closed check.
	keyIDs, err := f.store.ListPersonEnrichmentSuppressionKeyIDsContext(t.Context())
	requirements.NoError(err)
	keyID, err := hasher.KeyID()
	requirements.NoError(err)
	assert.Equal(t, []string{keyID}, keyIDs)
}
