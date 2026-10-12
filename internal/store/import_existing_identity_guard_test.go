package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Native unchanged-row adoption must never bind a receipt decision to a
// replacement message, another source or a terminal logical import.
func TestImportExistingIdentityGuard(t *testing.T) {
	for _, scenario := range []string{"matching", "wrong_source", "wrong_key", "missing_id", "missing_run", "completed_run"} {
		t.Run(scenario, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f := storetest.New(t)
			source, err := f.Store.GetOrCreateSource("meeting_import", "synthetic-meetings")
			require.NoError(err)
			conversation, err := f.Store.EnsureConversation(source.ID, "meeting:synthetic", "Synthetic meeting")
			require.NoError(err)
			message := &store.Message{SourceID: source.ID, SourceMessageID: "meeting:synthetic", ConversationID: conversation, MessageType: "meeting_transcript"}
			message.ID, err = f.Store.UpsertMessage(message)
			require.NoError(err)
			execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), source.ID)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
			syncID, err := execution.StartSyncContext(t.Context(), "import", "")
			require.NoError(err)
			scoped := f.Store.ScopedToSync(source.ID, syncID)
			run, err := scoped.BeginNativeImportAttempt(t.Context(), source.ID, "meeting_import", false, syncID, 0)
			require.NoError(err)
			importing, err := scoped.WithImportRun(t.Context(), run.ID)
			require.NoError(err)
			guard := store.MessageIdentityGuard{ID: message.ID, SourceID: source.ID, SourceMessageID: message.SourceMessageID}
			switch scenario {
			case "wrong_source":
				guard.SourceID++
			case "wrong_key":
				guard.SourceMessageID = "meeting:replacement"
			case "missing_id":
				guard.ID++
			case "missing_run":
				importing = scoped.WithIngestContext(store.IngestContext{Mode: store.IngestImport})
			case "completed_run":
				require.NoError(scoped.CompleteSync(syncID, ""))
			}
			err = importing.RetainExistingImportMessageContext(t.Context(), guard)
			want := 0
			if scenario == "matching" {
				require.NoError(err)
				want = 1
			} else {
				require.Error(err)
			}
			var decisions, events int
			require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions WHERE notified=FALSE AND import_run_id IS NULL AND archived_at IS NULL`).Scan(&decisions))
			assert.Equal(want, decisions)
			require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Zero(events)
		})
	}
}
