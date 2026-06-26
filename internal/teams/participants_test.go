package teams

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveParticipant_EmailUserUsesIDAsEmail(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st, nil)
	id := &Identity{ID: "alice@outlook.com", DisplayName: "Alice", UserIdentityType: "emailUser"}
	pid, err := r.resolve(context.Background(), id)
	require.NoError(t, err)
	assert.NotZero(t, pid)
}

func TestResolveParticipant_AADUserResolvesMail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fake := &fakeUserLookup{mail: map[string]string{"obj-1": "bob@example.com"}}
	r := newParticipantResolver(st, fake)
	id := &Identity{ID: "obj-1", DisplayName: "Bob", UserIdentityType: "aadUser"}
	pid, err := r.resolve(context.Background(), id)
	require.NoError(err)
	assert.NotZero(pid)

	_, err = r.resolve(context.Background(), id) // cache hit
	require.NoError(err)
	assert.Equal(1, fake.calls)
}

func TestResolveParticipant_NilReturnsZero(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st, nil)
	pid, err := r.resolve(context.Background(), nil)
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
