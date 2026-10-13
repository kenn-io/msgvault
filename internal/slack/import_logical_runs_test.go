package slack

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func syntheticImportSlackdump(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	Require.NoError(t, os.MkdirAll(filepath.Join(dir, "example-channel"), 0o700))
	for name, content := range map[string]string{
		"users.json":                      `[{"id":"U_OWNER","team_id":"T_EXAMPLE","name":"synthetic_owner","profile":{"email":"owner@example.net","display_name":"Synthetic Owner"}},{"id":"U_PEER","team_id":"T_EXAMPLE","name":"synthetic_peer","profile":{"email":"peer@example.net","display_name":"Synthetic Peer"}}]`,
		"channels.json":                   `[{"id":"C_EXAMPLE","name":"example-channel","is_channel":true,"is_member":true,"members":["U_OWNER","U_PEER"]}]`,
		"example-channel/2024-01-01.json": `[{"type":"message","ts":"1704067200.000001","user":"U_PEER","text":"Synthetic offline Slack message"}]`,
	} {
		Require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

func TestImportSlackdumpOwnsFreshSilentLogicalRuns(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := syntheticImportSlackdump(t)
	st := testutil.NewTestStore(t)
	caller := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	var previous string
	for range 2 {
		summary, err := ImportSlackdump(t.Context(), caller, path, SlackdumpImportOptions{Me: "owner@example.net"})
		require.NoError(err)
		require.NotEmpty(summary.ImportRunID)
		assert.NotEqual(previous, summary.ImportRunID)
		previous = summary.ImportRunID
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM import_runs WHERE completed=TRUE AND import_format='slackdump' AND notify_imports=FALSE`: 2,
		`SELECT COUNT(*) FROM sync_runs WHERE status='completed'`:                                                      2,
		`SELECT COUNT(*) FROM messages`:                 1,
		`SELECT COUNT(*) FROM import_message_decisions`: 1,
		`SELECT COUNT(*) FROM mcp_event_log`:            0,
	} {
		var got int
		require.NoError(st.DB().QueryRow(query).Scan(&got))
		assert.Equal(want, got, query)
	}
}
