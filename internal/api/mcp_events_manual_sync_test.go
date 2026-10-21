package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsPrepareAndFinishDuringManualSync(t *testing.T) {
	for _, path := range []string{"/api/v1/cli/sync", "/api/v1/cli/sync-full", "/api/v1/cli/run"} {
		t.Run(path, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			resource, err := f.Store.GetMCPResource(t.Context(), "source", f.Source.ID)
			require.NoError(err)
			require.NoError(f.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: f.Source.ID, SourceReference: resource.Reference, SourceType: f.Source.SourceType, NativeIdentity: f.Source.Identifier, Known: true, MessageRead: true}))
			now := time.Now().UTC()
			_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Retention: time.Minute, Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
			require.NoError(err)
			input := store.MCPSubscription{ID: fmt.Sprintf("sub_%064x", 1), Principal: "synthetic-owner", Name: "msgvault.message_archived", Arguments: []byte(`{}`), ScopeKind: "conversation", ScopeID: f.ConvID, SourceID: f.Source.ID, CallbackURL: "https://receiver.example.net/events", SecretEnc: []byte("synthetic-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour)}
			require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))
			sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
			require.NoError(err)
			require.NotNil(sub)

			gate := NewSerialOperationGate()
			srv := &Server{operationGate: gate}
			committed := make(chan error, 1)
			finishSync := make(chan struct{})
			syncDone := make(chan struct{})
			// Keep the real manual-sync middleware admission across an archive
			// commit and a paused provider boundary, as the CLI runner does.
			handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				_, err := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build()})
				committed <- err
				<-finishSync
			}))
			go func() {
				defer close(syncDone)
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"args":["sync-slack"]}`)))
			}()
			t.Cleanup(func() { close(finishSync); <-syncDone })
			select {
			case err := <-committed:
				require.NoError(err)
			case <-time.After(30 * time.Second):
				require.FailNow("manual sync did not commit")
			}

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var delivery *store.MCPDelivery
			require.NoError(srv.MCPEventsDeliveryOperation(ctx, func() error {
				var err error
				delivery, err = f.Store.PrepareMCPDelivery(ctx, sub.ID, sub.Generation, time.Now().UTC(), func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
					return []byte(`{"eventId":"synthetic"}`), nil
				})
				return err
			}), "delivery must prepare before the manual sync exits")
			require.NotNil(delivery)
			require.NoError(srv.MCPEventsDeliveryOperation(ctx, func() error {
				return f.Store.CheckMCPDeliveryOccurrence(ctx, sub.ID, sub.Generation, delivery.Subscription.PendingSeq, time.Now().UTC())
			}))
			require.NoError(srv.MCPEventsOperation(ctx, func() error {
				_, err := f.Store.FinishMCPDelivery(ctx, sub.ID, sub.Generation, delivery.Subscription.PendingSeq, time.Now().UTC(), http.StatusNoContent, time.Time{})
				return err
			}))
			settled, err := f.Store.GetMCPSubscription(t.Context(), sub.ID)
			require.NoError(err)
			require.NotNil(settled)
			assert.Equal("active", settled.State)
			assert.Positive(settled.CursorSeq)
			assert.Zero(settled.PendingSeq)
			select {
			case <-syncDone:
				require.FailNow("manual sync exited before Events delivery completed")
			default:
			}
		})
	}
}

func TestMCPEventsManualSyncAdmissionPreservesExclusiveWorkAndDrain(t *testing.T) {
	for _, draining := range []bool{false, true} {
		t.Run(fmt.Sprintf("draining=%v", draining), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				require := require.New(t)
				gate := NewSerialOperationGate()
				srv := &Server{operationGate: gate}
				releaseSync, ok := beginGateWorkBounded(t.Context(), gate, "msgvault sync")
				require.True(ok)
				defer releaseSync()
				entered, finish := make(chan struct{}), make(chan struct{})
				eventsDone := make(chan error, 1)
				go func() {
					eventsDone <- srv.MCPEventsDeliveryOperation(t.Context(), func() error {
						close(entered)
						<-finish
						return nil
					})
				}()
				var finishOnce sync.Once
				finishEvents := func() { finishOnce.Do(func() { close(finish) }) }
				defer func() {
					releaseSync()
					finishEvents()
					synctest.Wait()
					require.NoError(<-eventsDone)
				}()
				synctest.Wait()
				select {
				case <-entered:
				default:
					require.FailNow("Events must enter while the manual sync holds the gate")
				}
				releaseSync()
				finished := make(chan bool, 1)
				if draining {
					gate.StartDrain()
					go func() { finished <- gate.Wait(t.Context()) == nil }()
					err := srv.MCPEventsOperation(t.Context(), func() error { return nil })
					require.Error(err, "draining must reject new Events work")
				} else {
					go func() {
						release, acquired := gate.BeginWorkContext(t.Context())
						if acquired {
							release()
						}
						finished <- acquired
					}()
				}
				synctest.Wait()
				select {
				case <-finished:
					require.FailNow("exclusive work and shutdown must wait for admitted Events work")
				default:
				}
				finishEvents()
				synctest.Wait()
				require.True(<-finished)
			})
		})
	}
}

func TestMCPEventsQueuedBeforeManualSyncStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := NewSerialOperationGate()
		srv := &Server{operationGate: gate}
		ctx, cancel := context.WithCancel(t.Context())
		releaseFirst, acquired := gate.BeginWorkContext(ctx)
		require.True(t, acquired)
		finishSync := make(chan struct{})
		defer func() { close(finishSync); releaseFirst(); cancel(); synctest.Wait() }()
		syncAcquired := make(chan bool, 1)
		go func() {
			release, ok := beginGateWorkBounded(ctx, gate, "msgvault sync")
			syncAcquired <- ok
			if ok {
				<-finishSync
				release()
			}
		}()
		synctest.Wait()
		entered := make(chan struct{})
		go func() {
			_ = srv.MCPEventsOperation(ctx, func() error { close(entered); return nil })
		}()
		synctest.Wait()
		// Keep the request queue occupied after the manual sync is admitted.
		go func() {
			release, ok := gate.BeginRequestWorkContext(ctx, "exclusive archive work")
			if ok {
				release()
			}
		}()
		synctest.Wait()
		releaseFirst()
		synctest.Wait()
		require.True(t, <-syncAcquired)
		select {
		case <-entered:
		default:
			require.FailNow(t, "queued Events must enter when a manual sync starts, even with other requests waiting")
		}
	})
}
