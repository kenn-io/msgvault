package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonMatchConsentQuestionVersionLegacyMigration(t *testing.T) {
	st := testutil.NewTestStore(t)
	drop := `ALTER TABLE person_match_consents DROP COLUMN question_version`
	if st.IsPostgreSQL() {
		drop += ` CASCADE`
	}
	_, err := st.DB().Exec(drop)
	require.NoError(t, err)

	require.NoError(t, st.InitSchema())
	disclosure := personmatch.Disclosure{Endpoint: personmatch.Endpoint, ModelID: personmatch.ModelID,
		PacketSchema: personmatch.PacketSchema, RetentionDeclaration: "operator-confirmed-retention-v1",
		PolicyVersion: personmatch.PolicyVersion, QuestionVersion: personmatch.QuestionVersion}
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), disclosure, "operator")
	require.NoError(t, err)
}

func TestPersonMatchConsentExactDisclosureAndRevocation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	disclosure := personmatch.Disclosure{Endpoint: "https://api.typesafe.ai/v1/systemone",
		ModelID: "jev-1.13.0", PacketSchema: "person-match-packet-v1",
		RetentionDeclaration: "operator-confirmed-retention-v1", PolicyVersion: "person-match-policy-v1",
		QuestionVersion: personmatch.QuestionVersion}
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(err)
	active, err := st.HasPersonMatchConsentContext(t.Context(), fingerprint)
	require.NoError(err)
	assert.False(active)

	grant, created, err := st.GrantPersonMatchConsentContext(t.Context(), disclosure, "operator")
	require.NoError(err)
	assert.True(created)
	assert.Equal(fingerprint, grant.DisclosureFingerprint)
	active, err = st.HasPersonMatchConsentContext(t.Context(), fingerprint)
	require.NoError(err)
	assert.True(active)

	for _, change := range []func(*personmatch.Disclosure){
		func(d *personmatch.Disclosure) { d.Endpoint += "/changed" },
		func(d *personmatch.Disclosure) { d.ModelID = "jev-1.13.1" },
		func(d *personmatch.Disclosure) { d.PacketSchema += "-changed" },
		func(d *personmatch.Disclosure) { d.RetentionDeclaration += "-changed" },
		func(d *personmatch.Disclosure) { d.PolicyVersion += "-changed" },
		func(d *personmatch.Disclosure) { d.QuestionVersion = "same_person_v2" },
	} {
		changed := disclosure
		change(&changed)
		other, err := changed.Fingerprint()
		require.NoError(err)
		active, err := st.HasPersonMatchConsentContext(t.Context(), other)
		require.NoError(err)
		assert.False(active)
	}

	again, created, err := st.GrantPersonMatchConsentContext(t.Context(), disclosure, "operator")
	require.NoError(err)
	assert.False(created)
	assert.Equal(grant.ID, again.ID)

	revoked, err := st.RevokePersonMatchConsentContext(t.Context(), fingerprint, "operator")
	require.NoError(err)
	assert.True(revoked)
	active, err = st.HasPersonMatchConsentContext(t.Context(), fingerprint)
	require.NoError(err)
	assert.False(active)
	regrant, created, err := st.GrantPersonMatchConsentContext(t.Context(), disclosure, "operator")
	require.NoError(err)
	assert.True(created)
	assert.NotEqual(grant.ID, regrant.ID)
	var preserved int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT 1 FROM person_match_consents WHERE id = ? AND revoked_at IS NOT NULL`), grant.ID).Scan(&preserved))
	assert.Equal(1, preserved)
}

func TestPersonMatchConsentRejectsInvalidInputs(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.HasPersonMatchConsentContext(t.Context(), "not-a-fingerprint")
	require.Error(t, err)
	_, err = st.RevokePersonMatchConsentContext(t.Context(), "not-a-fingerprint", "operator")
	require.Error(t, err)
	_, _, err = st.GrantPersonMatchConsentContext(t.Context(), personmatch.Disclosure{}, "operator")
	require.Error(t, err)
}
