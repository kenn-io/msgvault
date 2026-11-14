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
	writeLines(t, root, append([]string{header, account, self, contact, direct}, frames...))
}
func writeLines(t *testing.T, root string, lines []string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0600))
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
	original := strings.Replace(incoming, `"incoming":{}`, `"incoming":{"dateReceived":"1700000000100"}`, 1)
	root := fixture(t, original)
	opts := options(t, root)
	first, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, first.MessagesAdded)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.JSONEq(strings.TrimSuffix(strings.TrimPrefix(original, `{"chatItem":`), `}`), string(raw))
	// Every local ID changes, and an edit changes the top-level time.
	edited := `{"chatItem":{"chatId":"70","authorId":"20","dateSent":"1700000005000","incoming":{"dateReceived":"1700000005100"},"revisions":[{"chatId":"70","authorId":"20","dateSent":"1700000000000","incoming":{"dateReceived":"1700000000100"},"standardMessage":{"text":{"body":"Hello 星"}}}],"standardMessage":{"text":{"body":"edited"}}}}`
	lines := []string{header, account, strings.Replace(self, `"1"`, `"10"`, 1), strings.Replace(contact, `"2"`, `"20"`, 1), `{"chat":{"id":"70","recipientId":"20"}}`, edited}
	writeLines(t, root, lines)
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

func TestImportDistinctMessagesAtSameMillisecond(t *testing.T) {
	first := strings.Replace(incoming, `"incoming":{}`, `"incoming":{"dateReceived":"1700000000100"}`, 1)
	outgoing := `{"chatItem":{"chatId":"7","authorId":"1","dateSent":"1700000000000","outgoing":{"dateReceived":"1700000000100"},"standardMessage":{"text":{"body":"First message"}}}}`
	later := func(frame, body string) string {
		return strings.Replace(strings.Replace(frame, body, "Another message", 1), "1700000000100", "1700000000200", 1)
	}
	for _, tc := range []struct {
		name           string
		frames, replay []string
	}{
		{"incoming receive times", []string{first, later(first, "Hello 星")}, []string{later(first, "Hello 星")}},
		{"outgoing receive times", []string{outgoing, later(outgoing, "First message")}, []string{later(outgoing, "First message")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := fixture(t, tc.frames...)
			opts := options(t, root)
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, len(tc.frames), summary.MessagesAdded)
			writeFrames(t, root, tc.replay...)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.Zero(t, summary.MessagesAdded)
			assert.EqualValues(t, 1, summary.MessagesExisting)
			assert.Equal(t, 2, messageCount(t, st))
		})
	}
}

func TestImportDirectionsGroupsAndOmissions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming,
		`{"recipient":{"id":"3","group":{"masterKey":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=","snapshot":{"title":{"title":"Example group"}}}}}`,
		`{"chat":{"id":"8","recipientId":"3"}}`,
		`{"chat":{"id":"9","recipientId":"1"}}`,
		`{"chatItem":{"chatId":"8","authorId":"1","dateSent":"1700000000001","outgoing":{},"revisions":[{"chatId":"8","authorId":"1","dateSent":"1700000000000","outgoing":{},"standardMessage":{"text":{"body":"Original group"}}}],"pinDetails":{"pinnedAtTimestamp":"1700000000500","pinNeverExpires":true},"standardMessage":{"text":{"body":"Group","bodyRanges":[{"start":0,"length":5,"style":"BOLD"}]},"quote":{"targetSentTimestamp":"1700000000000","authorId":"2","text":{"body":"Original"}},"reactions":[{"emoji":"👍","authorId":"2","sentTimestamp":"1700000000001"}],"linkPreview":[{"url":"https://example.com","title":"Example"}],"attachments":[{"flag":"VOICE_MESSAGE"}]}}}`,
		`{"chatItem":{"chatId":"9","authorId":"1","dateSent":"1700000000002","outgoing":{},"standardMessage":{"text":{"body":"Note"}}}}`,
		`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000004","incoming":{},"remoteDeletedMessage":{}}}`,
		// Unknown and empty frames exercise frame classification.
		`{"futureFrame":{}}`, `{}`)
	f, err := os.OpenFile(filepath.Join(root, "main.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(err)
	for _, kind := range []string{"contactMessage", "stickerMessage", "updateMessage", "paymentNotification", "giftBadge", "viewOnceMessage", "directStoryReplyMessage", "adminDeletedMessage"} {
		_, err = fmt.Fprintf(f, "{\"chatItem\":{\"chatId\":\"7\",\"authorId\":\"2\",\"dateSent\":\"1700000000000\",\"incoming\":{},\"%s\":{}}}\n", kind)
		require.NoError(err)
	}
	for _, kind := range []string{"adHocCall", "installedStickerPack", "hiddenStickerPack", "favoriteStickerList", "notificationProfile", "chatFolder"} {
		_, err = fmt.Fprintf(f, "{\"%s\":{}}\n", kind)
		require.NoError(err)
	}
	require.NoError(f.Close())
	summary, err := Import(t.Context(), st, options(t, root))
	require.NoError(err)
	assert.EqualValues(3, summary.MessagesAdded)
	for _, kind := range []string{"contactMessage", "stickerMessage", "remoteDeletedMessage", "updateMessage", "paymentNotification", "giftBadge", "viewOnceMessage", "directStoryReplyMessage", "adminDeletedMessage"} {
		assert.EqualValues(1, summary.Skipped[kind], kind)
	}
	for _, kind := range []string{"adHocCall", "installedStickerPack", "hiddenStickerPack", "favoriteStickerList", "notificationProfile", "chatFolder"} {
		assert.EqualValues(1, summary.Omitted[kind], kind)
	}
	assert.EqualValues(1, summary.Omitted["quote"])
	assert.EqualValues(1, summary.Omitted["pin"])
	assert.EqualValues(1, summary.Omitted["formatting"])
	assert.EqualValues(1, summary.Omitted["revision_history"])
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
	assert.Equal(scopedKey(accountScope("fictional"), "self"), owner)
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
	return hex.EncodeToString(sha256Sum(data))
}
func sha256Sum(data []byte) []byte {
	hash := sha256.Sum256(data)
	return hash[:]
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
func TestImportMediaResolution(t *testing.T) {
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
				if !tc.long {
					hash := hashHex([]byte("wrong"))
					assert.NoFileExists(filepath.Join(opts.AttachmentsDir, hash[:2], hash))
				}
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
		{"agreeing duplicates", strings.Join([]string{header, account, self, contact, direct, strings.Replace(contact, `"profileGivenName":"Example"`, `"profileGivenName":"Example","blocked":true`, 1), `{"chat":{"id":"7","recipientId":"2","archived":true}}`}, "\n"), ""},
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
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

var nonregularInputs = []struct {
	name  string
	check func(*testing.T, string, string)
}{
	{"directory", func(t *testing.T, root, name string) {
		t.Helper()
		path := filepath.Join(root, name)
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Mkdir(path, 0700))
		_, err := openReader(t.Context(), root)
		require.ErrorContains(t, err, name)
		require.ErrorContains(t, err, "regular file")
	}},
}

func TestReaderRejectsNonregularInput(t *testing.T) {
	for _, name := range []string{"metadata.json", "main.jsonl"} {
		for _, tc := range nonregularInputs {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				tc.check(t, fixture(t), name)
			})
		}
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
	require.NoError(t, json.Unmarshal([]byte(`{"incoming":{"dateReceived":null}}`), &item))
	require.NotNil(t, item.Incoming)
	assert.Nil(t, item.Incoming.DateReceived)
}

func TestImportIdentityTimeAndDirectionSkips(t *testing.T) {
	// Standard text messages isolate identity, time, and direction checks.
	cases := []struct{ frame, reason string }{
		{`{"chatItem":{"chatId":"7","authorId":"2","incoming":{},"standardMessage":{"text":{"body":"Example"}}}}`, "sent_time"},
		{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000003","outgoing":{},"standardMessage":{"text":{"body":"Example"}}}}`, "direction_conflict"},
		{`{"chatItem":{"chatId":"99","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"Example"}}}}`, "unresolved_chat"},
		{`{"chatItem":{"chatId":"7","authorId":"99","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"Example"}}}}`, "author_identity"},
		{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"0","incoming":{},"standardMessage":{"text":{"body":"Example"}}}}`, "sent_time"},
		{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"18446744073709551615","incoming":{},"standardMessage":{"text":{"body":"Example"}}}}`, "sent_time"},
		{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","directionless":{},"standardMessage":{"text":{"body":"Example"}}}}`, "unknown_direction"},
	}
	for _, revision := range []string{`"authorId":"1","dateSent":"1699999999999"`, `"authorId":"2","dateSent":"1700000000001"`, `"authorId":"2","dateSent":"0"`} {
		cases = append(cases, struct{ frame, reason string }{`{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"revisions":[{"chatId":"7",` + revision + `}],"standardMessage":{"text":{"body":"Example"}}}}`, "revision"})
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
	for _, recipient := range []string{`{"contact":{"e164":"15555550100"}}`, `{"contact":{"pni":"AQEBAQEBAQEBAQEBAQEBAQ=="}}`, `{"contact":{"aci":"AQ=="}}`, `{"group":{"masterKey":"AQ=="}}`, `{"contact":{"profileGivenName":"Fictional name only"}}`} {
		t.Run(recipient, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := fixture(t)
			lines := []string{header, account, self, `{"recipient":{"id":"2",` + strings.TrimPrefix(recipient, "{") + `}`, direct, incoming}
			writeLines(t, root, lines)
			s, err := Import(t.Context(), st, options(t, root))
			require.NoError(t, err)
			assert.Equal(t, map[string]int64{"recipient_identity": 1}, s.Skipped)
			assert.Empty(t, s.Omitted)
		})
	}
}

// Not parallel: CancelAfterCommit replaces process-wide SQL logging.
func TestImportCancellationRetainsCommittedSnapshot(t *testing.T) {
	for _, kind := range []string{"parse failure at line 7", "cancel after first commit"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			second := strings.Replace(incoming, "1700000000000", "1700000000001", 1)
			if kind == "parse failure at line 7" {
				second = `{"chatItem":`
			}
			first := strings.Replace(incoming, `"standardMessage":{`, `"standardMessage":{"quote":{},`, 1)
			root := fixture(t, first, second)
			opts := options(t, root)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
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
			assert.EqualValues(1, summary.MessagesAdded)
			assert.EqualValues(1, summary.Omitted["quote"])
			source, err := st.GetSourceByIDContext(t.Context(), summary.SourceID)
			require.NoError(err)
			assert.JSONEq(fmt.Sprintf(`{"signal_export_format":%d}`, formatVersion), source.SyncCursor.String)
			assert.Equal(1, messageCount(t, st))
			writeFrames(t, root, first, strings.Replace(incoming, "1700000000000", "1700000000001", 1))
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

func TestSourceFormatRefusesNewerVersion(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming)
	opts := options(t, root)
	source, err := st.GetOrCreateSourceContext(t.Context(), SourceType, opts.Account)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncState(t.Context(), source.ID, fmt.Sprintf(`{"signal_export_format":%d}`, formatVersion+1)))
	summary, err := Import(t.Context(), st, opts)
	require.ErrorIs(t, err, ErrNewerFormat)
	assert.Zero(t, summary.SourceID)
	assert.Zero(t, messageCount(t, st))
}

// A missing slot is filled only by a unique UUID or locator key from its first
// record; position and a later export's hash never assign bytes.
func TestRepairFillsOriginalSlotByIdentity(t *testing.T) {
	a, b := []byte("original fictional media"), []byte("replacement fictional media")
	message := func(body, standard string) string {
		if standard != "" {
			standard = "," + standard
		}
		return `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"` + body + `"}` + standard + `}}}`
	}
	attachments := func(uuid string, pointer string) string {
		return `"attachments":[{"clientUuid":"` + uuid + `","pointer":` + pointer + `}]`
	}
	otherKey := strings.Replace(pointer(a), `"key":"AQ=="`, `"key":"Ag=="`, 1)
	otherKeyB := strings.Replace(pointer(b), `"key":"AQ=="`, `"key":"Ag=="`, 1)
	for _, tc := range []struct {
		name, first, unmatched, repaired                     string
		unmatchedMedia, repairedMedia                        []byte
		want                                                 map[string]string
		firstStored, firstMissing, finalStored, finalMissing int
	}{
		{"hashed original ignores new hash", attachments("AQ==", pointer(a)), attachments("AQ==", otherKeyB), attachments("AQ==", otherKeyB), b, a, map[string]string{"attachment:0": hashHex(a)}, 0, 1, 1, 0},
		{"hashed long text ignores new hash", `"longText":` + pointer(a), `"longText":` + otherKeyB, `"longText":` + otherKeyB, b, a, map[string]string{"long_text": hashHex(a)}, 0, 1, 1, 0},
		{"hashless uuid after reorder", attachments("AQ==", `{"locatorInfo":{"key":"Ag=="}}`), attachments("BA==", pointer(a)), `"attachments":[{"clientUuid":"Aw==","pointer":{}},{"clientUuid":"AQ==","pointer":` + pointer(a) + `}]`, a, a, map[string]string{"attachment:0": hashHex(a)}, 0, 1, 1, 0},
		{"hashless long text by key", `"longText":{"locatorInfo":{"key":"AQ=="}}`, `"longText":` + otherKey, `"longText":` + pointer(a), a, a, map[string]string{"long_text": hashHex(a)}, 0, 1, 1, 0},
		{"earlier album slot removed", `"attachments":[{"clientUuid":"AQ==","pointer":{"locatorInfo":{"key":"AQ=="}}},{"clientUuid":"Ag==","pointer":{"locatorInfo":{"key":"Ag=="}}}]`, attachments("Aw==", strings.Replace(pointer(b), `"key":"AQ=="`, `"key":"Aw=="`, 1)), attachments("Ag==", otherKeyB), b, b, map[string]string{"attachment:0": "", "attachment:1": hashHex(b)}, 0, 2, 1, 1},
		{"equal bytes in separate slots", `"attachments":[{"clientUuid":"AQ==","pointer":` + pointer(a) + `},{"clientUuid":"Ag==","pointer":` + pointer(a) + `}]`, `"attachments":[{"clientUuid":"AQ==","pointer":` + pointer(a) + `},{"clientUuid":"Ag==","pointer":` + pointer(a) + `}]`, `"attachments":[{"clientUuid":"AQ==","pointer":` + pointer(a) + `},{"clientUuid":"Ag==","pointer":` + pointer(a) + `}]`, nil, a, map[string]string{"attachment:0": hashHex(a), "attachment:1": hashHex(a)}, 0, 2, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := fixture(t, message("original", tc.first))
			opts := options(t, root)
			_, err := Import(t.Context(), st, opts)
			require.NoError(err)
			id, _ := onlyMessage(t, st)
			writeFrames(t, root, message("changed", tc.unmatched))
			if tc.unmatchedMedia != nil {
				putMedia(t, root, tc.unmatchedMedia)
			}
			summary, err := Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(tc.firstStored, summary.AttachmentsStored)
			assert.Equal(tc.firstMissing, summary.AttachmentsMissing)
			writeFrames(t, root, message("changed", tc.repaired))
			putMedia(t, root, tc.repairedMedia)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(err)
			assert.Equal(tc.finalStored, summary.AttachmentsStored)
			assert.Equal(tc.finalMissing, summary.AttachmentsMissing)
			assert.Equal(tc.want, slotHashes(t, st, id))
		})
	}
}

func TestLongTextTurnedInlineRetainsSnapshot(t *testing.T) {
	data := []byte("original fictional media")
	original := `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"original"},"longText":` + pointer(data) + `}}}`
	inline := `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"original fictional media"}}}}`
	st := testutil.NewTestStore(t)
	root := fixture(t, original)
	opts := options(t, root)
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	id, raw := onlyMessage(t, st)
	writeFrames(t, root, inline)
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, summary.AttachmentsStored)
	assert.Equal(t, 1, summary.AttachmentsMissing)
	assert.EqualValues(t, 1, summary.MediaMissing["missing"])
	assert.EqualValues(t, 1, summary.Omitted["incomplete_long_text"])
	assert.EqualValues(t, 1, summary.MessagesChanged)
	assert.Equal(t, map[string]string{"long_text": ""}, slotHashes(t, st, id))
	again, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, raw, again)
	assertBody(t, st, id, "original")
}

func TestMediaIndexLimit(t *testing.T) {
	data := []byte("fictional index media")
	other := []byte("fictional other index media")
	require.NotEqual(t, hashHex(data)[:2], hashHex(other)[:2])
	root := fixture(t)
	path := putMedia(t, root, data)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "unrelated"), []byte("x"), 0600))
	r, err := openReader(t.Context(), root)
	require.NoError(t, err)
	r.maxMedia = 2
	_, reason, err := r.mediaName(t.Context(), hashHex(data))
	require.NoError(t, err)
	assert.Empty(t, reason)
	require.NoError(t, r.Close())

	r, err = openReader(t.Context(), root)
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	r.maxMedia = 1
	for _, hash := range []string{hashHex(data), hashHex(other)} {
		_, reason, err := r.mediaName(t.Context(), hash)
		require.NoError(t, err)
		assert.Equal(t, "index_limit", reason)
	}
}

func TestMatchingPointerRequiresUniqueUnclaimedIdentity(t *testing.T) {
	old := messageAttachment{ClientUUID: []byte{1}, Pointer: &filePointer{}}
	old.Pointer.Locator.Key = []byte{2}
	one := messageAttachment{ClientUUID: []byte{1}, Pointer: &filePointer{}}
	two := messageAttachment{ClientUUID: []byte{1}, Pointer: &filePointer{}}
	two.Pointer.Locator.Key = []byte{2}
	m := &standardMessage{Attachments: []messageAttachment{one, two}}
	assert.Same(t, two.Pointer, matchingPointer(old, []messageAttachment{old}, m, map[int]bool{}))
	old2 := messageAttachment{ClientUUID: []byte{3}, Pointer: &filePointer{}}
	old2.Pointer.Locator.Key = []byte{3}
	claimable := messageAttachment{ClientUUID: []byte{1}, Pointer: &filePointer{}}
	claimable.Pointer.Locator.Key = []byte{3}
	claimMessage := &standardMessage{Attachments: []messageAttachment{claimable}}
	claimed := map[int]bool{}
	assert.Same(t, claimable.Pointer, matchingPointer(old, []messageAttachment{old, old2}, claimMessage, claimed))
	assert.Nil(t, matchingPointer(old2, []messageAttachment{old, old2}, claimMessage, claimed))
	assert.Nil(t, matchingPointer(old, []messageAttachment{old, old}, m, map[int]bool{}))
	old.Pointer.Locator.Key = []byte{3}
	assert.Nil(t, matchingPointer(old, []messageAttachment{old}, m, map[int]bool{}))
}

func TestImportAmbiguousSameTimeReplay(t *testing.T) {
	data := []byte("fictional attachment")
	message := func(body, p string) string {
		return `{"chatItem":{"chatId":"7","authorId":"2","dateSent":"1700000000000","incoming":{},"standardMessage":{"text":{"body":"` + body + `"},"attachments":[{"clientUuid":"AQ==","pointer":` + p + `}]}}}`
	}
	missing := `{"locatorInfo":{"key":"AQ=="}}`
	between := strings.Replace(incoming, "1700000000000", "1700000000001", 1)
	for _, tc := range []struct {
		name                     string
		first, replay, corrected []string
		existing                 int64
	}{
		{"existing pair reordered", []string{message("A", missing), between, message("B", missing)}, []string{message("B", pointer(data)), between, message("C", pointer(data))}, []string{message("A", pointer(data)), between, message("B", pointer(data))}, 1},
		{"earlier message added", []string{message("B", missing)}, []string{message("A", pointer(data)), message("B", pointer(data))}, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := fixture(t, tc.first...)
			opts := options(t, root)
			first, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, len(tc.first), first.MessagesAdded)
			assert.Equal(t, len(tc.first)-int(tc.existing), first.AttachmentsMissing)
			var firstID, lastID int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT min(id), max(id) FROM messages").Scan(&firstID, &lastID))
			ids := []int64{firstID}
			if lastID != firstID {
				ids = append(ids, lastID)
			}
			putMedia(t, root, data)
			writeFrames(t, root, tc.replay...)
			summary, err := Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, 2, summary.Skipped["ambiguous_message"])
			assert.Zero(t, summary.MessagesAdded)
			assert.EqualValues(t, tc.existing, summary.MessagesExisting)
			assert.Zero(t, summary.MessagesChanged)
			assert.Empty(t, summary.MediaMissing)
			assert.Zero(t, summary.AttachmentsStored)
			assert.Zero(t, summary.AttachmentsMissing)
			assert.Equal(t, len(tc.first), messageCount(t, st))
			for _, id := range ids {
				assert.Equal(t, map[string]string{"attachment:0": ""}, slotHashes(t, st, id))
			}
			files, err := os.ReadDir(opts.AttachmentsDir)
			require.NoError(t, err)
			assert.Empty(t, files)
			if tc.corrected == nil {
				assertBody(t, st, firstID, "B")
				return
			}
			writeFrames(t, root, tc.corrected...)
			summary, err = Import(t.Context(), st, opts)
			require.NoError(t, err)
			assert.EqualValues(t, len(tc.corrected), summary.MessagesExisting)
			assert.Zero(t, summary.Skipped["ambiguous_message"])
			assert.Equal(t, 2, summary.AttachmentsStored)
			for _, id := range ids {
				assert.Equal(t, map[string]string{"attachment:0": hashHex(data)}, slotHashes(t, st, id))
			}
		})
	}
}

func TestReviewOmissionsForExpiredMetadata(t *testing.T) {
	st := testutil.NewTestStore(t)
	frame := strings.Replace(incoming, `"incoming":{}`, `"incoming":{},"sms":true,"expireStartDate":"1700000000000","expiresInMs":"60000"`, 1)
	root := fixture(t, frame)
	opts := options(t, root)
	for range 2 {
		summary, err := Import(t.Context(), st, opts)
		require.NoError(t, err)
		assert.EqualValues(t, 1, summary.Omitted["sms"])
		assert.EqualValues(t, 1, summary.Omitted["expire_start_date"])
		assert.EqualValues(t, 1, summary.Omitted["expires_in_ms"])
	}
}

func TestImportIgnoresInvalidDirectionWhenCountingCollisions(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := fixture(t, incoming)
	opts := options(t, root)
	_, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	edited := strings.Replace(incoming, "Hello 星", "Edited", 1)
	invalid := strings.Replace(incoming, `"incoming":{}`, `"outgoing":{}`, 1)
	writeFrames(t, root, invalid, edited)
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.Skipped["direction_conflict"])
	assert.Zero(t, summary.Skipped["ambiguous_message"])
	assert.EqualValues(t, 1, summary.MessagesExisting)
	assert.EqualValues(t, 1, summary.MessagesChanged)
	id, _ := onlyMessage(t, st)
	assertBody(t, st, id, "Hello 星")
}
