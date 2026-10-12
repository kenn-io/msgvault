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
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

type mediaRoundTripper func(*http.Request) (*http.Response, error)

func (f mediaRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMediaURLRestrictsOriginsAndRefusesRedirectsWithoutCredentials(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	for _, url := range []string{
		"https://api.inline.chat/file?signature=synthetic", "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/key?signature=synthetic", "https://bucket.0123456789abcdef0123456789abcdef.eu.r2.cloudflarestorage.com/key",
	} {
		_, err := mediaURL(url)
		requires.NoError(err)
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

func TestMediaDownloadTimeoutScalesWithPolicyCap(t *testing.T) {
	for _, tc := range []struct {
		name          string
		maxBytes      int64
		downloadTime  time.Duration
		callerTimeout time.Duration
		wantElapsed   time.Duration
		wantError     bool
	}{
		{"small cap remains bounded", 1 << 20, 11 * time.Minute, 0, 10 * time.Minute, true},
		{"default cap permits slow downloads", attachmentpolicy.DefaultChatMaxBytes, 11 * time.Minute, 0, 11 * time.Minute, false},
		{"larger cap permits longer downloads", 1 << 30, 40 * time.Minute, 0, 40 * time.Minute, false},
		{"caller deadline still applies", 1 << 30, 11 * time.Minute, 2 * time.Minute, 2 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A virtual slow connection exercises the real HTTP client's deadline
			// without downloading large files or waiting for real-time timeouts.
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.callerTimeout)
					defer cancel()
				}
				imp := NewImporter(nil, nil)
				imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
					timer := time.NewTimer(tc.downloadTime)
					defer timer.Stop()
					select {
					case <-request.Context().Done():
						return nil, request.Context().Err()
					case <-timer.C:
						return &http.Response{StatusCode: http.StatusOK, ContentLength: 6,
							Body: io.NopCloser(strings.NewReader("bytes!")), Request: request}, nil
					}
				})
				started := time.Now()
				content, err := imp.download(ctx, "https://api.inline.chat/file", tc.maxBytes)
				if tc.wantError {
					require.Error(t, err)
					assert.Empty(t, content)
				} else {
					require.NoError(t, err)
					assert.Equal(t, "bytes!", string(content))
				}
				assert.Equal(t, tc.wantElapsed, time.Since(started))
			})
		})
	}
}

func TestDeferredMediaBackfillRefreshesURLAndPreservesDuplicateOccurrences(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	imp, client, opts := importerFixture(t)
	for id := int64(1); id <= 2; id++ {
		message := syntheticMessage(1, id, "")
		message.Media = []Media{{ID: fmt.Sprintf("photo:%d", id+8), ChatID: 1, MessageID: id, Role: "photo", URL: "https://api.inline.chat/file?expired=synthetic"}}
		client.messages[1] = append(client.messages[1], message)
	}
	opts.NoMedia = true
	first, err := imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(2, first.AttachmentsPending)
	assertions.Zero(client.filesCalls)
	ids := []int64{archivedMessage(t, imp.store, first.SourceID, 1, 1), archivedMessage(t, imp.store, first.SourceID, 1, 2)}
	assertions.NotEqual(ids[0], ids[1])
	for _, id := range ids {
		hasAttachments, attachmentCount := storedMediaMessageStats(t, imp, id)
		assertions.True(hasAttachments)
		assertions.Equal(1, attachmentCount)
		refs, err := imp.store.MessageInlineProviderAttachments(id)
		requires.NoError(err)
		requires.Len(refs, 1)
		for _, ref := range refs {
			assertions.Equal(attachmentpolicy.StatePending, ref.State)
			assertions.Equal("image", ref.MediaType)
		}
	}
	client.files = func(_ context.Context, chatID int64, ids []int64) ([]Media, error) {
		assert.Equal(t, int64(1), chatID)
		require.Len(t, ids, 1)
		require.Contains(t, []int64{1, 2}, ids[0])
		media := client.messages[1][ids[0]-1].Media[0]
		media.URL = "https://api.inline.chat/file?fresh=synthetic"
		return []Media{media}, nil
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
	var hashes []string
	for index, id := range ids {
		hasAttachments, attachmentCount := storedMediaMessageStats(t, imp, id)
		assertions.True(hasAttachments)
		assertions.Equal(1, attachmentCount)
		refs, err := imp.store.MessageInlineProviderAttachments(id)
		requires.NoError(err)
		requires.Len(refs, 1)
		ref := refs[fmt.Sprintf("inline:photo:%d", index+9)]
		assertions.NotEmpty(ref.ContentHash)
		assertions.Equal("image/png", ref.MimeType)
		hashes = append(hashes, ref.ContentHash)
	}
	assertions.Equal(hashes[0], hashes[1])
	state := storedState(t, imp, first.SourceID, opts.Account.Identifier())
	assertions.Equal(int64(2), state.chat(1).Head, "media run cannot overwrite message-history resume state")
	_, err = imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(2, calls, "stored occurrences never re-download")
	opts.Full = true
	for index := range client.messages[1] {
		client.messages[1][index].Media = nil
	}
	_, err = imp.Import(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(2, calls, "resync preserves stored occurrences without downloads")
	for _, id := range ids {
		hasAttachments, attachmentCount := storedMediaMessageStats(t, imp, id)
		assertions.True(hasAttachments)
		assertions.Equal(1, attachmentCount)
	}
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

func TestUnknownRosterFailsClosedThenBackfillRetriesAfterRemovingParticipantCap(t *testing.T) {
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
	refs, err := imp.store.MessageInlineProviderAttachments(archivedMessage(t, imp.store, first.SourceID, 1, 1))
	requires.NoError(err)
	assertions.Equal(attachmentpolicy.SkipParticipantThreshold, refs["inline:document:1"].SkipReason)
	opts.MediaPolicy.MaxParticipants = 0
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("content")), Request: request}, nil
	})
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	requires.NoError(err)
	assertions.Equal(1, backfill.AttachmentsDownloaded)
	assertions.Equal(1, client.filesCalls)
	assertions.False(client.chats[1].MemberCountKnown, "group membership remains unknown after the policy change")
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

func storedMediaMessageStats(t *testing.T, imp *Importer, messageID int64) (hasAttachments bool, attachmentCount int) {
	t.Helper()
	require.NoError(t, imp.store.DB().QueryRow(
		imp.store.Rebind(`SELECT has_attachments, attachment_count FROM messages WHERE id = ?`), messageID,
	).Scan(&hasAttachments, &attachmentCount))
	return hasAttachments, attachmentCount
}

func TestMediaImportPersistsAttachmentStatsForListingsAndSearch(t *testing.T) {
	for _, state := range []attachmentpolicy.DownloadState{
		attachmentpolicy.StatePending, attachmentpolicy.StateSkipped,
		attachmentpolicy.StateFailed, attachmentpolicy.StateStored,
		attachmentpolicy.StateUnavailable,
	} {
		t.Run(string(state), func(t *testing.T) {
			assertions := assert.New(t)
			requires := require.New(t)
			imp, client, opts := importerFixture(t)
			message := syntheticMessage(1, 1, "media")
			message.Media = []Media{{ID: "document:1", ChatID: 1, MessageID: 1, Role: "document", Size: 6, URL: "https://api.inline.chat/file"}}
			client.messages[1] = []Message{message, syntheticMessage(1, 2, "text only")}
			switch state {
			case attachmentpolicy.StatePending:
				opts.NoMedia = true
			case attachmentpolicy.StateSkipped:
				opts.MediaPolicy.MaxBytes = 1
			case attachmentpolicy.StateFailed:
				client.files = func(context.Context, int64, []int64) ([]Media, error) {
					return nil, errors.New("temporary refresh failure")
				}
			case attachmentpolicy.StateStored:
				imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("bytes!")), ContentLength: 6, Request: request}, nil
				})
			case attachmentpolicy.StateUnavailable:
				client.files = func(context.Context, int64, []int64) ([]Media, error) {
					return nil, errors.New("unexpected terminal media retry")
				}
				imp.mediaTransport = mediaRoundTripper(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("unexpected terminal media download")
				})
				// Preserve a terminal marker from an existing archive without
				// claiming a new permanent-unavailability source response.
				opts.NoMedia = true
				seed, err := imp.Import(t.Context(), opts)
				requires.NoError(err)
				id := archivedMessage(t, imp.store, seed.SourceID, 1, 1)
				refs, err := imp.store.MessageInlineProviderAttachments(id)
				requires.NoError(err)
				requires.Contains(refs, "inline:document:1")
				ref := refs["inline:document:1"]
				ref.State = attachmentpolicy.StateUnavailable
				ref.SkipReason = attachmentpolicy.SkipSourceUnavailable
				refs[ref.SourceAttachmentID] = ref
				requires.NoError(imp.store.ReplaceMessageInlineProviderAttachments(id, refsSlice(refs)))
				// Older archives can retain incorrect statistics from before
				// Inline replacement recomputed them; full replay repairs them.
				_, err = imp.store.DB().Exec(imp.store.Rebind(`UPDATE messages SET has_attachments = FALSE, attachment_count = 0 WHERE id = ?`), id)
				requires.NoError(err)
				opts.NoMedia = false
				opts.Full = true
			}
			summary, err := imp.Import(t.Context(), opts)
			requires.NoError(err)
			id := archivedMessage(t, imp.store, summary.SourceID, 1, 1)
			hasAttachments, attachmentCount := storedMediaMessageStats(t, imp, id)
			assertions.True(hasAttachments)
			assertions.Equal(1, attachmentCount)
			hasAttachments, attachmentCount = storedMediaMessageStats(t, imp, archivedMessage(t, imp.store, summary.SourceID, 1, 2))
			assertions.False(hasAttachments)
			assertions.Zero(attachmentCount)
			refs, err := imp.store.MessageInlineProviderAttachments(id)
			requires.NoError(err)
			assertions.Equal(state, refs["inline:document:1"].State)
			engine := query.NewEngine(imp.store.DB(), imp.store.IsPostgreSQL())
			results, err := engine.Search(t.Context(), search.Parse("has:attachment"), 10, 0)
			requires.NoError(err)
			requires.Len(results, 1)
			assertions.Equal(id, results[0].ID)
			assertions.True(results[0].HasAttachments)
			assertions.Equal(1, results[0].AttachmentCount)
			if state == attachmentpolicy.StateUnavailable {
				_, err = imp.BackfillMedia(t.Context(), opts)
				requires.NoError(err)
				assertions.Zero(client.filesCalls, "terminal unavailable occurrences must not retry")
			}
		})
	}
}
