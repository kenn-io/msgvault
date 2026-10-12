package gvoice

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

func syntheticVoiceText(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	Require.NoError(t, os.Mkdir(filepath.Join(directory, "Calls"), 0o700))
	Require.NoError(t, os.WriteFile(filepath.Join(directory, "Phones.vcf"), []byte("BEGIN:VCARD\nVERSION:3.0\nitem1.TEL:+15550000001\nitem1.X-ABLabel:Google Voice\nEND:VCARD\n"), 0o600))
	html := `<html><body><div class="message"><abbr class="dt" title="2024-06-01T12:00:00.000Z"></abbr><a class="tel" href="tel:+15550000002"><span class="fn">Synthetic Sender</span></a><q>Synthetic native text</q></div></body></html>`
	Require.NoError(t, os.WriteFile(filepath.Join(directory, "Calls", "Synthetic Sender - Text - 2024-06-01T12_00_00Z.html"), []byte(html), 0o600))
	return directory
}

// A direct native call has its own non-resumable logical operation. Caller
// ingestion metadata cannot turn an offline export into live observations.
func TestImportGvoiceFreshLogicalRunsDoNotInheritLiveMode(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("google_voice", "+15550000001")
	require.NoError(err)
	directory := syntheticVoiceText(t)
	for range 2 {
		client, err := NewClient(directory)
		require.NoError(err)
		summary, err := client.Import(t.Context(), st.WithIngestContext(store.IngestContext{Mode: store.IngestLive}), source.ID)
		require.NoError(err)
		assert.Equal(1, summary.MessagesImported)
	}
	var runs, completed, syncs, messages, decisions, events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs`).Scan(&runs))
	assert.Equal(2, runs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_runs WHERE completed=TRUE AND import_format='google_voice' AND notify_imports=FALSE`).Scan(&completed))
	assert.Equal(2, completed)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&syncs))
	assert.Equal(2, syncs)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
	assert.Equal(1, decisions)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Zero(events)
}
