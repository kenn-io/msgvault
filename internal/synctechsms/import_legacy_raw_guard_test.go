package synctechsms

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
)

// SMS and MMS share a raw-format tag. An original native archive, with no
// retained identity yet, must not adopt an SMS-shaped payload as MMS evidence.
func TestImportSynctechLegacyIdentityRequiresNativeMMSRawShape(t *testing.T) {
	for _, boundary := range []string{"capture", "physical-delete"} {
		t.Run(boundary, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := openOriginalNativeSynctechArchive(t)
			var messageID int64
			require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
			require.NoError(st.UpsertMessageRawWithFormat(messageID, []byte(`{"Timestamp":"2024-06-01T12:01:00Z","Address":"+15550000002","Body":"Synthetic wrong kind","Type":1}`), RawFormat))
			var err error
			if boundary == "capture" {
				_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-synctech", ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: SourceType, Kinds: []string{"message"}}}})
				require.ErrorContains(err, "events_storage_unavailable")
			} else {
				_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at=?,delete_batch_id=? WHERE id=?`), time.Now().UTC(), "synthetic-synctech-raw-guard", messageID)
				require.NoError(err)
				_, err = st.DeleteDedupedBatchContext(t.Context(), "synthetic-synctech-raw-guard")
				require.ErrorContains(err, "incomplete native MMS identity")
			}
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Equal(1, count, "failed adoption cannot destroy native evidence")
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count)
		})
	}
}
