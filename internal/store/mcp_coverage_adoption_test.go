package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Nested test scopes use assertion objects.
	Require "github.com/stretchr/testify/require" //nolint:importas // Nested test scopes use assertion objects.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/eventstest"
)

func openMCPEventsLegacyFixture(t *testing.T) *store.Store {
	t.Helper()
	return eventstest.OpenLegacy(t, filepath.Join("testdata", "mcp_events_legacy"))
}

func legacyMCPEventsConfig(principal string, derived bool) store.MCPEventsConfig {
	cfg := store.MCPEventsConfig{Enabled: true, Principal: principal}
	for _, source := range []string{"gmail", "imap"} {
		cfg.Capabilities = append(cfg.Capabilities,
			store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: source, Kinds: []string{"message"}},
			store.MCPEventCapability{Family: "msgvault.draft_changed", SourceType: source, Kinds: []string{"created", "updated", "deleted"}})
		if derived {
			for _, family := range []string{"msgvault.source_message_archived", "msgvault.inbox_message_archived"} {
				cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: family, SourceType: source, Kinds: []string{"message"}})
			}
		}
	}
	cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: "msgvault.calendar_event_changed", SourceType: "gcal", Kinds: []string{"created", "updated", "cancelled"}})
	return cfg
}

func TestMCPEventsLegacyCoverageAdoptionPreservesPending(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st := openMCPEventsLegacyFixture(t)
	rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	require.Len(rows, 1)
	before := rows[0]
	require.Equal("active", before.State)
	require.Positive(before.PendingSeq)
	require.NotEmpty(before.PendingEnvelope)
	require.NoError(st.InitSchemaContext(t.Context()))
	cfg := legacyMCPEventsConfig(before.Principal, true)
	clock, err := st.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(before.CursorEpoch, clock.Epoch)
	grantSyntheticSourceAuthority(t, st, before.SourceID)
	after, err := st.GetMCPSubscription(t.Context(), before.ID)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal(before, *after, "adoption must preserve all durable delivery state")
	// Read at the fixture's captured time. Migration does not extend expiry.
	event, err := st.GetMCPEvent(t.Context(), before.ID, before.CursorSeq, before.Principal, before.UpdatedAt)
	require.NoError(err)
	called := false
	require.NoError(st.ReadMCPEventMessage(t.Context(), before.ID, event.Seq, before.Principal, event.MessageID, before.UpdatedAt, func(tx *sql.Tx) error {
		called = true
		var body string
		return tx.QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), event.MessageID).Scan(&body)
	}))
	assert.True(called)
	require.NoError(st.InitSchemaContext(t.Context()))
	again, err := st.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	assert.Equal(clock, again)
	after, err = st.GetMCPSubscription(t.Context(), before.ID)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal(before, *after)
}

func TestMCPEventsCoverageAdoptionDoesNotHideLaterFamilyLoss(t *testing.T) {
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
	reduced, err := st.ConfigureMCPEvents(t.Context(), legacyMCPEventsConfig(rows[0].Principal, false))
	require.NoError(err)
	assert.Equal(adopted.Epoch+1, reduced.Epoch)
	restored, err := st.ConfigureMCPEvents(t.Context(), full)
	require.NoError(err)
	assert.Equal(reduced.Epoch+1, restored.Epoch)
	after, err := st.GetMCPSubscription(t.Context(), rows[0].ID)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal("capture_gap", after.StopReason)
	assert.Empty(after.PendingEnvelope)
}

func TestMCPEventsCoverageAdoptionRejectsIncompatibleChanges(t *testing.T) {
	for _, name := range []string{"disabled", "source_removed", "unsupported_derived_kind", "unknown_family", "existing_derived_authority"} {
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st := openMCPEventsLegacyFixture(t)
			rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
			require.NoError(err)
			require.Len(rows, 1)
			require.NoError(st.InitSchemaContext(t.Context()))
			cfg := legacyMCPEventsConfig(rows[0].Principal, true)
			switch name {
			case "disabled":
				cfg.Enabled = false
			case "source_removed":
				caps := cfg.Capabilities[:0]
				for _, cap := range cfg.Capabilities {
					if cap.SourceType != "imap" {
						caps = append(caps, cap)
					}
				}
				cfg.Capabilities = caps
			case "unsupported_derived_kind":
				cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"reaction"}})
			case "unknown_family":
				cfg.Capabilities = append(cfg.Capabilities, store.MCPEventCapability{Family: "example.unknown", SourceType: "gmail", Kinds: []string{"message"}})
			case "existing_derived_authority":
				_, err := st.DB().Exec(`UPDATE mcp_event_subscriptions SET name='msgvault.source_message_archived'`)
				require.NoError(err)
			}
			changed, err := st.ConfigureMCPEvents(t.Context(), cfg)
			require.NoError(err)
			assert.Equal(rows[0].CursorEpoch+1, changed.Epoch)
			after, err := st.GetMCPSubscription(t.Context(), rows[0].ID)
			require.NoError(err)
			require.NotNil(after)
			assert.Equal("capture_gap", after.StopReason)
			assert.Empty(after.PendingEnvelope)
			// An incompatible first configuration consumes the exception too.
			legacy, err := st.ConfigureMCPEvents(t.Context(), legacyMCPEventsConfig(rows[0].Principal, false))
			require.NoError(err)
			readded, err := st.ConfigureMCPEvents(t.Context(), legacyMCPEventsConfig(rows[0].Principal, true))
			require.NoError(err)
			assert.Equal(legacy.Epoch+1, readded.Epoch)
		})
	}
}
