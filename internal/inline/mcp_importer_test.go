package inline

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type archivedMCPReaction struct {
	UserIdentifier string
	Type           string
	Emoji          string
	CreatedAt      sql.NullTime
}

func readMCPReactionSnapshot(t *testing.T, st *store.Store, messageID int64) []archivedMCPReaction {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`SELECT pi.identifier_value, r.reaction_type, r.reaction_value, r.created_at
		FROM reactions r JOIN participant_identifiers pi ON pi.participant_id = r.participant_id
		WHERE r.message_id = ? AND pi.identifier_type = ? ORDER BY pi.identifier_value, r.reaction_value`), messageID, SourceType)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, rows.Close()) })
	snapshot := []archivedMCPReaction{}
	for rows.Next() {
		var reaction archivedMCPReaction
		require.NoError(t, rows.Scan(&reaction.UserIdentifier, &reaction.Type, &reaction.Emoji, &reaction.CreatedAt))
		if reaction.CreatedAt.Valid {
			reaction.CreatedAt.Time = reaction.CreatedAt.Time.UTC()
		}
		snapshot = append(snapshot, reaction)
	}
	require.NoError(t, rows.Err())
	return snapshot
}

func setMCPReactionHistory(f *inlineMCPFixture, chatID int64, base, suffix string) {
	message := strings.TrimSuffix(base, "}") + suffix + `}`
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payloads["messages.list"] = fmt.Sprintf(`{"chat":{"chatId":"%d"},"nextOffsetId":null,"messages":[%s]}`, chatID, message)
}

func TestMCPImportPersistsAndReplacesReactionSnapshots(t *testing.T) {
	for _, structured := range []bool{true, false} {
		t.Run(fmt.Sprintf("structured_%t", structured), func(t *testing.T) {
			checks := assert.New(t)
			requires := require.New(t)
			f := newInlineMCPFixture(t, map[string]string{
				"conversations.get": `{"chat":{"chatId":"7","title":"Example chat","kind":"space_chat"},"details":{},"participants":[]}`,
			}, structured)
			base := inlineMessageJSON(t, 5, "complete", "")
			setMCPReactionHistory(f, 7, base, `,"reactions":[{"userId":"2","emoji":"👍"}]`)
			client := connectInlineFixture(t, f)
			account, err := client.Me(t.Context())
			requires.NoError(err)
			st := testutil.NewTestStore(t)
			_, err = st.GetOrCreateSource(SourceType, account.Identifier())
			requires.NoError(err)
			imp := NewImporter(st, client)
			opts := ImportOptions{Account: account, ChatIDs: []int64{7}, AttachmentsDir: t.TempDir()}
			first, err := imp.Import(t.Context(), opts)
			requires.NoError(err)
			checks.Equal(1, first.MessagesAdded)
			messageID := archivedMessage(t, st, first.SourceID, 7, 5)
			one := []archivedMCPReaction{{UserIdentifier: "api.inline.chat:user:2", Type: "emoji", Emoji: "👍"}}
			two := append(append([]archivedMCPReaction{}, one...), archivedMCPReaction{UserIdentifier: "api.inline.chat:user:3", Type: "emoji", Emoji: "👀"})
			checks.Equal(one, readMCPReactionSnapshot(t, st, messageID))

			populated := `,"reactions":[{"userId":"2","emoji":"👍"},{"userId":"3","emoji":"👀"},{"userId":"3","emoji":"👀"}]`
			opts.Full = true
			for _, step := range []struct {
				name, suffix string
				want         []archivedMCPReaction
			}{
				{"omitted projection", "", one},
				{"null projection", `,"reactions":null`, one},
				{"populated replacement", populated, two},
				{"repeated snapshot", populated, two},
				{"empty replacement", `,"reactions":[]`, []archivedMCPReaction{}},
			} {
				t.Run(step.name, func(t *testing.T) {
					checks := assert.New(t)
					requires := require.New(t)
					setMCPReactionHistory(f, 7, base, step.suffix)
					summary, err := imp.Import(t.Context(), opts)
					requires.NoError(err)
					checks.Equal(1, summary.MessagesUpdated)
					checks.Equal(messageID, archivedMessage(t, st, summary.SourceID, 7, 5))
					checks.Equal(step.want, readMCPReactionSnapshot(t, st, messageID))
				})
			}
		})
	}
}

func TestMCPReactionRefreshRetainsCLIRawEvidence(t *testing.T) {
	requires := require.New(t)
	imp, cli, opts := importerFixture(t)
	message, err := cliDecodeMessage(jsontext.Value(`{"id":1,"chat_id":1,"from_id":101,"date":1700000000,"message":"original","reactions":{"reactions":[{"user_id":101,"emoji":"👍","date":1700000200}]}}`), 1)
	requires.NoError(err)
	cli.messages[1] = []Message{message}
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	messageID := archivedMessage(t, imp.store, first.SourceID, 1, 1)
	originalRaw, err := imp.store.GetMessageRaw(messageID)
	requires.NoError(err)
	known := []archivedMCPReaction{{UserIdentifier: "api.inline.chat:user:101", Type: "emoji", Emoji: "👍", CreatedAt: sql.NullTime{Time: time.Unix(1700000200, 0).UTC(), Valid: true}}}
	assert.Equal(t, known, readMCPReactionSnapshot(t, imp.store, messageID))

	f := newInlineMCPFixture(t, map[string]string{
		"conversations.get": `{"chat":{"chatId":"1","title":"Example chat","kind":"space_chat"},"details":{},"participants":[]}`,
	}, true)
	f.owner.Store(opts.Account.UserID)
	client := connectInlineFixture(t, f)
	imp = NewImporter(imp.store, client)
	opts.Full = true
	base := strings.Replace(inlineMessageJSON(t, 1, "current", ""), `"chatId":"7"`, `"chatId":"1"`, 1)
	unknown := []archivedMCPReaction{{UserIdentifier: "api.inline.chat:user:101", Type: "emoji", Emoji: "👍"}}
	for _, step := range []struct {
		name, suffix string
		want         []archivedMCPReaction
	}{
		{"omitted projection", "", known},
		{"null projection", `,"reactions":null`, known},
		{"current snapshot with unknown date", `,"reactions":[{"userId":"101","emoji":"👍"}]`, unknown},
		{"empty replacement", `,"reactions":[]`, []archivedMCPReaction{}},
	} {
		t.Run(step.name, func(t *testing.T) {
			checks := assert.New(t)
			requires := require.New(t)
			setMCPReactionHistory(f, 1, base, step.suffix)
			_, err := imp.Import(t.Context(), opts)
			requires.NoError(err)
			checks.Equal(step.want, readMCPReactionSnapshot(t, imp.store, messageID))
			raw, err := imp.store.GetMessageRaw(messageID)
			requires.NoError(err)
			checks.Equal(originalRaw, raw)
			value, err := imp.store.GetMessageMetadata(messageID)
			requires.NoError(err)
			var metadata messageMetadata
			requires.NoError(json.Unmarshal([]byte(value.String), &metadata))
			checks.Equal(RawCLIFormat, metadata.RawFormat)
			checks.Equal(RawMCPFormat, metadata.ProjectionFormat)
		})
	}
}

// Exercise failed reconnect verification through real MCP media reads and Store
// writes, including the recoverable media-error path that permits later reads.
func TestMCPMediaRefreshVerificationFailureBlocksFurtherReads(t *testing.T) {
	for _, tc := range []struct {
		name        string
		structured  bool
		accountJSON string
		reject      bool
		expirations int32
	}{
		{name: "tool error", structured: true, reject: true, expirations: 1},
		{name: "malformed tool JSON", accountJSON: `not JSON`, expirations: 1},
		{name: "malformed account", structured: true, accountJSON: `{"user":{"id":2},"session":{"scopes":["messages:read"]}}`, expirations: 1},
		{name: "missing read scope", structured: true, accountJSON: `{"user":{"id":"2"},"session":{"scopes":[]}}`, expirations: 1},
		{name: "expired verification request", structured: true, expirations: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			requires := require.New(t)
			media := `{"kind":"document","id":"11","url":"https://api.inline.chat/file?fresh=synthetic","fileName":"notes.txt","mimeType":"text/plain","sizeBytes":6}`
			message := inlineMessageJSON(t, 5, "original body", media)
			fixture := newInlineMCPFixture(t, map[string]string{
				"conversations.get": `{"chat":{"chatId":"7","kind":"dm","peer":{"userId":"2"}},"details":{}}`,
				"messages.list":     `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[` + message + `]}`,
				"files.get":         `{"chat":{"chatId":"7"},"items":[]}`,
			}, tc.structured)
			client := connectInlineFixture(t, fixture)
			st := testutil.NewTestStore(t)
			account := Account{UserID: 1, Origin: ProductionOrigin}
			_, err := st.GetOrCreateSource(SourceType, account.Identifier())
			requires.NoError(err)
			imp := NewImporter(st, client)
			opts := ImportOptions{Account: account, ChatIDs: []int64{7}, AttachmentsDir: t.TempDir(), NoMedia: true}
			first, err := imp.Import(t.Context(), opts)
			requires.NoError(err)
			requires.Equal(1, first.AttachmentsPending)
			id := archivedMessage(t, st, first.SourceID, 7, 5)
			conversation, err := client.Conversation(t.Context(), 7)
			requires.NoError(err)
			parsed, err := decodeMCPMessage(jsontext.Value(message), 7)
			requires.NoError(err)

			initial := fixture.inits.Load()
			fixture.owner.Store(2)
			fixture.mu.Lock()
			if tc.reject {
				fixture.reject = "account.me"
			}
			fixture.payloads["account.me"] = tc.accountJSON
			fixture.mu.Unlock()
			fixture.missing.Store(tc.expirations)
			opts.NoMedia = false
			summary := &ImportSummary{}
			// Media refresh failures are recoverable, so the caller can proceed
			// to another media or history read after this returns successfully.
			requires.NoError(imp.persistMedia(t.Context(), id, parsed.Text, conversation, parsed.Media, opts, summary))
			checks.Equal(1, summary.Errors)
			checks.Equal(1, summary.AttachmentsPending)
			refs, err := st.MessageInlineProviderAttachments(id)
			requires.NoError(err)
			requires.Contains(refs, "inline:document:11")
			checks.Equal(attachmentpolicy.StateFailed, refs["inline:document:11"].State)
			checks.Empty(refs["inline:document:11"].ContentHash)
			checks.Equal(tc.expirations, fixture.expired.Load())

			// The provider recovers under a different account. The unverified
			// session must stay blocked rather than reading that account's data.
			fixture.mu.Lock()
			fixture.reject = ""
			delete(fixture.payloads, "account.me")
			calls := len(fixture.calls)
			fixture.mu.Unlock()
			fixture.missing.Store(0)
			_, err = client.Files(t.Context(), 7, []int64{5})
			requires.ErrorIs(err, ErrContract)
			_, err = client.Messages(t.Context(), 7, 0)
			requires.ErrorIs(err, ErrContract)
			checks.Equal(initial+1, fixture.inits.Load(), "a failed verification must not reconnect on later reads")
			fixture.mu.Lock()
			checks.Len(fixture.calls, calls, "later reads must not reach the unverified session")
			fixture.mu.Unlock()
		})
	}
}

// Exercise the SDK transport, provider parser and archive together. The local
// server supplies the external read contract; the importer and Store are real.
func TestMCPImportPersistsDiscoveredHistoryAndBackfill(t *testing.T) {
	for _, structured := range []bool{true, false} {
		t.Run(fmt.Sprintf("structured_%t", structured), func(t *testing.T) {
			checks := assert.New(t)
			requires := require.New(t)
			media := `{"kind":"document","id":"11","url":"https://api.inline.chat/file?fresh=synthetic","fileName":"notes.txt","mimeType":"text/plain","sizeBytes":6}`
			message := inlineMessageJSON(t, 5, "original body", media)
			history := func(message string) string {
				return `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[` + message + `]}`
			}
			file := strings.TrimSuffix(media, "}") + `,"source":"message_media","messageId":"5"}`
			fixture := newInlineMCPFixture(t, map[string]string{
				"conversations.list": `{"sort":"id","nextAfterChatId":null,"items":[{"chatId":"7","kind":"dm"}]}`,
				"conversations.get":  `{"chat":{"chatId":"7","kind":"dm","peer":{"userId":"2"}},"details":{"parentChatId":null,"parentMessageId":null}}`,
				"messages.list":      history(message),
				"files.get":          `{"chat":{"chatId":"7"},"items":[{"message":` + message + `,"files":[` + file + `]}]}`,
			}, structured)
			st := testutil.NewTestStore(t)
			account := Account{UserID: 1, Origin: ProductionOrigin}
			_, err := st.GetOrCreateSource(SourceType, account.Identifier())
			requires.NoError(err)
			imp := NewImporter(st, connectInlineFixture(t, fixture))
			opts := ImportOptions{Account: account, AttachmentsDir: t.TempDir(), NoMedia: true,
				MediaPolicy: attachmentpolicy.Policy{MaxParticipants: 20, MaxBytes: attachmentpolicy.DefaultChatMaxBytes}}
			first, err := imp.Import(t.Context(), opts)
			requires.NoError(err)
			checks.Equal(1, first.MessagesAdded)
			checks.Equal(1, first.AttachmentsPending)
			id := archivedMessage(t, st, first.SourceID, 7, 5)
			body, err := st.GetMessageBodyText(id)
			requires.NoError(err)
			checks.Equal("original body", body)
			before, err := storedState(t, imp, first.SourceID, account.Identifier()).Marshal()
			requires.NoError(err)
			calls := 0
			imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				checks.Empty(request.Header.Get("Authorization"))
				checks.Equal("synthetic", request.URL.Query().Get("fresh"))
				return &http.Response{StatusCode: http.StatusOK, ContentLength: 6,
					Body: io.NopCloser(strings.NewReader("bytes!")), Request: request}, nil
			})
			backfill, err := imp.BackfillMedia(t.Context(), opts)
			requires.NoError(err)
			checks.Equal(1, backfill.AttachmentsDownloaded)
			checks.Equal(1, calls)
			after, err := storedState(t, imp, first.SourceID, account.Identifier()).Marshal()
			requires.NoError(err)
			checks.Equal(before, after, "media work preserves message resume state")
			refs, err := st.MessageInlineProviderAttachments(id)
			requires.NoError(err)
			requires.Contains(refs, "inline:document:11")
			checks.Equal(attachmentpolicy.StateStored, refs["inline:document:11"].State)
			checks.NotEmpty(refs["inline:document:11"].ContentHash)
			second, err := imp.Import(t.Context(), opts)
			requires.NoError(err)
			checks.Zero(second.MessagesProcessed)
			fixture.mu.Lock()
			fixture.payloads["messages.list"] = history(inlineMessageJSON(t, 5, "edited body", media))
			fixture.mu.Unlock()
			opts.Full = true
			full, err := imp.Import(t.Context(), opts)
			requires.NoError(err)
			checks.Equal(1, full.MessagesUpdated)
			checks.Zero(full.MessagesAdded)
			checks.Equal(id, archivedMessage(t, st, first.SourceID, 7, 5))
			body, err = st.GetMessageBodyText(id)
			requires.NoError(err)
			checks.Equal("edited body", body)
			hasAttachments, count := storedMediaMessageStats(t, imp, id)
			checks.True(hasAttachments)
			checks.Equal(1, count)
			checks.Equal(1, calls, "stored media is retained during full refresh")
		})
	}
}
