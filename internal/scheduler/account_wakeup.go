package scheduler

import (
	"context"
	"errors"
	"time"
)

// Registration and pass pointers are immutable identities. String-keyed status
// maps remain the public view, but an old cron closure or gated run cannot
// mutate a replacement registration's state.
type accountRegistration struct {
	active *accountPass
	// FIFO contains at most one ordinary and one strict wake intent. Mixed
	// scopes cannot replace each other's callback or completion ownership.
	pending []*accountPass
}

type accountPass struct {
	wake    bool
	settled bool
	waiters map[chan error]struct{}
}

func newAccountPass(wake bool) *accountPass {
	return &accountPass{wake: wake, waiters: make(map[chan error]struct{})}
}

// WithAccountWakeSyncFunc sets the source-bound reconciliation used by passes
// admitting provider hints. Cron/manual passes without hints retain SyncFunc.
// Configure this before starting listeners.
func (s *Scheduler) WithAccountWakeSyncFunc(run SyncFunc) *Scheduler {
	s.mu.Lock()
	s.accountWakeSync = run
	s.mu.Unlock()
	return s
}

// WakeAccount asynchronously admits a provider hint. A queued pass covers the
// hint; an ordinary queued pass keeps its callback and retains one strict
// follow-up, as does an executing pass. Each scope retains at most one pending
// pass, with FIFO admission between ordinary and strict wake scopes. A wake-only
// burst therefore retains at most one follow-up; mixed origins may retain two.
// No manual completion notification is requested. The account key must match exactly.
func (s *Scheduler) WakeAccount(identifier string) (JobDisposition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, disposition, err := s.reserveAccountWakeLocked(identifier)
	return disposition, err
}

// WakeAccountAndWait admits a hint and waits for its qualifying reconciliation
// pass. Success means provider work completed after admission, without failure,
// cancellation, deferral or preemption. Canceling ctx unregisters only this
// waiter, leaving the shared pass and other waiters intact.
func (s *Scheduler) WakeAccountAndWait(ctx context.Context, identifier string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	pass, _, err := s.reserveAccountWakeLocked(identifier)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	receipt := make(chan error, 1)
	pass.waiters[receipt] = struct{}{}
	s.mu.Unlock()
	select {
	case err := <-receipt:
		return err
	case <-ctx.Done():
		s.mu.Lock()
		delete(pass.waiters, receipt)
		s.mu.Unlock()
		return ctx.Err()
	}
}

func (s *Scheduler) reserveAccountWakeLocked(identifier string) (*accountPass, JobDisposition, error) {
	if s.stopped {
		return nil, "", errors.New("scheduler is stopped")
	}
	registration := s.accounts[identifier]
	if registration == nil {
		return nil, "", errors.New("wake account is not scheduled")
	}
	if !s.running[identifier] {
		pass := newAccountPass(s.accountWakeSync != nil)
		s.startAccountPassLocked(identifier, registration, pass)
		return pass, JobStarted, nil
	}
	if _, queued := s.queuedAt[identifier]; queued && !registration.active.settled &&
		(registration.active.wake || s.accountWakeSync == nil) {
		return registration.active, JobCoalesced, nil
	}
	pass, added := s.pendingAccountPassLocked(identifier, registration, s.accountWakeSync != nil)
	if added {
		return pass, JobPending, nil
	}
	return pass, JobCoalesced, nil
}

func (s *Scheduler) pendingAccountPassLocked(identifier string, registration *accountRegistration, wake bool) (*accountPass, bool) {
	for _, pass := range registration.pending {
		if pass.wake == wake {
			return pass, false
		}
	}
	pass := newAccountPass(wake)
	registration.pending = append(registration.pending, pass)
	s.pending[identifier] = true
	return pass, true
}

func (s *Scheduler) startAccountPassLocked(identifier string, registration *accountRegistration, pass *accountPass) {
	registration.active = pass
	s.running[identifier] = true
	s.queuedAt[identifier] = time.Now()
	s.wg.Add(1)
	go s.runAccountPass(identifier, registration, pass)
}

// settleAccountPassLocked is idempotent, including after caller cancellation.
func settleAccountPassLocked(pass *accountPass, err error) {
	if pass == nil || pass.settled {
		return
	}
	pass.settled = true
	for waiter := range pass.waiters {
		waiter <- err
		delete(pass.waiters, waiter)
	}
}

func (s *Scheduler) retireAccountLocked(identifier string) {
	if registration := s.accounts[identifier]; registration != nil {
		err := errors.New("wake account registration was removed or replaced")
		settleAccountPassLocked(registration.active, err)
		for _, pass := range registration.pending {
			settleAccountPassLocked(pass, err)
		}
	}
	delete(s.accounts, identifier)
	delete(s.running, identifier)
	delete(s.queuedAt, identifier)
	delete(s.startedAt, identifier)
	delete(s.pending, identifier)
	delete(s.manual, identifier)
}

func (s *Scheduler) onAccountRegistrationTick(identifier string, registration *accountRegistration) {
	s.mu.Lock()
	if s.stopped || registration == nil || s.accounts[identifier] != registration {
		s.mu.Unlock()
		return
	}
	if s.running[identifier] {
		_, queued := s.queuedAt[identifier]
		if queued && !registration.active.settled && (!registration.active.wake || s.accountWakeSync == nil) {
			s.coalesceTickLocked("email", identifier, s.queuedAt, s.pending)
		} else {
			if _, added := s.pendingAccountPassLocked(identifier, registration, false); added {
				s.logger.Info("scheduled sync skipped: previous run still active; queued one follow-up run", "email", identifier)
			}
		}
		s.mu.Unlock()
		return
	}
	pass := newAccountPass(false)
	registration.active = pass
	s.running[identifier] = true
	s.queuedAt[identifier] = time.Now()
	s.wg.Add(1)
	s.mu.Unlock()
	s.runAccountPass(identifier, registration, pass)
}

func (s *Scheduler) finishAccountPass(identifier string, registration *accountRegistration, pass *accountPass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts[identifier] != registration || registration.active != pass {
		return
	}
	settleAccountPassLocked(pass, errors.New("wake pass did not complete"))
	delete(s.queuedAt, identifier)
	delete(s.startedAt, identifier)
	if len(registration.pending) != 0 && !s.stopped {
		next := registration.pending[0]
		registration.pending = registration.pending[1:]
		if len(registration.pending) == 0 {
			delete(s.pending, identifier)
		}
		s.startAccountPassLocked(identifier, registration, next)
		return
	}
	for _, next := range registration.pending {
		settleAccountPassLocked(next, errors.New("wake follow-up did not run"))
	}
	registration.active, registration.pending = nil, nil
	delete(s.pending, identifier)
	delete(s.manual, identifier)
	s.running[identifier] = false
}
