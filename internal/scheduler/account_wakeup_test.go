package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/msgvault/internal/jobctx"
)

const wakeTestAccount = "mail@example.test"

// Bounded bursts before and during provider work have different admission
// meanings: queued hints share the current pass, executing hints share one next
// pass. Holding each boundary makes the invariant independent of runner speed.
func TestWakeAccountBurstProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		assertions := assert.New(ht)
		requirements := require.New(ht)
		// Bound materialized work, not the generator's count domain.
		queued := min(hegel.Draw(ht, hegel.Integers(0, math.MaxInt)), 50)
		running := 1 + min(hegel.Draw(ht, hegel.Integers(0, math.MaxInt)), 49)
		gate := &wakeTestGate{entered: make(chan struct{}, 2), allow: make(chan struct{})}
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		var calls atomic.Int32
		s := New(func(context.Context, string) error {
			calls.Add(1)
			started <- struct{}{}
			<-release
			return nil
		}).WithLogger(slog.New(slog.DiscardHandler)).WithWorkTracker(gate)
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		d, err := s.WakeAccount(wakeTestAccount)
		requirements.NoError(err)
		assertions.Equal(JobStarted, d)
		<-gate.entered
		for range queued {
			d, err = s.WakeAccount(wakeTestAccount)
			requirements.NoError(err)
			assertions.Equal(JobCoalesced, d)
		}
		gate.allow <- struct{}{}
		<-started
		for i := range running {
			d, err = s.WakeAccount(wakeTestAccount)
			requirements.NoError(err)
			want := JobCoalesced
			if i == 0 {
				want = JobPending
			}
			assertions.Equal(want, d)
		}
		release <- struct{}{}
		<-gate.entered
		gate.allow <- struct{}{}
		<-started
		close(release)
		<-s.Stop().Done()
		assertions.Equal(int32(2), calls.Load())
	}, hegel.WithTestCases(1000))
}

type wakeTestGate struct {
	entered chan struct{}
	allow   chan struct{}
}

func (g *wakeTestGate) BeginWork() (func(), bool) { return g.BeginWorkContext(context.Background()) }
func (g *wakeTestGate) BeginWorkContext(ctx context.Context) (func(), bool) {
	g.entered <- struct{}{}
	select {
	case <-g.allow:
		return func() {}, true
	case <-ctx.Done():
		return nil, false
	}
}

func TestWakeReceiptUsesFuturePassAndWakeCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		ordinary := make(chan struct{})
		release := make(chan struct{})
		wakeStarted := make(chan struct{})
		wakeRelease := make(chan struct{})
		var notified atomic.Int32
		s := New(func(context.Context, string) error { close(ordinary); <-release; return nil }).
			WithCompletion(func() { notified.Add(1) }).
			WithAccountWakeSyncFunc(func(context.Context, string) error { close(wakeStarted); <-wakeRelease; return nil })
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		requirements.NoError(s.TriggerSync(wakeTestAccount, false))
		<-ordinary
		result := make(chan error, 1)
		go func() { result <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		synctest.Wait()
		close(release)
		<-wakeStarted
		select {
		case <-result:
			requirements.FailNow("receipt completed from preceding pass")
		default:
		}
		close(wakeRelease)
		requirements.NoError(<-result)
		assertions.Zero(notified.Load())
		<-s.Stop().Done()
	})
}

func TestWakeReceiptOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runErr error
		refuse bool
	}{
		{"success before cleanup cancellation", nil, false},
		{"failure", errors.New("synthetic failure"), false},
		{"bare defer", ErrDeferUntilNextTrigger, false},
		{"gate refusal", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			var gate WorkTracker = &fakeWorkTracker{}
			if tc.refuse {
				gate = wakeRefuseGate{}
			}
			s := New(func(context.Context, string) error { return tc.runErr }).WithWorkTracker(gate)
			requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
			err := s.WakeAccountAndWait(t.Context(), wakeTestAccount)
			if tc.refuse {
				requirements.Error(err)
			} else if tc.runErr != nil {
				requirements.ErrorIs(err, tc.runErr)
			} else {
				requirements.NoError(err)
			}
			<-s.Stop().Done()
		})
	}
}

func TestWakeReceiptRejectsNilErrorPreemption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		tracker := &yieldingWorkTracker{}
		tracker.yield.Store(true)
		s := New(func(ctx context.Context, _ string) error {
			select {
			case <-jobctx.PreemptionSignal(ctx):
			case <-ctx.Done():
			}
			return nil
		}).WithWorkTracker(tracker)
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		err := s.WakeAccountAndWait(t.Context(), wakeTestAccount)
		requirements.Error(err)
		<-s.Stop().Done()
	})
}

func TestWakeReceiptCancellationRemovalAndReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		gate := &wakeTestGate{entered: make(chan struct{}, 4), allow: make(chan struct{})}
		var calls atomic.Int32
		s := New(func(context.Context, string) error { calls.Add(1); return nil }).WithWorkTracker(gate)
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		oldCron := s.cron.Entry(s.jobs[wakeTestAccount]).Job
		ctx, cancel := context.WithCancel(t.Context())
		canceled := make(chan error, 1)
		go func() { canceled <- s.WakeAccountAndWait(ctx, wakeTestAccount) }()
		<-gate.entered
		cancel()
		requirements.ErrorIs(<-canceled, context.Canceled)
		removed := make(chan error, 1)
		go func() { removed <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		synctest.Wait()
		s.RemoveAccount(wakeTestAccount)
		requirements.Error(<-removed)
		_, err := s.WakeAccount(wakeTestAccount)
		requirements.Error(err)
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		oldCron.Run()
		fresh := make(chan error, 1)
		go func() { fresh <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		<-gate.entered
		// Release the stale gated run, then the new one. The old finisher must
		// neither clear the new reservation nor execute the new registration.
		gate.allow <- struct{}{}
		gate.allow <- struct{}{}
		requirements.NoError(<-fresh)
		assertions.Equal(int32(1), calls.Load())
		<-s.Stop().Done()
		_, err = s.WakeAccount(wakeTestAccount)
		requirements.Error(err)
	})
}

func TestWakeReceiptStopAndUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		gate := &wakeTestGate{entered: make(chan struct{}, 1), allow: make(chan struct{})}
		s := New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
		_, err := s.WakeAccount("missing")
		requirements.Error(err)
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		result := make(chan error, 1)
		go func() { result <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		<-gate.entered
		stopped := s.Stop()
		requirements.Error(<-result)
		<-stopped.Done()
	})
}

type wakeRefuseGate struct{}

func (wakeRefuseGate) BeginWork() (func(), bool)                       { return nil, false }
func (wakeRefuseGate) BeginWorkContext(context.Context) (func(), bool) { return nil, false }

func TestWakeRefusedGateClearsQueuedStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		s := New(func(context.Context, string) error { return nil }).WithWorkTracker(wakeRefuseGate{}).WithLogger(logger)
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		requirements.Error(s.WakeAccountAndWait(t.Context(), wakeTestAccount))
		synctest.Wait()
		status := s.Status()
		requirements.Len(status, 1)
		assertions.False(status[0].Queued)
		assertions.False(status[0].Running)
		assertions.Contains(logs.String(), "scheduled sync skipped: daemon is draining")
		assertions.Contains(logs.String(), "email="+wakeTestAccount)
		<-s.Stop().Done()
	})
}

func TestWakeSettledQueuedPassCannotAcceptAnotherReceipt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		gate := &wakeTestGate{entered: make(chan struct{}, 2), allow: make(chan struct{})}
		s := New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		_, err := s.WakeAccount(wakeTestAccount)
		requirements.NoError(err)
		<-gate.entered
		// Exercise the production settlement transition before its finisher
		// runs, the admission race exposed by gate refusal.
		s.mu.Lock()
		pass := s.accounts[wakeTestAccount].active
		settleAccountPassLocked(pass, errors.New("gate refused"))
		s.mu.Unlock()
		result := make(chan error, 1)
		go func() { result <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		synctest.Wait()
		s.mu.RLock()
		joinedOld := len(pass.waiters)
		s.mu.RUnlock()
		assertions.Zero(joinedOld, "settled passes must be closed to new receipts")
		gate.allow <- struct{}{}
		<-gate.entered
		gate.allow <- struct{}{}
		requirements.NoError(<-result)
		<-s.Stop().Done()
	})
}

func TestWakeYieldContinuationRetainsStrictCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		tracker := &yieldingWorkTracker{}
		tracker.yield.Store(true)
		var ordinary, strict atomic.Int32
		s := New(func(context.Context, string) error { ordinary.Add(1); return nil }).WithWorkTracker(tracker)
		s.WithAccountWakeSyncFunc(func(ctx context.Context, _ string) error {
			if strict.Add(1) == 1 {
				<-jobctx.PreemptionSignal(ctx)
				tracker.yield.Store(false)
			}
			return nil
		})
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		requirements.Error(s.WakeAccountAndWait(t.Context(), wakeTestAccount))
		synctest.Wait()
		assertions.Equal(int32(2), strict.Load())
		assertions.Zero(ordinary.Load(), "yield cannot resume through ordinary selector dispatch")
		<-s.Stop().Done()
	})
}

func TestWakeCancellationIsolatesSharedQueuedReceipt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		gate := &wakeTestGate{entered: make(chan struct{}, 1), allow: make(chan struct{})}
		var calls atomic.Int32
		s := New(func(context.Context, string) error { calls.Add(1); return nil }).WithWorkTracker(gate)
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		ctx, cancel := context.WithCancel(t.Context())
		first, second := make(chan error, 1), make(chan error, 1)
		go func() { first <- s.WakeAccountAndWait(ctx, wakeTestAccount) }()
		<-gate.entered
		go func() { second <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		synctest.Wait()
		cancel()
		requirements.ErrorIs(<-first, context.Canceled)
		select {
		case <-second:
			requirements.FailNow("canceling another caller settled this receipt")
		default:
		}
		gate.allow <- struct{}{}
		requirements.NoError(<-second)
		assertions.Equal(int32(1), calls.Load())
		<-s.Stop().Done()
	})
}

func TestWakeReplacementFencesExecutingFinisher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		started := make(chan struct{}, 2)
		oldRelease, newRelease := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		s := New(func(context.Context, string) error {
			call := calls.Add(1)
			started <- struct{}{}
			if call == 1 {
				<-oldRelease
			} else {
				<-newRelease
			}
			return nil
		})
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		oldReceipt, freshReceipt := make(chan error, 1), make(chan error, 1)
		go func() { oldReceipt <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		<-started
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 1 * * *"))
		requirements.Error(<-oldReceipt)
		go func() { freshReceipt <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		<-started
		close(oldRelease)
		synctest.Wait()
		status := s.Status()
		requirements.Len(status, 1)
		assertions.True(status[0].Running)
		select {
		case <-freshReceipt:
			requirements.FailNow("old finisher settled replacement receipt")
		default:
		}
		close(newRelease)
		requirements.NoError(<-freshReceipt)
		<-s.Stop().Done()
	})
}

func TestWakeStopSettlesActiveAndPendingBeforeDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		started, release := make(chan struct{}), make(chan struct{})
		s := New(func(context.Context, string) error { close(started); <-release; return nil })
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		active, pending := make(chan error, 1), make(chan error, 1)
		go func() { active <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		<-started
		go func() { pending <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		synctest.Wait()
		drained := s.Stop()
		requirements.Error(<-active)
		requirements.Error(<-pending)
		select {
		case <-drained.Done():
			requirements.FailNow("Stop reported drain before blocked callback returned")
		default:
		}
		close(release)
		<-drained.Done()
	})
}

func TestWakeExactKeysAndReceiptBeforeAsyncPostSync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		s := New(func(context.Context, string) error { return nil }).WithWorkTracker(&fakeWorkTracker{})
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		for _, key := range []string{"Mail@example.test", " mail@example.test", "mail@example.test ", "other@example.test"} {
			_, err := s.WakeAccount(key)
			requirements.Error(err)
			requirements.Error(s.WakeAccountAndWait(t.Context(), key))
		}
		postStarted := make(chan struct{})
		s.SetVisualPostSyncJob(func(ctx context.Context) error { close(postStarted); <-ctx.Done(); return ctx.Err() })
		requirements.NoError(s.WakeAccountAndWait(t.Context(), wakeTestAccount))
		<-postStarted // provider is still blocked after receipt success
		<-s.Stop().Done()
	})
}

// Coalescing is an outstanding-work invariant, not a lifetime run-count limit.
// Multiple completed passes may each admit a fresh follow-up without losing the
// origin callback or issuing an automatic manual-completion notification.
func TestWakeAcrossPassesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		assertions := assert.New(ht)
		requirements := require.New(ht)
		rounds := 1 + min(hegel.Draw(ht, hegel.Integers(0, math.MaxInt)), 4)
		strict := hegel.Draw(ht, hegel.Booleans())
		trackerKind := hegel.Draw(ht, hegel.Integers(0, 2))
		started, release := make(chan struct{}, 1), make(chan struct{})
		var calls, ordinary, completions atomic.Int32
		run := func() {
			calls.Add(1)
			started <- struct{}{}
			<-release
		}
		s := New(func(context.Context, string) error { ordinary.Add(1); run(); return nil }).
			WithCompletion(func() { completions.Add(1) }).WithLogger(slog.New(slog.DiscardHandler))
		if strict {
			s.WithAccountWakeSyncFunc(func(context.Context, string) error { run(); return nil })
		}
		if trackerKind == 1 {
			s.WithWorkTracker(&fakeWorkTracker{})
		}
		if trackerKind == 2 {
			s.WithWorkTracker(&labelRecordingTracker{})
		}
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		_, err := s.WakeAccount(wakeTestAccount)
		requirements.NoError(err)
		<-started
		for range rounds {
			hints := 1 + min(hegel.Draw(ht, hegel.Integers(0, math.MaxInt)), 19)
			for i := range hints {
				disposition, err := s.WakeAccount(wakeTestAccount)
				requirements.NoError(err)
				want := JobCoalesced
				if i == 0 {
					want = JobPending
				}
				assertions.Equal(want, disposition)
			}
			release <- struct{}{}
			<-started
		}
		close(release)
		<-s.Stop().Done()
		assertions.Equal(int32(rounds+1), calls.Load())
		if strict {
			assertions.Zero(ordinary.Load())
		} else {
			assertions.Equal(calls.Load(), ordinary.Load())
		}
		assertions.Zero(completions.Load())
	}, hegel.WithTestCases(1000))
}

func TestWakeQueuedOrdinaryPassKeepsItsCallbackAndManualOutcome(t *testing.T) {
	for _, origin := range []string{"manual", "ordinary", "cron"} {
		t.Run(origin, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				gate := &wakeTestGate{entered: make(chan struct{}, 4), allow: make(chan struct{})}
				var ordinary, strict, completion atomic.Int32
				incomplete := errors.New("strict pass incomplete")
				s := New(func(context.Context, string) error { ordinary.Add(1); return nil }).WithWorkTracker(gate).
					WithCompletion(func() { completion.Add(1) }).WithAccountWakeSyncFunc(func(context.Context, string) error { strict.Add(1); return incomplete })
				requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
				if origin == "cron" {
					go s.onAccountTick(wakeTestAccount)
				} else {
					requirements.NoError(s.TriggerSync(wakeTestAccount, origin == "manual"))
				}
				<-gate.entered
				result := make(chan error, 1)
				go func() { result <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
				synctest.Wait()
				// A later ordinary tick cannot erase the pending strict intent.
				s.onAccountTick(wakeTestAccount)
				close(gate.allow)
				requirements.ErrorIs(<-result, incomplete)
				synctest.Wait()
				assertions.Equal(int32(1), ordinary.Load(), "an accepted ordinary pass must keep its callback")
				assertions.Equal(int32(1), strict.Load())
				want := int32(0)
				if origin == "manual" {
					want = 1
				}
				assertions.Equal(want, completion.Load(), "strict outcome cannot redefine the preceding manual pass")
				<-s.Stop().Done()
			})
		})
	}
}

func TestWakeStrictPassCannotCompleteYieldedManualScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		tracker := &yieldingWorkTracker{}
		started := make(chan struct{})
		var ordinary, strict, completions, notifiedAtOrdinary atomic.Int32
		s := New(func(ctx context.Context, _ string) error {
			if ordinary.Add(1) == 1 {
				close(started)
				<-jobctx.PreemptionSignal(ctx)
				tracker.yield.Store(false)
			}
			return nil
		}).WithWorkTracker(tracker).WithAccountWakeSyncFunc(func(context.Context, string) error { strict.Add(1); return nil }).
			WithCompletion(func() { completions.Add(1); notifiedAtOrdinary.Store(ordinary.Load()) })
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		requirements.NoError(s.TriggerSync(wakeTestAccount, true))
		<-started
		result := make(chan error, 1)
		go func() { result <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		synctest.Wait()
		tracker.yield.Store(true)
		requirements.NoError(<-result)
		synctest.Wait()
		assertions.Equal(int32(2), ordinary.Load())
		assertions.Equal(int32(1), strict.Load())
		assertions.Equal(int32(1), completions.Load())
		assertions.Equal(int32(2), notifiedAtOrdinary.Load(), "strict success cannot notify before ordinary reconciliation succeeds")
		<-s.Stop().Done()
	})
}

func TestWakeHintsCannotOverwritePendingCronScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		started, release := make(chan struct{}), make(chan struct{})
		var ordinary, strict atomic.Int32
		s := New(func(context.Context, string) error { ordinary.Add(1); return nil }).
			WithAccountWakeSyncFunc(func(context.Context, string) error {
				if strict.Add(1) == 1 {
					close(started)
					<-release
				}
				return nil
			})
		requirements.NoError(s.AddAccount(wakeTestAccount, "0 0 * * *"))
		_, err := s.WakeAccount(wakeTestAccount)
		requirements.NoError(err)
		<-started
		s.onAccountTick(wakeTestAccount)
		result := make(chan error, 1)
		go func() { result <- s.WakeAccountAndWait(t.Context(), wakeTestAccount) }()
		synctest.Wait()
		close(release)
		requirements.NoError(<-result)
		synctest.Wait()
		assertions.Equal(int32(1), ordinary.Load(), "hints must retain the distinct cron scope")
		assertions.Equal(int32(2), strict.Load())
		<-s.Stop().Done()
	})
}
