package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationPolicyPublicationRetriesSQLiteSnapshotUpgrade(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	requirements.NoError(st.InitSchema())
	st.DB().SetMaxOpenConns(2)
	ctx, err := st.CaptureConversationPolicyWork(t.Context())
	requirements.NoError(err)

	attempts := 0
	err = retryContendedWriteErr(ctx, st, "test policy publication", func() error {
		return st.withConversationPolicyPublicationLogged(ctx, func(tx *loggedTx) error {
			attempts++
			if attempts == 1 {
				if _, err := st.DB().ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES('test_external_policy_writer','committed')`); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES('test_policy_publication_retry','committed')`)
			return err
		})
	})
	requirements.NoError(err)
	assertions.Equal(2, attempts, "a stale WAL snapshot must rerun the complete transaction")
	var value string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT value FROM archive_metadata WHERE key='test_policy_publication_retry'`).Scan(&value))
	assertions.Equal("committed", value)
}
