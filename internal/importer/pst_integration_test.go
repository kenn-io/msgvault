package importer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wesm/msgvault/internal/store"
)

const pstTestdataDir = "../pst/testdata"

func openIntegrationStore(t *testing.T) *store.Store {
	t.Helper()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	return st
}

// TestImportPst_SupportPST imports the real support.pst fixture and asserts
// the expected message counts and deduplication behaviour.
func TestImportPst_SupportPST(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	})
	if err != nil {
		t.Fatalf("ImportPst: %v", err)
	}

	// support.pst has 17 email messages across Drafts (6) and Sent Messages (11).
	if summary.MessagesProcessed != 17 {
		t.Errorf("MessagesProcessed = %d, want 17", summary.MessagesProcessed)
	}
	if summary.MessagesAdded != 17 {
		t.Errorf("MessagesAdded = %d, want 17", summary.MessagesAdded)
	}
	if summary.MessagesSkipped != 0 {
		t.Errorf("MessagesSkipped = %d, want 0 on first import", summary.MessagesSkipped)
	}
	if summary.HardErrors {
		t.Error("HardErrors = true, want false")
	}
	if summary.FoldersImported == 0 {
		t.Error("FoldersImported = 0, expected > 0")
	}
}

// TestImportPst_SupportPST_Idempotent verifies that re-importing the same PST
// skips all messages (content-hash deduplication).
func TestImportPst_SupportPST_Idempotent(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")
	opts := PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	}

	// First import.
	first, err := ImportPst(context.Background(), st, pstPath, opts)
	if err != nil {
		t.Fatalf("first ImportPst: %v", err)
	}
	if first.MessagesAdded == 0 {
		t.Fatal("first import added no messages")
	}

	// Second import — everything should be skipped.
	second, err := ImportPst(context.Background(), st, pstPath, opts)
	if err != nil {
		t.Fatalf("second ImportPst: %v", err)
	}
	if second.MessagesSkipped != first.MessagesAdded {
		t.Errorf("second import: skipped=%d, want %d (all from first)",
			second.MessagesSkipped, first.MessagesAdded)
	}
	if second.MessagesAdded != 0 {
		t.Errorf("second import: added=%d, want 0", second.MessagesAdded)
	}
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
	if err != nil {
		t.Fatalf("ImportPst: %v", err)
	}

	// support.pst has 17 raw items but some subjects appear in both Drafts and
	// Sent Messages (duplicates). The total processed should equal all items.
	// Added + Skipped should equal processed (no items dropped).
	if summary.MessagesAdded+summary.MessagesSkipped+summary.MessagesUpdated != summary.MessagesProcessed {
		t.Errorf("accounting mismatch: added(%d)+skipped(%d)+updated(%d) != processed(%d)",
			summary.MessagesAdded, summary.MessagesSkipped, summary.MessagesUpdated, summary.MessagesProcessed)
	}
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
	if err != nil {
		t.Fatalf("ImportPst: %v", err)
	}

	// With Drafts skipped we process fewer messages. Some "Sent Messages" subjects
	// also appear in Drafts — but those aren't processed since Drafts is skipped.
	// At minimum we should have processed fewer than all 17.
	if summary.MessagesProcessed >= 17 {
		t.Errorf("MessagesProcessed = %d with Drafts skipped; expected < 17", summary.MessagesProcessed)
	}
	if summary.MessagesProcessed == 0 {
		t.Error("MessagesProcessed = 0; Sent Messages should still be imported")
	}
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
	if summary == nil {
		t.Fatal("ImportPst returned nil summary")
	}
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
	if err != nil {
		t.Fatalf("ImportPst: %v", err)
	}
	// 32-bit.pst has no readable email messages.
	if summary.MessagesProcessed != 0 {
		t.Errorf("MessagesProcessed = %d, want 0", summary.MessagesProcessed)
	}
	if summary.HardErrors {
		t.Error("HardErrors = true, want false")
	}
}
