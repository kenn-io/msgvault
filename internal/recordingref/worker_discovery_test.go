package recordingref

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRecordingReferenceDiscoveryResumesAfterBudget(t *testing.T) {
	f := storetest.New(t)
	first := f.CreateMessage("first")
	second := f.CreateMessage("second")
	recordingBody(t, f, first, "https://loom.com/share/first")
	recordingBody(t, f, second, "https://loom.com/share/second")
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
		require.Equal(1, count, "yield before the next message when the discovery budget expires")
		cursor, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
		require.NoError(err)
		assert.True(cursor.AfterRow)
		assert.Equal(first, cursor.AfterID, "save only completed reconciliation")

		require.NoError(worker.RunBatch(t.Context()))
		require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM recording_references`).Scan(&count))
		assert.Equal(2, count, "the next pass discovers the remaining message")
	})
}
