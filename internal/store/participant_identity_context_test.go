package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGetParticipantIdentityContext(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	root, err := st.EnsureParticipant("root@example.com", "Root Example", "example.com")
	require.NoError(err)
	const key = "beeper:8:whatsapp:9:@user:x.y"
	alias, err := st.EnsureParticipantByIdentifier("beeper", key, "Alias Example")
	require.NoError(err)
	phone, err := st.EnsureParticipantByPhone("+15550100001", "Phone Example", "sms")
	require.NoError(err)
	service, err := st.ResolveCommunicationServiceContext(ctx, "whatsapp")
	require.NoError(err)
	require.NoError(st.ClassifyParticipantIdentifierServiceContext(
		ctx, "beeper", key, &service.ID, new("account"), new("local-whatsapp_ba_example")))
	_, err = st.LinkParticipants(root, alias)
	require.NoError(err)
	candidate := upsertPairCandidate(t, st, alias, phone, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err)

	details, err := st.GetParticipantIdentityContext(ctx, []int64{root, alias, phone, alias, 0, -1})
	require.NoError(err)
	assert.ElementsMatch([]store.ParticipantIdentityMember{
		{ParticipantID: root, DisplayName: "Root Example", Email: "root@example.com"},
		{ParticipantID: alias, DisplayName: "Alias Example"},
		{ParticipantID: phone, DisplayName: "Phone Example", Phone: "+15550100001"},
	}, details.Members)
	assert.Contains(details.Identifiers, store.ParticipantIdentifierContext{
		ParticipantID: alias, Type: "beeper", Value: key,
		ServiceSlug: "whatsapp", ServiceLabel: "WhatsApp",
		ScopeKind: "account", ScopeValue: "local-whatsapp_ba_example",
	})
	assert.ElementsMatch([]store.ParticipantLinkContext{
		{ParticipantA: root, ParticipantB: alias, OriginKind: "manual"},
		{ParticipantA: alias, ParticipantB: phone, OriginKind: "candidate",
			Source: "archive_observation", Basis: "stable_provider_id"},
	}, details.Links)

	empty, err := st.GetParticipantIdentityContext(ctx, nil)
	require.NoError(err)
	assert.Empty(empty.Members)
	assert.Empty(empty.Identifiers)
	assert.Empty(empty.Links)
}

func TestGetParticipantIdentityContextBatchesAndExcludesOutsideLinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ids := make([]int64, 302)
	wantMembers := make([]store.ParticipantIdentityMember, len(ids))
	wantIdentifiers := make([]store.ParticipantIdentifierContext, len(ids))
	for i := range ids {
		key := fmt.Sprintf("beeper-user-%d", i)
		name := fmt.Sprintf("Example Person %d", i)
		id, err := st.EnsureParticipantByIdentifier("beeper", key, name)
		require.NoError(err)
		ids[i] = id
		wantMembers[i] = store.ParticipantIdentityMember{ParticipantID: id, DisplayName: name}
		wantIdentifiers[i] = store.ParticipantIdentifierContext{ParticipantID: id, Type: "beeper", Value: key}
	}
	outside, err := st.EnsureParticipant("outside@example.com", "Outside Example", "example.com")
	require.NoError(err)
	// One edge crosses the batch boundary; another starts in the second batch.
	for _, pair := range [][2]int64{{ids[0], ids[301]}, {ids[300], ids[301]}, {ids[0], outside}} {
		_, err = st.LinkParticipants(pair[0], pair[1])
		require.NoError(err)
	}

	details, err := st.GetParticipantIdentityContext(t.Context(), ids)
	require.NoError(err)
	assert.ElementsMatch(wantMembers, details.Members)
	assert.ElementsMatch(wantIdentifiers, details.Identifiers)
	assert.ElementsMatch([]store.ParticipantLinkContext{
		{ParticipantA: ids[0], ParticipantB: ids[301], OriginKind: "manual"},
		{ParticipantA: ids[300], ParticipantB: ids[301], OriginKind: "manual"},
	}, details.Links)
}
