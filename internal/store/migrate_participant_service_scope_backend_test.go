package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func rebuildLegacyParticipantIdentifiers(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := t.Context()
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(ctx, `ALTER TABLE participant_identifiers
			DROP COLUMN service_id,
			DROP COLUMN scope_kind,
			DROP COLUMN scope_value`)
		require.NoError(t, err)
		return
	}
	for _, statement := range []string{
		`CREATE TABLE participant_identifiers_legacy (
			id INTEGER PRIMARY KEY,
			participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
			identifier_type TEXT NOT NULL,
			identifier_value TEXT NOT NULL,
			display_value TEXT,
			is_primary BOOLEAN DEFAULT FALSE,
			UNIQUE(identifier_type, identifier_value)
		)`,
		`INSERT INTO participant_identifiers_legacy (
			id, participant_id, identifier_type, identifier_value, display_value, is_primary
		) SELECT id, participant_id, identifier_type, identifier_value, display_value, is_primary
		  FROM participant_identifiers`,
		`DROP TABLE participant_identifiers`,
		`ALTER TABLE participant_identifiers_legacy RENAME TO participant_identifiers`,
	} {
		_, err := st.DB().ExecContext(ctx, statement)
		require.NoError(t, err)
	}
}

func installRejectParticipantIdentifierWrite(t *testing.T, st *store.Store) {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `ALTER TABLE participant_identifiers
			ADD CONSTRAINT participant_identifiers_reject_test_values
			CHECK (identifier_value NOT IN ('legacy-reject', '+15550009999'))`)
		require.NoError(t, err)
		return
	}
	_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_participant_identifier_test_values
		BEFORE INSERT ON participant_identifiers
		WHEN NEW.identifier_value IN ('legacy-reject', '+15550009999')
		BEGIN
			SELECT RAISE(ABORT, 'rejected participant identifier test value');
		END`)
	require.NoError(t, err)
}

func TestParticipantIdentifiersServiceScopeLegacyTableUpgrade(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	participantID, err := st.EnsureParticipantByIdentifier(
		"imessage", "+12025550123", "Test User",
	)
	require.NoError(err)

	rebuildLegacyParticipantIdentifiers(t, st)

	_, err = st.DB().ExecContext(ctx, st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`),
		"participant_identifiers_service_scope_v1",
	)
	require.NoError(err)

	require.NoError(st.InitSchemaContext(ctx))

	var serviceSlug sql.NullString
	err = st.DB().QueryRowContext(ctx, st.Rebind(`SELECT cs.slug
		FROM participant_identifiers pi
		LEFT JOIN communication_services cs ON cs.id = pi.service_id
		WHERE pi.participant_id = ? AND pi.identifier_type = 'imessage'`), participantID,
	).Scan(&serviceSlug)
	require.NoError(err)
	assert.Equal("imessage", serviceSlug.String)
	assert.True(serviceSlug.Valid)

	_, err = st.DB().ExecContext(ctx, `DELETE FROM communication_services WHERE slug = 'imessage'`)
	require.NoError(err)
	var serviceID sql.NullInt64
	err = st.DB().QueryRowContext(ctx, st.Rebind(`SELECT service_id
		FROM participant_identifiers
		WHERE participant_id = ? AND identifier_type = 'imessage'`), participantID,
	).Scan(&serviceID)
	require.NoError(err)
	assert.False(serviceID.Valid, "deleting the service must retain and unclassify the identifier")
}

func TestEnsureParticipantCreationSupportsLegacyIdentifierColumns(t *testing.T) {
	tests := []struct {
		name           string
		identifierType string
		identifier     string
		serviceSlug    string
		ensure         func(*store.Store) (int64, error)
	}{
		{
			name:           "phone",
			identifierType: "whatsapp",
			identifier:     "+15550001111",
			serviceSlug:    "whatsapp",
			ensure: func(st *store.Store) (int64, error) {
				return st.EnsureParticipantByPhone(
					"+15550001111", "Legacy Phone", "whatsapp",
				)
			},
		},
		{
			name:           "generic identifier",
			identifierType: "matrix",
			identifier:     "@legacy:example.test",
			serviceSlug:    "matrix",
			ensure: func(st *store.Store) (int64, error) {
				return st.EnsureParticipantByIdentifier(
					"matrix", "@legacy:example.test", "Legacy Matrix",
				)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := storetest.New(t).Store
			rebuildLegacyParticipantIdentifiers(t, st)

			participantID, err := tt.ensure(st)
			require.NoError(err)
			var storedParticipantID int64
			err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
				SELECT participant_id FROM participant_identifiers
				WHERE identifier_type = ? AND identifier_value = ?
			`), tt.identifierType, tt.identifier).Scan(&storedParticipantID)
			require.NoError(err)
			assert.Equal(participantID, storedParticipantID)

			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
				DELETE FROM applied_migrations WHERE name IN (?, ?)
			`),
				"participant_identifiers_service_scope_v1",
				"participant_identifiers_service_scope_v2",
			)
			require.NoError(err)
			require.NoError(st.InitSchemaContext(t.Context()))
			var serviceSlug sql.NullString
			err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
				SELECT cs.slug
				FROM participant_identifiers pi
				LEFT JOIN communication_services cs ON cs.id = pi.service_id
				WHERE pi.identifier_type = ? AND pi.identifier_value = ?
			`), tt.identifierType, tt.identifier).Scan(&serviceSlug)
			require.NoError(err)
			assert.Equal(tt.serviceSlug, serviceSlug.String)
			assert.True(serviceSlug.Valid)
		})
	}
}

func TestEnsureParticipantCreationRollsBackWhenIdentifierWriteFails(t *testing.T) {
	tests := []struct {
		name   string
		ensure func(*store.Store) (int64, error)
	}{
		{
			name: "phone",
			ensure: func(st *store.Store) (int64, error) {
				return st.EnsureParticipantByPhone(
					"+15550009999", "Rejected Phone", "whatsapp",
				)
			},
		},
		{
			name: "generic identifier",
			ensure: func(st *store.Store) (int64, error) {
				return st.EnsureParticipantByIdentifier(
					"matrix", "legacy-reject", "Rejected Matrix",
				)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := storetest.New(t).Store
			rebuildLegacyParticipantIdentifiers(t, st)
			installRejectParticipantIdentifierWrite(t, st)
			var before int
			require.NoError(st.DB().QueryRowContext(
				t.Context(), `SELECT COUNT(*) FROM participants`,
			).Scan(&before))

			_, err := tt.ensure(st)
			require.Error(err)
			var after int
			require.NoError(st.DB().QueryRowContext(
				t.Context(), `SELECT COUNT(*) FROM participants`,
			).Scan(&after))
			assert.Equal(before, after, "failed identifier write must not leave a participant")
		})
	}
}
