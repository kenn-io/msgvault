package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func receivedIDs(t *testing.T, st *store.Store, address string) []int64 {
	t.Helper()
	results, _, err := st.SearchMessagesQuery(search.Parse("received:"+address), 0, 100)
	require.NoError(t, err)
	ids := make([]int64, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestImportMboxHealsPendingAccountAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	dir := t.TempDir()
	write := func(name, messageID string) string {
		raw := "From sender@example.test Mon Jan 1 12:00:00 +0000 2024\r\nFrom: Sender <sender@example.test>\r\n" +
			"X-Delivered-To: work@example.org\r\nTo: list@example.test\r\nMessage-ID: <" + messageID + "@example.test>\r\n" +
			"Subject: Synthetic\r\n\r\nSynthetic message.\r\n"
		path := filepath.Join(dir, name)
		require.NoError(os.WriteFile(path, []byte(raw), 0600))
		return path
	}
	opts := MboxImportOptions{SourceType: "mbox", Identifier: "archive@example.net", NoResume: true}
	_, err := ImportMbox(t.Context(), st, write("one.mbox", "one"), opts)
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("mbox", "archive@example.net")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "work@example.org", "manual"))
	ledger := "rederive:account-attribution:mbox:archive@example.net:v2"
	legacy := func(dropLedger bool) {
		_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET account_address = NULL, account_path = NULL WHERE source_id = ?`), source.ID)
		require.NoError(err)
		if dropLedger {
			_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), ledger)
			require.NoError(err)
		}
	}
	legacy(true)
	require.Empty(receivedIDs(t, st, "work@example.org"))

	_, err = ImportMbox(t.Context(), st, write("two.mbox", "two"), opts)
	require.NoError(err)
	assert.Len(receivedIDs(t, st, "work@example.org"), 2, "the next import heals the archived message")

	legacy(false)
	_, err = ImportMbox(t.Context(), st, write("three.mbox", "three"), opts)
	require.NoError(err)
	assert.Len(receivedIDs(t, st, "work@example.org"), 1, "a recorded pass is skipped; only the new message derives")
}
