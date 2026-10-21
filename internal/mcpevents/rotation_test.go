package mcpevents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
)

func TestRotationCancelsPausedDeliveryAndRebindsIdenticalBytes(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	s, f, req := eventService(t)
	result, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	oldBody, newBody := make(chan []byte, 1), make(chan []byte, 1)
	oldCancelled := make(chan struct{})
	var sends atomic.Int32
	rotatedSecret := []byte(strings.Repeat("r", 32))
	s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		var object map[string]json.RawMessage
		if !Assert.NoError(t, json.Unmarshal(body, &object)) {
			return
		}
		if challenge, ok := object["challenge"]; ok {
			if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]json.RawMessage{"challenge": challenge})) {
				return
			}
			return
		}
		if sends.Add(1) == 1 {
			oldBody <- body
			<-r.Context().Done()
			close(oldCancelled)
			return
		}
		Assert.Equal(t, webhookSignature(rotatedSecret, r.Header.Get("Webhook-Id"), r.Header.Get("Webhook-Timestamp"), body)+" "+webhookSignature(make([]byte, 32), r.Header.Get("Webhook-Id"), r.Header.Get("Webhook-Timestamp"), body), r.Header.Get("Webhook-Signature"))
		newBody <- body
		rw.WriteHeader(http.StatusNoContent)
	})
	appendReceipt(t, s, f, 1)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var before []byte
	select {
	case before = <-oldBody:
	case <-time.After(10 * time.Second):
		require.FailNow("old delivery not started")
	}
	// A TTL renewal must leave this in-flight generation and pending attempt intact.
	_, err = s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	predecessor, err := s.st.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	assert.Equal(int64(1), predecessor.Generation)
	assert.Equal(1, predecessor.AttemptCount)
	req.Delivery.Secret = "whsec_" + base64.StdEncoding.EncodeToString(rotatedSecret)
	rotated, err := s.Subscribe(t.Context(), s.principal, req)
	require.NoError(err)
	assert.Equal(result.ID, rotated.ID)
	select {
	case <-oldCancelled:
	case <-time.After(10 * time.Second):
		require.FailNow("old generation request not cancelled")
	}
	select {
	case after := <-newBody:
		assert.Equal(before, after)
	case <-time.After(10 * time.Second):
		require.FailNow("new generation pending not sent")
	}
	require.Eventually(func() bool {
		row, err := s.st.GetMCPSubscription(t.Context(), result.ID)
		return err == nil && row.Generation == 2 && row.CursorSeq == 1 && row.PendingSeq == 0
	}, 10*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.FailNow("Run did not join rotated worker")
	}
}
