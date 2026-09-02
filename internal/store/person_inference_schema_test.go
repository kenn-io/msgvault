package store_test

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonInferenceProviderV2Schema(t *testing.T) {
	st := testutil.NewTestStore(t)
	profileColumns := liveTableColumns(t, st, "person_inference_profiles")
	for _, column := range []string{
		"auth_scheme", "credential_source", "credential_ref", "output_mode",
		"token_limit_parameter", "reasoning_effort", "reasoning_mode", "driver_version",
		"execution_boundary", "packet_renderer_policy", "program_fingerprint",
		"disclosed_packet_fields",
	} {
		assert.Contains(t, profileColumns, column)
	}
	assert.ElementsMatch(t, []string{
		"profile_fingerprint", "checked_at", "driver_version", "output_mode",
		"provider_request_id", "model_version",
	}, liveTableColumns(t, st, "person_inference_checks"))
}

// TestPersonInferenceProviderV2RemovesPreProfileRows covers an archive created
// by a development build before named protocol profiles existed: the new
// columns and check table appear, and the pre-profile row, which no current
// configuration can fingerprint, is removed rather than carried forward.
func TestPersonInferenceProviderV2RemovesPreProfileRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newUninitializedPersonInferenceMigrationStore(t)
	createdAtType := "DATETIME"
	if st.IsPostgreSQL() {
		createdAtType = "TIMESTAMPTZ"
	}

	_, err := st.DB().Exec(`
		CREATE TABLE person_inference_profiles (
			fingerprint TEXT PRIMARY KEY, provider_kind TEXT NOT NULL,
			endpoint TEXT NOT NULL, model TEXT NOT NULL, api_key_env TEXT NOT NULL,
			allow_anonymous BOOLEAN NOT NULL DEFAULT FALSE,
			retention_posture TEXT NOT NULL, training_posture TEXT NOT NULL,
			allowed_sources JSON NOT NULL, source_since TEXT NOT NULL, source_until TEXT,
			allow_sensitive BOOLEAN NOT NULL DEFAULT FALSE, policy_json JSON NOT NULL,
			created_at ` + createdAtType + ` NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO person_inference_profiles
			(fingerprint, provider_kind, endpoint, model, api_key_env,
			 retention_posture, training_posture, allowed_sources, source_since, policy_json)
		VALUES
			('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			 'openai_compatible', 'https://api.example.test/v1', 'legacy-model', 'LEGACY_KEY',
			 'zero_retention', 'no_training', '["conversation_text"]', '2025-01-01', '{}')`)
	require.NoError(err)
	require.NoError(st.InitSchema())

	assert.Contains(liveTableColumns(t, st, "person_inference_profiles"), "driver_version")
	assert.Contains(liveTableColumns(t, st, "person_inference_profiles"), "credential_ref")
	assert.ElementsMatch([]string{
		"profile_fingerprint", "checked_at", "driver_version", "output_mode",
		"provider_request_id", "model_version",
	}, liveTableColumns(t, st, "person_inference_checks"))
	var profiles, checks, consents int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM person_inference_profiles`).Scan(&profiles))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM person_inference_checks`).Scan(&checks))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM person_inference_consents`).Scan(&consents))
	assert.Zero(profiles)
	assert.Zero(checks)
	assert.Zero(consents)
}

func newUninitializedPersonInferenceMigrationStore(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dbURL) {
		st, err := store.OpenForTest(filepath.Join(t.TempDir(), "legacy.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, st.Close()) })
		return st
	}

	admin, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	require.NoError(t, admin.Ping())
	var entropy [8]byte
	_, err = rand.Read(entropy[:])
	require.NoError(t, err)
	schema := "msgvault_task3_" + hex.EncodeToString(entropy[:])
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)

	parsed, err := url.Parse(dbURL)
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	st, err := store.OpenForTest(parsed.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, st.Close())
		_, dropErr := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, dropErr)
		require.NoError(t, admin.Close())
	})
	return st
}

func TestPersonInferenceConsentSchemaEnforcesAuditState(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	profile := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)

	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by, revoked_by)
		VALUES (?, 'cli', 'cli')`), profile.Fingerprint)
	require.Error(err, "revocation actor without timestamp must fail")

	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'cli')`), profile.Fingerprint)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'second')`), profile.Fingerprint)
	require.Error(err, "only one active consent is allowed")

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE person_inference_consents
		SET revoked_by = 'cli', revoked_at = CURRENT_TIMESTAMP
		WHERE profile_fingerprint = ? AND revoked_at IS NULL`), profile.Fingerprint)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO person_inference_consents
			(profile_fingerprint, granted_by)
		VALUES (?, 'second')`), profile.Fingerprint)
	require.NoError(err, "a revoked consent must not block regrant")
}
