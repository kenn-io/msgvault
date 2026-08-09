package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParticipantIdentifiersServiceScopeBackfillClassifiesLegacyIdentifiers(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite file-path migration test")
	}
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "legacy.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	for _, identifier := range []struct{ kind, value string }{
		{"email", "alice@example.com"},
		{"phone", "+12025550123"},
		{"imessage", "+12025550124"},
		{"matrix", "@alice:example.org"},
		{"example-unknown", "alice"},
	} {
		_, err := st.EnsureParticipantByIdentifier(
			identifier.kind, identifier.value, "Alice Example",
		)
		require.NoError(err)
	}
	_, err = st.db.Exec(`UPDATE participant_identifiers
		SET service_id = NULL, scope_kind = NULL, scope_value = NULL`)
	require.NoError(err)
	_, err = st.db.Exec(
		`DELETE FROM applied_migrations WHERE name = ?`, migrationParticipantServiceScope,
	)
	require.NoError(err)
	require.NoError(st.InitSchema())
	classified, err := st.classifiedIdentifierServiceSlugs(context.Background())
	require.NoError(err)
	assert.Equal("imessage", classified["imessage:+12025550124"])
	assert.Equal("matrix", classified["matrix:@alice:example.org"])
	assert.Empty(classified["email:alice@example.com"])
	assert.Empty(classified["example-unknown:alice"])
	applied, err := st.IsMigrationApplied(migrationParticipantServiceScope)
	require.NoError(err)
	assert.True(applied)
}

func TestInitSchemaContext_ParticipantIdentifiersServiceScopeBackfillStopsWhenContextIsCancelled(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite file-path migration test")
	}
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "cancelled-backfill.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	_, err = st.EnsureParticipantByIdentifier(
		"imessage", "+12025550125", "Cancellation Test",
	)
	require.NoError(err)
	_, err = st.db.Exec(`UPDATE participant_identifiers SET service_id = NULL
		WHERE identifier_type = 'imessage' AND identifier_value = '+12025550125'`)
	require.NoError(err)

	_, err = st.db.Exec(
		`DELETE FROM applied_migrations WHERE name IN (?, ?)`,
		migrationParticipantServiceScope, migrationParticipantServiceScopeV2,
	)
	require.NoError(err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	trigger := cancelAtStatement{
		stop:   "UPDATE participant_identifiers",
		cancel: cancel,
	}
	trigger.install(st.db)

	err = st.InitSchemaContext(ctx)

	require.True(trigger.fired, "the service-scope backfill was never reached")
	require.Error(err, "a cancelled backfill must stop schema initialization")
	require.ErrorIs(err, context.Canceled)
	assert.Contains(err.Error(), "classify participant identifier service scope",
		"the cancellation must stop the backfill itself, not a later upgrade step")
	applied, ledgerErr := st.IsMigrationApplied(migrationParticipantServiceScope)
	require.NoError(ledgerErr)
	assert.False(applied, "a cancelled backfill must remain pending for the next startup")
}

func TestInitSchemaContext_CommunicationServiceSeedStopsWhenContextIsCancelled(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite statement interception test")
	}
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "cancelled-seed.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	_, err = st.db.Exec(
		`DELETE FROM applied_migrations WHERE name = ?`, communicationServicesSeedV1,
	)
	require.NoError(err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	trigger := cancelAtStatement{
		stop:   "INTO communication_services",
		cancel: cancel,
	}
	trigger.install(st.db)

	err = st.InitSchemaContext(ctx)

	require.True(trigger.fired, "the communication-service seed was never reached")
	require.Error(err, "a cancelled seed must stop schema initialization")
	require.ErrorIs(err, context.Canceled)
	assert.Contains(err.Error(), "seed communication services",
		"the cancellation must stop the seed itself, not a later upgrade step")
	applied, ledgerErr := st.IsMigrationApplied(communicationServicesSeedV1)
	require.NoError(ledgerErr)
	assert.False(applied, "a cancelled seed must remain pending for the next startup")
}
