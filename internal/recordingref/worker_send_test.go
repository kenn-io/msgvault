package recordingref

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRecordingReferenceRemovalBeforeSend(t *testing.T) {
	for _, change := range []string{"source deletion", "link removal"} {
		for _, tc := range []struct {
			name, stateAtChange string
			beforePreparation   bool
			requests            int
		}{
			{"before preparation", "pending", true, 0},
			{"after preparation yields", "uncertain", false, 1},
		} {
			t.Run(change+"/"+tc.name, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				f := storetest.New(t)
				id := f.CreateMessage("recording")
				recordingBody(t, f, id, "https://loom.com/share/recording")
				requests := make(chan string, 2)
				client := recordingClient(t, func(w http.ResponseWriter, r *http.Request) {
					var req docbankmedia.ReferenceRequest
					if !assert.NoError(json.UnmarshalRead(r.Body, &req)) {
						return
					}
					requests <- req.ReferenceURL
					writeReceipt(w, req.OperationID)
				})
				worker := NewWorker(f.Store, client, "destination", nil, nil)
				message, found, err := f.Store.ReadRecordingMessage(t.Context(), id)
				require.NoError(err)
				require.True(found)
				require.NoError(worker.reconcile(t.Context(), message))
				claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 1)
				require.NoError(err)
				require.Len(claims, 1)

				var gate sync.Mutex
				changed := false
				mutate := func() {
					gate.Lock()
					defer gate.Unlock()
					state, _, _ := recordingState(t, f, id)
					assert.Equal(tc.stateAtChange, state, "the worker must mark sending before yielding after its read")
					if change == "source deletion" {
						require.NoError(f.Store.MarkMessagesDeletedBatch(f.Source.ID, []string{"recording"}))
					} else {
						require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "Recording link removed", Valid: true}, sql.NullString{}))
					}
					changed = true
				}
				if tc.beforePreparation {
					mutate()
				}
				worker.WithOperationGate(func(context.Context) (func(), bool) {
					gate.Lock()
					return func() {
						gate.Unlock()
						if !changed {
							mutate()
						}
					}, true
				})
				require.NoError(worker.deliver(t.Context(), claims[0]))
				assert.True(changed)
				require.Len(requests, tc.requests)
				if tc.requests > 0 {
					assert.Equal("https://loom.com/share/recording", <-requests)
				}
				state, _, _ := recordingState(t, f, id)
				assert.Equal("withdrawn", state)
			})
		}
	}
}
