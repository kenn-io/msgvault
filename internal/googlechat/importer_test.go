package googlechat

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
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
	// A partial later export retains the original raw message.
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
	for _, tc := range []struct{ name, ref string }{{"traversal", "../secret"}, {"escaped symlink", "escape.txt"}, {"oversized", "large.txt"}, {"nonregular", "directory"}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			a, b := t.TempDir(), t.TempDir()
			require.NoError(os.MkdirAll(filepath.Join(b, "Groups"), 0700))
			encodedRef, err := json.Marshal(tc.ref)
			require.NoError(err)
			write(t, a, "Groups/DM a/messages.json", strings.ReplaceAll(fixtureMessages(t), `"File-example.txt"`, string(encodedRef)))
			dir := filepath.Join(a, "Groups/DM a")
			switch tc.name {
			case "traversal":
				write(t, a, "Groups/secret", "outside the conversation")
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
			}
			summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{a, b}, AttachmentsDir: t.TempDir()})
			require.NoError(err)
			assert.Equal(1, summary.AttachmentsMissing)
			assert.Zero(summary.AttachmentsStored)
			if tc.name == "nonregular" || tc.name == "traversal" {
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
		{"two person DM", "DM test", `{"members":[{"email":"owner@example.com"},{"email":"peer@example.com"}]}`, "chat"},
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
			if tc.name == "two person DM" || tc.name == "group DM" {
				require.NoError(os.Remove(filepath.Join(root, "Groups", tc.group, "group_info.json")))
				write(t, root, "Groups/"+tc.group+"/messages.json", strings.ReplaceAll(fixtureMessages(t), "group/topic/message", "group/topic/new"))
				_, err = Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
				require.NoError(err)
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
				assert.Equal(tc.want, kind)
				if tc.name == "group DM" {
					write(t, root, "Groups/"+tc.group+"/group_info.json", `{"members":[{"email":"owner@example.com"},{"email":"peer@example.com"}]}`)
					_, err = Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
					require.NoError(err)
					require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
					assert.Equal(tc.want, kind)
				}
			}
		})
	}
}

func TestImportGoogleChatFailedMessageDoesNotCountStoredAttachments(t *testing.T) {
	st := testutil.NewTestStore(t)
	a, b := t.TempDir(), t.TempDir()
	message := strings.Replace(fixtureMessages(t), `"export_name":"File-example.txt"`, `"export_name":"File-example.txt"},{"original_name":"second.txt","export_name":"File-second.txt"`, 1)
	write(t, a, "Groups/DM a/messages.json", message)
	write(t, a, "Groups/DM a/File-example.txt", "first")
	write(t, a, "Groups/DM a/File-second.txt", "second")
	write(t, b, "Groups/DM a/File-second.txt", "other")

	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{a, b}, AttachmentsDir: t.TempDir()})
	require.ErrorContains(t, err, "ambiguous")
	assert.Zero(t, summary.AttachmentsStored)
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&count))
	assert.Zero(t, count)
}

const fixtureDate = "Monday, January 18, 2021 at 1:27:33 PM UTC"

func TestImportGoogleChatMetadataDoesNotEmbedWholeRoster(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	members := []string{`{"name":"Owner","email":"owner@example.com","user_type":"Human"}`}
	for i := range 300 {
		members = append(members, fmt.Sprintf(`{"name":"Member %d","email":"member%d@example.com","user_type":"Human"}`, i, i))
	}
	testutil.WriteFile(t, root, "Groups/Space s/group_info.json", []byte(`{"name":"Big","members":[`+strings.Join(members, ",")+`]}`))
	var msgs []string
	for i := range 50 {
		msgs = append(msgs, fmt.Sprintf(`{"creator":{"name":"Owner","email":"owner@example.com","user_type":"Human"},"created_date":%q,"text":"t%d","topic_id":"t","message_id":"s/t/m%d"}`, fixtureDate, i, i))
	}
	testutil.WriteFile(t, root, "Groups/Space s/messages.json", []byte(`{"messages":[`+strings.Join(msgs, ",")+`]}`))
	_, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(err)
	var metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages WHERE source_message_id='s/t/m7'`).Scan(&metadata))
	assert.NotContains(metadata, "member250@example.com", "per-message metadata must not copy the full roster")
}

func TestImportGoogleChatContainerEvidenceSurvivesSparseReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	full, partial := t.TempDir(), t.TempDir()
	write(t, full, "Groups/DM a/messages.json", fixtureMessages(t))
	groupRaw := `{ "members":[{"email":"owner@example.com"},{"email":"peer@example.com"}], "unknown_group":{"retained":true}, "number":1e3 }`
	write(t, full, "Groups/DM a/group_info.json", groupRaw)
	profileRaw := `{ "user":{"email":"owner@example.com"}, "unknown_profile":{"retained":true} }`
	write(t, full, "Users/User synthetic/user_info.json", profileRaw)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{full}}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	_, err = Import(t.Context(), st, opts)
	require.NoError(err, "repeated full export retains one copy of each container snapshot")
	write(t, partial, "Groups/DM a/messages.json", strings.Replace(fixtureMessages(t), "group/topic/message", "group/topic/new", 1))
	opts.Roots = []string{partial}
	_, err = Import(t.Context(), st, opts)
	require.NoError(err)
	var group sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM conversations`).Scan(&group))
	var evidence struct {
		Files []provenance `json:"group_files"`
	}
	require.NoError(json.Unmarshal([]byte(group.String), &evidence))
	assert.ElementsMatch([]provenance{
		{Path: "Groups/DM a/group_info.json", Data: groupRaw},
		{Path: "Groups/DM a/messages.json"},
	}, evidence.Files, "relative container provenance is retained once")
	assert.NotContains(group.String, full)
	var metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages ORDER BY id LIMIT 1`).Scan(&metadata))
	for _, key := range []string{"provider_record", "group_info.json", "messages.json"} {
		assert.NotContains(metadata, key)
	}
}

func TestImportGoogleChatRunAccounting(t *testing.T) {
	for _, mode := range []string{"completed", "failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
			if mode == "failed" {
				write(t, root, "Groups/Space b/messages.json", `{"messages":[`)
			}
			if mode == "cancelled" {
				write(t, root, "Groups/Space b/messages.json", strings.ReplaceAll(fixtureMessages(t), "group/topic/message", "group/topic/second"))
			}
			opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}}
			var summary *ImportSummary
			var err error
			if mode == "cancelled" {
				summary, err = cancelAfterCommittedMessage(t, st, opts)
			} else {
				summary, err = Import(t.Context(), st, opts)
			}
			statusWant := "failed"
			if mode == "completed" {
				require.NoError(err)
				statusWant = "completed"
			} else {
				require.Error(err)
			}
			require.EqualValues(1, summary.MessagesAdded)
			var status string
			var processed, added int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status,messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status, &processed, &added))
			assert.Equal(statusWant, status)
			assert.EqualValues(1, processed)
			assert.Equal(summary.MessagesAdded, added)
			if mode == "cancelled" {
				require.ErrorIs(err, context.Canceled)
				replay, replayErr := Import(t.Context(), st, opts)
				require.NoError(replayErr)
				assert.EqualValues(2, replay.MessagesAdded+int64(replay.Duplicates))
				assert.Positive(replay.Duplicates)
			}
			if mode == "completed" {
				_, err = Import(t.Context(), st, opts)
				require.NoError(err)
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&processed, &added))
				assert.EqualValues(1, processed)
				assert.Zero(added)
			}
		})
	}
}

// A known email can exist without a name before Takeout supplies that name.
// Use public Store setup and the real importer; no provider/user data is read.
func TestImportGoogleChatFillsMissingParticipantName(t *testing.T) {
	for _, tc := range []struct{ name, existing, want string }{
		{"nameless", "", "Synthetic Sender"},
		{"curated", "Kept Name", "Kept Name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			pid, err := st.EnsureParticipantContext(t.Context(), "sender@example.com", tc.existing, "example.com")
			require.NoError(err)
			root := t.TempDir()
			write(t, root, "Groups/DM synthetic/group_info.json", `{"members":[{"email":"sender@example.com"}]}`)
			testutil.WriteFile(t, root, "Groups/DM synthetic/messages.json", []byte(`{"messages":[{"message_id":"synthetic/topic/message","creator":{"email":"sender@example.com","name":"Synthetic Sender","user_type":"Human"},"text":"synthetic body","created_date":"Monday, January 18, 2021 at 1:27:33 PM UTC"}]}`))
			summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
			require.NoError(err)
			require.EqualValues(1, summary.MessagesAdded)
			var name string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(display_name,'') FROM participants WHERE id = ?`), pid).Scan(&name))
			assert.Equal(t, tc.want, name, "Takeout should fill a missing name and preserve an existing one")
		})
	}
}

func TestImportGoogleChatReplayIdentityFence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}}
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&id))
	before, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE message_raw SET raw_format='future_google_chat'`)
	require.NoError(err)
	summary, err := Import(t.Context(), st, opts)
	assert.Zero(summary.MessagesAdded)
	require.ErrorContains(err, "stored raw format")
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(before, raw)
}

func TestImportGoogleChatRootAliasesKeepProfilesAndMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Takeout/Google Chat/Groups/DM a/messages.json", fixtureMessages(t))
	write(t, root, "Takeout/Google Chat/Groups/DM a/File-example.txt", "synthetic")
	write(t, root, "Takeout/Google Chat/Users/User a/user_info.json", `{"user":{"email":"owner@example.com"}}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{filepath.Join(root, "Takeout/Google Chat/Groups"), root, filepath.Join(root, "Takeout/Google Chat")}, AttachmentsDir: t.TempDir()})
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
}

func TestImportGoogleChatReportsUnknownRosterMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
	write(t, root, "Groups/DM a/group_info.json", `{"members":[{"name":"Synthetic bot","user_type":"Bot"}]}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(err)
	assert.Equal(1, summary.UnknownMembers)
	assert.Zero(summary.UnknownSenders)
	assert.Zero(summary.Unsupported)
}

func TestImportGoogleChatDoesNotInferRecipientsFromRoster(t *testing.T) {
	for _, tc := range []struct {
		name, creatorEmail string
		fromCount          int
		creatorInRoster    bool
	}{
		{"missing creator email", "", 0, false},
		{"creator outside roster", "senderneedle@example.com", 1, false},
		{"creator in roster", "senderneedle@example.com", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			require.True(st.FTS5Available(), "regression requires real FTS")
			root := t.TempDir()
			data := strings.Replace(fixtureMessages(t), `"email":"owner@example.com",`, "", 1)
			if tc.creatorEmail != "" {
				data = strings.Replace(fixtureMessages(t), "owner@example.com", tc.creatorEmail, 1)
			}
			write(t, root, "Groups/DM synthetic/messages.json", data)
			roster := `{"members":[{"email":"recipientneedle1@example.com"},{"email":"recipientneedle2@example.com"}]}`
			if tc.creatorInRoster {
				roster = strings.Replace(roster, "recipientneedle1@example.com", tc.creatorEmail, 1)
			}
			write(t, root, "Groups/DM synthetic/group_info.json", roster)
			summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}, AttachmentsDir: t.TempDir()})
			require.NoError(err)
			require.EqualValues(1, summary.MessagesAdded)
			var toCount, fromCount int
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='to'`).Scan(&toCount))
			assert.Zero(toCount, "membership alone cannot establish delivery recipients")
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='from'`).Scan(&fromCount))
			assert.Equal(tc.fromCount, fromCount)
			rows, err := st.DB().QueryContext(t.Context(), `SELECT p.email_address FROM conversation_participants cp JOIN participants p ON p.id=cp.participant_id`)
			require.NoError(err)
			defer func() { _ = rows.Close() }()
			var members []string
			for rows.Next() {
				var email string
				require.NoError(rows.Scan(&email))
				members = append(members, email)
			}
			require.NoError(rows.Err())
			require.NoError(rows.Close())
			wantMembers := []string{"recipientneedle1@example.com", "recipientneedle2@example.com"}
			if tc.creatorInRoster {
				wantMembers[0] = tc.creatorEmail
			} else if tc.creatorEmail != "" {
				wantMembers = append(wantMembers, tc.creatorEmail)
			}
			assert.ElementsMatch(wantMembers, members, "observed roster and sender membership remain")
			for _, rebuilt := range []bool{false, true} {
				if rebuilt {
					n, err := st.RebuildFTSContext(t.Context(), nil)
					require.NoError(err)
					require.EqualValues(1, n)
				}
				for _, search := range []struct {
					term string
					want int
				}{{"recipientneedle1", 0}, {"recipientneedle2", 0}, {"Bodyneedle", 1}, {"senderneedle", tc.fromCount}} {
					_, count, err := st.SearchMessagesContext(t.Context(), search.term, 0, 10)
					require.NoError(err)
					assert.EqualValues(search.want, count, "term=%s rebuilt=%t", search.term, rebuilt)
				}
			}
		})
	}
}

type cancelOnCommittedMessage struct {
	slog.Handler

	cancel   context.CancelFunc
	st       *store.Store
	ctx      context.Context
	canceled bool
}

func (*cancelOnCommittedMessage) Enabled(context.Context, slog.Level) bool { return true }
func (h *cancelOnCommittedMessage) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "sql tx commit" || h.canceled {
		return nil
	}
	var n int
	if err := h.st.DB().QueryRowContext(h.ctx, `SELECT count(*) FROM messages`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		h.canceled = true
		h.cancel()
	}
	return nil
}

// Observe the existing synchronous post-commit log, then verify durable rows.
// This needs no production hook, polling or timing threshold. Slow logging stays
// disabled so it cannot replace the commit event on a loaded runner.
func cancelAfterCommittedMessage(t *testing.T, st *store.Store, opts ImportOptions) (*ImportSummary, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := &cancelOnCommittedMessage{Handler: slog.DiscardHandler, cancel: cancel, st: st, ctx: t.Context()}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)
	store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
	defer store.ConfigureSQLLogging(store.SQLLogOptions{})
	return Import(ctx, st, opts)
}

func TestImportGoogleChatRosterDoesNotProveDelivery(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", fixtureMessages(t))
	write(t, root, "Groups/DM a/group_info.json", `{"members":[{"email":"owner@example.com"},{"email":"peer@example.com"}]}`)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}, AttachmentsDir: t.TempDir()}
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE conversations SET conversation_type='direct_chat'`)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "Groups", "DM a", "group_info.json")))
	write(t, root, "Groups/DM a/File-example.txt", "synthetic")
	_, err = Import(t.Context(), st, opts)
	require.NoError(t, err)

	var messageID, participantID, attachmentID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&messageID))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM participants WHERE email_address='peer@example.com'`).Scan(&participantID))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM attachments`).Scan(&attachmentID))
	scope := personscope.Scope{ParticipantIDs: []int64{participantID}, Directions: []personscope.Direction{personscope.ToPerson}, IncludeUnclassifiedRosterRows: true}
	provenance, err := st.PersonProvenanceForMessages(t.Context(), []int64{messageID}, scope)
	require.NoError(t, err)
	require.NotNil(t, provenance[messageID])
	assert.Equal(t, []personscope.Role{personscope.RoleConversationMember}, provenance[messageID].Roles)
	rows, err := st.FilenameSuggestionRowsContext(t.Context(), []int64{attachmentID}, store.FilenameSuggestionScope{Person: &scope})
	require.NoError(t, err)
	assert.Empty(t, rows)
}
