package discord

import (
	"database/sql"
	"encoding/json/v2"
	"io"
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

func packageFixture(t *testing.T, transcript string) string {
	t.Helper()
	root := t.TempDir()
	testutil.WriteFile(t, root, "Account/user.json", []byte(`{"id":"100000000000000001","email":"discard@example.com"}`))
	testutil.WriteFile(t, root, "Messages/c200000000000000001/channel.json", []byte(`{"id":"200000000000000001","type":1,"recipients":["100000000000000001","100000000000000002"]}`))
	testutil.WriteFile(t, root, "Messages/c200000000000000001/messages.json", []byte(transcript))
	return root
}

func TestPackageImportSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	root := packageFixture(t, `[{"ID":"300000000000000001","Timestamp":"2024-03-15T18:22:41-07:00","Contents":"original words","Attachments":"https://cdn.discordapp.com/attachments/1/2/example.png"}]`)
	testutil.WriteFile(t, root, "Messages/index.json", []byte(`{"200000000000000001":"Direct Message with Example","200000000000000002":null}`))
	first, err := ImportPackage(t.Context(), st, root)
	require.NoError(err)
	assert.EqualValues(1, first.MessagesAdded)
	checkPackageRunCounts(t, st, first)
	assert.EqualValues(1, first.AttachmentLinks)
	var sourceType, identifier, channel, body, metadata, storage, media, hash string
	var fromMe bool
	var sent time.Time
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT s.source_type,s.identifier,c.source_conversation_id,m.is_from_me,m.sent_at,b.body_text,m.metadata FROM messages m JOIN sources s ON s.id=m.source_id JOIN conversations c ON c.id=m.conversation_id JOIN message_bodies b ON b.message_id=m.id`).Scan(&sourceType, &identifier, &channel, &fromMe, &sent, &body, &metadata))
	assert.Equal("discord_package", sourceType)
	assert.Equal("100000000000000001", identifier)
	assert.Equal("200000000000000001", channel)
	var title string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title FROM conversations`).Scan(&title))
	assert.Equal("Direct Message with Example", title)
	assert.True(fromMe)
	assert.Equal("original words", body)
	assert.Equal("2024-03-16T01:22:41Z", sent.UTC().Format(time.RFC3339))
	var meta map[string]any
	require.NoError(json.Unmarshal([]byte(metadata), &meta))
	assert.Equal("sent_only", meta["coverage"])
	assert.Equal("2024-03-15T18:22:41-07:00", meta["original_timestamp"])
	assert.Equal("Messages/c200000000000000001/messages.json", meta["input_path"])
	assert.NotContains(metadata, "discard@example.com")
	assert.EqualValues(1, meta["row_ordinal"])
	var messageID int64
	var received sql.NullTime
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id,received_at FROM messages`).Scan(&messageID, &received))
	assert.False(received.Valid)
	raw, err := st.GetMessageRawContext(t.Context(), messageID)
	require.NoError(err)
	assert.JSONEq(`{"ID":"300000000000000001","Timestamp":"2024-03-15T18:22:41-07:00","Contents":"original words","Attachments":"https://cdn.discordapp.com/attachments/1/2/example.png"}`, string(raw))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT storage_path,media_type,coalesce(content_hash,'') FROM attachments`).Scan(&storage, &media, &hash))
	assert.Equal("https://cdn.discordapp.com/attachments/1/2/example.png", storage)
	assert.Equal("link", media)
	assert.Empty(hash)
	testutil.WriteFile(t, root, "Messages/c200000000000000001/messages.json", []byte(`[{"ID":"300000000000000001","Timestamp":"2024-03-15T18:22:41-07:00","Contents":"updated snapshot","Attachments":""}]`))
	again, err := ImportPackage(t.Context(), st, root)
	require.NoError(err)
	assert.Zero(again.MessagesAdded)
	assert.EqualValues(1, again.MessagesUpdated)
	checkPackageRunCounts(t, st, again)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&n))
	assert.Equal(1, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Equal("updated snapshot", body)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM attachments`).Scan(&n))
	assert.Zero(n)
	matches, total, err := st.SearchMessagesContext(t.Context(), "updated", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
	assert.Len(matches, 1)
}

// TestPackageJSONFixture exercises the independently authored, documented mock
// layout without implying that the fixture is a Discord-produced export.
func TestPackageJSONFixture(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	summary, err := ImportPackage(t.Context(), st, "testdata/package-json")
	requirements.NoError(err)
	checks.Equal(2, summary.Channels)
	checks.EqualValues(2, summary.MessagesAdded)
	checks.Zero(summary.MessagesSkipped)
	checks.EqualValues(2, summary.AttachmentLinks)

	for _, channelID := range []string{"725000000000000051", "725000000000000052"} {
		t.Run(channelID, func(t *testing.T) {
			var metadata string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT metadata FROM conversations WHERE source_conversation_id=?`), channelID).Scan(&metadata))
			channelJSON, err := os.ReadFile(filepath.Join("testdata/package-json/Messages", "c"+channelID, "channel.json"))
			require.NoError(t, err)
			assert.JSONEq(t, `{"coverage":"sent_only","channel":`+string(channelJSON)+`}`, metadata)
		})
	}
	var title string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT title FROM conversations WHERE source_conversation_id='725000000000000052'`).Scan(&title))
	checks.Equal("sketchbook", title)

	rows, err := st.DB().QueryContext(t.Context(), `SELECT storage_path FROM attachments ORDER BY storage_path`)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var links []string
	for rows.Next() {
		var link string
		requirements.NoError(rows.Scan(&link))
		links = append(links, link)
	}
	requirements.NoError(rows.Err())
	checks.Equal([]string{"https://media.example.com/attachments/lantern.png", "https://media.example.com/attachments/palette.txt"}, links)
}

func TestPackageTimestampProvenance(t *testing.T) {
	for _, tt := range []struct{ stamp, id, want, source string }{
		{"2016-04-30 11:18:25", "175928847299117063", "2016-04-30T11:18:25.796Z", "snowflake"},
		{"2025-02-09 13:14:15.123456+00:00", "726000000000000061", "2025-02-09T13:14:15.123456Z", "timestamp"},
		{"2024-03-15 18:22:41", "unknown-stable-id", "", "unknown"},
	} {
		t.Run(tt.source, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			record, err := json.Marshal(map[string]string{"ID": tt.id, "Timestamp": tt.stamp, "Contents": "clock test"})
			require.NoError(err)
			root := packageFixture(t, "["+string(record)+"]")
			_, err = ImportPackage(t.Context(), st, root)
			require.NoError(err)
			var sent sql.NullTime
			var metadata string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sent_at,metadata FROM messages`).Scan(&sent, &metadata))
			if tt.want == "" {
				assert.False(sent.Valid)
			} else {
				assert.Equal(tt.want, sent.Time.UTC().Format(time.RFC3339Nano))
			}
			var meta map[string]any
			require.NoError(json.Unmarshal([]byte(metadata), &meta))
			assert.Equal(tt.source, meta["time_source"])
			assert.Equal(tt.stamp, meta["original_timestamp"])
		})
	}
}

func TestPackageLegacyCSV(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	summary, err := ImportPackage(t.Context(), st, "testdata/package-csv")
	require.NoError(err)
	assert.EqualValues(5, summary.MessagesAdded)
	assert.EqualValues(1, summary.AttachmentLinks)
	var sent time.Time
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sent_at FROM messages WHERE source_message_id='726000000000000061'`).Scan(&sent))
	assert.Equal("2025-02-09T13:14:15Z", sent.UTC().Format(time.RFC3339))
}

func TestPackagePartialFailureReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := packageFixture(t, `[{"ID":"1","Contents":"committed"},broken]`)
	summary, err := ImportPackage(t.Context(), st, root)
	require.Error(err)
	assert.EqualValues(1, summary.MessagesAdded)
	checkPackageRunCounts(t, st, summary)
	testutil.WriteFile(t, root, "Messages/c200000000000000001/messages.json", []byte(`[{"ID":"1","Contents":"committed"},{"ID":"2","Contents":"next"}]`))
	summary, err = ImportPackage(t.Context(), st, root)
	require.NoError(err)
	assert.EqualValues(1, summary.MessagesAdded)
	assert.EqualValues(1, summary.MessagesUpdated)
	checkPackageRunCounts(t, st, summary)
}

func TestPackageUnsafeAndOversizedInput(t *testing.T) {
	for _, tt := range []struct{ kind, want string }{
		{"symlink", "path escapes from parent"}, {"large metadata", "metadata exceeds"}, {"large record", "record 1 exceeds 1048576 bytes"}, {"large CSV record", "record 1 exceeds 1048576 bytes"}, {"combined CSV header", "CSV header"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			require := require.New(t)
			root := packageFixture(t, `[{"ID":"1","Contents":"safe"}]`)
			switch tt.kind {
			case "symlink":
				outside := t.TempDir()
				testutil.WriteFile(t, outside, "messages.json", []byte(`[{"ID":"2"}]`))
				require.NoError(os.Remove(filepath.Join(root, "Messages/c200000000000000001/messages.json")))
				require.NoError(os.Symlink(filepath.Join(outside, "messages.json"), filepath.Join(root, "Messages/c200000000000000001/messages.json")))
			case "large metadata":
				testutil.WriteFile(t, root, "Account/user.json", []byte(`{"id":"1","unused":"`+strings.Repeat("x", 2<<20)+`"}`))
			case "combined CSV header":
				require.NoError(os.Remove(filepath.Join(root, "Messages/c200000000000000001/messages.json")))
				testutil.WriteFile(t, root, "Messages/c200000000000000001/messages.csv", []byte("\"ID,Timestamp,Contents,Attachments\"\n1\n"))
			case "large CSV record":
				require.NoError(os.Remove(filepath.Join(root, "Messages/c200000000000000001/messages.json")))
				testutil.WriteFile(t, root, "Messages/c200000000000000001/messages.csv", []byte("ID,Timestamp,Contents,Attachments\n1,,\""+strings.Repeat("x", 2<<20)+"\",\n"))
			case "large record":
				testutil.WriteFile(t, root, "Messages/c200000000000000001/messages.json", []byte(`[{"ID":"1","Contents":"`+strings.Repeat("x", 2<<20)+`"}]`))
			}
			st := testutil.NewTestStore(t)
			_, err := ImportPackage(t.Context(), st, root)
			require.ErrorContains(err, tt.want)
			if strings.HasPrefix(tt.kind, "large ") && tt.kind != "large metadata" {
				require.ErrorContains(err, "Messages/c200000000000000001/messages.")
			}
		})
	}
}

func TestPackageChannelTypesAndSkips(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := packageFixture(t, `[{"ID":"1","Contents":""},{"Contents":"missing id"},{"ID":3,"Attachments":"https://example.com/a file:///etc/passwd ../escape"},{"ID":"7","Contents":"bad attachment shape","Attachments":["https://example.com/unsupported-shape"]},{"ID":"8","Attachments":"../escape"}]`)
	testutil.WriteFile(t, root, "Messages/200000000000000002/channel.json", []byte(`{"id":"200000000000000002","type":"GROUP_DM","name":"Example group","recipients":["100000000000000002"]}`))
	testutil.WriteFile(t, root, "Messages/200000000000000002/messages.json", []byte(`[{"ID":"4","Contents":"group text"}]`))
	testutil.WriteFile(t, root, "Messages/200000000000000003/channel.json", []byte(`{"id":"200000000000000003","type":0,"name":"general","guild":{"id":"200000000000000004","name":"Example guild"}}`))
	testutil.WriteFile(t, root, "Messages/200000000000000003/messages.json", []byte(`[{"ID":"5","Contents":"guild text"}]`))
	testutil.WriteFile(t, root, "Messages/200000000000000004/messages.json", []byte(`[{"ID":"6","Contents":"unknown channel"}]`))
	require.NoError(os.Mkdir(filepath.Join(root, "Messages/renamed"), 0o700))
	require.NoError(os.Symlink("c200000000000000001", filepath.Join(root, "Messages/200000000000000005")))
	testutil.WriteFile(t, root, "Messages/notes.txt", []byte("unrelated"))
	summary, err := ImportPackage(t.Context(), st, root)
	require.NoError(err)
	assert.EqualValues(4, summary.MessagesAdded)
	assert.EqualValues(4, summary.MessagesSkipped)
	assert.EqualValues(1, summary.SkipReasons["empty_record"])
	assert.EqualValues(1, summary.SkipReasons["missing_id"])
	assert.EqualValues(1, summary.SkipReasons["malformed_record"])
	var rejectedID, diagnostic string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT source_message_id,error_message FROM sync_run_items WHERE error_kind='malformed_record'`).Scan(&rejectedID, &diagnostic))
	assert.Equal("7", rejectedID)
	assert.Contains(diagnostic, "Attachments")
	assert.Contains(diagnostic, "string")
	assert.NotContains(diagnostic, "unsupported-shape")
	assert.EqualValues(1, summary.SkipReasons["unsupported_attachment_only"])
	assert.EqualValues(3, summary.UnsupportedAttachmentLinks)
	assert.Equal(4, summary.Channels)
	assert.Equal(2, summary.ChannelsSkipped)
	var kind string
	for id, want := range map[string]string{"200000000000000001": "direct_chat", "200000000000000002": "group_chat", "200000000000000003": "channel", "200000000000000004": "channel"} {
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT conversation_type FROM conversations WHERE source_conversation_id=?`), id).Scan(&kind))
		assert.Equal(want, kind)
	}
	testutil.WriteFile(t, root, "Account/user.json", []byte(`{"id":"100000000000000002"}`))
	other, err := ImportPackage(t.Context(), st, root)
	require.NoError(err)
	assert.NotEqual(summary.SourceID, other.SourceID)
	assert.EqualValues(4, other.MessagesAdded)
}

func TestPackageAtomicAttachmentSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite trigger regression")
	}
	root := packageFixture(t, `[{"ID":"1","Contents":"original","Attachments":"https://example.com/original"}]`)
	_, err := ImportPackage(t.Context(), st, root)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_package_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'attachment failure'); END`)
	require.NoError(err)
	testutil.WriteFile(t, root, "Messages/c200000000000000001/messages.json", []byte(`[{"ID":"1","Contents":"changed","Attachments":"https://example.com/changed"}]`))
	summary, err := ImportPackage(t.Context(), st, root)
	require.ErrorContains(err, "attachment failure")
	assert.Zero(summary.MessagesUpdated)
	var body, path string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT storage_path FROM attachments`).Scan(&path))
	assert.Equal("original", body)
	assert.Equal("https://example.com/original", path)
}

func TestPackageCSVOriginalMultilineRecord(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := packageFixture(t, `[]`)
	require.NoError(os.Remove(filepath.Join(root, "Messages/c200000000000000001/messages.json")))
	original := "1,2024-01-01 01:00:00+00:00,\"two\nlines, quoted\",https://example.com/a\n"
	testutil.WriteFile(t, root, "Messages/c200000000000000001/messages.csv", []byte("ID,Timestamp,Contents,Attachments\n"+original))
	reader, err := openPackage(t.Context(), root)
	require.NoError(err)
	defer func() { require.NoError(reader.root.Close()) }()
	count := 0
	require.NoError(reader.transcript(t.Context(), "c200000000000000001", func(record packageRecord) error {
		count++
		assert.Equal("two\nlines, quoted", record.Contents)
		assert.Equal(original, string(record.Raw))
		assert.Equal(1, record.Ordinal)
		return nil
	}))
	assert.Equal(1, count)
}

func FuzzPackageJSON(f *testing.F) {
	for _, seed := range []string{`[]`, `[{"ID":"1","Contents":"text"}]`, `[{"ID":1,"Attachments":["https://example.com/a"]}]`, `[{"ID":"1"},broken]`, `[null,42,{}]`} {
		f.Add(seed)
	}
	// A valid value only one byte past the limit reaches the explicit guard
	// if decoder read-ahead fits in its budget; removing it must fail the check.
	f.Add(`[{"Contents":"` + strings.Repeat("x", packageRecordLimit-len(`{"Contents":""}`)+1) + `"}]`)
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 2*packageRecordLimit {
			t.Skip("bounded fuzz input")
		}
		limited := &io.LimitedReader{R: strings.NewReader(input), N: packageRecordLimit + 1}
		_ = readPackageJSON(limited, "messages.json", func(record packageRecord) error {
			require.LessOrEqual(t, len(record.Raw), packageRecordLimit)
			return nil
		})
	})
}

func TestPackageReplayStableMetadata(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	root := packageFixture(t, `[{"ID":"1","Contents":"unchanged"}]`)
	_, err := ImportPackage(t.Context(), st, root)
	requirements.NoError(err)
	var before, channelBefore string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages`).Scan(&before))
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM conversations`).Scan(&channelBefore))
	for range 20 {
		_, err = ImportPackage(t.Context(), st, root)
		requirements.NoError(err)
		var after, channelAfter string
		requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages`).Scan(&after))
		requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM conversations`).Scan(&channelAfter))
		checks.Equal(before, after)
		checks.Equal(channelBefore, channelAfter)
	}
}

func TestPackageDirectCounterpart(t *testing.T) {
	for _, kind := range []string{`1`, `"DM"`, `3`, `0`} {
		t.Run(kind, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			root := packageFixture(t, `[{"ID":"1","Contents":"sent history"}]`)
			testutil.WriteFile(t, root, "Messages/c200000000000000001/channel.json", []byte(`{"type":`+kind+`,"recipients":["100000000000000001","100000000000000002"]}`))
			_, err := ImportPackage(t.Context(), st, root)
			requirements.NoError(err)
			peer, err := st.EnsureParticipantByIdentifierContext(t.Context(), "discord_user_id", "100000000000000002", "")
			requirements.NoError(err)
			var id int64
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&id))
			provenance, err := st.PersonProvenanceForMessages(t.Context(), []int64{id}, personscope.Scope{ParticipantIDs: []int64{peer}})
			requirements.NoError(err)
			var recipients int
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='to'`).Scan(&recipients))
			if kind == `1` || kind == `"DM"` {
				checks.NotNil(provenance[id])
				checks.Equal(1, recipients)
				chats, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "100000000000000002"})
				requirements.NoError(err)
				checks.Len(chats.Results, 1)
				// A changed snapshot with unknown recipients must clear the old to edge.
				testutil.WriteFile(t, root, "Messages/c200000000000000001/channel.json", []byte(`{"type":1,"recipients":[]}`))
				_, err = ImportPackage(t.Context(), st, root)
				requirements.NoError(err)
				requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM message_recipients WHERE recipient_type='to'`).Scan(&recipients))
				checks.Zero(recipients)
				provenance, err = st.PersonProvenanceForMessages(t.Context(), []int64{id}, personscope.Scope{ParticipantIDs: []int64{peer}})
				requirements.NoError(err)
				checks.Empty(provenance)
				chats, err = st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "100000000000000002"})
				requirements.NoError(err)
				checks.Empty(chats.Results)
			} else {
				checks.Empty(provenance)
				checks.Zero(recipients)
			}
		})
	}
}

func checkPackageRunCounts(t *testing.T, st *store.Store, summary *PackageSummary) {
	t.Helper()
	checks := assert.New(t)
	requirements := require.New(t)
	var processed, added, updated int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT messages_processed,messages_added,messages_updated FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&processed, &added, &updated))
	checks.Equal(summary.MessagesProcessed, processed)
	checks.Equal(summary.MessagesAdded, added)
	checks.Equal(summary.MessagesUpdated, updated)
}
