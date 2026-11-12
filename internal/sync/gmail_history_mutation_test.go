package sync

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestGmailHistoryMutationFailureRetainsCursor(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		for _, mutation := range []string{"label-add", "label-remove", "delete"} {
			t.Run(fmt.Sprintf("recovery=%t/%s", recovery, mutation), func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				e := acquisitionDurabilityEnv(t)
				seedMessages(e, 1, 1000, "victim")
				runFullSync(t, e)
				source := gmailCoverageSource(t.Context(), t, e)
				ids, err := e.Store.MessageExistsBatch(source.ID, []string{"victim"})
				require.NoError(err)
				require.Len(ids, 1)
				id := ids["victim"]
				inbox, err := e.Store.LabelIDContext(t.Context(), source.ID, "INBOX")
				require.NoError(err)
				starred, err := e.Store.LabelIDContext(t.Context(), source.ID, "STARRED")
				require.NoError(err)

				var heal func()
				switch mutation {
				case "label-add":
					e.SetHistory(1001, historyLabelAdded("victim", "STARRED"))
					heal = acquisitionWriteFault(t, e, "message_labels", "INSERT", fmt.Sprintf("NEW.message_id = %d", id))
				case "label-remove":
					e.SetHistory(1001, historyLabelRemoved("victim", "INBOX"))
					heal = acquisitionWriteFault(t, e, "message_labels", "DELETE", fmt.Sprintf("OLD.message_id = %d", id))
				case "delete":
					e.SetHistory(1001, historyDeleted("victim"))
					heal = acquisitionWriteFault(t, e, "messages", "UPDATE", "OLD.source_message_id = 'victim' AND NEW.deleted_from_source_at IS NOT NULL")
				}
				api := &acquisitionDurabilityAPI{MockAPI: e.Mock, catchup: recovery}
				e.Syncer = New(api, e.Store, nil)
				if recovery {
					_, err = e.Syncer.RecoverExpiredHistory(t.Context(), source)
				} else {
					_, err = e.Syncer.Incremental(t.Context(), source)
				}
				require.ErrorContains(err, "synthetic acquisition write failure")
				reopenAcquisitionStore(t, e)
				assert.Equal("1000", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
				run, err := e.Store.GetLatestSync(source.ID)
				require.NoError(err)
				assert.Equal(store.SyncStatusFailed, run.Status)
				if recovery {
					cp, err := decodeRecoveryCheckpoint(run.CursorBefore.String)
					require.NoError(err)
					assert.Equal(recoveryCatchup, cp.Phase)
					assert.Equal("1000", run.CursorAfter.String)
				}
				labels, err := e.Store.MessageLabelIDsContext(t.Context(), id)
				require.NoError(err)
				assert.ElementsMatch([]int64{inbox}, labels)
				assertDeletedFromSource(t, e.Store, "victim", false)
				heal()

				// A new owner must replay the failed event, including catch-up,
				// without acquiring the already archived message again.
				gets := len(e.Mock.GetMessageCalls)
				summary, err := e.Syncer.Incremental(t.Context(), gmailCoverageSource(t.Context(), t, e))
				require.NoError(err)
				assert.Zero(summary.Errors)
				assert.Zero(summary.MessagesAdded)
				assert.Len(e.Mock.GetMessageCalls, gets)
				reopenAcquisitionStore(t, e)
				assert.Equal("1001", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
				wantLabels := []int64{inbox}
				switch mutation {
				case "label-add":
					wantLabels = append(wantLabels, starred)
				case "label-remove":
					wantLabels = nil
				}
				labels, err = e.Store.MessageLabelIDsContext(t.Context(), id)
				require.NoError(err)
				assert.ElementsMatch(wantLabels, labels)
				assertDeletedFromSource(t, e.Store, "victim", mutation == "delete")
				raw, err := e.Store.GetMessageRaw(id)
				require.NoError(err)
				assert.Equal(testMIME(), raw, "keep the original row and RAW")
			})
		}
	}
}
