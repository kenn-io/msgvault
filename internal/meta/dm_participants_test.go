package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDMActorIdentifierFamilies(t *testing.T) {
	assert := assert.New(t)

	fb := DMSource{AppID: "9", PageID: "100", Platform: PlatformMessenger}
	ig := DMSource{AppID: "9", PageID: "100", IGID: "1784", Platform: PlatformInstagram}
	assert.Equal("facebook_messages:1:9:3:100:3:300", DMActorIdentifier(fb, "300"))
	assert.Equal("instagram_messages:1:9:4:1784:3:300", DMActorIdentifier(ig, "300"))
	assert.NotEqual(DMActorIdentifier(fb, "300"), ActorIdentifier("9", "100", "300"))
	assert.NotEqual(DMActorIdentifier(ig, "300"), DMActorIdentifier(fb, "300"))
}
func TestDMScopedIdentityPerPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewSQLiteTestStore(t)
	var ids []int64
	for _, src := range []DMSource{{AppID: "9", PageID: "100", Platform: PlatformMessenger}, {AppID: "9", PageID: "101", Platform: PlatformMessenger}, {AppID: "99", PageID: "100", Platform: PlatformMessenger}} {
		s, err := st.GetOrCreateSource(src.Platform.SourceType(), src.Identifier())
		require.NoError(err)
		id, err := ResolveDMActor(t.Context(), st, s.ID, src, "300", "Example Customer")
		require.NoError(err)
		require.Positive(id)
		ids = append(ids, id)
	}
	assert.NotEqual(ids[0], ids[1])
	assert.NotEqual(ids[0], ids[2])
}
func TestDMExplicitPersonLink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewSQLiteTestStore(t)
	src := DMSource{AppID: "9", PageID: "100", Platform: PlatformMessenger}
	s, err := st.GetOrCreateSource(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	dm, err := ResolveDMActor(t.Context(), st, s.ID, src, "300", "Example Customer")
	require.NoError(err)
	comment, err := st.EnsureParticipantByIdentifierContext(t.Context(), SourceType, ActorIdentifier("9", "100", "300"), "Example Customer")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(comment)
	require.NoError(err)
	_, err = st.LinkParticipants(comment, dm)
	require.NoError(err)
	linked, err := st.PersonForParticipants([]int64{dm})
	require.NoError(err)
	require.NotNil(linked)
	assert.Equal(person.ID, linked.ID)
}
func TestDMNeverRecordsEmail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, f, imp, src := setupDMBackfill(t, PlatformMessenger, 1)
	f.providerEmail = "ignore@example.com"
	sum, err := imp.Import(t.Context(), src, ImportOptions{})
	require.NoError(err)
	require.NotNil(sum)
	assert.Equal(1, sum.MessagesAdded)
	assert.Equal(1, f.nodes["m_0"])
	s, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	m := message(t, st, s.ID, "m_0")
	require.True(m.SenderID.Valid)
	obs, err := st.ListParticipantObservationsContext(t.Context(), m.SenderID.Int64, true)
	require.NoError(err)
	require.Len(obs, 1)
	assert.Equal(store.ContactAddressProviderIdentity, obs[0].AddressKind)
	assert.NotContains(obs[0].OriginalValue, "@")
}
func TestDMUsernameNotAHandle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewSQLiteTestStore(t)
	src := DMSource{AppID: "9", PageID: "100", IGID: "1784", Platform: PlatformInstagram}
	s, err := st.GetOrCreateSource(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	id, err := ResolveDMActor(t.Context(), st, s.ID, src, "300", "example_customer")
	require.NoError(err)
	obs, err := st.ListParticipantObservationsContext(t.Context(), id, true)
	require.NoError(err)
	require.Len(obs, 1)
	assert.Equal(store.ContactAddressProviderIdentity, obs[0].AddressKind)
	require.NotNil(obs[0].ServiceSlug)
	assert.Equal("instagram", *obs[0].ServiceSlug)
	require.NotNil(obs[0].ProviderUserID)
	assert.Equal("instagram_messages:1:9:4:1784:3:300", *obs[0].ProviderUserID)
	assert.NotContains(obs[0].OriginalValue, "example_customer")
}
