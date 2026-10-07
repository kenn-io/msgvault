package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// createAppleBenchmarkFixture writes a synthetic ChatStorage.sqlite with
// direct chats of text messages and the Z_OPT columns Core Data maintains.
func createAppleBenchmarkFixture(b *testing.B, chats, messagesPerChat int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "ChatStorage.sqlite")
	execAppleFixture(b, path, appleFixtureSchema+appleMarkerSchema+`
		CREATE INDEX ZWAMESSAGE_ZCHATSESSION_INDEX ON ZWAMESSAGE (ZCHATSESSION);
		INSERT INTO Z_METADATA VALUES (1, 'benchmark-store-uuid', NULL);
	`)
	db, err := sql.Open("sqlite3", path)
	require.NoError(b, err)
	defer func() { require.NoError(b, db.Close()) }()

	tx, err := db.Begin()
	require.NoError(b, err)
	rowID := 0
	for chat := 1; chat <= chats; chat++ {
		jid := fmt.Sprintf("1555%07d@s.whatsapp.net", chat)
		_, err := tx.Exec(`INSERT INTO ZWACHATSESSION VALUES (?, ?, ?, 0, ?)`,
			chat, jid, fmt.Sprintf("Chat %d", chat), 700000000+chat)
		require.NoError(b, err)
		for range messagesPerChat {
			rowID++
			_, err := tx.Exec(`INSERT INTO ZWAMESSAGE VALUES (?, ?, NULL, ?, ?, ?, ?, 0, ?, 1)`,
				rowID, chat, fmt.Sprintf("bench-%d", rowID), rowID%2,
				600000000+rowID, fmt.Sprintf("benchmark message %d", rowID), jid)
			require.NoError(b, err)
		}
	}
	require.NoError(b, tx.Commit())
	return path
}

// BenchmarkImportAppleNoChangeRerun measures a rerun over an archive that
// already holds every message, with and without per-chat change markers.
func BenchmarkImportAppleNoChangeRerun(b *testing.B) {
	const chats, messagesPerChat = 100, 100
	for _, full := range []bool{false, true} {
		name := "markers"
		if full {
			name = "full"
		}
		b.Run(name, func(b *testing.B) {
			chatDBPath := createAppleBenchmarkFixture(b, chats, messagesPerChat)
			st, err := store.OpenForTest(filepath.Join(b.TempDir(), "msgvault.db"))
			require.NoError(b, err)
			b.Cleanup(func() { _ = st.Close() })
			require.NoError(b, st.InitSchema())
			importer := NewImporter(st, nil)
			opts := ImportOptions{Phone: "+15555550100", BatchSize: 1000, Full: full}
			first, err := importer.Import(context.Background(), chatDBPath, opts)
			require.NoError(b, err)
			require.Equal(b, int64(chats*messagesPerChat), first.MessagesAdded)

			b.ResetTimer()
			for b.Loop() {
				summary, err := importer.Import(context.Background(), chatDBPath, opts)
				require.NoError(b, err)
				require.Zero(b, summary.MessagesAdded)
			}
		})
	}
}
