package googlechat

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func write(t *testing.T, root, name, content string) {
	t.Helper()
	testutil.WriteFile(t, root, name, []byte(content))
}

func fixtureMessages(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/takeout/Takeout/Google Chat/Groups/DM synthetic-dm/messages.json")
	require.NoError(t, err)
	return strings.TrimSpace(string(data))
}

func TestImportGoogleChatCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Me: " OWNER@example.com ", Roots: []string{"testdata/takeout"}, AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(4, summary.MessagesAdded)
	assert.Equal(2, summary.Conversations)
	assert.Equal(1, summary.MembershipWithoutMessages)
	assert.Equal(1, summary.DatesUnparsed)
	assert.Equal(1, summary.Unsupported)
	assert.Equal(1, summary.UnknownSenders)
	assert.Equal(1, summary.AttachmentsStored)
	assert.Equal(1, summary.AttachmentsMissing)
	var senders, conversations int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='from'`).Scan(&senders))
	assert.Equal(3, senders)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM conversations WHERE message_count>0`).Scan(&conversations))
	assert.Equal(2, conversations)
}
func TestImportGoogleChatRawMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{"testdata/takeout"}, AttachmentsDir: t.TempDir()}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	var id int64
	var fromMe bool
	var sent sql.NullTime
	var metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id, is_from_me, sent_at, metadata FROM messages WHERE source_message_id = 'group/topic/message'`).Scan(&id, &fromMe, &sent, &metadata))
	assert.True(fromMe)
	require.True(sent.Valid)
	assert.True(time.Date(2021, 1, 18, 13, 27, 33, 0, time.UTC).Equal(sent.Time))
	var meta map[string]any
	require.NoError(json.Unmarshal([]byte(metadata), &meta))
	assert.Equal("topic-a", meta["topic_id"])
	assert.Equal("DM synthetic-dm", meta["group_id"])
	var conversationMetadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM conversations WHERE source_conversation_id='DM synthetic-dm'`).Scan(&conversationMetadata))
	var fileMeta struct {
		Files []provenance `json:"group_files"`
	}
	require.NoError(json.Unmarshal([]byte(conversationMetadata), &fileMeta))
	require.NotEmpty(fileMeta.Files)
	for _, file := range fileMeta.Files {
		assert.False(filepath.IsAbs(file.Path))
		_, err := os.Stat(filepath.Join(opts.Roots[0], file.Path))
		require.NoError(err, "relative provenance resolves under the supplied input")
	}
}
func TestImportGoogleChatReplayKeepsSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{"testdata/takeout"}, AttachmentsDir: t.TempDir()}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='group/topic/message'`).Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	// A partial later export cannot clear text, raw bytes or stored media.
	root := t.TempDir()
	write(t, root, "Groups/DM synthetic-dm/messages.json", strings.Replace(fixtureMessages(t), "Bodyneedle synthetic", "replacement", 1))
	opts.Roots = []string{root}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(1, summary.Duplicates)
	assert.Zero(summary.AttachmentsMissing)
	rawAfter, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(string(raw), string(rawAfter))
}
func TestImportGoogleChatAccountScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{"testdata/takeout"}, AttachmentsDir: t.TempDir()}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	opts.Roots = []string{"testdata/takeout/Takeout/Google Chat/Groups"}
	opts.Me = "other@example.com"
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(4, summary.MessagesAdded)
	var sources int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sources WHERE source_type='google_chat'`).Scan(&sources))
	assert.Equal(2, sources)
}

func TestImportGoogleChatMultipartRepairAndOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "reverse"}[reverse], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			a, b := t.TempDir(), t.TempDir()
			write(t, a, "Google Chat/Groups/DM test/messages.json", fixtureMessages(t))
			opts := ImportOptions{Me: "owner@example.com", Roots: []string{a}, AttachmentsDir: t.TempDir()}
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(1, summary.AttachmentsMissing)
			write(t, b, "Groups/DM test/File-example.txt", "")
			opts.Roots = []string{a, b}
			if reverse {
				opts.Roots = []string{b, a}
			}
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(1, summary.Duplicates)
			assert.Equal(1, summary.AttachmentsStored)
			var path, hash string
			var size int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT storage_path,content_hash,size FROM attachments`).Scan(&path, &hash, &size))
			assert.Equal("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", hash)
			assert.Zero(size)
			data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, path))
			require.NoError(err)
			assert.Empty(data)
		})
	}
}

func TestImportGoogleChatValidation(t *testing.T) {
	for _, tc := range []struct{ name, me, data, want string }{
		{"display address", "Example <owner@example.com>", fixtureMessages(t), "--me"},
		{"trailing", "owner@example.com", fixtureMessages(t) + ` {}`, "JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "Groups/DM a/messages.json", tc.data)
			summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: tc.me, Roots: []string{root}})
			require.ErrorContains(t, err, tc.want)
			assert.Zero(t, summary.MessagesAdded)
		})
	}
}

func TestImportGoogleChatMediaSafety(t *testing.T) {
	for _, tc := range []struct{ name, ref string }{{"traversal", "../secret"}, {"absolute", "/secret"}, {"missing", "File-example.txt"}, {"escaped symlink", "escape.txt"}, {"oversized", "large.txt"}, {"ambiguous", "duplicate.txt"}, {"nonregular", "directory"}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			a, b := t.TempDir(), t.TempDir()
			require.NoError(os.MkdirAll(filepath.Join(b, "Groups"), 0700))
			ref := tc.ref
			if tc.name == "absolute" {
				ref = filepath.Join(b, "secret")
			}
			encodedRef, err := json.Marshal(ref)
			require.NoError(err)
			write(t, a, "Groups/DM a/messages.json", strings.ReplaceAll(fixtureMessages(t), `"File-example.txt"`, string(encodedRef)))
			dir := filepath.Join(a, "Groups/DM a")
			switch tc.name {
			case "traversal":
				write(t, a, "Groups/secret", "outside the conversation")
			case "absolute":
				write(t, b, "secret", "outside the supplied conversation")
			case "nonregular":
				require.NoError(os.Mkdir(filepath.Join(dir, tc.ref), 0700))
			case "escaped symlink":
				write(t, b, "secret", "secret")
				require.NoError(os.Symlink(filepath.Join(b, "secret"), filepath.Join(dir, tc.ref)))
			case "oversized":
				f, err := os.Create(filepath.Join(dir, tc.ref))
				require.NoError(err)
				require.NoError(f.Truncate(251 << 20))
				require.NoError(f.Close())
			case "ambiguous":
				write(t, a, "Groups/DM a/duplicate.txt", "a")
				write(t, b, "Groups/DM a/duplicate.txt", "b")
			}
			summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{a, b}, AttachmentsDir: t.TempDir()})
			if tc.name == "ambiguous" {
				require.ErrorContains(err, "ambiguous")
				return
			}
			require.NoError(err)
			assert.Equal(1, summary.AttachmentsMissing)
			assert.Zero(summary.AttachmentsStored)
			if tc.name == "nonregular" || tc.name == "traversal" || tc.name == "absolute" {
				assert.Equal(1, summary.AttachmentsUnsupported)
			}
			if tc.name == "oversized" {
				assert.Equal(1, summary.AttachmentsOversized)
				var reason, state string
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT attachment_skip_reason,attachment_state FROM attachments`).Scan(&reason, &state))
				assert.Equal("size_cap", reason)
				assert.Equal("skipped", state)
			}
		})
	}
}

func TestImportGoogleChatStorageFailure(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
	write(t, root, "Groups/DM a/File-example.txt", "bytes")
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, []byte("blocked"), 0600))
	summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: "owner@example.com", Roots: []string{root}, AttachmentsDir: blocked})
	require.ErrorContains(t, err, "store media")
	assert.Zero(t, summary.MessagesAdded)
}

func TestImportGoogleChatMembershipIsAdditiveOnDuplicateReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/Space a/messages.json", fixtureMessages(t))
	write(t, root, "Groups/Space a/group_info.json", `{"members":[{"email":"peer@example.net","name":"Example Peer"}]}`)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	write(t, root, "Groups/Space a/group_info.json", `{"members":[{"email":"later@example.org","name":"Example Later"}]}`)
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.Duplicates)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_participants`).Scan(&n))
	assert.Equal(3, n, "original member, original sender and later member remain")
}

func TestImportGoogleChatConflictingDuplicateRecord(t *testing.T) {
	for _, mode := range []string{"same conversation", "cross conversation"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			first := strings.TrimSuffix(strings.TrimPrefix(fixtureMessages(t), `{"messages":[`), `]}`)
			later := strings.ReplaceAll(first, "group/topic/message", "group/topic/later")
			opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}}
			if mode == "same conversation" {
				write(t, root, "Groups/DM a/messages.json", `{"messages":[`+first+`,`+strings.Replace(first, "Bodyneedle", "changed", 1)+`,`+later+`]}`)
			} else {
				write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
				_, err := Import(t.Context(), st, opts)
				require.NoError(err)
				write(t, root, "Groups/DM a/messages.json", strings.ReplaceAll(fixtureMessages(t), "group/topic/message", "group/topic/new"))
				write(t, root, "Groups/Space b/messages.json", fixtureMessages(t))
				write(t, root, "Groups/Space c/messages.json", `{"messages":[`+later+`]}`)
			}
			for _, replay := range []bool{false, true} {
				summary, err := Import(t.Context(), st, opts)
				require.NoError(err)
				assert.Equal(1, summary.Unsupported)
				if replay {
					assert.Zero(summary.MessagesAdded)
					assert.Equal(2, summary.Duplicates)
				} else {
					assert.EqualValues(2, summary.MessagesAdded)
				}
			}
			var id int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='group/topic/message'`).Scan(&id))
			raw, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(first, string(raw))
		})
	}
}

func TestImportGoogleChatReplayKeepsOriginalMediaSlots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	original := strings.Replace(fixtureMessages(t), `"export_name":"File-example.txt"`, `"export_name":"File-example.txt"},{"original_name":"second.txt","export_name":"File-second.txt"`, 1)
	write(t, root, "Groups/DM a/messages.json", original)
	write(t, root, "Groups/DM a/File-example.txt", "first bytes")
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}, AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	assert.Equal(1, summary.AttachmentsMissing)
	// A changed export moves a different attachment into the missing array slot.
	// Repair must use the archived provider reference, never the new slot order.
	write(t, root, "Groups/DM a/messages.json", strings.ReplaceAll(original, "File-second.txt", "File-other.txt"))
	write(t, root, "Groups/DM a/File-other.txt", "wrong bytes")
	write(t, root, "Groups/DM a/File-second.txt", "second bytes")
	summary, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	var path string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT storage_path FROM attachments WHERE source_part_key = 'file:1'`).Scan(&path))
	content, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, path))
	require.NoError(err)
	assert.Equal("second bytes", string(content))
}

// Public export derivatives retain provider structure, with wholly replaced
// identities/content/media and documented publisher irregularities. See testdata.
func TestImportGoogleChatRedactedPublicExports(t *testing.T) {
	for _, tc := range []struct {
		name            string
		messages        int64
		stored, missing int
	}{
		{"civic", 84, 5, 1},
		{"niimpy", 4, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			opts := ImportOptions{Me: "person1@example.com", Roots: []string{filepath.Join("testdata", "redacted-public", tc.name)}, AttachmentsDir: t.TempDir()}
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(tc.messages, summary.MessagesAdded)
			assert.Equal(tc.stored, summary.AttachmentsStored)
			assert.Equal(tc.missing, summary.AttachmentsMissing)
			assert.Zero(summary.DatesUnparsed)
			assert.Zero(summary.UnknownSenders)
			assert.Zero(summary.Unsupported)
			var count int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE sent_at IS NOT NULL`).Scan(&count))
			assert.Equal(tc.messages, count)
			files, err := filepath.Glob(filepath.Join(opts.Roots[0], "Groups", "*", "messages.json"))
			require.NoError(err)
			require.Len(files, 1)
			input, err := os.ReadFile(files[0])
			require.NoError(err)
			var envelope struct {
				Messages []jsontext.Value `json:"messages"`
			}
			require.NoError(json.Unmarshal(input, &envelope))
			for _, record := range envelope.Messages {
				var provider message
				require.NoError(json.Unmarshal(record, &provider))
				var id int64
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), provider.ID).Scan(&id))
				raw, err := st.GetMessageRawContext(t.Context(), id)
				require.NoError(err)
				assert.Equal(string(record), string(raw), "all optional metadata and absent text remain intact")
			}
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Zero(summary.MessagesAdded)
			assert.EqualValues(tc.messages, summary.Duplicates)
			assert.Zero(summary.AttachmentsStored)
			assert.Equal(tc.missing, summary.AttachmentsMissing)
		})
	}
}

func TestImportGoogleChatConversationKindRequiresRosterEvidence(t *testing.T) {
	for _, tc := range []struct{ name, group, roster, want string }{
		{"missing roster", "DM test", "", "chat"},
		{"empty roster", "DM test", `{"members":[]}`, "chat"},
		{"partial roster", "DM test", `{"members":[{"email":"owner@example.com"}]}`, "chat"},
		{"two person DM", "DM test", `{"members":[{"email":"owner@example.com"},{"email":"peer@example.com"}]}`, "direct_chat"},
		{"group DM", "DM test", `{"members":[{"email":"owner@example.com"},{"email":"peer@example.com"},{"email":"third@example.com"}]}`, "group_chat"},
		{"space", "Space test", "", "group_chat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			root := t.TempDir()
			write(t, root, "Groups/"+tc.group+"/messages.json", fixtureMessages(t))
			if tc.roster != "" {
				write(t, root, "Groups/"+tc.group+"/group_info.json", tc.roster)
			}
			st := testutil.NewTestStore(t)
			_, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
			require.NoError(err)
			var kind string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
			assert.Equal(tc.want, kind)
			if tc.name == "two person DM" {
				require.NoError(os.Remove(filepath.Join(root, "Groups", tc.group, "group_info.json")))
				write(t, root, "Groups/"+tc.group+"/messages.json", strings.ReplaceAll(fixtureMessages(t), "group/topic/message", "group/topic/new"))
				_, err = Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
				require.NoError(err)
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
				assert.Equal("direct_chat", kind)
			}
		})
	}
}
