//go:build cgo

package store

import (
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestOrdinaryMailPersistDoesNotWriteMeetingKeys(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if st.dialect.DriverName() == postgresDriverName {
		t.Skip("SQLite authorizer observes DELETE statements even when derived tables are empty")
	}
	source, err := st.GetOrCreateSource("gmail", "mail@example.com")
	requirements.NoError(err)
	st.db.SetMaxOpenConns(1)
	conn, err := st.db.Conn(t.Context())
	requirements.NoError(err)
	requirements.NoError(conn.Raw(func(driverConn any) error {
		sqlite, ok := driverConn.(*sqlite3.SQLiteConn)
		requirements.True(ok)
		sqlite.RegisterAuthorizer(func(operation int, table, _, _ string) int {
			if operation == sqlite3.SQLITE_DELETE && (table == "meeting_link_keys" || table == "meeting_link_key_state") {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	requirements.NoError(conn.Close())
	_, err = st.PersistMessage(&MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: "ordinary-mail", MessageType: "email"},
		Conversation: &ConversationPersistData{SourceConversationID: "ordinary-mail", ConversationType: "email", Title: "Synthetic mail"},
	})
	requirements.NoError(err)
}
