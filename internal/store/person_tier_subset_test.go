package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTierSubsetCopiesTierTables(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(sourcePath)
	require.NoError(t, err)
	require.NoError(t, st.InitSchemaContext(t.Context()))
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic thread")
	require.NoError(t, err)
	participant, err := st.EnsureParticipant("subset-person@example.test", "Subset Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{ConversationID: conv, SourceID: source.ID, SourceMessageID: "subset-message", SenderID: sql.NullInt64{Int64: participant, Valid: true}, MessageType: "email"})
	require.NoError(t, err)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, query := range []string{
		`INSERT INTO person_tier_opt_outs(person_id,lane,actor) VALUES(?,'briefs','owner')`,
		`INSERT INTO person_tier_state(person_id,tier,tier_since,lanes_json,enrichment_eligible,rule,rule_version,config_fingerprint,as_of_local_date,run_id,run_started_at,applied_at) VALUES(?,2,'2026-10-01','["tracking"]',FALSE,'tier2_two_way',1,'config','2026-10-01','run','2026-10-01','2026-10-01')`,
		`INSERT INTO person_tier_observations(person_id,computed_tier,rule,signals_json,shadow_tier,shadow_since,rule_version,config_fingerprint,as_of_local_date,activity_ready,evaluated_at) VALUES(?,2,'tier2_two_way','{}',2,'2026-10-01',1,'config','2026-10-01',TRUE,'2026-10-01')`,
		`INSERT INTO person_tier_events(person_id,from_tier,to_tier,reason,mode,lanes_json,signals_json,rule,rule_version,actor,run_id) VALUES(?,3,2,'promotion','apply','{}','{}','tier2_two_way',1,'owner','run')`,
	} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(query), person.ID)
		require.NoError(t, err)
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_tracking(person_id,tracked_at,provenance,actor) VALUES(?,?,'auto','signal-tiers')`), person.ID, at)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	destDir := filepath.Join(t.TempDir(), "subset")
	_, err = store.CopySubsetWithOptions(sourcePath, destDir, 1, store.CopySubsetOptions{IncludeIdentity: true, IncludeProfiles: true})
	require.NoError(t, err)
	subset, err := store.Open(filepath.Join(destDir, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, subset.Close()) })
	for _, table := range []string{"person_tier_opt_outs", "person_tier_state", "person_tier_observations", "person_tier_events"} {
		var count int
		require.NoError(t, subset.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
}
