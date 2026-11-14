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
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming)
	opts := options(t, root)
	first, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, first.MessagesAdded)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
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
	assertBody(t, st, id, "Hello 星")
	again, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, again)
	opts.Account = "another-fictional-account"
	third, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.NotEqual(first.SourceID, third.SourceID)
	assert.Equal(2, messageCount(t, st))
	// A first visit to an edited item uses the original time and current text.
	var sent time.Time
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT id, sent_at FROM messages WHERE source_id = ?"), third.SourceID).Scan(&id, &sent))
	assert.Equal(time.UnixMilli(1700000000000).UTC(), sent.UTC())
	assertBody(t, st, id, "edited")
}
func TestImportDirectionsGroupsAndOmissions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	// Duplicate definitions with extra fields agree with the first ones.
	root := fixture(t, incoming, strings.Replace(contact, `"profileGivenName":"Example"`, `"profileGivenName":"Example","blocked":true`, 1), `{"chat":{"id":"7","recipientId":"2","archived":true}}`,
		`{"recipient":{"id":"3","group":{"masterKey":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=","snapshot":{"title":{"title":"Example group"}}}}}`,
		`{"chat":{"id":"8","recipientId":"3"}}`,
		`{"chat":{"id":"9","recipientId":"1"}}`,
		`{"chatItem":{"chatId":"8","authorId":"1","dateSent":"1700000000001","outgoing":{},"pinDetails":{"pinnedAtTimestamp":"1700000000500","pinNeverExpires":true},"standardMessage":{"text":{"body":"Group"},"quote":{},"reactions":[{}],"linkPreview":[{}],"attachments":[{"flag":"VOICE_MESSAGE"}]}}}`,
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
	assert.EqualValues(1, summary.Omitted["link_preview"])
	assert.EqualValues(1, summary.Omitted["attachment_flag"])
	assert.EqualValues(1, summary.UnknownFrames)
	assert.EqualValues(1, summary.EmptyFrames)
	var fromMe, owners int
	var owner string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE is_from_me = true").Scan(&fromMe))
	assert.Equal(2, fromMe)
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*), min(address) FROM account_identities").Scan(&owners, &owner))
	assert.Equal(1, owners)
	assert.Equal("v1:a524644266885e16a3bb16166ae38bdbfb36af08fc51036f4dbb91423c7cbcc1:self", owner)
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
func hashHex(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// onlyMessage returns the single archived message and its first raw record.
func onlyMessage(t *testing.T, st *store.Store) (int64, []byte) {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	return id, raw
}
func slotHashes(t *testing.T, st *store.Store, id int64) map[string]string {
	t.Helper()
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	slots := map[string]string{}
	for _, ref := range refs {
		slots[ref.SourcePartKey] = ref.ContentHash
	}
	return slots
}
func assertBody(t *testing.T, st *store.Store, id int64, want string) {
	t.Helper()
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, want, body)
}
func TestImportUnsafeMedia(t *testing.T) {
	for _, tc := range []struct {
		kind, reason string
		long         bool
	}{
		{"wrong-hash", "hash_mismatch", false}, {"final-symlink", "unsafe_or_nonregular", false},
		{"directory-symlink", "missing_or_unsafe", false}, {"directory", "unsafe_or_nonregular", false},
		{"ambiguous", "ambiguous", false}, {"extensionless-positive", "", false},
		{"extensionless-positive", "", true},
		{"too-large", "oversized", false},
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
			if tc.long && tc.reason != "" {
				assert.EqualValues(1, summary.Omitted["incomplete_long_text"])
				id, _ := onlyMessage(t, st)
				assertBody(t, st, id, "inline")
			}
			if kind == "wrong-hash" {
				id, _ := onlyMessage(t, st)
				slots := slotHashes(t, st, id)
				require.Len(slots, want)
				for _, hash := range slots {
					assert.Empty(hash)
				}
				hash := hashHex([]byte("wrong"))
				assert.NoFileExists(filepath.Join(opts.AttachmentsDir, hash[:2], hash))
			}
			if kind == "extensionless-positive" {
				assert.Equal(want, summary.AttachmentsStored)
				id, _ := onlyMessage(t, st)
				if tc.long {
					assertBody(t, st, id, string(data))
				} else {
					refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
					require.NoError(err)
					require.Len(refs, 2)
					assert.Equal("display.txt", refs[0].Filename)
					assert.Equal("display.txt", refs[1].Filename)
					assert.Equal(refs[0].StoragePath, refs[1].StoragePath)
					stored, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, refs[0].StoragePath))
					require.NoError(err)
					assert.Equal(data, stored)
				}
			} else {
				assert.Equal(want, summary.AttachmentsMissing)
			}
		})
	}
}
func TestReaderValidation(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"header", `{"version":"2"}`, "unsupported version"},
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
			r, err := openReader(t.Context(), root)
			if err == nil {
				defer func() { require.NoError(t, r.Close()) }()
				err = r.frames(t.Context(), func(*chatItem, []byte, int) error { return nil })
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
	for _, name := range []string{"metadata.json", "main.jsonl"} {
		t.Run(name+"/directory", func(t *testing.T) {
			root := fixture(t)
			path := filepath.Join(root, name)
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Mkdir(path, 0700))
			_, err := openReader(t.Context(), root)
			require.ErrorContains(t, err, name)
			require.ErrorContains(t, err, "regular file")
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
func TestProtoScalars(t *testing.T) {
	var item chatItem
	require.NoError(t, json.Unmarshal([]byte(`{"chatId":"007"}`), &item))
	assert.Equal(t, "7", string(item.ChatID))
	require.Error(t, json.Unmarshal([]byte(`{"chatId":7}`), &item))
	require.Error(t, json.Unmarshal([]byte(`{"chatId":"invalid"}`), &item))
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
	for _, revision := range []string{`"authorId":"1","dateSent":"1699999999999"`, `"authorId":"2","dateSent":"1700000000001"`, `"authorId":"2","dateSent":"0"`} {
		cases = append(cases, struct{ frame, reason string }{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"revisions":[{"chatId":"7",` + revision + `}],"standardMessage":{}}}`, "revision"})
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
func TestNativeFixtureProvenance(t *testing.T) {
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
			groupMessage := `{"chatItem":{"chatId":"8","authorId":"2","dateSent":"1700000000001","incoming":{},"standardMessage":{"text":{"body":"group"}}}}`
			lines := []string{header, account, self, `{"recipient":{"id":"2","contact":` + contact + `}}`, direct, group, groupChat, incoming, groupMessage}
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
			opts := options(t, root)
			_, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			lines[3] = `{"recipient":{"id":"2","contact":{"e164":"15555550100","aci":"AQEBAQEBAQEBAQEBAQEBAQ=="}}}`
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, 2, summary.MessagesExisting)
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
	source, err := st.GetOrCreateSourceContext(t.Context(), SourceType, opts.Account)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncState(t.Context(), source.ID, `{"signal_export_format":2}`))
	summary, err := Import(t.Context(), st, opts)
	require.ErrorIs(t, err, ErrNewerFormat)
	assert.Zero(t, summary.SourceID)
	assert.Zero(t, messageCount(t, st))
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
	id, _ := onlyMessage(t, st)
	assertBody(t, st, id, "short")
	assert.Equal(map[string]string{"long_text": ""}, slotHashes(t, st, id))
}

// A missing slot is filled only by a unique UUID or locator key from its first
// record; position and a later export's hash never assign bytes.
func TestRepairFillsOriginalSlotByIdentity(t *testing.T) {
	a, b := []byte("original fictional media"), []byte("replacement fictional media")
	message := func(body, standard string) string {
		return `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"` + body + `"},` + standard + `}}}`
	}
	attachments := func(uuid string, pointer string) string {
		return `"attachments":[{"clientUuid":"` + uuid + `","pointer":` + pointer + `}]`
	}
	otherKey := strings.Replace(pointer(a), `"key":"AQ=="`, `"key":"Ag=="`, 1)
	for _, tc := range []struct {
		name, first, unmatched, repaired string
		unmatchedMedia, repairedMedia    []byte
		want                             map[string]string
	}{
		{"hashed original ignores new hash", attachments("AQ==", pointer(a)), attachments("AQ==", pointer(b)), attachments("AQ==", pointer(b)), b, a, map[string]string{"attachment:0": hashHex(a)}},
		{"hashed long text ignores new hash", `"longText":` + pointer(a), `"longText":` + pointer(b), `"longText":` + pointer(b), b, a, map[string]string{"long_text": hashHex(a)}},
		{"hashless uuid after reorder", attachments("AQ==", `{"locatorInfo":{"key":"Ag=="}}`), attachments("BA==", pointer(a)), `"attachments":[{"clientUuid":"Aw==","pointer":{}},{"clientUuid":"AQ==","pointer":` + pointer(a) + `}]`, a, a, map[string]string{"attachment:0": hashHex(a)}},
		{"hashless long text by key", `"longText":{"locatorInfo":{"key":"AQ=="}}`, `"longText":` + otherKey, `"longText":` + pointer(a), a, a, map[string]string{"long_text": hashHex(a)}},
		{"earlier album slot removed", `"attachments":[{"clientUuid":"AQ==","pointer":` + pointer(a) + `},{"clientUuid":"Ag==","pointer":` + pointer(b) + `}]`, attachments("Aw==", pointer(b)), attachments("Ag==", pointer(b)), b, b, map[string]string{"attachment:0": "", "attachment:1": hashHex(b)}},
		{"equal bytes in separate slots", `"attachments":[{"clientUuid":"AQ==","pointer":` + pointer(a) + `},{"clientUuid":"Ag==","pointer":` + pointer(a) + `}]`, attachments("Aw==", pointer(b)), `"attachments":[{"clientUuid":"AQ==","pointer":` + pointer(a) + `},{"clientUuid":"Ag==","pointer":` + pointer(a) + `}]`, b, a, map[string]string{"attachment:0": hashHex(a), "attachment:1": hashHex(a)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := fixture(t, message("original", tc.first))
			opts := options(t, root)
			_, err := Import(t.Context(), st, opts)
			require.NoError(err)
			id, raw := onlyMessage(t, st)
			writeFrames(t, root, message("changed", tc.unmatched))
			putMedia(t, root, tc.unmatchedMedia)
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Zero(summary.AttachmentsStored)
			assert.Equal(len(tc.want), summary.AttachmentsMissing)
			for key := range tc.want {
				assert.Empty(slotHashes(t, st, id)[key])
			}
			hash := hashHex(tc.unmatchedMedia)
			assert.NoFileExists(filepath.Join(opts.AttachmentsDir, hash[:2], hash))
			writeFrames(t, root, message("changed", tc.repaired))
			putMedia(t, root, tc.repairedMedia)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			stored := 0
			for _, hash := range tc.want {
				if hash != "" {
					stored++
				}
			}
			assert.Equal(stored, summary.AttachmentsStored)
			assert.Equal(len(tc.want)-stored, summary.AttachmentsMissing)
			assert.Equal(tc.want, slotHashes(t, st, id))
			again, err := st.GetMessageRawContext(t.Context(), id)
			require.NoError(err)
			assert.Equal(raw, again)
			assertBody(t, st, id, "original")
		})
	}
}

// The limit counts directory entries and later frame callbacks report missing media.
func TestMediaIndexLimit(t *testing.T) {
	data := []byte("fictional index media")
	other := []byte("fictional other index media")
	require.NotEqual(t, hashHex(data)[:2], hashHex(other)[:2])
	frame := mediaMessage(data, false)
	second := strings.Replace(mediaMessage(other, false), "1700000000000", "1700000000001", 1)
	root := fixture(t, frame, second)
	path := putMedia(t, root, data)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "unrelated"), []byte("x"), 0600))
	r, err := openReader(t.Context(), root)
	require.NoError(t, err)
	r.maxMedia = 2
	_, reason, err := r.mediaName(t.Context(), hashHex(data))
	require.NoError(t, err)
	assert.Empty(t, reason)
	assert.Equal(t, 2, r.mediaNames)
	require.NoError(t, r.Close())

	r, err = openReader(t.Context(), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	r.maxMedia = 1
	st, opts := testutil.NewTestStore(t), options(t, root)
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
	root := fixture(t)
	lines := []string{header, account, self, `{"recipient":{"id":"2","contact":{"e164":"15555550100","aci":"AQEBAQEBAQEBAQEBAQEBAQ=="}}}`, direct, incoming}
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
	_, err = Import(t.Context(), st, options(t, root))
	require.ErrorContains(t, err, "signal contact identifiers belong to different participants")
	assert.Zero(t, messageCount(t, st))
	aciOwner, _, err := st.ParticipantByIdentifierContext(t.Context(), SourceType, aci)
	require.NoError(t, err)
	phoneOwner, _, err := st.ParticipantByIdentifierContext(t.Context(), SourceType, phone)
	require.NoError(t, err)
	assert.Equal(t, first, aciOwner)
	assert.Equal(t, second, phoneOwner)
}
