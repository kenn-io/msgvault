package cmd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

// --full-reconcile must reach the importer in both account modes: a plain
// rerun trusts the receipt, while reconciliation repairs a removed search row.
func TestImportEmlxCommandFullReconcileRepairsSearch(t *testing.T) {
	for _, mode := range []string{"manual", "auto"} {
		t.Run(mode, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			home, err := filepath.EvalSymlinks(t.TempDir())
			r.NoError(err)
			mail := filepath.Join(home, "Mail")
			const guid = "11111111-2222-3333-4444-555555555555"
			account := filepath.Join(mail, "V10", guid)
			raw := email.NewMessage().From("sender@example.test").To("owner@example.test").
				Header("Message-ID", "<reconcile-cli@example.test>").Body("reconcilecommandneedle").Bytes()
			emlxBudgetWriteFile(t, filepath.Join(account, "Inbox.mbox"), "1.emlx", raw)
			run := emlxBudgetCommand(t, home)
			args := []string{account, "--identifier", "owner@example.test"}
			if mode == "auto" {
				args = []string{mail, "--accounts-db", emlxBudgetAccountsDB(t, home, guid)}
			}
			indexed := func() int {
				t.Helper()
				st, err := store.Open(filepath.Join(home, "msgvault.db"))
				r.NoError(err)
				defer func() { r.NoError(st.Close()) }()
				var n int
				r.NoError(st.DB().QueryRow(
					`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'reconcilecommandneedle'`).Scan(&n))
				return n
			}

			_, err = run(args...)
			r.NoError(err)
			r.Equal(1, indexed())
			st, err := store.Open(filepath.Join(home, "msgvault.db"))
			r.NoError(err)
			_, err = st.DB().Exec(`DELETE FROM messages_fts`)
			r.NoError(err)
			r.NoError(st.Close())

			_, err = run(args...)
			r.NoError(err)
			a.Zero(indexed(), "a plain rerun trusts the completed receipt")
			_, err = run(append(args, "--full-reconcile")...)
			r.NoError(err)
			a.Equal(1, indexed(), "--full-reconcile repairs the search entry")
		})
	}
}
