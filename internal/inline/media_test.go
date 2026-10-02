package inline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

type mediaRoundTripper func(*http.Request) (*http.Response, error)

func (f mediaRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMediaURLRestrictsOriginsAndRefusesRedirectsWithoutCredentials(t *testing.T) {
	assertions := assert.New(t)

	for _, url := range []string{
		"https://api.inline.chat/file?signature=synthetic", "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/key?signature=synthetic", "https://bucket.0123456789abcdef0123456789abcdef.eu.r2.cloudflarestorage.com/key",
	} {
		_, err := mediaURL(url)
		assertions.NoError(err)
	}
	for _, url := range []string{"http://api.inline.chat/file", "https://api.inline.chat/other", "https://api.inline.chat:443/file", "https://user@api.inline.chat/file", "https://api.inline.chat.evil.example/file", "https://127.0.0.1/file", "https://r2.cloudflarestorage.com/file", "https://example.com/file", "https://api.inline.chat/file#fragment"} {
		_, err := mediaURL(url)
		require.Error(t, err, url)
	}
	imp := NewImporter(nil, nil)
	calls := 0
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		assert.Empty(t, request.Header.Get("Authorization"))
		assert.Empty(t, request.Header.Get("Cookie"))
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://api.inline.chat/file?another=synthetic"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	_, err := imp.download(t.Context(), "https://api.inline.chat/file?signature=synthetic", 100)
	require.Error(t, err)
	assertions.Equal(1, calls)
}

func TestMediaDownloadCapsDeclaredAndStreamedBytes(t *testing.T) {
	for _, declared := range []int64{-1, 100} {
		t.Run(fmt.Sprintf("declared_%d", declared), func(t *testing.T) {
			imp := NewImporter(nil, nil)
			imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: declared, Body: io.NopCloser(strings.NewReader("too long")), Request: request}, nil
			})
			_, err := imp.download(t.Context(), "https://api.inline.chat/file", 3)
			require.ErrorIs(t, err, errMediaTooLarge)
		})
	}
	imp := NewImporter(nil, nil)
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("bounded")), Request: request}, nil
	})
	content, err := imp.download(t.Context(), "https://api.inline.chat/file", math.MaxInt64)
	require.NoError(t, err)
	assert.Equal(t, "bounded", string(content), "maximum cap must not overflow the bounded reader")
}

func TestDeferredMediaBackfillRefreshesURLAndPreservesDuplicateOccurrences(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	message := syntheticMessage(1, 1, "")
	message.Media = []Media{{ID: "photo:9", ChatID: 1, MessageID: 1, Role: "photo", URL: "https://api.inline.chat/file?expired=synthetic"}, {ID: "photo:10", ChatID: 1, MessageID: 1, Role: "photo"}}
	client.messages[1] = []Message{message}
	opts.NoMedia = true
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(2, first.AttachmentsPending)
	assertions.Zero(client.filesCalls)
	id := archivedMessage(t, imp.store, first.SourceID, 1, 1)
	refs, err := imp.store.MessageInlineProviderAttachments(id)
	requires.NoError(err)
	requires.Len(refs, 2)
	for _, ref := range refs {
		assertions.Equal(attachmentpolicy.StatePending, ref.State)
		assertions.Equal("image", ref.MediaType)
	}
	client.files = func(_ context.Context, chatID int64, ids []int64) ([]Media, error) {
		assert.Equal(t, int64(1), chatID)
		assert.Equal(t, []int64{1}, ids)
		fresh := []Media{}
		for _, media := range message.Media {
			media.URL = "https://api.inline.chat/file?fresh=synthetic"
			fresh = append(fresh, media)
		}
		return fresh, nil
	}
	calls := 0
	content := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, "synthetic", request.URL.Query().Get("fresh"))
		assert.Empty(t, request.Header.Get("Authorization"))
		assert.Empty(t, request.Header.Get("Cookie"))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(content)), ContentLength: int64(len(content)), Request: request}, nil
	})
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(2, backfill.AttachmentsDownloaded)
	assertions.Equal(2, calls)
	refs, err = imp.store.MessageInlineProviderAttachments(id)
	requires.NoError(err)
	requires.Len(refs, 2)
	assertions.Equal(refs["inline:photo:9"].ContentHash, refs["inline:photo:10"].ContentHash)
	assertions.NotEmpty(refs["inline:photo:9"].ContentHash)
	assertions.Equal("image/png", refs["inline:photo:9"].MimeType)
	state := storedState(t, imp, first.SourceID, opts.Account.Identifier())
	assertions.Equal(int64(1), state.chat(1).Head, "media run cannot overwrite message-history resume state")
	_, err = imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(2, calls, "stored occurrences never re-download")
}

func TestMediaFailureLeavesDurableMarkerAndPolicyCanRetry(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	message := syntheticMessage(1, 1, "media")
	message.Media = []Media{{ID: "document:9", ChatID: 1, MessageID: 1, Role: "document", Size: 6, URL: "https://api.inline.chat/file"}}
	client.messages[1] = []Message{message}
	opts.MediaPolicy.MaxBytes = 3
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, first.AttachmentsSkipped)
	assertions.Zero(client.filesCalls)
	id := archivedMessage(t, imp.store, first.SourceID, 1, 1)
	refs, err := imp.store.MessageInlineProviderAttachments(id)
	requires.NoError(err)
	assertions.Equal(attachmentpolicy.SkipSizeCap, refs["inline:document:9"].SkipReason)
	opts.MediaPolicy.MaxBytes = 10
	client.files = func(context.Context, int64, []int64) ([]Media, error) {
		return nil, errors.New("temporary refresh failure")
	}
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, backfill.Errors)
	refs, err = imp.store.MessageInlineProviderAttachments(id)
	requires.NoError(err)
	assertions.Equal(attachmentpolicy.StateFailed, refs["inline:document:9"].State)
	client.files = nil
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("bytes!")), Request: request}, nil
	})
	backfill, err = imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, backfill.AttachmentsDownloaded)
}

func TestUnknownRosterFailsClosedThenBackfillRechecksAuthoritativeCount(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	client.chats[1] = Conversation{ID: 1, Type: "group", MemberCountKnown: false}
	message := syntheticMessage(1, 1, "media")
	message.Media = []Media{{ID: "document:1", ChatID: 1, MessageID: 1, Role: "document", URL: "https://api.inline.chat/file"}}
	client.messages[1] = []Message{message}
	opts.MediaPolicy.MaxParticipants = 20
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, first.AttachmentsSkipped)
	assertions.Zero(client.filesCalls)
	client.chats[1] = Conversation{ID: 1, Type: "group", MemberCountKnown: true, MemberCount: 2}
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("content")), Request: request}, nil
	})
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, backfill.AttachmentsDownloaded)
}

func TestUnknownRosterDoesNotOverflowMaximumParticipantCap(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	client.chats[1] = Conversation{ID: 1, Type: "group", MemberCountKnown: false}
	message := syntheticMessage(1, 1, "media")
	message.Media = []Media{{ID: "document:1", ChatID: 1, MessageID: 1, Role: "document", URL: "https://api.inline.chat/file"}}
	client.messages[1] = []Message{message}
	opts.MediaPolicy.MaxParticipants = int(^uint(0) >> 1)
	summary, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, summary.AttachmentsSkipped)
	assertions.Zero(client.filesCalls)
	refs, err := imp.store.MessageInlineProviderAttachments(archivedMessage(t, imp.store, summary.SourceID, 1, 1))
	requires.NoError(err)
	assertions.Equal(attachmentpolicy.SkipParticipantThreshold, refs["inline:document:1"].SkipReason)
}

func TestMediaBackfillOnlyContactsSelectedChats(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	client.chats[2] = Conversation{ID: 2, Type: "dm", MemberCountKnown: true, MemberCount: 2}
	for _, chatID := range []int64{1, 2} {
		message := syntheticMessage(chatID, 1, "media")
		message.Media = []Media{{ID: "document:1", ChatID: chatID, MessageID: 1, Role: "document", URL: "https://api.inline.chat/file"}}
		client.messages[chatID] = []Message{message}
	}
	opts.ChatIDs = []int64{1, 2}
	opts.NoMedia = true
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	opts.ChatIDs = []int64{2}
	client.files = func(_ context.Context, chatID int64, ids []int64) ([]Media, error) {
		assert.Equal(t, int64(2), chatID)
		return client.messages[2][0].Media, nil
	}
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("selected")), Request: request}, nil
	})
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, backfill.AttachmentsDownloaded)
	refs, err := imp.store.MessageInlineProviderAttachments(archivedMessage(t, imp.store, first.SourceID, 1, 1))
	requires.NoError(err)
	assertions.Equal(attachmentpolicy.StatePending, refs["inline:document:1"].State)
}

func TestMessageMediaIdentityMismatchNeverAdvancesCursor(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	message := syntheticMessage(1, 1, "invalid media")
	message.Media = []Media{{ID: "document:1", ChatID: 1, MessageID: 2}}
	client.messages[1] = []Message{message}
	summary, err := imp.Import(t.Context(), opts)
	requires.Error(err)
	assertions.Zero(summary.MessagesProcessed)
	rows, err := imp.store.MessageExistsBatch(summary.SourceID, []string{messageKey(1, 1)})
	requires.NoError(err)
	assertions.Empty(rows)
	assertions.Zero(storedState(t, imp, summary.SourceID, opts.Account.Identifier()).chat(1).HistoryBefore)
}

func TestCrossProjectionMetadataKeepsUnsignedOccurrenceIdentity(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	media := Media{ID: "photo:1", ChatID: 1, MessageID: 1, Role: "photo", URL: "https://api.inline.chat/file?signature=synthetic"}
	ref, err := mediaReference(media)
	requires.NoError(err)
	assertions.Equal("inline:photo:1", ref.SourcePartKey)
	assertions.Equal("inline:pending:photo:1", ref.StoragePath)
	var stored Media
	requires.NoError(json.Unmarshal([]byte(ref.Metadata), &stored))
	assertions.Equal(media.ID, stored.ID)
}
