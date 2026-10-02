package inline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type historyRequest struct{ ChatID, BeforeID int64 }
type fakeClient struct {
	account       Account
	chats         map[int64]Conversation
	messages      map[int64][]Message
	pageSize      int
	requests      []historyRequest
	filesCalls    int
	discoverCalls int
	discover      func(context.Context) ([]Conversation, error)
	beforePage    func(context.Context, int64, int64) error
	files         func(context.Context, int64, []int64) ([]Media, error)
}

func (c *fakeClient) Close() error                        { return nil }
func (c *fakeClient) Me(context.Context) (Account, error) { return c.account, nil }
func (c *fakeClient) Discover(ctx context.Context) ([]Conversation, error) {
	c.discoverCalls++
	if c.discover != nil {
		return c.discover(ctx)
	}
	var result []Conversation
	for _, chat := range c.chats {
		result = append(result, chat)
	}
	slices.SortFunc(result, func(a, b Conversation) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return result, nil
}
func (c *fakeClient) Conversation(_ context.Context, id int64) (Conversation, error) {
	chat, ok := c.chats[id]
	if !ok {
		return Conversation{}, errors.New("not selected")
	}
	return chat, nil
}
func (c *fakeClient) Messages(ctx context.Context, id, before int64) (Page, error) {
	c.requests = append(c.requests, historyRequest{id, before})
	if c.beforePage != nil {
		if err := c.beforePage(ctx, id, before); err != nil {
			return Page{}, err
		}
	}
	all := slices.Clone(c.messages[id])
	slices.SortFunc(all, func(a, b Message) int {
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	eligible := []Message{}
	for _, message := range all {
		if before == 0 || message.ID < before {
			eligible = append(eligible, message)
		}
	}
	size := c.pageSize
	if size <= 0 {
		size = 2
	}
	page := Page{Messages: eligible}
	if len(eligible) > size {
		page.Messages = eligible[:size]
		page.HasMore = true
		page.NextBeforeID = page.Messages[len(page.Messages)-1].ID
	}
	return page, nil
}
func (c *fakeClient) Files(ctx context.Context, id int64, ids []int64) ([]Media, error) {
	c.filesCalls++
	if c.files != nil {
		return c.files(ctx, id, ids)
	}
	var result []Media
	for _, message := range c.messages[id] {
		if slices.Contains(ids, message.ID) {
			result = append(result, message.Media...)
		}
	}
	return result, nil
}

func syntheticMessage(chatID, id int64, text string) Message {
	return Message{ID: id, ChatID: chatID, SenderID: 101, Text: text, SentAt: time.Unix(1700000000+id, 0).UTC(), Raw: json.RawMessage(fmt.Sprintf(`{"id":%d,"chat_id":%d,"text":%q}`, id, chatID, text)), RawFormat: RawCLIFormat}
}

func importerFixture(t *testing.T) (*Importer, *fakeClient, ImportOptions) {
	t.Helper()
	client := &fakeClient{account: Account{UserID: 99, Origin: ProductionOrigin}, chats: map[int64]Conversation{1: {ID: 1, Type: "dm", Title: "Synthetic chat", MemberCount: 2, MemberCountKnown: true}}, messages: map[int64][]Message{}}
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, client.account.Identifier())
	require.NoError(t, err)
	return NewImporter(st, client), client, ImportOptions{Account: client.account, ChatIDs: []int64{1}, AttachmentsDir: t.TempDir()}
}

func archivedMessage(t *testing.T, st *store.Store, sourceID, chatID, messageID int64) int64 {
	t.Helper()
	rows, err := st.MessageExistsBatch(sourceID, []string{messageKey(chatID, messageID)})
	require.NoError(t, err)
	id := rows[messageKey(chatID, messageID)]
	require.Positive(t, id)
	return id
}

func storedState(t *testing.T, imp *Importer, sourceID int64, account string) *SyncState {
	t.Helper()
	state, err := imp.loadState(sourceID, account)
	require.NoError(t, err)
	return state
}

func TestImportCapturesHistoryRepliesAndFullRepairWithoutDeletingArchive(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	for id := int64(1); id <= 5; id++ {
		client.messages[1] = append(client.messages[1], syntheticMessage(1, id, fmt.Sprintf("message %d", id)))
	}
	client.messages[1][4].ReplyToMessageID = 1
	summary, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(5, summary.MessagesAdded)
	parent := archivedMessage(t, imp.store, summary.SourceID, 1, 1)
	child := archivedMessage(t, imp.store, summary.SourceID, 1, 5)
	var metadata string
	requires.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT metadata FROM messages WHERE id = ?`), child).Scan(&metadata))
	assertions.NotContains(metadata, `"edited_at"`, "messages with no source edit date must omit it from archived metadata")
	parentLink, err := imp.store.GetMessageReplyToMessageIDContext(t.Context(), child)
	requires.NoError(err)
	assertions.Equal(parent, parentLink.Int64)
	state := storedState(t, imp, summary.SourceID, opts.Account.Identifier())
	assertions.True(state.chat(1).HistoryDone)
	assertions.Equal(int64(5), state.chat(1).Head)
	second, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Zero(second.MessagesAdded)
	assertions.Zero(second.MessagesProcessed)
	// New replies to an old root are ordinary chat history, independent of the
	// root's timestamp. Capture the new ID without refreshing older snapshots.
	newReply := syntheticMessage(1, 6, "late reply")
	newReply.ReplyToMessageID = 1
	client.messages[1] = append(client.messages[1], newReply)
	client.messages[1][2].Text = "edited surviving message"
	client.messages[1][2].EditedAt = time.Unix(1700001000, 0).UTC()
	incremental, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, incremental.MessagesAdded)
	id3 := archivedMessage(t, imp.store, summary.SourceID, 1, 3)
	body, err := imp.store.GetMessageBodyText(id3)
	requires.NoError(err)
	assertions.Equal("message 3", body)
	// Removed source messages remain present when a full repair updates all
	// surviving records. Archive-local labels must survive provider refresh.
	labelID, err := imp.store.EnsureLabel(summary.SourceID, "archive-tag", "archive tag", "user")
	requires.NoError(err)
	requires.NoError(imp.store.AddMessageLabels(id3, []int64{labelID}))
	client.messages[1] = client.messages[1][1:]
	opts.Full = true
	full, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(5, full.MessagesUpdated)
	assertions.Zero(full.MessagesAdded)
	body, err = imp.store.GetMessageBodyText(id3)
	requires.NoError(err)
	assertions.Equal("edited surviving message", body)
	requires.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT metadata FROM messages WHERE id = ?`), id3).Scan(&metadata))
	assertions.Contains(metadata, `"edited_at":"2023-11-14T22:30:00Z"`, "full repair must retain the actual source edit date")
	archivedMessage(t, imp.store, summary.SourceID, 1, 1)
	var labels int
	requires.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`), id3, labelID).Scan(&labels))
	assertions.Equal(1, labels)
}

func TestLimitedHistoryAndInterruptedFullRepairConvergeAcrossRuns(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	for id := int64(1); id <= 5; id++ {
		client.messages[1] = append(client.messages[1], syntheticMessage(1, id, "original"))
	}
	opts.Limit = 1
	var sourceID int64
	for range 5 {
		summary, err := imp.Import(t.Context(), opts)
		requires.NoError(err)
		assertions.Equal(1, summary.MessagesProcessed)
		sourceID = summary.SourceID
	}
	// The final message can consume the budget just before a terminal empty
	// read; the next run certifies complete coverage without revisiting IDs.
	_, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.True(storedState(t, imp, sourceID, opts.Account.Identifier()).chat(1).HistoryDone)
	for index := range client.messages[1] {
		client.messages[1][index].Text = "repaired"
	}
	opts.Full = true
	for index := range 5 {
		summary, runErr := imp.Import(t.Context(), opts)
		requires.NoError(runErr)
		assertions.Equal(1, summary.MessagesUpdated)
		if index == 0 {
			client.messages[1] = append(client.messages[1], syntheticMessage(1, 6, "arrival during repair"))
		}
	}
	state := storedState(t, imp, sourceID, opts.Account.Identifier())
	assertions.False(state.chat(1).RepairActive)
	for id := int64(1); id <= 5; id++ {
		body, readErr := imp.store.GetMessageBodyText(archivedMessage(t, imp.store, sourceID, 1, id))
		requires.NoError(readErr)
		assertions.Equal("repaired", body)
	}
	opts.Full = false
	summary, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, summary.MessagesAdded)
	archivedMessage(t, imp.store, sourceID, 1, 6)
}

func TestImportCancellationPersistsOnlyDurableCursor(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	for id := int64(1); id <= 5; id++ {
		client.messages[1] = append(client.messages[1], syntheticMessage(1, id, "history"))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.beforePage = func(_ context.Context, _ int64, before int64) error {
		if before == 4 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	summary, err := imp.Import(ctx, opts)
	requires.ErrorIs(err, context.Canceled)
	assertions.Equal(2, summary.MessagesAdded)
	state := storedState(t, imp, summary.SourceID, opts.Account.Identifier())
	assertions.Equal(int64(4), state.chat(1).HistoryBefore)
	client.beforePage = nil
	resumed, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(3, resumed.MessagesAdded)
	var count int
	requires.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='inline'`).Scan(&count))
	assertions.Equal(5, count)
}

func TestSQLiteBodyWriteFailureHoldsCursorAndRetriesSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	imp.store = testutil.NewSQLiteTestStore(t)
	_, registerErr := imp.store.GetOrCreateSource(SourceType, client.account.Identifier())
	requires.NoError(registerErr)
	for id := int64(1); id <= 5; id++ {
		client.messages[1] = append(client.messages[1], syntheticMessage(1, id, "history"))
	}
	_, err := imp.store.DB().Exec(`CREATE TRIGGER fail_inline_body BEFORE INSERT ON message_bodies WHEN NEW.message_id IN (SELECT id FROM messages WHERE source_message_id='chat:1:message:3') BEGIN SELECT RAISE(ABORT,'synthetic body failure'); END`)
	requires.NoError(err)
	summary, err := imp.Import(t.Context(), opts)
	requires.Error(err)
	assertions.Equal(2, summary.MessagesAdded)
	state := storedState(t, imp, summary.SourceID, opts.Account.Identifier())
	assertions.Equal(int64(4), state.chat(1).HistoryBefore)
	rows, err := imp.store.MessageExistsBatch(summary.SourceID, []string{messageKey(1, 3)})
	requires.NoError(err)
	assertions.Empty(rows, "failed body transaction must not leave a message-only snapshot")
	_, err = imp.store.DB().Exec(`DROP TRIGGER fail_inline_body`)
	requires.NoError(err)
	var databaseSequence int
	var databaseName, databasePath string
	requires.NoError(imp.store.DB().QueryRow(`PRAGMA database_list`).Scan(&databaseSequence, &databaseName, &databasePath))
	requires.NoError(imp.store.Close())
	reopened, err := store.OpenForTest(databasePath)
	requires.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	imp = NewImporter(reopened, client)
	resumed, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(3, resumed.MessagesAdded)
	id := archivedMessage(t, imp.store, summary.SourceID, 1, 3)
	raw, err := imp.store.GetMessageRaw(id)
	requires.NoError(err)
	assertions.NotEmpty(raw)
	var indexed int
	requires.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'history'`).Scan(&indexed))
	assertions.Equal(5, indexed)
}

func TestAccountMismatchRejectsBeforeMessageWrites(t *testing.T) {
	imp, client, opts := importerFixture(t)
	client.account.UserID = 100
	_, err := imp.Import(t.Context(), opts)
	require.ErrorContains(t, err, "different account")
	var count int
	require.NoError(t, imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type='inline'`).Scan(&count))
	assert.Zero(t, count)
}

func TestImportRequiresRegisteredSourceWithoutRecreatingAccount(t *testing.T) {
	imp, client, opts := importerFixture(t)
	imp.store = testutil.NewSQLiteTestStore(t)
	client.messages[1] = []Message{syntheticMessage(1, 1, "unregistered")}
	_, err := imp.Import(t.Context(), opts)
	require.Error(t, err)
	for _, table := range []string{"sources", "sync_runs", "conversations", "messages"} {
		var count int
		require.NoError(t, imp.store.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count))
		assert.Zero(t, count, table)
	}
}

func TestDefaultDiscoveryAddsNewChatsAndExplicitSelectionRestrictsScope(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	opts.ChatIDs = nil
	client.messages[1] = []Message{syntheticMessage(1, 1, "first chat")}
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, first.MessagesAdded)
	assertions.Equal(1, client.discoverCalls)
	client.chats[2] = Conversation{ID: 2, Type: "group", ParentChatID: 1, RootMessageID: 1}
	client.messages[2] = []Message{syntheticMessage(2, 1, "new child")}
	second, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, second.MessagesAdded)
	assertions.Equal(2, client.discoverCalls)
	archivedMessage(t, imp.store, first.SourceID, 2, 1)
	client.chats[3] = Conversation{ID: 3, Type: "dm"}
	client.messages[3] = []Message{syntheticMessage(3, 1, "outside explicit selection")}
	opts.ChatIDs = []int64{1}
	third, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Zero(third.MessagesAdded)
	assertions.Equal(2, client.discoverCalls, "explicit selection does not broaden discovery")
	rows, err := imp.store.MessageExistsBatch(first.SourceID, []string{messageKey(3, 1)})
	requires.NoError(err)
	assertions.Empty(rows)
}

func TestDefaultDiscoveryAcceptsAuthoritativeEmptyCatalogAndRejectsInvalidIdentity(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	opts.ChatIDs = nil
	client.chats = map[int64]Conversation{}
	summary, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Zero(summary.MessagesProcessed)
	assertions.Zero(summary.ConversationsProcessed)
	for _, catalog := range [][]Conversation{{{ID: 0}}, {{ID: 1}, {ID: 1}}} {
		client.discover = func(context.Context) ([]Conversation, error) { return catalog, nil }
		_, err = imp.Import(t.Context(), opts)
		requires.ErrorContains(err, "invalid or repeated chat identity")
	}
}

func TestInlineIDsScopeCollidingMessagesToSelectedChats(t *testing.T) {
	imp, client, opts := importerFixture(t)
	client.chats[2] = Conversation{ID: 2, Type: "group"}
	client.messages[1] = []Message{syntheticMessage(1, 1, "first chat")}
	client.messages[2] = []Message{syntheticMessage(2, 1, "second chat")}
	opts.ChatIDs = []int64{1, 2}
	summary, err := imp.Import(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.MessagesAdded)
	assert.NotEqual(t, archivedMessage(t, imp.store, summary.SourceID, 1, 1), archivedMessage(t, imp.store, summary.SourceID, 2, 1))
}

func TestPausedRepairDoesNotBlockFullRepairOfNewSelection(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	client.messages[1] = []Message{syntheticMessage(1, 2, "a2"), syntheticMessage(1, 1, "a1")}
	client.chats[2] = Conversation{ID: 2, Type: "dm"}
	client.messages[2] = []Message{syntheticMessage(2, 2, "b2"), syntheticMessage(2, 1, "b1")}
	opts.ChatIDs = []int64{1, 2}
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	opts.ChatIDs = []int64{1}
	opts.Full = true
	opts.Limit = 1
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.True(storedState(t, imp, first.SourceID, opts.Account.Identifier()).chat(1).RepairActive)
	client.messages[2][0].Text = "b2 repaired"
	opts.ChatIDs = []int64{2}
	opts.Limit = 0
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	body, err := imp.store.GetMessageBodyText(archivedMessage(t, imp.store, first.SourceID, 2, 2))
	requires.NoError(err)
	assertions.Equal("b2 repaired", body)
	state := storedState(t, imp, first.SourceID, opts.Account.Identifier())
	assertions.False(state.RepairPending)
	assertions.True(state.chat(1).RepairActive, "paused chat debt retained")
}

func TestFullRepairIncludesAddedScopeWithoutRestartingActiveChat(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	client.messages[1] = []Message{syntheticMessage(1, 3, "a3"), syntheticMessage(1, 2, "a2"), syntheticMessage(1, 1, "a1")}
	client.chats[2] = Conversation{ID: 2, Type: "dm"}
	client.messages[2] = []Message{syntheticMessage(2, 2, "b2"), syntheticMessage(2, 1, "b1")}
	opts.ChatIDs = []int64{1, 2}
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	opts.ChatIDs = []int64{1}
	opts.Full, opts.Limit = true, 1
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	client.messages[2][0].Text = "b2 repaired"
	opts.ChatIDs, opts.Limit = []int64{1, 2}, 0
	summary, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(4, summary.MessagesUpdated, "resume A after its durable cursor and refresh newly selected B")
	body, err := imp.store.GetMessageBodyText(archivedMessage(t, imp.store, first.SourceID, 2, 2))
	requires.NoError(err)
	assertions.Equal("b2 repaired", body)
	state := storedState(t, imp, first.SourceID, opts.Account.Identifier())
	assertions.False(state.RepairPending)
	assertions.Empty(state.RepairScope)
}

func TestAlternateProjectionPreservesRawAndPendingMediaEvidence(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	message := syntheticMessage(1, 1, "original")
	message.Media = []Media{{ID: "photo:9", ChatID: 1, MessageID: 1, MIMEType: "image/jpeg", Role: "photo"}}
	client.messages[1] = []Message{message}
	opts.NoMedia = true
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	id := archivedMessage(t, imp.store, first.SourceID, 1, 1)
	originalRaw, err := imp.store.GetMessageRaw(id)
	requires.NoError(err)
	client.messages[1][0].Text = "new projection"
	client.messages[1][0].Raw = json.RawMessage(`{"id":"1","text":"new projection"}`)
	client.messages[1][0].RawFormat = RawMCPFormat
	client.messages[1][0].Media = nil
	opts.Full = true
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	raw, err := imp.store.GetMessageRaw(id)
	requires.NoError(err)
	assertions.Equal(originalRaw, raw)
	value, err := imp.store.GetMessageMetadata(id)
	requires.NoError(err)
	var metadata messageMetadata
	requires.NoError(json.Unmarshal([]byte(value.String), &metadata))
	assertions.Equal(RawCLIFormat, metadata.RawFormat)
	assertions.Equal(RawMCPFormat, metadata.ProjectionFormat)
	requires.Len(metadata.Media, 1)
	assertions.Equal("photo:9", metadata.Media[0].ID)
	refs, err := imp.store.MessageInlineProviderAttachments(id)
	requires.NoError(err)
	assertions.Contains(refs, "inline:photo:9")
}

func TestFullReactionRemovalAndUnavailableProjectionPreservesSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	message := syntheticMessage(1, 1, "reacted")
	message.Reactions = []Reaction{{UserID: 101, Emoji: "test"}}
	client.messages[1] = []Message{message}
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	id := archivedMessage(t, imp.store, first.SourceID, 1, 1)
	client.messages[1][0].RawFormat = RawMCPFormat
	client.messages[1][0].Reactions = nil
	opts.Full = true
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	var count int
	requires.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`), id).Scan(&count))
	assertions.Equal(1, count)
	client.messages[1][0].RawFormat = RawCLIFormat
	client.messages[1][0].Reactions = []Reaction{}
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	requires.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`), id).Scan(&count))
	assertions.Zero(count)
}

func TestImportUsesReactionTimestampAndKeepsMissingDateUnknown(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	message := syntheticMessage(1, 1, "reacted")
	date := message.SentAt.Add(time.Hour)
	message.Reactions = []Reaction{{UserID: 101, Emoji: "known", CreatedAt: date}, {UserID: 101, Emoji: "unknown"}}
	client.messages[1] = []Message{message}
	summary, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	id := archivedMessage(t, imp.store, summary.SourceID, 1, 1)
	var known, unknown sql.NullTime
	requires.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), id, "known").Scan(&known))
	requires.NoError(imp.store.DB().QueryRow(imp.store.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), id, "unknown").Scan(&unknown))
	requires.True(known.Valid)
	assertions.Equal(date, known.Time)
	assertions.False(unknown.Valid)
}

func TestValidatePageRejectsIdentityAndCursorMismatch(t *testing.T) {
	message := syntheticMessage(1, 3, "valid")
	for _, page := range []Page{{Messages: []Message{message, message}}, {Messages: []Message{message}, HasMore: true, NextBeforeID: 2}, {HasMore: true, NextBeforeID: 2}, {Messages: []Message{syntheticMessage(2, 3, "wrong chat")}}, {Messages: []Message{message}, NextBeforeID: 3}} {
		require.Error(t, validatePage(page, 1, 0))
	}
	require.Error(t, validatePage(Page{Messages: []Message{message}}, 1, 3))
	require.NoError(t, validatePage(Page{Messages: []Message{message}, HasMore: true, NextBeforeID: 3}, 1, 0))
}

func TestLoadSyncStateRejectsForeignAccountAndMalformedCursor(t *testing.T) {
	_, err := LoadSyncState(`{"account":"api.inline.chat:user:100","chats":{}}`, "api.inline.chat:user:99")
	require.Error(t, err)
	_, err = LoadSyncState(`{"account":"api.inline.chat:user:99","chats":{"chat:1":{"history_before":-1}}}`, "api.inline.chat:user:99")
	require.Error(t, err)
	_, err = LoadSyncState(`{"account":"api.inline.chat:user:99","chats":{"chat:1":null}}`, "api.inline.chat:user:99")
	require.Error(t, err)
}
