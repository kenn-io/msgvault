package slack

import (
	"archive/zip"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const slackdumpFixture = "testdata/slackdump/standard"

type slackdumpSnapshot struct {
	Users         []User
	Conversations []Conversation
	Messages      map[string][]Message
	Attachments   map[string][]byte
}

func TestSlackdumpExportDirectoryAndZIPParity(t *testing.T) {
	dirSnapshot := readSlackdumpSnapshot(t, slackdumpFixture)
	zipSnapshot := readSlackdumpSnapshot(t, zipSlackdumpFixtureReverseOrder(t, slackdumpFixture))

	assert.Equal(t, dirSnapshot, zipSnapshot)
}

func TestSlackdumpExportReadsUsers(t *testing.T) {
	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, export.Close())
	}()

	users, err := export.users()
	require.NoError(t, err)
	assert.Equal(t, []User{
		{ID: "UALICE", TeamID: "T_TEST", Name: "alice", Profile: UserProfile{Email: "alice@example.com", DisplayName: "Alice Test"}},
		{ID: "UBOB", TeamID: "T_TEST", Name: "bob", Profile: UserProfile{Email: "bob@example.com", DisplayName: "Bob Test"}},
	}, users)
}

func TestSlackdumpExportMergesConversationCatalogAndNormalizesDMs(t *testing.T) {
	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, export.Close())
	}()

	conversations, err := export.conversations("UALICE")
	require.NoError(t, err)
	assert.Equal(t, []Conversation{
		{ID: "C001", Name: "general", IsChannel: true, IsMember: true, Members: []string{"UALICE", "UBOB"}},
		{ID: "D001", IsIM: true, User: "UBOB"},
		{ID: "D002", IsIM: true, User: "UALICE"},
		{ID: "G001", Name: "private-team", IsGroup: true, IsPrivate: true, IsMember: true, Members: []string{"UALICE", "UBOB"}},
		{ID: "G002", Name: "mpdm-alice--bob-1", IsGroup: true, IsMpim: true, IsPrivate: true, IsMember: true, Members: []string{"UALICE", "UBOB"}},
	}, conversations)
}

func TestSlackdumpExportOrdersDailyMessagesAndPreservesRawJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(err)
	defer func() {
		assert.NoError(export.Close())
	}()

	messages, err := export.messages(Conversation{ID: "C001", Name: "general", IsChannel: true})
	require.NoError(err, "non-date JSON siblings must be ignored")
	require.Len(messages, 2)
	assert.Equal([]string{"1704067200.000001", "1704153600.000002"}, []string{messages[0].TS, messages[1].TS})
	assert.JSONEq(
		`{"type":"message","ts":"1704067200.000001","thread_ts":"1704067200.000001","reply_count":1,"user":"UALICE","text":"first day <@UBOB>","files":[{"id":"F_STD","name":"what do these colored bars mean?.txt","mimetype":"text/plain","size":26}],"future_field":{"kept":true}}`,
		string(messages[0].Raw),
	)
}

func TestSlackdumpExportMapsConversationDirectories(t *testing.T) {
	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, export.Close())
	}()

	tests := []struct {
		name         string
		conversation Conversation
		wantTS       string
		wantText     string
	}{
		{
			name:         "public channel name",
			conversation: Conversation{ID: "C001", Name: "general", IsChannel: true},
			wantTS:       "1704067200.000001",
			wantText:     "first day <@UBOB>",
		},
		{
			name:         "private group name",
			conversation: Conversation{ID: "G001", Name: "private-team", IsGroup: true},
			wantTS:       "1704240000.000003",
			wantText:     "private group",
		},
		{
			name:         "MPIM name",
			conversation: Conversation{ID: "G002", Name: "mpdm-alice--bob-1", IsMpim: true},
			wantTS:       "1704326400.000004",
			wantText:     "group direct message",
		},
		{
			name:         "DM channel ID",
			conversation: Conversation{ID: "D001", IsIM: true, User: "UBOB"},
			wantTS:       "1704412800.000005",
			wantText:     "direct message",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			messages, err := export.messages(tt.conversation)
			require.NoError(err)
			require.NotEmpty(messages)
			assert.Equal(tt.wantTS, messages[0].TS)
			assert.Equal(tt.wantText, messages[0].Text)
		})
	}
}

func TestSlackdumpExportTreatsMissingConversationDirectoryAsEmpty(t *testing.T) {
	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, export.Close())
	}()

	messages, err := export.messages(Conversation{ID: "C_EMPTY", Name: "empty-channel", IsChannel: true})
	require.NoError(t, err)
	assert.Empty(t, messages)
}

func TestSlackdumpExportReadsAttachmentsAcrossLayouts(t *testing.T) {
	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, export.Close())
	}()

	tests := []struct {
		name         string
		conversation Conversation
		file         File
		want         string
	}{
		{
			name:         "standard portable filename",
			conversation: Conversation{ID: "C001", Name: "general", IsChannel: true},
			file:         File{ID: "F_STD", Name: "what do these colored bars mean?.txt"},
			want:         "standard attachment bytes\n",
		},
		{
			name:         "Mattermost portable filename",
			conversation: Conversation{ID: "G001", Name: "private-team", IsGroup: true},
			file:         File{ID: "F_MM", Name: "quarterly report?.txt"},
			want:         "mattermost attachment bytes\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, found, err := export.attachment(tt.conversation, tt.file)
			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, []byte(tt.want), content)
		})
	}
}

func TestSlackdumpExportResolvesStandardAttachmentByWorkspaceFileID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := copySlackdumpFixture(t)
	filename := "F_STD-what do these colored bars mean_.txt"
	source := filepath.Join(root, "general", "attachments", filename)
	destinationDir := filepath.Join(root, "private-team", "attachments")
	require.NoError(os.MkdirAll(destinationDir, 0o700))
	require.NoError(os.Rename(source, filepath.Join(destinationDir, filename)))

	export, err := openSlackdumpExport(root)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(export.Close()) })

	content, found, err := export.attachment(
		Conversation{ID: "C001", Name: "general", IsChannel: true},
		File{ID: "F_STD", Name: "what do these colored bars mean?.txt"},
	)
	require.NoError(err)
	assert.True(found)
	assert.Equal([]byte("standard attachment bytes\n"), content)
}

func TestSlackdumpExportReportsMissingAttachment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(err)
	defer func() {
		assert.NoError(export.Close())
	}()

	content, found, err := export.attachment(
		Conversation{ID: "C001", Name: "general", IsChannel: true},
		File{ID: "F_MISSING", Name: "missing.txt"},
	)
	require.NoError(err)
	assert.False(found)
	assert.Nil(content)
}

func TestSlackdumpExportBoundsAttachmentReads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(err)
	defer func() {
		assert.NoError(export.Close())
	}()

	content, found, err := export.attachmentWithLimit(
		Conversation{ID: "C001", Name: "general", IsChannel: true},
		File{ID: "F_STD", Name: "what do these colored bars mean?.txt"},
		4,
	)
	require.ErrorIs(err, ErrAssetTooLarge)
	assert.False(found)
	assert.Nil(content)
}

func TestSlackdumpExportDoesNotResolveAttachmentNamesOutsideTheirLayout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(err)
	defer func() {
		assert.NoError(export.Close())
	}()

	content, found, err := export.attachment(
		Conversation{ID: "C001", Name: "general", IsChannel: true},
		File{ID: "F_TRAVERSAL", Name: "../../users.json"},
	)
	require.NoError(err)
	assert.False(found)
	assert.Nil(content)
}

func TestSlackdumpExportRejectsAttachmentIDTraversal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(export.Close()) })

	content, found, err := export.attachment(
		Conversation{ID: "C001", Name: "general", IsChannel: true},
		File{ID: "..", Name: "users.json"},
	)
	require.Error(err)
	require.ErrorContains(err, "invalid attachment ID")
	assert.False(found)
	assert.Nil(content)
}

func TestSlackdumpExportDoesNotFollowAttachmentSymlinkOutsideDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := copySlackdumpFixture(t)
	external := filepath.Join(t.TempDir(), "external.txt")
	require.NoError(os.WriteFile(external, []byte("must not be imported"), 0o600))
	attachmentPath := filepath.Join(
		root,
		"general",
		"attachments",
		"F_STD-what do these colored bars mean_.txt",
	)
	require.NoError(os.Remove(attachmentPath))
	if err := os.Symlink(external, attachmentPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	export, err := openSlackdumpExport(root)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(export.Close()) })

	content, found, err := export.attachment(Conversation{ID: "C001", Name: "general"}, File{
		ID: "F_STD", Name: "what do these colored bars mean?.txt",
	})
	require.Error(err)
	assert.False(found)
	assert.NotEqual([]byte("must not be imported"), content)
}

func TestSlackdumpSanitizeFilenameMatchesPortableSlackdumpNames(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: `all<>:"/\|?*characters.txt`, want: "all_________characters.txt"},
		{name: "CON.txt", want: "_CON.txt"},
		{name: "trailing. ", want: "trailing"},
		{name: "", want: "unnamed_file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, slackdumpSanitizeFilename(tt.name))
		})
	}
}

func TestSlackdumpExportReportsMalformedInputPath(t *testing.T) {
	t.Run("malformed required index", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := copySlackdumpFixture(t)
		require.NoError(os.WriteFile(filepath.Join(root, "users.json"), []byte(`{"id":`), 0o644))

		export, err := openSlackdumpExport(root)
		require.NoError(err)
		defer func() {
			assert.NoError(export.Close())
		}()

		_, err = export.users()
		require.Error(err)
		require.ErrorContains(err, root)
		assert.ErrorContains(err, "users.json")
	})

	t.Run("missing required index", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := copySlackdumpFixture(t)
		require.NoError(os.Remove(filepath.Join(root, "users.json")))

		export, err := openSlackdumpExport(root)
		require.NoError(err)
		defer func() {
			assert.NoError(export.Close())
		}()

		_, err = export.users()
		require.Error(err)
		require.ErrorIs(err, fs.ErrNotExist)
		require.ErrorContains(err, root)
		assert.ErrorContains(err, "users.json")
	})

	t.Run("daily messages", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := copySlackdumpFixture(t)
		messagePath := filepath.Join("general", "2024-01-02.json")
		require.NoError(os.WriteFile(filepath.Join(root, messagePath), []byte(`[{"type":"message"`), 0o644))

		export, err := openSlackdumpExport(root)
		require.NoError(err)
		defer func() {
			assert.NoError(export.Close())
		}()

		_, err = export.messages(Conversation{ID: "C001", Name: "general", IsChannel: true})
		require.Error(err)
		require.ErrorContains(err, root)
		assert.ErrorContains(err, filepath.ToSlash(messagePath))
	})
}

func TestSlackdumpExportAcceptsMissingOptionalCatalogs(t *testing.T) {
	tests := []struct {
		missing string
		wantIDs []string
	}{
		{missing: "channels.json", wantIDs: []string{"D001", "D002", "G001", "G002"}},
		{missing: "groups.json", wantIDs: []string{"C001", "D001", "D002", "G002"}},
		{missing: "mpims.json", wantIDs: []string{"C001", "D001", "D002", "G001"}},
		{missing: "dms.json", wantIDs: []string{"C001", "G001", "G002"}},
	}
	for _, tt := range tests {
		t.Run(tt.missing, func(t *testing.T) {
			require := require.New(t)

			root := copySlackdumpFixture(t)
			require.NoError(os.Remove(filepath.Join(root, tt.missing)))

			export, err := openSlackdumpExport(root)
			require.NoError(err)
			defer func() {
				assert.NoError(t, export.Close())
			}()

			conversations, err := export.conversations("UALICE")
			require.NoError(err)
			gotIDs := make([]string, len(conversations))
			for i := range conversations {
				gotIDs[i] = conversations[i].ID
			}
			assert.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}

func TestSlackdumpExportIgnoresUnknownMalformedRootSibling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	export, err := openSlackdumpExport(slackdumpFixture)
	require.NoError(err)
	defer func() {
		assert.NoError(export.Close())
	}()

	users, err := export.users()
	require.NoError(err)
	assert.Len(users, 2)
	conversations, err := export.conversations("UALICE")
	require.NoError(err)
	assert.Len(conversations, 5)
}

func readSlackdumpSnapshot(t *testing.T, path string) slackdumpSnapshot {
	t.Helper()

	export, err := openSlackdumpExport(path)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, export.Close())
	}()

	users, err := export.users()
	require.NoError(t, err)
	conversations, err := export.conversations("UALICE")
	require.NoError(t, err)
	messages := make(map[string][]Message, len(conversations))
	for _, conversation := range conversations {
		messages[conversation.ID], err = export.messages(conversation)
		require.NoError(t, err)
	}
	attachments := make(map[string][]byte, 2)
	for _, item := range []struct {
		conversation Conversation
		file         File
	}{
		{
			conversation: Conversation{ID: "C001", Name: "general", IsChannel: true},
			file:         File{ID: "F_STD", Name: "what do these colored bars mean?.txt"},
		},
		{
			conversation: Conversation{ID: "G001", Name: "private-team", IsGroup: true},
			file:         File{ID: "F_MM", Name: "quarterly report?.txt"},
		},
	} {
		var found bool
		attachments[item.file.ID], found, err = export.attachment(item.conversation, item.file)
		require.NoError(t, err)
		require.True(t, found)
	}

	return slackdumpSnapshot{
		Users:         users,
		Conversations: conversations,
		Messages:      messages,
		Attachments:   attachments,
	}
}

func zipSlackdumpFixtureReverseOrder(t *testing.T, root string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "export.zip")
	file, err := os.Create(path)
	require.NoError(t, err)

	var paths []string
	err = fs.WalkDir(os.DirFS(root), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	require.NoError(t, err)
	// Slackdump readers must order daily files themselves rather than inherit
	// archive insertion order. In particular, 2024-01-02 is packed before
	// 2024-01-01 here.
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))

	zw := zip.NewWriter(file)
	for _, path := range paths {
		content, err := fs.ReadFile(os.DirFS(root), path)
		require.NoError(t, err)

		destination, err := zw.Create(filepath.ToSlash(path))
		require.NoError(t, err)
		_, err = destination.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, file.Close())
	return path
}

func copySlackdumpFixture(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS(slackdumpFixture)))
	return root
}
