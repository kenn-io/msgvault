package mcpevents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
)

func TestDelayedSubscriptionRequestPreservesNewerDelivery(t *testing.T) {
	for _, method := range []string{"subscribe", "unsubscribe"} {
		t.Run(method, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			s, f, req := eventService(t)
			result, err := s.Subscribe(t.Context(), s.principal, req)
			require.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			olderCtx, cancelOlder := context.WithCancel(ctx)
			defer cancelOlder()
			committed, resume := make(chan struct{}), make(chan struct{})
			resumeRequest := sync.OnceFunc(func() { close(resume) })
			s.opts.WithOperation = func(callCtx context.Context, fn func() error) error {
				if err := fn(); err != nil {
					return err
				}
				if callCtx == olderCtx {
					row, err := s.st.GetMCPSubscription(callCtx, result.ID)
					if err != nil {
						return err
					}
					if row.Generation == 2 {
						// Let another request and its worker run after this commit.
						close(committed)
						select {
						case <-resume:
						case <-callCtx.Done():
							return callCtx.Err()
						}
					}
				}
				return nil
			}
			started, acknowledge := make(chan struct{}, 1), make(chan struct{})
			s.webhook = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
				var body struct {
					Challenge string `json:"challenge"`
				}
				if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					return
				}
				if body.Challenge != "" {
					Assert.NoError(t, json.NewEncoder(rw).Encode(body))
					return
				}
				started <- struct{}{}
				select {
				case <-acknowledge:
					rw.WriteHeader(http.StatusNoContent)
				case <-r.Context().Done():
				}
			})
			olderDone := make(chan struct{})
			var olderErr error
			go func() {
				defer close(olderDone)
				if method == "unsubscribe" {
					olderErr = s.Unsubscribe(olderCtx, s.principal, UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: req.Delivery})
				} else {
					older := req
					older.Delivery.Secret = "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("r", 32)))
					_, olderErr = s.Subscribe(olderCtx, s.principal, older)
				}
			}()
			t.Cleanup(func() {
				resumeRequest()
				cancelOlder()
				<-olderDone
			})
			select {
			case <-committed:
			case <-ctx.Done():
				require.FailNow("older request did not commit")
			}
			req.Delivery.Secret = "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("n", 32)))
			_, err = s.Subscribe(ctx, s.principal, req)
			require.NoError(err)
			current, err := s.st.GetMCPSubscription(ctx, result.ID)
			require.NoError(err)
			require.Equal(int64(3), current.Generation)
			appendReceipt(t, s, f, 1)
			// Four failed attempts leave the newer worker the final delivery attempt.
			now := time.Now().UTC()
			for range 4 {
				pending, err := s.st.PrepareMCPDelivery(ctx, result.ID, current.Generation, now, func(sub store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
					return json.Marshal(s.envelope(sub, event))
				})
				require.NoError(err)
				require.NotNil(pending)
				_, err = s.st.FinishMCPDelivery(ctx, result.ID, current.Generation, 1, now, http.StatusServiceUnavailable, now)
				require.NoError(err)
			}
			runDone := make(chan error, 1)
			go func() { runDone <- s.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				assert.NoError(<-runDone)
			})
			select {
			case <-started:
			case <-ctx.Done():
				require.FailNow("newer delivery did not start")
			}
			current, err = s.st.GetMCPSubscription(ctx, result.ID)
			require.NoError(err)
			require.Equal(5, current.AttemptCount)
			resumeRequest()
			select {
			case <-olderDone:
				require.NoError(olderErr)
			case <-ctx.Done():
				require.FailNow("older request did not return")
			}
			close(acknowledge)
			require.Eventually(func() bool {
				current, err = s.st.GetMCPSubscription(ctx, result.ID)
				return err == nil && current.CursorSeq == 1 && current.PendingSeq == 0
			}, 5*time.Second, 10*time.Millisecond)
			assert.Equal("acknowledged", current.LastOutcome)
			assert.Zero(current.DeadLetterCount)
		})
	}
}

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
