package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func assertParticipantIdentifierClassification(
	t *testing.T,
	st *store.Store,
	identifierType, identifierValue, serviceSlug, scopeKind, scopeValue string,
) {
	t.Helper()
	var gotService, gotScopeKind, gotScopeValue sql.NullString
	err := st.DB().QueryRow(st.Rebind(`SELECT cs.slug, pi.scope_kind, pi.scope_value
		FROM participant_identifiers pi
		LEFT JOIN communication_services cs ON cs.id = pi.service_id
		WHERE pi.identifier_type = ? AND pi.identifier_value = ?`),
		identifierType, identifierValue,
	).Scan(&gotService, &gotScopeKind, &gotScopeValue)
	require.NoError(t, err)
	assert.Equal(t, serviceSlug, gotService.String, "service slug")
	assert.Equal(t, serviceSlug != "", gotService.Valid, "service presence")
	assert.Equal(t, scopeKind, gotScopeKind.String, "scope kind")
	assert.Equal(t, scopeKind != "", gotScopeKind.Valid, "scope-kind presence")
	assert.Equal(t, scopeValue, gotScopeValue.String, "scope value")
	assert.Equal(t, scopeValue != "", gotScopeValue.Valid, "scope-value presence")
}

func TestParticipantIdentifierWritePathsClassifyServiceAndScope(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	_, err := st.EnsureParticipantByPhone(
		"+15550100001", "Test User", "whatsapp",
	)
	require.NoError(err)
	assertParticipantIdentifierClassification(
		t, st, "whatsapp", "+15550100001", "whatsapp", "", "",
	)

	_, err = st.EnsureParticipantByIdentifier(
		"discord_user_id", "discord-user-1", "Test User",
	)
	require.NoError(err)
	assertParticipantIdentifierClassification(
		t, st, "discord_user_id", "discord-user-1", "discord", "", "",
	)

	participantID := f.EnsureParticipant(
		"slack-user@example.com", "Test User", "example.com",
	)
	require.NoError(st.SetParticipantIdentifier(
		participantID, "slack", "T-SYNTHETIC:U-SYNTHETIC",
	))
	assertParticipantIdentifierClassification(
		t, st, "slack", "T-SYNTHETIC:U-SYNTHETIC",
		"slack", "workspace", "T-SYNTHETIC",
	)

	for _, tc := range []struct {
		identifierType  string
		identifierValue string
		serviceSlug     string
		scopeKind       string
		scopeValue      string
	}{
		{"matrix", "@alice:matrix.example:8448", "matrix", "server", "matrix.example:8448"},
		{"synctech_sms", "22000", "sms", "", ""},
		{"google_voice", "+15550100002", "google-voice", "", ""},
		{"beeper", "@alice:beeper.local", "", "", ""},
		{"example-unknown", "alice", "", "", ""},
	} {
		_, err = st.EnsureParticipantByIdentifier(
			tc.identifierType, tc.identifierValue, "Test User",
		)
		require.NoError(err)
		assertParticipantIdentifierClassification(
			t, st, tc.identifierType, tc.identifierValue,
			tc.serviceSlug, tc.scopeKind, tc.scopeValue,
		)
	}
}

func TestParticipantIdentifierServiceScopeV2RepairsAlreadyMigratedRows(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	_, err := st.EnsureParticipantByIdentifier(
		"imessage", "+15550100003", "Test User",
	)
	require.NoError(err)
	_, err = st.EnsureParticipantByIdentifier(
		"slack", "T-REPAIR:U-REPAIR", "Test User",
	)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE participant_identifiers
		SET service_id = NULL, scope_kind = NULL, scope_value = NULL
		WHERE identifier_value IN ('+15550100003', 'T-REPAIR:U-REPAIR')`)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`),
		"participant_identifiers_service_scope_v2",
	)
	require.NoError(err)

	require.NoError(st.InitSchema())

	assertParticipantIdentifierClassification(
		t, st, "imessage", "+15550100003", "imessage", "", "",
	)
	assertParticipantIdentifierClassification(
		t, st, "slack", "T-REPAIR:U-REPAIR",
		"slack", "workspace", "T-REPAIR",
	)
	applied, err := st.IsMigrationApplied("participant_identifiers_service_scope_v2")
	require.NoError(err)
	assert.True(t, applied)
}
