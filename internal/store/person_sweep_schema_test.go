package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonSweepSchemaParity(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)

	var clockRows int
	requirements.NoError(f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM person_sweep_change_clock WHERE singleton = TRUE AND sequence >= 0 AND enabled = TRUE`,
	).Scan(&clockRows))
	checks.Equal(1, clockRows)

	for _, indexName := range []string{
		"idx_person_sweep_changes_person_sequence",
		"idx_person_sweep_changes_source_sequence",
	} {
		var count int
		if f.store.IsPostgreSQL() {
			requirements.NoError(f.store.DB().QueryRow(`
				SELECT COUNT(*) FROM pg_indexes
				WHERE schemaname = current_schema() AND tablename = 'person_sweep_changes' AND indexname = $1`,
				indexName).Scan(&count))
		} else {
			requirements.NoError(f.store.DB().QueryRow(`
				SELECT COUNT(*) FROM sqlite_master
				WHERE type = 'index' AND tbl_name = 'person_sweep_changes' AND name = ?`,
				indexName).Scan(&count))
		}
		checks.Equal(1, count, indexName)
	}

	invalidValues := []struct {
		column string
		value  string
	}{
		{column: "source_lane", value: "raw_media"},
		{column: "change_kind", value: "rewrite"},
		{column: "evidence_effect", value: "guessed-effect"},
	}
	for i, invalid := range invalidValues {
		_, err := f.store.DB().Exec(f.store.Rebind(`
			INSERT INTO person_sweep_changes
				(sequence, person_id, source_lane, change_kind, evidence_effect, recorded_at)
			VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			9_000+i, f.alicePersonID,
			map[bool]string{true: invalid.value, false: "conversation_text"}[invalid.column == "source_lane"],
			map[bool]string{true: invalid.value, false: "upsert"}[invalid.column == "change_kind"],
			map[bool]string{true: invalid.value, false: ""}[invalid.column == "evidence_effect"])
		requirements.Error(err, "invalid %s must violate the closed schema vocabulary", invalid.column)
	}
}
