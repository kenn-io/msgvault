package mcpevents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func eventService(t *testing.T) (*Service, *storetest.Fixture, SubscribeRequest) {
	t.Helper()
	f := storetest.New(t)
	s, err := New(t.Context(), f.Store, Options{Enabled: true, Sources: []string{"gmail", "gcal"}, KeyPath: filepath.Join(t.TempDir(), "key"), OwnerKey: "synthetic-owner", Retention: 7 * 24 * time.Hour})
	Require.NoError(t, err)
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	return s, f, SubscribeRequest{Name: messageFamily, Arguments: map[string]any{"conversation_id": strconv.FormatInt(f.ConvID, 10)}, Delivery: Delivery{Mode: "webhook", URL: "https://receiver.example.net/events", Secret: "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32))}}
}

func TestSubscribeRefreshRotationReplayAndSafeStatus(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, _, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	initial, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	require.NotNil(initial)
	assert.Equal(int64(1), initial.Generation)
	assert.Equal(int64(1), initial.SecretRevision)
	assert.Equal(int64(1), initial.VerifiedRevision)
	assert.InDelta(time.Now().Add(24*time.Hour).UnixMilli(), first.RefreshBefore, 1000)
	// Durable verification is reused: the callback is deliberately unavailable.
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(http.StatusInternalServerError) })
	renew, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	assert.Equal(first.ID, renew.ID)
	after, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(initial.Generation, after.Generation)
	renewedCiphertext := append([]byte(nil), after.SecretEnc...)
	req.Delivery.Secret = "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32)))
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.Error(err)
	after, err = s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(renewedCiphertext, after.SecretEnc, "failed challenge leaves predecessor untouched")
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	after, err = s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(int64(2), after.Generation)
	assert.Equal(int64(2), after.SecretRevision)
	old, err := decryptSecret(s.key, first.ID, "previous", after.PreviousSecretEnc)
	require.NoError(err)
	assert.Equal(make([]byte, 32), old)
	req.Cursor = &first.Cursor
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	after, err = s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal(int64(3), after.Generation)
	status, err := s.Status(t.Context(), s.principal)
	require.NoError(err)
	encoded, err := json.Marshal(status)
	require.NoError(err)
	assert.NotContains(string(encoded), req.Delivery.URL)
	assert.NotContains(string(encoded), req.Delivery.Secret)
	assert.NotContains(string(encoded), "SecretEnc")
	require.NoError(s.Unsubscribe(t.Context(), s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery}))
	assert.NoError(s.Unsubscribe(t.Context(), s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery}))
	after, err = s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal("unsubscribed", after.State)
}

func TestVerificationActivationRequiresExactPredecessor(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, _, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	entered, release := make(chan struct{}), make(chan struct{})
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	req.Delivery.Secret = "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32)))
	done := make(chan error, 1)
	go func() { _, err := s.Subscribe(t.Context(), s.principal, req); done <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		require.FailNow("challenge did not start")
	}
	require.NoError(s.Unsubscribe(t.Context(), s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery}))
	close(release)
	select {
	case err := <-done:
		require.Error(err)
		var eventErr *Error
		require.ErrorAs(err, &eventErr)
		assert.Equal("concurrent_update", eventErr.Reason)
	case <-time.After(10 * time.Second):
		require.FailNow("challenge did not finish")
	}
	after, err := s.st.GetMCPSubscription(t.Context(), first.ID)
	require.NoError(err)
	assert.Equal("unsubscribed", after.State)
}

func TestCallbackNetworkRunsOutsideOperationGate(t *testing.T) {
	s, _, req := eventService(t)
	var gated atomic.Bool
	s.opts.WithOperation = func(ctx context.Context, fn func() error) error {
		Require.False(t, gated.Swap(true))
		defer gated.Store(false)
		return fn()
	}
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		Assert.False(t, gated.Load())
		var v map[string]any
		if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&v)) {
			return
		}
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]any{"challenge": v["challenge"]})) {
			return
		}
	})
	_, err := s.Subscribe(t.Context(), s.principal, req)
	Require.NoError(t, err)
}

func appendReceipt(t *testing.T, s *Service, f *storetest.Fixture, seq int64) {
	t.Helper()
	var epoch int64
	Require.NoError(t, f.Store.DB().QueryRow(`SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&epoch))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log(seq,epoch,family,kind,scope_kind,scope_id,item_key,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES(?,?,'msgvault.message_archived','message','conversation',?,?,?, ?,FALSE,?,?,?)`), seq, epoch, f.ConvID, fmt.Sprintf("synthetic:%d", seq), f.ConvID, f.Source.ID, now, now, `{"kind":"message","from_me":false}`)
	Require.NoError(t, err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_clock SET head_seq=? WHERE singleton=1`), seq)
	Require.NoError(t, err)
	s.Wake()
}

func TestIndependentWorkersAndShutdownJoin(t *testing.T) {
	require := Require.New(t)
	s, f, req := eventService(t)
	first, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	req.Delivery.URL = "https://other.example.net/events"
	second, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	blocked, fast, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		if r.Header.Get("X-Mcp-Subscription-Id") == first.ID {
			close(blocked)
			<-r.Context().Done()
			close(joined)
		} else {
			Assert.Equal(t, second.ID, r.Header.Get("X-Mcp-Subscription-Id"))
			close(fast)
			rw.WriteHeader(http.StatusNoContent)
		}
	})
	appendReceipt(t, s, f, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		require.FailNow("first worker did not dial")
	}
	select {
	case <-fast:
	case <-time.After(10 * time.Second):
		require.FailNow("slow receiver stalled independent worker")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("Run did not join workers")
	}
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		require.FailNow("callback not cancelled")
	}
}
