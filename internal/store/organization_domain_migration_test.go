package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestInitSchemaCanonicalizesLegacyOrganizationDomains(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()

	first, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Unicode Domain", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	second, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Shared Domain", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	var personID int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id
	`), "organization-domain-migration-person").Scan(&personID))
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: personID, OrganizationID: first.ID, Source: store.ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: personID, OrganizationID: second.ID, Source: store.ProvenanceUser,
	})
	require.NoError(err)
	var vcardRevisionBefore int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT vcard_projection_revision FROM persons WHERE id = ?
	`), personID).Scan(&vcardRevisionBefore))
	legacyUpdatedAt := time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)

	_, err = st.DB().ExecContext(ctx, st.Rebind(`
		UPDATE organizations
		SET primary_domain = ?, updated_at = ?
		WHERE id IN (?, ?)
	`), "bücher.example", legacyUpdatedAt, first.ID, second.ID)
	require.NoError(err, "seed legacy Unicode primary domains")

	insertIdentifier := func(organizationID int64, value string) int64 {
		t.Helper()
		var id int64
		err := st.DB().QueryRowContext(ctx, st.Rebind(`
			INSERT INTO organization_identifiers (
				organization_id, identifier_kind, identifier_value,
				normalized_value, source
			) VALUES (?, 'domain', ?, ?, 'user')
			RETURNING id
		`), organizationID, value, value).Scan(&id)
		require.NoError(err)
		return id
	}

	unicodeID := insertIdentifier(first.ID, "bücher.example")
	asciiID := insertIdentifier(first.ID, "xn--bcher-kva.example")
	sharedUnicodeID := insertIdentifier(second.ID, "bücher.example")

	_, err = st.DB().ExecContext(ctx, st.Rebind(`
		DELETE FROM applied_migrations
		WHERE name = ?
	`), "organization_domain_idna_v1")
	require.NoError(err, "reset organization domain migration sentinel")

	require.NoError(st.InitSchemaContext(ctx), "upgrade legacy organization domains")
	var migrationApplied bool
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT EXISTS (
			SELECT 1 FROM applied_migrations WHERE name = ?
		)
	`), "organization_domain_idna_v1").Scan(&migrationApplied))
	assert.True(migrationApplied)

	for _, input := range []struct {
		id       int64
		revision int64
	}{{first.ID, first.Revision}, {second.ID, second.Revision}} {
		organizationID := input.id
		organization, getErr := st.GetOrganizationContext(ctx, organizationID)
		require.NoError(getErr)
		require.NotNil(organization.PrimaryDomain)
		assert.Equal("xn--bcher-kva.example", *organization.PrimaryDomain)
		assert.Equal(input.revision+1, organization.Revision)
		assert.True(organization.UpdatedAt.After(legacyUpdatedAt))
	}
	var vcardRevisionAfter int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT vcard_projection_revision FROM persons WHERE id = ?
	`), personID).Scan(&vcardRevisionAfter))
	assert.Equal(vcardRevisionBefore+1, vcardRevisionAfter,
		"one person employed by two affected organizations must be bumped once")

	type identifierState struct {
		normalizedValue string
		activeUntil     sql.NullTime
		supersededAt    sql.NullTime
	}
	readState := func(id int64) identifierState {
		t.Helper()
		var state identifierState
		err := st.DB().QueryRowContext(ctx, st.Rebind(`
			SELECT normalized_value, active_until, superseded_at
			FROM organization_identifiers
			WHERE id = ?
		`), id).Scan(&state.normalizedValue, &state.activeUntil, &state.supersededAt)
		require.NoError(err)
		return state
	}

	unicodeState := readState(unicodeID)
	asciiState := readState(asciiID)
	sharedState := readState(sharedUnicodeID)
	assert.Equal("xn--bcher-kva.example", unicodeState.normalizedValue)
	assert.True(unicodeState.activeUntil.Valid)
	assert.True(unicodeState.supersededAt.Valid)
	assert.Equal("xn--bcher-kva.example", asciiState.normalizedValue)
	assert.False(asciiState.activeUntil.Valid)
	assert.False(asciiState.supersededAt.Valid,
		"the already-canonical active identifier must win an in-organization collision")
	assert.Equal("xn--bcher-kva.example", sharedState.normalizedValue)
	assert.False(sharedState.activeUntil.Valid)
	assert.False(sharedState.supersededAt.Valid,
		"the same domain on another organization must remain active")

	var activeFirst, activeSecond int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT COUNT(*)
		FROM organization_identifiers
		WHERE organization_id = ? AND identifier_kind = 'domain'
		  AND active_until IS NULL AND superseded_at IS NULL
	`), first.ID).Scan(&activeFirst))
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT COUNT(*)
		FROM organization_identifiers
		WHERE organization_id = ? AND identifier_kind = 'domain'
		  AND active_until IS NULL AND superseded_at IS NULL
	`), second.ID).Scan(&activeSecond))
	assert.Equal(1, activeFirst)
	assert.Equal(1, activeSecond)

	require.NoError(st.InitSchemaContext(ctx), "repeat schema initialization")
	assert.Equal(unicodeState, readState(unicodeID),
		"the migration ledger must make the collision retirement idempotent")
	var repeatedVCardRevision int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`
		SELECT vcard_projection_revision FROM persons WHERE id = ?
	`), personID).Scan(&repeatedVCardRevision))
	assert.Equal(vcardRevisionAfter, repeatedVCardRevision)
}
