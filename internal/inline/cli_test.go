package inline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cliMessageFixture(id int64) string {
	return fmt.Sprintf(`{"id":%d,"chat_id":123,"from_id":9007199254740991,"date":1700000000,"message":" hello 🚀 ","out":true,"edit_date":1700000010,"reply_to_msg_id":1,"reactions":{"reactions":[{"user_id":77,"emoji":"👍"}]},"media":{"media":{"Document":{"document":{"id":55,"file_name":"report.txt","mime_type":"text/plain","size":7,"cdn_url":"https://cdn.example.test/report?signature=private"}}}},"entities":{"entities":[]}}`, id)
}

func TestCLIPageUsesNativeJSONAndPreservesRawMessage(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	rawMessage := cliMessageFixture(7)
	page, err := cliDecodePage([]byte(`{"messages":[`+rawMessage+`],"page":{"fetchedCount":1,"returnedCount":1,"nextOffsetId":7,"filtersAppliedToPage":false}}`), 123, 8)
	requires.NoError(err)
	requires.Len(page.Messages, 1)
	message := page.Messages[0]
	assertions.Equal(int64(9007199254740991), message.SenderID)
	assertions.Equal(" hello 🚀 ", message.Text)
	assertions.Equal(rawMessage, string(message.Raw))
	assertions.Equal(RawCLIFormat, message.RawFormat)
	assertions.Equal(time.Unix(1700000010, 0).UTC(), message.EditedAt)
	assertions.Equal(int64(1), message.ReplyToMessageID)
	assertions.Equal([]Reaction{{UserID: 77, Emoji: "👍", Count: 1}}, message.Reactions)
	assertions.Equal([]Media{{ID: "document:55", ChatID: 123, MessageID: 7, Filename: "report.txt", MIMEType: "text/plain", URL: "https://cdn.example.test/report?signature=private", Size: 7, Role: "document"}}, message.Media)
	assertions.False(page.HasMore)
}

func TestCLILegacyUnfilteredPageDerivesStrictCursor(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	messages := make([]json.RawMessage, cliPageSize)
	for i := range messages {
		messages[i] = json.RawMessage(cliMessageFixture(int64(i + 1)))
	}
	raw, err := json.Marshal(map[string]any{"messages": messages})
	requires.NoError(err)
	page, err := cliDecodePage(raw, 123, 101)
	requires.NoError(err)
	assertions.True(page.HasMore)
	assertions.Equal(int64(1), page.NextBeforeID)
	assertions.Equal(int64(100), page.Messages[0].ID)
	assertions.Equal(int64(1), page.Messages[99].ID)
	_, err = cliDecodePage(raw, 123, 100)
	requires.ErrorContains(err, "advance exclusively")
	page, err = cliDecodePage([]byte(`{"messages":[]}`), 123, 1)
	requires.NoError(err)
	assertions.False(page.HasMore)
	assertions.Zero(page.NextBeforeID)
}

func TestCLIPageRejectsUnsafeOrUnsupportedShapes(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"missing messages", `{}`},
		{"null messages", `{"messages":null}`},
		{"duplicate JSON members", `{"messages":[],"messages":[]}`},
		{"float ID", `{"messages":[{"id":1.5,"chat_id":123,"from_id":7,"date":0}]}`},
		{"exponential ID", `{"messages":[{"id":1e3,"chat_id":123,"from_id":7,"date":0}]}`},
		{"unsafe ID", `{"messages":[{"id":9007199254740992,"chat_id":123,"from_id":7,"date":0}]}`},
		{"wrong chat", `{"messages":[{"id":7,"chat_id":124,"from_id":7,"date":0}]}`},
		{"camel protobuf JSON", `{"messages":[{"id":7,"chatId":123,"fromId":7,"date":0}]}`},
		{"duplicate IDs", `{"messages":[` + cliMessageFixture(7) + `,` + cliMessageFixture(7) + `]}`},
		{"filtered page", `{"messages":[],"page":{"fetchedCount":0,"returnedCount":0,"nextOffsetId":null,"filtersAppliedToPage":true}}`},
		{"inconsistent counts", `{"messages":[],"page":{"fetchedCount":1,"returnedCount":0,"nextOffsetId":null,"filtersAppliedToPage":false}}`},
		{"unknown media", `{"messages":[{"id":7,"chat_id":123,"from_id":7,"date":0,"media":{"media":{"NewType":{}}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) { _, err := cliDecodePage([]byte(test.body), 123, 0); require.Error(t, err) })
	}
}

func TestCLIConversationDistinguishesCanonicalChatAndPeer(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	direct, err := cliDecodeConversation(json.RawMessage(`{"id":123,"title":"","peer_id":{"type":{"User":{"user_id":999}}}}`), 123, 42)
	requires.NoError(err)
	assertions.Equal(int64(123), direct.ID)
	assertions.Equal("direct_chat", direct.Type)
	assertions.True(direct.MemberCountKnown)
	assertions.Equal(2, direct.MemberCount)
	self, err := cliDecodeConversation(json.RawMessage(`{"id":123,"peer_id":{"type":{"User":{"user_id":42}}}}`), 123, 42)
	requires.NoError(err)
	assertions.True(self.MemberCountKnown)
	assertions.Equal(1, self.MemberCount)
	unverified, err := cliDecodeConversation(json.RawMessage(`{"id":123,"peer_id":{"type":{"User":{"user_id":42}}}}`), 123, 0)
	requires.NoError(err)
	assertions.False(unverified.MemberCountKnown)
	child, err := cliDecodeConversation(json.RawMessage(`{"id":123,"title":"Reply","peer_id":{"type":{"Chat":{"chat_id":123}}},"parent_chat_id":100,"parent_message_id":7}`), 123, 42)
	requires.NoError(err)
	assertions.Equal(int64(100), child.ParentChatID)
	assertions.Equal(int64(7), child.RootMessageID)
	assertions.False(child.MemberCountKnown)
	_, err = cliDecodeConversation(json.RawMessage(`{"id":123,"peer_id":{"type":{"Chat":{"chat_id":456}}}}`), 123, 42)
	requires.Error(err)
	_, err = cliDecodeConversation(json.RawMessage(`{"id":123,"peerId":{"chatId":123}}`), 123, 42)
	requires.Error(err)
}

func TestCLICatalogDecodesAllCanonicalChatsAndPreservesRaw(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	direct := `{"id":123,"title":"","peer_id":{"type":{"User":{"user_id":999}}},"future_metadata":{"nested":[1,2]}}`
	child := `{"id":456,"title":"Reply","peer_id":{"type":{"Chat":{"chat_id":456}}},"parent_chat_id":123,"parent_message_id":7}`
	conversations, err := cliDecodeCatalog([]byte(`{"chats":[`+child+`,`+direct+`],"dialogs":[],"spaces":[],"users":[],"messages":[],"folders":[]}`), 42)
	requires.NoError(err)
	requires.Len(conversations, 2)
	assertions.Equal(int64(123), conversations[0].ID)
	assertions.Equal("direct_chat", conversations[0].Type)
	assertions.Equal(direct, string(conversations[0].Raw))
	assertions.Equal(int64(123), conversations[1].ParentChatID)
	assertions.Equal(int64(7), conversations[1].RootMessageID)

	chats := make([]json.RawMessage, cliPageSize+1)
	for i := range chats {
		chats[i] = json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"Fixture","peer_id":{"type":{"Chat":{"chat_id":%d}}}}`, i+1, i+1))
	}
	raw, err := json.Marshal(map[string]any{"chats": chats})
	requires.NoError(err)
	conversations, err = cliDecodeCatalog(raw, 42)
	requires.NoError(err)
	assertions.Len(conversations, cliPageSize+1, "catalog has no message-page cap or client limit")
	conversations, err = cliDecodeCatalog([]byte(`{"chats":[]}`), 42)
	requires.NoError(err)
	assertions.NotNil(conversations)
	assertions.Empty(conversations)
}

func TestCLICatalogRejectsMalformedOrAmbiguousShapes(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"chats":null}`,
		`{"chats":[],"chats":[]}`,
		`{"chats":[null]}`,
		`{"chats":[{"id":1e3}]}`,
		`{"chats":[{"id":9007199254740992}]}`,
		`{"chats":[{"id":123,"peerId":{"chatId":123}}]}`,
		`{"chats":[{"id":123,"peer_id":{"type":{"Chat":{"chat_id":123}}}},{"id":123,"peer_id":{"type":{"Chat":{"chat_id":123}}}}]}`,
	} {
		_, err := cliDecodeCatalog([]byte(raw), 42)
		require.Error(t, err)
	}
}

func TestCLIDiscoverReportsUnsupportedCatalogCommand(t *testing.T) {
	_, err := NewCLIClient(cliProcessFixture(t, "exit 2\n")).Discover(context.Background())
	require.ErrorContains(t, err, "upgrade Inline")
	require.ErrorContains(t, err, "--include-subthreads")
}

func TestCLIReactionSnapshotIncludesAuthoritativeEmptySet(t *testing.T) {
	for _, suffix := range []string{"", `,"reactions":null`, `,"reactions":{"reactions":[]}`} {
		message, err := cliDecodeMessage(json.RawMessage(`{"id":7,"chat_id":123,"from_id":42,"date":0`+suffix+`}`), 123)
		require.NoError(t, err)
		require.NotNil(t, message.Reactions, "empty native snapshots must remove old reactions on re-import")
		assert.Empty(t, message.Reactions)
	}
}

func TestCLIReactionUsesSourceDateAndPreservesUnknownDate(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	message, err := cliDecodeMessage(json.RawMessage(`{"id":7,"chat_id":123,"from_id":42,"date":1700000000,"reactions":{"reactions":[{"user_id":77,"emoji":"👍","date":1700000200},{"user_id":78,"emoji":"🎉"}]}}`), 123)
	requires.NoError(err)
	requires.Len(message.Reactions, 2)
	assertions.Equal(time.Unix(1700000200, 0).UTC(), message.Reactions[0].CreatedAt)
	assertions.True(message.Reactions[1].CreatedAt.IsZero(), "missing dates must not adopt the message timestamp")
	for _, invalid := range []string{"-1", "253402300800", "1.5", `"1700000200"`} {
		_, err := cliDecodeMessage(json.RawMessage(`{"id":7,"chat_id":123,"from_id":42,"date":0,"reactions":{"reactions":[{"user_id":77,"emoji":"👍","date":`+invalid+`}]}}`), 123)
		requires.Error(err)
	}
}

func TestCLIMediaUsesLargestAvailablePhotoAndSeconds(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	photo, err := cliDecodeMedia(map[string]json.RawMessage{"Photo": json.RawMessage(`{"photo":{"id":55,"format":2,"sizes":[{"w":200,"h":200,"size":200,"cdn_url":null},{"w":20,"h":20,"size":20,"cdn_url":"https://cdn.example.test/small"},{"w":100,"h":100,"size":100,"cdn_url":"https://cdn.example.test/big"}]}}`)}, 123, 7)
	requires.NoError(err)
	requires.Len(photo, 1)
	assertions.Equal("https://cdn.example.test/big", photo[0].URL)
	assertions.Equal("image/png", photo[0].MIMEType)
	assertions.Equal(int64(100), photo[0].Size)
	voice, err := cliDecodeMedia(map[string]json.RawMessage{"Voice": json.RawMessage(`{"voice":{"id":56,"duration":3,"size":20,"mime_type":"audio/ogg","cdn_url":"https://cdn.example.test/voice"}}`)}, 123, 8)
	requires.NoError(err)
	assertions.Equal(3000, voice[0].DurationMS)
	assertions.Equal("voice", voice[0].Role)
}

// These executable fixtures exercise the stable subprocess boundary, not an
// internal command stub: actual argv, exit status, bounded pipes and cancellation
// are tested without reading a person's CLI account or contacting production.
func cliProcessFixture(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	path := filepath.Join(t.TempDir(), "inline fixture")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700))
	return path
}

func TestCLIClientExercisesReadCommandsThroughExternalProcess(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	executable := cliProcessFixture(t, `
case "$*" in
  "--version") printf '%s' 'inline 0.7.15' ;;
  "doctor --json --compact") printf '%s' '{"config":{"apiBaseUrl":"https://api.inline.chat/v1","realtimeUrl":"wss://api.inline.chat/realtime"},"paths":{"secretsPath":"discarded"}}' ;;
  "auth me --json --compact") printf '%s' '{"id":42,"first_name":"Example","last_name":"User"}' ;;
  "chats list --include-subthreads --json --compact") printf '%s' '{"chats":[{"id":123,"title":"Fixture","peer_id":{"type":{"Chat":{"chat_id":123}}}}],"dialogs":[],"spaces":[],"users":[],"messages":[],"folders":[]}' ;;
  "chats get --chat-id 123 --json --compact") printf '%s' '{"chat":{"id":123,"title":"Fixture","peer_id":{"type":{"Chat":{"chat_id":123}}}}}' ;;
  "chats get --chat-id 456 --json --compact") printf '%s' '{"chat":{"id":456,"title":"","peer_id":{"type":{"User":{"user_id":42}}}}}' ;;
  "messages list --chat-id 123 --limit 100 --offset-id 8 --json --compact") printf '%s' '{"messages":[{"id":7,"chat_id":123,"from_id":42,"date":1700000000,"message":"fixture"}]}' ;;
  "messages get --chat-id 123 --message-id 7 --json --compact") printf '%s' '{"id":7,"chat_id":123,"from_id":42,"date":1700000000,"media":{"media":{"Voice":{"voice":{"id":56,"duration":3,"size":20,"mime_type":"audio/ogg","cdn_url":"https://cdn.example.test/voice"}}}}}' ;;
  *) printf '%s' 'unexpected read contract' >&2; exit 9 ;;
esac
`)
	client := NewCLIClient(executable)
	account, err := client.Me(context.Background())
	requires.NoError(err)
	assertions.Equal(Account{UserID: 42, Origin: ProductionOrigin, DisplayName: "Example User"}, account)
	conversations, err := client.Discover(context.Background())
	requires.NoError(err)
	requires.Len(conversations, 1)
	assertions.Equal(int64(123), conversations[0].ID)
	conversation, err := client.Conversation(context.Background(), 123)
	requires.NoError(err)
	assertions.Equal("Fixture", conversation.Title)
	self, err := client.Conversation(context.Background(), 456)
	requires.NoError(err)
	assertions.True(self.MemberCountKnown)
	assertions.Equal(1, self.MemberCount)
	page, err := client.Messages(context.Background(), 123, 8)
	requires.NoError(err)
	requires.Len(page.Messages, 1)
	media, err := client.Files(context.Background(), 123, []int64{7})
	requires.NoError(err)
	requires.Len(media, 1)
	assertions.Equal("voice:56", media[0].ID)
}

func TestCLIClientBoundsPipesCancelsAndDoesNotEchoDiagnostics(t *testing.T) {
	t.Run("stdout", func(t *testing.T) {
		client := NewCLIClient(cliProcessFixture(t, "while :; do printf '%s' '0123456789'; done\n"))
		client.maxStdout = 64
		_, err := client.run(context.Background(), "fixture")
		require.ErrorContains(t, err, "bounded response limit")
	})
	t.Run("stderr", func(t *testing.T) {
		client := NewCLIClient(cliProcessFixture(t, "while :; do printf '%s' 'private-account-text' >&2; done\n"))
		client.maxStderr = 64
		_, err := client.run(context.Background(), "fixture")
		require.ErrorContains(t, err, "bounded response limit")
		assert.NotContains(t, err.Error(), "private-account-text")
	})
	t.Run("exit", func(t *testing.T) {
		client := NewCLIClient(cliProcessFixture(t, "printf '%s' 'secret=private-account-text' >&2; exit 9\n"))
		_, err := client.run(context.Background(), "fixture")
		require.ErrorContains(t, err, "status 9")
		assert.NotContains(t, err.Error(), "private-account-text")
	})
	t.Run("cancel", func(t *testing.T) {
		client := NewCLIClient(cliProcessFixture(t, "exec sleep 30\n"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.run(ctx, "fixture")
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("timeout", func(t *testing.T) {
		client := NewCLIClient(cliProcessFixture(t, "exec sleep 30\n"))
		client.timeout = 30 * time.Millisecond
		_, err := client.run(context.Background(), "fixture")
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestCLIClientRejectsUnsupportedVersionAndOrigin(t *testing.T) {
	assertions := assert.New(t)

	for _, test := range []struct{ name, version, origin string }{
		{"version", "different-cli 1.0.0", "https://api.inline.chat/v1"},
		{"origin", "inline 0.7.15", "https://dev.example.test/v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf("case \"$1\" in\n--version) printf '%%s' '%s' ;;\ndoctor) printf '%%s' '{\"config\":{\"apiBaseUrl\":\"%s\",\"realtimeUrl\":\"wss://api.inline.chat/realtime\"}}' ;;\n*) exit 9 ;;\nesac\n", test.version, test.origin)
			client := NewCLIClient(cliProcessFixture(t, body))
			_, err := client.Me(context.Background())
			require.Error(t, err)
		})
	}
	assertions.False(cliProductionEndpoint("https://api.inline.chat@evil.example.test/v1", "https"))
	assertions.False(cliProductionEndpoint("https://api.inline.chat/v1?token=private", "https"))
	assertions.False(cliProductionEndpoint("http://api.inline.chat/v1", "https"))
	assertions.True(cliProductionEndpoint("https://api.inline.chat:443/v1", "https"))
}

func TestCLIFileRefreshRequiresCompleteRequestedSet(t *testing.T) {
	requires := require.New(t)

	fixture := cliProcessFixture(t, "printf '%s' '{\"messages\":[{\"id\":7,\"chat_id\":123,\"from_id\":42,\"date\":0}],\"missingMessageIds\":[8]}'\n")
	_, err := NewCLIClient(fixture).Files(context.Background(), 123, []int64{7, 8})
	requires.ErrorContains(err, "all requested messages")
	_, err = NewCLIClient(fixture).Files(context.Background(), 123, []int64{7, 7})
	requires.ErrorContains(err, "duplicate")
	_, err = NewCLIClient(fixture).Messages(context.Background(), 123, -1)
	requires.ErrorContains(err, "cursor")
	assert.NotContains(t, strings.ToLower(err.Error()), "private")
}
