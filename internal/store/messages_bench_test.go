package store_test

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func BenchmarkGetRandomMessageIDs(b *testing.B) {
	for _, size := range []int{10000, 100000, 1000000} {
		b.Run(fmt.Sprintf("messages=%d", size), func(b *testing.B) {
			st, err := store.Open(filepath.Join(b.TempDir(), "sample.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, st.Close()) })
			require.NoError(b, st.InitSchema())
			source, err := st.GetOrCreateSource("gmail", "sample@example.com")
			require.NoError(b, err)
			conversation, err := st.EnsureConversationWithType(source.ID, "sample", "email_thread", "")
			require.NoError(b, err)

			tx, err := st.DB().BeginTx(b.Context(), nil)
			require.NoError(b, err)
			defer func() { _ = tx.Rollback() }()
			stmt, err := tx.PrepareContext(b.Context(), st.Rebind(`
				INSERT INTO messages (source_id, conversation_id, source_message_id, message_type)
				VALUES (?, ?, ?, 'email')
			`))
			require.NoError(b, err)
			defer func() { require.NoError(b, stmt.Close()) }()
			for i := range size {
				_, err := stmt.ExecContext(b.Context(), source.ID, conversation, strconv.Itoa(i))
				require.NoError(b, err)
			}
			require.NoError(b, tx.Commit())

			for _, limit := range []int{100, 1000} {
				b.Run(fmt.Sprintf("sample=%d", limit), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						ids, err := st.GetRandomMessageIDs(source.ID, limit)
						require.NoError(b, err)
						require.Len(b, ids, limit)
					}
				})
			}
		})
	}
}
