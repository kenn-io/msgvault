package daemonclient_test

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type nativeTagsStore struct {
	*store.Store

	change  *emailtags.Change
	fail    bool
	catalog []emailtags.Tag
}

func (s *nativeTagsStore) MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error) {
	s.change = change
	result := &emailtags.Result{MessageID: id, SourceID: 2, Provider: "imap", Mailbox: mailbox, Tags: []string{"Next"}, Before: []string{"Old"}, Verified: true}
	result.AvailableTags = s.catalog
	if s.catalog != nil {
		result.Provider = "gmail"
		result.Tags, result.Before = []string{"Label_0"}, []string{"Label_1"}
	}
	if s.fail {
		return result, emailtags.Failure("remote_unknown", "Read current tags before retrying", result, nil)
	}
	return result, nil
}

func TestMessageTagsClientFullCatalogAboveOneMiB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	catalog := make([]emailtags.Tag, 5000)
	for i := range catalog {
		catalog[i] = emailtags.Tag{ID: "Label_" + strconv.Itoa(i), Name: strings.Repeat("Category", 32)}
	}
	encoded, err := json.Marshal(catalog)
	require.NoError(err)
	require.Greater(len(encoded), 1<<20)
	st := &nativeTagsStore{Store: testutil.NewTestStore(t), catalog: catalog}
	srv := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(srv.Close)
	c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err)
	t.Cleanup(func() { _ = c.Close() })
	read, err := c.MessageTags(t.Context(), 7, nil, "")
	require.NoError(err)
	assert.Equal(catalog, read.AvailableTags)
	change := &emailtags.Change{Add: []string{"Label_0"}}
	updated, err := c.MessageTags(t.Context(), 7, change, "")
	require.NoError(err)
	assert.Equal(catalog, updated.AvailableTags)
	st.fail = true
	partial, err := c.MessageTags(t.Context(), 7, change, "")
	require.Error(err)
	require.NotNil(partial)
	assert.Equal(catalog, partial.AvailableTags)
}
func TestMessageTagsClientPreservesPartialResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &nativeTagsStore{Store: testutil.NewTestStore(t)}
	srv := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(srv.Close)
	c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(c.Close()) })
	read, err := c.MessageTags(t.Context(), 7, nil, "INBOX")
	require.NoError(err)
	assert.Equal("INBOX", read.Mailbox)
	change := &emailtags.Change{Add: []string{"Next"}, Mailbox: "INBOX", DryRun: true}
	_, err = c.MessageTags(t.Context(), 7, change, "")
	require.NoError(err)
	require.NotNil(st.change)
	assert.True(st.change.DryRun)
	st.fail = true
	result, err := c.MessageTags(t.Context(), 7, change, "")
	require.Error(err)
	require.NotNil(result)
	var failure *emailtags.Error
	require.ErrorAs(err, &failure)
	assert.Equal("remote_unknown", failure.Code)
	require.NotNil(failure.Result)
	assert.Equal(result.Tags, failure.Result.Tags)
}

func TestMessageTagsClientResponseLossIsUnknownWrite(t *testing.T) {
	for _, scenario := range []string{"disconnect", "invalid JSON"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			calls := atomic.Int32{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if scenario == "disconnect" {
					hijacker, ok := w.(http.Hijacker)
					if !assert.True(ok) {
						return
					}
					conn, _, err := hijacker.Hijack()
					if !assert.NoError(err) {
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"broken"`))
			}))
			t.Cleanup(srv.Close)
			c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
			require.NoError(err)
			t.Cleanup(func() { _ = c.Close() })
			_, err = c.MessageTags(t.Context(), 7, &emailtags.Change{Add: []string{"Next"}}, "")
			require.Error(err)
			var failure *emailtags.Error
			require.ErrorAs(err, &failure)
			assert.Equal("remote_unknown", failure.Code)
			assert.Contains(failure.Message, "read current tags before retrying")
			assert.Equal(int32(1), calls.Load())
		})
	}
}
