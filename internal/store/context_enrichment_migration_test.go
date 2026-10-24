package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestContextEnrichmentMigrationPreservesProfilesAndCounters(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	// Recreate the previous release's constraint before adding real profile and
	// consent data. The production migration must preserve inbound references.
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(ctx, `ALTER TABLE person_enrichment_profiles DROP CONSTRAINT person_enrichment_profiles_provider_kind_check, ADD CONSTRAINT person_enrichment_profiles_provider_kind_check CHECK (provider_kind IN ('exa','sixtyfour'))`)
		requirements.NoError(err)
	} else {
		conn, err := st.DB().Conn(ctx)
		requirements.NoError(err)
		var definition string
		requirements.NoError(conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='person_enrichment_profiles'`).Scan(&definition))
		definition = strings.Replace(definition, "'exa', 'sixtyfour', 'context'", "'exa', 'sixtyfour'", 1)
		definition = strings.Replace(definition, "person_enrichment_profiles", "legacy_enrichment_profiles", 1)
		_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`)
		requirements.NoError(err)
		for _, statement := range []string{definition, `DROP TABLE person_enrichment_profiles`, `ALTER TABLE legacy_enrichment_profiles RENAME TO person_enrichment_profiles`, `PRAGMA foreign_keys=ON`} {
			_, err = conn.ExecContext(ctx, statement)
			requirements.NoError(err)
		}
		requirements.NoError(conn.Close())
	}
	profile := enrichmentTestProfile(t)
	_, err := st.EnsurePersonEnrichmentProfile(ctx, profile)
	requirements.NoError(err)
	_, _, err = st.GrantPersonEnrichmentConsent(ctx, profile.Fingerprint, "migration-test")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_enrichment_day_counters(profile_fingerprint,utc_day,requests_started) VALUES (?, '2026-01-01', 3)`), profile.Fingerprint)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, `DELETE FROM applied_migrations WHERE name='context_enrichment_v1'`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, `UPDATE archive_metadata SET value='12' WHERE key='schema_version'`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(ctx))
	requirements.NoError(st.InitSchemaContext(ctx))
	loaded, err := st.LoadProviderProfile(ctx, profile.Fingerprint)
	requirements.NoError(err)
	checks.Equal(profile, loaded)
	active, err := st.HasActivePersonEnrichmentConsent(ctx, profile.Fingerprint)
	requirements.NoError(err)
	checks.True(active)
	var requests int
	requirements.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT requests_started FROM person_enrichment_day_counters WHERE profile_fingerprint=?`), profile.Fingerprint).Scan(&requests))
	checks.Equal(3, requests)
	_, err = st.DB().ExecContext(ctx, `INSERT INTO person_enrichment_profiles(fingerprint,provider_name,provider_kind,provider_namespace,endpoint,api_key_env,policy_json) SELECT 'context-profile',provider_name,'context',provider_namespace,endpoint,api_key_env,policy_json FROM person_enrichment_profiles`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(ctx, `INSERT INTO person_enrichment_profiles(fingerprint,provider_name,provider_kind,provider_namespace,endpoint,api_key_env,policy_json) SELECT 'invalid-profile',provider_name,'unknown',provider_namespace,endpoint,api_key_env,policy_json FROM person_enrichment_profiles LIMIT 1`)
	requirements.Error(err)
}
