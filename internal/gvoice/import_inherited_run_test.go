package gvoice

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestImportGvoiceDoesNotInheritAnotherNativeImportRun(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-voice", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "google_voice", Kinds: []string{"message"}}, {Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
	require.NoError(err)
	foreign, err := st.GetOrCreateSource("eml", "foreign@example.test")
	require.NoError(err)
	source, err := st.GetOrCreateSource("google_voice", "+15550000001")
	require.NoError(err)
	sync, err := st.StartSync(foreign.ID, "synthetic-foreign-import")
	require.NoError(err)
	t.Cleanup(func() { Require.NoError(t, st.FailSync(sync, "synthetic cleanup")) })
	view := st.ScopedToSync(foreign.ID, sync)
	run, err := view.BeginNativeImportAttempt(t.Context(), foreign.ID, "eml", true, sync, 0)
	require.NoError(err)
	view, err = view.WithImportRun(t.Context(), run.ID)
	require.NoError(err)
	client, err := NewClient(syntheticVoiceText(t))
	require.NoError(err)
	summary, err := client.Import(t.Context(), view, source.ID)
	require.NoError(err)
	require.Equal(1, summary.MessagesImported)
	assert.NotEqual(run.ID, summary.ImportRunID)
	var format, status string
	var notify bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT import_format,notify_imports FROM import_runs WHERE id=?`), summary.ImportRunID).Scan(&format, &notify))
	assert.Equal("google_voice", format)
	assert.False(notify)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT status FROM sync_runs WHERE id=?`), sync).Scan(&status))
	assert.Equal("running", status, "the offline call does not finalize its caller's operation")
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}
