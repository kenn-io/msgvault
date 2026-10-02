package storetest

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// NewAccountBenchmarkArchive contains 150k messages and 1001 confirmed masks. The final
// unconfirmed mask occurs once; confirmation must not scan the other rows.
func NewAccountBenchmarkArchive(b *testing.B, size int) (*store.Store, int64, string) {
	b.Helper()
	path := filepath.Join(b.TempDir(), "archive.db")
	st, err := store.OpenForTest(path)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, st.Close()) })
	require.NoError(b, st.InitSchema())
	src, err := st.GetOrCreateSource("gmail", "inbox@example.net")
	require.NoError(b, err)
	conversationID, err := st.EnsureConversation(src.ID, "benchmark", "Synthetic archive")
	require.NoError(b, err)
	confirmations := []store.IdentityConfirmation{{Identifier: src.Identifier, Signals: []string{"manual"}}, {Identifier: "work@example.org", Signals: []string{"manual"}}}
	for i := range 1001 {
		confirmations = append(confirmations, store.IdentityConfirmation{Identifier: fmt.Sprintf("mask%d@example.org", i), Signals: []string{"provider-alias", "fastmail-masked-email"}})
	}
	_, err = st.AddAccountIdentitiesBatchContext(b.Context(), src.ID, confirmations)
	require.NoError(b, err)
	tx, err := st.DB().BeginTx(b.Context(), nil)
	require.NoError(b, err)
	message, err := tx.Prepare(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at,subject,snippet,account_address,account_path,account_attribution_basis) VALUES (?,?,?,'email','2026-01-01','quarterly report','synthetic archived text',?,'inbound','original-recipient')`)
	require.NoError(b, err)
	defer func() { require.NoError(b, message.Close()) }()
	mention, err := tx.Prepare(`INSERT INTO message_account_mentions(source_id,message_id,address_key) VALUES (?,?,?)`)
	require.NoError(b, err)
	defer func() { require.NoError(b, mention.Close()) }()
	evidence, err := tx.Prepare(`INSERT INTO message_account_evidence(message_id,evidence) VALUES (?,?)`)
	require.NoError(b, err)
	defer func() { require.NoError(b, evidence.Close()) }()
	for i := range size {
		address := "inbox@example.net"
		if i%10 == 0 {
			address = "work@example.org"
		}
		if i%10 == 1 {
			address = fmt.Sprintf("mask%d@example.org", i%1001)
		}
		headerAddress := address
		if i == size-1 {
			headerAddress = "newmask@example.org"
		}
		result, err := message.Exec(src.ID, conversationID, strconv.Itoa(i), address)
		require.NoError(b, err)
		id, err := result.LastInsertId()
		require.NoError(b, err)
		_, err = mention.Exec(src.ID, id, headerAddress)
		require.NoError(b, err)
		_, err = evidence.Exec(id, fmt.Sprintf(`{"original":[%q]}`, headerAddress))
		require.NoError(b, err)
	}
	require.NoError(b, tx.Commit())
	_, err = st.DB().Exec(`INSERT INTO messages_fts(message_id,subject,body) SELECT id,'quarterly report','synthetic archived text' FROM messages`)
	require.NoError(b, err)
	_, err = st.DB().Exec("ANALYZE")
	require.NoError(b, err)
	return st, src.ID, path
}
