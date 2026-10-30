package threads

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestThreadsAuthorOccurrences(t *testing.T) {
	t.Run("rename recycled missing and merged", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		w, s := writerFixture(t)
		o := ItemOrigin{Kind: "conversation", RootID: "100"}
		m := writerReply("101", "100", "100")
		m.Username = new("author_a")
		id := applyWriter(t, w, m, o)
		pid := writerSender(t, s, id)
		first := writerMetadata(t, s, id)
		for _, name := range []string{"author_b", "author_a"} {
			m.Username = &name
			assert.Equal(id, applyWriter(t, w, m, o))
			assert.Equal(pid, writerSender(t, s, id))
		}
		meta := writerMetadata(t, s, id)
		assert.Equal("author_a", meta["first_username"])
		assert.Equal("author_a", meta["current_username"])
		assert.Equal(first["username_first_observed_at"], meta["username_first_observed_at"])
		obs, err := s.ListParticipantObservationsContext(context.Background(), pid, false)
		require.NoError(err)
		require.Len(obs, 2)
		for _, ob := range obs {
			assert.Nil(ob.ProviderUserID)
			assert.Nil(ob.ScopeKind)
			assert.Nil(ob.ScopeValue)
			require.NotNil(ob.ServiceSlug)
			assert.Equal("threads", *ob.ServiceSlug)
			assert.Equal(store.ProvenanceArchiveObservation, ob.Envelope.Source)
			require.NotNil(ob.Envelope.SourceRef)
			assert.Contains(*ob.Envelope.SourceRef, writerSource)
		}
		applyWriter(t, w, m, o)
		replayed, err := s.ListParticipantObservationsContext(context.Background(), pid, false)
		require.NoError(err)
		assert.Equal(obs, replayed, "replay must not fabricate observation history")
		recycled := writerReply("102", "100", "100")
		recycled.Username = new("author_a")
		other := applyWriter(t, w, recycled, o)
		assert.NotEqual(pid, writerSender(t, s, other))
		missing := writerReply("103", "100", "100")
		unknown := applyWriter(t, w, missing, o)
		unknownPID := writerSender(t, s, unknown)
		r := writerRecipients(t, s, unknown)
		assert.Equal("Unknown Threads author", r[0].DisplayName)
		missing.Username = new("later_author")
		applyWriter(t, w, missing, o)
		assert.Equal(unknownPID, writerSender(t, s, unknown))
		assert.Equal("later_author", writerMetadata(t, s, unknown)["first_username"])
		require.NoError(s.MergeParticipantsContext(context.Background(), pid, unknownPID))
		applyWriter(t, w, m, o)
		assert.Equal(unknownPID, writerSender(t, s, id), "identifier must be resolved after each merge")
	})
	t.Run("owner provenance and contradictions", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		w, s := writerFixture(t)
		own := writerReply("201", "200", "200")
		own.Username = new("owner_handle")
		id := applyWriter(t, w, own, ItemOrigin{Kind: "own_replies"})
		pid := writerSender(t, s, id)
		identities, err := s.ListAccountIdentities(w.sourceID)
		require.NoError(err)
		require.Len(identities, 1)
		assert.Equal(writerSource, identities[0].Address)
		var sourceFlag, identityDerived bool
		require.NoError(s.DB().QueryRow(s.Rebind(`SELECT source_is_from_me, identity_is_from_me FROM messages WHERE id=?`), id).Scan(&sourceFlag, &identityDerived))
		assert.True(sourceFlag)
		obs, err := s.ListParticipantObservationsContext(context.Background(), pid, false)
		require.NoError(err)
		require.Len(obs, 2)
		for _, ob := range obs {
			require.NotNil(ob.ProviderUserID)
			assert.Contains(*ob.ProviderUserID, "123")
			assert.Contains(*ob.ProviderUserID, "456")
			assert.Nil(ob.ScopeKind)
			assert.Nil(ob.ScopeValue)
		}
		sameHandle := writerReply("202", "200", "200")
		sameHandle.Username = own.Username
		otherID := applyWriter(t, w, sameHandle, ItemOrigin{Kind: "conversation", RootID: "200"})
		assert.NotEqual(pid, writerSender(t, s, otherID))
		msg, err := s.GetMessageContext(context.Background(), otherID)
		require.NoError(err)
		assert.False(msg.IsFromMe, "matching owner handle grants no ownership")
		explicit := writerReply("203", "200", "200")
		explicit.IsReplyOwnedByMe = new(true)
		explicitID := applyWriter(t, w, explicit, ItemOrigin{Kind: "conversation", RootID: "200"})
		assert.Equal(pid, writerSender(t, s, explicitID))
		assert.Equal(false, writerMetadata(t, s, explicitID)["root_owned"])
		own.IsReplyOwnedByMe = new(false)
		_, err = w.Apply(context.Background(), own, ItemOrigin{Kind: "own_replies"})
		assert.Error(err)
		own.IsReplyOwnedByMe = new(true)
		own.Owner = &MediaReference{ID: "999"}
		_, err = w.Apply(context.Background(), own, ItemOrigin{Kind: "own_replies"})
		assert.Error(err)
	})
	t.Run("omitted username retains observation time", func(t *testing.T) {
		w, s := writerFixture(t)
		m := writerReply("301", "300", "300")
		m.Username = new("author_a")
		id := applyWriter(t, w, m, ItemOrigin{Kind: "conversation", RootID: "300"})
		before := writerMetadata(t, s, id)
		m.Timestamp = m.Timestamp.Add(time.Hour)
		m.Username = nil
		applyWriter(t, w, m, ItemOrigin{Kind: "conversation", RootID: "300"})
		after := writerMetadata(t, s, id)
		assert.Equal(t, before["username_observed_at"], after["username_observed_at"])
	})
}

func TestThreadsAuthorObservationTimes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w, s := writerFixture(t)
	now := writerTime
	w.now = func() time.Time { return now }
	o := ItemOrigin{Kind: "conversation", RootID: "400"}
	m := writerReply("401", "400", "400")
	m.Username = new("author_a")
	id := applyWriter(t, w, m, o)
	first := writerMetadata(t, s, id)
	pid := writerSender(t, s, id)
	now = now.Add(time.Hour)
	m.Username = new("author_b")
	applyWriter(t, w, m, o)
	history, err := s.ListParticipantObservationsContext(t.Context(), pid, false)
	require.NoError(err)
	require.Len(history, 2)
	now = now.Add(time.Hour)
	m.Username = new("author_a")
	applyWriter(t, w, m, o)
	current := writerMetadata(t, s, id)
	assert.NotEqual(first["username_observed_at"], current["username_observed_at"], "A observed again must not reuse its first observation time")
	assert.Equal(now.Format(time.RFC3339Nano), current["username_observed_at"], "A observed again is current evidence, not the old Store row's timestamp")
	assert.Equal(first["username_first_observed_at"], current["username_first_observed_at"])
	assert.Equal("author_a", current["first_username"])
	assert.Equal("author_a", current["current_username"])
	unchanged, err := s.ListParticipantObservationsContext(t.Context(), pid, false)
	require.NoError(err)
	assert.Equal(history, unchanged, "current evidence must not fabricate historical observation rows")
	now = now.Add(time.Hour)
	m.Username = nil
	applyWriter(t, w, m, o)
	assert.Equal(current["username_observed_at"], writerMetadata(t, s, id)["username_observed_at"], "omission preserves the previous observation time")
	now = now.Add(time.Hour)
	m.Username = new("author_a")
	applyWriter(t, w, m, o)
	assert.Equal(now.Format(time.RFC3339Nano), writerMetadata(t, s, id)["username_observed_at"], "a supplied equal username is observed now too")
	canonical := writerMetadata(t, s, id)
	now = now.Add(time.Hour)
	provisional := writerReply("401", "400", "400")
	provisional.Username = new("unverified_provisional_handle")
	applyWriter(t, w, provisional, ItemOrigin{Kind: "sent_provisional", RootID: "400"})
	assert.Equal(canonical, writerMetadata(t, s, id), "a later provisional item cannot advance canonical author evidence")
}

// Hegel's retained counterexample has 30 occurrences: the first two already
// observed A before an accepted merge, the rest have no initial username.
// Merge legitimately retains one closed historical A; later A -> B -> A and
// sparse replay must preserve it without inventing more observations.
func TestThreadsOccurrenceMergedObservationHistoryRegression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w, st := writerFixture(t)
	items := make([]Media, 30)
	pids := make([]int64, 30)
	for n := range items {
		items[n] = writerReply(fmt.Sprint(101+n), "100", "100")
		if n < 2 {
			items[n].Username = new("author_a")
		}
		id := applyWriter(t, w, items[n], ItemOrigin{Kind: "conversation", RootID: "100"})
		pids[n] = writerSender(t, st, id)
	}
	require.NoError(st.MergeParticipantsContext(context.Background(), pids[0], pids[1]))
	for _, name := range []string{"author_a", "author_b", "author_a"} {
		for n := range items {
			items[n].Username = &name
			applyWriter(t, w, items[n], ItemOrigin{Kind: "conversation", RootID: "100"})
			omitted := items[n]
			omitted.Username = nil
			applyWriter(t, w, omitted, ItemOrigin{Kind: "conversation", RootID: "100"})
		}
	}
	applyWriter(t, w, items[0], ItemOrigin{Kind: "conversation", RootID: "100"})
	all, err := st.ListParticipantObservationsContext(context.Background(), pids[1], false)
	require.NoError(err)
	assert.Len(all, 3)
	current, err := st.ListParticipantObservationsContext(context.Background(), pids[1], true)
	require.NoError(err)
	values := []string{}
	for _, ob := range current {
		values = append(values, ob.NormalizedValue)
	}
	assert.ElementsMatch([]string{"author_a", "author_b"}, values)
}
