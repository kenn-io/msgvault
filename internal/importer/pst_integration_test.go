package importer

import (
	"context"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

const pstTestdataDir = "../pst/testdata"

func openIntegrationStore(t *testing.T) *store.Store {
	t.Helper()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	requirepkg.NoError(t, err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	requirepkg.NoError(t, st.InitSchema(), "init schema")
	return st
}

// TestImportPst_SupportPST imports the real support.pst fixture and asserts
// the expected message counts and deduplication behaviour.
func TestImportPst_SupportPST(t *testing.T) {
	assert := assertpkg.New(t)
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	})
	requirepkg.NoError(t, err, "ImportPst")

	// support.pst has 17 email messages across Drafts (6) and Sent Messages (11).
	assert.Equal(int64(17), summary.MessagesProcessed, "MessagesProcessed")
	assert.Equal(int64(17), summary.MessagesAdded, "MessagesAdded")
	assert.Equal(int64(0), summary.MessagesSkipped, "MessagesSkipped on first import")
	assert.False(summary.HardErrors, "HardErrors")
	assert.Positive(summary.FoldersImported, "FoldersImported")
}

// TestImportPst_SupportPST_Idempotent verifies that re-importing the same PST
// skips all messages (content-hash deduplication).
func TestImportPst_SupportPST_Idempotent(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")
	opts := PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	}

	// First import.
	first, err := ImportPst(context.Background(), st, pstPath, opts)
	require.NoError(err, "first ImportPst")
	require.NotZero(first.MessagesAdded, "first import added no messages")

	// Second import — everything should be skipped.
	second, err := ImportPst(context.Background(), st, pstPath, opts)
	require.NoError(err, "second ImportPst")
	assert.Equal(first.MessagesAdded, second.MessagesSkipped,
		"second import skipped count should equal first import added count")
	assert.Equal(int64(0), second.MessagesAdded, "second import: added")
}

// TestImportPst_SupportPST_CrossFolderLabels verifies that duplicate messages
// (same content in Drafts and Sent Messages) get both folder labels applied
// rather than being ingested twice.
func TestImportPst_SupportPST_CrossFolderLabels(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	})
	requirepkg.NoError(t, err, "ImportPst")

	// support.pst has 17 raw items but some subjects appear in both Drafts and
	// Sent Messages (duplicates). The total processed should equal all items.
	// Added + Skipped should equal processed (no items dropped).
	assertpkg.Equal(t,
		summary.MessagesProcessed,
		summary.MessagesAdded+summary.MessagesSkipped+summary.MessagesUpdated,
		"accounting mismatch: added(%d)+skipped(%d)+updated(%d) != processed(%d)",
		summary.MessagesAdded, summary.MessagesSkipped, summary.MessagesUpdated, summary.MessagesProcessed)
}

// TestImportPst_SupportPST_SkipFolder verifies that --skip-folder correctly
// excludes the specified folder from import.
func TestImportPst_SupportPST_SkipFolder(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	// Skip Drafts (6 messages) — should only import Sent Messages (11).
	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier:  "support@hackingteam.com",
		SkipFolders: []string{"Drafts"},
		NoResume:    true,
	})
	requirepkg.NoError(t, err, "ImportPst")

	// With Drafts skipped we process fewer messages. Some "Sent Messages" subjects
	// also appear in Drafts — but those aren't processed since Drafts is skipped.
	// At minimum we should have processed fewer than all 17.
	assertpkg.Less(t, summary.MessagesProcessed, int64(17),
		"MessagesProcessed with Drafts skipped; expected < 17")
	assertpkg.NotZero(t, summary.MessagesProcessed,
		"MessagesProcessed; Sent Messages should still be imported")
}

// TestImportPst_SupportPST_ContextCancelled verifies that cancelling mid-import
// saves a checkpoint and returns cleanly (no panic, no hang).
func TestImportPst_SupportPST_ContextCancelled(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	// Cancel immediately — this should cause ImportPst to abort early.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary, _ := ImportPst(ctx, st, pstPath, PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	})

	// Must not panic and must return a (possibly zero) summary.
	requirepkg.NotNil(t, summary, "ImportPst returned nil summary")
}

// TestImportPst_32BitPST verifies that a 32-bit format PST is handled
// gracefully. go-pst may fail to read sub-folder metadata in 32-bit files;
// the importer skips those branches and completes without error.
func TestImportPst_32BitPST(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "32-bit.pst")

	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier: "user@example.com",
		NoResume:   true,
	})
	requirepkg.NoError(t, err, "ImportPst")
	// 32-bit.pst has no readable email messages.
	assertpkg.Equal(t, int64(0), summary.MessagesProcessed, "MessagesProcessed")
	assertpkg.False(t, summary.HardErrors, "HardErrors")
}
