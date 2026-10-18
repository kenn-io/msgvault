package cmd

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/scheduler"
)

func TestPeopleTierSchedulerDefersAtEOFWithExpiredBudget(t *testing.T) {
	for _, pruneFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(pruneFailure), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, runner := tierScheduledFixture(t)
			person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("budget-eof@example.com", "Budget EOF", "example.com"))
			require.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_tier_events(person_id,from_tier,to_tier,reason,mode,lanes_json,signals_json,rule,rule_version,actor,run_id,created_at) VALUES(?,3,2,'promotion','preview','{}','{}','minimum_tier',1,'synthetic','expired-preview',?)`), person.ID, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
			require.NoError(err)
			if pruneFailure {
				if f.Store.IsPostgreSQL() {
					_, err = f.Store.DB().ExecContext(t.Context(), `CREATE FUNCTION reject_tier_preview_prune() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic preview prune failure'; END $$`)
					require.NoError(err)
					_, err = f.Store.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_tier_preview_prune BEFORE DELETE ON person_tier_events FOR EACH ROW EXECUTE FUNCTION reject_tier_preview_prune()`)
				} else {
					_, err = f.Store.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_tier_preview_prune BEFORE DELETE ON person_tier_events BEGIN SELECT RAISE(ABORT,'synthetic preview prune failure'); END`)
				}
				require.NoError(err)
			}
			sched := scheduler.New(nil)
			t.Cleanup(func() { <-sched.Stop().Done() })
			job := newPeopleTierScheduledJob(runner)
			productionRun := job.Run
			var calls atomic.Int32
			var budgetCause error
			var completedProgress bool
			job.MaxRuntime = 10 * time.Millisecond
			job.Run = func(ctx context.Context) error {
				if calls.Add(1) > 1 {
					// Keep any incorrect follow-up observable until its real budget or
					// scheduler shutdown cancels it. Never launch another Store scan.
					<-ctx.Done()
					return scheduler.ErrDeferUntilNextTrigger
				}
				// Let actual fixture work reach EOF regardless of machine speed, then
				// return under the scheduler's real expired MaxRuntime context. The
				// separate bound only limits waiting for observable Store completion.
				workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				err := productionRun(workCtx)
				cancel()
				<-ctx.Done()
				budgetCause = context.Cause(ctx)
				completedProgress = jobctx.HasProgress(ctx)
				return err
			}
			require.NoError(sched.AddJob(job))
			err = sched.TriggerJob(peopleTierJobName)
			require.ErrorIs(budgetCause, jobctx.ErrRunBudgetExceeded)
			require.True(completedProgress, "the real Store runner recorded its committed final page")
			if pruneFailure {
				require.ErrorContains(err, "synthetic preview prune failure")
			} else {
				require.NoError(err)
			}
			statuses := sched.JobStatus()
			require.Len(statuses, 1)
			assert.False(statuses[0].Running, "completed EOF must not reserve another scan")
			assert.False(statuses[0].Queued, "a reserved follow-up is visible before its callback starts")
			assert.False(statuses[0].Pending)
			if pruneFailure {
				assert.Contains(statuses[0].LastError, "synthetic preview prune failure")
			} else {
				assert.Empty(statuses[0].LastError)
			}
			assert.Equal(int32(1), calls.Load())
			run, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
			require.NoError(err)
			assert.Nil(run)
			var observed, expired int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_observations`).Scan(&observed))
			assert.Equal(1, observed)
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_tier_events WHERE run_id='expired-preview'`).Scan(&expired))
			if pruneFailure {
				assert.Equal(1, expired)
			} else {
				assert.Zero(expired)
			}
		})
	}
}
