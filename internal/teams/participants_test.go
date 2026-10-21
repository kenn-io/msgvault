package teams

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveParticipant_EmailUserUsesIDAsEmail(t *testing.T) {
	st := testutil.NewTestStore(t)
	room := participantResolverTestRoom(t, st)
	r := newParticipantResolver(st, nil)
	id := &Identity{ID: "synthetic@example.test", DisplayName: "Synthetic", UserIdentityType: "emailUser"}
	pid, err := r.resolve(context.Background(), id, room)
	require.NoError(t, err)
	assert.NotZero(t, pid)
}

func TestResolveParticipant_AADUserResolvesMail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	room := participantResolverTestRoom(t, st)
	fake := &fakeUserLookup{mail: map[string]string{"obj-1": "bob@example.com"}}
	r := newParticipantResolver(st, fake)
	id := &Identity{ID: "obj-1", DisplayName: "Bob", UserIdentityType: "aadUser"}
	pid, err := r.resolve(context.Background(), id, room)
	require.NoError(err)
	assert.NotZero(pid)

	_, err = r.resolve(context.Background(), id, room) // cache hit
	require.NoError(err)
	assert.Equal(1, fake.calls)
}

func TestResolveParticipant_NilReturnsZero(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st, nil)
	pid, err := r.resolve(context.Background(), nil, 0)
	require.NoError(t, err)
	assert.Zero(t, pid)
}

type fakeUserLookup struct {
	mail  map[string]string
	calls int
}

func (f *fakeUserLookup) GetUser(_ context.Context, id string) (*GraphUser, error) {
	f.calls++
	return &GraphUser{ID: id, Mail: f.mail[id]}, nil
}

func participantResolverTestRoom(t *testing.T, st *store.Store) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("teams", "owner@example.test")
	require.NoError(t, err)
	room, err := st.EnsureConversation(source.ID, "resolver-room", "Synthetic")
	require.NoError(t, err)
	require.NoError(t, st.SetConversationPolicyOverride(t.Context(), source.ID, "resolver-room", conversationpolicy.Full))
	return room
}
