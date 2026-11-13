package signalandroid

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

// writeTablesFixture writes a SQLite file containing only the given DDL.
func writeTablesFixture(t *testing.T, version int, ddl ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database.sqlite")
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), strings.Join(ddl, ";\n"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "DatabaseVersion.sbf"), []byte("VERSION:uint32:"+strconv.Itoa(version)+"\n"), 0o600))
	return path
}

func groupRecipientPhones(t *testing.T, st *store.Store) []string {
	t.Helper()
	require := require.New(t)
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT p.phone_number FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.recipient_type = 'to' AND m.source_message_id = ?`),
		"signal:group:"+signalfixture.GroupB64+":"+signalfixture.BobACI+":"+ms(signalfixture.TGroup))
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var phones []string
	for rows.Next() {
		var phone string
		require.NoError(rows.Scan(&phone))
		phones = append(phones, phone)
	}
	require.NoError(rows.Err())
	return phones
}
