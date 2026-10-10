package daemonclient_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type nativeTagsStore struct {
	*store.Store

	change        *emailtags.Change
	fail          bool
	catalog       []emailtags.Tag
	controlCalls  []inboxcontrol.Request
	rejectPreview bool
}

// This controlled backend exercises the stable HTTP wire contract. Native
// provider, lease and receipt behavior has separate real-client integration tests.
func (s *nativeTagsStore) ResolveMessageTagTarget(_ context.Context, id int64, mailbox string) (inboxcontrol.Target, error) {
	provider := "imap"
	if s.catalog != nil {
		provider = "gmail"
	}
	target := inboxcontrol.Target{SourceID: 2, SourceType: provider, SourceIdentifier: "owner@example.com", AccountID: "owner@example.com", Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "native-7"}
	if provider == "imap" {
		target.Mailbox, target.UIDValidity, target.UID = "INBOX", 1, 7
	}
	return target, nil
}

func (s *nativeTagsStore) ControlInbox(ctx context.Context, request inboxcontrol.Request, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, _ func(context.Context) (func(), error)) (*inboxcontrol.Result, error) {
	if err := authorize(ctx, principal, request); err != nil {
		return nil, err
	}
	if s.rejectPreview && request.DryRun {
		return nil, inboxcontrol.ErrDenied
	}
	s.controlCalls = append(s.controlCalls, request)
	before := inboxcontrol.State{Target: *request.Target, Inbox: new(true), Read: new(false), Tags: []string{"Old"}, ObservedAt: time.Now().UTC()}
	if s.catalog != nil {
		before.Tags = []string{"Label_1"}
	}
	projected := before
	projected.Tags = emailtags.Project(before.Tags, *request.Tags, request.Target.SourceType == "imap")
	if request.DryRun {
		return &inboxcontrol.Result{Before: &before, Projected: &projected, PreviewToken: "synthetic-preview", ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
	if request.PreviewToken != "synthetic-preview" || request.Expected == nil || request.IdempotencyKey == "" {
		return nil, inboxcontrol.ErrInvalid
	}
	return &inboxcontrol.Result{Before: request.Expected, After: &projected, Receipt: &inboxcontrol.Receipt{ID: "signed-tag-receipt", Status: inboxcontrol.StatusVerified}}, nil
}

func TestMessageTagsPreviewFailureDoesNotClaimVerified(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := &nativeTagsStore{Store: testutil.NewTestStore(t), rejectPreview: true}
	srv := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	defer srv.Close()
	c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	requirements.NoError(err)
	defer func() { assert.NoError(t, c.Close()) }()
	result, err := c.MessageTags(t.Context(), 7, &emailtags.Change{Add: []string{"Next"}}, "")
	var failure *emailtags.Error
	requirements.ErrorAs(err, &failure)
	assertions.Equal("insufficient_scope", failure.Code)
	requirements.NotNil(result)
	assertions.False(result.Verified, "a verified read is not a verified mutation")
	assertions.Empty(st.controlCalls)
}

func TestMessageTagsClientUsesSignedInboxControl(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := &nativeTagsStore{Store: testutil.NewTestStore(t)}
	srv := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	defer srv.Close()
	c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	requirements.NoError(err)
	defer func() { assert.NoError(t, c.Close()) }()
	result, err := c.MessageTags(t.Context(), 7, &emailtags.Change{Add: []string{"Next"}, Remove: []string{"Old"}, Mailbox: "INBOX"}, "")
	requirements.NoError(err)
	assertions.True(result.Verified)
	assertions.Equal([]string{"Next"}, result.Tags)
	requirements.Len(st.controlCalls, 2)
	assertions.True(st.controlCalls[0].DryRun)
	assertions.False(st.controlCalls[1].DryRun)
	assertions.NotEmpty(st.controlCalls[1].PreviewToken)
	assertions.NotEmpty(st.controlCalls[1].IdempotencyKey)
	assertions.Equal("INBOX", st.controlCalls[1].Target.Mailbox)
	assertions.Empty(st.controlCalls[1].Tags.Mailbox)
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
	assertions := assert.New(t)
	requirements := require.New(t)
	catalog := make([]emailtags.Tag, 5000)
	for i := range catalog {
		catalog[i] = emailtags.Tag{ID: "Label_" + strconv.Itoa(i), Name: strings.Repeat("Category", 32)}
	}
	encoded, err := json.Marshal(catalog)
	requirements.NoError(err)
	requirements.Greater(len(encoded), 1<<20)
	st := &nativeTagsStore{Store: testutil.NewTestStore(t), catalog: catalog}
	srv := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(srv.Close)
	c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { _ = c.Close() })
	read, err := c.MessageTags(t.Context(), 7, nil, "")
	requirements.NoError(err)
	assertions.Equal(catalog, read.AvailableTags)
	change := &emailtags.Change{Add: []string{"Label_0"}}
	updated, err := c.MessageTags(t.Context(), 7, change, "")
	requirements.NoError(err)
	assertions.Equal(catalog, updated.AvailableTags)
	st.fail = true
	partial, err := c.MessageTags(t.Context(), 7, change, "")
	requirements.Error(err)
	requirements.NotNil(partial)
	assertions.Equal(catalog, partial.AvailableTags)
}
func TestMessageTagsClientPreservesPartialResult(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := &nativeTagsStore{Store: testutil.NewTestStore(t)}
	srv := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(srv.Close)
	c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(c.Close()) })
	read, err := c.MessageTags(t.Context(), 7, nil, "INBOX")
	requirements.NoError(err)
	assertions.Equal("INBOX", read.Mailbox)
	change := &emailtags.Change{Add: []string{"Next"}, Mailbox: "INBOX", DryRun: true}
	_, err = c.MessageTags(t.Context(), 7, change, "")
	requirements.NoError(err)
	requirements.NotNil(st.change)
	assertions.True(st.change.DryRun)
	st.fail = true
	result, err := c.MessageTags(t.Context(), 7, change, "")
	requirements.Error(err)
	requirements.NotNil(result)
	var failure *emailtags.Error
	requirements.ErrorAs(err, &failure)
	assertions.Equal("remote_unknown", failure.Code)
	requirements.NotNil(failure.Result)
	assertions.Equal(result.Tags, failure.Result.Tags)
}

func TestMessageTagsClientResponseLossIsUnknownWrite(t *testing.T) {
	for _, scenario := range []string{"disconnect", "invalid JSON"} {
		t.Run(scenario, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			calls := atomic.Int32{}
			st := &nativeTagsStore{Store: testutil.NewTestStore(t)}
			router := api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/inbox/control" {
					router.ServeHTTP(w, r)
					return
				}
				data, err := io.ReadAll(r.Body)
				if !assertions.NoError(err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var request inboxcontrol.Request
				if !assertions.NoError(json.Unmarshal(data, &request)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(data))
				if request.DryRun {
					router.ServeHTTP(w, r)
					return
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, r)
				if !assertions.Equal(http.StatusOK, recorder.Code, recorder.Body.String()) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				calls.Add(1)
				if scenario == "disconnect" {
					hijacker, ok := w.(http.Hijacker)
					if !assertions.True(ok) {
						return
					}
					conn, _, err := hijacker.Hijack()
					if !assertions.NoError(err) {
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
			requirements.NoError(err)
			t.Cleanup(func() { _ = c.Close() })
			result, err := c.MessageTags(t.Context(), 7, &emailtags.Change{Add: []string{"Next"}}, "")
			requirements.Error(err)
			var failure *emailtags.Error
			requirements.ErrorAs(err, &failure)
			assertions.Equal("remote_unknown", failure.Code)
			assertions.Contains(failure.Message, "inspect the receipt before retrying")
			requirements.NotNil(result)
			assertions.NotEmpty(result.IdempotencyKey)
			assertions.Empty(result.ReceiptStatus, "a lost response cannot prove the stored receipt status")
			assertions.False(result.Verified)
			assertions.Equal(int32(1), calls.Load())
		})
	}
}
