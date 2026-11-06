package scheduler

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/netresearch/go-cron"
	"go.kenn.io/msgvault/internal/config"
)

// AcquisitionOutcome separates provider completion from admission and indexing.
type AcquisitionOutcome string

const (
	AcquisitionIncomplete AcquisitionOutcome = "incomplete"
	AcquisitionDeferred   AcquisitionOutcome = "deferred"
	AcquisitionCancelled  AcquisitionOutcome = "cancelled"
	AcquisitionFailed     AcquisitionOutcome = "failed"
)

// AcquisitionError retains the cause and provider floor without publishing
// provider response bodies, URLs or credentials through account status.
type AcquisitionError struct {
	Outcome     AcquisitionOutcome
	NotBefore   time.Time
	Credentials bool
	Err         error
}

func (e *AcquisitionError) Error() string {
	if e.Credentials {
		return "email credentials unavailable; reauthorize the selected account"
	}
	switch e.Outcome {
	case AcquisitionIncomplete:
		return "email acquisition incomplete; retry scheduled"
	case AcquisitionDeferred:
		return "email acquisition deferred; retry scheduled"
	case AcquisitionCancelled:
		return "email acquisition interrupted; catchup remains pending"
	default:
		return "email acquisition failed; retry scheduled"
	}
}
func (e *AcquisitionError) Unwrap() error             { return e.Err }
func (e *AcquisitionError) RetryNotBefore() time.Time { return e.NotBefore }

// Pointer identity is the immutable registration generation. Old callbacks and
// finishers retain their old pointer even after a key is removed and re-added.
// Every field is protected by Scheduler.mu.
type accountRegistration struct {
	interval      time.Duration
	entry         cron.EntryID
	pollPending   bool
	pollOnly      bool
	nextEligible  time.Time
	providerFloor time.Time
	failures      int
	lastAttempted time.Time
	lastCompleted time.Time
	state         string
}

// SetAccountPollInterval adds a typed timer to an existing account lane.
// Daemon configuration calls this at registration; config changes take effect
// on restart. Updating its cron schedule retains this registration and timer.
func (s *Scheduler) SetAccountPollInterval(identifier string, interval time.Duration) error {
	if err := config.ValidateAccountPollInterval(interval); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.accounts[identifier]
	if r == nil {
		return fmt.Errorf("account is not scheduled")
	}
	if r.entry != 0 {
		s.cron.Remove(r.entry)
		r.entry = 0
	}
	r.interval = interval
	if interval == 0 {
		r.pollPending = false
		r.state = ""
		return nil
	}
	entry, err := s.cron.ScheduleJob(cron.Every(interval), cron.FuncJob(func() {
		s.onRegisteredAccountTick(identifier, r, true)
	}))
	if err != nil {
		return fmt.Errorf("schedule account poll: %w", err)
	}
	r.entry = entry
	r.state = "waiting"
	return nil
}

func (s *Scheduler) currentAccountLocked(email string, r *accountRegistration) bool {
	return r != nil && s.accounts[email] == r
}

func (s *Scheduler) onRegisteredAccountTick(email string, r *accountRegistration, poll bool) {
	s.mu.Lock()
	if s.stopped || !s.currentAccountLocked(email, r) || (poll && r.interval == 0) {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	if now.Before(r.providerFloor) {
		if !poll {
			s.pending[email] = true
		}
		r.state = "deferred"
		s.mu.Unlock()
		return
	}
	if poll && !s.pending[email] && now.Before(r.nextEligible) {
		s.mu.Unlock()
		return
	}
	if s.running[email] {
		if _, queued := s.queuedAt[email]; queued {
			if !poll {
				r.pollOnly = false // cron intent already covered by this queued run
			}
		} else if poll {
			r.pollPending = true
		} else {
			s.coalesceTickLocked("email", email, s.queuedAt, s.pending)
		}
		s.mu.Unlock()
		return
	}
	s.reserveAccountLocked(email, r, poll && !s.pending[email])
	s.mu.Unlock()
	s.runSync(email, r)
}

func (s *Scheduler) reserveAccountLocked(email string, r *accountRegistration, pollOnly bool) {
	s.running[email] = true
	s.queuedAt[email] = time.Now()
	delete(s.pending, email)
	r.pollPending = false
	r.pollOnly = pollOnly
	s.wg.Add(1)
}

func accountPollBackoff(interval time.Duration, failures int) time.Duration {
	const ceiling = 15 * time.Minute
	delay := interval
	for range min(failures, 10) {
		delay = min(2*delay, ceiling)
	}
	if room := min(delay/10, ceiling-delay); room > 0 && failures > 0 {
		delay += time.Duration(rand.Int64N(int64(room) + 1))
	}
	return delay
}

func (s *Scheduler) recordAccountPollLocked(r *accountRegistration, err error, yielded, cancelled bool) {
	var floor interface{ RetryNotBefore() time.Time }
	if errors.As(err, &floor) && floor.RetryNotBefore().After(r.providerFloor) {
		r.providerFloor = floor.RetryNotBefore()
	}
	if r.interval == 0 {
		return
	}
	now := time.Now()
	var acquisition *AcquisitionError
	_ = errors.As(err, &acquisition)
	switch {
	case yielded || cancelled || (acquisition != nil && acquisition.Outcome == AcquisitionCancelled):
		r.state = "yielded"
		if r.pollOnly {
			r.nextEligible = now.Add(r.interval)
		}
	case errors.Is(err, ErrDeferUntilNextTrigger) || (acquisition != nil && acquisition.Outcome == AcquisitionDeferred):
		r.state = "deferred"
	case err != nil:
		r.failures = min(r.failures+1, 10)
		r.nextEligible = now.Add(accountPollBackoff(r.interval, r.failures))
		r.state = "degraded"
	case !s.stopped && s.ctx.Err() == nil:
		r.failures = 0
		r.lastCompleted = now
		r.nextEligible = r.lastAttempted.Add(r.interval)
		r.state = "waiting"
	}
	if r.providerFloor.After(r.nextEligible) {
		r.nextEligible = r.providerFloor
	}
}

func (s *Scheduler) accountPollStatusLocked(status *AccountStatus, r *accountRegistration) {
	if r == nil || r.interval == 0 {
		return
	}
	status.PollInterval = r.interval.String()
	status.PollState = r.state
	status.LastAttempted = r.lastAttempted
	status.LastCompleted = r.lastCompleted
	status.NextEligiblePoll = r.nextEligible
	status.Pending = status.Pending || r.pollPending
	if status.Queued {
		status.PollState = "queued"
	} else if status.Running {
		status.PollState = "polling"
	}
	if err := s.lastErr[status.Email]; err != nil {
		var acquisition *AcquisitionError
		if errors.As(err, &acquisition) {
			status.LastError = acquisition.Error()
		} else {
			status.LastError = "email acquisition failed; retry scheduled"
		}
	}
}
