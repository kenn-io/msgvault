package signalexport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const header = `{"version":"1","backupTimeMs":"1730000000000","currentAppVersion":"fictional-test"}`
const account = `{"account":{}}`
const self = `{"recipient":{"id":"1","self":{}}}`
const contact = `{"recipient":{"id":"2","contact":{"aci":"AQEBAQEBAQEBAQEBAQEBAQ==","profileGivenName":"Example"}}}`
const direct = `{"chat":{"id":"7","recipientId":"2"}}`
const incoming = `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"Hello 星"}}}}`

func fixture(t *testing.T, frames ...string) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"version":1}`), 0600))
	writeFrames(t, root, frames...)
	return root
}
func writeFrames(t *testing.T, root string, frames ...string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(append([]string{header, account, self, contact, direct}, frames...), "\n")+"\n"), 0600))
}
func options(t *testing.T, root string) ImportOptions {
	t.Helper()
	return ImportOptions{RootDir: root, Account: "fictional", AttachmentsDir: t.TempDir()}
}
func messageCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	return n
}
func TestImportSnapshotsAndScope(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming)
	opts := options(t, root)
	first, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, first.MessagesAdded)
	var id, conv int64
	var key string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id, conversation_id, source_message_id FROM messages").Scan(&id, &conv, &key))
	raw, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	assert.JSONEq(t, strings.TrimSuffix(strings.TrimPrefix(incoming, `{"chatItem":`), `}`), string(raw))
	// Every local ID changes, and an edit changes the top-level time.
	edited := `{"chatItem":{"chatId":"70","authorId":"20","dateSent":"1700000005000","incoming":{},"revisions":[{"chatId":"70","authorId":"20","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"Hello 星"}}}],"standardMessage":{"text":{"body":"edited"}}}}`
	lines := []string{header, account, strings.Replace(self, `"1"`, `"10"`, 1), strings.Replace(contact, `"2"`, `"20"`, 1), `{"chat":{"id":"70","recipientId":"20"}}`, edited}
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
	second, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 0, second.MessagesAdded)
	assert.EqualValues(t, 1, second.MessagesExisting)
	assert.EqualValues(t, 1, second.MessagesChanged)
	assert.Equal(t, 1, messageCount(t, st))
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "Hello 星", body)
	again, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	assert.Equal(t, raw, again)
	opts.Account = "another-fictional-account"
	third, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.NotEqual(t, first.SourceID, third.SourceID)
	assert.Equal(t, 2, messageCount(t, st))
}
func TestImportDirectionsGroupsAndOmissions(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming,
		`{"recipient":{"id":"3","group":{"masterKey":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=","snapshot":{"title":{"title":"Example group"}}}}}`,
		`{"chat":{"id":"8","recipientId":"3"}}`,
		`{"chat":{"id":"9","recipientId":"1"}}`,
		`{"chatItem":{"chatId":"8","authorId":"1","dateSent":"1700000000001","outgoing":{},"pinDetails":{"pinnedAtTimestamp":"1700000000500","pinNeverExpires":true},"standardMessage":{"text":{"body":"Group"},"quote":{},"reactions":[{}],"linkPreview":[{}]}}}`,
		`{"chatItem":{"chatId":"9","authorId":"1","dateSent":"1700000000002","outgoing":{},"standardMessage":{"text":{"body":"Note"}}}}`,
		`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000004","incoming":{},"remoteDeletedMessage":{}}}`,
		`{"futureFrame":{}}`, `{}`)
	summary, err := Import(t.Context(), st, options(t, root))
	require.NoError(t, err)
	assert.EqualValues(t, 3, summary.MessagesAdded)
	assert.EqualValues(t, 1, summary.Skipped["remoteDeletedMessage"])
	assert.EqualValues(t, 1, summary.Omitted["quote"])
	assert.EqualValues(t, 1, summary.Omitted["pin"])
	assert.EqualValues(t, 1, summary.Omitted["reactions"])
	assert.EqualValues(t, 1, summary.UnknownFrames)
	assert.EqualValues(t, 1, summary.EmptyFrames)
	var fromMe, owners int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE is_from_me = true").Scan(&fromMe))
	assert.Equal(t, 2, fromMe)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM account_identities").Scan(&owners))
	assert.Equal(t, 1, owners)
	var fabricated int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM message_recipients r JOIN messages m ON m.id=r.message_id JOIN conversations c ON c.id=m.conversation_id WHERE c.conversation_type='group_chat' AND r.recipient_type='to'").Scan(&fabricated))
	assert.Zero(t, fabricated)
}
func pointer(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf(`{"contentType":"text/plain","fileName":"../../display.txt","locatorInfo":{"plaintextHash":%q,"localKey":"AQ=="}}`, base64.StdEncoding.EncodeToString(hash[:]))
}
func mediaMessage(data []byte, long bool) string {
	standard := fmt.Sprintf(`{"attachments":[{"pointer":%s},{"pointer":%s}]}`, pointer(data), pointer(data))
	if long {
		standard = fmt.Sprintf(`{"text":{"body":"inline"},"longText":%s}`, pointer(data))
	}
	return fmt.Sprintf(`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":%s}}`, standard)
}
func mediaPath(root string, data []byte) string {
	hash := sha256.Sum256(data)
	name := hex.EncodeToString(hash[:])
	return filepath.Join(root, "files", name[:2], name+".txt")
}
func putMedia(t *testing.T, root string, data []byte) string {
	t.Helper()
	path := mediaPath(root, data)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, data, 0600))
	return path
}
func TestImportMediaRepairOccurrencesAndLongText(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(fmt.Sprint(long), func(t *testing.T) {
			st := testutil.NewTestStore(t)
			data := []byte("Fictional long text 星")
			root := fixture(t, mediaMessage(data, long))
			opts := options(t, root)
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			want := 2
			if long {
				want = 1
			}
			assert.Equal(t, want, summary.AttachmentsMissing)
			path := putMedia(t, root, data)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Equal(t, want, summary.AttachmentsStored)
			assert.Zero(t, summary.AttachmentsMissing)
			assert.Zero(t, summary.Omitted["incomplete_long_text"])
			changed := int64(0)
			if long {
				changed = 1 // Full text differs from the first retained inline body.
			}
			assert.Equal(t, changed, summary.MessagesChanged)
			var id int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, refs, want)
			for _, ref := range refs {
				assert.NotEmpty(t, ref.ContentHash)
				assert.Equal(t, "display.txt", ref.Filename)
			}
			require.NoError(t, os.Remove(path))
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, 1, summary.MessagesExisting)
			assert.Equal(t, changed, summary.MessagesChanged)
			assert.Zero(t, summary.AttachmentsStored)
			assert.Zero(t, summary.AttachmentsMissing)
			assert.Zero(t, summary.Omitted["incomplete_long_text"])
			refs, err = st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(t, err)
			for _, ref := range refs {
				assert.NotEmpty(t, ref.ContentHash)
			}
			if long {
				// First snapshot is retained even after missing long text becomes available.
				body, err := st.GetMessageBodyText(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, "inline", body)
				st2 := testutil.NewTestStore(t)
				putMedia(t, root, data)
				_, err = Import(t.Context(), st2, opts)
				require.NoError(t, err)
				require.NoError(t, st2.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
				body, err = st2.GetMessageBodyText(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, string(data), body)
			}
		})
	}
}
func TestImportUnsafeMedia(t *testing.T) {
	for _, tc := range []struct {
		kind, reason string
		long         bool
	}{
		{"wrong-hash", "hash_mismatch", false}, {"final-symlink", "unsafe_or_nonregular", false},
		{"directory-symlink", "missing_or_unsafe", false}, {"directory", "unsafe_or_nonregular", false},
		{"ambiguous", "ambiguous", false}, {"extensionless-positive", "", false},
		{"encrypted-name", "missing", false}, {"too-large", "oversized", false},
		{"too-large", "oversized", true}, {"wrong-hash", "hash_mismatch", true},
	} {
		kind := tc.kind
		t.Run(fmt.Sprintf("%s/long=%t", kind, tc.long), func(t *testing.T) {
			st := testutil.NewTestStore(t)
			data := []byte("fictional media")
			root := fixture(t, mediaMessage(data, tc.long))
			path := putMedia(t, root, data)
			switch kind {
			case "wrong-hash":
				require.NoError(t, os.WriteFile(path, []byte("wrong"), 0600))
			case "final-symlink":
				require.NoError(t, os.Remove(path))
				outside := filepath.Join(t.TempDir(), "outside")
				require.NoError(t, os.WriteFile(outside, data, 0600))
				require.NoError(t, os.Symlink(outside, path))
			case "directory-symlink":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Remove(filepath.Dir(path)))
				outside := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(outside, filepath.Base(path)), data, 0600))
				require.NoError(t, os.Symlink(outside, filepath.Dir(path)))
			case "directory":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0700))
			case "ambiguous":
				require.NoError(t, os.WriteFile(strings.TrimSuffix(path, ".txt")+".bin", data, 0600))
			case "extensionless-positive":
				require.NoError(t, os.Rename(path, strings.TrimSuffix(path, ".txt")))
			case "encrypted-name":
				require.NoError(t, os.Rename(path, filepath.Join(filepath.Dir(path), strings.Repeat("a", 64))))
			case "too-large":
				file, err := os.OpenFile(path, os.O_WRONLY, 0600)
				require.NoError(t, err)
				limit := int64(256 << 20)
				if tc.long {
					limit = 16 << 20
				}
				require.NoError(t, file.Truncate(limit+1))
				require.NoError(t, file.Close())
			}
			opts := options(t, root)
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			want := 2
			if tc.long {
				want = 1
			}
			if tc.reason != "" {
				assert.EqualValues(t, want, summary.MediaMissing[tc.reason])
			}
			if tc.long {
				assert.EqualValues(t, 1, summary.Omitted["incomplete_long_text"])
				var id int64
				require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
				body, err := st.GetMessageBodyText(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, "inline", body)
			}
			if kind == "wrong-hash" {
				var id int64
				require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
				refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, refs, want)
				for _, ref := range refs {
					assert.Empty(t, ref.ContentHash)
				}
				wrong := sha256.Sum256([]byte("wrong"))
				hash := hex.EncodeToString(wrong[:])
				assert.NoFileExists(t, filepath.Join(opts.AttachmentsDir, hash[:2], hash))
			}
			if kind == "extensionless-positive" {
				assert.Equal(t, want, summary.AttachmentsStored)
			} else {
				assert.Equal(t, want, summary.AttachmentsMissing)
			}
		})
	}
}
func TestReaderValidation(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"header", `{"version":1}`, "version"},
		{"account", header + "\n{}", "account"},
		{"duplicate", strings.Join([]string{header, account, self, `{"recipient":{"id":"1","contact":{"aci":"AQEBAQEBAQEBAQEBAQEBAQ=="}}}`}, "\n"), "conflicting"},
		{"truncated", header + "\n" + account + "\n{", "line 3"},
		{"multiple", header + "\n" + account + "\n{} {}", "line 3"},
		{"null", header + "\n" + account + "\nnull", "line 3"},
		{"second-account", header + "\n" + account + "\n" + account, "account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixture(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(tc.content), 0600))
			_, err := Import(t.Context(), testutil.NewTestStore(t), options(t, root))
			require.ErrorContains(t, err, tc.want)
		})
	}
}
func TestReaderBounds(t *testing.T) {
	for _, kind := range []string{"record", "metadata", "entries", "strings"} {
		t.Run(kind, func(t *testing.T) {
			root := fixture(t, incoming)
			if kind == "record" {
				writeFrames(t, root, `{"padding":"`+strings.Repeat("x", 16<<20)+`"}`)
			}
			if kind == "metadata" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "metadata.json"), []byte(strings.Repeat(" ", 64<<10+1)), 0600))
			}
			r, err := openReader(t.Context(), root)
			if kind == "metadata" {
				require.ErrorContains(t, err, "metadata")
				return
			}
			require.NoError(t, err)
			defer r.Close()
			if kind == "entries" {
				r.maxEntries = 1
			}
			if kind == "strings" {
				r.maxStrings = 1
			}
			err = r.frames(t.Context(), func(_ *chatItem, _ []byte, _ int) error { return nil })
			require.Error(t, err)
			assert.Contains(t, err.Error(), map[string]string{"record": "record", "entries": "lookup entries", "strings": "lookup strings"}[kind])
		})
	}
}
func TestProtoScalarsAndRevisionValidation(t *testing.T) {
	var item chatItem
	require.NoError(t, json.Unmarshal([]byte(`{"chatId":"18446744073709551615","dateSent":"1700000000000"}`), &item))
	assert.Equal(t, "18446744073709551615", string(item.ChatID))
	for _, suffix := range []string{`"authorId":"1","dateSent":"1699999999999"`, `"authorId":"2","dateSent":"1700000000001"`, `"authorId":"2","dateSent":"0"`} {
		root := fixture(t, `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"revisions":[{"chatId":"7",`+suffix+`}],"standardMessage":{}}}`)
		summary, err := Import(t.Context(), testutil.NewTestStore(t), options(t, root))
		require.NoError(t, err)
		assert.EqualValues(t, 1, summary.Skipped["revision"])
	}
}

func TestImportIdentityTimeAndDirectionSkips(t *testing.T) {
	cases := []struct{ frame, reason string }{
		{`{"chatItem":{"chatId":"7","authorId":"2","incoming":{},"standardMessage":{}}}`, "sent_time"},
		{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000003","outgoing":{},"standardMessage":{}}}`, "direction_conflict"},
		{`{"chatItem":{"chatId":"99","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{}}}`, "unresolved_chat"},
		{`{"chatItem":{"chatId":"7","authorId":"99","dateSent":"1700000000000","incoming":{},"standardMessage":{}}}`, "author_identity"},
		{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"0","incoming":{},"standardMessage":{}}}`, "sent_time"},
		{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"18446744073709551615","incoming":{},"standardMessage":{}}}`, "sent_time"},
		{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","directionless":{},"standardMessage":{}}}`, "unknown_direction"},
	}
	for _, tc := range cases {
		t.Run(tc.reason+tc.frame, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := fixture(t, tc.frame)
			s, err := Import(t.Context(), st, options(t, root))
			require.NoError(t, err)
			assert.EqualValues(t, 1, s.Skipped[tc.reason])
			assert.Zero(t, messageCount(t, st))
		})
	}
	for _, recipient := range []string{`{"contact":{"pni":"AQEBAQEBAQEBAQEBAQEBAQ==","e164":"15555550100"}}`, `{"contact":{"aci":"AQ=="}}`, `{"group":{"masterKey":"AQ=="}}`, `{"contact":{"profileGivenName":"Fictional name only"}}`} {
		t.Run(recipient, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := fixture(t)
			lines := []string{header, account, self, `{"recipient":{"id":"2",` + strings.TrimPrefix(recipient, "{") + `}`, direct, incoming}
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
			s, err := Import(t.Context(), st, options(t, root))
			require.NoError(t, err)
			assert.EqualValues(t, 1, s.Skipped["recipient_identity"])
		})
	}
}

// Not parallel: CancelAfterCommit replaces process-wide SQL logging.
func TestImportCancellationRetainsCommittedSnapshot(t *testing.T) {
	for _, kind := range []string{"parse failure at line 7", "cancel after first commit", "already canceled"} {
		t.Run(kind, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			second := strings.Replace(incoming, "1700000000000", "1700000000001", 1)
			if kind == "parse failure at line 7" {
				second = `{"chatItem":`
			}
			root := fixture(t, incoming, second)
			opts := options(t, root)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "already canceled" {
				cancel()
			} else if kind == "cancel after first commit" {
				canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
					var n int
					require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
					return n > 0
				})
				defer func() { assert.True(t, canceled()) }()
			}
			summary, err := Import(ctx, st, opts)
			if kind == "parse failure at line 7" {
				require.ErrorContains(t, err, "line 7")
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
			if kind == "already canceled" {
				assert.Zero(t, summary.MessagesAdded)
				assert.Zero(t, messageCount(t, st))
				return
			}
			assert.EqualValues(t, 1, summary.MessagesAdded)
			assert.Equal(t, 1, messageCount(t, st))
			writeFrames(t, root, incoming, strings.Replace(incoming, "1700000000000", "1700000000001", 1))
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, 1, summary.MessagesAdded)
			assert.EqualValues(t, 1, summary.MessagesExisting)
			assert.Equal(t, 2, messageCount(t, st))
		})
	}
}
func TestImportMediaStorageFailureIsFatal(t *testing.T) {
	st := testutil.NewTestStore(t)
	data := []byte("fictional store failure")
	root := fixture(t, mediaMessage(data, false))
	putMedia(t, root, data)
	opts := options(t, root)
	path := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0600))
	opts.AttachmentsDir = path
	s, err := Import(t.Context(), st, opts)
	require.ErrorContains(t, err, "store Signal media")
	assert.Zero(t, s.MessagesAdded)
}
func TestNativeFixtureOriginalTimeAndProvenance(t *testing.T) {
	st := testutil.NewTestStore(t)
	opts := options(t, "testdata/native-v1")
	s, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 3, s.MessagesAdded)
	assert.Equal(t, 1, s.AttachmentsStored)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE is_from_me = false").Scan(&id))
	var metadata, rawFormat string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT metadata FROM messages WHERE id = ?"), id).Scan(&metadata))
	assert.Contains(t, metadata, `"adapter_version":1`)
	assert.Contains(t, metadata, `"backupTimeMs":"1730000000000"`)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT raw_format FROM message_raw WHERE message_id = ?"), id).Scan(&rawFormat))
	assert.Equal(t, "signal-export-jsonl", rawFormat)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs[0].StoragePath))
	require.NoError(t, err)
	assert.Equal(t, "Wholly fictional Signal fixture media.\n", string(data))
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?", "workshop").Scan(&n))
	assert.Equal(t, 1, n)
	// Omitted records and tombstones do not remove existing messages.
	root := fixture(t, `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"remoteDeletedMessage":{}}}`)
	opts.RootDir = root
	s, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 3, messageCount(t, st))
}

func TestImportPopulatedLongTextMissingExportFile(t *testing.T) {
	st := testutil.NewTestStore(t)
	data := []byte("Fictional full long text")
	root := fixture(t, mediaMessage(data, true))
	opts := options(t, root)
	file := putMedia(t, root, data)
	s, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.AttachmentsStored)
	require.NoError(t, os.Remove(file))
	s, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, s.MessagesExisting)
	assert.Zero(t, s.MessagesChanged)
	assert.Zero(t, s.AttachmentsMissing)
	assert.Zero(t, s.Omitted["incomplete_long_text"])
}
func TestImportEditedItemOriginalSentAt(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000005000","incoming":{},"revisions":[{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"old"}}}],"standardMessage":{"text":{"body":"current"}}}}`)
	s, err := Import(t.Context(), st, options(t, root))
	require.NoError(t, err)
	assert.EqualValues(t, 1, s.MessagesAdded)
	var sent time.Time
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id,sent_at FROM messages").Scan(&id, &sent))
	assert.Equal(t, time.UnixMilli(1700000000000).UTC(), sent.UTC())
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "current", body)
}

func TestReaderSemanticDuplicateDefinitions(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, strings.Replace(contact, `"profileGivenName":"Example"`, `"profileGivenName":"Example","blocked":true`, 1), `{"chat":{"id":"7","recipientId":"2","archived":true}}`, incoming)
	s, err := Import(t.Context(), st, options(t, root))
	require.NoError(t, err)
	assert.EqualValues(t, 1, s.MessagesAdded)
}

func TestImportRepairUsesFirstMediaPointers(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(fmt.Sprint(long), func(t *testing.T) {
			frame := func(data []byte) string {
				standard := fmt.Sprintf(`{"text":{"body":"inline"},"attachments":[{"pointer":%s}]}`, pointer(data))
				if long {
					standard = fmt.Sprintf(`{"text":{"body":"inline"},"longText":%s}`, pointer(data))
				}
				return fmt.Sprintf(`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":%s}}`, standard)
			}
			st := testutil.NewTestStore(t)
			a, b := []byte("original fictional media"), []byte("replacement fictional media")
			root := fixture(t, frame(a))
			opts := options(t, root)
			_, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			var id int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
			raw, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(t, err)
			putMedia(t, root, b)
			writeFrames(t, root, frame(b))
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Equal(t, int64(1), summary.MediaMissing["missing"])
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, refs, 1)
			assert.Empty(t, refs[0].ContentHash)
			wrong := sha256.Sum256(b)
			hash := hex.EncodeToString(wrong[:])
			assert.NoFileExists(t, filepath.Join(opts.AttachmentsDir, hash[:2], hash))
			retained, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, raw, retained)
			body, err := st.GetMessageBodyText(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, "inline", body)
			putMedia(t, root, a)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Equal(t, 1, summary.AttachmentsStored)
			refs, err = st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(t, err)
			original := sha256.Sum256(a)
			assert.Equal(t, hex.EncodeToString(original[:]), refs[0].ContentHash)
		})
	}
}

func TestMediaPrefixCacheBoundedLookup(t *testing.T) {
	for _, cacheLimit := range []int{2, 3} {
		t.Run(fmt.Sprint(cacheLimit), func(t *testing.T) {
			a, b := []byte("fictional cached media 0"), []byte("fictional cached media 1")
			root := fixture(t)
			pathA, pathB := putMedia(t, root, a), putMedia(t, root, b)
			require.Equal(t, filepath.Dir(pathA), filepath.Dir(pathB))
			warm := []byte("fictional warm cache")
			warmPath := putMedia(t, root, warm)
			require.NotEqual(t, filepath.Dir(pathA), filepath.Dir(warmPath))
			r, err := openReader(t.Context(), root)
			require.NoError(t, err)
			defer r.Close()
			r.maxMedia = cacheLimit
			warmHash := sha256.Sum256(warm)
			_, reason, err := r.mediaName(t.Context(), hex.EncodeToString(warmHash[:]))
			require.NoError(t, err)
			require.Empty(t, reason)
			var message standardMessage
			require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"attachments":[{"pointer":%s},{"pointer":%s}]}`, pointer(a), pointer(b))), &message))
			summary := &ImportSummary{MediaMissing: map[string]int64{}, Omitted: map[string]int64{}}
			_, writes, _, err := mediaWrites(t.Context(), testutil.NewTestStore(t), r, 0, &message, options(t, root), summary)
			require.NoError(t, err)
			require.Len(t, writes, 2)
			assert.Equal(t, 2, summary.AttachmentsStored)
			for i, data := range [][]byte{a, b} {
				hash := sha256.Sum256(data)
				assert.Equal(t, hex.EncodeToString(hash[:]), writes[i].ContentHash)
			}
			assert.LessOrEqual(t, r.mediaNames, cacheLimit)
			hashA := sha256.Sum256(a)
			hash := hex.EncodeToString(hashA[:])
			_, cached := r.media[hash[:2]]
			assert.Equal(t, cacheLimit == 3, cached)
			if cacheLimit == 2 {
				require.NoError(t, os.WriteFile(strings.TrimSuffix(pathA, ".txt")+".bin", a, 0600))
				_, reason, err := r.mediaName(t.Context(), hash)
				require.NoError(t, err)
				assert.Equal(t, "ambiguous", reason)
				assert.NotContains(t, r.media, hash[:2])
			}
		})
	}
}

func TestImportRepairRemovedAlbumSlot(t *testing.T) {
	a, b := []byte("fictional removed image"), []byte("fictional retained image")
	frame := func(attachments string) string {
		return fmt.Sprintf(`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"album"},"attachments":[%s]}}}`, attachments)
	}
	st := testutil.NewTestStore(t)
	root := fixture(t, frame(fmt.Sprintf(`{"pointer":%s},{"pointer":%s}`, pointer(a), pointer(b))))
	opts := options(t, root)
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	putMedia(t, root, b)
	writeFrames(t, root, frame(fmt.Sprintf(`{"pointer":%s}`, pointer(b))))
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsStored)
	assert.EqualValues(t, 1, summary.MediaMissing["missing"])
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	hash := sha256.Sum256(b)
	for _, ref := range refs {
		switch ref.SourcePartKey {
		case "attachment:0":
			assert.Empty(t, ref.ContentHash)
		case "attachment:1":
			assert.Equal(t, hex.EncodeToString(hash[:]), ref.ContentHash)
		default:
			assert.Fail(t, "unexpected media slot", ref.SourcePartKey)
		}
	}
	retained, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, raw, retained)
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "album", body)
}

func TestImportLongTextTurnsInline(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, strings.Replace(mediaMessage([]byte("short and the rest"), true), `"inline"`, `"short"`, 1))
	opts := options(t, root)
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	writeFrames(t, root, strings.Replace(incoming, "Hello 星", "short and the rest", 1))
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MediaMissing["missing"])
	assert.EqualValues(t, 1, summary.Omitted["incomplete_long_text"])
	assert.EqualValues(t, 1, summary.MessagesChanged)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "short", body)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "long_text", refs[0].SourcePartKey)
	assert.Empty(t, refs[0].ContentHash)
}
