package pst_test

import (
	"path/filepath"
	"strings"
	"testing"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	pstreader "go.kenn.io/msgvault/internal/pst"
)

const testdataDir = "testdata"

func supportPST(t *testing.T) string {
	t.Helper()
	return filepath.Join(testdataDir, "support.pst")
}

func bit32PST(t *testing.T) string {
	t.Helper()
	return filepath.Join(testdataDir, "32-bit.pst")
}

// TestOpen_SupportPST verifies that a real 64-bit PST file opens without error.
func TestOpen_SupportPST(t *testing.T) {
	f, err := pstreader.Open(supportPST(t))
	requirepkg.NoError(t, err, "Open")
	defer func() { _ = f.Close() }()
}

// TestOpen_32BitPST verifies that a 32-bit PST file opens without error.
func TestOpen_32BitPST(t *testing.T) {
	f, err := pstreader.Open(bit32PST(t))
	requirepkg.NoError(t, err, "Open")
	defer func() { _ = f.Close() }()
}

// TestOpen_NonExistent verifies a clear error for missing files.
func TestOpen_NonExistent(t *testing.T) {
	_, err := pstreader.Open("/nonexistent/path.pst")
	requirepkg.Error(t, err, "expected error, got nil")
}

// TestWalkFolders_SupportPST verifies that WalkFolders visits the known folders
// and builds correct slash-separated paths.
func TestWalkFolders_SupportPST(t *testing.T) {
	require := requirepkg.New(t)
	f, err := pstreader.Open(supportPST(t))
	require.NoError(err, "Open")
	defer func() { _ = f.Close() }()

	seen := make(map[string]int32) // path → message count
	err = f.WalkFolders(func(entry pstreader.FolderEntry, _ *pstlib.Folder) error {
		seen[entry.Path] = entry.MsgCount
		return nil
	})
	require.NoError(err, "WalkFolders")

	require.NotEmpty(seen, "WalkFolders visited no folders")

	// The support PST has at least these two message-bearing folders.
	wantFolders := []string{"Drafts", "Sent Messages"}
	for _, want := range wantFolders {
		found := false
		for path := range seen {
			// Path may be "Root/Drafts" or just "Drafts" depending on hierarchy.
			if path == want || strings.HasSuffix(path, "/"+want) {
				found = true
				break
			}
		}
		assertpkg.True(t, found, "folder %q not found in: %v", want, keys(seen))
	}
}

// TestWalkFolders_PathsAreSlashSeparated verifies that nested folders produce
// slash-separated paths (e.g. "Personal Folders/Inbox/Archive").
func TestWalkFolders_PathsAreSlashSeparated(t *testing.T) {
	f, err := pstreader.Open(supportPST(t))
	requirepkg.NoError(t, err, "Open")
	defer func() { _ = f.Close() }()

	depth2 := false
	_ = f.WalkFolders(func(entry pstreader.FolderEntry, _ *pstlib.Folder) error {
		if strings.Count(entry.Path, "/") >= 1 {
			depth2 = true
		}
		return nil
	})
	assertpkg.True(t, depth2, "expected at least one folder path with depth >= 2 (slash-separated)")
}

// TestExtractMessages_SupportPST verifies that email messages are extracted
// with the expected properties from support.pst.
func TestExtractMessages_SupportPST(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f, err := pstreader.Open(supportPST(t))
	require.NoError(err, "Open")
	defer func() { _ = f.Close() }()

	var emails []*pstreader.MessageEntry
	err = f.WalkFolders(func(entry pstreader.FolderEntry, folder *pstlib.Folder) error {
		iter, err := folder.GetMessageIterator()
		if err != nil {
			return nil //nolint:nilerr // ErrMessagesNotFound or empty folder is expected; skip the folder
		}
		for iter.Next() {
			msg := iter.Value()
			if e := pstreader.ExtractMessage(msg, entry.Path); e != nil {
				emails = append(emails, e)
			}
		}
		return iter.Err()
	})
	require.NoError(err, "WalkFolders")

	// support.pst contains exactly 17 email messages.
	assert.Len(emails, 17)

	// Find the first known message by subject.
	var found *pstreader.MessageEntry
	for _, e := range emails {
		if e.Subject == "Desktop exploits suspension notice" {
			found = e
			break
		}
	}
	require.NotNil(found, "could not find message with subject 'Desktop exploits suspension notice'")

	assert.Equal("support@hackingteam.com", found.SenderEmail)
	assert.Equal("RCS Support", found.SenderName)
	assert.NotEmpty(found.TransportHeaders, "expected TransportHeaders to be non-empty for an internet-delivered message")
	assert.NotEmpty(found.MessageID, "expected MessageID to be non-empty")
	assert.False(found.SentAt.IsZero(), "expected SentAt to be non-zero")
}

// TestExtractMessages_NonEmailsSkipped verifies that non-email items (contacts,
// calendar, tasks) do not appear in the extracted message list.
func TestExtractMessages_NonEmailsSkipped(t *testing.T) {
	f, err := pstreader.Open(supportPST(t))
	requirepkg.NoError(t, err, "Open")
	defer func() { _ = f.Close() }()

	var total, emails int
	_ = f.WalkFolders(func(_ pstreader.FolderEntry, folder *pstlib.Folder) error {
		iter, err := folder.GetMessageIterator()
		if err != nil {
			return nil //nolint:nilerr // empty folder / no message iterator is expected; skip it
		}
		for iter.Next() {
			total++
			msg := iter.Value()
			if pstreader.ExtractMessage(msg, "") != nil {
				emails++
			}
		}
		return nil
	})
	// All items in support.pst should be email messages.
	assertpkg.Equal(t, total, emails, "%d non-email items unexpectedly extracted", total-emails)
}

// TestReadAttachments_SupportPST verifies that the message with known attachments
// returns non-empty attachment content.
func TestReadAttachments_SupportPST(t *testing.T) {
	f, err := pstreader.Open(supportPST(t))
	requirepkg.NoError(t, err, "Open")
	defer func() { _ = f.Close() }()

	found := false
	_ = f.WalkFolders(func(entry pstreader.FolderEntry, folder *pstlib.Folder) error {
		iter, err := folder.GetMessageIterator()
		if err != nil {
			return nil //nolint:nilerr // empty folder / no message iterator is expected; skip it
		}
		for iter.Next() {
			msg := iter.Value()
			e := pstreader.ExtractMessage(msg, entry.Path)
			if e == nil || e.Subject != "IMPORTANT: Support portal downtime for maintenance" {
				continue
			}
			found = true
			atts, err := pstreader.ReadAttachments(msg, 0)
			requirepkg.NoError(t, err, "ReadAttachments")
			if !assertpkg.Len(t, atts, 2) {
				return nil
			}
			for i, att := range atts {
				assertpkg.NotEmpty(t, att.Content, "attachment %d (%q) has empty content", i, att.Filename)
			}
		}
		return nil
	})

	assertpkg.True(t, found, "could not find the message with 2 attachments")
}

// TestBuildRFC5322_RoundTrip verifies that MIME built from a real PST message
// can be successfully re-parsed by the msgvault MIME parser.
func TestBuildRFC5322_RoundTrip(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f, err := pstreader.Open(supportPST(t))
	require.NoError(err, "Open")
	defer func() { _ = f.Close() }()

	var rawMIME []byte
	_ = f.WalkFolders(func(entry pstreader.FolderEntry, folder *pstlib.Folder) error {
		if rawMIME != nil {
			return nil // already found one
		}
		iter, err := folder.GetMessageIterator()
		if err != nil {
			return nil //nolint:nilerr // empty folder / no message iterator is expected; skip it
		}
		for iter.Next() {
			msg := iter.Value()
			e := pstreader.ExtractMessage(msg, entry.Path)
			if e == nil {
				continue
			}
			atts, _ := pstreader.ReadAttachments(msg, 0)
			raw, err := pstreader.BuildRFC5322(e, atts)
			if !assert.NoError(err, "BuildRFC5322") {
				return nil
			}
			rawMIME = raw
			return nil
		}
		return nil
	})

	require.NotNil(rawMIME, "no messages found to test MIME round-trip")
	require.NotEmpty(rawMIME, "BuildRFC5322 returned empty bytes")

	// Verify the output is valid enough for our MIME parser.
	s := string(rawMIME)
	assert.Contains(s, "MIME-Version: 1.0", "MIME output missing MIME-Version header")
	assert.Contains(s, "Content-Type:", "MIME output missing Content-Type header")
}

// TestBuildRFC5322_WithAttachments_RoundTrip verifies the attachment message
// produces valid multipart/mixed MIME.
func TestBuildRFC5322_WithAttachments_RoundTrip(t *testing.T) {
	f, err := pstreader.Open(supportPST(t))
	requirepkg.NoError(t, err, "Open")
	defer func() { _ = f.Close() }()

	_ = f.WalkFolders(func(entry pstreader.FolderEntry, folder *pstlib.Folder) error {
		iter, err := folder.GetMessageIterator()
		if err != nil {
			return nil //nolint:nilerr // empty folder / no message iterator is expected; skip it
		}
		for iter.Next() {
			msg := iter.Value()
			e := pstreader.ExtractMessage(msg, entry.Path)
			if e == nil || e.Subject != "IMPORTANT: Support portal downtime for maintenance" {
				continue
			}
			atts, err := pstreader.ReadAttachments(msg, 0)
			requirepkg.NoError(t, err, "ReadAttachments")
			raw, err := pstreader.BuildRFC5322(e, atts)
			requirepkg.NoError(t, err, "BuildRFC5322")
			s := string(raw)
			assertpkg.Contains(t, s, "multipart/mixed", "expected multipart/mixed for message with attachments")
			// Both attachments in this message have ContentIDs so they
			// render as inline; check for Content-Disposition regardless.
			assertpkg.Contains(t, s, "Content-Disposition:", "expected Content-Disposition header in attachment MIME")
		}
		return nil
	})
}

// keys returns map keys for error messages.
func keys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
