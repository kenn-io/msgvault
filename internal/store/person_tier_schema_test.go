package store_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonTierSchemaPresenceAndConstraints(t *testing.T) {
	f := storetest.New(t)
	p, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("tier-schema@example.com", "Synthetic Contact", "example.com"))
	require.NoError(t, err)
	for _, table := range []string{"person_tier_opt_outs", "person_tier_state", "person_tier_observations", "person_tier_events"} {
		var n int
		require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n))
		assert.Zero(t, n)
	}
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_tracking (person_id) VALUES (?)`), p.ID)
	require.NoError(t, err)
	var provenance, actor string
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT provenance, actor FROM person_tracking WHERE person_id = ?`), p.ID).Scan(&provenance, &actor))
	assert.Equal(t, "manual", provenance)
	assert.Empty(t, actor)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE person_tracking SET provenance='other' WHERE person_id = ?`), p.ID)
	assert.Error(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_tier_opt_outs (person_id,lane,actor) VALUES (?,'tracking','operator')`), p.ID)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_tier_opt_outs (person_id,lane,actor) VALUES (?,'tracking','operator')`), p.ID)
	assert.Error(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_tier_opt_outs (person_id,lane,actor) VALUES (?,'unknown','operator')`), p.ID)
	assert.Error(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM persons WHERE id = ?`), p.ID)
	require.NoError(t, err)
	var n int
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_opt_outs`).Scan(&n))
	assert.Zero(t, n)
}

func TestPersonTierSchemaChecksAndCascades(t *testing.T) {
	f := storetest.New(t)
	p, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("checks@example.org", "Synthetic Checks", "example.org"))
	require.NoError(t, err)
	for _, tc := range []struct {
		table   string
		columns []string
		values  []any
		invalid map[int]any
	}{
		{"person_tier_state",
			[]string{"person_id", "tier", "tier_since", "candidate_tier", "candidate_since", "lanes_json", "enrichment_eligible", "rule", "rule_version", "config_fingerprint", "as_of_local_date", "run_id", "run_started_at", "applied_at"},
			[]any{p.ID, 1, "2026-10-17T00:00:00Z", nil, nil, "[]", false, "none", 1, "fingerprint", "2026-10-17", "run", "2026-10-17T00:00:00Z", "2026-10-17T00:00:00Z"},
			map[int]any{1: 4, 3: 4, 4: "bad", 10: "bad"}},
		{"person_tier_observations",
			[]string{"person_id", "computed_tier", "rule", "signals_json", "shadow_tier", "shadow_since", "shadow_candidate_tier", "shadow_candidate_since", "rule_version", "config_fingerprint", "as_of_local_date", "activity_ready", "evaluated_at"},
			[]any{p.ID, 1, "none", "{}", 1, "2026-10-17", nil, nil, 1, "fingerprint", "2026-10-17", true, "2026-10-17T00:00:00Z"},
			map[int]any{1: 4, 4: 4, 5: "bad", 6: 4, 7: "bad", 10: "bad"}},
		{"person_tier_events",
			[]string{"person_id", "from_tier", "to_tier", "reason", "mode", "lanes_json", "signals_json", "rule", "rule_version", "actor", "run_id"},
			[]any{p.ID, nil, 1, "promotion", "apply", "{}", "{}", "tier1_intensity", 1, "operator", "run"},
			map[int]any{1: 4, 2: 4, 3: "unknown", 4: "unknown"}},
	} {
		t.Run(tc.table, func(t *testing.T) {
			query := f.Store.Rebind("INSERT INTO " + tc.table + " (" + strings.Join(tc.columns, ",") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(tc.columns)), ",") + ")")
			// Prove the baseline insert is valid, then roll it back so a PK
			// conflict cannot make every negative case pass accidentally.
			tx, err := f.Store.DB().BeginTx(t.Context(), nil)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), query, tc.values...)
			require.NoError(t, err)
			require.NoError(t, tx.Rollback())
			for index, invalid := range tc.invalid {
				values := slices.Clone(tc.values)
				values[index] = invalid
				_, err := f.Store.DB().ExecContext(t.Context(), query, values...)
				assert.Error(t, err, "constraint on %s", tc.columns[index])
			}
			_, err = f.Store.DB().ExecContext(t.Context(), query, tc.values...)
			require.NoError(t, err)
		})
	}
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM persons WHERE id = ?`), p.ID)
	require.NoError(t, err)
	for _, table := range []string{"person_tier_state", "person_tier_observations", "person_tier_events"} {
		var count int
		require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
		assert.Zero(t, count)
	}
}

func TestPersonTierSchemaLegacyColumnsUpgrade(t *testing.T) {
	f := storetest.New(t)
	p, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("legacy-tier@example.org", "Legacy Contact", "example.org"))
	require.NoError(t, err)
	// Reproduce the v10 presence tables without the additive columns. Their
	// populated rows exercise the real legacy-column migration on both dialects.
	idType, timeType := "INTEGER", "DATETIME"
	if f.Store.IsPostgreSQL() {
		idType, timeType = "BIGINT", "TIMESTAMPTZ"
	}
	for _, statement := range []string{
		`DROP TABLE person_tracking`, `DROP TABLE person_brief_enrollments`,
		fmt.Sprintf(`CREATE TABLE person_tracking (person_id %s PRIMARY KEY REFERENCES persons(id) ON DELETE CASCADE, tracked_at %s NOT NULL DEFAULT CURRENT_TIMESTAMP)`, idType, timeType),
		fmt.Sprintf(`CREATE TABLE person_brief_enrollments (person_id %s PRIMARY KEY REFERENCES persons(id) ON DELETE CASCADE, enabled_at %s NOT NULL DEFAULT CURRENT_TIMESTAMP, actor TEXT NOT NULL)`, idType, timeType),
	} {
		_, err = f.Store.DB().ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_tracking (person_id) VALUES (?)`), p.ID)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_brief_enrollments (person_id,actor) VALUES (?,'legacy-operator')`), p.ID)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value='10' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.ErrorContains(t, f.Store.ValidateSchemaContext(t.Context()), "run setup")
	for range 2 {
		require.NoError(t, f.Store.InitSchemaContext(t.Context()))
		var provenance, actor, briefProvenance, briefActor string
		require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT provenance, actor FROM person_tracking WHERE person_id = ?`), p.ID).Scan(&provenance, &actor))
		require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT provenance, actor FROM person_brief_enrollments WHERE person_id = ?`), p.ID).Scan(&briefProvenance, &briefActor))
		assert.Equal(t, "manual", provenance)
		assert.Empty(t, actor)
		assert.Equal(t, "manual", briefProvenance)
		assert.Equal(t, "legacy-operator", briefActor)
		version, err := f.Store.SchemaVersionContext(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 11, version)
	}
}
