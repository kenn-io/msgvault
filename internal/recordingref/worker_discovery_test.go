package recordingref

import (
	"context"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRecordingReferenceDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages int
		yield    bool
	}{
		{"backfill across pages", 405, false},
		{"resume after budget", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := storetest.New(t)
			var first int64
			for i := range tc.messages {
				id := f.CreateMessage(strconv.Itoa(i))
				if i == 0 {
					first = id
				}
				recordingBody(t, f, id, "https://loom.com/share/"+strconv.Itoa(i))
			}
			_, err := f.Store.DB().Exec(`UPDATE messages SET content_changed_at='2000-01-01 00:00:00.000'`)
			require.NoError(t, err)
			client, err := docbankmedia.NewClient("http://127.0.0.1:1", func() (string, error) {
				return "", docbankmedia.ErrCredentialUnavailable
			})
			require.NoError(t, err)
			synctest.Test(t, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				budgetExpired := false
				worker := NewWorker(f.Store, client, "destination", nil, nil).WithOperationGate(func(context.Context) (func(), bool) {
					return func() {
						if !tc.yield {
							return
						}
						var count int
						require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM recording_references`).Scan(&count))
						if count == 1 && !budgetExpired {
							budgetExpired = true
							synctest.Sleep(21 * time.Second)
						}
					}, true
				})
				require.NoError(worker.RunBatch(t.Context()))
				var count int
				require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM recording_references`).Scan(&count))
				cursor, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
				require.NoError(err)
				if tc.yield {
					assert.Equal(1, count, "yield before the next message when the discovery budget expires")
					assert.True(cursor.AfterRow)
					assert.Equal(first, cursor.AfterID, "save only completed reconciliation")
					require.NoError(worker.RunBatch(t.Context()))
					require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM recording_references`).Scan(&count))
				} else {
					assert.True(cursor.At.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
					assert.False(cursor.AfterRow)
				}
				assert.Equal(tc.messages, count)
			})
		})
	}
}
