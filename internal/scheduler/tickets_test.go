package scheduler

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustRequestJob(tb testing.TB, s *Scheduler, name string) JobRequest {
	tb.Helper()
	req, err := s.RequestJob(name)
	require.NoError(tb, err, "RequestJob %s", name)
	require.NotZero(tb, req.Ticket.Seq, "RequestJob %s issued no ticket", name)
	return req
}

func waitTicket(tb testing.TB, s *Scheduler, name string, ticket Ticket) TicketStatus {
	tb.Helper()
	status, err := s.WaitTicket(context.Background(), name, ticket)
	require.NoError(tb, err, "WaitTicket %s", ticket)
	return status
}

// peekTicket reads a ticket of the job named "job" without waiting.
func peekTicket(tb testing.TB, s *Scheduler, ticket Ticket) TicketStatus {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, err := s.WaitTicket(ctx, "job", ticket)
	require.NoError(tb, err, "WaitTicket %s", ticket)
	return status
}

// TestRequestJobTicketsDuringRunShareOneRerun: K requests that arrive while a
// run executes are answered by exactly one rerun that took the gate after all
// of them, never by the run that was already executing.
func TestRequestJobTicketsDuringRunShareOneRerun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)

		started := make(chan struct{}, 4)
		release := make(chan struct{}, 4)
		var runs, active, maxActive atomic.Int32
		s := New(func(context.Context, string) error { return nil })
		defer func() { <-s.Stop().Done() }()
		require.NoError(s.AddJob(Job{
			Name:     "job",
			Schedule: "0 0 1 1 *",
			Run: func(context.Context) error {
				n := active.Add(1)
				if n > maxActive.Load() {
					maxActive.Store(n)
				}
				runs.Add(1)
				time.Sleep(time.Second) // distinct run start times on the fake clock
				started <- struct{}{}
				<-release
				active.Add(-1)
				return nil
			},
		}))

		first := mustRequestJob(t, s, "job")
		assert.Equal(JobStarted, first.Disposition)
		<-started
		assert.Equal(TicketRunning, peekTicket(t, s, first.Ticket).State)

		const k = 8
		tickets := make(chan Ticket, k)
		var wg sync.WaitGroup
		for range k {
			wg.Go(func() { tickets <- mustRequestJob(t, s, "job").Ticket })
		}
		wg.Wait()
		close(tickets)
		var during []Ticket
		for ticket := range tickets {
			during = append(during, ticket)
			assert.Equal(TicketQueued, peekTicket(t, s, ticket).State,
				"a request made during a run is not covered by that run")
		}

		release <- struct{}{}
		<-started
		firstStatus := peekTicket(t, s, first.Ticket)
		assert.Equal(TicketCompleted, firstStatus.State)
		for _, ticket := range during {
			assert.Equal(TicketRunning, peekTicket(t, s, ticket).State,
				"the rerun covers every request made before it took the gate")
		}

		release <- struct{}{}
		var rerunStarted time.Time
		for _, ticket := range during {
			status := waitTicket(t, s, "job", ticket)
			assert.Equal(TicketCompleted, status.State)
			if rerunStarted.IsZero() {
				rerunStarted = status.RunStartedAt
			}
			assert.Equal(rerunStarted, status.RunStartedAt, "one rerun answers all K tickets")
		}
		assert.False(rerunStarted.Before(firstStatus.RunFinishedAt),
			"the answering run took the gate after the first run finished")
		synctest.Wait()
		assert.Equal(int32(2), runs.Load(), "one run plus exactly one rerun")
		assert.Equal(int32(1), maxActive.Load(), "runs never overlap")
	})
}

// TestRequestJobSeesDataWrittenDuringRun models a source read once per run:
// a message added after the running run read the source, then requested,
// is in the archive once the request's ticket completes.
func TestRequestJobSeesDataWrittenDuringRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)

		var mu sync.Mutex
		source := []string{"m0"}
		var archive []string
		read := make(chan struct{}, 4)
		release := make(chan struct{}, 4)
		s := New(func(context.Context, string) error { return nil })
		defer func() { <-s.Stop().Done() }()
		require.NoError(s.AddJob(Job{
			Name:     "chat",
			Schedule: "0 0 1 1 *",
			Run: func(context.Context) error {
				mu.Lock()
				snapshot := slices.Clone(source)
				mu.Unlock()
				read <- struct{}{}
				<-release
				mu.Lock()
				for _, m := range snapshot {
					if !slices.Contains(archive, m) {
						archive = append(archive, m)
					}
				}
				mu.Unlock()
				return nil
			},
		}))
		release <- struct{}{}
		release <- struct{}{}

		first := mustRequestJob(t, s, "chat")
		<-read
		mu.Lock()
		source = append(source, "m1")
		mu.Unlock()
		second := mustRequestJob(t, s, "chat")

		firstStatus := waitTicket(t, s, "chat", first.Ticket)
		assert.Equal(TicketCompleted, firstStatus.State)
		status := waitTicket(t, s, "chat", second.Ticket)
		require.Equal(TicketCompleted, status.State)
		mu.Lock()
		defer mu.Unlock()
		assert.Contains(archive, "m1", "the message added after the first read is archived when its ticket completes")
	})
}

// TestTicketsTerminateWhenNoRunFollows: every accepted ticket reaches a
// terminal state when the scheduler stops, when its job is removed, and when
// it was issued by another scheduler instance.
func TestTicketsTerminateWhenNoRunFollows(t *testing.T) {
	newBlockedJob := func(t *testing.T) (*Scheduler, chan struct{}, chan struct{}) {
		t.Helper()
		started := make(chan struct{}, 4)
		release := make(chan struct{})
		s := New(func(context.Context, string) error { return nil })
		require.NoError(t, s.AddJob(Job{
			Name:     "job",
			Schedule: "0 0 1 1 *",
			Run: func(ctx context.Context) error {
				started <- struct{}{}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		}))
		return s, started, release
	}

	t.Run("stop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, started, _ := newBlockedJob(t)
			running := mustRequestJob(t, s, "job")
			<-started
			queued := mustRequestJob(t, s, "job")
			coalesced := mustRequestJob(t, s, "job")

			type waited struct {
				status TicketStatus
				err    error
			}
			results := make(chan waited, 3)
			for _, ticket := range []Ticket{running.Ticket, queued.Ticket, coalesced.Ticket} {
				go func() {
					status, err := s.WaitTicket(context.Background(), "job", ticket)
					results <- waited{status, err}
				}()
			}
			synctest.Wait()
			<-s.Stop().Done()
			for range 3 {
				r := <-results
				require.NoError(t, r.err)
				assert.Equal(t, TicketAbandoned, r.status.State, "ticket %s", r.status.Ticket)
				assert.NotEmpty(t, r.status.Error)
			}
			_, err := s.RequestJob("job")
			assert.Error(t, err, "a stopped scheduler issues no ticket")
		})
	})

	t.Run("job removed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, started, release := newBlockedJob(t)
			defer func() { <-s.Stop().Done() }()
			running := mustRequestJob(t, s, "job")
			<-started
			queued := mustRequestJob(t, s, "job")
			s.RemoveJob("job")
			close(release)

			assert.Equal(t, TicketCompleted, waitTicket(t, s, "job", running.Ticket).State,
				"the run already holding the gate still answers its ticket")
			status := waitTicket(t, s, "job", queued.Ticket)
			assert.Equal(t, TicketAbandoned, status.State)
			assert.Equal(t, "job removed", status.Error)
		})
	})

	t.Run("earlier process", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			old, started, release := newBlockedJob(t)
			stale := mustRequestJob(t, old, "job")
			<-started
			close(release)
			<-old.Stop().Done()

			s, _, _ := newBlockedJob(t)
			defer func() { <-s.Stop().Done() }()
			status := waitTicket(t, s, "job", stale.Ticket)
			assert.Equal(t, TicketAbandoned, status.State)

			_, err := s.WaitTicket(context.Background(), "job", Ticket{Epoch: s.ticketEpoch, Seq: 99})
			assert.ErrorIs(t, err, ErrUnknownTicket)
		})
	})
}

// TestTicketFailsWithRunError: a run that returns an error answers the
// tickets it covers as failed, with the error.
func TestTicketFailsWithRunError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(func(context.Context, string) error { return nil })
		defer func() { <-s.Stop().Done() }()
		require.NoError(t, s.AddJob(Job{
			Name:     "job",
			Schedule: "0 0 1 1 *",
			Run:      func(context.Context) error { return errors.New("source unreadable") },
		}))
		status := waitTicket(t, s, "job", mustRequestJob(t, s, "job").Ticket)
		assert.Equal(t, TicketFailed, status.State)
		assert.Equal(t, "source unreadable", status.Error)
	})
}

// TestTicketNotAnsweredByUnfinishedPass: a pass that reports more work does
// not answer the ticket; the continuation that finishes the work does.
func TestTicketNotAnsweredByUnfinishedPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		var passes atomic.Int32
		s := New(func(context.Context, string) error { return nil })
		defer func() { <-s.Stop().Done() }()
		require.NoError(s.AddJob(Job{
			Name:       "job",
			Schedule:   "0 0 1 1 *",
			MinSpacing: time.Hour,
			Run: func(context.Context) error {
				if passes.Add(1) == 1 {
					return ErrReschedule
				}
				return nil
			},
		}))
		start := time.Now()
		status := waitTicket(t, s, "job", mustRequestJob(t, s, "job").Ticket)
		assert.Equal(TicketCompleted, status.State)
		assert.Equal(int32(2), passes.Load())
		assert.Less(time.Since(start), time.Hour, "a continuation is not delayed by MinSpacing")
	})
}

// TestMinSpacingDelaysRerun: a request made right after a run delays the
// rerun to the job's minimum spacing; it is not answered by the earlier run.
func TestMinSpacingDelaysRerun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		const spacing = time.Minute
		var gates []time.Time
		var mu sync.Mutex
		s := New(func(context.Context, string) error { return nil })
		defer func() { <-s.Stop().Done() }()
		require.NoError(s.AddJob(Job{
			Name:       "job",
			Schedule:   "0 0 1 1 *",
			MinSpacing: spacing,
			Run: func(context.Context) error {
				mu.Lock()
				gates = append(gates, time.Now())
				mu.Unlock()
				time.Sleep(time.Second)
				return nil
			},
		}))

		first := waitTicket(t, s, "job", mustRequestJob(t, s, "job").Ticket)
		require.Equal(TicketCompleted, first.State)
		second := mustRequestJob(t, s, "job")
		assert.Equal(JobStarted, second.Disposition)
		synctest.Wait()
		assert.Equal(TicketQueued, peekTicket(t, s, second.Ticket).State,
			"the rerun waits for the spacing instead of being answered by the first run")
		third := mustRequestJob(t, s, "job")
		assert.Equal(JobCoalesced, third.Disposition, "a request during the delay joins the delayed run")

		secondStatus := waitTicket(t, s, "job", second.Ticket)
		assert.Equal(TicketCompleted, secondStatus.State)
		assert.Equal(secondStatus, func() TicketStatus {
			st := waitTicket(t, s, "job", third.Ticket)
			st.Ticket = secondStatus.Ticket
			return st
		}(), "both requests are answered by the same delayed run")
		mu.Lock()
		defer mu.Unlock()
		require.Len(gates, 2)
		assert.GreaterOrEqual(gates[1].Sub(gates[0]), spacing)
	})
}

func TestParseTicketRoundTrip(t *testing.T) {
	ticket := Ticket{Epoch: "0123abcd", Seq: 42}
	parsed, err := ParseTicket(ticket.String())
	require.NoError(t, err)
	assert.Equal(t, ticket, parsed)
	for _, bad := range []string{"", "abc", "-1", "abc-0", "abc-x"} {
		_, err := ParseTicket(bad)
		assert.ErrorIs(t, err, ErrInvalidTicket, "%q", bad)
	}
}
