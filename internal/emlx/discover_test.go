package emlx

import (
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// testMailboxGUID is the synthetic mailbox GUID used by the V10 fixtures.
const testMailboxGUID = "9F0F15DD-4CBC-448A-9EBF-C385A47A3A67"

// mkMailbox creates a mock legacy Apple Mail mailbox structure
// with a direct Messages/ subdirectory.
func mkMailbox(t *testing.T, base string, emlxFiles ...string) {
	t.Helper()
	msgDir := filepath.Join(base, "Messages")
	requirepkg.NoError(t, os.MkdirAll(msgDir, 0700), "mkdir %q", msgDir)
	for _, name := range emlxFiles {
		data := "10\nFrom: x\r\n\r\n"
		path := filepath.Join(msgDir, name)
		requirepkg.NoError(t, os.WriteFile(path, []byte(data), 0600), "write %q", path)
	}
}

// mkV10Mailbox creates a modern V10-style mailbox structure
// with <GUID>/Data/Messages/ layout.
func mkV10Mailbox(
	t *testing.T, base string, emlxFiles ...string,
) {
	t.Helper()
	msgDir := filepath.Join(base, testMailboxGUID, "Data", "Messages")
	requirepkg.NoError(t, os.MkdirAll(msgDir, 0700), "mkdir %q", msgDir)
	for _, name := range emlxFiles {
		data := "10\nFrom: x\r\n\r\n"
		path := filepath.Join(msgDir, name)
		requirepkg.NoError(t, os.WriteFile(path, []byte(data), 0600), "write %q", path)
	}
}

func TestDiscoverMailboxes_SingleMailbox(t *testing.T) {
	require := requirepkg.New(t)
	root := t.TempDir()
	mboxDir := filepath.Join(root, "INBOX.mbox")
	mkMailbox(t, mboxDir, "1.emlx", "2.emlx")

	// Pass the .mbox directory itself.
	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1)
	require.Equal("INBOX", mailboxes[0].Label)
	require.Len(mailboxes[0].Files, 2)
}

func TestDiscoverMailboxes_RecursiveWalk(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := t.TempDir()
	mail := filepath.Join(root, "Mail")

	mkMailbox(t, filepath.Join(mail, "Mailboxes", "Classes", "Accardi.mbox"), "1.emlx")
	mkMailbox(t, filepath.Join(mail, "Mailboxes", "Sent.mbox"), "1.emlx", "2.emlx")
	mkMailbox(t, filepath.Join(mail, "IMAP-wesm@po14.mit.edu", "INBOX.imapmbox"), "1.emlx")
	mkMailbox(t, filepath.Join(mail, "POP-wesmckinn@pop.gmail.com", "Sent Messages.mbox"), "1.emlx")

	mailboxes, err := DiscoverMailboxes(mail)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 4)

	labels := make(map[string]int)
	for _, mb := range mailboxes {
		labels[mb.Label] = len(mb.Files)
	}

	tests := []struct {
		label     string
		wantFiles int
	}{
		{"Classes/Accardi", 1},
		{"Sent", 2},
		{"INBOX", 1},
		{"Sent Messages", 1},
	}
	for _, tc := range tests {
		n, ok := labels[tc.label]
		if !assert.True(ok, "missing label %q (have: %v)", tc.label, labels) {
			continue
		}
		assert.Equal(tc.wantFiles, n, "label %q files", tc.label)
	}
}

func TestDiscoverMailboxes_EmptyMailbox(t *testing.T) {
	root := t.TempDir()
	mboxDir := filepath.Join(root, "Empty.mbox")
	// Create Messages/ but no .emlx files.
	requirepkg.NoError(t, os.MkdirAll(filepath.Join(mboxDir, "Messages"), 0700), "mkdir")

	mailboxes, err := DiscoverMailboxes(root)
	requirepkg.NoError(t, err, "DiscoverMailboxes")
	requirepkg.Empty(t, mailboxes)
}

func TestDiscoverMailboxes_PartialEmlxSkipped(t *testing.T) {
	require := requirepkg.New(t)
	root := t.TempDir()
	mboxDir := filepath.Join(root, "Test.mbox")
	mkMailbox(t, mboxDir, "1.emlx", "2.partial.emlx")

	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1)
	require.Len(mailboxes[0].Files, 1, "partial should be skipped")
	require.Equal("1.emlx", filepath.Base(mailboxes[0].Files[0]))
}

func TestDiscoverMailboxes_NotADirectory(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "notdir")
	requirepkg.NoError(t, os.WriteFile(file, []byte("x"), 0600), "write")

	_, err := DiscoverMailboxes(file)
	requirepkg.Error(t, err, "expected error for non-directory")
}

func TestDiscoverMailboxes_NestedMbox(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := t.TempDir()
	mail := filepath.Join(root, "Mail")

	// Parent .mbox contains a child .mbox inside it.
	// Apple Mail sometimes nests mailboxes this way.
	mkMailbox(t, filepath.Join(mail, "Parent.mbox"), "1.emlx")
	mkMailbox(t, filepath.Join(mail, "Parent.mbox", "Child.mbox"), "1.emlx")

	mailboxes, err := DiscoverMailboxes(mail)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 2)

	labels := make(map[string]bool)
	for _, mb := range mailboxes {
		labels[mb.Label] = true
	}
	assert.True(labels["Parent"], "missing Parent label, have: %v", labels)
	assert.True(labels["Parent/Child"], "missing Parent/Child label, have: %v", labels)
}

func TestLabelFromPath(t *testing.T) {
	tests := []struct {
		root string
		path string
		want string
	}{
		{
			"/Mail",
			"/Mail/Mailboxes/Classes/Accardi.mbox",
			"Classes/Accardi",
		},
		{
			"/Mail",
			"/Mail/IMAP-wesm@po14.mit.edu/INBOX.imapmbox",
			"INBOX",
		},
		{
			"/Mail",
			"/Mail/POP-wesmckinn@pop.gmail.com/Sent Messages.mbox",
			"Sent Messages",
		},
		{
			"/Mail",
			"/Mail/Mailboxes/Sent.mbox",
			"Sent",
		},
		{
			"/Mail",
			"/Mail/INBOX.mbox",
			"INBOX",
		},
		// V10 account GUID stripped from labels.
		{
			"/Mail/V10",
			"/Mail/V10/13C9A646-EE0A-4698-B5A2-E07FFBDDEED3/INBOX.mbox",
			"INBOX",
		},
		{
			"/Mail/V10",
			"/Mail/V10/13C9A646-EE0A-4698-B5A2-E07FFBDDEED3/Sent Messages.mbox",
			"Sent Messages",
		},
	}
	for _, tc := range tests {
		got := LabelFromPath(tc.root, tc.path)
		assertpkg.Equal(t, tc.want, got, "LabelFromPath(%q, %q)", tc.root, tc.path)
	}
}

func TestDiscoverMailboxes_FilesSorted(t *testing.T) {
	require := requirepkg.New(t)
	root := t.TempDir()
	mboxDir := filepath.Join(root, "Test.mbox")
	mkMailbox(t, mboxDir, "300.emlx", "10.emlx", "2.emlx", "1.emlx")

	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1)

	wantNames := []string{"1.emlx", "10.emlx", "2.emlx", "300.emlx"}
	files := mailboxes[0].Files
	require.Len(files, len(wantNames))
	for i := range wantNames {
		require.Equal(wantNames[i], filepath.Base(files[i]), "files[%d] basename", i)
	}
}

func TestDiscoverMailboxes_V10Layout(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := t.TempDir()
	v10 := filepath.Join(root, "V10")
	acctGUID := "13C9A646-EE0A-4698-B5A2-E07FFBDDEED3"
	acctDir := filepath.Join(v10, acctGUID)

	mkV10Mailbox(t,
		filepath.Join(acctDir, "INBOX.mbox"),
		"1.emlx", "2.emlx", "3.emlx",
	)
	mkV10Mailbox(t,
		filepath.Join(acctDir, "Sent Messages.mbox"),
		"10.emlx",
	)
	mkV10Mailbox(t,
		filepath.Join(acctDir, "Junk.mbox"),
		"27.emlx", "28.emlx",
	)

	mailboxes, err := DiscoverMailboxes(v10)
	require.NoError(err, "DiscoverMailboxes")
	if len(mailboxes) != 3 {
		for _, mb := range mailboxes {
			t.Logf("  label=%q path=%q files=%d",
				mb.Label, mb.Path, len(mb.Files))
		}
	}
	require.Len(mailboxes, 3)

	labels := make(map[string]int)
	for _, mb := range mailboxes {
		labels[mb.Label] = len(mb.Files)
	}

	tests := []struct {
		label     string
		wantFiles int
	}{
		{"INBOX", 3},
		{"Sent Messages", 1},
		{"Junk", 2},
	}
	for _, tc := range tests {
		n, ok := labels[tc.label]
		if !assert.True(ok, "missing label %q (have: %v)", tc.label, labels) {
			continue
		}
		assert.Equal(tc.wantFiles, n, "label %q files", tc.label)
	}
}

func TestDiscoverMailboxes_V10SingleMailbox(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := t.TempDir()
	guid := "9F0F15DD-4CBC-448A-9EBF-C385A47A3A67"
	mboxDir := filepath.Join(root, "INBOX.mbox")
	mkV10Mailbox(t, mboxDir, "1.emlx", "2.emlx")

	// Point directly at the .mbox directory.
	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1)
	assert.Equal("INBOX", mailboxes[0].Label)
	assert.Len(mailboxes[0].Files, 2)

	// MsgDir should point to the GUID/Data/Messages path.
	wantSuffix := filepath.Join(guid, "Data", "Messages")
	assert.True(filepath.IsAbs(mailboxes[0].MsgDir), "MsgDir not absolute: %q", mailboxes[0].MsgDir)
	rel, _ := filepath.Rel(mboxDir, mailboxes[0].MsgDir)
	assert.Equal(wantSuffix, rel, "MsgDir relative")
}

func TestDiscoverMailboxes_V10PartialSkipped(t *testing.T) {
	require := requirepkg.New(t)
	root := t.TempDir()
	mboxDir := filepath.Join(root, "Test.mbox")
	mkV10Mailbox(t, mboxDir,
		"1.emlx", "2.partial.emlx",
	)

	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1)
	require.Len(mailboxes[0].Files, 1)
	assertpkg.Equal(t, "1.emlx", filepath.Base(mailboxes[0].Files[0]))
}

func TestDiscoverMailboxes_MixedLegacyAndV10(t *testing.T) {
	require := requirepkg.New(t)
	root := t.TempDir()
	guid := "9F0F15DD-4CBC-448A-9EBF-C385A47A3A67"
	mboxDir := filepath.Join(root, "INBOX.mbox")

	// Create empty legacy Messages/ alongside populated V10 path.
	require.NoError(os.MkdirAll(filepath.Join(mboxDir, "Messages"), 0700), "mkdir")
	mkV10Mailbox(t, mboxDir, "1.emlx", "2.emlx")

	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1)
	require.Len(mailboxes[0].Files, 2, "should use V10 path")

	// MsgDir should point to the V10 path, not the empty legacy one.
	wantSuffix := filepath.Join(guid, "Data", "Messages")
	rel, _ := filepath.Rel(mboxDir, mailboxes[0].MsgDir)
	assertpkg.Equal(t, wantSuffix, rel, "MsgDir relative")
}

// mkV10PartitionedMailbox creates a V10 mailbox with .emlx files in
// both the primary Messages/ directory and in numeric partition
// subdirectories at various nesting depths.
//
// Layout created:
//
//	base/<guid>/Data/Messages/1.emlx       (top-level)
//	base/<guid>/Data/0/3/Messages/123.emlx (2-level partition)
//	base/<guid>/Data/9/Messages/456.emlx   (1-level partition)
func mkV10PartitionedMailbox(t *testing.T, base, guid string) {
	t.Helper()
	dataDir := filepath.Join(base, guid, "Data")

	writeEmlxFile := func(dir, name string) {
		t.Helper()
		requirepkg.NoError(t, os.MkdirAll(dir, 0700), "mkdir %q", dir)
		path := filepath.Join(dir, name)
		requirepkg.NoError(t, os.WriteFile(path, []byte("10\nFrom: x\r\n\r\n"), 0600), "write %q", path)
	}

	writeEmlxFile(filepath.Join(dataDir, "Messages"), "1.emlx")
	writeEmlxFile(filepath.Join(dataDir, "0", "3", "Messages"), "123.emlx")
	writeEmlxFile(filepath.Join(dataDir, "9", "Messages"), "456.emlx")
}

func TestDiscoverMailboxes_V10Partitioned(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	root := t.TempDir()
	guid := "9F0F15DD-4CBC-448A-9EBF-C385A47A3A67"
	mboxDir := filepath.Join(root, "INBOX.mbox")
	mkV10PartitionedMailbox(t, mboxDir, guid)

	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1)

	mb := mailboxes[0]
	assert.Equal("INBOX", mb.Label)

	// Should find all 3 files: 1 top-level + 2 from partitions.
	require.Len(mb.Files, 3, "want 3 files, got %v", mb.Files)

	// Verify all paths are absolute and point to existing files.
	for _, path := range mb.Files {
		assert.True(filepath.IsAbs(path), "expected absolute path, got %q", path)
		_, err := os.Stat(path)
		require.NoError(err, "stat %q", path)
	}

	// Verify expected basenames are present.
	baseNames := make(map[string]bool)
	for _, f := range mb.Files {
		baseNames[filepath.Base(f)] = true
	}
	for _, want := range []string{"1.emlx", "123.emlx", "456.emlx"} {
		assert.True(baseNames[want], "missing file %q in Files", want)
	}
}

func TestDiscoverMailboxes_V10PartitionedOnly(t *testing.T) {
	require := requirepkg.New(t)
	root := t.TempDir()
	guid := "9F0F15DD-4CBC-448A-9EBF-C385A47A3A67"
	mboxDir := filepath.Join(root, "INBOX.mbox")

	// Create the primary Messages/ dir but leave it empty.
	// (Tests the case where Messages/ exists but is empty.)
	primaryMsg := filepath.Join(mboxDir, guid, "Data", "Messages")
	require.NoError(os.MkdirAll(primaryMsg, 0700), "mkdir %q", primaryMsg)

	// Place files only in partition dirs.
	partDir := filepath.Join(mboxDir, guid, "Data", "3", "Messages")
	require.NoError(os.MkdirAll(partDir, 0700), "mkdir %q", partDir)
	for _, name := range []string{"100.emlx", "200.emlx"} {
		path := filepath.Join(partDir, name)
		require.NoError(os.WriteFile(path, []byte("10\nFrom: x\r\n\r\n"), 0600), "write %q", path)
	}

	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1, "partitioned-only mailbox should be detected")

	mb := mailboxes[0]
	require.Len(mb.Files, 2, "got files %v", mb.Files)

	for _, path := range mb.Files {
		_, err := os.Stat(path)
		assertpkg.NoError(t, err, "stat %q", path)
	}
}

// TestDiscoverMailboxes_V10NoTopLevelMessages tests the case where
// Data/Messages/ does not exist at all — only numeric partition dirs.
// This matches real Apple Mail behavior for large mailboxes.
func TestDiscoverMailboxes_V10NoTopLevelMessages(t *testing.T) {
	require := requirepkg.New(t)
	root := t.TempDir()
	guid := "9F0F15DD-4CBC-448A-9EBF-C385A47A3A67"
	mboxDir := filepath.Join(root, "Sent Messages.mbox")

	// Do NOT create Data/Messages/ — only create partition dirs.
	for _, partPath := range []string{
		filepath.Join(mboxDir, guid, "Data", "9", "9", "Messages"),
		filepath.Join(mboxDir, guid, "Data", "0", "0", "1", "Messages"),
	} {
		require.NoError(os.MkdirAll(partPath, 0700), "mkdir %q", partPath)
	}
	testFiles := map[string]string{
		"500.emlx": filepath.Join(mboxDir, guid, "Data", "9", "9", "Messages"),
		"600.emlx": filepath.Join(mboxDir, guid, "Data", "0", "0", "1", "Messages"),
		"700.emlx": filepath.Join(mboxDir, guid, "Data", "0", "0", "1", "Messages"),
	}
	for name, dir := range testFiles {
		path := filepath.Join(dir, name)
		require.NoError(os.WriteFile(path, []byte("10\nFrom: x\r\n\r\n"), 0600), "write %q", path)
	}

	mailboxes, err := DiscoverMailboxes(mboxDir)
	require.NoError(err, "DiscoverMailboxes")
	require.Len(mailboxes, 1, "no Data/Messages/ dir")

	mb := mailboxes[0]
	require.Len(mb.Files, 3, "got files %v", mb.Files)

	for _, path := range mb.Files {
		_, err := os.Stat(path)
		assertpkg.NoError(t, err, "stat %q", path)
	}
}

func TestIsUUID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"13C9A646-EE0A-4698-B5A2-E07FFBDDEED3", true},
		{"9f0f15dd-4cbc-448a-9ebf-c385a47a3a67", true},
		{"INBOX", false},
		{"Mailboxes", false},
		{"IMAP-foo@bar.com", false},
		{"", false},
		{"not-a-uuid-at-all-nope-definitely", false},
	}
	for _, tc := range tests {
		got := IsUUID(tc.input)
		assertpkg.Equal(t, tc.want, got, "IsUUID(%q)", tc.input)
	}
}
