package meta

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type unsubscribeAPI struct {
	MarketingAPI

	onCall        func()
	psids, tokens []string
	err           error
}

func (a *unsubscribeAPI) UnsubscribePSIDs(_ context.Context, _ string, ids []string) (int, error) {
	a.psids = append(a.psids, ids...)
	if a.onCall != nil {
		a.onCall()
	}
	return 0, a.err
}
func (a *unsubscribeAPI) UnsubscribeTokens(_ context.Context, _ string, tokens []string) (int, error) {
	a.tokens = append(a.tokens, tokens...)
	if a.onCall != nil {
		a.onCall()
	}
	return 0, a.err
}
func TestUnsubscribeLocalBeforeProvider(t *testing.T) {
	assertions := assert.New(t)

	f := newSenderFixture(t, 1)
	subs := subscriptionRows(t, f.st, f.b.SourceID)
	a := &unsubscribeAPI{onCall: func() {
		rows := subscriptionRows(t, f.st, f.b.SourceID)
		assert.Equal(t, "unsubscribed", rows[0].Status)
		events, err := f.st.LatestConsentEventsContext(t.Context(), []int64{subs[0].ID})
		require.NoError(t, err)
		require.NotEmpty(t, events[subs[0].ID])
		assert.Equal(t, "local_unsubscribe", events[subs[0].ID][0].Kind)
	}}
	r, err := Unsubscribe(t.Context(), f.st, a, f.policy, UnsubscribeSelector{SubscriptionID: subs[0].ID})
	require.NoError(t, err)
	assertions.Equal(1, r.Local)
	assertions.True(r.ProviderOK)
	assertions.Equal([]string{"300"}, a.psids)
	assertions.Empty(a.tokens)
}
func TestUnsubscribeProviderFailureKeepsSuppression(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 1)
	sub := subscriptionRows(t, f.st, f.b.SourceID)[0]
	a := &unsubscribeAPI{err: errors.New("secret synthetic-mm-token-1")}
	r, err := Unsubscribe(t.Context(), f.st, a, f.policy, UnsubscribeSelector{SubscriptionID: sub.ID})
	requirements.Error(err)
	assertions.NotContains(err.Error(), "synthetic-mm-token")
	assertions.False(r.ProviderOK)
	assertions.Equal(1, r.Local)
	assertions.Equal("unsubscribed", subscriptionRows(t, f.st, f.b.SourceID)[0].Status)
	events, err := f.st.LatestConsentEventsContext(t.Context(), []int64{sub.ID})
	requirements.NoError(err)
	assertions.Equal("provider_unsubscribe_failed", events[sub.ID][0].Kind)
}
func TestUnsubscribeByParticipantCoversAllTokens(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 1)
	first := subscriptionRows(t, f.st, f.b.SourceID)[0]
	_, err := SyncSubscriptions(t.Context(), f.st, subscriptionPage(subscriptionToken("synthetic-mm-token-1", "300"), subscriptionToken("synthetic-mm-token-2", "300")), f.policy.Source, 0)
	requirements.NoError(err)
	a := &unsubscribeAPI{}
	r, err := Unsubscribe(t.Context(), f.st, a, f.policy, UnsubscribeSelector{ParticipantID: first.ParticipantID.Int64})
	requirements.NoError(err)
	assertions.Equal(2, r.Local)
	assertions.Equal([]string{"300"}, a.psids)
	for _, sub := range subscriptionRows(t, f.st, f.b.SourceID) {
		assertions.Equal("unsubscribed", sub.Status)
	}
}
func TestUnsubscribeDisabledStillSuppressesLocally(t *testing.T) {
	assertions := assert.New(t)

	f := newSenderFixture(t, 1)
	f.policy.AllowSend = false
	sub := subscriptionRows(t, f.st, f.b.SourceID)[0]
	a := &unsubscribeAPI{}
	r, err := Unsubscribe(t.Context(), f.st, a, f.policy, UnsubscribeSelector{SubscriptionID: sub.ID})
	require.NoError(t, err)
	assertions.Equal(ErrSendsDisabled.Error(), r.Note)
	assertions.Equal(1, r.Local)
	assertions.False(r.ProviderOK)
	assertions.Empty(a.psids)
	assertions.Equal("unsubscribed", subscriptionRows(t, f.st, f.b.SourceID)[0].Status)
}
func TestUnsubscribeCustomAudienceAndSelectorGuards(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 1)
	_, err := SyncSubscriptions(t.Context(), f.st, subscriptionPage(subscriptionToken("synthetic-custom-token", "")), f.policy.Source, 0)
	requirements.NoError(err)
	rows, err := f.st.ListMarketingSubscriptionsContext(t.Context(), f.b.SourceID, store.SubscriptionFilter{Status: "active"})
	requirements.NoError(err)
	requirements.Len(rows, 1)
	a := &unsubscribeAPI{}
	_, err = Unsubscribe(t.Context(), f.st, a, f.policy, UnsubscribeSelector{})
	requirements.Error(err)
	_, err = Unsubscribe(t.Context(), f.st, a, f.policy, UnsubscribeSelector{ParticipantID: 1, SubscriptionID: rows[0].ID})
	requirements.Error(err)
	r, err := Unsubscribe(t.Context(), f.st, a, f.policy, UnsubscribeSelector{SubscriptionID: rows[0].ID})
	requirements.NoError(err)
	assertions.True(r.ProviderOK)
	assertions.Equal([]string{"synthetic-custom-token"}, a.tokens)
	assertions.Empty(a.psids)
}
