package threads

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestThreadsOnePublishAcrossCrashes(t *testing.T) {
	for _, mode := range []string{"drop", "500", "after-barrier", "cancel-after-barrier"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newSendFixture(t)
			f.setMode(mode)
			sender := NewSender(f.st, f.options)
			if mode == "after-barrier" {
				sender.afterDispatch = func() error { return errors.New("synthetic process death") }
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel-after-barrier" {
				sender.afterDispatch = func() error { cancel(); return ctx.Err() }
			}
			result, err := sender.Send(ctx, f.request())
			require.Error(err)
			assert.Equal("remote_unknown", result.Status)
			r, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			assert.NotNil(r.PublishDispatchedAt)
			expected := int32(1)
			if mode == "after-barrier" || mode == "cancel-after-barrier" {
				expected = 0
			}
			assert.Equal(expected, f.publishes.Load())
			f.setMode("")
			f.setStatuses("FINISHED")
			f.options.OpenAPI = func(context.Context, int64) (PublishAPI, []string, error) {
				require.Fail("unknown receipt attempted provider work")
				return nil, nil, errors.New("no credentials")
			}
			for range 3 {
				again, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
				require.NoError(err)
				assert.Equal("remote_unknown", again.Status)
			}
			assert.Equal(expected, f.publishes.Load())
			assert.Equal(int32(1), f.creates.Load())
		})
	}
}
func installSenderReceiptFault(t *testing.T, st *store.Store, condition string) {
	t.Helper()
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_threads_sender_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic_sender_write'; END $$;
   CREATE TRIGGER threads_sender_fault BEFORE UPDATE ON threads_reply_sends FOR EACH ROW WHEN (`+condition+`) EXECUTE FUNCTION fail_threads_sender_write()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER threads_sender_fault BEFORE UPDATE ON threads_reply_sends WHEN `+condition+` BEGIN SELECT RAISE(FAIL,'synthetic_sender_write'); END`)
	}
	require.NoError(t, err)
}
func dropSenderReceiptFault(t *testing.T, st *store.Store) {
	t.Helper()
	query := `DROP TRIGGER threads_sender_fault`
	if st.IsPostgreSQL() {
		query += ` ON threads_reply_sends`
	}
	_, err := st.DB().ExecContext(t.Context(), query)
	require.NoError(t, err)
}
func TestThreadsOnePublishTransitionFailures(t *testing.T) {
	for _, state := range []string{"container_saved", "ready", "publish_dispatched"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newSendFixture(t)
			installSenderReceiptFault(t, f.st, "NEW.state='"+state+"'")
			_, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
			require.Error(err)
			assert.Zero(f.publishes.Load())
			r, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
			require.NoError(err)
			assert.Nil(r.PublishDispatchedAt)
			if state == "container_saved" {
				assert.Empty(r.ContainerID)
			} else {
				assert.Equal("400", r.ContainerID)
			}
			dropSenderReceiptFault(t, f.st)
			result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
			require.NoError(err)
			assert.Equal("500", result.MediaID)
			assert.Equal(int32(1), f.publishes.Load())
			f.mu.Lock()
			published := append([]string(nil), f.publishedContainers...)
			f.mu.Unlock()
			if state == "container_saved" {
				assert.Equal(int32(2), f.creates.Load())
				assert.Equal([]string{"401"}, published)
			} else {
				assert.Equal(int32(1), f.creates.Load())
				assert.Equal([]string{"400"}, published)
			}
		})
	}
}
func TestThreadsOnePublishLostCreateResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	f.mu.Lock()
	f.createMode = "drop"
	f.mu.Unlock()
	_, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.Error(err)
	assert.Equal(int32(1), f.creates.Load())
	assert.Zero(f.publishes.Load())
	r, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Equal("claimed", r.State)
	assert.Empty(r.ContainerID)
	f.mu.Lock()
	f.createMode = ""
	f.mu.Unlock()
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.Equal("500", result.MediaID)
	assert.Equal(int32(2), f.creates.Load())
	assert.Equal(int32(1), f.publishes.Load())
}
func TestThreadsOnePublishContainerOutcomes(t *testing.T) {
	for _, status := range []string{"PUBLISHED", "ERROR", "EXPIRED"} {
		t.Run(status, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newSendFixture(t)
			f.setStatuses(status)
			result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
			if status == "PUBLISHED" {
				assert.Equal("remote_unknown", result.Status)
			} else {
				require.Error(err)
				assert.Equal("send_failed", result.Status)
				_, err = f.st.UpdateChatDraftContext(t.Context(), f.draft.DraftID, 1, "new reviewed text")
				require.NoError(err)
			}
			assert.Zero(f.publishes.Load())
			f.setStatuses("FINISHED")
			_, _ = NewSender(f.st, f.options).Send(t.Context(), f.request())
			assert.Equal(int32(1), f.creates.Load())
			assert.Zero(f.publishes.Load())
		})
	}
}
func TestThreadsPublishProtocolDurablePollSchedule(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	f.setStatuses("IN_PROGRESS")
	for range 2 {
		before := f.clock.Now()
		result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
		require.NoError(err)
		assert.Equal("container_saved", result.Status)
		assert.LessOrEqual(f.clock.Now().Sub(before), 5*time.Minute)
	}
	assert.Equal(int32(1), f.creates.Load())
	assert.Zero(f.publishes.Load())
	f.mu.Lock()
	times := append([]time.Time(nil), f.statusTimes...)
	f.mu.Unlock()
	require.GreaterOrEqual(len(times), 2)
	for n := 1; n < len(times); n++ {
		assert.GreaterOrEqual(times[n].Sub(times[n-1]), time.Minute)
	}
	r, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	require.NotNil(r.NextPollAt)
	assert.Greater(r.NextPollAt.UnixNano(), f.clock.Now().UnixNano())
}
func TestThreadsPublishProtocolEarlyResumeRetainsPollReservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	f.setStatuses("IN_PROGRESS", "FINISHED")
	f.options.Wait = func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		count := len(f.statusTimes)
		f.mu.Unlock()
		if count > 0 && d > time.Second {
			return &PauseError{Reason: "synthetic_wait_pause"}
		}
		return f.clock.Wait(ctx, d)
	}
	_, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.Error(err)
	f.mu.Lock()
	count := len(f.statusTimes)
	f.mu.Unlock()
	require.Equal(1, count)
	_, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.Error(err)
	f.mu.Lock()
	assert.Len(f.statusTimes, 1)
	f.mu.Unlock()
	f.options.Wait = f.clock.Wait
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.Equal("500", result.MediaID)
	assert.Equal(int32(1), f.creates.Load())
	assert.Equal(int32(1), f.publishes.Load())
}

// Hegel reduced this to mode0/processing0/retries1/cap3/body="r".
func TestThreadsOnePublishBudgetBeforeBarrierRegression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newSendFixture(t)
	draft, err := f.st.CreateChatDraftContext(t.Context(), f.draft.ConversationID, f.rootID, "r", func(string, string) error { return nil })
	require.NoError(err)
	f.draft = draft
	base, httpClient := f.api.base.String(), f.api.http
	f.api, err = NewClient(ClientOptions{BaseURL: base, HTTPClient: httpClient, AccessToken: testToken, Budget: f.clock.budget(3)})
	require.NoError(err)
	result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
	_, paused := errors.AsType[*PauseError](err)
	require.True(paused, "%v", err)
	assert.Equal("ready", result.Status)
	receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
	require.NoError(err)
	assert.Nil(receipt.PublishDispatchedAt)
	assert.Zero(f.publishes.Load())
	f.api, err = NewClient(ClientOptions{BaseURL: base, HTTPClient: httpClient, AccessToken: testToken, Budget: f.clock.budget(20)})
	require.NoError(err)
	result, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
	require.NoError(err)
	assert.Equal("500", result.MediaID)
	assert.Equal(int32(1), f.creates.Load())
	assert.Equal(int32(1), f.publishes.Load())
}

func TestThreadsOnePublishLimiterTimeBeforeBarrier(t *testing.T) {
	for _, tc := range []struct{ remaining, oversleep time.Duration }{
		{remaining: 250 * time.Millisecond},
		{remaining: 500 * time.Millisecond},
		{remaining: 750 * time.Millisecond},
		{remaining: 750 * time.Millisecond, oversleep: 500 * time.Millisecond},
	} {
		name := tc.remaining.String()
		if tc.oversleep > 0 {
			name += "-delayed-wake"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := newSendFixture(t)
				base, httpClient := f.api.base.String(), f.api.http
				started := f.clock.Now()
				oversleep := tc.oversleep
				limiter := NewRateLimiter(f.clock.Now, func(ctx context.Context, delay time.Duration) error {
					// Model a delayed wake deterministically, without relying on host scheduling.
					if !f.clock.Now().Before(started.Add(31 * time.Second)) {
						delay += oversleep
						oversleep = 0
					}
					return f.clock.Wait(ctx, delay)
				})
				// Quota and create occur at 0.5s and 1s; the first status is at 31s.
				// That status leaves a half-second limiter delay before publication.
				budget := NewRequestBudget(BudgetOptions{MaxAttempts: 20, MaxDuration: 31*time.Second + tc.remaining, Now: f.clock.Now, Wait: f.clock.Wait, Limiter: limiter})
				var err error
				f.api, err = NewClient(ClientOptions{BaseURL: base, HTTPClient: httpClient, AccessToken: testToken, Budget: budget})
				require.NoError(err)
				result, err := NewSender(f.st, f.options).Send(t.Context(), f.request())
				if tc.remaining > 500*time.Millisecond && tc.oversleep == 0 {
					require.NoError(err)
					assert.Equal("sent", result.RemoteStatus)
					assert.Equal(int32(1), f.publishes.Load())
					assert.Equal(4, budget.Attempts())
					return
				}
				var pause *PauseError
				require.ErrorAs(err, &pause)
				assert.Equal("time_limit", pause.Reason)
				assert.Equal("ready", result.Status)
				assert.Equal("unpublished", result.RemoteStatus)
				assert.Equal(3, budget.Attempts(), "refused publication must not consume a transport attempt")
				assert.Zero(f.publishes.Load())
				receipt, err := f.st.GetThreadsReplySendContext(t.Context(), f.draft.DraftID)
				require.NoError(err)
				assert.Nil(receipt.PublishDispatchedAt, "no publish request occurred; the draft must remain resumable")
				assert.Equal("400", receipt.ContainerID)
				// A fresh run retains the source limiter and publishes the existing container once.
				resumed := NewRequestBudget(BudgetOptions{MaxAttempts: 20, Now: f.clock.Now, Wait: f.clock.Wait, Limiter: limiter})
				f.api, err = NewClient(ClientOptions{BaseURL: base, HTTPClient: httpClient, AccessToken: testToken, Budget: resumed})
				require.NoError(err)
				result, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
				require.NoError(err)
				assert.Equal("500", result.MediaID)
				assert.Equal(int32(1), f.creates.Load())
				assert.Equal(int32(1), f.publishes.Load())
				_, err = NewSender(f.st, f.options).Send(t.Context(), f.request())
				require.NoError(err)
				assert.Equal(int32(1), f.publishes.Load(), "replaying the receipt cannot dispatch again")
			})
		})
	}
}
