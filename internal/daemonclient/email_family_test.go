package daemonclient_test

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
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

func TestEmailFamilyDecisionClientReadsValidResponseLargerThanSixteenMiB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	snippet := strings.Repeat("x", (16<<20)+1024)
	encoded, err := json.Marshal(store.EmailFamilyDecisionResult{
		Decision: store.EmailFamilyDecision{ID: 1, Kind: "handled", State: "active", MessageIDs: []int64{7}},
		Family: &store.EmailFamily{
			AnchorMessageID: 7,
			Members:         []store.EmailFamilyMember{{MessageID: 7, Snippet: snippet}},
			Decisions:       []store.EmailFamilyDecision{},
		},
	})
	require.NoError(err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	t.Cleanup(srv.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })

	result, err := client.GetEmailFamilyDecisionContext(t.Context(), 1, store.EmailFamilyOptions{})
	require.NoError(err)
	require.NotNil(result.Family)
	require.Len(result.Family.Members, 1)
	assert.Equal(snippet, result.Family.Members[0].Snippet)
}
