package youtube

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"hegel.dev/go/hegel"
)

func archiveFixture(t *testing.T, mismatch bool, late *bool, quota *bool, missing ...*bool) *Client {
	t.Helper()
	return fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
			return
		}
		switch r.URL.Path {
		case "/channels":
			id := "UCfixture"
			if mismatch {
				id = "UCother"
			}
			_, _ = fmt.Fprintf(w, `{"items":[{"id":%q,"snippet":{"title":"Fixture channel"},"contentDetails":{"relatedPlaylists":{"uploads":"UUfixture"}}}]}`, id)
		case "/playlistItems":
			assert.Equal(t, "50", r.URL.Query().Get("maxResults"))
			_, _ = fmt.Fprint(w, `{"items":[{"contentDetails":{"videoId":"oldvideo"}},{"contentDetails":{"videoId":"disabledvideo"}}]}`)
		case "/videos":
			if len(missing) > 0 && *missing[0] && r.URL.Query().Get("id") == "oldvideo" {
				_, _ = fmt.Fprint(w, `{"items":[]}`)
				return
			}
			v := Video{ID: r.URL.Query().Get("id"), Snippet: VideoSnippet{
				ChannelID: "UCfixture", Title: "Fixture video", Description: "Fixture description",
				PublishedAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
			}}
			v.Status.PrivacyStatus = "unlisted"
			assert.NoError(t, json.MarshalWrite(w, page[Video]{Items: []Video{v}}))
		case "/commentThreads":
			if r.URL.Query().Get("videoId") == "newvideo" {
				_, _ = fmt.Fprint(w, `{"items":[]}`)
				return
			}
			if quota != nil && *quota {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"error":{"errors":[{"reason":"quotaExceeded"}]}}`)
				return
			}
			if r.URL.Query().Get("videoId") == "disabledvideo" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"error":{"errors":[{"reason":"commentsDisabled"}]}}`)
				return
			}
			top := Comment{ID: "parent", Snippet: CommentSnippet{VideoID: "oldvideo", TextDisplay: "First comment", AuthorDisplayName: "Same name", ModerationStatus: "published"}}
			top.Snippet.AuthorChannelID.Value = "UCauthorA"
			th := thread{ID: "thread-fixture"}
			th.Snippet.TopLevelComment = top
			th.Snippet.VideoID = "oldvideo"
			th.Snippet.TotalReplyCount = 2
			yes := true
			th.Snippet.CanReply = &yes
			items := []thread{th}
			if late != nil && *late {
				c := top
				c.ID = "later"
				c.Snippet.TextDisplay = "Later comment"
				c.Snippet.AuthorChannelID.Value = "UCauthorB"
				extra := thread{ID: "thread-late"}
				extra.Snippet.TopLevelComment = c
				extra.Snippet.CanReply = &yes
				items = append(items, extra)
			}
			b, err := json.Marshal(page[thread]{Items: items})
			assert.NoError(t, err)
			_, err = w.Write(b)
			assert.NoError(t, err)
		case "/comments":
			assert.Equal(t, "plainText", r.URL.Query().Get("textFormat"))
			assert.Empty(t, r.URL.Query().Get("moderationStatus"))
			id := "parent.reply1"
			status := "heldForReview"
			author := "UCfixture"
			if r.URL.Query().Get("pageToken") == "next" {
				id = "parent.reply2"
				status = "published"
				author = ""
			}
			c := Comment{ID: id, Snippet: CommentSnippet{ParentID: "parent", VideoID: "oldvideo", TextDisplay: id, ModerationStatus: status}}
			c.Snippet.AuthorChannelID.Value = author
			token := "next"
			if r.URL.Query().Get("pageToken") == "next" {
				token = ""
			}
			b, err := json.Marshal(page[Comment]{Items: []Comment{c}, NextPageToken: token})
			assert.NoError(t, err)
			_, err = w.Write(b)
			assert.NoError(t, err)
		default:
			assert.Fail(t, "unexpected fixture route", r.URL.Path)
		}
	})
}

func TestArchiveMismatchStoresNothing(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, archiveFixture(t, true, nil, nil))
	_, err := imp.Register(t.Context(), "UCfixture")
	require.Error(t, err)
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sources`).Scan(&count))
	assert.Zero(t, count)
}

func TestRegisterCompletesWhenChannelTitleConflictsWithSourceSelector(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identifier string
		alias      bool
	}{
		{name: "identifier", identifier: "Fixture channel"},
		{name: "alias", identifier: "existing-source@example.test", alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			other, err := st.GetOrCreateSource("gmail", tc.identifier)
			require.NoError(err)
			if tc.alias {
				alias := "Fixture channel"
				_, err = st.UpdateSourceSettingsContext(t.Context(), other.ID, store.SourceSettingsUpdate{Alias: &alias})
				require.NoError(err)
			}

			imp := NewImporter(st, archiveFixture(t, false, nil, nil))
			source, err := imp.Register(t.Context(), "UCfixture")
			require.NoError(err)
			assert.False(source.DisplayName.Valid, "conflicting title should leave the source's current name unchanged")

			participantID, _, err := st.ParticipantByIdentifierContext(t.Context(), SourceType, SourceIdentifier("UCfixture"))
			require.NoError(err)
			assert.NotZero(participantID, "registration should still create the channel participant")
			identities, err := st.ListAccountIdentitiesContext(t.Context(), source.ID)
			require.NoError(err)
			require.Len(identities, 1)
			assert.Equal(SourceIdentifier("UCfixture"), identities[0].Address)
			assert.Equal("youtube_channel", identities[0].SourceSignal)

			priorName := "Saved YouTube channel"
			require.NoError(st.UpdateSourceDisplayNameContext(t.Context(), source.ID, priorName))
			retried, err := imp.Register(t.Context(), "UCfixture")
			require.NoError(err)
			assert.Equal(source.ID, retried.ID)
			assert.Equal(priorName, retried.DisplayName.String, "conflicting title should retain the saved name")
		})
	}
}

func TestArchiveTinyBudgetsEventuallyFinishAndAppend(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	late := false
	imp := NewImporter(st, archiveFixture(t, false, &late, nil))
	source, err := imp.Register(t.Context(), "UCfixture")
	require.NoError(err)
	for cycle := range 2 {
		done := false
		for range 50 {
			sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 2})
			require.NoError(err)
			if !sum.Partial {
				done = true
				break
			}
		}
		require.True(done, "finite fixture must finish despite one useful request per run")
		rows, err := st.MessageMetadataBatch(t.Context(), source.ID, []string{"parent", "parent.reply1", "parent.reply2", "later"})
		require.NoError(err)
		require.Contains(rows, "parent")
		require.Contains(rows, "parent.reply1")
		require.Contains(rows, "parent.reply2")
		assert.Contains(rows["parent"].Metadata.String, `"moderation_status":"published"`)
		assert.Contains(rows["parent.reply1"].Metadata.String, `"moderation_status":"heldForReview"`)
		mine, err := st.GetMessageIsFromMe(t.Context(), rows["parent.reply1"].ID)
		require.NoError(err)
		assert.True(mine)
		if cycle == 1 {
			require.Contains(rows, "later")
		}
		late = true
	}
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE source_id = ?`, source.ID).Scan(&count))
	assert.Equal(6, count) // Two video roots and four unique comments.
	var disabledVideoMessageCount int
	var disabledVideoLastMessageAt sql.NullTime
	var disabledVideoLastMessagePreview sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), `
		SELECT c.message_count, c.last_message_at, c.last_message_preview FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ? AND m.source_message_id = ?`, source.ID, "video:disabledvideo").Scan(
		&disabledVideoMessageCount, &disabledVideoLastMessageAt, &disabledVideoLastMessagePreview))
	assert.Equal(1, disabledVideoMessageCount)
	assert.True(disabledVideoLastMessageAt.Valid)
	assert.True(disabledVideoLastMessagePreview.Valid)
	var missingAuthor any
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id = 'parent.reply2'`).Scan(&missingAuthor))
	assert.Nil(missingAuthor)
	var a, b int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id = 'parent'`).Scan(&a))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id = 'later'`).Scan(&b))
	assert.NotEqual(a, b)
}

func TestArchivePartialCommentPageRefreshesStatsForEachTouchedPage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	late := true
	imp := NewImporter(st, archiveFixture(t, false, &late, nil))
	source, err := imp.Register(t.Context(), "UCfixture")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TABLE fixture_stats_refreshes (conversation_id INTEGER NOT NULL)`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fixture_stats_refresh AFTER UPDATE OF message_count ON conversations
		BEGIN INSERT INTO fixture_stats_refreshes (conversation_id) VALUES (NEW.id); END`)
	requirements.NoError(err)

	sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 5})
	requirements.NoError(err)
	assertions.True(sum.Partial)
	var refreshes int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM fixture_stats_refreshes`).Scan(&refreshes))
	assertions.Equal(3, refreshes) // The video and both non-empty thread pages fit the five-request budget.
	var messageCount int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT message_count FROM conversations WHERE source_id = ? AND source_conversation_id = ?`, source.ID, "oldvideo").Scan(&messageCount))
	assertions.Equal(3, messageCount)
}

func TestArchiveQuotaRemainsIncompleteAndResumes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	quota := true
	imp := NewImporter(st, archiveFixture(t, false, nil, &quota))
	source, err := imp.Register(t.Context(), "UCfixture")
	requirements.NoError(err)
	sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	requirements.ErrorIs(err, ErrQuota)
	assertions.True(sum.Partial)
	run, err := st.GetLatestSyncContext(t.Context(), source.ID, 0)
	requirements.NoError(err)
	assertions.Equal(int64(2), run.MessagesProcessed)
	assertions.Equal(int64(2), run.MessagesAdded, "both video writes before the quota error must be recorded")
	assertions.Zero(run.MessagesUpdated)
	var completed int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sync_runs WHERE source_id=? AND status='completed'`, source.ID).Scan(&completed))
	assertions.Zero(completed)
	quota = false
	sum, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	requirements.NoError(err)
	assertions.False(sum.Partial)
}

func TestArchiveSyncRunCountsAddedUpdatedAndUnchangedMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	client := archiveFixture(t, false, nil, nil)
	changed := false
	original := client.api.Transport
	if original == nil {
		original = http.DefaultTransport
	}
	client.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(req)
		if err != nil || !changed {
			return resp, err
		}
		var old, replacement string
		switch strings.TrimPrefix(req.URL.Path, "/youtube/v3") {
		case "/videos":
			old, replacement = `"description":"Fixture description"`, `"description":"Changed fixture description"`
		case "/commentThreads":
			old, replacement = `"textDisplay":"First comment"`, `"textDisplay":"Changed first comment"`
		default:
			return resp, nil
		}
		body, err := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		rewritten := strings.ReplaceAll(string(body), old, replacement)
		resp.Body = io.NopCloser(strings.NewReader(rewritten))
		resp.ContentLength = int64(len(rewritten))
		return resp, nil
	})
	imp := NewImporter(st, client)
	source, err := imp.Register(t.Context(), "UCfixture")
	require.NoError(err)

	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	run, err := st.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(err)
	assert.Equal(int64(5), run.MessagesAdded)
	assert.Zero(run.MessagesUpdated)

	videoRows, err := st.MessageMetadataBatch(t.Context(), source.ID, []string{"video:oldvideo"})
	require.NoError(err)
	require.Contains(videoRows, "video:oldvideo")
	normalizedVideoMetadata := `{"privacy_status":"unlisted","video_id":"oldvideo","channel_id":"UCfixture","provider":"youtube"}`
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET metadata = ? WHERE id = ?`, normalizedVideoMetadata, videoRows["video:oldvideo"].ID)
	require.NoError(err)

	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	run, err = st.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(err)
	assert.Zero(run.MessagesAdded)
	assert.Zero(run.MessagesUpdated, "an unchanged replay must not count as updates")
	videoMetadata, err := st.GetMessageMetadataContext(t.Context(), videoRows["video:oldvideo"].ID)
	require.NoError(err)
	assert.Equal(normalizedVideoMetadata, videoMetadata.String, "an unchanged replay must not rewrite normalized metadata")

	changed = true
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	run, err = st.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(err)
	assert.Zero(run.MessagesAdded)
	assert.Equal(int64(3), run.MessagesUpdated, "both video roots and the changed comment must be counted")
}

func TestArchiveDeferredResourcesDoNotBlockNewCycles(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	missing := true
	imp := NewImporter(st, archiveFixture(t, false, nil, nil, &missing))
	source, err := imp.Register(t.Context(), "UCfixture")
	requirements.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	requirements.NoError(err)
	// Persisted retries remain visible, but a new cycle must still discover
	// other uploads. Intercept just discovery to add a new synthetic video.
	original := imp.client.api.Transport
	if original == nil {
		original = http.DefaultTransport
	}
	imp.client.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(req)
		if err == nil && req.URL.Path == "/youtube/v3/playlistItems" {
			requirements.NoError(resp.Body.Close())
			resp.Body = io.NopCloser(strings.NewReader(`{"items":[{"contentDetails":{"videoId":"newvideo"}}]}`))
		}
		return resp, err
	})
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	requirements.NoError(err)
	rows, err := st.MessageMetadataBatch(t.Context(), source.ID, []string{"video:newvideo"})
	requirements.NoError(err)
	requirements.Contains(rows, "video:newvideo")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestArchiveBudgetPartitionProperty(t *testing.T) {
	// Real Store/HTTP state transitions are slower than the pure properties;
	// 30 independent temporary archives supplement their 2,000-case runs.
	hegel.Test(t, func(ht *hegel.T) {
		budget := hegel.Draw(ht, hegel.Integers(2, 30))
		st := testutil.NewTestStore(t)
		imp := NewImporter(st, archiveFixture(t, false, nil, nil))
		src, err := imp.Register(t.Context(), "UCfixture")
		require.NoError(ht, err)
		done := false
		for range 40 {
			sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: budget})
			require.NoError(ht, err)
			if !sum.Partial {
				done = true
				break
			}
		}
		require.True(ht, done)
		rows, err := st.MessageMetadataBatch(t.Context(), src.ID, []string{"video:oldvideo", "video:disabledvideo", "parent", "parent.reply1", "parent.reply2"})
		require.NoError(ht, err)
		assert.Len(ht, rows, 5)
	}, hegel.WithTestCases(30))
}

func TestArchiveMultipleDeferredTinyBudgetFindsNewUpload(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	c := archiveFixture(t, false, nil, nil)
	later := false
	original := c.api.Transport
	if original == nil {
		original = http.DefaultTransport
	}
	c.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(req)
		if err != nil {
			return resp, err
		}
		body := ""
		if req.URL.Path == "/youtube/v3/videos" && (req.URL.Query().Get("id") == "oldvideo" || req.URL.Query().Get("id") == "disabledvideo") {
			body = `{"items":[]}`
		}
		if req.URL.Path == "/youtube/v3/playlistItems" && later {
			body = `{"items":[{"contentDetails":{"videoId":"oldvideo"}},{"contentDetails":{"videoId":"disabledvideo"}},{"contentDetails":{"videoId":"newvideo"}}]}`
		}
		if body != "" {
			requirements.NoError(resp.Body.Close())
			resp.Body = io.NopCloser(strings.NewReader(body))
		}
		return resp, nil
	})
	imp := NewImporter(st, c)
	src, err := imp.Register(t.Context(), "UCfixture")
	requirements.NoError(err)
	for i := range 30 {
		if i == 5 {
			later = true
		}
		_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 2})
		requirements.NoError(err)
	}
	rows, err := st.MessageMetadataBatch(t.Context(), src.ID, []string{"video:newvideo"})
	requirements.NoError(err)
	requirements.Contains(rows, "video:newvideo")
}

func TestForbiddenResourceDoesNotBlockOtherVideos(t *testing.T) {
	for _, kind := range []string{"video", "threads", "replies"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			c := archiveFixture(t, false, nil, nil)
			original := c.api.Transport
			if original == nil {
				original = http.DefaultTransport
			}
			forbidden := true
			c.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				resp, err := original.RoundTrip(req)
				if err != nil {
					return resp, err
				}
				replacement := ""
				if req.URL.Path == "/youtube/v3/playlistItems" {
					replacement = `{"items":[{"contentDetails":{"videoId":"oldvideo"}},{"contentDetails":{"videoId":"newvideo"}}]}`
				}
				if req.URL.Path == "/youtube/v3/commentThreads" && req.URL.Query().Get("videoId") == "newvideo" && req.URL.Query().Get("moderationStatus") == "published" {
					replacement = `{"items":[{"snippet":{"videoId":"newvideo","topLevelComment":{"id":"other-comment","snippet":{"videoId":"newvideo","textDisplay":"Other video"}}}}]}`
				}
				if replacement != "" {
					require.NoError(resp.Body.Close())
					resp.Body = io.NopCloser(strings.NewReader(replacement))
				}
				if forbidden && ((kind == "video" && req.URL.Path == "/youtube/v3/videos" && req.URL.Query().Get("id") == "oldvideo") || (kind == "threads" && req.URL.Path == "/youtube/v3/commentThreads" && req.URL.Query().Get("videoId") == "oldvideo") || (kind == "replies" && req.URL.Path == "/youtube/v3/comments")) {
					require.NoError(resp.Body.Close())
					resp.StatusCode = http.StatusForbidden
					resp.Body = io.NopCloser(strings.NewReader(`{"error":{"errors":[{"reason":"forbidden"}]}}`))
				}
				return resp, nil
			})
			imp := NewImporter(st, c)
			src, err := imp.Register(t.Context(), "UCfixture")
			require.NoError(err)
			_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
			require.NoError(err)
			rows, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"other-comment"})
			require.NoError(err)
			require.Contains(rows, "other-comment")
			forbidden = false
			_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
			require.NoError(err)
			rows, err = st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"parent", "parent.reply1"})
			require.NoError(err)
			require.Contains(rows, "parent")
			require.Contains(rows, "parent.reply1")
		})
	}
}

func TestInvalidPageTokenRestartsListingOnce(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	c := archiveFixture(t, false, nil, nil)
	original := c.api.Transport
	initial := 0
	invalid := 0
	c.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(req)
		if err != nil || req.URL.Path != "/youtube/v3/playlistItems" {
			return resp, err
		}
		require.NoError(resp.Body.Close())
		if req.URL.Query().Get("pageToken") == "expired" {
			invalid++
			resp.StatusCode = http.StatusBadRequest
			resp.Body = io.NopCloser(strings.NewReader(`{"error":{"errors":[{"reason":"invalidPageToken"}]}}`))
		} else {
			initial++
			if initial == 1 {
				resp.Body = io.NopCloser(strings.NewReader(`{"items":[],"nextPageToken":"expired"}`))
			} else {
				resp.Body = io.NopCloser(strings.NewReader(`{"items":[]}`))
			}
		}
		return resp, nil
	})
	imp := NewImporter(st, c)
	_, err := imp.Register(t.Context(), "UCfixture")
	require.NoError(err)
	sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 10})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(2, initial)
	assert.Equal(1, invalid)
}
