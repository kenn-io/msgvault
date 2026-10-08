package inline

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/testutil"
)

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
