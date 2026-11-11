package youtube

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	yt "google.golang.org/api/youtube/v3"
)

func archiveFixture(t *testing.T, mismatch bool, late *bool, quota *bool, quotaPath ...string) *Client {
	t.Helper()
	return fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
			return
		}
		quotaEndpoint := "/commentThreads"
		if len(quotaPath) > 0 {
			quotaEndpoint = quotaPath[0]
		}
		if quota != nil && *quota && r.URL.Path == quotaEndpoint {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"error":{"errors":[{"reason":"quotaExceeded"}]}}`)
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
			response := yt.VideoListResponse{}
			for id := range strings.SplitSeq(r.URL.Query().Get("id"), ",") {
				response.Items = append(response.Items, &yt.Video{Id: id, Snippet: &yt.VideoSnippet{ChannelId: "UCfixture", Title: "Fixture video", Description: "Fixture description", PublishedAt: "2026-01-01T00:00:00Z"}, Status: &yt.VideoStatus{PrivacyStatus: "unlisted"}})
			}
			assert.NoError(t, json.MarshalWrite(w, response))

		case "/commentThreads":
			if r.URL.Query().Get("videoId") == "newvideo" {
				_, _ = fmt.Fprint(w, `{"items":[]}`)
				return
			}
			if r.URL.Query().Get("videoId") == "disabledvideo" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprint(w, `{"error":{"errors":[{"reason":"commentsDisabled"}]}}`)
				return
			}
			if r.URL.Query().Get("moderationStatus") != "published" {
				_, _ = fmt.Fprint(w, `{"items":[]}`)
				return
			}
			extra := ""
			if late != nil && *late {
				extra = `,{"id":"thread-late","snippet":{"videoId":"oldvideo","topLevelComment":{"id":"later","snippet":{"videoId":"oldvideo","textDisplay":"Later comment","authorDisplayName":"Same name","authorChannelId":{"value":"UCauthorB"},"moderationStatus":"published"}}}}`
			}
			_, _ = fmt.Fprintf(w, `{"items":[{"id":"thread-fixture","snippet":{"videoId":"oldvideo","totalReplyCount":2,"topLevelComment":{"id":"parent","snippet":{"videoId":"oldvideo","textDisplay":"First comment","authorDisplayName":"Same name","authorChannelId":{"value":"UCauthorA"},"moderationStatus":"published"}}}}%s]}`, extra)

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
			token := "next"
			if r.URL.Query().Get("pageToken") == "next" {
				token = ""
			}
			_, _ = fmt.Fprintf(w, `{"items":[{"id":%q,"snippet":{"parentId":"parent","videoId":"oldvideo","textDisplay":%q,"moderationStatus":%q,"authorChannelId":{"value":%q}}}],"nextPageToken":%q}`, id, id, status, author, token)

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

func TestArchivePreservesNewerCheckpoint(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	client := archiveFixture(t, false, nil, nil)
	imp := NewImporter(st, client)
	source, err := imp.Register(t.Context(), "UCfixture")
	require.NoError(err)
	const saved = `{"version":2,"ownership_cursor":"future-owner","queue":{"kind":"uploads","token":"future-page"},"future_field":"preserve"}`
	run, err := st.StartSyncContext(t.Context(), source.ID, SourceType)
	require.NoError(err)
	require.NoError(st.PauseSyncWithCheckpoint(t.Context(), run, &store.Checkpoint{PageToken: saved}))
	for range 2 {
		_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 2})
		require.ErrorContains(err, "unsupported")
		checkpoint, err := st.GetLatestCheckpointedSyncByType(t.Context(), source.ID, SourceType)
		require.NoError(err)
		require.Equal(saved, checkpoint.CursorBefore.String)
	}
}

func TestArchiveRestartsInvalidCheckpoint(t *testing.T) {
	for _, tc := range []struct{ name, checkpoint string }{
		{"invalid version type", `{"ownership_cursor":"stale-owner","queue":[{"kind":"uploads","token":"stale-page"}],"version":"invalid"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			late := false
			client := archiveFixture(t, false, &late, nil)
			original := client.api.Transport
			client.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasPrefix(req.URL.Query().Get("pageToken"), "stale-") {
					return nil, errors.New("stale checkpoint reached provider")
				}
				return original.RoundTrip(req)
			})
			imp := NewImporter(st, client)
			source, err := imp.Register(t.Context(), "UCfixture")
			require.NoError(err)
			_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
			require.NoError(err)
			ids := []string{"parent", "parent.reply1", "parent.reply2"}
			before, err := st.StoredMessagesContext(t.Context(), source.ID, "youtube_json", ids)
			require.NoError(err)
			require.Len(before, len(ids))
			run, err := st.StartSyncContext(t.Context(), source.ID, SourceType)
			require.NoError(err)
			require.NoError(st.PauseSyncWithCheckpoint(t.Context(), run, &store.Checkpoint{PageToken: tc.checkpoint}))
			late = true
			finished := false
			for range 50 {
				sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 2})
				require.NoError(err)
				require.LessOrEqual(sum.Requests, 2)
				if !sum.Partial {
					finished = true
					break
				}
			}
			require.True(finished)
			after, err := st.StoredMessagesContext(t.Context(), source.ID, "youtube_json", append(ids, "later"))
			require.NoError(err)
			require.Len(after, 4)
			for _, id := range ids {
				require.Equal(before[id].ID, after[id].ID)
			}
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
		if cycle == 1 {
			require.Contains(rows, "later")
		}
		late = true
	}
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE source_id = ?`, source.ID).Scan(&count))
	assert.Equal(6, count) // Two video roots and four unique comments.
}

func TestArchivePartialCommentPageRefreshesStatsForEachTouchedPage(t *testing.T) {
	t.Run("partial page", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		st := testutil.NewSQLiteTestStore(t)
		late := true
		imp := NewImporter(st, archiveFixture(t, false, &late, nil))
		source, err := imp.Register(t.Context(), "UCfixture")
		requirements.NoError(err)

		sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 5})
		requirements.NoError(err)
		assertions.True(sum.Partial)
		var messageCount int
		requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT message_count FROM conversations WHERE source_id = ? AND source_conversation_id = ?`, source.ID, "oldvideo").Scan(&messageCount))
		assertions.Equal(3, messageCount)
	})
	t.Run("partial batch", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewSQLiteTestStore(t)
		c := archiveFixture(t, false, nil, nil)
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_second_video BEFORE INSERT ON messages WHEN NEW.source_message_id = 'video:disabledvideo' BEGIN SELECT RAISE(ABORT, 'fixture write failure'); END`)
		require.NoError(err)
		imp := NewImporter(st, c)
		src, err := imp.Register(t.Context(), "UCfixture")
		require.NoError(err)
		_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 10})
		require.Error(err)
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_count FROM conversations WHERE source_id=? AND source_conversation_id=?`), src.ID, "oldvideo").Scan(&count))
		require.Equal(1, count)
	})
}

func TestArchiveQuotaRemainsIncompleteAndResumes(t *testing.T) {
	for _, tc := range []struct {
		endpoint     string
		expectCounts bool
	}{{"/commentThreads", true}, {"/videos", false}} {
		t.Run(tc.endpoint, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			quota := true
			imp := NewImporter(st, archiveFixture(t, false, nil, &quota, tc.endpoint))
			source, err := imp.Register(t.Context(), "UCfixture")
			require.NoError(err)
			sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 10})
			require.ErrorIs(err, ErrQuota)
			require.True(sum.Partial)
			if tc.expectCounts {
				run, err := st.GetLatestSyncContext(t.Context(), source.ID, 0)
				require.NoError(err)
				require.Equal(int64(2), run.MessagesProcessed)
				require.Equal(int64(2), run.MessagesAdded)
				require.Zero(run.MessagesUpdated)
				var completed int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM sync_runs WHERE source_id=? AND status='completed'`), source.ID).Scan(&completed))
				require.Zero(completed)
			} else {
				checkpoint, err := st.GetLatestCheckpointedSyncByType(t.Context(), source.ID, SourceType)
				require.NoError(err)
				var saved syncState
				require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &saved))
				require.Equal("videos", saved.Queue[0].Kind)
				require.Equal([]string{"oldvideo", "disabledvideo"}, saved.Queue[0].VideoIDs)
			}
			quota = false
			sum, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 20})
			require.NoError(err)
			require.False(sum.Partial)
			rows, err := st.StoredMessagesContext(t.Context(), source.ID, "youtube_json", []string{"video:oldvideo", "video:disabledvideo", "parent", "parent.reply1", "parent.reply2"})
			require.NoError(err)
			require.Len(rows, 5)
		})
	}
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

	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	run, err = st.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(err)
	assert.Zero(run.MessagesAdded)
	assert.Zero(run.MessagesUpdated, "an unchanged replay must not count as updates")

	changed = true
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	run, err = st.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(err)
	assert.Zero(run.MessagesAdded)
	assert.Equal(int64(3), run.MessagesUpdated, "both video roots and the changed comment must be counted")
}

func TestArchiveResolvesCommentAuthor(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	c := archiveFixture(t, false, nil, nil)
	original := c.api.Transport
	resolved := false
	c.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(req)
		if err != nil || req.URL.Path != "/youtube/v3/commentThreads" {
			return resp, err
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if err := resp.Body.Close(); err != nil {
			return nil, err
		}
		text := string(body)
		if resolved {
			text = strings.ReplaceAll(text, `"authorDisplayName":"Same name","authorChannelId":{"value":"UCauthorA"}`, `"authorChannelId":{"value":"UCauthorA"}`)
		} else {
			text = strings.ReplaceAll(text, `,"authorChannelId":{"value":"UCauthorA"}`, "")
		}
		resp.Body = io.NopCloser(strings.NewReader(text))
		return resp, nil
	})
	imp := NewImporter(st, c)
	src, err := imp.Register(t.Context(), "UCfixture")
	require.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	resolved = true
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"parent"})
	require.NoError(err)
	summaries, err := st.GetMessagesSummariesByIDsContext(t.Context(), []int64{rows["parent"].ID})
	require.NoError(err)
	require.Len(summaries, 1)
	require.Equal("Same name", summaries[0].FromName)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUnavailableResourceKeepsArchivedRowsAndDoesNotBlockOtherVideos(t *testing.T) {
	for _, kind := range []string{"video forbidden", "threads forbidden", "replies forbidden", "video missing", "video missing error", "replies missing"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			c := archiveFixture(t, false, nil, nil)
			original := c.api.Transport
			if original == nil {
				original = http.DefaultTransport
			}
			unavailable := false
			sawUnavailable := false
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
					if req.URL.Query().Get("pageToken") == "later" {
						replacement = `{"items":[]}`
						if unavailable {
							require.True(sawUnavailable)
							replacement = `{"items":[{"snippet":{"videoId":"newvideo","topLevelComment":{"id":"other-comment","snippet":{"videoId":"newvideo","textDisplay":"Other video"}}}}]}`
						}
					} else {
						replacement = `{"items":[],"nextPageToken":"later"}`
					}
				}
				if replacement != "" {
					require.NoError(resp.Body.Close())
					resp.Body = io.NopCloser(strings.NewReader(replacement))
				}
				video := req.URL.Path == "/youtube/v3/videos" && strings.Contains(req.URL.Query().Get("id"), "oldvideo")
				threads := req.URL.Path == "/youtube/v3/commentThreads" && req.URL.Query().Get("videoId") == "oldvideo"
				replies := req.URL.Path == "/youtube/v3/comments"
				if unavailable && ((strings.HasPrefix(kind, "video ") && video) || (strings.HasPrefix(kind, "threads ") && threads) || (strings.HasPrefix(kind, "replies ") && replies)) {
					sawUnavailable = true
					require.NoError(resp.Body.Close())
					switch kind {
					case "video missing error":
						resp.StatusCode = http.StatusNotFound
						resp.Body = io.NopCloser(strings.NewReader(`{"error":{"errors":[{"reason":"videoNotFound"}]}}`))
					case "video missing":
						resp.Body = io.NopCloser(strings.NewReader(`{"items":[{"id":"newvideo","snippet":{"channelId":"UCfixture"},"status":{}}]}`))
					case "replies missing":
						resp.StatusCode = http.StatusNotFound
						resp.Body = io.NopCloser(strings.NewReader(`{"error":{"errors":[{"reason":"commentNotFound"}]}}`))
					default:
						resp.StatusCode = http.StatusForbidden
						resp.Body = io.NopCloser(strings.NewReader(`{"error":{"errors":[{"reason":"forbidden"}]}}`))
					}
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
			require.Empty(rows)
			unavailable = true
			sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
			require.NoError(err)
			require.False(sum.Partial)
			rows, err = st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"video:oldvideo", "parent", "parent.reply1", "parent.reply2", "other-comment"})
			require.NoError(err)
			for _, id := range []string{"video:oldvideo", "parent", "parent.reply1", "parent.reply2", "other-comment"} {
				require.Contains(rows, id)
			}
		})
	}
}

func TestAppendUniqueWorkKeepsPendingPage(t *testing.T) {
	pending := work{Kind: "replies", VideoID: "video", Parent: "comment", Token: "next"}
	batch := work{Kind: "videos", VideoIDs: []string{"a", "b"}}
	jobs := appendUniqueWork([]work{pending}, work{Kind: "replies", VideoID: "video", Parent: "comment"}, batch, batch)
	require.Equal(t, []work{pending, batch}, jobs)
}

func TestInvalidPageTokenRestartsListing(t *testing.T) {
	t.Run("expired markers", func(t *testing.T) {
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
			token := req.URL.Query().Get("pageToken")
			if token == "expired-first" || token == "expired-later" {
				invalid++
				resp.StatusCode = http.StatusBadRequest
				resp.Body = io.NopCloser(strings.NewReader(`{"error":{"errors":[{"reason":"invalidPageToken"}]}}`))
			} else if token == "" {
				initial++
				if initial == 1 {
					resp.Body = io.NopCloser(strings.NewReader(`{"items":[],"nextPageToken":"expired-first"}`))
				} else {
					resp.Body = io.NopCloser(strings.NewReader(`{"items":[],"nextPageToken":"later"}`))
				}
			} else if invalid == 1 {
				resp.Body = io.NopCloser(strings.NewReader(`{"items":[],"nextPageToken":"expired-later"}`))
			} else {
				resp.Body = io.NopCloser(strings.NewReader(`{"items":[]}`))
			}
			return resp, nil
		})
		imp := NewImporter(st, c)
		src, err := imp.Register(t.Context(), "UCfixture")
		require.NoError(err)
		sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 20})
		require.NoError(err)
		assert.False(sum.Partial)
		assert.Equal(3, initial)
		assert.Equal(2, invalid)
		run, err := st.GetLatestSyncContext(t.Context(), src.ID, 0)
		require.NoError(err)
		assert.Equal("completed", run.Status)
	})
	t.Run("missing marker", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewTestStore(t)
		c := archiveFixture(t, false, nil, nil)
		original := c.api.Transport
		c.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := original.RoundTrip(req)
			if err == nil && req.URL.Path == "/youtube/v3/playlistItems" {
				require.Empty(req.URL.Query().Get("pageToken"))
				require.NoError(resp.Body.Close())
				resp.StatusCode = http.StatusBadRequest
				resp.Body = io.NopCloser(strings.NewReader(`{"error":{"errors":[{"reason":"invalidPageToken"}]}}`))
			}
			return resp, err
		})
		imp := NewImporter(st, c)
		_, err := imp.Register(t.Context(), "UCfixture")
		require.NoError(err)
		sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 5})
		require.NoError(err)
		require.False(sum.Partial)
	})
}

func TestOwnershipPagesRespectBudgetAndResume(t *testing.T) {
	for _, tc := range []struct{ target, budget int }{{2, 2}, {3, 2}, {2, 3}} {
		target, budget := tc.target, tc.budget
		t.Run(fmt.Sprintf("page%d-budget%d", target, budget), func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			calls := 0
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					_, _ = fmt.Fprint(w, `{"access_token":"fixture","token_type":"Bearer","expires_in":3600}`)
					return
				}
				calls++
				switch r.URL.Path {
				case "/channels":
					page := 1
					if p := r.URL.Query().Get("pageToken"); p != "" {
						_, _ = fmt.Sscan(p, &page)
					}
					if page == target {
						_, _ = fmt.Fprint(w, `{"items":[{"id":"UCfixture","snippet":{"title":"Fixture"},"contentDetails":{"relatedPlaylists":{"uploads":"UUfixture"}}}]}`)
					} else {
						_, _ = fmt.Fprintf(w, `{"items":[],"nextPageToken":"%d"}`, page+1)
					}
				case "/playlistItems":
					_, _ = fmt.Fprint(w, `{"items":[{"contentDetails":{"videoId":"video"}}]}`)
				case "/videos":
					_, _ = fmt.Fprint(w, `{"items":[{"id":"video","snippet":{"channelId":"UCfixture"},"status":{}}]}`)
				case "/commentThreads":
					_, _ = fmt.Fprint(w, `{"items":[{"snippet":{"videoId":"video","topLevelComment":{"id":"comment","snippet":{"videoId":"video","textDisplay":"hello"}}}}]}`)
				default:
					assert.Fail(t, "unexpected path", r.URL.Path)
				}
			})
			imp := NewImporter(st, client)
			src, err := imp.Register(t.Context(), "UCfixture")
			require.NoError(err)
			require.Equal(target, calls)
			found := false
			for range 20 {
				before := calls
				sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: budget})
				require.NoError(err)
				require.LessOrEqual(calls-before, budget)
				require.Equal(calls-before, sum.Requests)
				rows, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"comment"})
				require.NoError(err)
				if len(rows) > 0 {
					found = true
					break
				}
			}
			require.True(found)
		})
	}
}

func TestOwnershipFailureSavesProgress(t *testing.T) {
	for _, failure := range []string{"invalidPageToken", "quotaExceeded", "transport", "not-owned"} {
		t.Run(failure, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			mode := "register"
			var lastToken string
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					_, _ = fmt.Fprint(w, `{"access_token":"fixture","token_type":"Bearer","expires_in":3600}`)
					return
				}
				if r.URL.Path == "/playlistItems" {
					_, _ = fmt.Fprint(w, `{"items":[]}`)
					return
				}
				if r.URL.Path != "/channels" {
					assert.Fail(t, "unexpected content request", r.URL.Path)
					return
				}
				lastToken = r.URL.Query().Get("pageToken")
				if mode == "register" {
					_, _ = fmt.Fprint(w, `{"items":[{"id":"UCfixture","snippet":{"title":"Fixture"},"contentDetails":{"relatedPlaylists":{"uploads":"UUfixture"}}}]}`)
					return
				}
				if mode == "seek" {
					_, _ = fmt.Fprint(w, `{"items":[],"nextPageToken":"next"}`)
					return
				}
				if failure == "not-owned" {
					_, _ = fmt.Fprint(w, `{"items":[]}`)
					return
				}
				if failure == "invalidPageToken" {
					w.WriteHeader(http.StatusBadRequest)
				} else {
					w.WriteHeader(http.StatusForbidden)
				}
				_, _ = fmt.Fprintf(w, `{"error":{"errors":[{"reason":%q}]}}`, failure)
			})
			original := client.api.Transport
			client.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if mode == "fail" && failure == "transport" {
					return nil, errors.New("fixture transport failure")
				}
				return original.RoundTrip(req)
			})
			imp := NewImporter(st, client)
			src, err := imp.Register(t.Context(), "UCfixture")
			require.NoError(err)
			mode = "seek"
			sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 2})
			require.NoError(err)
			require.True(sum.Partial)
			mode = "fail"
			sum, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 2})
			require.Error(err)
			require.Equal(1, sum.Requests)
			if failure == "quotaExceeded" {
				require.ErrorIs(err, ErrQuota)
			}
			cp, err := st.GetLatestCheckpointedSyncByType(t.Context(), src.ID, SourceType)
			require.NoError(err)
			require.NotNil(cp)
			var state syncState
			require.NoError(json.Unmarshal([]byte(cp.CursorBefore.String), &state))
			if failure == "invalidPageToken" || failure == "not-owned" {
				require.Empty(state.OwnershipCursor)
			} else {
				require.Equal("next", state.OwnershipCursor)
			}
			if failure == "invalidPageToken" {
				mode = "register"
				_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 2})
				require.NoError(err)
				require.Empty(lastToken)
			}
		})
	}
}

func TestArchiveBatchesVideosAcrossResumedRuns(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var batches []int
	owners := 0
	passes := map[string]int{}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = fmt.Fprint(w, `{"access_token":"fixture","token_type":"Bearer","expires_in":3600}`)
		case "/channels":
			owners++
			_, _ = fmt.Fprint(w, `{"items":[{"id":"UCfixture","snippet":{"title":"Channel"},"contentDetails":{"relatedPlaylists":{"uploads":"UUfixture"}}}]}`)
		case "/playlistItems":
			start := 0
			if r.URL.Query().Get("pageToken") != "" {
				_, _ = fmt.Sscan(r.URL.Query().Get("pageToken"), &start)
			}
			items := make([]*yt.PlaylistItem, 0, 50)
			for i := start; i < min(start+50, 120); i++ {
				items = append(items, &yt.PlaylistItem{ContentDetails: &yt.PlaylistItemContentDetails{VideoId: fmt.Sprintf("video%d", i)}})
			}
			result := yt.PlaylistItemListResponse{Items: items}
			if start+50 < 120 {
				result.NextPageToken = strconv.Itoa(start + 50)
			}
			assert.NoError(t, json.MarshalWrite(w, result))
		case "/videos":
			ids := strings.Split(r.URL.Query().Get("id"), ",")
			batches = append(batches, len(ids))
			result := yt.VideoListResponse{}
			for _, id := range ids {
				result.Items = append(result.Items, &yt.Video{Id: id, Snippet: &yt.VideoSnippet{ChannelId: "UCfixture", Title: id}, Status: &yt.VideoStatus{PrivacyStatus: "public"}})
			}
			assert.NoError(t, json.MarshalWrite(w, result))
		case "/commentThreads":
			pass := r.URL.Query().Get("moderationStatus")
			passes[pass]++
			if pass == "published" {
				id := r.URL.Query().Get("videoId")
				result := yt.CommentThreadListResponse{Items: []*yt.CommentThread{{Snippet: &yt.CommentThreadSnippet{VideoId: id, TopLevelComment: &yt.Comment{Id: "comment-" + id, Snippet: &yt.CommentSnippet{VideoId: id, TextDisplay: "Comment"}}}}}}
				assert.NoError(t, json.MarshalWrite(w, result))
			} else {
				_, _ = fmt.Fprint(w, `{"items":[]}`)
			}
		default:
			assert.Fail(t, "unexpected route", r.URL.Path)
		}
	})
	imp := NewImporter(st, c)
	src, err := imp.Register(t.Context(), "UCfixture")
	require.NoError(err)
	runs := 0
	completed := false
	for range 40 {
		runs++
		sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 25})
		require.NoError(err)

		if !sum.Partial {
			completed = true
			break
		}
	}
	require.Greater(runs, 1)
	require.True(completed)
	require.Equal(runs+1, owners)
	require.Equal([]int{50, 50, 20}, batches)
	for _, pass := range []string{"published", "heldForReview", "likelySpam"} {
		require.Equal(120, passes[pass], pass)
	}
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"video:video0", "comment-video0", "video:video119", "comment-video119"})
	require.NoError(err)
	require.Len(rows, 4)
}

func TestUnknownModerationDoesNotBlockLaterVideos(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	client := archiveFixture(t, false, nil, nil)
	original := client.api.Transport
	unknown := false
	client.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := original.RoundTrip(req)
		if err != nil || req.URL.Path != "/youtube/v3/commentThreads" || !unknown {
			return resp, err
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if err := resp.Body.Close(); err != nil {
			return nil, err
		}
		text := strings.ReplaceAll(string(body), `"moderationStatus":"published"`, `"moderationStatus":"futureStatus"`)
		if req.URL.Query().Get("videoId") == "newvideo" && req.URL.Query().Get("moderationStatus") == "published" {
			text = `{"items":[{"snippet":{"videoId":"newvideo","topLevelComment":{"id":"later-comment","snippet":{"videoId":"newvideo","textDisplay":"Later","moderationStatus":"futureStatus"}}}}]}`
		}
		resp.Body = io.NopCloser(strings.NewReader(text))
		return resp, nil
	})
	// Include a later video in the uploads page.
	base := client.api.Transport
	client.api.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(req)
		if err == nil && req.URL.Path == "/youtube/v3/playlistItems" {
			if err := resp.Body.Close(); err != nil {
				return nil, err
			}
			resp.Body = io.NopCloser(strings.NewReader(`{"items":[{"contentDetails":{"videoId":"oldvideo"}},{"contentDetails":{"videoId":"newvideo"}}]}`))
		}
		return resp, err
	})
	imp := NewImporter(st, client)
	src, err := imp.Register(t.Context(), "UCfixture")
	require.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	unknown = true
	sum, err := imp.Import(t.Context(), ImportOptions{ChannelID: "UCfixture", MaxRequests: 100})
	require.NoError(err)
	require.False(sum.Partial)
	rows, err := st.MessageMetadataBatch(t.Context(), src.ID, []string{"parent", "later-comment"})
	require.NoError(err)
	require.Contains(rows, "later-comment")
	var metadata commentMetadata
	require.NoError(json.Unmarshal([]byte(rows["parent"].Metadata.String), &metadata))
	require.Equal("published", metadata.ModerationStatus)
	labels, err := st.MessageLabelIDsContext(t.Context(), rows["later-comment"].ID)
	require.NoError(err)
	require.Empty(labels)
}
