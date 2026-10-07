package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A ticket names one manual request to run a generic job. Tickets are issued
// in order per job (the request sequence). A run records the highest ticket
// issued when it takes the work gate (the sequence it covers); ticket n is
// answered by the first run to finish whose covered sequence is at least n.
// A run that had already taken the gate when a request arrived may have read
// its source before the request, so it never answers that request.
//
// Tickets live in memory. When the scheduler stops, every unanswered ticket
// is abandoned, and a ticket issued by an earlier daemon process reads as
// abandoned.

// TicketState is where a ticket stands.
type TicketState string

const (
	// TicketQueued: no run covering the ticket has taken the gate yet.
	TicketQueued TicketState = "queued"
	// TicketRunning: a run that started after the request is executing.
	TicketRunning TicketState = "running"
	// TicketCompleted: a run that started after the request finished
	// without error.
	TicketCompleted TicketState = "completed"
	// TicketFailed: a run that started after the request returned an error.
	TicketFailed TicketState = "failed"
	// TicketAbandoned: no run will answer the request (scheduler stopped,
	// job removed, follow-up dropped, or ticket from an earlier process).
	TicketAbandoned TicketState = "abandoned"
)

// Terminal reports whether the state can no longer change.
func (s TicketState) Terminal() bool {
	return s == TicketCompleted || s == TicketFailed || s == TicketAbandoned
}

// Ticket identifies one request. Epoch distinguishes scheduler instances so a
// ticket survives neither a restart nor a mix-up between schedulers.
type Ticket struct {
	Epoch string
	Seq   uint64
}

func (t Ticket) String() string {
	if t.Seq == 0 {
		return ""
	}
	return t.Epoch + "-" + strconv.FormatUint(t.Seq, 10)
}

// ErrInvalidTicket reports a ticket string that does not parse.
var ErrInvalidTicket = errors.New("invalid sync ticket")

// ErrUnknownTicket reports a ticket this scheduler never issued for the job.
var ErrUnknownTicket = errors.New("unknown sync ticket")

// ErrExpiredTicket reports a ticket whose result is no longer retained.
var ErrExpiredTicket = errors.New("sync ticket result is no longer retained")

// ParseTicket parses the form produced by Ticket.String.
func ParseTicket(s string) (Ticket, error) {
	epoch, seqText, ok := strings.Cut(s, "-")
	if !ok || epoch == "" {
		return Ticket{}, fmt.Errorf("%w: %q", ErrInvalidTicket, s)
	}
	seq, err := strconv.ParseUint(seqText, 10, 64)
	if err != nil || seq == 0 {
		return Ticket{}, fmt.Errorf("%w: %q", ErrInvalidTicket, s)
	}
	return Ticket{Epoch: epoch, Seq: seq}, nil
}

// TicketStatus reports a ticket and, once known, the run that answered it.
type TicketStatus struct {
	Ticket string      `json:"ticket"`
	Job    string      `json:"job"`
	State  TicketState `json:"state"`
	// Error is the run error for failed, or why the ticket was abandoned.
	Error string `json:"error,omitempty"`
	// RunStartedAt is when the answering run took the work gate.
	RunStartedAt time.Time `json:"run_started_at,omitzero"`
	// RunFinishedAt is when the answering run finished.
	RunFinishedAt time.Time `json:"run_finished_at,omitzero"`
}

// JobRequest is the result of a manual trigger: what the trigger did and the
// ticket to wait on.
type JobRequest struct {
	Disposition JobDisposition
	Ticket      Ticket
}

// ticketResolutionsKept bounds how many answered ranges each job remembers.
const ticketResolutionsKept = 64

// ticketResolution answers tickets from..through with one outcome.
type ticketResolution struct {
	from, through uint64
	state         TicketState
	err           string
	started       time.Time
	finished      time.Time
}

// jobTickets is the ticket state of one generic job. Guarded by Scheduler.mu.
type jobTickets struct {
	issued          uint64    // highest ticket issued
	covering        uint64    // covered sequence of the run holding the gate; 0 if none
	coveringStarted time.Time // when that run took the gate
	resolvedThrough uint64    // every ticket <= this is answered
	resolutions     []ticketResolution
}

func newTicketEpoch() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to
		// the clock so tickets stay distinct across restarts regardless.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// ticketsLocked returns the job's ticket state, creating it. Caller holds s.mu.
func (s *Scheduler) ticketsLocked(name string) *jobTickets {
	jt := s.genericTickets[name]
	if jt == nil {
		jt = &jobTickets{}
		s.genericTickets[name] = jt
	}
	return jt
}

// issueTicketLocked issues the next ticket for name. Caller holds s.mu.
func (s *Scheduler) issueTicketLocked(name string) Ticket {
	jt := s.ticketsLocked(name)
	jt.issued++
	return Ticket{Epoch: s.ticketEpoch, Seq: jt.issued}
}

// coverTicketsLocked records that a run of name took the gate: it covers
// every ticket issued so far. Caller holds s.mu.
func (s *Scheduler) coverTicketsLocked(name string, at time.Time) {
	jt := s.ticketsLocked(name)
	jt.covering = jt.issued
	jt.coveringStarted = at
	s.notifyTicketsLocked()
}

// uncoverTicketsLocked records that the gate-holding run ended without
// answering its tickets (it yielded or has more work); a follow-up run will
// cover them. Caller holds s.mu.
func (s *Scheduler) uncoverTicketsLocked(name string) {
	jt := s.ticketsLocked(name)
	jt.covering = 0
	jt.coveringStarted = time.Time{}
	s.notifyTicketsLocked()
}

// answerTicketsLocked resolves the tickets covered by the run that just
// finished. Caller holds s.mu.
func (s *Scheduler) answerTicketsLocked(name string, state TicketState, runErr error) {
	jt := s.ticketsLocked(name)
	through, started := jt.covering, jt.coveringStarted
	jt.covering = 0
	jt.coveringStarted = time.Time{}
	var msg string
	if runErr != nil {
		msg = runErr.Error()
	}
	s.resolveTicketsLocked(jt, through, state, msg, started)
}

// abandonTicketsLocked resolves every unanswered ticket of name as
// abandoned. Caller holds s.mu.
func (s *Scheduler) abandonTicketsLocked(name, reason string) {
	jt := s.ticketsLocked(name)
	if jt.issued <= jt.resolvedThrough {
		return
	}
	s.logger.Warn("sync tickets abandoned", "job", name,
		"tickets", jt.issued-jt.resolvedThrough, "reason", reason)
	jt.covering = 0
	jt.coveringStarted = time.Time{}
	s.resolveTicketsLocked(jt, jt.issued, TicketAbandoned, reason, time.Time{})
}

func (s *Scheduler) resolveTicketsLocked(jt *jobTickets, through uint64, state TicketState, msg string, started time.Time) {
	if through <= jt.resolvedThrough {
		return
	}
	jt.resolutions = append(jt.resolutions, ticketResolution{
		from:     jt.resolvedThrough + 1,
		through:  through,
		state:    state,
		err:      msg,
		started:  started,
		finished: time.Now(),
	})
	if extra := len(jt.resolutions) - ticketResolutionsKept; extra > 0 {
		jt.resolutions = append(jt.resolutions[:0:0], jt.resolutions[extra:]...)
	}
	jt.resolvedThrough = through
	s.notifyTicketsLocked()
}

// notifyTicketsLocked wakes every WaitTicket call. Caller holds s.mu.
func (s *Scheduler) notifyTicketsLocked() {
	close(s.ticketsChanged)
	s.ticketsChanged = make(chan struct{})
}

// ticketStatusLocked reports t's status and a channel closed on the next
// ticket change. Caller holds s.mu (read or write).
func (s *Scheduler) ticketStatusLocked(name string, t Ticket) (TicketStatus, <-chan struct{}, error) {
	status := TicketStatus{Ticket: t.String(), Job: name}
	if t.Epoch != s.ticketEpoch {
		status.State = TicketAbandoned
		status.Error = "ticket was issued by an earlier daemon process"
		return status, nil, nil
	}
	jt := s.genericTickets[name]
	if jt == nil || t.Seq > jt.issued {
		return TicketStatus{}, nil, fmt.Errorf("%w: %s for job %q", ErrUnknownTicket, t, name)
	}
	if t.Seq <= jt.resolvedThrough {
		for _, r := range jt.resolutions {
			if t.Seq >= r.from && t.Seq <= r.through {
				status.State = r.state
				status.Error = r.err
				status.RunStartedAt = r.started
				status.RunFinishedAt = r.finished
				return status, nil, nil
			}
		}
		return TicketStatus{}, nil, fmt.Errorf("%w: %s", ErrExpiredTicket, t)
	}
	if jt.covering >= t.Seq {
		status.State = TicketRunning
		status.RunStartedAt = jt.coveringStarted
	} else {
		status.State = TicketQueued
	}
	return status, s.ticketsChanged, nil
}

// WaitTicket blocks until the ticket reaches a terminal state or ctx ends,
// and returns its status at that point. A ctx that ends first is not an
// error: the returned status is simply not terminal yet, so an already-done
// ctx reads the status without waiting.
func (s *Scheduler) WaitTicket(ctx context.Context, name string, t Ticket) (TicketStatus, error) {
	for {
		s.mu.RLock()
		status, changed, err := s.ticketStatusLocked(name, t)
		s.mu.RUnlock()
		if err != nil || status.State.Terminal() {
			return status, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return status, nil
		}
	}
}
