package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
)

func microsoftStateIDs(tb testing.TB, st *store.Store, message int64) []string {
	tb.Helper()
	rows, err := st.DB().Query(st.Rebind(`SELECT l.source_label_id FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=?`), message)
	require.NoError(tb, err)
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(tb, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(tb, rows.Err())
	return ids
}

func TestMicrosoftMailStateSurvivesMovesCategoriesAndNameCollisions(t *testing.T) {
	st, source, id := tagFixture(t, "msmail", "m1")
	assert := assert.New(t)
	require := require.New(t)
	folders, err := st.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{
		"inbox": {Name: "Provider: Unread", Type: "system"}, "archive": {Name: "Provider: Flagged", Type: "user"},
	})
	require.NoError(err)
	read, flag := false, "flagged"
	inbox, archive := folders["inbox"], folders["archive"]
	categories := []string{"Provider: Unread", "Next"}
	_, err = st.ReconcileMicrosoftMailStateContext(t.Context(), id, &inbox, &categories, store.MicrosoftMailState{IsRead: &read, FlagStatus: &flag})
	require.NoError(err)
	assert.ElementsMatch([]string{"inbox", "msmail-category:Provider: Unread", "msmail-category:Next", "msmail-state:unread", "msmail-state:flagged"}, microsoftStateIDs(t, st, id))
	changed, err := st.ReconcileMicrosoftMailLabelsContext(t.Context(), id, &archive, nil)
	require.NoError(err)
	assert.True(changed)
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(err)
	require.NoError(st.SaveEmailTagsContext(t.Context(), target, &emailtags.MessageTagResult{Provider: "msmail", Verified: true, Tags: []string{"Other"}}))
	want := []string{"archive", "msmail-category:Other", "msmail-state:unread", "msmail-state:flagged"}
	assert.ElementsMatch(want, microsoftStateIDs(t, st, id))
	// Later native folders may claim a display name previously assigned to state.
	_, err = st.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{
		"later": {Name: "Provider: Unread (2)", Type: "user"},
	})
	require.NoError(err)
	assert.ElementsMatch(want, microsoftStateIDs(t, st, id))
	var kind, name string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT label_type,name FROM labels WHERE source_id=? AND source_label_id='msmail-state:unread'`), source.ID).Scan(&kind, &name))
	assert.Equal("system", kind)
	assert.NotEqual("Provider: Unread (2)", name)
}

func TestMicrosoftMailStatePartialObservationsPreserveLastKnownProjection(t *testing.T) {
	st, source, id := tagFixture(t, "msmail", "m1")
	folders, err := st.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{"inbox": {Name: "Inbox", Type: "system"}})
	require.NoError(t, err)
	inbox := folders["inbox"]
	hegel.Test(t, func(ht *hegel.T) {
		read := hegel.Draw(ht, hegel.Booleans())
		flagged := hegel.Draw(ht, hegel.Booleans())
		localRead := hegel.Draw(ht, hegel.Booleans())
		flag := "notFlagged"
		if flagged {
			flag = "flagged"
		}
		_, err := st.ReconcileMicrosoftMailStateContext(t.Context(), id, &inbox, nil, store.MicrosoftMailState{IsRead: &read, FlagStatus: &flag})
		require.NoError(ht, err)
		_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=? WHERE id=?`), localRead, id)
		require.NoError(ht, err)
		observations := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 11)))
		for _, observation := range observations[:min(len(observations), 100)] {
			state := store.MicrosoftMailState{}
			if value := observation % 3; value != 0 {
				read = value == 2
				state.IsRead = &read
			}
			if value := observation / 3; value != 0 {
				flag = []string{"", "flagged", "notFlagged", "complete"}[value]
				state.FlagStatus = &flag
				flagged = value == 1
			}
			changed, err := st.ReconcileMicrosoftMailStateContext(t.Context(), id, nil, nil, state)
			require.NoError(ht, err)
			assert.False(ht, changed, "state updates are not folder moves")
			want := []string{"inbox"}
			if !read {
				want = append(want, "msmail-state:unread")
			}
			if flagged {
				want = append(want, "msmail-state:flagged")
			}
			assert.ElementsMatch(ht, want, microsoftStateIDs(ht, st, id))
			var actualLocalRead bool
			require.NoError(ht, st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), id).Scan(&actualLocalRead))
			assert.Equal(ht, localRead, actualLocalRead, "provider state preserves local reader state")
		}
	})
}
