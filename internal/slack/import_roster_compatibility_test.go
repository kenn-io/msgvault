package slack

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportSlackdumpRefreshPreservesUnobservedDirectRecipients(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notify bool
		events int
	}{{"silent", false, 0}, {"notifying", true, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)
			root := syntheticImportSlackdump(t)
			require.NoError(os.RemoveAll(filepath.Join(root, "example-channel")))
			require.NoError(os.WriteFile(filepath.Join(root, "channels.json"), []byte(`[]`), 0o600))
			require.NoError(os.MkdirAll(filepath.Join(root, "example-group"), 0o700))
			require.NoError(os.WriteFile(filepath.Join(root, "mpims.json"), []byte(`[{"id":"G_EXAMPLE","name":"example-group","is_mpim":true,"members":["U_OWNER","U_PEER"]}]`), 0o600))
			require.NoError(os.WriteFile(filepath.Join(root, "example-group/2024-01-01.json"), []byte(`[{"type":"message","ts":"1704067200.000001","user":"U_PEER","text":"Synthetic offline group message"}]`), 0o600))
			st := testutil.NewTestStore(t)
			configureSlackdumpNotifications(t, st)
			_, err := ImportSlackdump(t.Context(), st, root, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: tc.notify})
			require.NoError(err)
			before, err := st.InspectMessage("G_EXAMPLE:1704067200.000001")
			require.NoError(err)
			require.Equal(1, before.RecipientCounts["to"])
			// An export without roster evidence preserves prior archived addressees.
			require.NoError(os.WriteFile(filepath.Join(root, "mpims.json"), []byte(`[{"id":"G_EXAMPLE","name":"example-group","is_mpim":true,"num_members":2}]`), 0o600))
			_, err = ImportSlackdump(t.Context(), st, root, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: tc.notify})
			require.NoError(err)
			after, err := st.InspectMessage("G_EXAMPLE:1704067200.000001")
			require.NoError(err)
			assert.Equal(1, after.RecipientCounts["to"], "missing roster is not an observed empty addressee set")
			// A later observed roster still replaces addressees, even if only the sender remains.
			require.NoError(os.WriteFile(filepath.Join(root, "mpims.json"), []byte(`[{"id":"G_EXAMPLE","name":"example-group","is_mpim":true,"members":["U_PEER"]}]`), 0o600))
			_, err = ImportSlackdump(t.Context(), st, root, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: tc.notify})
			require.NoError(err)
			observed, err := st.InspectMessage("G_EXAMPLE:1704067200.000001")
			require.NoError(err)
			assert.Equal(0, observed.RecipientCounts["to"], "an observed roster replaces prior addressees")
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(tc.events, events, "refreshing retained content never emits another import arrival")
		})
	}
}
