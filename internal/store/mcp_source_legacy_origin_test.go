package store_test

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"testing"
)

func TestMCPSourceUnprovenLegacyOriginDoesNotBreakUpgrade(t *testing.T) {
	require := Require.New(t)

	st := openMCPEventsLegacyFixture(t)
	subscriptions, err := st.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	require.Len(subscriptions, 1)
	before := subscriptions[0]
	// The actual old schema can retain an unsupported family without evidence
	// for its origin. Cutover must deny it without breaking valid Phase1 data.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE mcp_event_log SET family='msgvault.source_message_archived',scope_kind='source',scope_id=source_id,conversation_id=0,message_id=NULL WHERE seq=?`), before.CursorSeq)
	require.NoError(err)
	require.NoError(st.InitSchemaContext(t.Context()), "unprovable source occurrence cannot make the whole archive unavailable")
	_, err = st.ConfigureMCPEvents(t.Context(), legacyMCPEventsConfig(before.Principal, true))
	require.NoError(err, "resource adoption must deny unproven source origins without failing daemon startup")
	var valid bool
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT valid FROM mcp_event_resource_bindings WHERE seq=?`), before.CursorSeq).Scan(&valid))
	Assert.False(t, valid, "unsupported legacy source authority must fail closed")
	_, err = st.GetMCPEvent(t.Context(), before.ID, before.PendingSeq, before.Principal, before.UpdatedAt)
	require.Error(err, "legacy storage alone does not prove current native permission")
	grantSyntheticSourceAuthority(t, st, before.SourceID)
	_, err = st.GetMCPEvent(t.Context(), before.ID, before.PendingSeq, before.Principal, before.UpdatedAt)
	require.NoError(err, "current verified authority preserves the untouched original-family receipt")
}
