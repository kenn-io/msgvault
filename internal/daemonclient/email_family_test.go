package daemonclient_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type largeEmailFamilyStore struct {
	*store.Store
	family *store.EmailFamily
}

func (s *largeEmailFamilyStore) EmailFamilyContext(context.Context, int64, store.EmailFamilyOptions) (*store.EmailFamily, error) {
	return s.family, nil
}

func TestEmailFamilyClientReadsValidResponseLargerThanEightMiB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	subject := strings.Repeat("x", 9<<20)
	backend := &largeEmailFamilyStore{
		Store: testutil.NewTestStore(t),
		family: &store.EmailFamily{
			AnchorMessageID: 7,
			Members:         []store.EmailFamilyMember{{Subject: subject}},
		},
	}
	srv := httptest.NewServer(api.NewServer(&config.Config{}, backend, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(srv.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })

	family, err := client.EmailFamilyContext(t.Context(), 7, store.EmailFamilyOptions{})
	require.NoError(err)
	require.Len(family.Members, 1)
	assert.Len(family.Members[0].Subject, len(subject))
	assert.Equal(subject, family.Members[0].Subject)
}
