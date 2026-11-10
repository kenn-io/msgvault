package googlechat

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
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

const one = `{"messages":[{"creator":{"name":"Example Owner","email":"owner@example.com","user_type":"Human"},"created_date":"Monday, January 18, 2021 at 1:27:33\u202fPM UTC","text":"Bodyneedle synthetic","topic_id":"topic-a","message_id":"group/topic/message","future":{"keep":true},"attached_files":[{"original_name":"example.txt","export_name":"File-example.txt"}]}]}`

func TestImportGoogleChatPreservesSearchRawScopeAndReplay(t *testing.T) {
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Me: " OWNER@example.com ", Roots: []string{"testdata/takeout"}, AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 4, summary.MessagesAdded)
	assert.Equal(t, 2, summary.Conversations)
	assert.Equal(t, 1, summary.MembershipWithoutMessages)
	assert.Equal(t, 1, summary.DatesUnparsed)
	assert.Equal(t, 1, summary.Unsupported)
	assert.Equal(t, 1, summary.UnknownSenders)
	assert.Equal(t, 1, summary.AttachmentsStored)
	assert.Equal(t, 1, summary.AttachmentsMissing)
	var id int64
	var fromMe bool
	var sent sql.NullTime
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id, is_from_me, sent_at, metadata FROM messages WHERE source_message_id = 'group/topic/message'`).Scan(&id, &fromMe, &sent, &metadata))
	assert.True(t, fromMe)
	require.True(t, sent.Valid)
	assert.True(t, time.Date(2021, 1, 18, 13, 27, 33, 0, time.UTC).Equal(sent.Time))
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(metadata), &meta))
	assert.Equal(t, "topic-a", meta["topic_id"])
	assert.Equal(t, "DM synthetic-dm", meta["group_id"])
	assert.Contains(t, metadata, `"keep":true`)
	assert.Contains(t, metadata, "messages.json")
	var fileMeta struct {
		Files []provenance `json:"message_files"`
	}
	require.NoError(t, json.Unmarshal([]byte(metadata), &fileMeta))
	require.NotEmpty(t, fileMeta.Files)
	for _, file := range fileMeta.Files {
		_, err := os.Stat(filepath.Join(file.Root, file.Path))
		require.NoError(t, err, "stored file provenance resolves to supplied input")
	}
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"future":{"keep":true}`)
	var body string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&body))
	assert.Equal(t, "Bodyneedle synthetic", body)
	_, total, err := st.SearchMessages("Bodyneedle", 0, 100)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	for _, query := range []string{`SELECT count(*) FROM message_recipients WHERE recipient_type = 'from'`, `SELECT count(*) FROM conversations WHERE message_count > 0`} {
		var n int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), query).Scan(&n))
		if strings.Contains(query, "recipients") {
			assert.Equal(t, 3, n)
		} else {
			assert.Equal(t, 2, n)
		}
	}
	// A partial later export cannot clear text, raw bytes or stored media.
	root := t.TempDir()
	write(t, root, "Groups/DM synthetic-dm/messages.json", strings.Replace(one, "Bodyneedle synthetic", "replacement", 1))
	opts.Roots = []string{root}
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, summary.MessagesAdded)
	assert.Equal(t, 1, summary.Duplicates)
	assert.Zero(t, summary.AttachmentsMissing)
	rawAfter, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, string(raw), string(rawAfter))
	opts.Me = "other@example.com"
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sources WHERE source_type = 'google_chat'`).Scan(&n))
	assert.Equal(t, 2, n)
}

func TestImportGoogleChatMultipartRepairAndOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "reverse"}[reverse], func(t *testing.T) {
			st := testutil.NewTestStore(t)
			a, b := t.TempDir(), t.TempDir()
			write(t, a, "Google Chat/Groups/DM test/messages.json", one)
			opts := ImportOptions{Me: "owner@example.com", Roots: []string{a}, AttachmentsDir: t.TempDir()}
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Equal(t, 1, summary.AttachmentsMissing)
			write(t, b, "Groups/DM test/File-example.txt", "")
			opts.Roots = []string{a, b}
			if reverse {
				opts.Roots = []string{b, a}
			}
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Equal(t, 1, summary.Duplicates)
			assert.Equal(t, 1, summary.AttachmentsStored)
			var path, hash string
			var size int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT storage_path,content_hash,size FROM attachments`).Scan(&path, &hash, &size))
			assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", hash)
			assert.Zero(t, size)
			data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, path))
			require.NoError(t, err)
			assert.Empty(t, data)
		})
	}
}

type cancelWriter struct{ cancel context.CancelFunc }

func (w cancelWriter) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }

func TestImportGoogleChatInterruptedReplay(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	for _, group := range []string{"DM a", "Space b"} {
		write(t, root, "Groups/"+group+"/messages.json", strings.Replace(one, "group/topic/message", group+"/topic/message", 1))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}, Logger: slog.New(slog.NewTextHandler(cancelWriter{cancel}, nil))}
	summary, err := Import(ctx, st, opts)
	require.ErrorIs(t, err, context.Canceled)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	var status string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs`).Scan(&status))
	assert.Equal(t, "failed", status)
	opts.Logger = nil
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	assert.Equal(t, 1, summary.Duplicates)
}

func TestImportGoogleChatValidation(t *testing.T) {
	for _, tc := range []struct{ name, me, path, data, want string }{
		{"owner missing", "", "Groups/DM a/messages.json", one, "--me"},
		{"display address", "Example <owner@example.com>", "Groups/DM a/messages.json", one, "--me"},
		{"owner conflict", "other@example.com", "Users/User a/user_info.json", `{"user":{"email":"owner@example.com"}}`, "disagrees"},
		{"malformed", "owner@example.com", "Groups/DM a/messages.json", `{"messages":[`, "JSON"},
		{"no layout", "owner@example.com", "unrelated.json", one, "layout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, tc.path, tc.data)
			summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: tc.me, Roots: []string{root}})
			require.ErrorContains(t, err, tc.want)
			assert.Zero(t, summary.MessagesAdded)
		})
	}
	t.Run("archive", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "takeout.zip", "")
		_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: "owner@example.com", Roots: []string{filepath.Join(root, "takeout.zip")}})
		require.ErrorContains(t, err, "extract")
	})
	t.Run("conflicting JSON", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		write(t, a, "Groups/DM a/messages.json", one)
		write(t, b, "Groups/DM a/messages.json", strings.Replace(one, "Bodyneedle", "changed", 1))
		_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: "owner@example.com", Roots: []string{a, b}})
		require.ErrorContains(t, err, "conflict")
	})
}

func TestImportGoogleChatMediaSafety(t *testing.T) {
	for _, tc := range []struct{ name, ref string }{{"traversal", "../secret"}, {"absolute", "/secret"}, {"backslash", `..\\secret`}, {"missing", "File-example.txt"}, {"escaped symlink", "escape.txt"}, {"oversized", "large.txt"}, {"ambiguous", "duplicate.txt"}} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			a, b := t.TempDir(), t.TempDir()
			write(t, a, "Groups/DM a/messages.json", strings.ReplaceAll(one, "File-example.txt", tc.ref))
			dir := filepath.Join(a, "Groups/DM a")
			switch tc.name {
			case "escaped symlink":
				write(t, b, "secret", "secret")
				require.NoError(t, os.Symlink(filepath.Join(b, "secret"), filepath.Join(dir, tc.ref)))
			case "oversized":
				f, err := os.Create(filepath.Join(dir, tc.ref))
				require.NoError(t, err)
				require.NoError(t, f.Truncate(251<<20))
				require.NoError(t, f.Close())
			case "ambiguous":
				write(t, a, "Groups/DM a/duplicate.txt", "a")
				write(t, b, "Groups/DM a/duplicate.txt", "b")
			}
			summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{a, b}, AttachmentsDir: t.TempDir()})
			if tc.name == "ambiguous" {
				require.ErrorContains(t, err, "ambiguous")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 1, summary.AttachmentsMissing)
			assert.Zero(t, summary.AttachmentsStored)
		})
	}
}

func TestImportGoogleChatStorageFailure(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", one)
	write(t, root, "Groups/DM a/File-example.txt", "bytes")
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, []byte("blocked"), 0600))
	summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: "owner@example.com", Roots: []string{root}, AttachmentsDir: blocked})
	require.ErrorContains(t, err, "store media")
	assert.Zero(t, summary.MessagesAdded)
}

func TestImportGoogleChatMembershipIsAdditiveOnDuplicateReplay(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/Space a/messages.json", one)
	write(t, root, "Groups/Space a/group_info.json", `{"members":[{"email":"peer@example.net","name":"Example Peer"}]}`)
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}}
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	write(t, root, "Groups/Space a/group_info.json", `{"members":[{"email":"later@example.org","name":"Example Later"}]}`)
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Duplicates)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_participants`).Scan(&n))
	assert.Equal(t, 3, n, "original member, original sender and later member remain")
}

func TestImportGoogleChatConflictingDuplicateRecord(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	first := strings.TrimSuffix(strings.TrimPrefix(one, `{"messages":[`), `]}`)
	write(t, root, "Groups/DM a/messages.json", `{"messages":[`+first+`,`+strings.Replace(first, "Bodyneedle", "changed", 1)+`]}`)
	summary, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.ErrorContains(t, err, "conflicting message ID")
	assert.EqualValues(t, 1, summary.MessagesAdded)
}

func TestImportGoogleChatReplayKeepsOriginalMediaSlots(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	original := strings.Replace(one, `"export_name":"File-example.txt"`, `"export_name":"File-example.txt"},{"original_name":"second.txt","export_name":"File-second.txt"`, 1)
	write(t, root, "Groups/DM a/messages.json", original)
	write(t, root, "Groups/DM a/File-example.txt", "first bytes")
	opts := ImportOptions{Me: "owner@example.com", Roots: []string{root}, AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsStored)
	assert.Equal(t, 1, summary.AttachmentsMissing)
	// A changed export moves a different attachment into the missing array slot.
	// Repair must use the archived provider reference, never the new slot order.
	write(t, root, "Groups/DM a/messages.json", strings.ReplaceAll(original, "File-second.txt", "File-other.txt"))
	write(t, root, "Groups/DM a/File-other.txt", "wrong bytes")
	write(t, root, "Groups/DM a/File-second.txt", "second bytes")
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsStored)
	var path string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT storage_path FROM attachments WHERE source_part_key = 'file:1'`).Scan(&path))
	content, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, path))
	require.NoError(t, err)
	assert.Equal(t, "second bytes", string(content))
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
			st := testutil.NewTestStore(t)
			opts := ImportOptions{Me: "person1@example.com", Roots: []string{filepath.Join("testdata", "redacted-public", tc.name)}, AttachmentsDir: t.TempDir()}
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Equal(t, tc.messages, summary.MessagesAdded)
			assert.Equal(t, tc.stored, summary.AttachmentsStored)
			assert.Equal(t, tc.missing, summary.AttachmentsMissing)
			assert.Zero(t, summary.DatesUnparsed)
			assert.Zero(t, summary.UnknownSenders)
			assert.Zero(t, summary.Unsupported)
			var count int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE sent_at IS NOT NULL`).Scan(&count))
			assert.Equal(t, tc.messages, count)
			files, err := filepath.Glob(filepath.Join(opts.Roots[0], "Groups", "*", "messages.json"))
			require.NoError(t, err)
			require.Len(t, files, 1)
			input, err := os.ReadFile(files[0])
			require.NoError(t, err)
			var envelope struct {
				Messages []jsontext.Value `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(input, &envelope))
			for _, record := range envelope.Messages {
				var provider message
				require.NoError(t, json.Unmarshal(record, &provider))
				var id int64
				require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), provider.ID).Scan(&id))
				raw, err := st.GetMessageRawContext(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, string(record), string(raw), "all optional metadata and absent text remain intact")
			}
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Zero(t, summary.MessagesAdded)
			assert.EqualValues(t, tc.messages, summary.Duplicates)
			assert.Zero(t, summary.AttachmentsStored)
			assert.Equal(t, tc.missing, summary.AttachmentsMissing)
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
			root := t.TempDir()
			write(t, root, "Groups/"+tc.group+"/messages.json", one)
			if tc.roster != "" {
				write(t, root, "Groups/"+tc.group+"/group_info.json", tc.roster)
			}
			st := testutil.NewTestStore(t)
			_, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
			require.NoError(t, err)
			var kind string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT conversation_type FROM conversations`).Scan(&kind))
			assert.Equal(t, tc.want, kind)
		})
	}
}
