package emailpush

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type gmailTimer struct {
	delay time.Duration
	fire  chan time.Time
}
type gmailTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan gmailTimer
}

func newGmailClock() *gmailTestClock {
	return &gmailTestClock{now: time.Now(), timers: make(chan gmailTimer, 64)}
}
func (c *gmailTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *gmailTestClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.timers <- gmailTimer{delay: d, fire: ch}
	return ch
}
func (c *gmailTestClock) Fire(timer gmailTimer) {
	c.mu.Lock()
	c.now = c.now.Add(timer.delay)
	now := c.now
	c.mu.Unlock()
	timer.fire <- now
}

type gmailPullStep struct {
	deliveries []Delivery
	status     int
}
type gmailHarness struct {
	t            testing.TB
	ctx          context.Context
	cancel       context.CancelFunc
	clock        *gmailTestClock
	listener     *GmailListener
	steps        chan gmailPullStep
	pulls        chan int32
	acks         chan []string
	reconciles   chan int32
	watches      chan int32
	reported     chan error
	done         chan error
	available    atomic.Bool
	reconciled   atomic.Int32
	watchCount   atomic.Int32
	sourceChecks atomic.Int32
	ackStatus    atomic.Int32
	closeServer  func()
	started      bool
}

func newGmailHarness(tb testing.TB) *gmailHarness {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	h := &gmailHarness{t: tb, ctx: ctx, cancel: cancel, clock: newGmailClock(), steps: make(chan gmailPullStep, 32), pulls: make(chan int32, 64), acks: make(chan []string, 64), reconciles: make(chan int32, 64), watches: make(chan int32, 64), reported: make(chan error, 64), done: make(chan error, 1)}
	h.available.Store(true)
	var pullCount atomic.Int32
	ps, closeServer := testPubSub(tb, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			activeSubscription(tb, w, nil)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":acknowledge") {
			var body struct {
				IDs []string `json:"ackIds"`
			}
			if !assert.NoError(tb, json.UnmarshalRead(r.Body, &body)) {
				return
			}
			status := h.ackStatus.Load()
			h.acks <- body.IDs
			if status != 0 {
				w.WriteHeader(int(status))
				return
			}
			_, err := w.Write([]byte(`{}`))
			assert.NoError(tb, err)
			return
		}
		// Drain the synthetic POST body before waiting, so net/http can observe
		// the client closing an outstanding Pull and cancel its request context.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		h.pulls <- pullCount.Add(1)
		select {
		case step := <-h.steps:
			if step.status != 0 {
				w.WriteHeader(step.status)
				return
			}
			messages := make([]any, len(step.deliveries))
			for i, d := range step.deliveries {
				messages[i] = map[string]any{"ackId": d.AckID, "message": map[string]string{"data": d.Data}}
			}
			if !assert.NoError(tb, json.MarshalWrite(w, map[string]any{"receivedMessages": messages})) {
				return
			}
		case <-r.Context().Done():
		}
	})
	h.closeServer = closeServer
	h.listener = &GmailListener{
		Subscriber: ps, Clock: h.clock, RetryMin: time.Second, RetryMax: time.Minute,
		Watch: func(ctx context.Context) (GmailWatch, error) {
			h.watches <- h.watchCount.Add(1)
			return GmailWatch{EmailAddress: "synthetic@example.com", Expiration: h.clock.Now().Add(time.Hour)}, nil
		},
		ValidateSource: func(ctx context.Context) error {
			h.sourceChecks.Add(1)
			if !h.available.Load() {
				return ErrSourceUnavailable
			}
			return nil
		},
		Reconcile: func(ctx context.Context) error { h.reconciles <- h.reconciled.Add(1); return nil },
		OnError:   func(err error) { h.reported <- err },
	}
	return h
}
func (h *gmailHarness) Start() { h.started = true; go func() { h.done <- h.listener.Run(h.ctx) }() }
func (h *gmailHarness) Close() {
	h.cancel()
	if h.started {
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			assert.Fail(h.t, "listener did not join after cancellation")
		}
	}
	h.closeServer()
}
func gmailAwait[T any](ctx context.Context, tb testing.TB, ch <-chan T) T {
	tb.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		require.NoError(tb, ctx.Err())
		var zero T
		return zero
	}
}
func (h *gmailHarness) RetryTimer() gmailTimer {
	for {
		timer := gmailAwait(h.ctx, h.t, h.clock.timers)
		if timer.delay <= time.Minute {
			return timer
		}
	}
}
func (h *gmailHarness) TakeDone() error {
	select {
	case err := <-h.done:
		h.started = false
		return err
	case <-time.After(10 * time.Second):
		require.FailNow(h.t, "listener did not finish")
		return nil
	}
}

func TestGmailListenerStartupIdentityOrdering(t *testing.T) {
	assertions := assert.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	verified := make(chan struct{})
	release := make(chan struct{})
	originalWatch := h.listener.Watch
	h.listener.Watch = func(ctx context.Context) (GmailWatch, error) {
		close(verified)
		select {
		case <-release:
			return originalWatch(ctx)
		case <-ctx.Done():
			return GmailWatch{}, ctx.Err()
		}
	}
	h.Start()
	gmailAwait(h.ctx, t, verified)
	assertions.Zero(h.reconciled.Load())
	assertions.Empty(h.pulls)
	close(release)
	assertions.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
	assertions.Equal(int32(1), gmailAwait(h.ctx, t, h.pulls))
}

func TestGmailListenerFailedInitialWatchNeverRoutes(t *testing.T) {
	assertions := assert.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.Watch = func(context.Context) (GmailWatch, error) {
		return GmailWatch{}, errors.New("synthetic private profile failure")
	}
	h.Start()
	err := h.TakeDone()
	require.Error(t, err)
	assertions.NotContains(err.Error(), "synthetic private profile")
	assertions.Zero(h.reconciled.Load())
	assertions.Empty(h.pulls)
	assertions.Empty(h.acks)
}

func TestGmailListenerRenewsWhileCatchupWaits(t *testing.T) {
	assertions := assert.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	entered := make(chan struct{})
	h.listener.Reconcile = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
	h.Start()
	gmailAwait(h.ctx, t, entered)
	timer := gmailAwait(h.ctx, t, h.clock.timers)
	assertions.Equal(30*time.Minute, timer.delay)
	assertions.Equal(int32(1), gmailAwait(h.ctx, t, h.watches))
	h.clock.Fire(timer)
	assertions.Equal(int32(2), gmailAwait(h.ctx, t, h.watches))
	assertions.Empty(h.pulls)
}

func TestGmailListenerValidBatchACKWaitsForCatchup(t *testing.T) {
	h := newGmailHarness(t)
	defer h.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	h.listener.Reconcile = func(ctx context.Context) error {
		n := h.reconciled.Add(1)
		h.reconciles <- n
		if n == 2 {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "ack", Data: encodedNotification(t, "synthetic@example.com", "42")}}}
	h.Start()
	gmailAwait(h.ctx, t, entered)
	assert.Empty(t, h.acks)
	close(release)
	assert.Equal(t, []string{"ack"}, gmailAwait(h.ctx, t, h.acks))
	assert.GreaterOrEqual(t, h.sourceChecks.Load(), int32(3))
}

func TestGmailListenerCatchupErrorsNeverACK(t *testing.T) {
	for name, failure := range map[string]error{"partial": errors.New("incomplete acquisition"), "preempted": errors.New("preempted"), "canceled": context.Canceled, "failed": errors.New("provider failure")} {
		t.Run(name, func(t *testing.T) {
			h := newGmailHarness(t)
			defer h.Close()
			h.listener.Reconcile = func(context.Context) error {
				if h.reconciled.Add(1) > 1 {
					return failure
				}
				return nil
			}
			h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "ack", Data: encodedNotification(t, "synthetic@example.com", "42")}}}
			h.Start()
			require.Error(t, gmailAwait(h.ctx, t, h.reported))
			h.cancel()
			require.Error(t, h.TakeDone())
			assert.Empty(t, h.acks)
		})
	}
}

func TestGmailListenerSourceRemovedBeforeACK(t *testing.T) {
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.Reconcile = func(context.Context) error {
		if h.reconciled.Add(1) > 1 {
			h.available.Store(false)
		}
		return nil
	}
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "ack", Data: encodedNotification(t, "synthetic@example.com", "1")}}}
	h.Start()
	require.ErrorIs(t, h.TakeDone(), ErrSourceUnavailable)
	assert.Empty(t, h.acks)
}

func TestGmailListenerPoisonACKRequiresCurrentSource(t *testing.T) {
	h := newGmailHarness(t)
	defer h.Close()
	h.Start()
	gmailAwait(h.ctx, t, h.pulls)
	h.available.Store(false)
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "poison", Data: "!invalid"}}}
	require.ErrorIs(t, h.TakeDone(), ErrSourceUnavailable)
	assert.Empty(t, h.acks)
}

func TestGmailListenerPoisonDoesNotReconcile(t *testing.T) {
	h := newGmailHarness(t)
	defer h.Close()
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "foreign", Data: encodedNotification(t, "foreign@example.com", "1")}, {AckID: "bad", Data: "!invalid"}}}
	h.Start()
	assert.Equal(t, []string{"foreign", "bad"}, gmailAwait(h.ctx, t, h.acks))
	gmailAwait(h.ctx, t, h.pulls)
	gmailAwait(h.ctx, t, h.pulls)
	assert.Equal(t, int32(1), h.reconciled.Load())
}

func TestGmailListenerDuplicateAndOutOfOrderProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		low := hegel.Draw(ht, hegel.Integers(uint64(1), uint64(math.MaxUint64-1)))
		high := hegel.Draw(ht, hegel.Integers(low+1, uint64(math.MaxUint64)))
		h := newGmailHarness(ht)
		defer h.Close()
		for i, id := range []uint64{high, low, high, low} {
			h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: fmt.Sprintf("ack-%d", i), Data: encodedNotification(ht, "synthetic@example.com", strconv.FormatUint(id, 10))}}}
		}
		h.Start()
		for i := range 4 {
			assert.Equal(ht, []string{fmt.Sprintf("ack-%d", i)}, gmailAwait(h.ctx, ht, h.acks))
		}
		assert.Equal(ht, int32(5), h.reconciled.Load())
	})
}

func TestGmailListenerEmptyPullDoesNotReconcile(t *testing.T) {
	h := newGmailHarness(t)
	defer h.Close()
	h.steps <- gmailPullStep{}
	h.Start()
	gmailAwait(h.ctx, t, h.pulls)
	timer := h.RetryTimer()
	h.clock.Fire(timer)
	assert.Equal(t, int32(2), gmailAwait(h.ctx, t, h.pulls))
	assert.Equal(t, int32(1), h.reconciled.Load())
}

func TestGmailListenerRecoveredPullRequestsCatchup(t *testing.T) {
	assertions := assert.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.steps <- gmailPullStep{status: http.StatusServiceUnavailable}
	h.steps <- gmailPullStep{}
	h.Start()
	assertions.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
	gmailAwait(h.ctx, t, h.pulls)
	timer := h.RetryTimer()
	assertions.GreaterOrEqual(timer.delay, 500*time.Millisecond)
	assertions.LessOrEqual(timer.delay, time.Second)
	h.clock.Fire(timer)
	assertions.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles)) // Startup is first; empty recovered Pull is second.
}

func TestGmailListenerRenewalIntervalProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		seconds := hegel.Draw(ht, hegel.Integers(int64(1), int64(14*24*60*60)))
		ttl := time.Duration(seconds) * time.Second
		h := newGmailHarness(ht)
		defer h.Close()
		h.listener.Watch = func(context.Context) (GmailWatch, error) {
			return GmailWatch{EmailAddress: "synthetic@example.com", Expiration: h.clock.Now().Add(ttl)}, nil
		}
		h.Start()
		gmailAwait(h.ctx, ht, h.reconciles)
		timer := gmailAwait(h.ctx, ht, h.clock.timers)
		assert.Equal(ht, min(24*time.Hour, ttl/2), timer.delay)
	})
}

func TestGmailListenerCancellationJoinsRenewal(t *testing.T) {
	h := newGmailHarness(t)
	defer h.Close()
	entered, exited := make(chan struct{}), make(chan struct{})
	h.listener.Watch = func(ctx context.Context) (GmailWatch, error) {
		if h.watchCount.Add(1) > 1 {
			close(entered)
			<-ctx.Done()
			close(exited)
			return GmailWatch{}, ctx.Err()
		}
		return GmailWatch{EmailAddress: "synthetic@example.com", Expiration: h.clock.Now().Add(time.Hour)}, nil
	}
	h.Start()
	gmailAwait(h.ctx, t, h.reconciles)
	h.clock.Fire(gmailAwait(h.ctx, t, h.clock.timers))
	gmailAwait(h.ctx, t, entered)
	h.cancel()
	require.Error(t, h.TakeDone())
	select {
	case <-exited:
	default:
		assert.Fail(t, "Run returned before renewal joined")
	}
	assert.Empty(t, h.acks)
}

func TestGmailListenerInvalidConfiguration(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	listener := &GmailListener{}
	requirements.Error(listener.Run(context.Background()))
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.Watch = func(context.Context) (GmailWatch, error) {
		return GmailWatch{EmailAddress: "", Expiration: h.clock.Now().Add(time.Hour)}, nil
	}
	requirements.Error(h.listener.Run(h.ctx))
	assertions.Zero(h.reconciled.Load())
	assertions.Empty(h.pulls)
}

func TestGmailListenerRenewalWakesQuietPull(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.Start()
	requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
	gmailAwait(h.ctx, t, h.pulls)
	h.clock.Fire(gmailAwait(h.ctx, t, h.clock.timers))
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Empty(h.acks)
}

func TestGmailListenerRenewalCoalescesBlockedCatchup(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	h.listener.Reconcile = func(ctx context.Context) error {
		n := h.reconciled.Add(1)
		h.reconciles <- n
		if n == 2 {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	h.Start()
	gmailAwait(h.ctx, t, h.reconciles)
	gmailAwait(h.ctx, t, h.pulls)
	requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.watches))
	h.clock.Fire(gmailAwait(h.ctx, t, h.clock.timers))
	gmailAwait(h.ctx, t, entered)
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.watches))
	for n := int32(3); n <= 5; n++ {
		h.clock.Fire(gmailAwait(h.ctx, t, h.clock.timers))
		requirements.Equal(n, gmailAwait(h.ctx, t, h.watches))
	}
	// The next renewal timer proves the last Watch result and intent were published.
	gmailAwait(h.ctx, t, h.clock.timers)
	close(release)
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Equal(int32(3), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.pulls))
	requirements.Equal(int32(3), h.reconciled.Load())
}

func TestGmailListenerStartupFailureRetainsCatchup(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.Reconcile = func(context.Context) error {
		n := h.reconciled.Add(1)
		h.reconciles <- n
		if n == 1 {
			return errors.New("temporary startup acquisition failure")
		}
		return nil
	}
	h.Start()
	requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
	h.clock.Fire(h.RetryTimer())
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.pulls))
}

func TestGmailListenerRecoveryFailureRetainsCatchup(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.Reconcile = func(context.Context) error {
		n := h.reconciled.Add(1)
		h.reconciles <- n
		if n == 2 {
			return errors.New("temporary recovery acquisition failure")
		}
		return nil
	}
	h.steps <- gmailPullStep{status: http.StatusServiceUnavailable}
	h.steps <- gmailPullStep{}
	h.Start()
	requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
	gmailAwait(h.ctx, t, h.pulls)
	h.clock.Fire(h.RetryTimer())
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
	h.clock.Fire(h.RetryTimer())
	requirements.Equal(int32(3), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Empty(h.acks)
}

func TestGmailListenerRetryRetainsPendingBatch(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.Reconcile = func(context.Context) error {
		n := h.reconciled.Add(1)
		h.reconciles <- n
		if n == 2 {
			return errors.New("partial acquisition")
		}
		return nil
	}
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "ack", Data: encodedNotification(t, "synthetic@example.com", "42")}}}
	h.Start()
	gmailAwait(h.ctx, t, h.reconciles)
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
	timer := h.RetryTimer()
	requirements.Empty(h.acks)
	h.clock.Fire(timer)
	requirements.Equal(int32(3), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Equal([]string{"ack"}, gmailAwait(h.ctx, t, h.acks))
}

func TestGmailListenerMixedBatchProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(1, 10))
		h := newGmailHarness(ht)
		defer h.Close()
		var valid, poison []string
		deliveries := make([]Delivery, count)
		for i := range count {
			id := fmt.Sprintf("ack-%d", i)
			isValid := hegel.Draw(ht, hegel.Booleans())
			data := encodedNotification(ht, "synthetic@example.com", strconv.Itoa(i+1))
			if isValid {
				valid = append(valid, id)
			} else {
				poison = append(poison, id)
				if hegel.Draw(ht, hegel.Booleans()) {
					data = encodedNotification(ht, "foreign@example.com", "1")
				} else {
					data = "!malformed"
				}
			}
			deliveries[i] = Delivery{AckID: id, Data: data}
		}
		h.steps <- gmailPullStep{deliveries: deliveries}
		h.Start()
		if len(poison) > 0 {
			assert.Equal(ht, poison, gmailAwait(h.ctx, ht, h.acks))
		}
		if len(valid) > 0 {
			assert.Equal(ht, valid, gmailAwait(h.ctx, ht, h.acks))
		}
		gmailAwait(h.ctx, ht, h.pulls)
		gmailAwait(h.ctx, ht, h.pulls)
		want := int32(1)
		if len(valid) > 0 {
			want++
		}
		assert.Equal(ht, want, h.reconciled.Load())
	})
}

func TestGmailListenerACKFailureRetainsBatch(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.ackStatus.Store(http.StatusServiceUnavailable)
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "ack", Data: encodedNotification(t, "synthetic@example.com", "1")}}}
	h.Start()
	requirements.Equal([]string{"ack"}, gmailAwait(h.ctx, t, h.acks))
	h.ackStatus.Store(0)
	h.clock.Fire(h.RetryTimer())
	requirements.Equal([]string{"ack"}, gmailAwait(h.ctx, t, h.acks))
	requirements.Equal(int32(2), h.reconciled.Load())
}

func TestGmailListenerRejectsMissingCallbacks(t *testing.T) {
	for _, missing := range []string{"subscriber", "watch", "source", "reconcile"} {
		t.Run(missing, func(t *testing.T) {
			requirements := require.New(t)
			h := newGmailHarness(t)
			defer h.Close()
			switch missing {
			case "subscriber":
				h.listener.Subscriber = nil
			case "watch":
				h.listener.Watch = nil
			case "source":
				h.listener.ValidateSource = nil
			case "reconcile":
				h.listener.Reconcile = nil
			}
			requirements.Error(h.listener.Run(h.ctx))
			requirements.Zero(h.sourceChecks.Load())
			requirements.Zero(h.watchCount.Load())
			requirements.Empty(h.pulls)
			requirements.Empty(h.acks)
		})
	}
}

func TestGmailListenerCanceledBeforeSetup(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requirements.ErrorIs(h.listener.Run(ctx), context.Canceled)
	requirements.Zero(h.sourceChecks.Load())
	requirements.Zero(h.watchCount.Load())
	requirements.Empty(h.acks)
}

func TestGmailListenerRejectsWatchExpiration(t *testing.T) {
	for _, expiry := range []time.Time{{}, time.UnixMilli(1)} {
		t.Run(strconv.FormatInt(expiry.UnixMilli(), 10), func(t *testing.T) {
			h := newGmailHarness(t)
			defer h.Close()
			h.listener.Watch = func(context.Context) (GmailWatch, error) {
				return GmailWatch{EmailAddress: "synthetic@example.com", Expiration: expiry}, nil
			}
			require.Error(t, h.listener.Run(h.ctx))
			assert.Zero(t, h.reconciled.Load())
			assert.Empty(t, h.acks)
		})
	}
}

func TestGmailListenerRenewalFailureRetriesWithoutBlockingPull(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.Watch = func(context.Context) (GmailWatch, error) {
		n := h.watchCount.Add(1)
		h.watches <- n
		if n == 2 {
			return GmailWatch{}, errors.New("synthetic-private-watch-marker")
		}
		return GmailWatch{EmailAddress: "SYNTHETIC@example.com", Expiration: h.clock.Now().Add(time.Hour)}, nil
	}
	h.Start()
	gmailAwait(h.ctx, t, h.reconciles)
	gmailAwait(h.ctx, t, h.pulls)
	h.clock.Fire(gmailAwait(h.ctx, t, h.clock.timers))
	err := gmailAwait(h.ctx, t, h.reported)
	requirements.NotContains(err.Error(), "synthetic-private-watch-marker")
	timer := h.RetryTimer()
	requirements.GreaterOrEqual(timer.delay, 500*time.Millisecond)
	requirements.LessOrEqual(timer.delay, time.Second)
	h.clock.Fire(timer)
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Equal(int32(3), h.watchCount.Load())
}

func TestGmailListenerRenewedIdentityChangeStops(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.Watch = func(context.Context) (GmailWatch, error) {
		email := "synthetic@example.com"
		if h.watchCount.Add(1) > 1 {
			email = "foreign@example.com"
		}
		return GmailWatch{EmailAddress: email, Expiration: h.clock.Now().Add(time.Hour)}, nil
	}
	h.Start()
	gmailAwait(h.ctx, t, h.reconciles)
	gmailAwait(h.ctx, t, h.pulls)
	h.clock.Fire(gmailAwait(h.ctx, t, h.clock.timers))
	requirements.ErrorIs(h.TakeDone(), ErrSourceUnavailable)
	requirements.Empty(h.acks)
}

func TestGmailListenerInvalidBrokerBatchNeverACK(t *testing.T) {
	for _, deliveries := range [][]Delivery{{{AckID: "", Data: "!invalid"}}, make([]Delivery, 11)} {
		t.Run(strconv.Itoa(len(deliveries)), func(t *testing.T) {
			requirements := require.New(t)
			h := newGmailHarness(t)
			defer h.Close()
			h.steps <- gmailPullStep{deliveries: deliveries}
			h.Start()
			requirements.Error(gmailAwait(h.ctx, t, h.reported))
			h.cancel()
			requirements.Error(h.TakeDone())
			requirements.Empty(h.acks)
			requirements.Equal(int32(1), h.reconciled.Load())
		})
	}
}

func TestGmailListenerRetryableSourceReadRetainsBatch(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	original := h.listener.ValidateSource
	var transient atomic.Bool
	h.listener.ValidateSource = func(ctx context.Context) error {
		if transient.Load() {
			return errors.New("synthetic temporary Store read")
		}
		return original(ctx)
	}
	h.listener.Reconcile = func(context.Context) error {
		n := h.reconciled.Add(1)
		h.reconciles <- n
		if n == 2 {
			transient.Store(true)
		}
		return nil
	}
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "ack", Data: encodedNotification(t, "synthetic@example.com", "1")}}}
	h.Start()
	gmailAwait(h.ctx, t, h.reconciles)
	gmailAwait(h.ctx, t, h.reconciles)
	select {
	case err := <-h.reported:
		requirements.NotErrorIs(err, ErrSourceUnavailable)
	case err := <-h.done:
		h.started = false
		requirements.FailNow("retryable Store read stopped listener", err)
	case <-h.ctx.Done():
		requirements.NoError(h.ctx.Err())
	}
	requirements.Empty(h.acks)
	transient.Store(false)
	h.clock.Fire(h.RetryTimer())
	requirements.Equal([]string{"ack"}, gmailAwait(h.ctx, t, h.acks))
	requirements.Equal(int32(2), h.reconciled.Load())
}

func TestGmailListenerRetryableSourceReadDuringRenewal(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	original := h.listener.ValidateSource
	var transient atomic.Bool
	h.listener.ValidateSource = func(ctx context.Context) error {
		if transient.Load() {
			return errors.New("synthetic temporary Store read")
		}
		return original(ctx)
	}
	h.Start()
	gmailAwait(h.ctx, t, h.reconciles)
	gmailAwait(h.ctx, t, h.pulls)
	transient.Store(true)
	h.clock.Fire(gmailAwait(h.ctx, t, h.clock.timers))
	select {
	case err := <-h.reported:
		requirements.NotErrorIs(err, ErrSourceUnavailable)
	case err := <-h.done:
		h.started = false
		requirements.FailNow("retryable renewal Store read stopped listener", err)
	case <-h.ctx.Done():
		requirements.NoError(h.ctx.Err())
	}
	transient.Store(false)
	h.clock.Fire(h.RetryTimer())
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
}

// Quiet empty pulls should reduce idle requests without inventing catchup. Vary
// positive retry floors and ceilings across the complete time.Duration domain.
func TestGmailListenerIdleBackoffProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		requirements := require.New(ht)
		minimumNS := hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		maximumNS := hegel.Draw(ht, hegel.Integers(minimumNS, int64(math.MaxInt64)))
		minimum, maximum := time.Duration(minimumNS), time.Duration(maximumNS)
		h := newGmailHarness(ht)
		defer h.Close()
		h.listener.RetryMin, h.listener.RetryMax = minimum, maximum
		h.Start()
		requirements.Equal(int32(1), gmailAwait(h.ctx, ht, h.reconciles))
		// Capture renewal before delivering an empty Pull, so idle/failure timers
		// have a distinct observable owner regardless of goroutine scheduling.
		renewal := gmailAwait(h.ctx, ht, h.clock.timers)
		requirements.Equal(30*time.Minute, renewal.delay)
		gmailAwait(h.ctx, ht, h.pulls)
		capDelay := max(minimum, min(maximum, 10*time.Second))
		expected := minimum
		for step := range 8 {
			h.steps <- gmailPullStep{}
			idle := gmailAwait(h.ctx, ht, h.clock.timers)
			requirements.Equal(expected, idle.delay)
			requirements.Empty(h.acks)
			requirements.Equal(int32(1), h.reconciled.Load(), "empty success is not a reconciliation hint")
			h.clock.Fire(idle)
			requirements.Equal(int32(step+2), gmailAwait(h.ctx, ht, h.pulls))
			// Add only the remaining headroom, so even MaxInt64 cannot overflow.
			expected += min(expected, capDelay-expected)
		}
	})
}

func TestGmailListenerIdleBackoffResets(t *testing.T) {
	for _, event := range []string{"valid-delivery", "poison-delivery", "recovered-pull", "renewal"} {
		t.Run(event, func(t *testing.T) {
			requirements := require.New(t)
			h := newGmailHarness(t)
			defer h.Close()
			h.Start()
			requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
			renewal := gmailAwait(h.ctx, t, h.clock.timers)
			requirements.Equal(30*time.Minute, renewal.delay)
			gmailAwait(h.ctx, t, h.pulls)
			for range 5 {
				h.steps <- gmailPullStep{}
				h.clock.Fire(h.RetryTimer())
				gmailAwait(h.ctx, t, h.pulls)
			}
			switch event {
			case "valid-delivery", "poison-delivery":
				data := "!malformed"
				if event == "valid-delivery" {
					data = encodedNotification(t, "synthetic@example.com", "1")
				}
				h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "activity", Data: data}}}
				requirements.Equal([]string{"activity"}, gmailAwait(h.ctx, t, h.acks))
			case "recovered-pull":
				h.steps <- gmailPullStep{status: http.StatusServiceUnavailable}
				requirements.Error(gmailAwait(h.ctx, t, h.reported))
				h.clock.Fire(h.RetryTimer())
				gmailAwait(h.ctx, t, h.pulls)
				h.steps <- gmailPullStep{}
				requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
			case "renewal":
				h.clock.Fire(renewal)
				requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
			}
			gmailAwait(h.ctx, t, h.pulls)
			h.steps <- gmailPullStep{}
			requirements.Equal(time.Second, h.RetryTimer().delay, "activity resets only the idle pacing")
		})
	}
}

func TestGmailListenerIdleBackoffInterrupts(t *testing.T) {
	for _, event := range []string{"cancellation", "renewal"} {
		t.Run(event, func(t *testing.T) {
			requirements := require.New(t)
			h := newGmailHarness(t)
			defer h.Close()
			h.Start()
			requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
			renewal := gmailAwait(h.ctx, t, h.clock.timers)
			requirements.Equal(30*time.Minute, renewal.delay)
			gmailAwait(h.ctx, t, h.pulls)
			for step := range 5 {
				h.steps <- gmailPullStep{}
				idle := h.RetryTimer()
				if step == 4 {
					requirements.Equal(10*time.Second, idle.delay, "default idle pacing reaches its latency cap")
					break // Do not fire the capped timer: the event must interrupt it.
				}
				h.clock.Fire(idle)
				gmailAwait(h.ctx, t, h.pulls)
			}
			if event == "cancellation" {
				h.cancel()
				requirements.ErrorIs(h.TakeDone(), context.Canceled)
				return
			}
			h.clock.Fire(renewal)
			requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
			gmailAwait(h.ctx, t, h.pulls)
			h.steps <- gmailPullStep{}
			requirements.Equal(time.Second, h.RetryTimer().delay)
			requirements.Empty(h.acks)
		})
	}
}

func TestGmailListenerIdleBackoffTinyOddCap(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.RetryMin, h.listener.RetryMax = time.Nanosecond, 3*time.Nanosecond
	h.Start()
	requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
	renewal := gmailAwait(h.ctx, t, h.clock.timers)
	requirements.Equal(30*time.Minute, renewal.delay)
	gmailAwait(h.ctx, t, h.pulls)
	for step, expected := range []time.Duration{time.Nanosecond, 2 * time.Nanosecond, 3 * time.Nanosecond} {
		h.steps <- gmailPullStep{}
		idle := gmailAwait(h.ctx, t, h.clock.timers)
		requirements.Equal(expected, idle.delay)
		requirements.Empty(h.acks)
		requirements.Equal(int32(1), h.reconciled.Load())
		h.clock.Fire(idle)
		requirements.Equal(int32(step+2), gmailAwait(h.ctx, t, h.pulls))
	}
}

func TestGmailListenerPoisonACKRetryBackoff(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	h.listener.RetryMin, h.listener.RetryMax = time.Nanosecond, 4*time.Nanosecond
	h.ackStatus.Store(http.StatusServiceUnavailable)
	h.Start()
	requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
	renewal := gmailAwait(h.ctx, t, h.clock.timers)
	requirements.Equal(30*time.Minute, renewal.delay)
	gmailAwait(h.ctx, t, h.pulls)
	for attempt, expected := range []time.Duration{time.Nanosecond, 2 * time.Nanosecond, 4 * time.Nanosecond, 4 * time.Nanosecond} {
		id := fmt.Sprintf("poison-%d", attempt)
		h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: id, Data: "!malformed"}}}
		requirements.Equal([]string{id}, gmailAwait(h.ctx, t, h.acks))
		requirements.Error(gmailAwait(h.ctx, t, h.reported))
		retry := gmailAwait(h.ctx, t, h.clock.timers)
		requirements.GreaterOrEqual(retry.delay, max(time.Nanosecond, expected/2))
		requirements.LessOrEqual(retry.delay, expected)
		requirements.Equal(int32(1), h.reconciled.Load(), "poison must not create catchup")
		h.clock.Fire(retry)
		gmailAwait(h.ctx, t, h.pulls)
		if attempt == 2 {
			// No-op ACK of an empty Pull cannot forgive the failed poison ACK.
			h.steps <- gmailPullStep{}
			h.clock.Fire(gmailAwait(h.ctx, t, h.clock.timers))
			gmailAwait(h.ctx, t, h.pulls)
		}
	}
	h.ackStatus.Store(0)
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "poison-reset", Data: "!malformed"}}}
	requirements.Equal([]string{"poison-reset"}, gmailAwait(h.ctx, t, h.acks))
	gmailAwait(h.ctx, t, h.pulls)
	h.ackStatus.Store(http.StatusServiceUnavailable)
	h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: "poison-after-reset", Data: "!malformed"}}}
	requirements.Equal([]string{"poison-after-reset"}, gmailAwait(h.ctx, t, h.acks))
	requirements.Error(gmailAwait(h.ctx, t, h.reported))
	requirements.Equal(time.Nanosecond, gmailAwait(h.ctx, t, h.clock.timers).delay, "a successful nonempty poison ACK resets its retry delay")
	requirements.Equal(int32(1), h.reconciled.Load())
	// Leave the retry timer unfired: cancellation must join the waiting listener.
	h.cancel()
	requirements.ErrorIs(h.TakeDone(), context.Canceled)
}

func TestGmailListenerPoisonACKRetryBackoffProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		requirements := require.New(ht)
		minimumNS := hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		maximumNS := hegel.Draw(ht, hegel.Integers(minimumNS, int64(math.MaxInt64)))
		minimum, maximum := time.Duration(minimumNS), time.Duration(maximumNS)
		h := newGmailHarness(ht)
		defer h.Close()
		h.listener.RetryMin, h.listener.RetryMax = minimum, maximum
		h.ackStatus.Store(http.StatusServiceUnavailable)
		h.Start()
		requirements.Equal(int32(1), gmailAwait(h.ctx, ht, h.reconciles))
		renewal := gmailAwait(h.ctx, ht, h.clock.timers)
		requirements.Equal(30*time.Minute, renewal.delay)
		gmailAwait(h.ctx, ht, h.pulls)
		expected := minimum
		for attempt := range 4 {
			id := fmt.Sprintf("poison-%d", attempt)
			h.steps <- gmailPullStep{deliveries: []Delivery{{AckID: id, Data: "!malformed"}}}
			requirements.Equal([]string{id}, gmailAwait(h.ctx, ht, h.acks))
			requirements.Error(gmailAwait(h.ctx, ht, h.reported))
			retry := gmailAwait(h.ctx, ht, h.clock.timers)
			requirements.GreaterOrEqual(retry.delay, max(time.Nanosecond, expected/2))
			requirements.LessOrEqual(retry.delay, expected)
			requirements.Equal(int32(1), h.reconciled.Load())
			h.clock.Fire(retry)
			gmailAwait(h.ctx, ht, h.pulls)
			expected += min(expected, maximum-expected)
		}
	})
}

func TestGmailListenerPoisonACKFailurePreservesValidReceipt(t *testing.T) {
	requirements := require.New(t)
	h := newGmailHarness(t)
	defer h.Close()
	var receiptAllowed atomic.Bool
	original := h.listener.Reconcile
	h.listener.Reconcile = func(ctx context.Context) error {
		if err := original(ctx); err != nil {
			return err
		}
		if h.reconciled.Load() > 1 && !receiptAllowed.Load() {
			return errors.New("synthetic incomplete receipt")
		}
		return nil
	}
	h.ackStatus.Store(http.StatusServiceUnavailable)
	h.Start()
	requirements.Equal(int32(1), gmailAwait(h.ctx, t, h.reconciles))
	renewal := gmailAwait(h.ctx, t, h.clock.timers)
	requirements.Equal(30*time.Minute, renewal.delay)
	gmailAwait(h.ctx, t, h.pulls)
	h.steps <- gmailPullStep{deliveries: []Delivery{
		{AckID: "poison", Data: "!malformed"},
		{AckID: "valid", Data: encodedNotification(t, "synthetic@example.com", "1")},
	}}
	requirements.Equal([]string{"poison"}, gmailAwait(h.ctx, t, h.acks))
	requirements.Error(gmailAwait(h.ctx, t, h.reported))
	// No timer is fired before valid catchup: poison debt must not hold it up.
	requirements.Equal(int32(2), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Error(gmailAwait(h.ctx, t, h.reported))
	requirements.Empty(h.acks, "an incomplete receipt cannot acknowledge the valid delivery")
	retry := gmailAwait(h.ctx, t, h.clock.timers)
	receiptAllowed.Store(true)
	h.ackStatus.Store(0)
	h.clock.Fire(retry)
	requirements.Equal(int32(3), gmailAwait(h.ctx, t, h.reconciles))
	requirements.Equal([]string{"valid"}, gmailAwait(h.ctx, t, h.acks))
}
