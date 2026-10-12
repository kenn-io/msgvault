package whatsapp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportWhatsAppAppleRequiredSnapshotRollsBackNativeRelations(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw", "conversation_participants", "mcp_event_log"} {
		t.Run(table, func(t *testing.T) {
			path := createAppleChatFixture(t)
			execAppleFixture(t, path, `DELETE FROM ZWAMESSAGE WHERE Z_PK<>1; DELETE FROM ZWAGROUPMEMBER; DELETE FROM ZWACHATSESSION WHERE Z_PK<>1`)
			st := testutil.NewTestStore(t)
			configureSyntheticWhatsAppImports(t, st, true)
			abortSyntheticWhatsAppInsert(t, st, table)
			opts := ImportOptions{Phone: "+15555550100", DisplayName: "Synthetic Owner", NotifyImports: true}
			_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.Error(t, err, "native Apple required write failures remain fatal")
			for _, name := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "attachments", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM "+name).Scan(&n))
				assert.Zero(t, n, "failed Apple native snapshot leaked "+name)
			}
		})
	}
}
