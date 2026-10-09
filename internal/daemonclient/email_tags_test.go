package daemonclient_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

	change  *emailtags.MessageTagChange
	failure *emailtags.MessageTagError
}

func (s *nativeTagsStore) MessageTags(ctx context.Context, id int64, change *emailtags.MessageTagChange, mailbox string, acquireWrite func(context.Context) (func(), error)) (*emailtags.MessageTagResult, error) {
	s.change = change
	if s.failure != nil {
		return s.failure.Result, s.failure
	}
	result := &emailtags.MessageTagResult{MessageID: id, SourceID: 2, Provider: "imap", Mailbox: mailbox, Tags: []string{"Next"}, Before: []string{"Old"}, Verified: true}
	return result, nil
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
	change := &emailtags.MessageTagChange{Add: []string{"Next"}, Mailbox: "INBOX", DryRun: true}
	_, err = c.MessageTags(t.Context(), 7, change, "")
	require.NoError(err)
	require.NotNil(st.change)
	assert.True(st.change.DryRun)
	st.failure = emailtags.Failure("remote_unknown", "Read current tags before retrying", read, nil)
	result, err := c.MessageTags(t.Context(), 7, change, "")
	require.Error(err)
	require.NotNil(result)
	var failure *emailtags.MessageTagError
	require.ErrorAs(err, &failure)
	assert.Equal("remote_unknown", failure.Code)
}

func TestMessageTagsClientPreservesLargePartialError(t *testing.T) {
	for _, tc := range []struct {
		code, message string
		verified      bool
	}{
		{"remote_accepted_local_failed", "Provider tags were verified but could not be saved locally; sync the account", true},
		{"remote_unknown", "Read current tags before retrying", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			observed := &emailtags.MessageTagResult{
				MessageID: 7, SourceID: 2, Provider: "gmail",
				Tags: []string{"INBOX", "Label_42"}, Before: []string{"INBOX"}, Verified: tc.verified,
			}
			// A valid label catalog can exceed the daemon client's 64 KiB error-body cap.
			for i := range 1000 {
				observed.AvailableTags = append(observed.AvailableTags, emailtags.MessageTag{
					ID: fmt.Sprintf("Label_%d", i), Name: strings.Repeat("Example label ", 8),
				})
			}
			st := &nativeTagsStore{
				Store:   testutil.NewTestStore(t),
				failure: emailtags.Failure(tc.code, tc.message, observed, nil),
			}
			srv := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
			t.Cleanup(srv.Close)
			c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(c.Close()) })
			result, err := c.MessageTags(t.Context(), 7, &emailtags.MessageTagChange{Add: []string{"Label_42"}}, "")
			var failure *emailtags.MessageTagError
			require.ErrorAs(err, &failure)
			assert.Equal(tc.code, failure.Code)
			assert.Equal(tc.message, failure.Message)
			require.NotNil(result)
			assert.Equal([]string{"INBOX", "Label_42"}, result.Tags)
			assert.Equal([]string{"INBOX"}, result.Before)
			assert.Equal(tc.verified, result.Verified)
			assert.Len(observed.AvailableTags, 1000, "writing an error must preserve the provider's result")
		})
	}
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
			_, err = c.MessageTags(t.Context(), 7, &emailtags.MessageTagChange{Add: []string{"Next"}}, "")
			require.Error(err)
			var failure *emailtags.MessageTagError
			require.ErrorAs(err, &failure)
			assert.Equal("remote_unknown", failure.Code)
			assert.Equal(int32(1), calls.Load())
		})
	}
}
