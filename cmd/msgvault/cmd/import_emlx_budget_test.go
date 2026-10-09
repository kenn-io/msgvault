package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// Both option literals must reach the real merge: losing either CLI value
// prevents the operator from completing this previously archived message.
func TestImportEmlxCommandBudgetRetry(t *testing.T) {
	for _, mode := range []string{"manual", "auto"} {
		t.Run(mode, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			home, err := filepath.EvalSymlinks(t.TempDir())
			r.NoError(err)
			mail := filepath.Join(home, "Mail")
			const guid = "11111111-2222-3333-4444-555555555555"
			account := filepath.Join(mail, "V10", guid)
			mbox := filepath.Join(account, "Inbox.mbox")
			raw := emlxBudgetPartial()
			oldPart, newPart := bytes.Repeat([]byte("a"), 200), bytes.Repeat([]byte("b"), 200)
			emlxBudgetWriteFile(t, mbox, "1.partial.emlx", raw)
			emlxBudgetWritePart(t, mbox, "1", "2", "one.bin", oldPart)
			budget := int64(len(raw) + 350)
			st, err := store.Open(filepath.Join(home, "msgvault.db"))
			r.NoError(err)
			r.NoError(st.InitSchema())
			seed, err := importer.ImportEmlxDir(t.Context(), st, account, importer.EmlxImportOptions{
				Identifier: "owner@example.test", AttachmentsDir: filepath.Join(home, "attachments"), MaxMessageBytes: budget,
			})
			r.NoError(err)
			r.False(seed.HardErrors, "seed must actually archive the first part")
			r.Equal(int64(1), seed.MessagesAdded)
			r.NoError(st.Close())
			emlxBudgetWriteFile(t, mbox, "2.partial.emlx", raw)
			emlxBudgetWritePart(t, mbox, "2", "3", "two.bin", newPart)

			run := emlxBudgetCommand(t, home)
			args := []string{account, "--identifier", "owner@example.test"}
			if mode == "auto" {
				args = []string{mail, "--accounts-db", emlxBudgetAccountsDB(t, home, guid)}
			}
			output, err := run(append(args, "--max-message-bytes", strconv.FormatInt(budget, 10))...)
			r.NoError(err, "a budget-limited merge is retryable, not a failed import")
			a.Contains(output, "Import complete (with errors).")
			emlxBudgetCheckArchive(t, home, oldPart, nil, 1)

			largeArgs := slices.Concat(args, []string{"--max-message-bytes", strconv.Itoa(len(raw) + 700)})
			_, err = run(largeArgs...)
			r.NoError(err, "a larger operator limit must complete the preserved-parts merge")
			emlxBudgetCheckArchive(t, home, oldPart, newPart, 2)
			output, err = run(largeArgs...)
			r.NoError(err)
			if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
				a.Contains(output, "Unchanged:      2 files (content reads avoided)")
			}
		})
	}
}

func TestImportEmlxCommandBudgetInvalidBeforeImport(t *testing.T) {
	for _, value := range []string{"0", "-1", "9223372036854775807", "not-bytes", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			home := t.TempDir()
			run := emlxBudgetCommand(t, home)
			_, err := run("--identifier", "owner@example.test", filepath.Join(home, "Mail"), "--max-message-bytes", value)
			r.ErrorContains(err, "max-message-bytes")
			if value == "0" || value == "-1" {
				r.ErrorContains(err, "positive")
			}
			if value == "9223372036854775807" {
				r.ErrorContains(err, "9223372036854775806")
			}
			_, err = os.Stat(filepath.Join(home, "msgvault.db"))
			a.ErrorIs(err, os.ErrNotExist, "invalid input must not open the archive")
		})
	}
}

func TestImportEmlxCommandBudgetForwarding(t *testing.T) {
	for _, value := range []string{"4096", "9223372036854775806"} {
		t.Run(value, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			_ = emlxBudgetCommand(t, t.TempDir())
			r.NoError(importEmlxCmd.ParseFlags([]string{"--max-message-bytes", value}), "operator flag must be accepted")
			args, err := daemonCLIArgsFromCobra(importEmlxCmd, []string{"Mail"})
			r.NoError(err)
			a.Equal([]string{"import-emlx", "--max-message-bytes=" + value, "Mail"}, args)
		})
	}
}

func TestImportEmlxCommandBudgetInvalidBeforeForwarding(t *testing.T) {
	for _, value := range []string{"0", "-1", "9223372036854775807"} {
		t.Run(value, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			home := t.TempDir()
			_ = emlxBudgetCommand(t, home)
			t.Setenv(daemonCLISubprocessEnv, "")
			server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
			cfg := testConfigValue()
			cfg.HomeDir = home
			importEmlxCmd.SetContext(configureRemoteDaemonForTest(t, server.URL, cfg))
			r.NoError(importEmlxCmd.ParseFlags([]string{"--max-message-bytes", value}), "operator flag must be accepted")
			err := importEmlxCmd.RunE(importEmlxCmd, []string{"Mail"})
			r.ErrorContains(err, "--max-message-bytes")
			if value == "9223372036854775807" {
				r.ErrorContains(err, "9223372036854775806")
			} else {
				r.ErrorContains(err, "positive")
			}
			a.Zero(requests.Load(), "invalid values must fail in the caller before daemon forwarding")
			_, err = os.Stat(filepath.Join(home, "msgvault.db"))
			a.ErrorIs(err, os.ErrNotExist)
		})
	}
}

// Use the registered command and real invocation lifecycle. The daemon runs
// each invocation in a fresh process, so command flags start from defaults.
func emlxBudgetCommand(t *testing.T, home string) func(...string) (string, error) {
	t.Helper()
	r := require.New(t)
	markDaemonCLISubprocessForTest(t)
	oldOut, oldErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	oldRootCtx, oldCmdCtx := rootCmd.Context(), importEmlxCmd.Context()
	t.Cleanup(func() {
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetArgs(nil)
		rootCmd.SetContext(oldRootCtx)
		importEmlxCmd.SetContext(oldCmdCtx)
	})
	for _, name := range []string{"identifier", "source-type", "no-default-identity", "accounts-db", "full-reconcile", "no-resume", "max-message-bytes"} {
		if flag := importEmlxCmd.Flags().Lookup(name); flag != nil {
			previous, changed := flag.Value.String(), flag.Changed
			t.Cleanup(func() { r.NoError(flag.Value.Set(previous)); flag.Changed = changed })
			r.NoError(flag.Value.Set(flag.DefValue))
			flag.Changed = false
		}
	}
	return func(args ...string) (string, error) {
		for _, name := range []string{"identifier", "accounts-db", "full-reconcile", "no-resume", "max-message-bytes"} {
			flag := importEmlxCmd.Flags().Lookup(name)
			r.NoError(flag.Value.Set(flag.DefValue))
			flag.Changed = false
		}
		var output bytes.Buffer
		rootCmd.SetOut(&output)
		rootCmd.SetErr(&output)
		rootCmd.SetArgs(append([]string{"--home", home, "--no-log-file", "import-emlx", "--no-default-identity"}, args...))
		err := executeRootContext(t.Context(), rootCmd)
		return output.String(), err
	}
}

func emlxBudgetPartial() []byte {
	return []byte("From: sender@example.test\nTo: owner@example.test\nSubject: Budget recovery\nMessage-ID: <budget-cli@example.test>\nMIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=\"=-b\"\n\n" +
		"--=-b\nContent-Type: text/plain; charset=utf-8\n\nbudgetrecoveryneedle\n\n" +
		"--=-b\nContent-Transfer-Encoding: base64\nContent-Disposition: attachment; filename=\"one.bin\"\nContent-Type: application/octet-stream\nX-Apple-Content-Length: 12\n\n\n" +
		"--=-b\nContent-Transfer-Encoding: base64\nContent-Disposition: attachment; filename=\"two.bin\"\nContent-Type: application/octet-stream\nX-Apple-Content-Length: 12\n\n\n--=-b--\n")
}

func emlxBudgetWriteFile(t *testing.T, mbox, name string, raw []byte) {
	t.Helper()
	r := require.New(t)
	dir := filepath.Join(mbox, "Messages")
	r.NoError(os.MkdirAll(dir, 0700))
	r.NoError(os.WriteFile(filepath.Join(dir, name), fmt.Appendf(nil, "%d\n%s", len(raw), raw), 0600))
}

func emlxBudgetWritePart(t *testing.T, mbox, occurrence, part, name string, content []byte) {
	t.Helper()
	r := require.New(t)
	dir := filepath.Join(mbox, "Attachments", occurrence, part)
	r.NoError(os.MkdirAll(dir, 0700))
	r.NoError(os.WriteFile(filepath.Join(dir, name), content, 0600))
}

func emlxBudgetAccountsDB(t *testing.T, home, guid string) string {
	t.Helper()
	r := require.New(t)
	path := filepath.Join(home, "Accounts4.sqlite")
	db, err := sql.Open("sqlite3", path)
	r.NoError(err)
	defer func() { r.NoError(db.Close()) }()
	_, err = db.Exec(`CREATE TABLE ZACCOUNT (Z_PK INTEGER PRIMARY KEY, ZIDENTIFIER TEXT, ZUSERNAME TEXT, ZACCOUNTDESCRIPTION TEXT, ZPARENTACCOUNT INTEGER)`)
	r.NoError(err)
	_, err = db.Exec(`INSERT INTO ZACCOUNT VALUES (1, ?, 'owner@example.test', 'Synthetic account', NULL)`, guid)
	r.NoError(err)
	return path
}

func emlxBudgetCheckArchive(t *testing.T, home string, oldPart, newPart []byte, completed int) {
	t.Helper()
	r, a := require.New(t), assert.New(t)
	st, err := store.Open(filepath.Join(home, "msgvault.db"))
	r.NoError(err)
	defer func() { r.NoError(st.Close()) }()
	var mid int64
	r.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE rfc822_message_id = 'budget-cli@example.test'`).Scan(&mid))
	raw, err := st.GetMessageRawContext(t.Context(), mid)
	r.NoError(err)
	parsed, err := mime.Parse(raw)
	r.NoError(err)
	r.Len(parsed.Attachments, 2)
	a.Equal(oldPart, parsed.Attachments[0].Content)
	if newPart == nil {
		a.Empty(parsed.Attachments[1].Content)
	} else {
		a.Equal(newPart, parsed.Attachments[1].Content)
		for _, part := range parsed.Attachments {
			var path string
			r.NoError(st.DB().QueryRow(`SELECT storage_path FROM attachments WHERE message_id = ? AND filename = ? AND COALESCE(content_hash, '') <> ''`, mid, part.Filename).Scan(&path))
			blob, err := os.ReadFile(filepath.Join(home, "attachments", filepath.FromSlash(path)))
			r.NoError(err)
			a.Equal(part.Content, blob)
		}
	}
	labels, err := st.MessageLabelIDsContext(t.Context(), mid)
	r.NoError(err)
	a.Len(labels, 1)
	r.True(st.FTS5Available())
	var indexed, receipts int
	r.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'budgetrecoveryneedle'`).Scan(&indexed))
	a.Equal(1, indexed)
	r.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM source_import_items WHERE provider = 'emlx-occurrence' AND status = 'imported'`).Scan(&receipts))
	// Platforms without filesystem fingerprints still complete receipts; they
	// only lack the signature that lets a rerun skip content reads.
	a.Equal(completed, receipts)
}
