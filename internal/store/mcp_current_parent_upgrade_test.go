package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested tests.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested tests.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Adopting the current SQL conversation despite an existing contradictory
// subscription reference must fail. This exercises native legacy data and
// the actual receipt/body authorization, not a fabricated resource token.
func TestMCPLegacyAdoptionHonorsRecordedConversationReference(t *testing.T) {
	for _, contradictory := range []bool{false, true} {
		name := "matching_reference"
		if contradictory {
			name = "contradictory_reference"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := openMCPEventsLegacyFixture(t)
			require.NoError(st.InitSchemaContext(t.Context()))
			var migratedSubscriptions int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_subscriptions WHERE scope_reference_id>=0`).Scan(&migratedSubscriptions))
			rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
			require.NoError(err)
			require.Len(rows, 1)
			sub := rows[0]
			var reference int64
			require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO mcp_event_conversation_refs (conversation_id) VALUES (?) RETURNING reference_id`), sub.ScopeID).Scan(&reference))
			recorded := reference
			if contradictory {
				foreign, err := st.EnsureConversation(sub.SourceID, "other-synthetic-upgrade-thread", "Other synthetic thread")
				require.NoError(err)
				require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO mcp_event_conversation_refs (conversation_id) VALUES (?) RETURNING reference_id`), foreign).Scan(&recorded))
				require.NotEqual(reference, recorded)
			}
			_, err = st.DB().Exec(st.Rebind(`UPDATE mcp_event_subscriptions SET scope_reference_id=? WHERE id=?`), recorded, sub.ID)
			require.NoError(err)
			cfg := legacyMCPEventsConfig(sub.Principal, true)
			_, err = st.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			grantSyntheticSourceAuthority(t, st, sub.SourceID)
			var messageID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_id FROM mcp_event_log WHERE seq=?`), sub.CursorSeq).Scan(&messageID))
			for attempt := range 2 {
				called := false
				readErr := st.ReadMCPEventMessage(t.Context(), sub.ID, sub.CursorSeq, sub.Principal, messageID, sub.UpdatedAt, func(tx *sql.Tx) error {
					called = true
					var body string
					return tx.QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageID).Scan(&body)
				})
				if contradictory {
					require.Error(readErr, "a contradictory recorded lifetime must never gain body authority, attempt %d", attempt)
					assert.False(called)
					require.Error(st.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, sub.UpdatedAt))
				} else {
					require.NoError(readErr)
					assert.True(called)
					require.NoError(st.CheckMCPSubscription(t.Context(), sub.ID, sub.Generation, sub.UpdatedAt))
				}
				// Restoring the numeric association cannot overturn the denied cutover.
				_, err = st.DB().Exec(st.Rebind(`UPDATE mcp_event_subscriptions SET scope_reference_id=? WHERE id=?`), reference, sub.ID)
				require.NoError(err)
				_, err = st.ConfigureMCPEvents(t.Context(), cfg)
				require.NoError(err)
			}
		})
	}
}

// Schema setup must seal both exceptions together, or retry could leave a
// resource cutover consumed while coverage still permits a legacy upgrade.
func TestMCPFreshSetupAdoptionRollbackAndRetry(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.DB().Exec(`DELETE FROM archive_metadata WHERE key IN ('mcp_events_resource_adoption_v1','mcp_events_scope_coverage_adoption_v1')`)
	require.NoError(err)
	setup := `CREATE TRIGGER reject_fresh_coverage_adoption BEFORE INSERT ON archive_metadata
		WHEN NEW.key='mcp_events_scope_coverage_adoption_v1'
		BEGIN SELECT RAISE(ABORT,'synthetic adoption marker failure'); END`
	cleanup := `DROP TRIGGER reject_fresh_coverage_adoption`
	if st.IsPostgreSQL() {
		setup = `CREATE FUNCTION reject_fresh_coverage_adoption() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF NEW.key='mcp_events_scope_coverage_adoption_v1' THEN RAISE EXCEPTION 'synthetic adoption marker failure'; END IF;
			RETURN NEW; END $$;
		CREATE TRIGGER reject_fresh_coverage_adoption BEFORE INSERT ON archive_metadata FOR EACH ROW EXECUTE FUNCTION reject_fresh_coverage_adoption()`
		cleanup = `DROP TRIGGER reject_fresh_coverage_adoption ON archive_metadata; DROP FUNCTION reject_fresh_coverage_adoption()`
	}
	_, err = st.DB().Exec(setup)
	require.NoError(err)
	require.Error(st.InitSchemaContext(t.Context()), "a rejected coverage seal fails schema setup")
	var markers int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key IN ('mcp_events_resource_adoption_v1','mcp_events_scope_coverage_adoption_v1')`).Scan(&markers))
	assert.Zero(markers, "resource and coverage seals roll back together")
	_, err = st.DB().Exec(cleanup)
	require.NoError(err)
	require.NoError(st.InitSchemaContext(t.Context()))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key IN ('mcp_events_resource_adoption_v1','mcp_events_scope_coverage_adoption_v1')`).Scan(&markers))
	assert.Equal(2, markers)
	clock, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{})
	require.NoError(err)
	assert.Zero(clock.Epoch, "setup and default-off startup never start capture")
}
