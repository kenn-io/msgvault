package scheduler

import (
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
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

type AcquisitionReason uint8

const (
	AcquisitionReasonUnknown AcquisitionReason = iota
	AcquisitionCanonicalTarget
	AcquisitionFilteredCoverage
)

// AcquisitionError retains the cause and provider floor without publishing
// provider response bodies, URLs or credentials through account status.
type AcquisitionError struct {
	Reason      AcquisitionReason
	Outcome     AcquisitionOutcome
	NotBefore   time.Time
	Credentials bool
	Err         error
}

func (e *AcquisitionError) Error() string {
	switch e.Reason {
	case AcquisitionReasonUnknown:
		// Use the safe outcome message below for an unspecified reason.
	case AcquisitionCanonicalTarget:
		return "Choose exactly one active mail account using its ACCOUNT value from msgvault list-accounts."
	case AcquisitionFilteredCoverage:
		return "Rapid polling: remove folder/date filters or disable poll_interval."
	}
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

// accountPollWake identifies one deadline callback. Its deadline never changes;
// Scheduler.mu protects the timer reference and registration attachment.
type accountPollWake struct {
	timer    *time.Timer
	deadline time.Time
}

// Pointer identity is the immutable registration generation. Old callbacks and
// finishers retain their old pointer even after a key is removed and re-added.
// Every field is protected by Scheduler.mu.
type accountRegistration struct {
	interval      time.Duration
	entry         cron.EntryID
	pollWake      *accountPollWake
	pollPending   bool
	pollOnly      bool
	nextEligible  time.Time
	providerFloor time.Time
	failures      int
	lastAttempted time.Time
	lastCompleted time.Time
	state         string
}

// SetAccountPollInterval adds a typed poll schedule to an existing account lane.
// Daemon configuration calls this at registration; config changes take effect
// on restart. Updating its cron schedule retains this registration.
func (s *Scheduler) SetAccountPollInterval(identifier string, interval time.Duration) error {
	if err := config.ValidateAccountPollInterval(interval); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.accounts[identifier]
	if r == nil {
		return errors.New("account is not scheduled")
	}
	cancelAccountPollWakeLocked(r)
	if r.entry != 0 {
		s.cron.Remove(r.entry)
		r.entry = 0
	}
	r.interval = interval
	if interval == 0 {
		r.pollPending = false
		r.providerFloor = time.Time{}
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
	s.onRegisteredAccountTickFromWake(email, r, poll, nil)
}

func (s *Scheduler) onAccountPollWake(email string, r *accountRegistration, wake *accountPollWake) {
	s.onRegisteredAccountTickFromWake(email, r, true, wake)
}

func (s *Scheduler) onRegisteredAccountTickFromWake(email string, r *accountRegistration, poll bool, wake *accountPollWake) {
	s.mu.Lock()
	if s.stopped || !s.currentAccountLocked(email, r) || (poll && r.interval == 0) ||
		(wake != nil && (r.pollWake != wake || r.pollWake.timer != wake.timer ||
			!r.pollWake.deadline.Equal(wake.deadline))) {
		s.mu.Unlock()
		return
	}
	if wake != nil {
		r.pollWake = nil
	}
	now := time.Now()
	if now.Before(r.providerFloor) {
		if !poll {
			s.pending[email] = true
		} else {
			s.armAccountPollWakeLocked(email, r, laterAccountPollDeadline(r))
		}
		r.state = "deferred"
		s.mu.Unlock()
		return
	}
	if poll && !s.pending[email] && now.Before(r.nextEligible) {
		s.armAccountPollWakeLocked(email, r, laterAccountPollDeadline(r))
		s.mu.Unlock()
		return
	}
	if poll {
		cancelAccountPollWakeLocked(r)
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

func laterAccountPollDeadline(r *accountRegistration) time.Time {
	if r.providerFloor.After(r.nextEligible) {
		return r.providerFloor
	}
	return r.nextEligible
}

func (s *Scheduler) armAccountPollWakeLocked(email string, r *accountRegistration, deadline time.Time) {
	if s.stopped || !s.currentAccountLocked(email, r) || r.interval == 0 {
		cancelAccountPollWakeLocked(r)
		return
	}
	if r.pollWake != nil && r.pollWake.deadline.Equal(deadline) {
		return
	}
	cancelAccountPollWakeLocked(r)
	delay := time.Until(deadline)
	if deadline.IsZero() || delay <= 0 {
		return
	}
	wake := &accountPollWake{deadline: deadline}
	wake.timer = time.AfterFunc(delay, func() {
		s.onAccountPollWake(email, r, wake)
	})
	r.pollWake = wake
}

func cancelAccountPollWakeLocked(r *accountRegistration) {
	if r.pollWake != nil {
		r.pollWake.timer.Stop()
		r.pollWake = nil
	}
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
		if jitter, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(room)+1)); err == nil {
			delay += time.Duration(jitter.Int64())
		}
	}
	return delay
}

func (s *Scheduler) recordAccountPollLocked(r *accountRegistration, err error, yielded, cancelled bool) {
	if r.interval == 0 {
		return
	}
	var floor interface{ RetryNotBefore() time.Time }
	if errors.As(err, &floor) && floor.RetryNotBefore().After(r.providerFloor) {
		r.providerFloor = floor.RetryNotBefore()
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
		if acquisition, ok := errors.AsType[*AcquisitionError](err); ok {
			status.LastError = acquisition.Error()
		} else {
			status.LastError = "email acquisition failed; retry scheduled"
		}
	}
}
