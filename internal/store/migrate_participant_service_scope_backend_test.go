package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestParticipantIdentifiersServiceScopeLegacyTableUpgrade(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	participantID, err := st.EnsureParticipantByIdentifier(
		"imessage", "+12025550123", "Test User",
	)
	require.NoError(err)

	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(ctx, `ALTER TABLE participant_identifiers
			DROP COLUMN service_id,
			DROP COLUMN scope_kind,
			DROP COLUMN scope_value`)
		require.NoError(err)
	} else {
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
			_, err = st.DB().ExecContext(ctx, statement)
			require.NoError(err)
		}
	}

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
