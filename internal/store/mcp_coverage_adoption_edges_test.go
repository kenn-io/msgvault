package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Nested test scopes use assertion objects.
	Require "github.com/stretchr/testify/require" //nolint:importas // Nested test scopes use assertion objects.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPEventsFreshCoverageConsumesAdoption(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st := testutil.NewTestStore(t)
			cfg := legacyMCPEventsConfig("synthetic-owner", false)
			cfg.Enabled = enabled
			first, err := st.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			cfg.Enabled = true
			legacy, err := st.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			if enabled {
				assert.Equal(first.Epoch, legacy.Epoch)
			} else {
				assert.Equal(first.Epoch+1, legacy.Epoch)
			}
			expanded, err := st.ConfigureMCPEvents(t.Context(), legacyMCPEventsConfig("synthetic-owner", true))
			require.NoError(err)
			assert.Equal(legacy.Epoch+1, expanded.Epoch, "a fresh archive cannot later pretend to be a legacy upgrade")
		})
	}
}

func TestMCPEventsCoverageAdoptionRollsBackWithConfiguration(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := openMCPEventsLegacyFixture(t)
	rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	require.Len(rows, 1)
	require.NoError(st.InitSchemaContext(t.Context()))
	setup := `CREATE TRIGGER reject_coverage_update BEFORE UPDATE OF coverage_fingerprint ON mcp_event_clock BEGIN SELECT RAISE(ABORT,'synthetic coverage update failure'); END`
	cleanup := `DROP TRIGGER reject_coverage_update`
	if st.IsPostgreSQL() {
		setup = `CREATE FUNCTION reject_coverage_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic coverage update failure'; END $$;
		CREATE TRIGGER reject_coverage_update BEFORE UPDATE OF coverage_fingerprint ON mcp_event_clock FOR EACH ROW EXECUTE FUNCTION reject_coverage_update()`
		cleanup = `DROP TRIGGER reject_coverage_update ON mcp_event_clock; DROP FUNCTION reject_coverage_update()`
	}
	_, err = st.DB().Exec(setup)
	require.NoError(err)
	cfg := legacyMCPEventsConfig(rows[0].Principal, true)
	_, err = st.ConfigureMCPEvents(t.Context(), cfg)
	require.Error(err)
	after, err := st.GetMCPSubscription(t.Context(), rows[0].ID)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal(rows[0], *after)
	_, err = st.DB().Exec(cleanup)
	require.NoError(err)
	clock, err := st.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(rows[0].CursorEpoch, clock.Epoch, "failed configuration must not consume first adoption")
	after, err = st.GetMCPSubscription(t.Context(), rows[0].ID)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal(rows[0], *after)
}

func TestMCPEventsCoverageAdoptionSurvivesEmptyRetainedState(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := openMCPEventsLegacyFixture(t)
	rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	require.Len(rows, 1)
	require.NoError(st.InitSchemaContext(t.Context()))
	full := legacyMCPEventsConfig(rows[0].Principal, true)
	adopted, err := st.ConfigureMCPEvents(t.Context(), full)
	require.NoError(err)
	assert.Equal(rows[0].CursorEpoch, adopted.Epoch)
	_, err = st.DB().Exec(`DELETE FROM mcp_event_subscriptions`)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM mcp_event_log`)
	require.NoError(err)
	legacy, err := st.ConfigureMCPEvents(t.Context(), legacyMCPEventsConfig(rows[0].Principal, false))
	require.NoError(err)
	assert.Equal(adopted.Epoch+1, legacy.Epoch)
	restored, err := st.ConfigureMCPEvents(t.Context(), full)
	require.NoError(err)
	assert.Equal(legacy.Epoch+1, restored.Epoch, "empty retained state must not reopen the adoption exception")
}
