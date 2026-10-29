package hybrid

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestParticipantFiltersRequireFullValueSupport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "synthetic-filter-policy")
	require.NoError(err)
	support, err := st.EnsureConversation(source.ID, "support", "")
	require.NoError(err)
	target, err := st.EnsureConversation(source.ID, "target", "")
	require.NoError(err)
	require.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "support", "full"))
	require.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "target", "full"))
	shared, err := st.ResolveConversationParticipantContext(t.Context(), support, store.ConversationParticipantIdentity{IdentifierType: "slack", IdentifierValue: "synthetic:shared", Email: "cold-only@hidden.test", DisplayName: "Cold Only Contact"})
	require.NoError(err)
	other, err := st.ResolveConversationParticipantContext(t.Context(), target, store.ConversationParticipantIdentity{IdentifierType: "slack", IdentifierValue: "synthetic:shared"})
	require.NoError(err)
	require.Equal(shared.ParticipantID, other.ParticipantID)
	// A shared row may retain both address kinds, each with room-specific support.
	_, err = st.DB().Exec(st.Rebind(`UPDATE participants SET phone_number=? WHERE id=?`), "+15555550101", shared.ParticipantID)
	require.NoError(err)
	_, err = st.RecordContactObservationContext(t.Context(), shared.ParticipantID, store.ParticipantContactObservationInput{SourceID: &source.ID, ConversationID: &support, AddressKind: store.ContactAddressPhone, OriginalValue: "+15555550101", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
	require.NoError(err)

	messageID, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: target, SourceMessageID: "full-target", MessageType: "slack", SenderID: sql.NullInt64{Int64: shared.ParticipantID, Valid: true}})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipientsContext(t.Context(), messageID, "from", []int64{shared.ParticipantID}, []string{""}))

	check := func(want int64) {
		fuzzy, err := BuildFilter(t.Context(), st.DB(), st.Rebind, search.Parse("from:hidden.test to:cold-only cc:hidden.test bcc:cold-only"))
		require.NoError(err)
		for _, groups := range [][][]int64{fuzzy.SenderGroups, fuzzy.ToGroups, fuzzy.CcGroups, fuzzy.BccGroups} {
			assert.Equal([][]int64{{want}}, groups)
		}
		for _, filter := range []query.MessageFilter{
			{Sender: "cold-only@hidden.test"}, {Sender: "+15555550101"}, {Recipient: "cold-only@hidden.test"}, {Domain: "hidden.test"},
		} {
			exact, err := BuildFilter(t.Context(), st.DB(), st.Rebind, nil)
			require.NoError(err)
			require.NoError(ApplyMessageFilter(t.Context(), st.DB(), st.Rebind, &exact, filter))
			groups := exact.SenderExactGroups
			if filter.Recipient != "" {
				groups = exact.RecipientAnyGroups
			}
			if filter.Domain != "" {
				groups = exact.SenderGroups
			}
			assert.Equal([][]int64{{want}}, groups, "%+v", filter)
		}
	}
	require.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "support", "cold"))
	check(noMatchSentinel)
	require.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "support", "full"))
	check(shared.ParticipantID)
}
