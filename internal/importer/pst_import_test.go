package importer

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// mockIngestFunc records IngestRawMessage calls for inspection in tests.
type mockIngestFunc struct {
	calls []mockIngestCall
	err   error
}

type mockIngestCall struct {
	SourceID     int64
	Identifier   string
	SourceMsgID  string
	RawHash      string
	LabelIDs     []int64
	FallbackDate time.Time
	RawLen       int
}

func (m *mockIngestFunc) fn(
	ctx context.Context, st *store.Store,
	sourceID int64, identifier, attachmentsDir string,
	labelIDs []int64, sourceMsgID, rawHash string,
	raw []byte, fallbackDate time.Time,
	log *slog.Logger,
) error {
	m.calls = append(m.calls, mockIngestCall{
		SourceID:     sourceID,
		Identifier:   identifier,
		SourceMsgID:  sourceMsgID,
		RawHash:      rawHash,
		LabelIDs:     append([]int64(nil), labelIDs...),
		FallbackDate: fallbackDate,
		RawLen:       len(raw),
	})
	return m.err
}

func openTestStorePst(t *testing.T) *store.Store {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	return st
}

// TestImportPst_MissingFile verifies that ImportPst returns an error for a
// non-existent PST file without panicking or corrupting the database.
func TestImportPst_MissingFile(t *testing.T) {
	st := openTestStorePst(t)
	mock := &mockIngestFunc{}

	_, err := ImportPst(context.Background(), st, "/nonexistent/path.pst", PstImportOptions{
		Identifier: "user@example.com",
		NoResume:   true,
		IngestFunc: mock.fn,
	})
	if err == nil {
		t.Fatal("expected error for non-existent PST file, got nil")
	}
	if len(mock.calls) != 0 {
		t.Errorf("expected 0 ingest calls, got %d", len(mock.calls))
	}
}

// TestImportPst_RequiresIdentifier verifies that ImportPst rejects an empty identifier.
func TestImportPst_RequiresIdentifier(t *testing.T) {
	st := openTestStorePst(t)
	_, err := ImportPst(context.Background(), st, "any.pst", PstImportOptions{
		Identifier: "",
	})
	if err == nil {
		t.Fatal("expected error for empty identifier")
	}
}

// TestPstCheckpoint_RoundTrip verifies that savePstCheckpoint stores a checkpoint
// that can be decoded back to the original values.
func TestPstCheckpoint_RoundTrip(t *testing.T) {
	st := openTestStorePst(t)
	src, err := st.GetOrCreateSource("pst", "user@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}

	syncID, err := st.StartSync(src.ID, "import-pst")
	if err != nil {
		t.Fatalf("start sync: %v", err)
	}

	cp := &store.Checkpoint{
		MessagesProcessed: 42,
		MessagesAdded:     40,
	}
	if err := savePstCheckpoint(st, syncID, "/path/to/file.pst", 3, "Inbox/Archive", 100, cp); err != nil {
		t.Fatalf("savePstCheckpoint: %v", err)
	}

	active, err := st.GetActiveSync(src.ID)
	if err != nil {
		t.Fatalf("get active sync: %v", err)
	}
	if active == nil {
		t.Fatal("expected active sync, got nil")
	}
	if !active.CursorBefore.Valid {
		t.Fatal("expected cursor_before to be set")
	}

	var saved pstCheckpoint
	if err := json.Unmarshal([]byte(active.CursorBefore.String), &saved); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}

	if saved.File != "/path/to/file.pst" {
		t.Errorf("File = %q, want %q", saved.File, "/path/to/file.pst")
	}
	if saved.FolderIndex != 3 {
		t.Errorf("FolderIndex = %d, want 3", saved.FolderIndex)
	}
	if saved.FolderPath != "Inbox/Archive" {
		t.Errorf("FolderPath = %q, want %q", saved.FolderPath, "Inbox/Archive")
	}
	if saved.MsgIndex != 100 {
		t.Errorf("MsgIndex = %d, want 100", saved.MsgIndex)
	}
}

// TestPstArchiveFingerprint verifies the helper produces stable, distinct
// identifiers per file. Without this, importing two PST archives with the
// same source identifier would collide on PST EntryIDs (which are unique
// only within a single archive) and falsely skip or update unrelated rows.
func TestPstArchiveFingerprint(t *testing.T) {
	dir := t.TempDir()

	// Two files with different headers — fingerprints must differ.
	headerA := append([]byte("!BDN\x00\x00\x00\x00"), make([]byte, 4096)...)
	headerB := append([]byte("!BDN\xff\xff\xff\xff"), make([]byte, 4096)...)
	pathA := filepath.Join(dir, "a.pst")
	pathB := filepath.Join(dir, "b.pst")
	if err := os.WriteFile(pathA, headerA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, headerB, 0o644); err != nil {
		t.Fatal(err)
	}

	fpA, err := pstArchiveFingerprint(pathA)
	if err != nil {
		t.Fatalf("fingerprint A: %v", err)
	}
	fpB, err := pstArchiveFingerprint(pathB)
	if err != nil {
		t.Fatalf("fingerprint B: %v", err)
	}

	if fpA == fpB {
		t.Errorf("expected distinct fingerprints, got %q twice", fpA)
	}
	if len(fpA) != 12 || len(fpB) != 12 {
		t.Errorf("expected 12-hex-char fingerprints, got %q (%d) and %q (%d)",
			fpA, len(fpA), fpB, len(fpB))
	}

	// Same bytes → same fingerprint, regardless of path. This is what
	// makes re-importing the same file idempotent under the new key.
	pathC := filepath.Join(dir, "renamed.pst")
	if err := os.WriteFile(pathC, headerA, 0o644); err != nil {
		t.Fatal(err)
	}
	fpC, err := pstArchiveFingerprint(pathC)
	if err != nil {
		t.Fatalf("fingerprint C: %v", err)
	}
	if fpC != fpA {
		t.Errorf("same bytes should fingerprint the same: %q vs %q", fpA, fpC)
	}
}

// TestImportPst_ContextCancelledBeforeOpen ensures that context cancellation
// before the PST file is opened is handled gracefully.
func TestImportPst_ContextCancelledBeforeOpen(t *testing.T) {
	st := openTestStorePst(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Use a non-existent path so Open fails fast.
	_, err := ImportPst(ctx, st, "/nonexistent.pst", PstImportOptions{
		Identifier: "user@example.com",
		NoResume:   true,
	})
	// Either ctx error or open error is acceptable — we just must not hang.
	if err == nil {
		t.Error("expected error (either ctx or open), got nil")
	}
}
