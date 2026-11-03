package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Embed the unused methods so this fake can only exercise the token-list path.
type subscriptionAPI struct {
	MarketingAPI

	list func(context.Context, string, string) (TokenPage, error)
}

func (a subscriptionAPI) SubscriptionTokens(ctx context.Context, page, after string) (TokenPage, error) {
	return a.list(ctx, page, after)
}
func subscriptionFixture(t *testing.T) (*store.Store, DMSource, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	src := DMSource{AppID: "9", PageID: "100", Platform: PlatformMessenger}
	s, err := st.GetOrCreateSource(SourceTypeFacebookMessages, src.Identifier())
	require.NoError(t, err)
	return st, src, s.ID
}
func subscriptionToken(token, psid string) SubscriptionToken {
	return SubscriptionToken{Token: token, PSID: psid, TopicTitle: "Example offers", Timezone: "UTC", Reoptin: "ENABLED", Created: time.Now().Add(-time.Hour), Expiry: time.Now().Add(48 * time.Hour)}
}
func subscriptionPage(tokens ...SubscriptionToken) subscriptionAPI {
	return subscriptionAPI{list: func(context.Context, string, string) (TokenPage, error) { return TokenPage{Data: tokens}, nil }}
}
func subscriptionRows(t *testing.T, st *store.Store, src int64) []store.MarketingSubscription {
	t.Helper()
	rows, err := st.ListMarketingSubscriptionsContext(t.Context(), src, store.SubscriptionFilter{})
	require.NoError(t, err)
	return rows
}
func assertSubscriptionWalk(t *testing.T, st *store.Store, id int64, state string, requests, listed int) {
	t.Helper()
	var gotState string
	var gotRequests, gotListed int
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT state,requests,listed FROM meta_marketing_list_walks WHERE id=?"), id).Scan(&gotState, &gotRequests, &gotListed))
	assert.Equal(t, state, gotState)
	assert.Equal(t, requests, gotRequests)
	assert.Equal(t, listed, gotListed)
}
func TestSyncCompleteWalkUpsertsAndLinks(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src, id := subscriptionFixture(t)
	api := subscriptionAPI{list: func(ctx context.Context, page, after string) (TokenPage, error) {
		assert.Equal(t, "100", page)
		if after == "" {
			return TokenPage{Data: []SubscriptionToken{subscriptionToken("synthetic-mm-token-1", "300")}, After: "cursor-2"}, nil
		}
		assert.Equal(t, "cursor-2", after)
		return TokenPage{Data: []SubscriptionToken{subscriptionToken("synthetic-mm-token-2", "301")}}, nil
	}}
	summary, err := SyncSubscriptions(t.Context(), st, api, src, 1000)
	requirements.NoError(err)
	assertions.True(summary.Complete)
	assertions.Equal(2, summary.Listed)
	assertions.Equal(2, summary.Requests)
	assertSubscriptionWalk(t, st, summary.WalkID, "complete", 2, 2)
	rows := subscriptionRows(t, st, id)
	requirements.Len(rows, 2)
	participant, err := ResolveDMActor(t.Context(), st, id, src, "300", "")
	requirements.NoError(err)
	assertions.Equal(participant, rows[0].ParticipantID.Int64)
	assertions.True(rows[0].LastConfirmedAt.Valid)
	assertions.Equal("Example offers", rows[0].TopicTitle)
	assertions.Equal("UTC", rows[0].Timezone)
	assertions.Equal("ENABLED", rows[0].Reoptin)
}
func TestSyncMarksAbsentOnlyAfterCompleteWalk(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src, id := subscriptionFixture(t)
	_, err := SyncSubscriptions(t.Context(), st, subscriptionPage(subscriptionToken("synthetic-mm-token-1", "300"), subscriptionToken("synthetic-mm-token-2", "301")), src, 0)
	requirements.NoError(err)
	summary, err := SyncSubscriptions(t.Context(), st, subscriptionPage(subscriptionToken("synthetic-mm-token-2", "301")), src, 0)
	requirements.NoError(err)
	assertions.True(summary.Complete)
	assertions.Equal(1, summary.Absent)
	assertions.Equal("absent", subscriptionRows(t, st, id)[0].Status)
}
func TestSyncPartialWalkDoesNotRefreshUnreached(t *testing.T) {
	for _, mode := range []string{"budget", "error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			st, src, id := subscriptionFixture(t)
			_, err := SyncSubscriptions(t.Context(), st, subscriptionPage(subscriptionToken("synthetic-mm-token-1", "300"), subscriptionToken("synthetic-mm-token-2", "301")), src, 0)
			requirements.NoError(err)
			before := subscriptionRows(t, st, id)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			api := subscriptionAPI{list: func(context.Context, string, string) (TokenPage, error) {
				calls++
				if calls == 1 {
					return TokenPage{Data: []SubscriptionToken{subscriptionToken("synthetic-mm-token-1", "300")}, After: "cursor-2"}, nil
				}
				if mode == "cancel" {
					cancel()
					return TokenPage{}, ctx.Err()
				}
				return TokenPage{}, &APIError{Status: 500}
			}}
			budget := 10
			if mode == "budget" {
				budget = 1
			}
			summary, err := SyncSubscriptions(ctx, st, api, src, budget)
			if mode == "budget" {
				requirements.NoError(err)
			} else {
				requirements.Error(err)
				assertions.NotContains(err.Error(), "synthetic-mm-token")
			}
			assertions.False(summary.Complete)
			assertions.Zero(summary.Absent)
			assertSubscriptionWalk(t, st, summary.WalkID, "partial", calls, 1)
			after := subscriptionRows(t, st, id)
			for i := range before {
				assertions.Equal("active", after[i].Status)
				assertions.True(before[i].LastConfirmedAt.Time.Equal(after[i].LastConfirmedAt.Time))
			}
		})
	}
}
func TestSyncNeverResumes(t *testing.T) {
	st, src, _ := subscriptionFixture(t)
	api := subscriptionAPI{list: func(_ context.Context, _, after string) (TokenPage, error) {
		assert.Empty(t, after)
		return TokenPage{After: "cursor-2"}, nil
	}}
	for range 2 {
		summary, err := SyncSubscriptions(t.Context(), st, api, src, 1)
		require.NoError(t, err)
		assert.False(t, summary.Complete)
	}
}

func TestSyncDeduplicatesTokenList(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src, id := subscriptionFixture(t)
	token := subscriptionToken("synthetic-mm-token-1", "300")
	api := subscriptionAPI{list: func(_ context.Context, _, after string) (TokenPage, error) {
		if after == "" {
			return TokenPage{Data: []SubscriptionToken{token, token}, After: "cursor-2"}, nil
		}
		return TokenPage{Data: []SubscriptionToken{token}}, nil
	}}
	summary, err := SyncSubscriptions(t.Context(), st, api, src, 0)
	requirements.NoError(err)
	assertions.True(summary.Complete)
	assertions.Equal(1, summary.Listed)
	assertSubscriptionWalk(t, st, summary.WalkID, "complete", 2, 1)
	rows := subscriptionRows(t, st, id)
	requirements.Len(rows, 1)
	events, err := st.LatestConsentEventsContext(t.Context(), []int64{rows[0].ID})
	requirements.NoError(err)
	assertions.Len(events[rows[0].ID], 1)
}
func TestSyncDoesNotReactivateStopped(t *testing.T) {
	for _, status := range []string{"stopped", "unsubscribed"} {
		t.Run(status, func(t *testing.T) {
			requirements := require.New(t)

			st, src, id := subscriptionFixture(t)
			api := subscriptionPage(subscriptionToken("synthetic-mm-token-1", "300"))
			_, err := SyncSubscriptions(t.Context(), st, api, src, 0)
			requirements.NoError(err)
			row := subscriptionRows(t, st, id)[0]
			requirements.NoError(st.SetSubscriptionStatusContext(t.Context(), row.ID, status, "local_unsubscribe", "owner", "owner-1"))
			_, err = SyncSubscriptions(t.Context(), st, api, src, 0)
			requirements.NoError(err)
			assert.Equal(t, status, subscriptionRows(t, st, id)[0].Status)
		})
	}
}
func TestSyncRequiresMessengerSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	src := DMSource{AppID: "9", PageID: "100", Platform: PlatformMessenger}
	_, err := st.GetOrCreateSource(SourceType, src.Identifier())
	require.NoError(t, err)
	_, err = SyncSubscriptions(t.Context(), st, subscriptionAPI{}, src, 0)
	require.EqualError(t, err, "register the Messenger source for app 9 Page 100 first")
}
func TestSyncLeavesDMCheckpointUntouched(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src, id := subscriptionFixture(t)
	run, err := st.StartSyncContext(t.Context(), id, "incremental")
	requirements.NoError(err)
	requirements.NoError(st.FailSyncWithCheckpointContext(t.Context(), run, "synthetic interrupted", &store.Checkpoint{PageToken: "dm-cursor", MessagesProcessed: 7}))
	before, err := st.GetLatestCheckpointedSyncContext(t.Context(), id)
	requirements.NoError(err)
	requirements.NotNil(before)
	_, err = SyncSubscriptions(t.Context(), st, subscriptionPage(), src, 0)
	requirements.NoError(err)
	after, err := st.GetLatestCheckpointedSyncContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal(before, after)
	var count int
	requirements.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM sync_runs").Scan(&count))
	assertions.Equal(1, count)
}
func TestSyncCustomAudienceTokenNoParticipant(t *testing.T) {
	st, src, id := subscriptionFixture(t)
	_, err := SyncSubscriptions(t.Context(), st, subscriptionPage(subscriptionToken("synthetic-mm-token-1", "")), src, 0)
	require.NoError(t, err)
	row := subscriptionRows(t, st, id)[0]
	assert.False(t, row.ParticipantID.Valid)
	assert.False(t, row.PSID.Valid)
}
func TestSyncWalkRowHasNoTokens(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	st, src, _ := subscriptionFixture(t)
	summary, err := SyncSubscriptions(t.Context(), st, subscriptionAPI{list: func(context.Context, string, string) (TokenPage, error) {
		return TokenPage{}, errors.New("synthetic-mm-token-secret")
	}}, src, 0)
	requirements.Error(err)
	assertions.NotContains(err.Error(), "synthetic-mm-token")
	var category string
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT error_category FROM meta_marketing_list_walks WHERE id=?"), summary.WalkID).Scan(&category))
	assertions.Equal("send_rejected", category)
}
func TestSyncRejectsPaginationHistoryThroughClient(t *testing.T) {
	type page struct{ token, after string }
	for _, tc := range []struct {
		name       string
		pages      []page
		wantListed int
		wantError  string
	}{
		{"non_adjacent_cycle", []page{{"synthetic-mm-token-1", "cursor-A"}, {"synthetic-mm-token-2", "cursor-B"}, {"synthetic-mm-token-3", "cursor-A"}}, 2, "invalid marketing pagination progress"},
		{"earlier_page_token", []page{{"synthetic-mm-token-1", "cursor-A"}, {"synthetic-mm-token-2", "cursor-synthetic-mm-token-1"}}, 1, "invalid marketing cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, src, _ := subscriptionFixture(t)
			var requests atomic.Int64
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				index := int(requests.Add(1)) - 1
				if !assertions.Less(index, len(tc.pages)) {
					http.Error(w, "unexpected page", http.StatusBadRequest)
					return
				}
				assertions.Equal(http.MethodGet, r.Method)
				assertions.Equal("/v26.0/100/notification_message_tokens", r.URL.Path)
				after := ""
				if index > 0 {
					after = tc.pages[index-1].after
				}
				assertions.Equal(after, r.URL.Query().Get("after"))
				page := tc.pages[index]
				assertions.NoError(json.MarshalWrite(w, map[string]any{
					"data":   []map[string]string{{"notification_messages_token": page.token}},
					"paging": map[string]any{"cursors": map[string]string{"after": page.after}, "next": "https://graph.example/next"},
				}))
			})
			summary, err := SyncSubscriptions(t.Context(), st, client, src, 5)
			requirements.EqualError(err, tc.wantError)
			assertions.False(summary.Complete)
			assertions.Equal(len(tc.pages), summary.Requests)
			assertions.Equal(int64(len(tc.pages)), requests.Load())
			assertions.Equal(tc.wantListed, summary.Listed)
			assertions.NotContains(err.Error(), "synthetic-mm-token")
			assertSubscriptionWalk(t, st, summary.WalkID, "partial", len(tc.pages), tc.wantListed)
		})
	}
}
