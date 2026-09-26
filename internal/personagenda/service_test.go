package personagenda

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
)

type fakeIdentityStore struct {
	uids []string
	err  error
}

func (f fakeIdentityStore) ListPersonUIDsContext(context.Context, int64) ([]string, error) {
	return append([]string(nil), f.uids...), f.err
}

func TestPersonUIDFailureClassification(t *testing.T) {
	t.Parallel()
	lookupFailure := errors.New("identity store unavailable")
	for _, tt := range []struct {
		name    string
		store   fakeIdentityStore
		wantErr error
	}{
		{"no stable uid", fakeIdentityStore{}, ErrPersonIdentity},
		{"blank canonical uid", fakeIdentityStore{uids: []string{"   "}}, ErrPersonIdentity},
		{"lookup failure", fakeIdentityStore{err: lookupFailure}, ErrIdentityLookup},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service := Service{Tasks: new(taskclient.KataClient), People: tt.store, Project: "msgvault"}
			_, err := service.List(t.Context(), 42)
			require.ErrorIs(t, err, tt.wantErr)
			if tt.store.err != nil {
				assert.ErrorIs(t, err, lookupFailure)
			}
		})
	}
}

func TestUpdateRejectsInvalidInputBeforeRemoteCall(t *testing.T) {
	t.Parallel()
	service := Service{Tasks: new(taskclient.KataClient), People: fakeIdentityStore{uids: []string{"canonical-uid"}}, Project: "msgvault"}
	for _, input := range []UpdateInput{{}, {List: new(strings.Repeat("x", 81))}, {List: new("bad\x00list")}} {
		_, err := service.Update(t.Context(), 42, "task", input)
		require.ErrorIs(t, err, taskclient.ErrRequestRejected)
	}
}

func TestItemKeepsOnlyHTTPWebURLs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ raw, want string }{
		{"https://tasks.example/item/one", "https://tasks.example/item/one"},
		{"http://tasks.example/item/one", "http://tasks.example/item/one"},
		{"javascript:alert(1)", ""}, {"data:text/html,hello", ""}, {"//tasks.example/one", ""},
		{"/relative", ""}, {"https:opaque", ""},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			item, err := itemFromTask(taskclient.KataTask{WebURL: tt.raw, Status: "closed"})
			require.NoError(t, err)
			assert.Equal(t, tt.want, item.WebURL)
			assert.Equal(t, StateCompleted, item.State)
		})
	}
}
