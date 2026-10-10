package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

func TestInboxGmailSyncRefreshRejectsStaleOrWrongIdentityAtomically(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, source, id := tagFixture(t, "gmail", "sync-metadata")
	native, err := st.EmailTagTargetContext(t.Context(), id, "")
	requirements.NoError(err)
	requirements.NoError(st.SaveEmailTagsContext(t.Context(), native, &emailtags.Result{Provider: "gmail", Tags: []string{"INBOX", "UNREAD", "LabelNext"}, AvailableTags: []emailtags.Tag{{ID: "LabelNext", Name: "Next"}}, Verified: true}))
	labels, err := st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{"LabelNext": {Name: "Next", Type: "user"}, "INBOX": {Name: "INBOX", Type: "system"}, "UNREAD": {Name: "UNREAD", Type: "system"}})
	requirements.NoError(err)
	observation := store.GmailInboxObservation{Tags: []string{"LabelNext"}, HistoryID: 123, ObservedAt: time.Now().UTC()}
	changed, err := st.RefreshGmailInboxLabelsContext(t.Context(), source.ID, id, "sync-metadata", []int64{labels["LabelNext"]}, observation)
	requirements.NoError(err)
	assertions.True(changed)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "sync-metadata"}
	initial, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(initial)
	assertions.False(*initial.Inbox)
	assertions.True(*initial.Read)
	older := store.GmailInboxObservation{Tags: []string{"INBOX", "UNREAD"}, HistoryID: 122, ObservedAt: observation.ObservedAt.Add(-time.Second)}
	_, err = st.RefreshGmailInboxLabelsContext(t.Context(), source.ID, id, "sync-metadata", []int64{labels["INBOX"], labels["UNREAD"]}, older)
	require.ErrorIs(t, err, inboxcontrol.ErrConflict)
	wrong := observation
	wrong.ObservedAt = observation.ObservedAt.Add(time.Second)
	_, err = st.RefreshGmailInboxLabelsContext(t.Context(), source.ID, id, "different-provider-id", []int64{labels["INBOX"]}, wrong)
	require.Error(t, err)
	assertions.Equal([]string{"Next"}, messageLabels(t, st, id))
	final, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	assertions.Equal(initial, final)
	unknown := store.GmailInboxObservation{ObservedAt: observation.ObservedAt.Add(2 * time.Second)}
	_, err = st.RefreshGmailInboxLabelsContext(t.Context(), source.ID, id, "sync-metadata", nil, unknown)
	requirements.NoError(err)
	assertions.Equal([]string{"Next"}, messageLabels(t, st, id), "unknown metadata must preserve prior labels")
	state, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(state)
	assertions.Nil(state.Inbox)
	assertions.Nil(state.Read)
}
