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
	"strconv"
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
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming)
	opts := options(t, root)
	first, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, first.MessagesAdded)
	var id, conv int64
	var key string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id, conversation_id, source_message_id FROM messages").Scan(&id, &conv, &key))
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.JSONEq(strings.TrimSuffix(strings.TrimPrefix(incoming, `{"chatItem":`), `}`), string(raw))
	// Every local ID changes, and an edit changes the top-level time.
	edited := `{"chatItem":{"chatId":"70","authorId":"20","dateSent":"1700000005000","incoming":{},"revisions":[{"chatId":"70","authorId":"20","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"Hello 星"}}}],"standardMessage":{"text":{"body":"edited"}}}}`
	lines := []string{header, account, strings.Replace(self, `"1"`, `"10"`, 1), strings.Replace(contact, `"2"`, `"20"`, 1), `{"chat":{"id":"70","recipientId":"20"}}`, edited}
	require.NoError(os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
	second, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(0, second.MessagesAdded)
	assert.EqualValues(1, second.MessagesExisting)
	assert.EqualValues(1, second.MessagesChanged)
	assert.Equal(1, messageCount(t, st))
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(err)
	assert.Equal("Hello 星", body)
	again, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, again)
	opts.Account = "another-fictional-account"
	third, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.NotEqual(first.SourceID, third.SourceID)
	assert.Equal(2, messageCount(t, st))
}
func TestImportDirectionsGroupsAndOmissions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)
	assert.EqualValues(3, summary.MessagesAdded)
	assert.EqualValues(1, summary.Skipped["remoteDeletedMessage"])
	assert.EqualValues(1, summary.Omitted["quote"])
	assert.EqualValues(1, summary.Omitted["pin"])
	assert.EqualValues(1, summary.Omitted["reactions"])
	assert.EqualValues(1, summary.UnknownFrames)
	assert.EqualValues(1, summary.EmptyFrames)
	var fromMe, owners int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE is_from_me = true").Scan(&fromMe))
	assert.Equal(2, fromMe)
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM account_identities").Scan(&owners))
	assert.Equal(1, owners)
	var fabricated int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM message_recipients r JOIN messages m ON m.id=r.message_id JOIN conversations c ON c.id=m.conversation_id WHERE c.conversation_type='group_chat' AND r.recipient_type='to'").Scan(&fabricated))
	assert.Zero(fabricated)
}
func pointer(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf(`{"contentType":"text/plain","fileName":"../../display.txt","locatorInfo":{"plaintextHash":%q,"key":"AQ=="}}`, base64.StdEncoding.EncodeToString(hash[:]))
}
func mediaMessage(data []byte, long bool) string {
	standard := fmt.Sprintf(`{"attachments":[{"clientUuid":"AQ==","pointer":%s},{"clientUuid":"Ag==","pointer":%s}]}`, pointer(data), pointer(data))
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
		t.Run(strconv.FormatBool(long), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			data := []byte("Fictional long text 星")
			if !long {
				data = []byte{0, 0, 0, 1} // Two fictional 16-bit PCM samples.
			}
			frame := mediaMessage(data, long)
			if !long {
				frame = strings.ReplaceAll(frame, `"contentType":"text/plain"`, `"contentType":"audio/L16;rate=8000;channels=1"`)
				frame = strings.Replace(frame, `"pointer":`, `"flag":"VOICE_MESSAGE","pointer":`, 1)
			}
			root := fixture(t, frame)
			opts := options(t, root)
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			want := 2
			if long {
				want = 1
			}
			assert.Equal(want, summary.AttachmentsMissing)
			path := putMedia(t, root, data)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(want, summary.AttachmentsStored)
			assert.Zero(summary.AttachmentsMissing)
			assert.Zero(summary.Omitted["incomplete_long_text"])
			changed := int64(0)
			if long {
				changed = 1 // Full text differs from the first retained inline body.
			}
			assert.Equal(changed, summary.MessagesChanged)
			var id int64
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(err)
			require.Len(refs, want)
			if !long {
				assert.EqualValues(1, summary.Omitted["attachment_flag"])
				stored, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs[0].StoragePath))
				require.NoError(err)
				assert.Equal(data, stored)
			}
			for _, ref := range refs {
				assert.NotEmpty(ref.ContentHash)
				assert.Equal("display.txt", ref.Filename)
			}
			require.NoError(os.Remove(path))
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.EqualValues(1, summary.MessagesExisting)
			assert.Equal(changed, summary.MessagesChanged)
			assert.Zero(summary.AttachmentsStored)
			assert.Zero(summary.AttachmentsMissing)
			assert.Zero(summary.Omitted["incomplete_long_text"])
			refs, err = st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(err)
			for _, ref := range refs {
				assert.NotEmpty(ref.ContentHash)
			}
			if long {
				// First snapshot is retained even after missing long text becomes available.
				body, err := st.GetMessageBodyText(t.Context(), id)
				require.NoError(err)
				assert.Equal("inline", body)
				st2 := testutil.NewTestStore(t)
				putMedia(t, root, data)
				_, err = Import(t.Context(), st2, opts)
				require.NoError(err)
				require.NoError(st2.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
				body, err = st2.GetMessageBodyText(t.Context(), id)
				require.NoError(err)
				assert.Equal(string(data), body)
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
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			data := []byte("fictional media")
			root := fixture(t, mediaMessage(data, tc.long))
			path := putMedia(t, root, data)
			switch kind {
			case "wrong-hash":
				require.NoError(os.WriteFile(path, []byte("wrong"), 0600))
			case "final-symlink":
				require.NoError(os.Remove(path))
				outside := filepath.Join(t.TempDir(), "outside")
				require.NoError(os.WriteFile(outside, data, 0600))
				require.NoError(os.Symlink(outside, path))
			case "directory-symlink":
				require.NoError(os.Remove(path))
				require.NoError(os.Remove(filepath.Dir(path)))
				outside := t.TempDir()
				require.NoError(os.WriteFile(filepath.Join(outside, filepath.Base(path)), data, 0600))
				require.NoError(os.Symlink(outside, filepath.Dir(path)))
			case "directory":
				require.NoError(os.Remove(path))
				require.NoError(os.Mkdir(path, 0700))
			case "ambiguous":
				require.NoError(os.WriteFile(strings.TrimSuffix(path, ".txt")+".bin", data, 0600))
			case "extensionless-positive":
				require.NoError(os.Rename(path, strings.TrimSuffix(path, ".txt")))
			case "encrypted-name":
				require.NoError(os.Rename(path, filepath.Join(filepath.Dir(path), strings.Repeat("a", 64))))
			case "too-large":
				file, err := os.OpenFile(path, os.O_WRONLY, 0600)
				require.NoError(err)
				limit := int64(256 << 20)
				if tc.long {
					limit = 16 << 20
				}
				require.NoError(file.Truncate(limit + 1))
				require.NoError(file.Close())
			}
			opts := options(t, root)
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			want := 2
			if tc.long {
				want = 1
			}
			if tc.reason != "" {
				assert.EqualValues(want, summary.MediaMissing[tc.reason])
			}
			if tc.long {
				assert.EqualValues(1, summary.Omitted["incomplete_long_text"])
				var id int64
				require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
				body, err := st.GetMessageBodyText(t.Context(), id)
				require.NoError(err)
				assert.Equal("inline", body)
			}
			if kind == "wrong-hash" {
				var id int64
				require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
				refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
				require.NoError(err)
				require.Len(refs, want)
				for _, ref := range refs {
					assert.Empty(ref.ContentHash)
				}
				wrong := sha256.Sum256([]byte("wrong"))
				hash := hex.EncodeToString(wrong[:])
				assert.NoFileExists(filepath.Join(opts.AttachmentsDir, hash[:2], hash))
			}
			if kind == "extensionless-positive" {
				assert.Equal(want, summary.AttachmentsStored)
			} else {
				assert.Equal(want, summary.AttachmentsMissing)
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
			require := require.New(t)
			root := fixture(t, incoming)
			if kind == "record" {
				writeFrames(t, root, `{"padding":"`+strings.Repeat("x", 16<<20)+`"}`)
			}
			if kind == "metadata" {
				require.NoError(os.WriteFile(filepath.Join(root, "metadata.json"), []byte(`{"version":1}`+strings.Repeat(" ", 64<<10)), 0600))
			}
			r, err := openReader(t.Context(), root)
			if kind == "metadata" {
				require.EqualError(err, "signal export: metadata limit (64 KiB)")
				return
			}
			require.NoError(err)
			defer func() { require.NoError(r.Close()) }()
			if kind == "entries" {
				r.maxEntries = 1
			}
			if kind == "strings" {
				r.maxStrings = 1
			}
			err = r.frames(t.Context(), func(_ *chatItem, _ []byte, _ int) error { return nil })
			require.Error(err)
			assert.Contains(t, err.Error(), map[string]string{"record": "record", "entries": "lookup entries", "strings": "lookup strings"}[kind])
		})
	}
}
func TestProtoScalarsAndRevisionValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var item chatItem
	require.NoError(json.Unmarshal([]byte(`{"chatId":"18446744073709551615","dateSent":"1700000000000"}`), &item))
	assert.Equal("18446744073709551615", string(item.ChatID))
	for _, suffix := range []string{`"authorId":"1","dateSent":"1699999999999"`, `"authorId":"2","dateSent":"1700000000001"`, `"authorId":"2","dateSent":"0"`} {
		root := fixture(t, `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"revisions":[{"chatId":"7",`+suffix+`}],"standardMessage":{}}}`)
		summary, err := Import(t.Context(), testutil.NewTestStore(t), options(t, root))
		require.NoError(err)
		assert.EqualValues(1, summary.Skipped["revision"])
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
	for _, recipient := range []string{`{"contact":{"pni":"AQEBAQEBAQEBAQEBAQEBAQ=="}}`, `{"contact":{"aci":"AQ=="}}`, `{"group":{"masterKey":"AQ=="}}`, `{"contact":{"profileGivenName":"Fictional name only"}}`} {
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
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			second := strings.Replace(incoming, "1700000000000", "1700000000001", 1)
			if kind == "parse failure at line 7" {
				second = `{"chatItem":`
			}
			root := fixture(t, incoming, second)
			opts := options(t, root)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "already canceled":
				cancel()
			case "cancel after first commit":
				canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
					var n int
					require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
					return n > 0
				})
				defer func() { assert.True(canceled()) }()
			}
			summary, err := Import(ctx, st, opts)
			if kind == "parse failure at line 7" {
				require.ErrorContains(err, "line 7")
			} else {
				require.ErrorIs(err, context.Canceled)
			}
			if kind == "already canceled" {
				assert.Zero(summary.MessagesAdded)
				assert.Zero(messageCount(t, st))
				return
			}
			assert.EqualValues(1, summary.MessagesAdded)
			assert.Equal(1, messageCount(t, st))
			source, err := st.GetOrCreateSourceContext(t.Context(), SourceType, opts.Account)
			require.NoError(err)
			assert.JSONEq(`{"signal_export_format":1}`, source.SyncCursor.String)
			writeFrames(t, root, incoming, strings.Replace(incoming, "1700000000000", "1700000000001", 1))
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.EqualValues(1, summary.MessagesAdded)
			assert.EqualValues(1, summary.MessagesExisting)
			assert.Equal(2, messageCount(t, st))
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
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := options(t, "testdata/native-v1")
	s, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(3, s.MessagesAdded)
	assert.Equal(1, s.AttachmentsStored)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE is_from_me = false").Scan(&id))
	var metadata, rawFormat string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT metadata FROM messages WHERE id = ?"), id).Scan(&metadata))
	var provenance struct {
		Header struct {
			BackupTimeMs string `json:"backupTimeMs"`
		} `json:"header"`
	}
	require.NoError(json.Unmarshal([]byte(metadata), &provenance))
	assert.Equal("1730000000000", provenance.Header.BackupTimeMs)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT raw_format FROM message_raw WHERE message_id = ?"), id).Scan(&rawFormat))
	assert.Equal("signal-export-jsonl", rawFormat)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 1)
	data, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs[0].StoragePath))
	require.NoError(err)
	assert.Equal("Wholly fictional Signal fixture media.\n", string(data))
	_, total, err := st.SearchMessagesContext(t.Context(), "workshop", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
	// Omitted records and tombstones do not remove existing messages.
	root := fixture(t, `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"remoteDeletedMessage":{}}}`)
	opts.RootDir = root
	_, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(3, messageCount(t, st))
}

func TestImportPopulatedLongTextMissingExportFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	data := []byte("Fictional full long text")
	root := fixture(t, mediaMessage(data, true))
	opts := options(t, root)
	file := putMedia(t, root, data)
	s, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, s.AttachmentsStored)
	require.NoError(os.Remove(file))
	s, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, s.MessagesExisting)
	assert.Zero(s.MessagesChanged)
	assert.Zero(s.AttachmentsMissing)
	assert.Zero(s.Omitted["incomplete_long_text"])
}
func TestImportEditedItemOriginalSentAt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := fixture(t, `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000005000","incoming":{},"revisions":[{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"old"}}}],"standardMessage":{"text":{"body":"current"}}}}`)
	s, err := Import(t.Context(), st, options(t, root))
	require.NoError(err)
	assert.EqualValues(1, s.MessagesAdded)
	var sent time.Time
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id,sent_at FROM messages").Scan(&id, &sent))
	assert.Equal(time.UnixMilli(1700000000000).UTC(), sent.UTC())
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(err)
	assert.Equal("current", body)
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
		t.Run(strconv.FormatBool(long), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			frame := func(data []byte) string {
				standard := fmt.Sprintf(`{"text":{"body":"inline"},"attachments":[{"clientUuid":"AQ==","pointer":%s}]}`, pointer(data))
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
			require.NoError(err)
			var id int64
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
			raw, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			putMedia(t, root, b)
			writeFrames(t, root, frame(b))
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(int64(1), summary.MediaMissing["missing"])
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(err)
			require.Len(refs, 1)
			assert.Empty(refs[0].ContentHash)
			wrong := sha256.Sum256(b)
			hash := hex.EncodeToString(wrong[:])
			assert.NoFileExists(filepath.Join(opts.AttachmentsDir, hash[:2], hash))
			retained, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(raw, retained)
			body, err := st.GetMessageBodyText(t.Context(), id)
			require.NoError(err)
			assert.Equal("inline", body)
			putMedia(t, root, a)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(1, summary.AttachmentsStored)
			refs, err = st.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(err)
			original := sha256.Sum256(a)
			assert.Equal(hex.EncodeToString(original[:]), refs[0].ContentHash)
		})
	}
}

func TestMediaPrefixCacheBoundedLookup(t *testing.T) {
	root := fixture(t)
	a, b := []byte("fictional cached media 0"), []byte("fictional cached media 1")
	putMedia(t, root, a)
	putMedia(t, root, b)
	r, err := openReader(t.Context(), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	r.maxMedia = 2
	hash := sha256.Sum256(a)
	_, reason, err := r.mediaName(t.Context(), hex.EncodeToString(hash[:]))
	require.NoError(t, err)
	assert.Empty(t, reason)
	assert.Equal(t, 2, r.mediaNames)
	r2, err := openReader(t.Context(), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, r2.Close()) }()
	r2.maxMedia = 1
	_, reason, err = r2.mediaName(t.Context(), hex.EncodeToString(hash[:]))
	require.NoError(t, err)
	assert.Equal(t, "index_limit", reason)
}

func TestRepairNewHashByStableIdentity(t *testing.T) {
	st := testutil.NewTestStore(t)
	first := `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"original"},"attachments":[{"clientUuid":"AQ==","pointer":{"locatorInfo":{"key":"Ag=="}}}]}}}`
	root := fixture(t, first)
	opts := options(t, root)
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsMissing)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	data := []byte("fictional attachment")
	putMedia(t, root, data)
	p := pointer(data)
	second := `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"changed"},"attachments":[{"clientUuid":"Aw==","pointer":{}},{"clientUuid":"AQ==","pointer":` + p + `}]}}}`
	writeFrames(t, root, strings.Replace(second, `"clientUuid":"AQ=="`, `"clientUuid":"BA=="`, 1))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsMissing)
	assert.Zero(t, summary.AttachmentsStored)
	writeFrames(t, root, second)
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsStored)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	hash := sha256.Sum256(data)
	assert.Equal(t, "attachment:0", refs[0].SourcePartKey)
	assert.Equal(t, hex.EncodeToString(hash[:]), refs[0].ContentHash)
	again, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, raw, again)
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "original", body)
}

func TestContactIdentifierAdded(t *testing.T) {
	for _, contact := range []string{
		`{"aci":"AQEBAQEBAQEBAQEBAQEBAQ=="}`,
		`{"e164":"15555550100"}`,
	} {
		t.Run(contact, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := fixture(t, incoming)
			group := `{"recipient":{"id":"3","group":{"masterKey":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="}}}`
			groupChat := `{"chat":{"id":"8","recipientId":"3"}}`
			groupMessage := `{"chatItem":{"chatId":"8","authorId":"2","dateSent":"1700000000001","incoming":{},"standardMessage":{"attachments":[{"clientUuid":"AQ=="}]}}}`
			lines := []string{header, account, self, `{"recipient":{"id":"2","contact":` + contact + `}}`, direct, group, groupChat, incoming, groupMessage}
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
			opts := options(t, root)
			_, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			lines[3] = `{"recipient":{"id":"2","contact":{"e164":"15555550100","aci":"AQEBAQEBAQEBAQEBAQEBAQ=="}}}`
			data := []byte("fictional group media")
			putMedia(t, root, data)
			lines[8] = strings.Replace(groupMessage, `"clientUuid":"AQ=="`, `"clientUuid":"AQ==","pointer":`+pointer(data), 1)
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, 2, summary.MessagesExisting)
			assert.Equal(t, 1, summary.AttachmentsStored)
			assert.Equal(t, 2, messageCount(t, st))
			var conversations int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&conversations))
			assert.Equal(t, 2, conversations)
			lines[3] = `{"recipient":{"id":"2","contact":{"aci":"AQEBAQEBAQEBAQEBAQEBAQ=="}}}`
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, 2, summary.MessagesExisting)
			assert.Equal(t, 2, messageCount(t, st))
		})
	}
}

func TestSourceFormatRefusesNewerVersion(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming)
	opts := options(t, root)
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncState(t.Context(), summary.SourceID, `{"signal_export_format":2}`))
	_, err = st.DB().ExecContext(t.Context(), "UPDATE messages SET source_message_id = ?", "newer-format-key")
	require.NoError(t, err)
	summary, err = Import(t.Context(), st, opts)
	require.ErrorIs(t, err, ErrNewerFormat)
	assert.Zero(t, summary.SourceID)
	assert.Equal(t, 1, messageCount(t, st))
}
func TestImportRepairRemovedAlbumSlot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, b := []byte("fictional removed image"), []byte("fictional retained image")
	frame := func(attachments string) string {
		return fmt.Sprintf(`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"album"},"attachments":[%s]}}}`, attachments)
	}
	st := testutil.NewTestStore(t)
	root := fixture(t, frame(fmt.Sprintf(`{"clientUuid":"AQ==","pointer":%s},{"clientUuid":"Ag==","pointer":%s}`, pointer(a), pointer(b))))
	opts := options(t, root)
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	putMedia(t, root, b)
	writeFrames(t, root, frame(fmt.Sprintf(`{"clientUuid":"Ag==","pointer":%s}`, pointer(b))))
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, summary.AttachmentsStored)
	assert.EqualValues(1, summary.MediaMissing["invalid_hash"])
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 2)
	hash := sha256.Sum256(b)
	for _, ref := range refs {
		switch ref.SourcePartKey {
		case "attachment:0":
			assert.Empty(ref.ContentHash)
		case "attachment:1":
			assert.Equal(hex.EncodeToString(hash[:]), ref.ContentHash)
		default:
			assert.Fail("unexpected media slot", ref.SourcePartKey)
		}
	}
	retained, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(raw, retained)
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(err)
	assert.Equal("album", body)
}

func TestImportLongTextTurnsInline(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := fixture(t, strings.Replace(mediaMessage([]byte("short and the rest"), true), `"inline"`, `"short"`, 1))
	opts := options(t, root)
	_, err := Import(t.Context(), st, opts)
	require.NoError(err)
	writeFrames(t, root, strings.Replace(incoming, "Hello 星", "short and the rest", 1))
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MediaMissing["invalid_hash"])
	assert.EqualValues(1, summary.Omitted["incomplete_long_text"])
	assert.EqualValues(1, summary.MessagesChanged)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(err)
	assert.Equal("short", body)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 1)
	assert.Equal("long_text", refs[0].SourcePartKey)
	assert.Empty(refs[0].ContentHash)
}

func TestPhoneOnlyGroupAuthorAndUnsupportedRecipient(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t,
		`{"recipient":{"id":"3","contact":{"e164":"15555550100"}}}`,
		`{"recipient":{"id":"4","contact":{"pni":"AQEBAQEBAQEBAQEBAQEBAQ=="}}}`,
		`{"recipient":{"id":"5","group":{"masterKey":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="}}}`,
		`{"chat":{"id":"8","recipientId":"5"}}`,
		`{"chatItem":{"chatId":"8","authorId":"3","dateSent":"1700000000001","incoming":{},"standardMessage":{}}}`,
		`{"chatItem":{"chatId":"8","authorId":"4","dateSent":"1700000000002","incoming":{},"standardMessage":{}}}`,
		`{bad`)
	summary, err := Import(t.Context(), st, options(t, root))
	require.Error(t, err)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	assert.Equal(t, map[string]int64{"author_identity": 1}, summary.Skipped)
	assert.NotContains(t, summary.Omitted, "unsupported_recipient")
}

func TestRepairLongTextWithoutOriginalHash(t *testing.T) {
	st := testutil.NewTestStore(t)
	first := `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"inline"},"longText":{"locatorInfo":{"key":"AQ=="}}}}}`
	root := fixture(t, first)
	opts := options(t, root)
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	data := []byte("long fictional text")
	putMedia(t, root, data)
	p := strings.Replace(pointer(data), `"key":"AQ=="`, `"key":"AQ=="`, 1)
	writeFrames(t, root, `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"inline"},"longText":`+p+`}}}`)
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.AttachmentsStored)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	hash := sha256.Sum256(data)
	assert.Equal(t, "long_text", refs[0].SourcePartKey)
	assert.Equal(t, hex.EncodeToString(hash[:]), refs[0].ContentHash)
	again, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, raw, again)
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "inline", body)
}

func TestMediaIndexCountsUnrelatedEntries(t *testing.T) {
	root := fixture(t)
	data := []byte("fictional index")
	path := putMedia(t, root, data)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "unrelated"), []byte("x"), 0600))
	r, err := openReader(t.Context(), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	r.maxMedia = 1
	hash := sha256.Sum256(data)
	_, reason, err := r.mediaName(t.Context(), hex.EncodeToString(hash[:]))
	require.NoError(t, err)
	assert.Equal(t, "index_limit", reason)
	_, reason, err = r.mediaName(t.Context(), hex.EncodeToString(hash[:]))
	require.NoError(t, err)
	assert.Equal(t, "index_limit", reason)
}

func TestMediaIndexLimitContinuesFrames(t *testing.T) {
	data := []byte("fictional index media")
	frame := mediaMessage(data, false)
	root := fixture(t, frame, strings.Replace(frame, "1700000000000", "1700000000001", 1))
	path := putMedia(t, root, data)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "unrelated"), []byte("x"), 0600))
	r, err := openReader(t.Context(), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	r.maxMedia = 1
	st := testutil.NewTestStore(t)
	opts := options(t, root)
	summary := &ImportSummary{MediaMissing: map[string]int64{}, Omitted: map[string]int64{}}
	visited := 0
	err = r.frames(t.Context(), func(m *chatItem, _ []byte, _ int) error {
		visited++
		_, _, _, err := mediaWrites(t.Context(), st, r, 0, m.Standard, opts, summary)
		return err
	})
	require.NoError(t, err)
	assert.Equal(t, 2, visited)
	assert.EqualValues(t, 4, summary.MediaMissing["index_limit"])
}

func TestMatchingPointerRequiresUniqueUnclaimedIdentity(t *testing.T) {
	old := messageAttachment{ClientUUID: []byte{1}, Pointer: &filePointer{}}
	old.Pointer.Locator.Key = []byte{2}
	one := messageAttachment{ClientUUID: []byte{1}, Pointer: &filePointer{}}
	two := messageAttachment{ClientUUID: []byte{1}, Pointer: &filePointer{}}
	two.Pointer.Locator.Key = []byte{2}
	m := &standardMessage{Attachments: []messageAttachment{one, two}}
	claimed := map[int]bool{}
	assert.Same(t, two.Pointer, matchingPointer(old, []messageAttachment{old}, m, claimed))
	assert.Nil(t, matchingPointer(old, []messageAttachment{old}, m, claimed))
	assert.Nil(t, matchingPointer(old, []messageAttachment{old, old}, m, map[int]bool{}))
	old.Pointer.Locator.Key = []byte{3}
	assert.Nil(t, matchingPointer(old, []messageAttachment{old}, m, map[int]bool{}))
}

func TestContactDoesNotReassignExistingAlias(t *testing.T) {
	st := testutil.NewTestStore(t)
	scope := accountScope("fictional")
	aci := scopedKey(scope, "aci:01010101010101010101010101010101")
	phone := scopedKey(scope, "e164:+15555550100")
	first, err := st.EnsureParticipantByIdentifierContext(t.Context(), SourceType, aci, "")
	require.NoError(t, err)
	second, err := st.EnsureParticipantByIdentifierContext(t.Context(), SourceType, phone, "")
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	root := fixture(t, incoming)
	lines := []string{header, account, self, `{"recipient":{"id":"2","contact":{"e164":"15555550100","aci":"AQEBAQEBAQEBAQEBAQEBAQ=="}}}`, direct, incoming}
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
	_, err = Import(t.Context(), st, options(t, root))
	require.ErrorContains(t, err, "Signal contact identifiers belong to different participants")
	assert.Zero(t, messageCount(t, st))
	aciOwner, _, err := st.ParticipantByIdentifierContext(t.Context(), SourceType, aci)
	require.NoError(t, err)
	phoneOwner, _, err := st.ParticipantByIdentifierContext(t.Context(), SourceType, phone)
	require.NoError(t, err)
	assert.Equal(t, first, aciOwner)
	assert.Equal(t, second, phoneOwner)
}
