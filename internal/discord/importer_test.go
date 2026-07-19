package discord

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type importerFakeAPI struct {
	mu sync.Mutex

	guild       Guild
	channels    []Channel
	active      []Channel
	messages    map[string][]Message
	queries     map[string][]MessageQuery
	failBefore  map[string]map[string]error
	injectAfter map[string][]Message
	catalogErr  error
	messageHook func(string, MessageQuery) ([]Message, error, bool)
}

func newImporterFakeAPI(channels ...Channel) *importerFakeAPI {
	return &importerFakeAPI{
		guild:       Guild{ID: "200", Name: "Test Guild"},
		channels:    channels,
		messages:    map[string][]Message{},
		queries:     map[string][]MessageQuery{},
		failBefore:  map[string]map[string]error{},
		injectAfter: map[string][]Message{},
	}
}

func (f *importerFakeAPI) Me(context.Context) (User, error) { return User{}, nil }
func (f *importerFakeAPI) Guilds(context.Context) ([]Guild, error) {
	return []Guild{f.guild}, nil
}
func (f *importerFakeAPI) Guild(_ context.Context, _ string) (Guild, error) {
	return f.guild, nil
}
func (f *importerFakeAPI) GuildChannels(_ context.Context, _ string) ([]Channel, error) {
	if f.catalogErr != nil {
		return nil, f.catalogErr
	}
	return slices.Clone(f.channels), nil
}
func (f *importerFakeAPI) ActiveThreads(context.Context, string) ([]Channel, error) {
	return slices.Clone(f.active), nil
}
func (f *importerFakeAPI) ArchivedThreads(context.Context, string, bool, time.Time) (ThreadPage, error) {
	return ThreadPage{}, nil
}
func (f *importerFakeAPI) Message(context.Context, string, string) (Message, error) {
	return Message{}, errors.New("not implemented")
}

func (f *importerFakeAPI) Messages(_ context.Context, channelID string, query MessageQuery) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries[channelID] = append(f.queries[channelID], query)
	if f.messageHook != nil {
		if page, err, handled := f.messageHook(channelID, query); handled {
			return slices.Clone(page), err
		}
	}
	if byCursor := f.failBefore[channelID]; byCursor != nil && query.Before != "" {
		if err := byCursor[query.Before]; err != nil {
			delete(byCursor, query.Before)
			return nil, err
		}
	}
	if query.Before != "" && len(f.injectAfter[channelID]) != 0 {
		f.messages[channelID] = append(f.messages[channelID], f.injectAfter[channelID]...)
		delete(f.injectAfter, channelID)
	}

	items := slices.Clone(f.messages[channelID])
	slices.SortFunc(items, func(left, right Message) int {
		lv, _ := ParseSnowflake(left.ID)
		rv, _ := ParseSnowflake(right.ID)
		switch {
		case lv < rv:
			return -1
		case lv > rv:
			return 1
		default:
			return 0
		}
	})
	var filtered []Message
	for _, message := range items {
		value, err := ParseSnowflake(message.ID)
		if err != nil {
			filtered = append(filtered, message)
			continue
		}
		if query.Before != "" {
			before, _ := ParseSnowflake(query.Before)
			if value >= before {
				continue
			}
		}
		if query.After != "" {
			after, _ := ParseSnowflake(query.After)
			if value <= after {
				continue
			}
		}
		filtered = append(filtered, message)
	}
	if query.After == "" {
		slices.Reverse(filtered)
	}
	if query.Limit > 0 && len(filtered) > query.Limit {
		filtered = filtered[:query.Limit]
	}
	return filtered, nil
}

func (f *importerFakeAPI) channelQueries(channelID string) []MessageQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries[channelID])
}

func importerTestChannel(id, name string) Channel {
	return Channel{ID: id, GuildID: "200", Type: channelTypeGuildText, Name: name}
}

func importerTestMessage(id, channelID, content string) Message {
	timestamp, _ := TimestampFromSnowflake(id)
	return Message{
		ID: id, ChannelID: channelID, GuildID: "200", Content: content,
		Timestamp: timestamp, Type: 0,
		Author: User{ID: "user-" + id, Username: "user-" + id},
	}
}

func newTestImporter(st *store.Store, api API) *Importer {
	importer := NewImporter(st, api)
	importer.pageSize = 2
	return importer
}

func importerTestSnowflake(t *testing.T, at time.Time, sequence uint64) string {
	t.Helper()
	lower, err := SnowflakeFromTimestamp(at)
	require.NoError(t, err)
	value, err := ParseSnowflake(lower)
	require.NoError(t, err)
	return strconv.FormatUint(value+sequence, 10)
}

func TestImporterPinsBackfillPagesBackwardThenCollectsForwardPerContainer(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "general"), importerTestChannel("400", "random"))
	for id := 101; id <= 105; id++ {
		api.messages["300"] = append(api.messages["300"], importerTestMessage(strconv.Itoa(id), "300", "message "+strconv.Itoa(id)))
	}
	api.messages["300"][4].MessageReference = &MessageReference{MessageID: "101", ChannelID: "300", GuildID: "200"}
	api.messages["300"][4].Mentions = []User{{ID: "mentioned", Username: "Mentioned User"}}
	api.messages["300"][4].Attachments = []Attachment{{ID: "attachment-1", Filename: "note.txt", Size: 12}}
	api.messages["300"][3].WebhookID = "webhook-1"
	api.messages["300"][3].Author = User{Username: "Per-message webhook name"}
	api.injectAfter["300"] = []Message{
		importerTestMessage("106", "300", "arrived during backfill"),
		importerTestMessage("107", "300", "arrived during backfill 2"),
	}
	api.messages["400"] = []Message{
		importerTestMessage("201", "400", "other one"),
		importerTestMessage("202", "400", "other two"),
	}

	summary, err := newTestImporter(st, api).Import(t.Context(), ImportOptions{
		GuildID: "200", AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	assert.Equal(int64(9), summary.MessagesProcessed)
	assert.Equal(int64(9), summary.MessagesAdded)
	assert.Equal(int64(1), summary.MediaPending)

	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal(ContainerState{
		HighWater: "107", BackfillBefore: "101", BackfillUpper: "105", BackfillComplete: true,
	}, state.Containers["300"])
	assert.Equal(ContainerState{
		HighWater: "202", BackfillBefore: "201", BackfillUpper: "202", BackfillComplete: true,
	}, state.Containers["400"])

	queries := api.channelQueries("300")
	require.GreaterOrEqual(len(queries), 6)
	require.NotEmpty(api.channelQueries("400"))
	assert.Equal(MessageQuery{Limit: 1}, queries[0])
	assert.Equal("106", queries[1].Before, "history starts immediately above the pinned head")
	assert.Equal("104", queries[2].Before)
	assert.Equal("102", queries[3].Before)
	assert.Equal("105", queries[4].After, "forward scan begins above the pinned head")
	assert.Equal("107", queries[5].After)

	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messageCount))
	assert.Equal(9, messageCount)
	var replyTo string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT parent.source_message_id
		FROM messages child JOIN messages parent ON parent.id = child.reply_to_message_id
		WHERE child.source_id = ? AND child.source_message_id = '105'`), source.ID).Scan(&replyTo))
	assert.Equal("101", replyTo, "reply deferred until the older parent exists")

	var webhookLabel string
	require.NoError(st.DB().QueryRow(`
		SELECT p.display_name FROM participants p
		JOIN participant_identifiers pi ON pi.participant_id = p.id
		WHERE pi.identifier_type = 'discord_webhook_id' AND pi.identifier_value = 'webhook-1'
	`).Scan(&webhookLabel))
	assert.Equal("Discord webhook webhook-1", webhookLabel)

	var rawFormat, body, metadata string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mr.raw_format, mb.body_text, m.metadata
		FROM messages m JOIN message_raw mr ON mr.message_id = m.id
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_id = ? AND m.source_message_id = '105'`), source.ID).Scan(&rawFormat, &body, &metadata))
	assert.Equal("discord_json", rawFormat)
	assert.Equal("message 105", body)
	assert.Contains(metadata, `"referenced_message_id":"101"`)

	var mentionCount, attachmentCount, conversationCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients mr JOIN messages m ON m.id = mr.message_id
		WHERE m.source_id = ? AND m.source_message_id = '105' AND mr.recipient_type = 'mention'`), source.ID).Scan(&mentionCount))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_id = ? AND m.source_message_id = '105'`), source.ID).Scan(&attachmentCount))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT message_count FROM conversations WHERE source_id = ? AND source_conversation_id = '300'`), source.ID).Scan(&conversationCount))
	assert.Equal(1, mentionCount)
	assert.Equal(1, attachmentCount)
	assert.Equal(7, conversationCount)
}

func TestImporterResumesFromNewestCheckpointMergedOverSuccessfulBaseline(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)

	baseline := NewSyncState()
	baseline.Containers["300"] = ContainerState{HighWater: "105", BackfillUpper: "105", BackfillBefore: "103"}
	baselineBlob, err := baseline.Marshal()
	require.NoError(err)
	baselineRun, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.CompleteSync(baselineRun, baselineBlob))

	checkpoint := NewSyncState()
	checkpoint.Containers["300"] = ContainerState{HighWater: "107", BackfillUpper: "105", BackfillBefore: "101"}
	checkpointBlob, err := checkpoint.Marshal()
	require.NoError(err)
	failedRun, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(failedRun, &store.Checkpoint{PageToken: checkpointBlob}))
	require.NoError(st.FailSync(failedRun, "synthetic crash"))

	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	for id := 99; id <= 108; id++ {
		api.messages["300"] = append(api.messages["300"], importerTestMessage(strconv.Itoa(id), "300", "message"))
	}
	_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200", AttachmentsDir: t.TempDir()})
	require.NoError(err)
	queries := api.channelQueries("300")
	require.NotEmpty(queries)
	assert.Equal("101", queries[0].Before, "newer failed checkpoint supplies the opaque backfill cursor")
	assert.NotEqual("106", queries[0].Before, "resume does not repin the channel head")

	completed, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := LoadSyncState(completed.CursorAfter.String)
	require.NoError(err)
	assert.Equal("108", state.Containers["300"].HighWater)
	assert.True(state.Containers["300"].BackfillComplete)
}

func TestImporterInitialStateResumesOnlyCompatibleRunShape(t *testing.T) {
	tests := []struct {
		name            string
		checkpointFull  bool
		checkpointLower string
		requestedFull   bool
		requestedLower  string
		wantCheckpoint  bool
	}{
		{name: "matching incremental", wantCheckpoint: true},
		{name: "matching full", checkpointFull: true, requestedFull: true, wantCheckpoint: true},
		{name: "full checkpoint ignored by incremental", checkpointFull: true},
		{name: "incremental checkpoint ignored by full", requestedFull: true},
		{name: "changed exact lower bound", checkpointLower: "42", requestedLower: "43"},
		{name: "matching exact lower bound", checkpointLower: "42", requestedLower: "42", wantCheckpoint: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewSQLiteTestStore(t)
			source, err := st.GetOrCreateSource("discord", "200")
			require.NoError(err)
			baseline := NewSyncState()
			baseline.Containers["300"] = ContainerState{
				HighWater: "900", BackfillBefore: "800", BackfillUpper: "900", BackfillComplete: true,
			}
			baselineBlob, err := baseline.Marshal()
			require.NoError(err)
			completedID, err := st.StartSync(source.ID, "discord")
			require.NoError(err)
			require.NoError(st.CompleteSync(completedID, baselineBlob))

			checkpoint := NewSyncState()
			checkpoint.Full = tt.checkpointFull
			checkpoint.LowerBound = tt.checkpointLower
			checkpoint.Containers["300"] = ContainerState{
				HighWater: "700", BackfillBefore: "600", BackfillUpper: "700",
			}
			checkpointBlob, err := checkpoint.Marshal()
			require.NoError(err)
			failedID, err := st.StartSync(source.ID, "discord")
			require.NoError(err)
			require.NoError(st.UpdateSyncCheckpoint(failedID, &store.Checkpoint{PageToken: checkpointBlob}))
			require.NoError(st.FailSync(failedID, "interrupted"))

			state, hadBaseline, err := newTestImporter(st, newImporterFakeAPI()).initialState(
				source.ID, tt.requestedFull, tt.requestedLower,
			)
			require.NoError(err)
			assert.Equal(tt.requestedFull, state.Full)
			assert.Equal(tt.requestedLower, state.LowerBound)
			if tt.wantCheckpoint {
				assert.Equal("600", state.Containers["300"].BackfillBefore)
			} else if tt.requestedFull {
				assert.Empty(state.Containers)
			} else {
				assert.Equal("800", state.Containers["300"].BackfillBefore)
			}
			assert.Equal(!tt.requestedFull, hadBaseline)
		})
	}
}

func TestImporterFailureCheckpointsDurablePagesAndResumeDoesNotReplayThem(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	for id := 101; id <= 105; id++ {
		api.messages["300"] = append(api.messages["300"], importerTestMessage(strconv.Itoa(id), "300", "message"))
	}
	api.messages["300"][4].MessageReference = &MessageReference{MessageID: "101", ChannelID: "300"}
	api.failBefore["300"] = map[string]error{"104": errors.New("page transport failed")}

	_, err := newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200", AttachmentsDir: t.TempDir()})
	require.Error(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	failed, err := st.GetLatestCheckpointedSync(source.ID)
	require.NoError(err)
	checkpoint, err := LoadSyncState(failed.CursorBefore.String)
	require.NoError(err)
	assert.Equal("104", checkpoint.Containers["300"].BackfillBefore)
	assert.Equal(int64(2), failed.MessagesProcessed)

	firstCallCount := len(api.channelQueries("300"))
	_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200", AttachmentsDir: t.TempDir()})
	require.NoError(err)
	resumed := api.channelQueries("300")[firstCallCount:]
	require.NotEmpty(resumed)
	assert.Equal("104", resumed[0].Before)
	for _, query := range resumed {
		assert.NotEqual("106", query.Before, "the already checkpointed first page is not replayed")
	}

	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(5, count, "replayed or resumed pages remain idempotent")
	var replyTo string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT parent.source_message_id
		FROM messages child JOIN messages parent ON parent.id = child.reply_to_message_id
		WHERE child.source_id = ? AND child.source_message_id = '105'`), source.ID).Scan(&replyTo))
	assert.Equal("101", replyTo, "deferred reply survives a crash between child and parent pages")
}

func TestImporterRepeatedIDsRefreshContentRawMetadataRecipientsAndAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	first := importerTestMessage("101", "300", "original")
	first.Mentions = []User{{ID: "old-mention", Username: "Old Mention"}}
	first.Attachments = []Attachment{{ID: "old-attachment", Filename: "old.txt", Size: 1}}
	api.messages["300"] = []Message{first}
	importer := newTestImporter(st, api)

	firstSummary, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	assert.Equal(int64(1), firstSummary.MessagesAdded)

	editedAt := time.Now().UTC()
	edited := importerTestMessage("101", "300", "edited")
	edited.EditedTimestamp = &editedAt
	edited.Mentions = []User{{ID: "new-mention", Username: "New Mention"}}
	edited.Attachments = []Attachment{{ID: "new-attachment", Filename: "new.txt", Size: 2}}
	edited.Reactions = []Reaction{{Count: 3, Emoji: Emoji{Name: "thumbsup"}}}
	edited.Raw = []byte(`{"id":"101","channel_id":"300","content":"edited","future":"retained"}`)
	api.messages["300"] = []Message{edited}

	secondSummary, err := importer.Import(t.Context(), ImportOptions{GuildID: "200", Full: true})
	require.NoError(err)
	assert.Equal(int64(0), secondSummary.MessagesAdded)
	assert.Equal(int64(1), secondSummary.MessagesUpdated)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)

	var count, editedFlag, oldMention, newMention, oldAttachment, newAttachment int
	var body, metadata string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*), MAX(CASE WHEN is_edited THEN 1 ELSE 0 END)
		FROM messages WHERE source_id = ? AND source_message_id = '101'`), source.ID).Scan(&count, &editedFlag))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text, m.metadata FROM messages m JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_id = ? AND m.source_message_id = '101'`), source.ID).Scan(&body, &metadata))
	for _, check := range []struct {
		participant string
		count       *int
	}{{"old-mention", &oldMention}, {"new-mention", &newMention}} {
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*) FROM message_recipients mr
			JOIN messages m ON m.id = mr.message_id
			JOIN participant_identifiers pi ON pi.participant_id = mr.participant_id
			WHERE m.source_id = ? AND m.source_message_id = '101'
			  AND mr.recipient_type = 'mention' AND pi.identifier_value = ?`), source.ID, check.participant).Scan(check.count))
	}
	for _, check := range []struct {
		attachment string
		count      *int
	}{{"discord:old-attachment", &oldAttachment}, {"discord:new-attachment", &newAttachment}} {
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*) FROM attachments a JOIN messages m ON m.id = a.message_id
			WHERE m.source_id = ? AND m.source_message_id = '101' AND a.source_attachment_id = ?`), source.ID, check.attachment).Scan(check.count))
	}
	assert.Equal(1, count)
	assert.Equal(1, editedFlag)
	assert.Equal("edited", body)
	assert.Contains(metadata, `"count":3`)
	assert.Equal(0, oldMention)
	assert.Equal(1, newMention)
	assert.Equal(0, oldAttachment)
	assert.Equal(1, newAttachment)
	raw, err := st.GetMessageRaw(messageIDBySource(t, st, source.ID, "101"))
	require.NoError(err)
	assert.JSONEq(`{"id":"101","channel_id":"300","content":"edited","future":"retained"}`, string(raw))
}

func TestImporterCountsEachAttachmentOnceAcrossHistoryAndRepair(t *testing.T) {
	for _, withArchiver := range []bool{false, true} {
		t.Run(strconv.FormatBool(withArchiver), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewSQLiteTestStore(t)
			now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
			messageID := importerTestSnowflake(t, now.Add(-time.Hour), 1)
			message := importerTestMessage(messageID, "300", "recent attachment")
			message.Attachments = []Attachment{{ID: "401", Filename: "pending.bin", Size: 42}}
			api := newImporterFakeAPI(importerTestChannel("300", "general"))
			api.messages["300"] = []Message{message}
			importer := newTestImporter(st, api)
			importer.now = func() time.Time { return now }
			opts := ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour}
			if withArchiver {
				opts.AttachmentsDir = t.TempDir()
			}

			summary, err := importer.Import(t.Context(), opts)
			require.NoError(err)
			assert.Equal(int64(1), summary.MediaPending)
			assert.Zero(summary.MediaDownloaded)
		})
	}
}

func TestImporterRepeatedPayloadRefreshesMetadataAndCountsOnlyNewAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "300", "channel", "general")
	require.NoError(err)
	importer := newTestImporter(st, newImporterFakeAPI())
	summary := &ImportSummary{processedMessageIDs: map[string]struct{}{}}
	message := importerTestMessage("501", "300", "attachments")
	message.Attachments = []Attachment{{ID: "401", Filename: "old.bin", Size: 1}}
	require.NoError(importer.persistPage(t.Context(), source.ID, conversationID, []Message{message}, summary, nil))

	message.Attachments = []Attachment{
		{ID: "401", Filename: "renamed.bin", Size: 2},
		{ID: "402", Filename: "new.bin", Size: 3},
	}
	require.NoError(importer.persistPage(t.Context(), source.ID, conversationID, []Message{message}, summary, nil))
	messageID := messageIDBySource(t, st, source.ID, "501")
	refs, err := st.MessageDiscordAttachments(messageID)
	require.NoError(err)
	require.Len(refs, 2)
	assert.Equal("renamed.bin", refs["discord:401"].Filename)
	assert.Equal("new.bin", refs["discord:402"].Filename)
	assert.Equal(int64(2), summary.MediaPending)
}

func messageIDBySource(t *testing.T, st *store.Store, sourceID int64, sourceMessageID string) int64 {
	t.Helper()
	var messageID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
	`), sourceID, sourceMessageID).Scan(&messageID))
	return messageID
}

func TestImporterAfterUsesExactSnowflakeLowerBoundAndFullStartsFresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	old := NewSyncState()
	old.Containers["300"] = ContainerState{HighWater: "999999999999999999", BackfillComplete: true}
	blob, err := old.Marshal()
	require.NoError(err)
	runID, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.CompleteSync(runID, blob))

	after := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	lowerString, err := SnowflakeFromTimestamp(after)
	require.NoError(err)
	lower, err := ParseSnowflake(lowerString)
	require.NoError(err)
	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	api.messages["300"] = []Message{
		importerTestMessage(strconv.FormatUint(lower-1, 10), "300", "before bound"),
		importerTestMessage(lowerString, "300", "at bound"),
		importerTestMessage(strconv.FormatUint(lower+1, 10), "300", "after bound"),
		importerTestMessage(strconv.FormatUint(lower+2, 10), "300", "after bound 2"),
	}

	_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{
		GuildID: "200", Full: true, After: after, AttachmentsDir: t.TempDir(),
	})
	require.NoError(err)
	queries := api.channelQueries("300")
	require.NotEmpty(queries)
	assert.Empty(queries[0].After, "full import ignores the completed high-water cursor")
	var ids []string
	rows, err := st.DB().Query(st.Rebind(`SELECT source_message_id FROM messages WHERE source_id = ? ORDER BY source_message_id`), source.ID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		require.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{strconv.FormatUint(lower+1, 10), strconv.FormatUint(lower+2, 10)}, ids)
}

func TestImporterMalformedStateFailsRunWithoutCallingDiscord(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	runID, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.CompleteSync(runID, `{"version":1,"containers":{"300":{"high_water":"bad"}}}`))
	api := newImporterFakeAPI(importerTestChannel("300", "general"))

	_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200", AttachmentsDir: t.TempDir()})
	require.Error(err)
	assert.Contains(err.Error(), "load last successful Discord sync state")
	assert.Empty(api.channelQueries("300"))
	latest, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(runID, latest.ID, "state-load failure must not supersede malformed progress")
	assert.Equal(store.SyncStatusCompleted, latest.Status)
}

func TestImporterFullIgnoresMalformedLatestCheckpointButIncrementalDoesNotSupersedeIt(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(strconv.FormatBool(full), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewSQLiteTestStore(t)
			source, err := st.GetOrCreateSource("discord", "200")
			require.NoError(err)
			baseline, err := NewSyncState().Marshal()
			require.NoError(err)
			completedID, err := st.StartSync(source.ID, "discord")
			require.NoError(err)
			require.NoError(st.CompleteSync(completedID, baseline))
			malformedID, err := st.StartSync(source.ID, "discord")
			require.NoError(err)
			require.NoError(st.UpdateSyncCheckpoint(malformedID, &store.Checkpoint{
				PageToken: `{"version":1,"containers":{"300":{"high_water":"bad"}}}`,
			}))
			require.NoError(st.FailSync(malformedID, "interrupted"))
			api := newImporterFakeAPI(importerTestChannel("300", "general"))
			api.messages["300"] = []Message{importerTestMessage("501", "300", "history")}

			_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200", Full: full})
			latest, latestErr := st.GetLatestSync(source.ID)
			require.NoError(latestErr)
			if full {
				require.NoError(err)
				assert.Equal(store.SyncStatusCompleted, latest.Status)
				assert.Greater(latest.ID, malformedID)
				assert.NotEmpty(api.channelQueries("300"))
			} else {
				require.ErrorContains(err, "load latest Discord checkpoint")
				assert.Equal(malformedID, latest.ID)
				assert.Equal(store.SyncStatusFailed, latest.Status)
				assert.Empty(api.channelQueries("300"))
			}
		})
	}
}

func TestImporterCatalogFailurePreservesSafeCheckpointAndFailsRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	baseline := NewSyncState()
	baseline.ThreadCatalog["300"] = ThreadCatalogState{
		PublicArchiveWatermark: "2026-07-01T00:00:00Z",
	}
	blob, err := baseline.Marshal()
	require.NoError(err)
	runID, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.CompleteSync(runID, blob))
	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	api.catalogErr = errors.New("catalog unavailable")

	summary, err := newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200"})
	require.Error(err)
	require.NotEmpty(summary.CatalogIssues)
	assert.True(summary.CatalogIssues[0].Fatal)
	failed, err := st.GetLatestCheckpointedSync(source.ID)
	require.NoError(err)
	checkpoint, err := LoadSyncState(failed.CursorBefore.String)
	require.NoError(err)
	assert.Equal(baseline.ThreadCatalog, checkpoint.ThreadCatalog)
	assert.Equal(store.SyncStatusFailed, failed.Status)
}

func TestImporterKeepsPreviouslyStoredAbsentContainerAndItsMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("discord", "200")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "300", "channel", "Archived thread")
	require.NoError(err)
	wantMetadata := `{"guild_id":"200","parent_channel_id":"250","discord_channel_type":11,"thread":{"archived":true}}`
	require.NoError(st.SetConversationMetadata(conversationID, sql.NullString{String: wantMetadata, Valid: true}))
	baseline := NewSyncState()
	baseline.Containers["300"] = ContainerState{
		HighWater: "100", BackfillUpper: "100", BackfillBefore: "1", BackfillComplete: true,
	}
	blob, err := baseline.Marshal()
	require.NoError(err)
	runID, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.CompleteSync(runID, blob))
	api := newImporterFakeAPI()
	api.messages["300"] = []Message{importerTestMessage("101", "300", "still accessible")}

	_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	queries := api.channelQueries("300")
	require.NotEmpty(queries)
	assert.Equal("100", queries[0].After)
	metadata, err := st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.JSONEq(wantMetadata, metadata.String)
}

func TestImporterPreviouslyStoredContainerFiltersUseArchivedParentMetadata(t *testing.T) {
	topLevelMetadata := `{"guild_id":"200","discord_channel_type":0}`
	threadMetadata := `{"guild_id":"200","parent_channel_id":"250","discord_channel_type":11,"thread":{"archived":true}}`
	malformedMetadata := `{not-json`
	tests := []struct {
		name         string
		metadata     *string
		guildConfig  config.DiscordGuildConfig
		wantImported bool
	}{
		{
			name: "changed include list omits stored top-level channel", metadata: &topLevelMetadata,
			guildConfig: config.DiscordGuildConfig{Include: []string{"400"}},
		},
		{
			name: "stored thread inherits new parent exclusion", metadata: &threadMetadata,
			guildConfig: config.DiscordGuildConfig{Exclude: []string{"250"}},
		},
		{
			name: "explicit stored thread inclusion overrides parent exclusion", metadata: &threadMetadata,
			guildConfig:  config.DiscordGuildConfig{Include: []string{"300"}, Exclude: []string{"250"}},
			wantImported: true,
		},
		{
			name: "explicit stored thread exclusion wins", metadata: &threadMetadata,
			guildConfig: config.DiscordGuildConfig{Include: []string{"300"}, Exclude: []string{"300", "250"}},
		},
		{
			name:        "missing metadata is skipped conservatively when filters need a parent",
			guildConfig: config.DiscordGuildConfig{Exclude: []string{"250"}},
		},
		{
			name: "malformed metadata is skipped conservatively when filters need a parent", metadata: &malformedMetadata,
			guildConfig: config.DiscordGuildConfig{Exclude: []string{"250"}},
		},
		{
			name: "missing metadata remains eligible without filters", wantImported: true,
		},
		{
			name: "explicit inclusion is safe with malformed metadata", metadata: &malformedMetadata,
			guildConfig:  config.DiscordGuildConfig{Include: []string{"300"}},
			wantImported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewSQLiteTestStore(t)
			source, err := st.GetOrCreateSource("discord", "200")
			require.NoError(err)
			conversationID, err := st.EnsureConversationWithType(source.ID, "300", "channel", "Stored container")
			require.NoError(err)
			if tt.metadata != nil {
				require.NoError(st.SetConversationMetadata(conversationID, sql.NullString{
					String: *tt.metadata, Valid: true,
				}))
			}
			baseline := NewSyncState()
			baseline.Containers["300"] = ContainerState{
				HighWater: "100", BackfillUpper: "100", BackfillBefore: "1", BackfillComplete: true,
			}
			blob, err := baseline.Marshal()
			require.NoError(err)
			runID, err := st.StartSync(source.ID, "discord")
			require.NoError(err)
			require.NoError(st.CompleteSync(runID, blob))
			api := newImporterFakeAPI()

			_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{
				GuildID: "200", GuildConfig: tt.guildConfig,
			})
			require.NoError(err)
			assert.Equal(tt.wantImported, len(api.channelQueries("300")) != 0)
		})
	}
}

func TestImporterEmptyContainerKeepsAForwardCursorForFutureMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var mu sync.Mutex
	available := false
	var afterCursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/guilds/200":
			writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "200", "name": "Test Guild"})
		case "/guilds/200/channels":
			writeDiscordJSON(w, http.StatusOK, []map[string]any{{
				"id": "300", "guild_id": "200", "type": 0, "name": "general",
			}})
		case "/guilds/200/threads/active":
			writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []any{}})
		case "/channels/300/threads/archived/public",
			"/channels/300/users/@me/threads/archived/private":
			writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []any{}, "has_more": false})
		case "/channels/300/messages":
			mu.Lock()
			after := request.URL.Query().Get("after")
			if after != "" {
				afterCursors = append(afterCursors, after)
			}
			hasMessage := available
			mu.Unlock()
			if !hasMessage {
				writeDiscordJSON(w, http.StatusOK, []any{})
				return
			}
			writeDiscordJSON(w, http.StatusOK, []map[string]any{{
				"id": "501", "channel_id": "300", "guild_id": "200",
				"author":  map[string]any{"id": "101", "username": "alice"},
				"content": "created later", "timestamp": "2026-07-19T00:00:00Z", "type": 0,
			}})
		default:
			writeDiscordJSON(w, http.StatusNotFound, map[string]any{"code": 0, "message": "not found"})
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)
	importer := newTestImporter(st, client)

	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	first, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	firstState, err := LoadSyncState(first.CursorAfter.String)
	require.NoError(err)
	assert.NotEqual("0", firstState.Containers["300"].HighWater)
	cursor, err := ParseSnowflake(firstState.Containers["300"].HighWater)
	require.NoError(err)
	assert.NotZero(cursor)
	assert.True(firstState.Containers["300"].BackfillComplete)
	legacyState := NewSyncState()
	legacyState.Containers["300"] = firstState.Containers["300"]
	legacyContainer := legacyState.Containers["300"]
	legacyContainer.HighWater = "0"
	legacyState.Containers["300"] = legacyContainer
	legacyBlob, err := legacyState.Marshal()
	require.NoError(err)
	legacyRun, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.CompleteSync(legacyRun, legacyBlob))

	mu.Lock()
	available = true
	mu.Unlock()
	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	mu.Lock()
	cursors := slices.Clone(afterCursors)
	mu.Unlock()
	require.NotEmpty(cursors)
	for _, after := range cursors {
		assert.NotEqual("0", after)
	}
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(1, count)
}

func TestImporterDisappearingPinnedHeadStillBecomesForwardCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	returnedPinnedHead := false
	api.messageHook = func(channelID string, query MessageQuery) ([]Message, error, bool) {
		if channelID == "300" && query.Limit == 1 && !returnedPinnedHead {
			returnedPinnedHead = true
			return []Message{importerTestMessage("500", "300", "deleted during scan")}, nil, true
		}
		return nil, nil, false
	}
	importer := newTestImporter(st, api)

	_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal("500", state.Containers["300"].HighWater)

	api.messageHook = nil
	api.messages["300"] = []Message{importerTestMessage("501", "300", "created later")}
	api.queries["300"] = nil
	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	require.NotEmpty(api.channelQueries("300"))
	assert.Equal("500", api.channelQueries("300")[0].After)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = '501'`,
	), source.ID).Scan(&count))
	assert.Equal(1, count)
}

func TestImporterIncrementalRepairUsesPinnedIntervalAndRefreshesMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	lower, err := SnowflakeFromTimestamp(now.Add(-7 * 24 * time.Hour))
	require.NoError(err)
	changedID := importerTestSnowflake(t, now.Add(-2*time.Hour), 1)
	deletedID := importerTestSnowflake(t, now.Add(-3*time.Hour), 2)
	abovePinnedHeadID := importerTestSnowflake(t, now.Add(-time.Hour), 3)

	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	api.messages["300"] = []Message{
		importerTestMessage(deletedID, "300", "will disappear"),
		importerTestMessage(changedID, "300", "before edit"),
	}
	importer := newTestImporter(st, api)
	importer.now = func() time.Time { return now }
	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "300", "channel", "general")
	require.NoError(err)

	editedAt := now.Add(-30 * time.Minute)
	changed := importerTestMessage(changedID, "300", "after edit")
	changed.EditedTimestamp = &editedAt
	changed.Reactions = []Reaction{{Emoji: Emoji{Name: "thumbsup"}, Count: 12}}
	// The pinned repair head is changedID. A concurrently archived message
	// above it must not enter the local comparison.
	api.messages["300"] = []Message{changed}
	api.queries["300"] = nil
	injected := false
	api.messageHook = func(_ string, query MessageQuery) ([]Message, error, bool) {
		if injected || query.Before == "" {
			return nil, nil, false
		}
		injected = true
		_, insertErr := st.UpsertMessage(&store.Message{
			SourceID: source.ID, ConversationID: conversationID,
			SourceMessageID: abovePinnedHeadID, MessageType: "discord",
			SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
		})
		require.NoError(insertErr)
		return nil, nil, false
	}
	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
	require.NoError(err)

	var body, metadata string
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text, m.metadata, m.is_edited
		FROM messages m JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, changedID).Scan(&body, &metadata, &edited))
	assert.Equal("after edit", body)
	assert.True(edited)
	assert.Contains(metadata, `"reaction_summaries":[{"emoji":"thumbsup","count":12}]`)
	assertMessageDeletionState(t, st, source.ID, deletedID, true)
	assertMessageDeletionState(t, st, source.ID, abovePinnedHeadID, false)

	queries := api.channelQueries("300")
	assert.Contains(queries, MessageQuery{Limit: 1})
	repairBefore, err := snowflakeSuccessor(changedID)
	require.NoError(err)
	assert.Contains(queries, MessageQuery{Before: repairBefore, Limit: 2})
	assert.NotEmpty(lower)
}

func TestImporterRepairClearsTombstoneWhenMessageReappears(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	messageID := importerTestSnowflake(t, now.Add(-time.Hour), 1)
	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	api.messages["300"] = []Message{importerTestMessage(messageID, "300", "present")}
	importer := newTestImporter(st, api)
	importer.now = func() time.Time { return now }
	_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	require.NoError(st.MarkMessageDeleted(source.ID, messageID))

	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
	require.NoError(err)
	assertMessageDeletionState(t, st, source.ID, messageID, false)
}

func TestImporterAccessFailuresRecordAndClearContainerMarkersWithoutChangingState(t *testing.T) {
	for _, tt := range []struct {
		name        string
		statusCode  int
		discordCode int
		marker      string
		reason      bool
		kind        ContainerIssueKind
	}{
		{
			name: "forbidden", statusCode: http.StatusForbidden, discordCode: 50013,
			marker: "container_inaccessible_since", kind: ContainerIssueForbidden,
		},
		{
			name: "unknown channel", statusCode: http.StatusNotFound, discordCode: 10003,
			marker: "container_missing_since", reason: true, kind: ContainerIssueUnknownChannel,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewSQLiteTestStore(t)
			now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
			messageID := importerTestSnowflake(t, now.Add(-time.Hour), 1)
			api := newImporterFakeAPI(importerTestChannel("300", "general"))
			api.messages["300"] = []Message{importerTestMessage(messageID, "300", "archived")}
			importer := newTestImporter(st, api)
			importer.now = func() time.Time { return now }
			_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
			require.NoError(err)
			source, err := st.GetSourceByIdentifier("200")
			require.NoError(err)
			before, err := st.GetLastSuccessfulSync(source.ID)
			require.NoError(err)

			failed := false
			api.messageHook = func(_ string, _ MessageQuery) ([]Message, error, bool) {
				if failed {
					return nil, nil, false
				}
				failed = true
				return nil, &APIError{
					Operation: "list channel messages", StatusCode: tt.statusCode, Code: tt.discordCode,
				}, true
			}
			summary, err := importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
			require.NoError(err)
			require.Len(summary.ContainerIssues, 1)
			assert.Equal(ContainerIssue{
				ContainerID: "300", Kind: tt.kind,
				StatusCode: tt.statusCode, DiscordCode: tt.discordCode,
			}, summary.ContainerIssues[0])
			after, err := st.GetLastSuccessfulSync(source.ID)
			require.NoError(err)
			assert.Equal(before.CursorAfter.String, after.CursorAfter.String)
			assertMessageDeletionState(t, st, source.ID, messageID, false)
			conversationID, err := st.EnsureConversationWithType(source.ID, "300", "channel", "general")
			require.NoError(err)
			metadata, err := st.GetConversationMetadata(conversationID)
			require.NoError(err)
			assert.Contains(metadata.String, `"discord_channel_type":0`)
			assert.Contains(metadata.String, `"`+tt.marker+`":"2026-07-19T12:00:00Z"`)
			if tt.reason {
				assert.Contains(metadata.String, `"container_missing_reason":"unknown_channel"`)
			}

			now = now.Add(6 * time.Hour)
			failed = false
			_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
			require.NoError(err)
			metadata, err = st.GetConversationMetadata(conversationID)
			require.NoError(err)
			assert.Contains(metadata.String, `"`+tt.marker+`":"2026-07-19T12:00:00Z"`,
				"repeated failures preserve when the inaccessible period began")

			api.messageHook = nil
			_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
			require.NoError(err)
			metadata, err = st.GetConversationMetadata(conversationID)
			require.NoError(err)
			assert.NotContains(metadata.String, "container_inaccessible_since")
			assert.NotContains(metadata.String, "container_missing_since")
			assert.NotContains(metadata.String, "container_missing_reason")
		})
	}
}

func TestImporterFirstSeenDeniedContainerRemainsRetryableWhenCatalogDropsIt(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "forbidden", err: &APIError{StatusCode: http.StatusForbidden, Code: 50013}},
		{name: "unknown channel", err: &APIError{StatusCode: http.StatusNotFound, Code: 10003}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewSQLiteTestStore(t)
			api := newImporterFakeAPI(importerTestChannel("250", "parent"))
			api.active = []Channel{{
				ID: "300", GuildID: "200", ParentID: "250", Type: 11, Name: "archived thread",
				ThreadMetadata: &ThreadMetadata{ArchiveTimestamp: time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)},
			}}
			api.messageHook = func(channelID string, _ MessageQuery) ([]Message, error, bool) {
				return nil, tt.err, channelID == "300"
			}
			importer := newTestImporter(st, api)

			_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
			require.NoError(err)
			source, err := st.GetSourceByIdentifier("200")
			require.NoError(err)
			run, err := st.GetLastSuccessfulSync(source.ID)
			require.NoError(err)
			state, err := LoadSyncState(run.CursorAfter.String)
			require.NoError(err)
			assert.Contains(state.Containers, "300")

			api.active = nil
			api.messageHook = nil
			api.messages["300"] = []Message{importerTestMessage("501", "300", "retry")}
			api.queries["300"] = nil
			_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200"})
			require.NoError(err)
			assert.NotEmpty(api.channelQueries("300"))
			var count int
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = '501'`,
			), source.ID).Scan(&count))
			assert.Equal(1, count)
		})
	}
}

func TestImporterDenialRetainsSafeForwardPageCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	api.messages["300"] = []Message{importerTestMessage("500", "300", "initial")}
	importer := newTestImporter(st, api)
	_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)

	api.messages["300"] = append(api.messages["300"], importerTestMessage("501", "300", "new"))
	api.messageHook = func(_ string, query MessageQuery) ([]Message, error, bool) {
		if query.After == "501" {
			return nil, &APIError{StatusCode: http.StatusForbidden, Code: 50013}, true
		}
		return nil, nil, false
	}
	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal("501", state.Containers["300"].HighWater)
}

func TestImporterCodeZero404FailsWithoutMissingMarkerOrCursorChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	messageID := importerTestSnowflake(t, now.Add(-time.Hour), 1)
	api := newImporterFakeAPI(importerTestChannel("300", "general"))
	api.messages["300"] = []Message{importerTestMessage(messageID, "300", "archived")}
	importer := newTestImporter(st, api)
	importer.now = func() time.Time { return now }
	_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	before, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)

	api.messageHook = func(_ string, _ MessageQuery) ([]Message, error, bool) {
		return nil, &APIError{
			Operation: "list channel messages", StatusCode: http.StatusNotFound,
		}, true
	}
	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.Error(err)
	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	assert.Zero(apiErr.Code)
	checkpoint, err := st.GetLatestCheckpointedSync(source.ID)
	require.NoError(err)
	assert.Equal(before.CursorAfter.String, checkpoint.CursorBefore.String)
	assertMessageDeletionState(t, st, source.ID, messageID, false)
	conversationID, err := st.EnsureConversationWithType(source.ID, "300", "channel", "general")
	require.NoError(err)
	metadata, err := st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.NotContains(metadata.String, "container_missing_since")
	assert.NotContains(metadata.String, "container_missing_reason")
}

func TestImporterIncompleteRepairNeverMarksDeletions(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "page failure", err: errors.New("temporary Discord failure")},
		{name: "cancellation", err: context.Canceled},
		{name: "malformed response", err: ErrDecodeResponse},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
			ids := []string{
				importerTestSnowflake(t, now.Add(-4*time.Hour), 1),
				importerTestSnowflake(t, now.Add(-3*time.Hour), 2),
				importerTestSnowflake(t, now.Add(-2*time.Hour), 3),
				importerTestSnowflake(t, now.Add(-time.Hour), 4),
			}
			api := newImporterFakeAPI(importerTestChannel("300", "general"))
			for _, id := range ids {
				api.messages["300"] = append(api.messages["300"], importerTestMessage(id, "300", id))
			}
			importer := newTestImporter(st, api)
			importer.now = func() time.Time { return now }
			_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
			require.NoError(err)
			source, err := st.GetSourceByIdentifier("200")
			require.NoError(err)

			api.messages["300"] = []Message{
				importerTestMessage(ids[1], "300", ids[1]),
				importerTestMessage(ids[2], "300", ids[2]),
				importerTestMessage(ids[3], "300", ids[3]),
			}
			api.failBefore["300"] = map[string]error{ids[2]: tt.err}
			_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", EditRescanWindow: 7 * 24 * time.Hour})
			require.Error(err)
			assertMessageDeletionState(t, st, source.ID, ids[0], false)
		})
	}

	t.Run("out of order partial page", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewSQLiteTestStore(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		ids := []string{
			importerTestSnowflake(t, now.Add(-4*time.Hour), 1),
			importerTestSnowflake(t, now.Add(-3*time.Hour), 2),
			importerTestSnowflake(t, now.Add(-2*time.Hour), 3),
			importerTestSnowflake(t, now.Add(-time.Hour), 4),
		}
		api := newImporterFakeAPI(importerTestChannel("300", "general"))
		for _, id := range ids {
			api.messages["300"] = append(api.messages["300"], importerTestMessage(id, "300", id))
		}
		importer := newTestImporter(st, api)
		importer.now = func() time.Time { return now }
		_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
		require.NoError(err)
		source, err := st.GetSourceByIdentifier("200")
		require.NoError(err)
		api.messages["300"] = []Message{
			importerTestMessage(ids[1], "300", ids[1]),
			importerTestMessage(ids[2], "300", ids[2]),
			importerTestMessage(ids[3], "300", ids[3]),
		}
		api.messageHook = func(channelID string, query MessageQuery) ([]Message, error, bool) {
			if channelID == "300" && query.Before == ids[2] {
				return []Message{
					importerTestMessage(ids[1], "300", ids[1]),
					importerTestMessage(ids[2], "300", ids[2]),
				}, nil, true
			}
			return nil, nil, false
		}
		_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200"})
		require.ErrorContains(err, "not strictly below prior cursor")
		assertMessageDeletionState(t, st, source.ID, ids[0], false)
	})
}

func TestImporterReconcilesSuccessfulContainerWhenAnotherContainerFails(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	deletedInSuccessful := importerTestSnowflake(t, now.Add(-3*time.Hour), 1)
	keptInSuccessful := importerTestSnowflake(t, now.Add(-2*time.Hour), 2)
	wouldDeleteInFailed := importerTestSnowflake(t, now.Add(-3*time.Hour), 3)
	keptInFailed := importerTestSnowflake(t, now.Add(-2*time.Hour), 4)
	api := newImporterFakeAPI(
		importerTestChannel("300", "general"), importerTestChannel("400", "random"),
	)
	api.messages["300"] = []Message{
		importerTestMessage(deletedInSuccessful, "300", "deleted"),
		importerTestMessage(keptInSuccessful, "300", "kept"),
	}
	api.messages["400"] = []Message{
		importerTestMessage(wouldDeleteInFailed, "400", "not safely deleted"),
		importerTestMessage(keptInFailed, "400", "kept"),
	}
	importer := newTestImporter(st, api)
	importer.now = func() time.Time { return now }
	_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	api.messages["300"] = []Message{importerTestMessage(keptInSuccessful, "300", "kept")}
	api.messages["400"] = []Message{importerTestMessage(keptInFailed, "400", "kept")}
	api.messageHook = func(channelID string, _ MessageQuery) ([]Message, error, bool) {
		if channelID == "400" {
			return nil, errors.New("container failed"), true
		}
		return nil, nil, false
	}
	_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.Error(err)
	assertMessageDeletionState(t, st, source.ID, deletedInSuccessful, true)
	assertMessageDeletionState(t, st, source.ID, wouldDeleteInFailed, false)
}

func TestImporterFullRepairRespectsAfterAndUnboundedEmptyRemote(t *testing.T) {
	t.Run("after leaves older rows untouched", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewSQLiteTestStore(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		oldID := importerTestSnowflake(t, now.Add(-30*24*time.Hour), 1)
		recentMissingID := importerTestSnowflake(t, now.Add(-2*time.Hour), 2)
		survivorID := importerTestSnowflake(t, now.Add(-time.Hour), 3)
		api := newImporterFakeAPI(importerTestChannel("300", "general"))
		api.messages["300"] = []Message{
			importerTestMessage(oldID, "300", "old"),
			importerTestMessage(recentMissingID, "300", "recent"),
			importerTestMessage(survivorID, "300", "survivor"),
		}
		importer := newTestImporter(st, api)
		importer.now = func() time.Time { return now }
		_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
		require.NoError(err)
		source, err := st.GetSourceByIdentifier("200")
		require.NoError(err)
		api.messages["300"] = []Message{importerTestMessage(survivorID, "300", "survivor")}
		_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", Full: true, After: now.Add(-7 * 24 * time.Hour)})
		require.NoError(err)
		assertMessageDeletionState(t, st, source.ID, oldID, false)
		assertMessageDeletionState(t, st, source.ID, recentMissingID, true)
	})

	t.Run("deleted newest archived message is inside pinned local upper", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewSQLiteTestStore(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		remoteOlderID := importerTestSnowflake(t, now.Add(-3*time.Hour), 1)
		deletedNewestID := importerTestSnowflake(t, now.Add(-time.Hour), 2)
		api := newImporterFakeAPI(importerTestChannel("300", "general"))
		api.messages["300"] = []Message{
			importerTestMessage(remoteOlderID, "300", "older"),
			importerTestMessage(deletedNewestID, "300", "newest"),
		}
		importer := newTestImporter(st, api)
		importer.now = func() time.Time { return now }
		_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
		require.NoError(err)
		source, err := st.GetSourceByIdentifier("200")
		require.NoError(err)
		api.messages["300"] = []Message{importerTestMessage(remoteOlderID, "300", "older")}
		_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", Full: true})
		require.NoError(err)
		assertMessageDeletionState(t, st, source.ID, deletedNewestID, true)
	})

	t.Run("unbounded empty remote detects historical deletions", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewSQLiteTestStore(t)
		now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
		messageID := importerTestSnowflake(t, now.Add(-30*24*time.Hour), 1)
		api := newImporterFakeAPI(importerTestChannel("300", "general"))
		api.messages["300"] = []Message{importerTestMessage(messageID, "300", "historical")}
		importer := newTestImporter(st, api)
		importer.now = func() time.Time { return now }
		_, err := importer.Import(t.Context(), ImportOptions{GuildID: "200"})
		require.NoError(err)
		source, err := st.GetSourceByIdentifier("200")
		require.NoError(err)
		api.messages["300"] = nil
		api.queries["300"] = nil
		_, err = importer.Import(t.Context(), ImportOptions{GuildID: "200", Full: true})
		require.NoError(err)
		assertMessageDeletionState(t, st, source.ID, messageID, true)
		for _, query := range api.channelQueries("300") {
			assert.NotEqual("0", query.After)
			assert.NotEqual("0", query.Before)
		}
	})
}

func assertMessageDeletionState(
	t *testing.T, st *store.Store, sourceID int64, sourceMessageID string, wantDeleted bool,
) {
	t.Helper()
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(st.Rebind(`
		SELECT deleted_from_source_at FROM messages
		WHERE source_id = ? AND source_message_id = ?`), sourceID, sourceMessageID).Scan(&deletedAt)
	require.NoError(t, err)
	assert.Equal(t, wantDeleted, deletedAt.Valid)
}
