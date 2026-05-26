package importer

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
	requirepkg.NoError(t, err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	requirepkg.NoError(t, st.InitSchema(), "init schema")
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
	requirepkg.Error(t, err, "expected error for non-existent PST file")
	assertpkg.Empty(t, mock.calls, "expected 0 ingest calls")
}

// TestImportPst_RequiresIdentifier verifies that ImportPst rejects an empty identifier.
func TestImportPst_RequiresIdentifier(t *testing.T) {
	st := openTestStorePst(t)
	_, err := ImportPst(context.Background(), st, "any.pst", PstImportOptions{
		Identifier: "",
	})
	requirepkg.Error(t, err, "expected error for empty identifier")
}

// TestPstCheckpoint_RoundTrip verifies that savePstCheckpoint stores a checkpoint
// that can be decoded back to the original values.
func TestPstCheckpoint_RoundTrip(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := openTestStorePst(t)
	src, err := st.GetOrCreateSource("pst", "user@example.com")
	require.NoError(err, "get/create source")

	syncID, err := st.StartSync(src.ID, "import-pst")
	require.NoError(err, "start sync")

	cp := &store.Checkpoint{
		MessagesProcessed: 42,
		MessagesAdded:     40,
	}
	require.NoError(savePstCheckpoint(st, syncID, "/path/to/file.pst", "abc123", 3, "Inbox/Archive", 100, cp),
		"savePstCheckpoint")

	active, err := st.GetActiveSync(src.ID)
	require.NoError(err, "get active sync")
	require.NotNil(active, "expected active sync")
	require.True(active.CursorBefore.Valid, "expected cursor_before to be set")

	var saved pstCheckpoint
	require.NoError(json.Unmarshal([]byte(active.CursorBefore.String), &saved), "unmarshal checkpoint")

	assert.Equal("/path/to/file.pst", saved.File)
	assert.Equal(3, saved.FolderIndex)
	assert.Equal("Inbox/Archive", saved.FolderPath)
	assert.Equal(int64(100), saved.MsgIndex)
	assert.Equal("abc123", saved.ArchiveID)
}

// TestPstArchiveFingerprint verifies the helper produces stable, distinct
// identifiers per file. Without this, importing two PST archives with the
// same source identifier would collide on PST EntryIDs (which are unique
// only within a single archive) and falsely skip or update unrelated rows.
func TestPstArchiveFingerprint(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dir := t.TempDir()

	// Two files with different headers — fingerprints must differ.
	headerA := append([]byte("!BDN\x00\x00\x00\x00"), make([]byte, 4096)...)
	headerB := append([]byte("!BDN\xff\xff\xff\xff"), make([]byte, 4096)...)
	pathA := filepath.Join(dir, "a.pst")
	pathB := filepath.Join(dir, "b.pst")
	require.NoError(os.WriteFile(pathA, headerA, 0o644))
	require.NoError(os.WriteFile(pathB, headerB, 0o644))

	fpA, err := pstArchiveFingerprint(pathA)
	require.NoError(err, "fingerprint A")
	fpB, err := pstArchiveFingerprint(pathB)
	require.NoError(err, "fingerprint B")

	assert.NotEqual(fpB, fpA, "expected distinct fingerprints")
	assert.Len(fpA, 12, "expected 12-hex-char fingerprints")
	assert.Len(fpB, 12, "expected 12-hex-char fingerprints")

	// Same bytes → same fingerprint, regardless of path. This is what
	// makes re-importing the same file idempotent under the new key.
	pathC := filepath.Join(dir, "renamed.pst")
	require.NoError(os.WriteFile(pathC, headerA, 0o644))
	fpC, err := pstArchiveFingerprint(pathC)
	require.NoError(err, "fingerprint C")
	assert.Equal(fpA, fpC, "same bytes should fingerprint the same")
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
	assertpkg.Error(t, err, "expected error (either ctx or open)")
}
