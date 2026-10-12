package synctechsms

import (
	"os"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// Preserve native participant-ID-derived keys, while preventing the same raw
// MMS from becoming a new notification after a participant merge or deletion.
func TestImportSynctechMMSIdentityMergeCannotReplayLogicalOccurrence(t *testing.T) {
	for _, initialNotify := range []bool{false, true} {
		name := "silent"
		wantEvents := 0
		if initialNotify {
			name = "notified"
			wantEvents = 1
		}
		for _, deletion := range []string{"retain", "physical-delete"} {
			t.Run(name+"/"+deletion, func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				st := testutil.NewTestStore(t)
				configureSyntheticSynctechImport(t, st, true)
				path := syntheticSynctechMMSFile(t)
				opts := ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true, NotifyImports: initialNotify}
				first, err := NewImporter(st, opts).ImportPath(path)
				require.NoError(err)
				require.Len(first.MessageIDs, 1)
				var oldKey string
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_message_id FROM messages WHERE id=?`), first.MessageIDs[0]).Scan(&oldKey))
				var remoteID int64
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id FROM participant_identifiers WHERE identifier_type=? AND identifier_value=?`), ParticipantIdentifierType, "+15550000002").Scan(&remoteID))
				survivorID, err := st.EnsureParticipantByPhone("+15550000003", "Synthetic Merge Survivor", ParticipantIdentifierType)
				require.NoError(err)
				require.NoError(st.MergeParticipants(remoteID, survivorID))
				if deletion == "physical-delete" {
					_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?`), time.Now().UTC(), "synthetic-synctech-merge-delete", first.MessageIDs[0])
					require.NoError(err)
					removed, err := st.DeleteDedupedBatchContext(t.Context(), "synthetic-synctech-merge-delete")
					require.NoError(err)
					assert.Equal(int64(1), removed)
				}
				opts.NotifyImports = true
				second, err := NewImporter(st, opts).ImportPath(path)
				require.NoError(err)
				require.Len(second.MessageIDs, 1)
				var newKey string
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_message_id FROM messages WHERE id=?`), second.MessageIDs[0]).Scan(&newKey))
				assert.NotEqual(oldKey, newKey, "native participant-ID-dependent archive dedupe remains unchanged")
				var events int
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
				assert.Equal(wantEvents, events, "identity reconciliation cannot replay an already imported logical MMS")
				raw, err := os.ReadFile(path)
				require.NoError(err)
				require.NoError(os.WriteFile(path, []byte(strings.Replace(string(raw), "synthetic-ready-mms", "synthetic-genuinely-new-mms", 1)), 0o600))
				_, err = NewImporter(st, opts).ImportPath(path)
				require.NoError(err)
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
				assert.Equal(wantEvents+1, events, "a genuinely new native MMS must still notify")
			})
		}
	}
}
