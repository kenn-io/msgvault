package gitlab

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/forge"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEventScanResumesAfterEveryPageOnTheSameDay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitLab(t)
	f.me["id"] = 77
	f.project(widgets, "acme/widgets")
	ann := person(t, 2, "ann", "Ann Example")
	start := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	var want []string
	for i := 1; i <= 201; i++ {
		thread := f.issue(widgets, int64(i), i, "An issue", ann, start.Format(time.RFC3339))
		f.commented(int64(i), start.Add(time.Duration(i)*time.Second).Format(time.RFC3339), thread)
		want = append(want, issueGID+strconv.Itoa(i))
	}
	stop := errors.New("interrupted after a page")
	state := cursor{Events: map[string]eventCursor{}}
	var got []string
	for i := range 3 {
		g := f.forge()
		err := g.scanEvents(t.Context(), &state, "commented", forge.NewOwnerFilter(nil, nil),
			func(refs []forge.ThreadRef, next []byte) error {
				for _, ref := range refs {
					got = append(got, ref.Key)
				}
				state = cursor{}
				require.NoError(json.Unmarshal(next, &state))
				return stop
			})
		require.ErrorIs(err, stop)
		assert.Len(got, min((i+1)*100, 201))
		assert.Equal(i < 2, state.Events["commented"].Next != "")
	}
	assert.Equal(want, got, "each interruption still advances to the next page")
	requests := requestsFor(f.requestLog(), "/api/v4/users/77/events")
	require.Len(requests, 3)
	assert.Equal("2", queryOf(t, requests[1]).Get("page"))
	assert.Equal("3", queryOf(t, requests[2]).Get("page"))
	assert.Empty(queryOf(t, requests[2]).Get("after"), "the original scan's query survives the newer watermark")

	thread := f.issue(widgets, 202, 202, "A later issue", ann, start.Format(time.RFC3339))
	f.commented(202, start.Add(time.Hour).Format(time.RFC3339), thread)
	f.clearLog()
	var later []forge.ThreadRef
	require.NoError(f.forge().scanEvents(t.Context(), &state, "commented", forge.NewOwnerFilter(nil, nil),
		func(refs []forge.ThreadRef, _ []byte) error {
			later = append(later, refs...)
			return nil
		}))
	require.Len(later, 1)
	assert.Equal(issueGID+"202", later[0].Key)
	assert.Equal("2026-11-07", queryOf(t, requestsFor(f.requestLog(), "/api/v4/users/77/events")[0]).Get("after"))
	assert.Empty(state.Events["commented"].Next)
}

func TestListScanResumesAfterEveryPageAtTheSameUpdateTime(t *testing.T) {
	for _, earlier := range []int{0, 99} {
		t.Run(fmt.Sprintf("%d earlier threads", earlier), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeGitLab(t)
			f.project(widgets, "acme/widgets")
			ann := person(t, 2, "ann", "Ann Example")
			updated := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
			var want []string
			for i := 1; i <= earlier+201; i++ {
				at := updated
				if i <= earlier {
					at = at.Add(-time.Hour)
				}
				issue := f.issue(widgets, int64(i), i, "An issue", ann, at.Format(time.RFC3339), "assigned_to_me")
				issue.object["created_at"] = updated.Add(-time.Duration(1000-i) * time.Hour).Format(time.RFC3339)
				want = append(want, issueGID+strconv.Itoa(i))
			}
			list := listStream{resource: resourceIssues, scope: "assigned_to_me"}
			state := cursor{Lists: map[string]listCursor{}}
			stop := errors.New("interrupted after a list page")
			var got []string
			for range 20 {
				if state.Lists[list.key()].Through.After(updated) {
					break
				}
				err := f.forge().scanList(t.Context(), &state, list, forge.NewOwnerFilter(nil, nil),
					func(refs []forge.ThreadRef, next []byte) error {
						for _, ref := range refs {
							got = append(got, ref.Key)
						}
						state = cursor{}
						require.NoError(json.Unmarshal(next, &state))
						return stop
					})
				require.ErrorIs(err, stop)
			}
			assert.Equal(want, got, "repeated interruptions reach every thread without duplicates")
			assert.Equal(listCursor{Through: updated.Add(time.Millisecond)}, state.Lists[list.key()],
				"the completed instant moves the list past it")
			requests := requestsFor(f.requestLog(), "/api/v4/issues?")
			for _, request := range requests {
				assert.Empty(queryOf(t, request).Get("page"), "every request asks for a first page")
			}
			assert.NotEmpty(queryOf(t, requests[len(requests)-1]).Get("created_before"), "the instant is read by creation time")

			// A later scan starts past the instant and discovers a newer update.
			id := earlier + 202
			f.issue(widgets, int64(id), id, "A later issue", ann,
				updated.Add(time.Hour).Format(time.RFC3339), "assigned_to_me")
			f.clearLog()
			var later []forge.ThreadRef
			require.NoError(f.forge().scanList(t.Context(), &state, list, forge.NewOwnerFilter(nil, nil),
				func(refs []forge.ThreadRef, _ []byte) error {
					later = append(later, refs...)
					return nil
				}))
			require.Len(later, 1)
			assert.Equal(issueGID+strconv.Itoa(id), later[0].Key)
			query := queryOf(t, requestsFor(f.requestLog(), "/api/v4/issues?")[0])
			assert.Equal(updatedAfter(updated.Add(time.Millisecond)), query.Get("updated_after"))
			assert.Empty(query.Get("created_after"))
		})
	}
}

func TestAPageOfThreadsCreatedAndUpdatedAtOneInstantIsReadWhole(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitLab(t)
	f.project(widgets, "acme/widgets")
	ann := person(t, 2, "ann", "Ann Example")
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	// Exactly a page of threads created and updated together, as an
	// import makes them, then one updated later.
	for i := 1; i <= perPage; i++ {
		issue := f.issue(widgets, int64(i), i, "An issue", ann, at, "assigned_to_me")
		issue.object["created_at"] = at
	}
	f.issue(widgets, 1000, 1000, "A later issue", ann, "2026-11-08T10:00:00Z", "assigned_to_me")
	list := listStream{resource: resourceIssues, scope: "assigned_to_me"}
	state := cursor{Lists: map[string]listCursor{}}
	var got []string
	require.NoError(f.forge().scanList(t.Context(), &state, list, forge.NewOwnerFilter(nil, nil),
		func(refs []forge.ThreadRef, _ []byte) error {
			for _, ref := range refs {
				got = append(got, ref.Key)
			}
			return nil
		}))
	assert.Len(got, perPage+1, "a last page that ends in the instant holds all of it")
}

func TestAThreadLeavingARunUpdatedAtOneInstantShiftsNoneOutOfTheScan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFakeGitLab(t)
	f.project(widgets, "acme/widgets")
	ann := person(t, 2, "ann", "Ann Example")
	updated := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	var first *fakeThread
	for i := 1; i <= 250; i++ {
		issue := f.issue(widgets, int64(i), i, "An issue", ann, updated.Format(time.RFC3339), "assigned_to_me")
		issue.object["created_at"] = updated.Add(-time.Duration(1000-i) * time.Hour).Format(time.RFC3339)
		if i == 1 {
			first = issue
		}
	}
	list := listStream{resource: resourceIssues, scope: "assigned_to_me"}
	state := cursor{Lists: map[string]listCursor{}}
	var got []string
	reports := 0
	require.NoError(f.forge().scanList(t.Context(), &state, list, forge.NewOwnerFilter(nil, nil),
		func(refs []forge.ThreadRef, _ []byte) error {
			for _, ref := range refs {
				got = append(got, ref.Key)
			}
			reports++
			if reports == 2 {
				// Once the scan has read the instant's first page, a thread
				// on it is updated, so every later thread of the instant
				// moves one place forward.
				first.object["updated_at"] = updated.Add(time.Hour).Format(time.RFC3339)
			}
			return nil
		}))
	slices.Sort(got)
	assert.Len(slices.Compact(got), 250, "every thread of the instant is read")
}

type knownThreads []forge.KnownThread

func (k knownThreads) Each(_ context.Context, fn func(forge.KnownThread) error) error {
	for _, thread := range k {
		if err := fn(thread); err != nil {
			return err
		}
	}
	return nil
}

func (k knownThreads) Oldest(_ context.Context, limit int) ([]forge.KnownThread, error) {
	return k[:min(limit, len(k))], nil
}

func TestArchivedThreadCheckCheckpointsEachBatch(t *testing.T) {
	for _, interrupt := range []string{"callback", "second batch fails"} {
		t.Run(interrupt, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFakeGitLab(t)
			f.project(widgets, "acme/widgets")
			ann := person(t, 2, "ann", "Ann Example")
			var known knownThreads
			for iid := 2; iid <= 102; iid++ {
				f.issue(widgets, int64(iid), iid, "An issue", ann, "2026-11-08T09:00:00Z")
				known = append(known, forge.KnownThread{Ref: threadRef(resourceIssues, int64(iid), widgets, iid, "acme/widgets", time.Time{})})
			}
			state := cursor{}
			stop := errors.New("interrupted after a batch")
			if interrupt == "second batch fails" {
				f.failPath, f.failAfter = "/api/v4/projects/5/issues", 1
			}
			var first []forge.ThreadRef
			err := f.forge().checkKnown(t.Context(), &state, forge.NewOwnerFilter(nil, nil), known,
				func(refs []forge.ThreadRef, next []byte) error {
					first = append(first, refs...)
					state = cursor{}
					require.NoError(json.Unmarshal(next, &state))
					if interrupt == "callback" {
						return stop
					}
					return nil
				})
			if interrupt == "callback" {
				require.ErrorIs(err, stop)
			} else {
				require.ErrorContains(err, "400")
			}
			require.Len(first, 100, "a later failure keeps the first batch's reports")
			require.NotNil(state.Known.Done)
			assert.Equal(knownGroup{Project: widgets, Resource: resourceIssues}, *state.Known.Done)
			assert.Equal(101, state.Known.LastIID)
			assert.True(state.Known.Through.IsZero(), "only a completed check advances its watermark")

			// A newly archived lower iid shifts batch indexes, but not the
			// resume boundary recorded by iid.
			f.issue(widgets, 1, 1, "An older issue", ann, "2026-11-08T09:00:00Z")
			known = append(known, forge.KnownThread{Ref: threadRef(resourceIssues, 1, widgets, 1, "acme/widgets", time.Time{})})
			f.failPath = ""
			f.clearLog()
			var resumed []forge.ThreadRef
			require.NoError(f.forge().checkKnown(t.Context(), &state, forge.NewOwnerFilter(nil, nil), known,
				func(refs []forge.ThreadRef, next []byte) error {
					resumed = append(resumed, refs...)
					state = cursor{}
					return json.Unmarshal(next, &state)
				}))
			require.Len(resumed, 1)
			assert.Equal(102, resumed[0].Number)
			requests := requestsFor(f.requestLog(), "/api/v4/projects/5/issues?")
			require.Len(requests, 1)
			assert.Equal([]string{"102"}, queryOf(t, requests[0])["iids[]"])
			assert.Nil(state.Known.Done)
			assert.Zero(state.Known.LastIID)
			assert.False(state.Known.Through.IsZero())
		})
	}
}

func TestArchivedThreadCheckFindsAnOpeningPostEditedDuringAPausedFetch(t *testing.T) {
	// This machine's clock times a fetch; GitLab's times an edit and the
	// check's watermark.
	for _, behind := range []time.Duration{0, 3 * time.Minute} {
		t.Run(fmt.Sprintf("GitLab's clock %s behind", behind), func(t *testing.T) {
			checkFindsAnOpeningPostEditedDuringAPausedFetch(t, behind)
		})
	}
}

func checkFindsAnOpeningPostEditedDuringAPausedFetch(t *testing.T, behind time.Duration) {
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	f.behind = behind
	source := newGitLabSource(t, st, f.forge().Instance())
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		defer func() { require.NoError(st.Close()) }()
		f.project(widgets, "acme/widgets")
		ann := person(t, 2, "ann", "Ann Example")
		created := time.Now().Add(-time.Hour).Format(time.RFC3339)
		issue := f.issue(widgets, 41, 1, "Widgets break", ann, created)
		issue.object["created_at"], issue.object["description"] = created, "Original opening post."
		issue.discussions = []*discussionDoc{
			fakeDiscussion("reply", true, fakeNote(t, 1, nil, "A reply.", ann, created)),
		}
		f.todos = []map[string]any{fakeTodo(1, "mentioned", created, widgets, "acme/widgets", issue)}
		sync := func() forge.Summary {
			g := f.forge()
			g.client.rest.Client.Transport = f
			summary, err := forge.NewSyncer(st, g, nil).Sync(t.Context(), source.ID, forge.Options{})
			require.NoError(err)
			return summary
		}

		// Read the opening post, then pause before reading discussions.
		f.limitPath = "/api/v4/projects/5/issues/1/discussions"
		paused := sync()
		require.True(paused.Paused)
		require.NotNil(paused.RateLimit)
		require.Equal(1, paused.Added)
		require.Equal(1, paused.Remaining)

		synctest.Sleep(time.Minute)
		issue.object["description"] = "Edited during the pause."
		issue.object["updated_at"] = time.Now().Add(-behind).Format(time.RFC3339)

		// The next check finishes before the resumed fetch does, so its
		// watermark falls between the fetch's first and last pages.
		synctest.Sleep(time.Minute)
		f.limitPath = ""
		f.clearLog()
		resumed := sync()
		require.False(resumed.Paused)
		require.Equal(1, resumed.Archived)
		require.Zero(resumed.Updated)
		assert.Empty(requestsFor(f.requestLog(), "/api/v4/projects/5/issues/1?"),
			"resuming the fetch doesn't read its opening post again")
		link := fmt.Sprint(issue.object["web_url"])
		assert.Equal("Original opening post.\n\n"+link, archived(t, st, source.ID)[issueGID+"41"].Body)

		synctest.Sleep(time.Minute)
		f.clearLog()
		checked := sync()
		assert.Equal(1, checked.Archived)
		assert.Equal(1, checked.Updated)
		assert.Zero(checked.Added)
		posts := archived(t, st, source.ID)
		assert.Equal("Edited during the pause.\n\n"+link, posts[issueGID+"41"].Body)
		assert.Equal("A reply.\n\n"+link+"#note_1", posts[noteGID(1)].Body)
	})
}

func TestReplyEventsFindSomeoneElsesMergeRequest(t *testing.T) {
	for _, kind := range []string{"DiffNote", "DiscussionNote"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			f := newFakeGitLab(t)
			f.project(gadgets, "acme/tools/gadgets")
			ann, me := person(t, 2, "ann", "Ann Example"), person(t, 1, "john_smith", "John Smith")
			mr := f.mergeRequest(gadgets, 34, 7, "Someone else's change", ann, "2026-11-08T09:00:00Z")
			mr.discussions = []*discussionDoc{fakeDiscussion("review", false,
				fakeNote(t, 1, kind, "Please check this.", ann, "2026-11-08T08:00:00Z"),
				fakeNote(t, 2, kind, "Checked.", me, "2026-11-08T09:00:00Z"))}
			f.commented(99, "2026-11-08T09:00:00Z", mr)
			f.events[0].object["target_type"] = kind
			f.events[0].object["target_id"] = 2
			f.events[0].object["target_iid"] = nil
			source := newGitLabSource(t, st, f.forge().Instance())
			summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
			require.NoError(err)
			assert.Equal(1, summary.Archived)
			posts := archived(t, st, source.ID)
			assert.Contains(posts, mergeRequestGID+"34")
			assert.Equal("Checked.\n\n"+fmt.Sprint(mr.object["web_url"])+"#note_2", posts[noteGID(2)].Body)
			assert.Equal(noteGID(1), posts[noteGID(2)].Metadata["in_reply_to"])
		})
	}
}

func TestSyncOnlyRewritesThePostThatChanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	f.project(gadgets, "acme/tools/gadgets")
	ann := person(t, 2, "ann", "Ann Example")
	mr := f.mergeRequest(gadgets, 34, 1, "Add gadgets", ann, "2026-11-08T09:00:00Z", "reviews_for_me")
	mr.object["head_pipeline"] = map[string]any{"status": "pending", "duration": nil, "updated_at": "2026-11-08T09:00:00Z"}
	note := fakeNote(t, 1, nil, "Looks good.", ann, "2026-11-08T09:00:00Z")
	mr.discussions = []*discussionDoc{fakeDiscussion("comment", true, note)}
	source := newGitLabSource(t, st, f.forge().Instance())
	sync := func() forge.Summary {
		summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
		require.NoError(err)
		return summary
	}
	require.Equal(2, sync().Added)
	mr.object["head_pipeline"] = map[string]any{"status": "success", "duration": 30, "updated_at": "2026-11-08T10:00:00Z"}
	mr.object["merge_status"], mr.object["detailed_merge_status"] = "cannot_be_merged", "conflict"
	mr.object["user_notes_count"], mr.object["upvotes"], mr.object["downvotes"] = 7, 3, 1
	mr.object["updated_at"] = "2026-11-08T10:00:00Z"
	activity := sync()
	assert.Equal(1, activity.Archived, "the changed thread was fetched")
	assert.Zero(activity.Updated, "pipeline, status, note count and votes don't change the post")
	assert.Zero(activity.Added)

	mr.object["title"], mr.object["description"] = "Add better gadgets", "Edited opening post."
	mr.object["updated_at"] = "2026-11-08T11:00:00Z"
	opening := sync()
	assert.Equal(1, opening.Updated)
	assert.Zero(opening.Added)
	posts := archived(t, st, source.ID)
	link := fmt.Sprint(mr.object["web_url"])
	assert.Equal("Add better gadgets", posts[mergeRequestGID+"34"].Subject)
	assert.Equal("Edited opening post.\n\n"+link, posts[mergeRequestGID+"34"].Body)
	assert.Equal("Looks good.\n\n"+link+"#note_1", posts[noteGID(1)].Body)

	note["body"], note["updated_at"] = "Actually, please revise.", "2026-11-08T12:00:00Z"
	mr.object["updated_at"] = "2026-11-08T12:00:00Z"
	edited := sync()
	assert.Equal(1, edited.Updated)
	assert.Zero(edited.Added)
	posts = archived(t, st, source.ID)
	assert.Equal("Actually, please revise.\n\n"+link+"#note_1", posts[noteGID(1)].Body)
	assert.Equal("Edited opening post.\n\n"+link, posts[mergeRequestGID+"34"].Body)
}

func TestPublicEmailJoinsTheMailParticipantButBotsAndImportUsersDoNot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	f.project(widgets, "acme/widgets")
	var want []int64
	for i, kind := range []string{"HUMAN", "PROJECT_BOT", "IMPORT_USER"} {
		id := int64(i + 2)
		email := fmt.Sprintf("author%d@example.com", id)
		participantID, err := st.EnsureParticipantContext(t.Context(), email, "Mail author", "example.com")
		require.NoError(err)
		want = append(want, participantID)
		// The author as REST embeds it before GitLab 18.0, without public_email:
		// the email comes from GraphQL's user lookup.
		author := person(t, id, "author"+strconv.FormatInt(id, 10), "An Author")
		require.NotContains(author, "public_email")
		node := userType(id, kind, kind == "PROJECT_BOT")
		node["publicEmail"] = email
		f.userTypes[id] = node
		f.issue(widgets, id, int(id), kind, author, "2026-11-08T09:00:00Z", "assigned_to_me")
	}
	source := newGitLabSource(t, st, f.forge().Instance())
	summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.Equal(3, summary.Added)
	for i, participantID := range want {
		var senderID int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
			`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`),
			source.ID, issueGID+strconv.Itoa(i+2)).Scan(&senderID))
		if i == 0 {
			assert.Equal(participantID, senderID, "a public email joins the existing mail participant")
		} else {
			assert.NotEqual(participantID, senderID, "bots and unavailable authors never join by email")
		}
	}
	posts := archived(t, st, source.ID)
	assert.Equal(string(f.forge().Instance())+":user:3", posts[issueGID+"3"].SenderValue)
	assert.NotEqual(Name, posts[issueGID+"4"].SenderType, "IMPORT_USER has no forge account identity")
	assert.Equal("url:https://gitlab.example.com/author4", posts[issueGID+"4"].SenderValue,
		"IMPORT_USER is unavailable, so it keeps its profile URL rather than a migrated name")
}

func TestAFullSyncLinksAnAuthorWhosePublicEmailAppearedSince(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	f.project(widgets, "acme/widgets")
	email := "author2@example.com"
	participantID, err := st.EnsureParticipantContext(t.Context(), email, "Mail author", "example.com")
	require.NoError(err)
	node := userType(2, "HUMAN", false)
	f.userTypes[2] = node
	f.issue(widgets, 2, 2, "Widgets", person(t, 2, "author2", "An Author"), "2026-11-08T09:00:00Z", "assigned_to_me")
	source := newGitLabSource(t, st, f.forge().Instance())
	sync := func(opts forge.Options) forge.Summary {
		summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, opts)
		require.NoError(err)
		return summary
	}
	sender := func() int64 {
		var senderID int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
			`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`),
			source.ID, issueGID+"2").Scan(&senderID))
		return senderID
	}

	require.Equal(1, sync(forge.Options{}).Added)
	assert.NotEqual(participantID, sender(), "no public email yet")

	// The author makes their email public. Before GitLab 18.0 only GraphQL
	// shows it; the issue's REST JSON is unchanged.
	node["publicEmail"] = email
	full := sync(forge.Options{Full: true})
	assert.Equal(1, full.Updated, "only what GraphQL said of the author changed")
	assert.Equal(participantID, sender(), "the post's sender joins the mail participant")
}

func TestIncludeOwnersLimitsWhatIsFetched(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	seed(t, f)
	f.mergeRequest(dots, 99, 3, "Update dotfiles", person(t, 1, "john_smith", "John Smith"),
		"2026-11-08T09:00:00Z", "created_by_me")
	source := newGitLabSource(t, st, f.forge().Instance())
	summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID,
		forge.Options{Owners: forge.NewOwnerFilter([]string{"john_smith"}, nil)})
	require.NoError(err)
	assert.Equal(2, summary.Archived)
	assert.Equal(3, summary.Added)
	posts := archived(t, st, source.ID)
	assert.Len(posts, 3)
	assert.Contains(posts, mergeRequestGID+"99", "included owners' merge requests are fetched too")
	log := f.requestLog()
	for _, project := range []string{"5", "6", "9"} {
		assert.Empty(requestsFor(log, "/api/v4/projects/"+project+"/"), "an excluded owner's threads remain unfetched")
	}
}

func TestDoneTodosAreDiscoveredAlongsidePendingOnes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	f.project(widgets, "acme/widgets")
	ann := person(t, 2, "ann", "Ann Example")
	for i, state := range []string{"pending", "done"} {
		id := int64(i + 1)
		issue := f.issue(widgets, id, i+1, state, ann, "2026-11-08T09:00:00Z")
		todo := fakeTodo(id, "mentioned", "2026-11-08T09:00:00Z", widgets, "acme/widgets", issue)
		todo["state"] = state
		f.todos = append(f.todos, todo)
	}
	source := newGitLabSource(t, st, f.forge().Instance())
	summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, forge.Options{})
	require.NoError(err)
	assert.Equal(2, summary.Archived)
	posts := archived(t, st, source.ID)
	assert.Equal("pending", posts[issueGID+"1"].Subject)
	assert.Equal("done", posts[issueGID+"2"].Subject)
}

func TestTheArchivedThreadCheckReadsNothingOfAProjectMovedToAnExcludedOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	source := newGitLabSource(t, st, f.forge().Instance())
	f.project(widgets, "acme/widgets")
	f.issue(widgets, 41, 1, "Widgets break", person(t, 2, "ann", "Ann Example"), "2026-11-07T08:00:00Z", "assigned_to_me")
	_, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, excludeSecretCorp())
	require.NoError(err)
	require.Contains(archived(t, st, source.ID), issueGID+"41")

	// The project moves under the excluded group after its issue was
	// archived under the old one.
	f.project(widgets, "secret-corp/widgets")
	f.clearLog()
	_, err = forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, excludeSecretCorp())
	require.NoError(err)
	log := f.requestLog()
	assert.NotEmpty(requestsFor(log, "/api/v4/projects/5?"), "the check reads the project's owner")
	assert.Empty(requestsFor(log, "/api/v4/projects/5/"), "and nothing of an excluded owner's project")
}

func TestAProjectMovedToAnExcludedOwnerSinceDiscoveryIsNotFetched(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	f := newFakeGitLab(t)
	source := newGitLabSource(t, st, f.forge().Instance())
	ann := person(t, 2, "ann", "Ann Example")
	// The to-do still shows the project at its old path; the project itself
	// now sits under an excluded group.
	f.project(widgets, "secret-corp/widgets")
	issue := f.issue(widgets, 41, 1, "Widgets break", ann, "2026-11-07T08:00:00Z")
	f.todos = []map[string]any{fakeTodo(1, "mentioned", "2026-11-07T08:00:00Z", widgets, "acme/widgets", issue)}

	summary, err := forge.NewSyncer(st, f.forge(), nil).Sync(t.Context(), source.ID, excludeSecretCorp())
	require.NoError(err)
	assert.Equal(1, summary.Excluded, "the owner is read from the project, not from the to-do")
	assert.Empty(requestsFor(f.requestLog(), "/api/v4/projects/5/issues/1"), "the moved project's thread is never read")
	assert.NotContains(archived(t, st, source.ID), issueGID+"41")
}
