package gmail

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type triageApplyService interface {
	ApplyTriage(context.Context, inboxcontrol.TriageProposal, inboxcontrol.Principal, func(context.Context) (func(), error)) ([]inboxcontrol.Result, error)
}

type triageApplyFixture struct {
	*triagePreviewFixture
	onCatalog             func()
	mu                    sync.Mutex
	native                map[string][]string
	gate                  chan struct{}
	gateCalls, leaseCalls int
	catalogDeleted        bool
	loseResponse          bool
	afterWrite            func()
}

func newTriageApplyFixture(t *testing.T, count int) *triageApplyFixture {
	t.Helper()
	f := &triageApplyFixture{triagePreviewFixture: newTriagePreviewFixture(t), native: map[string][]string{}, gate: make(chan struct{}, 1)}
	for _, id := range []string{"TodoLabel", "Unrelated"} {
		_, err := f.archive.Store.EnsureLabel(f.source.SourceID, id, id, "user")
		require.NoError(t, err)
	}
	for i := 0; i < count; i++ {
		if i > 0 {
			target := f.input.Items[0].Target
			target.ProviderID = "triage-message-" + strings.Repeat("x", i)
			target.ItemID = f.archive.CreateMessage(target.ProviderID)
			_, err := f.archive.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), Tags: slices.Clone(f.labels), Revision: "12", ObservedAt: f.now})
			require.NoError(t, err)
			f.input.Items = append(f.input.Items, inboxcontrol.TriageItemInput{Target: target, Categories: []string{"todo"}, IdempotencyKey: "triage-key-" + strings.Repeat("x", i)})
		}
		f.native[f.input.Items[i].Target.ProviderID] = slices.Clone(f.labels)
	}
	client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/profile":
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"emailAddress": f.source.AccountID}))
		case r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/labels":
			labels := []map[string]string{{"id": "TodoLabel", "name": "Todo", "type": "user"}}
			if f.catalogDeleted {
				labels = []map[string]string{}
			}
			if f.onCatalog != nil {
				f.onCatalog()
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"labels": labels}))
		case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"):
			id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
			if r.Method == http.MethodPost && strings.HasSuffix(id, "/modify") {
				id = strings.TrimSuffix(id, "/modify")
				f.writes.Add(1)
				// A dispatched write must already have durable evidence and exclude sync.
				item := f.input.Items[0]
				for _, candidate := range f.input.Items {
					if candidate.Target.ProviderID == id {
						item = candidate
					}
				}
				receipt, err := f.archive.Store.LookupInboxReceipt(r.Context(), f.principal.ID, f.source.SourceID, item.IdempotencyKey)
				require.NoError(t, err)
				require.NotNil(t, receipt)
				assert.Equal(t, inboxcontrol.StatusDispatching, receipt.Status)
				lease, err := f.archive.Store.AcquireSyncExecutionContext(r.Context(), f.source.SourceID)
				assert.Error(t, err)
				if lease != nil {
					require.NoError(t, lease.Release())
				}
				var change struct {
					Add    []string `json:"addLabelIds"`
					Remove []string `json:"removeLabelIds"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&change))
				assert.Equal(t, []string{"TodoLabel"}, change.Add)
				assert.Empty(t, change.Remove)
				f.native[id] = append(f.native[id], change.Add...)
				if f.afterWrite != nil {
					f.afterWrite()
				}
				if f.loseResponse {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			} else {
				if f.onRead != nil {
					f.onRead()
				}
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "metadata", r.URL.Query().Get("format"))
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": id, "labelIds": f.native[id], "historyId": "12"}))
		default:
			assert.Fail(t, "unexpected native request", r.Method+" "+r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	f.service.Resolve = func(ctx context.Context, r inboxcontrol.Request) (inboxcontrol.Provider, error) {
		if r.Target != nil {
			if err := f.archive.Store.ValidateInboxTargetContext(ctx, *r.Target); err != nil {
				return nil, err
			}
		}
		return NewInboxProvider(client, f.source), nil
	}
	f.service.AcquireSource = func(ctx context.Context, id int64) (func(), error) {
		f.leaseCalls++
		assert.Equal(t, f.source.SourceID, id)
		lease, err := f.archive.Store.AcquireSyncExecutionContext(ctx, id)
		if err != nil {
			return nil, err
		}
		return func() { assert.NoError(t, lease.Release()) }, nil
	}
	f.service.ReconcileState = func(ctx context.Context, before, after inboxcontrol.State) error {
		return f.archive.Store.ReconcileInboxProviderState(ctx, before.Target, before, after)
	}
	return f
}

func (f *triageApplyFixture) acquireGate(ctx context.Context) (func(), error) {
	f.gateCalls++
	select {
	case f.gate <- struct{}{}:
		return func() { <-f.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Omitting dispatch, double dispatch, nested gate acquisition, or treating our
// own reconciliation as a stale second item breaks this native batch test.
func TestInboxGmailTriageApplyBatchAndExpiredReceiptReplay(t *testing.T) {
	f := newTriageApplyFixture(t, 2)
	apply, ok := any(f.service).(triageApplyService)
	require.True(t, ok, "triage apply must use the native controller")
	p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	results, err := apply.ApplyTriage(t.Context(), *p, f.principal, f.acquireGate)
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, 1, f.gateCalls)
	assert.Equal(t, 1, f.leaseCalls)
	assert.Empty(t, f.gate, "batch must release the daemon gate")
	lease, err := f.archive.Store.AcquireSyncExecutionContext(t.Context(), f.source.SourceID)
	require.NoError(t, err)
	require.NoError(t, lease.Release())
	assert.Equal(t, int64(2), f.writes.Load())
	for _, r := range results {
		require.NotNil(t, r.Receipt)
		assert.Equal(t, inboxcontrol.StatusVerified, r.Receipt.Status)
		require.NotNil(t, r.After)
		assert.True(t, *r.After.Inbox)
		assert.False(t, *r.After.Read)
		assert.ElementsMatch(t, []string{"INBOX", "UNREAD", "STARRED", "Unrelated", "TodoLabel"}, r.After.Tags)
	}
	// A new service instance sharing the durable ledger and signing key can replay
	// after expiry without live provider access, a gate, or a source lease.
	restarted := *f.service
	restarted.Now = func() time.Time { return p.ExpiresAt.Add(time.Hour) }
	restarted.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) {
		assert.Fail(t, "receipt replay reached provider")
		return nil, inboxcontrol.ErrUnavailable
	}
	replay := any(&restarted).(triageApplyService)
	again, err := replay.ApplyTriage(t.Context(), *p, f.principal, nil)
	require.NoError(t, err)
	require.Len(t, again, 2)
	for i := range results {
		assert.Equal(t, results[i].Receipt.ID, again[i].Receipt.ID)
	}
	assert.Equal(t, int64(2), f.writes.Load())
}

func TestInboxGmailTriageApplyRejectsChangesBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *triageApplyFixture, *inboxcontrol.TriageProposal)
		want   error
	}{
		{"incoming after preview", func(t *testing.T, f *triageApplyFixture, _ *inboxcontrol.TriageProposal) {
			f.archive.CreateMessage("late-incoming")
		}, inboxcontrol.ErrPlanChanged},
		{"incoming during live read", func(t *testing.T, f *triageApplyFixture, _ *inboxcontrol.TriageProposal) {
			var once sync.Once
			f.onRead = func() { once.Do(func() { f.archive.CreateMessage("late-live-incoming") }) }
		}, inboxcontrol.ErrPlanChanged},
		{"mapping", func(t *testing.T, f *triageApplyFixture, _ *inboxcontrol.TriageProposal) {
			_, err := f.archive.Store.ReplaceInboxTriageMappings(t.Context(), f.source, map[string]string{"todo": "DifferentLabel"}, 1, inboxcontrol.Principal{ID: "owner-fixture", Owner: true})
			require.NoError(t, err)
		}, inboxcontrol.ErrPlanChanged},
		{"native catalog deleted", func(t *testing.T, f *triageApplyFixture, _ *inboxcontrol.TriageProposal) { f.catalogDeleted = true }, inboxcontrol.ErrDenied},
		{"native read changed", func(t *testing.T, f *triageApplyFixture, _ *inboxcontrol.TriageProposal) {
			f.native["triage-message"] = []string{"INBOX", "STARRED", "Unrelated"}
		}, inboxcontrol.ErrPlanChanged},
		{"native location changed", func(t *testing.T, f *triageApplyFixture, _ *inboxcontrol.TriageProposal) {
			f.native["triage-message"] = []string{"UNREAD", "STARRED", "Unrelated"}
		}, inboxcontrol.ErrPlanChanged},
		{"native unrelated tag changed", func(t *testing.T, f *triageApplyFixture, _ *inboxcontrol.TriageProposal) {
			f.native["triage-message"] = append(f.native["triage-message"], "OtherTag")
		}, inboxcontrol.ErrPlanChanged},
		{"revoked", func(t *testing.T, f *triageApplyFixture, _ *inboxcontrol.TriageProposal) { f.denied.Store(true) }, inboxcontrol.ErrDenied},
		{"expires during final catalog", func(t *testing.T, f *triageApplyFixture, p *inboxcontrol.TriageProposal) {
			var expired atomic.Bool
			original := f.now
			f.service.Now = func() time.Time {
				if expired.Load() {
					return p.ExpiresAt
				}
				return original
			}
			calls := 0
			f.onCatalog = func() {
				calls++
				if calls == 3 {
					expired.Store(true)
				}
			}
		}, inboxcontrol.ErrInvalid},
		{"expired", func(t *testing.T, f *triageApplyFixture, p *inboxcontrol.TriageProposal) { f.now = p.ExpiresAt }, inboxcontrol.ErrInvalid},
		{"changed signed classification", func(t *testing.T, f *triageApplyFixture, p *inboxcontrol.TriageProposal) {
			p.Items[0].Classification = "finished"
		}, inboxcontrol.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTriageApplyFixture(t, 1)
			apply, ok := any(f.service).(triageApplyService)
			require.True(t, ok)
			p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
			require.NoError(t, err)
			tc.change(t, f, p)
			results, err := apply.ApplyTriage(t.Context(), *p, f.principal, f.acquireGate)
			assert.ErrorIs(t, err, tc.want)
			assert.Zero(t, f.writes.Load())
			for _, r := range results {
				assert.Nil(t, r.Receipt)
			}
		})
	}
}

func TestInboxGmailTriageApplyPreservesPartialReceiptsOnNewArrival(t *testing.T) {
	f := newTriageApplyFixture(t, 2)
	apply, ok := any(f.service).(triageApplyService)
	require.True(t, ok)
	p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	f.afterWrite = func() { f.archive.CreateMessage("arrival-after-first-dispatch") }
	results, err := apply.ApplyTriage(t.Context(), *p, f.principal, f.acquireGate)
	require.ErrorIs(t, err, inboxcontrol.ErrPlanChanged)
	require.Len(t, results, 2)
	require.NotNil(t, results[0].Receipt)
	assert.Equal(t, inboxcontrol.StatusVerified, results[0].Receipt.Status)
	assert.Nil(t, results[1].Receipt)
	assert.Equal(t, int64(1), f.writes.Load())
	// Reusing the old batch returns its completed receipt but cannot silently
	// authorize the remaining item across the changed arrival watermark.
	again, err := apply.ApplyTriage(t.Context(), *p, f.principal, f.acquireGate)
	require.ErrorIs(t, err, inboxcontrol.ErrPlanChanged)
	require.Len(t, again, 2)
	require.NotNil(t, again[0].Receipt)
	assert.Equal(t, results[0].Receipt.ID, again[0].Receipt.ID)
	assert.Nil(t, again[1].Receipt)
	assert.Equal(t, int64(1), f.writes.Load())
}

func TestInboxGmailTriageApplyUnknownOutcomeNeverRedispatches(t *testing.T) {
	f := newTriageApplyFixture(t, 2)
	apply, ok := any(f.service).(triageApplyService)
	require.True(t, ok)
	p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	f.loseResponse = true
	results, err := apply.ApplyTriage(t.Context(), *p, f.principal, f.acquireGate)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	require.Len(t, results, 2)
	require.NotNil(t, results[0].Receipt)
	assert.Equal(t, inboxcontrol.StatusUnknown, results[0].Receipt.Status)
	assert.Nil(t, results[1].Receipt)
	assert.Equal(t, int64(1), f.writes.Load())
	restarted := *f.service
	restarted.Now = func() time.Time { return p.ExpiresAt.Add(time.Hour) }
	restarted.Resolve = func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) {
		assert.Fail(t, "unknown replay reached provider")
		return nil, inboxcontrol.ErrUnavailable
	}
	again, err := any(&restarted).(triageApplyService).ApplyTriage(t.Context(), *p, f.principal, nil)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	require.Len(t, again, 2)
	require.NotNil(t, again[0].Receipt)
	assert.Equal(t, results[0].Receipt.ID, again[0].Receipt.ID)
	assert.Equal(t, int64(1), f.writes.Load())
	// Original current authorization is still required to expose the receipt.
	f.denied.Store(true)
	denied, err := apply.ApplyTriage(t.Context(), *p, f.principal, nil)
	require.ErrorIs(t, err, inboxcontrol.ErrDenied)
	for _, r := range denied {
		assert.Nil(t, r.Receipt)
	}
}
