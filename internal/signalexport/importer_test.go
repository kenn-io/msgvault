package signalexport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
		`{"chatItem":{"chatId":"8","authorId":"1","dateSent":"1700000000001","outgoing":{},"standardMessage":{"text":{"body":"Group"},"quote":{},"reactions":[{}],"linkPreview":[{}]}}}`,
		`{"chatItem":{"chatId":"9","authorId":"1","dateSent":"1700000000002","outgoing":{},"standardMessage":{"text":{"body":"Note"}}}}`,
		`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000003","outgoing":{},"standardMessage":{}}}`,
		`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000004","incoming":{},"remoteDeletedMessage":{}}}`,
		`{"chatItem":{"chatId":"7","authorId":"2","incoming":{},"standardMessage":{}}}`,
		`{"futureFrame":{}}`, `{}`)
	summary, err := Import(t.Context(), st, options(t, root))
	require.NoError(t, err)
	assert.EqualValues(t, 3, summary.MessagesAdded)
	assert.EqualValues(t, 1, summary.Skipped["direction_conflict"])
	assert.EqualValues(t, 1, summary.Skipped["remoteDeletedMessage"])
	assert.EqualValues(t, 1, summary.Skipped["sent_time"])
	assert.EqualValues(t, 1, summary.Omitted["quote"])
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
func TestImportLateFailureAndReplay(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming, `{"chatItem":`)
	opts := options(t, root)
	summary, err := Import(t.Context(), st, opts)
	require.ErrorContains(t, err, "line 7")
	assert.EqualValues(t, 1, summary.MessagesAdded)
	assert.Equal(t, 1, messageCount(t, st))
	writeFrames(t, root, incoming)
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MessagesExisting)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Import(ctx, st, opts)
	require.ErrorIs(t, err, context.Canceled)
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
			_, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
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
	for _, kind := range []string{"wrong-hash", "final-symlink", "directory-symlink", "directory", "ambiguous", "extensionless", "encrypted-name", "too-large"} {
		t.Run(kind, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			data := []byte("fictional media")
			root := fixture(t, mediaMessage(data, false))
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
				require.NoError(t, os.Symlink(t.TempDir(), filepath.Dir(path)))
			case "directory":
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0700))
			case "ambiguous":
				require.NoError(t, os.WriteFile(strings.TrimSuffix(path, ".txt")+".bin", data, 0600))
			case "extensionless":
				require.NoError(t, os.Rename(path, strings.TrimSuffix(path, ".txt")))
			case "encrypted-name":
				require.NoError(t, os.Rename(path, filepath.Join(filepath.Dir(path), strings.Repeat("a", 64))))
			case "too-large":
				file, err := os.OpenFile(path, os.O_WRONLY, 0600)
				require.NoError(t, err)
				require.NoError(t, file.Truncate(256<<20+1))
				require.NoError(t, file.Close())
			}
			summary, err := Import(t.Context(), st, options(t, root))
			require.NoError(t, err)
			if kind == "extensionless" {
				assert.Equal(t, 2, summary.AttachmentsStored)
			} else {
				assert.Equal(t, 2, summary.AttachmentsMissing)
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

// The logger is only a cancellation trigger: persistence and replay both use
// the real parser, Store transaction and sync-run lifecycle.
type cancelWriter struct{ cancel context.CancelFunc }

func (w cancelWriter) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }
func TestImportCancellationRetainsCommittedSnapshot(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming, strings.Replace(incoming, "1700000000000", "1700000000001", 1))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := options(t, root)
	opts.Logger = slog.New(slog.NewTextHandler(cancelWriter{cancel}, nil))
	s, err := Import(ctx, st, opts)
	require.ErrorIs(t, err, context.Canceled)
	assert.EqualValues(t, 1, s.MessagesAdded)
	opts.Logger = nil
	s, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, s.MessagesAdded)
	assert.EqualValues(t, 1, s.MessagesExisting)
	assert.Equal(t, 2, messageCount(t, st))
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
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE is_from_me = false").Scan(&id))
	var metadata, rawFormat string
	var sent time.Time
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT sent_at FROM messages WHERE id = ?"), id).Scan(&sent))
	assert.Equal(t, time.UnixMilli(1700000000000).UTC(), sent.UTC())
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT metadata FROM messages WHERE id = ?"), id).Scan(&metadata))
	assert.Contains(t, metadata, `"adapter_version":1`)
	assert.Contains(t, metadata, `"backupTimeMs":"1730000000000"`)
	assert.NotContains(t, metadata, "masterKey")
	assert.NotContains(t, metadata, "localKey")
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT raw_format FROM message_raw WHERE message_id = ?"), id).Scan(&rawFormat))
	assert.Equal(t, "signal-export-jsonl", rawFormat)
	// Omitted records and tombstones do not remove existing messages.
	root := fixture(t, `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"remoteDeletedMessage":{}}}`)
	opts.RootDir = root
	s, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, s.Skipped["remoteDeletedMessage"])
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

func TestMediaLongTextLimitAndChangedLocalKey(t *testing.T) {
	st := testutil.NewTestStore(t)
	data := []byte("Fictional key-independent text")
	root := fixture(t, mediaMessage(data, true))
	opts := options(t, root)
	file := putMedia(t, root, data)
	s, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.AttachmentsStored)
	writeFrames(t, root, strings.ReplaceAll(mediaMessage(data, true), `"localKey":"AQ=="`, `"localKey":"Ag=="`))
	s, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, s.MessagesExisting)
	assert.Zero(t, s.MessagesChanged)
	assert.Zero(t, s.AttachmentsMissing)
	st2 := testutil.NewTestStore(t)
	f, err := os.OpenFile(file, os.O_WRONLY, 0600)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(16<<20+1))
	require.NoError(t, f.Close())
	s, err = Import(t.Context(), st2, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, s.Omitted["incomplete_long_text"])
	assert.EqualValues(t, 1, s.MediaMissing["oversized"])
}

func TestReaderSemanticDuplicateDefinitions(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, strings.Replace(contact, `"profileGivenName":"Example"`, `"profileGivenName":"Example","blocked":true`, 1), `{"chat":{"id":"7","recipientId":"2","archived":true}}`, incoming)
	s, err := Import(t.Context(), st, options(t, root))
	require.NoError(t, err)
	assert.EqualValues(t, 1, s.MessagesAdded)
}
