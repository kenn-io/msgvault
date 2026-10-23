package inline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

func assertInlineMessageSize(t *testing.T, imp *Importer, messageID, wantSize int64) {
	t.Helper()
	assertions := assert.New(t)
	requires := require.New(t)
	message, err := imp.store.GetMessage(messageID)
	requires.NoError(err)
	assertions.Equal(wantSize, message.SizeEstimate)
	engine := query.NewEngine(imp.store.DB(), imp.store.IsPostgreSQL())
	results, err := engine.Search(t.Context(), search.Parse(fmt.Sprintf("message_type:inline larger:%d smaller:%d", wantSize-1, wantSize+1)), 10, 0)
	requires.NoError(err)
	requires.Len(results, 1, "size filters must match the archived message")
	assertions.Equal(messageID, results[0].ID)
	assertions.Equal(wantSize, results[0].SizeEstimate)
}

func assertInlineSizeStats(t *testing.T, imp *Importer, wantMessageCount, wantSize, wantAttachmentSize, wantAttachmentCount int64) {
	t.Helper()
	assertions := assert.New(t)
	engine := query.NewEngine(imp.store.DB(), imp.store.IsPostgreSQL())
	stats, err := engine.GetTotalStats(t.Context(), query.StatsOptions{SearchQuery: "message_type:inline", SearchScope: true})
	require.NoError(t, err)
	assertions.Equal(wantMessageCount, stats.MessageCount)
	assertions.Equal(wantSize, stats.TotalSize)
	assertions.Equal(wantAttachmentSize, stats.AttachmentSize)
	assertions.Equal(wantAttachmentCount, stats.AttachmentCount)
}

func TestMediaSizeTracksDeferredCaptureBackfillAndRetainedReplay(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)
	imp, client, opts := importerFixture(t)
	message := syntheticMessage(1, 1, "note 🧾")
	message.Media = []Media{{ID: "document:1", ChatID: 1, MessageID: 1, Role: "document", Size: 3}}
	second := syntheticMessage(1, 2, "second record")
	second.Media = []Media{{ID: "document:2", ChatID: 1, MessageID: 2, Role: "document", Size: 40}}
	client.messages[1] = []Message{message, second}
	opts.NoMedia = true
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	id := archivedMessage(t, imp.store, first.SourceID, 1, 1)
	secondID := archivedMessage(t, imp.store, first.SourceID, 1, 2)
	assertInlineMessageSize(t, imp, id, int64(len(message.Text))+3)
	assertInlineMessageSize(t, imp, secondID, int64(len(second.Text))+40)
	assertInlineSizeStats(t, imp, 2, int64(len(message.Text)+len(second.Text))+43, 43, 2)

	// A later projection may omit previously captured media while changing
	// the body. The pending occurrence sizes still belong to the archive.
	client.messages[1][0].Media = nil
	client.messages[1][0].Text = "edited note 🧾"
	client.messages[1][1].Media = nil
	client.messages[1][1].Text = "edited record 🧾"
	opts.Full = true
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	bodySize := int64(len(client.messages[1][0].Text))
	secondBodySize := int64(len(client.messages[1][1].Text))
	assertInlineMessageSize(t, imp, id, bodySize+3)
	assertInlineMessageSize(t, imp, secondID, secondBodySize+40)
	assertInlineSizeStats(t, imp, 2, bodySize+secondBodySize+43, 43, 2)
	engine := query.NewEngine(imp.store.DB(), imp.store.IsPostgreSQL())
	results, err := engine.Search(t.Context(), search.Parse("message_type:inline larger:40"), 10, 0)
	requires.NoError(err)
	requires.Len(results, 1)
	assertions.Equal(secondID, results[0].ID)

	content := "same bytes"
	client.files = func(_ context.Context, chatID int64, ids []int64) ([]Media, error) {
		assertions.Equal(int64(1), chatID)
		requires.Len(ids, 1)
		requires.Contains([]int64{1, 2}, ids[0])
		return []Media{{ID: fmt.Sprintf("document:%d", ids[0]), ChatID: 1, MessageID: ids[0], Role: "document", Size: 40 + ids[0]*10, URL: "https://api.inline.chat/file"}}, nil
	}
	calls := 0
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(content)), Body: io.NopCloser(strings.NewReader(content)), Request: request}, nil
	})
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(2, backfill.AttachmentsDownloaded)
	actualSize := int64(len(content))
	assertInlineMessageSize(t, imp, id, bodySize+actualSize)
	assertInlineMessageSize(t, imp, secondID, secondBodySize+actualSize)
	assertInlineSizeStats(t, imp, 2, bodySize+secondBodySize+2*actualSize, 2*actualSize, 2)
	refs, err := imp.store.MessageInlineProviderAttachments(id)
	requires.NoError(err)
	requires.Len(refs, 1)
	secondRefs, err := imp.store.MessageInlineProviderAttachments(secondID)
	requires.NoError(err)
	requires.Len(secondRefs, 1)
	assertions.NotEmpty(refs["inline:document:1"].ContentHash)
	assertions.Equal(refs["inline:document:1"].ContentHash, secondRefs["inline:document:2"].ContentHash,
		"shared blobs still contribute once per attachment occurrence")

	results, err = engine.Search(t.Context(), search.Parse("message_type:inline larger:40"), 10, 0)
	requires.NoError(err)
	assertions.Empty(results, "downloaded bytes replace inaccurate provider size declarations")

	// Stored sizes remain authoritative through repeated full replay, whether
	// the provider lists inflated declarations or stops listing the media.
	client.messages[1][0].Media = []Media{{ID: "document:1", ChatID: 1, MessageID: 1, Role: "document", Size: 500}}
	client.messages[1][1].Media = []Media{{ID: "document:2", ChatID: 1, MessageID: 2, Role: "document", Size: 600}}
	opts.NoMedia = false
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertInlineMessageSize(t, imp, id, bodySize+actualSize)
	assertInlineMessageSize(t, imp, secondID, secondBodySize+actualSize)
	assertInlineSizeStats(t, imp, 2, bodySize+secondBodySize+2*actualSize, 2*actualSize, 2)
	client.messages[1][0].Media = nil
	client.messages[1][1].Media = nil
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertInlineMessageSize(t, imp, id, bodySize+actualSize)
	assertInlineMessageSize(t, imp, secondID, secondBodySize+actualSize)
	assertInlineSizeStats(t, imp, 2, bodySize+secondBodySize+2*actualSize, 2*actualSize, 2)
	assertions.Equal(2, calls, "replay must retain stored bytes without another download")
}

func TestMediaSizeIncludesPendingExcludedAndFailedOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name          string
		state         attachmentpolicy.DownloadState
		declaredSize  int64
		maxBytes      int64
		noMedia       bool
		refreshFails  bool
		content       string
		wantSize      int64
		contentLength int64
	}{
		{name: "deferred", state: attachmentpolicy.StatePending, declaredSize: 42, noMedia: true, wantSize: 42},
		{name: "policy excluded", state: attachmentpolicy.StateSkipped, declaredSize: 42, maxBytes: 3, wantSize: 42},
		{name: "refresh failed", state: attachmentpolicy.StateFailed, declaredSize: 42, refreshFails: true, wantSize: 42},
		{name: "stream exceeded cap", state: attachmentpolicy.StateSkipped, maxBytes: 3, content: "large bytes", contentLength: -1, wantSize: 4},
		{name: "unknown size downloaded", state: attachmentpolicy.StateStored, content: "actual bytes", contentLength: -1, wantSize: 12},
		{name: "empty download", state: attachmentpolicy.StateStored, declaredSize: 42, wantSize: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requires := require.New(t)
			imp, client, opts := importerFixture(t)
			message := syntheticMessage(1, 1, "media")
			message.Media = []Media{{ID: "document:1", ChatID: 1, MessageID: 1, Role: "document", Size: tc.declaredSize, URL: "https://api.inline.chat/file"}}
			client.messages[1] = []Message{message}
			opts.NoMedia = tc.noMedia
			opts.MediaPolicy.MaxBytes = tc.maxBytes
			if tc.refreshFails {
				client.files = func(context.Context, int64, []int64) ([]Media, error) {
					return nil, errors.New("temporary refresh failure")
				}
			}
			imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: tc.contentLength, Body: io.NopCloser(strings.NewReader(tc.content)), Request: request}, nil
			})
			summary, err := imp.Import(t.Context(), opts)
			requires.NoError(err)
			id := archivedMessage(t, imp.store, summary.SourceID, 1, 1)
			assertInlineMessageSize(t, imp, id, int64(len(message.Text))+tc.wantSize)
			assertInlineSizeStats(t, imp, 1, int64(len(message.Text))+tc.wantSize, tc.wantSize, 1)
			refs, err := imp.store.MessageInlineProviderAttachments(id)
			requires.NoError(err)
			assertions.Equal(tc.state, refs["inline:document:1"].State)
			assertions.Equal(tc.wantSize, int64(refs["inline:document:1"].Size))
		})
	}
}
