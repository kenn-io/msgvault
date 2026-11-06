package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"hegel.dev/go/hegel"
)

func TestBeeperEventsDisabledWithoutScheduledJob(t *testing.T) {
	for _, tt := range []struct {
		name                        string
		enabled, events, registered bool
		schedule                    string
	}{
		{"default", true, false, true, "* * * * *"}, {"disabled", false, true, true, "* * * * *"}, {"unscheduled", true, true, true, ""}, {"failed registration", true, true, false, "* * * * *"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.HomeDir = t.TempDir()
			cfg.Beeper.Enabled = tt.enabled
			cfg.Beeper.Events = tt.events
			cfg.Beeper.Schedule = tt.schedule
			require.NoError(t, beeper.SaveToken(cfg.TokensDir(), "synthetic"))
			requests := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				select {
				case requests <- struct{}{}:
				default:
				}
			}))
			defer server.Close()
			cfg.Beeper.URL = server.URL
			stop := startBeeperEvents(t.Context(), cfg, nil, tt.registered, testDiscardLogger())
			select {
			case <-requests:
				assert.Fail(t, "disabled event watcher made an HTTP request")
			case <-time.After(time.Second):
			}
			stop()
			select {
			case <-requests:
				assert.Fail(t, "disabled event watcher made an HTTP request")
			default:
			}
		})
	}
}
func TestBeeperWakeBurstIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		dirty := make(chan struct{}, 1)
		calls := 0
		go runBeeperWakeups(ctx, dirty, func() error { calls++; return nil })
		for range 100000 {
			markBeeperDirty(dirty)
		}
		synctest.Wait()
		assert.Zero(t, calls)
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, 1, calls)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		assert.Equal(t, 1, calls)
	})
}
func TestBeeperWakeContinuousTrafficDoesNotStarve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		dirty := make(chan struct{}, 1)
		calls := 0
		go runBeeperWakeups(ctx, dirty, func() error { calls++; return nil })
		markBeeperDirty(dirty)
		synctest.Wait()
		for range 10 {
			time.Sleep(100 * time.Millisecond)
			markBeeperDirty(dirty)
			synctest.Wait()
		}
		assert.GreaterOrEqual(t, calls, 1, "a fixed window must fire despite a continuously dirty source")
	})
}
func TestBeeperWakeRetriesRejectedAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		dirty := make(chan struct{}, 1)
		calls := 0
		go runBeeperWakeups(ctx, dirty, func() error {
			calls++
			if calls < 3 {
				return errors.New("admission unavailable")
			}
			return nil
		})
		markBeeperDirty(dirty)
		synctest.Wait()
		for want := 1; want <= 3; want++ {
			time.Sleep(time.Second)
			synctest.Wait()
			assert.Equal(t, want, calls)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		assert.Equal(t, 3, calls)
	})
}
func TestBeeperWakeQueuedAndRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		sched := scheduler.New(nil).WithLogger(testDiscardLogger())
		runs := 0
		release := make(chan struct{})
		requirements.NoError(sched.AddJob(scheduler.Job{Name: api.BeeperJobName, Schedule: "0 0 1 1 *", Run: func(ctx context.Context) error {
			runs++
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}}))
		sched.Start()
		defer func() { <-sched.Stop().Done() }()
		dirty := make(chan struct{}, 1)
		go runBeeperWakeups(ctx, dirty, func() error { _, err := sched.StartJob(api.BeeperJobName, false); return err })
		markBeeperDirty(dirty)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		assertions.Equal(1, runs)
		for range 3 {
			for range 1000 {
				markBeeperDirty(dirty)
			}
			synctest.Wait()
			time.Sleep(time.Second)
			synctest.Wait()
		}
		assertions.Equal(1, runs)
		release <- struct{}{}
		synctest.Wait()
		assertions.Equal(2, runs)
		close(release)
		synctest.Wait()
		assertions.Equal(2, runs)
	})
}
func TestBeeperEventsStopWaits(t *testing.T) {
	connected := make(chan struct{})
	disconnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			_, _ = w.Write([]byte(`{"endpoints":{"ws_events":"/events"}}`))
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		close(connected)
		_, _, _ = conn.Read(r.Context())
		close(disconnected)
	}))
	t.Cleanup(server.Close)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Beeper = config.BeeperConfig{Enabled: true, Events: true, Schedule: "* * * * *", URL: server.URL}
	require.NoError(t, beeper.SaveToken(cfg.TokensDir(), "synthetic"))
	sched := scheduler.New(nil)
	stop := startBeeperEvents(t.Context(), cfg, sched, true, testDiscardLogger())
	t.Cleanup(stop)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no connection")
	}
	stop()
	stop()
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "connection did not close")
	}
}
func FuzzBeeperWakeBurst(f *testing.F) {
	for _, n := range []uint16{0, 1, 2, 1024, 65535} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, n uint16) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dirty := make(chan struct{}, 1)
			calls := 0
			go runBeeperWakeups(ctx, dirty, func() error { calls++; return nil })
			for range n {
				markBeeperDirty(dirty)
			}
			synctest.Wait()
			time.Sleep(time.Second)
			synctest.Wait()
			want := 0
			if n > 0 {
				want = 1
			}
			assert.Equal(t, want, calls)
			time.Sleep(2 * time.Second)
			synctest.Wait()
			assert.Equal(t, want, calls)
		})
	})
}

func TestHegelBeeperWakeStateful(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		// Each byte specifies a burst (0..31 signals) and 0..7 rejected admissions.
		// Arbitrary sequence length covers successive idle, failed and successful waves.
		actions := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		type outcome struct{ accepted, attempts int }
		var got, want []outcome
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dirty := make(chan struct{}, 1)
			accepted, attempts, rejections := 0, 0, 0
			go runBeeperWakeups(ctx, dirty, func() error {
				attempts++
				if rejections > 0 {
					rejections--
					return errors.New("synthetic admission rejection")
				}
				accepted++
				return nil
			})
			wantAccepted, wantAttempts := 0, 0
			for _, action := range actions {
				burst := int(action & 31)
				failures := int(action >> 5)
				rejections = failures
				for range burst {
					markBeeperDirty(dirty)
				}
				synctest.Wait()
				for range failures + 1 {
					time.Sleep(time.Second)
					synctest.Wait()
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if burst > 0 {
					wantAccepted++
					wantAttempts += failures + 1
				}
				got = append(got, outcome{accepted, attempts})
				want = append(want, outcome{wantAccepted, wantAttempts})
			}
		})
		assert.Equal(ht, want, got, "each burst eventually admits once, with retries only after rejection")
	})
}

func TestBeeperWakeHintDuringAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		dirty := make(chan struct{}, 1)
		calls := 0
		go runBeeperWakeups(ctx, dirty, func() error {
			calls++
			if calls == 1 {
				markBeeperDirty(dirty)
			}
			return nil
		})
		markBeeperDirty(dirty)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, 1, calls)
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, 2, calls, "a concurrent hint must survive the current admission")
		time.Sleep(2 * time.Second)
		synctest.Wait()
		assert.Equal(t, 2, calls)
	})
}
func TestBeeperWakeCancelDuringRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		dirty := make(chan struct{}, 1)
		calls := 0
		go runBeeperWakeups(ctx, dirty, func() error { calls++; return errors.New("synthetic admission rejection") })
		markBeeperDirty(dirty)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, 1, calls)
		cancel()
		synctest.Wait()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		assert.Equal(t, 1, calls, "stop must cancel pending retries")
	})
}
