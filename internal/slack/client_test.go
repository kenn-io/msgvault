package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testClient(t *testing.T, f *fakeSlack) *Client {
	t.Helper()
	srv := f.serve()
	c := NewClient(srv.URL, "xoxp-test")
	c.disableRateLimits()
	return c
}

func TestClientRetriesOn429(t *testing.T) {
	f := newFakeSlack(t)
	f.rateLimit429s = 2
	c := testClient(t, f)

	auth, err := c.AuthTest(context.Background())
	require.NoError(t, err, "429s with Retry-After must be retried, not surfaced")
	assert.Equal(t, "T01", auth.TeamID)
	assert.Equal(t, "UME", auth.UserID)
}

func TestClientErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		apiError string
		want     error
	}{
		{"not found", "channel_not_found", ErrNotFound},
		{"auth", "invalid_auth", ErrAuth},
		{"missing scope", "missing_scope", ErrAuth},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := apiError("conversations.history", &apiResponse{Error: tt.apiError})
			assert.ErrorIs(t, err, tt.want)
		})
	}
	err := apiError("conversations.history", &apiResponse{Error: "fatal_error"})
	require.NotErrorIs(t, err, ErrNotFound)
	require.NotErrorIs(t, err, ErrAuth)
}

func TestClientRejectsUnallowlistedMethods(t *testing.T) {
	f := newFakeSlack(t)
	c := testClient(t, f)

	err := c.call(context.Background(), "chat.postMessage", nil, nil)
	require.ErrorContains(t, err, "not allowlisted",
		"the client must refuse methods outside its read-only allowlist before any request is made")
}

func TestValidateSearchScope(t *testing.T) {
	f := newFakeSlack(t)
	c := testClient(t, f)
	require.NoError(t, c.ValidateSearchScope(context.Background()))

	// A token minted without search:read must fail add-slack with
	// remediation instructions, not fail every future sync's sweep.
	f.searchMissingScope = true
	err := c.ValidateSearchScope(context.Background())
	require.ErrorIs(t, err, ErrAuth)
	assert.ErrorContains(t, err, "search:read")
}

func TestClientPagination(t *testing.T) {
	f := newFakeSlack(t)
	f.users = []map[string]any{
		{"id": "U1"}, {"id": "U2"}, {"id": "U3"}, {"id": "U4"}, {"id": "U5"},
	}
	c := testClient(t, f) // fake pageSize is 3: forces two pages

	var ids []string
	require.NoError(t, c.AllUsers(context.Background(), func(u User) error {
		ids = append(ids, u.ID)
		return nil
	}))
	assert.Equal(t, []string{"U1", "U2", "U3", "U4", "U5"}, ids)
}
