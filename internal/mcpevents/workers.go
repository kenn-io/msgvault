package mcpevents

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/store"
)

type worker struct {
	// generation is the subscription generation this worker serves. Only the
	// worker goroutine writes it, and it does so under Service.workersMu so
	// reconciliation can read it; the worker goroutine itself reads it freely.
	generation int64
	// sending is set while the worker owes a control envelope for a Store step
	// that already committed. Reconciliation leaves such a worker running so
	// the envelope is not abandoned, and the worker ends or continues on its
	// own once it has been sent. Guarded by Service.workersMu.
	sending      bool
	cancel       context.CancelFunc
	done         chan struct{}
	wake         chan struct{}
	deliveryWake chan struct{}
}

func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) wakeAfterCommit() {
	select {
	case s.deliveryHints <- struct{}{}:
	default:
	}
	s.workersMu.Lock()
	for _, w := range s.workers {
		select {
		case w.deliveryWake <- struct{}{}:
		default:
		}
	}
	s.workersMu.Unlock()
}

// reconcileOperation promotes subscription discovery when a committed event
// arrives while the service is waiting behind scheduled work. A completed
// background read is preserved, and its priority is carried to the workers it
// creates.
func (s *Service) reconcileOperation(ctx context.Context, priority bool, fn func() error) (bool, error) {
	if priority {
		return true, s.deliveryOperation(ctx, fn)
	}
	select {
	case <-s.deliveryHints:
		return true, s.deliveryOperation(ctx, fn)
	default:
	}
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- s.operation(operationCtx, fn) }()
	select {
	case err := <-completed:
		return false, err
	case <-s.deliveryHints:
		cancel()
		err := <-completed
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		if err == nil {
			return true, nil
		}
		return true, s.deliveryOperation(ctx, fn)
	case <-ctx.Done():
		cancel()
		err := <-completed
		if err != nil {
			return false, err
		}
		return false, ctx.Err()
	}
}
func (s *Service) stopWorker(id string, keepGeneration int64) {
	s.workersMu.Lock()
	w := s.workers[id]
	if w == nil || w.generation == keepGeneration {
		s.workersMu.Unlock()
		return
	}
	delete(s.workers, id)
	w.cancel()
	s.workersMu.Unlock()
	<-w.done
}
func (s *Service) reconcileWithPriority(ctx context.Context, priority bool) error {
	var rows []store.MCPSubscription
	priority, err := s.reconcileOperation(ctx, priority, func() error { var err error; rows, err = s.st.ListMCPSubscriptions(ctx, s.principal); return err })
	if err != nil {
		return safeStoreError(err)
	}
	active := make(map[string]int64)
	now := s.now()
	for _, row := range rows {
		if row.State == "active" && now.Before(row.ExpiresAt) || row.State == "stopped" && row.StopReason == "scope_removed" && row.LastOutcome == "terminate_pending" {
			// A purge stopped the second kind in its own transaction; it
			// keeps a worker only long enough to send its terminated envelope.
			active[row.ID] = row.Generation
		}
	}
	s.workersMu.Lock()
	toStop := make([]*worker, 0)
	for id, w := range s.workers {
		// A worker that adopted a newer generation after a gap reset than
		// this snapshot saw is current; the snapshot is merely older.
		if generation, ok := active[id]; (!ok || generation > w.generation) && !w.sending {
			delete(s.workers, id)
			w.cancel()
			toStop = append(toStop, w)
		}
	}
	s.workersMu.Unlock()
	for _, w := range toStop {
		<-w.done
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	select {
	case <-s.deliveryHints:
		priority = true
	default:
	}
	for id, generation := range active {
		w := s.workers[id]
		if w == nil {
			workerCtx, cancel := context.WithCancel(ctx)
			w = &worker{generation: generation, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), deliveryWake: make(chan struct{}, 1)}
			s.workers[id] = w
			go func() { defer close(w.done); s.runWorker(workerCtx, id, w) }()
		}
		select {
		case w.wake <- struct{}{}:
		default:
		}
		if priority {
			select {
			case w.deliveryWake <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

// Run supervises subscription workers until ctx ends. Storage failures in
// maintenance or reconciliation are logged and retried on the next tick, so a
// transient outage does not stop delivery for the life of the daemon.
func (s *Service) Run(ctx context.Context) error {
	if !s.opts.Enabled {
		return nil
	}
	s.workersMu.Lock()
	if s.running {
		s.workersMu.Unlock()
		return &Error{Code: CodeConflict, Reason: "events_already_running"}
	}
	s.running = true
	s.workersMu.Unlock()
	defer func() {
		s.workersMu.Lock()
		var join sync.WaitGroup
		for id, w := range s.workers {
			delete(s.workers, id)
			w.cancel()
			join.Go(func() { <-w.done })
		}
		s.workersMu.Unlock()
		join.Wait()
		s.webhook.transport.CloseIdleConnections()
		s.workersMu.Lock()
		s.running = false
		s.workersMu.Unlock()
	}()
	now := s.now().UTC()
	priority := s.maintain(ctx, false, "expire_subscriptions", func() error { return s.st.ExpireMCPSubscriptions(ctx, now) })
	priority = s.maintain(ctx, priority, "prune_events", func() error { return s.st.PruneMCPEvents(ctx, now, s.opts.Retention) })
	reconciliation := time.NewTicker(time.Second)
	defer reconciliation.Stop()
	expiry := time.NewTicker(time.Minute)
	defer expiry.Stop()
	pruning := time.NewTicker(retentionSweepInterval(s.opts.Retention))
	defer pruning.Stop()
	for {
		if err := s.reconcileWithPriority(ctx, priority); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "MCP Events reconciliation failed", "reason", errorReason(err))
		}
		priority = false
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-s.deliveryHints:
			priority = true
		case <-reconciliation.C:
		case now := <-expiry.C:
			if s.opts.Now != nil {
				now = s.now()
			}
			priority = s.maintain(ctx, false, "expire_subscriptions", func() error { return s.st.ExpireMCPSubscriptions(ctx, now) })
		case now := <-pruning.C:
			if s.opts.Now != nil {
				now = s.now()
			}
			priority = s.maintain(ctx, false, "prune_events", func() error { return s.st.PruneMCPEvents(ctx, now, s.opts.Retention) })
		}
	}
}

// maintain runs Store maintenance like reconciliation: a committed event
// promotes a queued background acquisition to delivery work, and the returned
// priority carries into the following reconcile so new subscriptions get
// workers while scheduled work holds the gate. Failures are logged with a
// fixed reason and retried on a later tick.
func (s *Service) maintain(ctx context.Context, priority bool, operation string, fn func() error) bool {
	priority, err := s.reconcileOperation(ctx, priority, fn)
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "MCP Events maintenance failed", "operation", operation, "reason", errorReason(safeStoreError(err)))
	}
	return priority
}

func retentionSweepInterval(retention time.Duration) time.Duration {
	interval := min(time.Hour, retention/2)
	if interval < time.Second {
		return time.Second
	}
	return interval
}

func waitForWorker(ctx context.Context, w *worker, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-w.wake:
		return true
	case <-w.deliveryWake:
		select {
		case w.deliveryWake <- struct{}{}:
		default:
		}
		return true
	case <-timer.C:
		return true
	}
}

// workerOperation runs idle work in the background gate. A committed event
// cancels a queued idle acquisition and retries it as request-aware delivery
// work. If the background operation completed at the same time, its result is
// preserved and priority remains set for the next Store operation.
func (s *Service) workerOperation(ctx context.Context, w *worker, priority bool, fn func(context.Context) error) (bool, bool, error) {
	call := func(callCtx context.Context, delivery bool) error {
		operation := s.operation
		if delivery {
			operation = s.deliveryOperation
		}
		return operation(callCtx, func() error { return fn(callCtx) })
	}
	if priority {
		return true, true, call(ctx, true)
	}
	select {
	case <-w.deliveryWake:
		return true, true, call(ctx, true)
	default:
	}
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- call(operationCtx, false) }()
	select {
	case err := <-completed:
		return false, false, err
	case <-w.deliveryWake:
		cancel()
		err := <-completed
		if ctx.Err() != nil {
			return true, false, ctx.Err()
		}
		if err == nil {
			return true, false, nil
		}
		return true, true, call(ctx, true)
	case <-ctx.Done():
		cancel()
		err := <-completed
		if err != nil {
			return false, false, err
		}
		return false, false, ctx.Err()
	}
}

func waitForWorkerRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// mcpRetryBackoff is the base wait after delivery attempt 1 through 4; the
// fifth failed attempt is dead-lettered, so it needs no entry.
var mcpRetryBackoff = [store.MCPMaxDeliveryAttempts - 1]time.Duration{30 * time.Second, time.Minute, 3 * time.Minute, 8 * time.Minute}

const mcpMaxRetryAfter = 5 * time.Minute

func retryDelay(status int, header string, attempt int, now time.Time) time.Duration {
	if status == 429 || status == 503 {
		if delay, ok := httpretry.ParseRetryAfter(header, mcpMaxRetryAfter, now); ok {
			return delay
		}
	}
	attempt = min(max(attempt, 1), len(mcpRetryBackoff))
	base := mcpRetryBackoff[attempt-1]
	return base + time.Duration(rand.Int64N(int64(base/10)+1)) //nolint:gosec // Retry jitter does not generate security tokens.
}

func (s *Service) runWorker(ctx context.Context, id string, w *worker) {
	priority := false
	for ctx.Err() == nil {
		var delivery *store.MCPDelivery
		prepare := func(prepareCtx context.Context) error {
			var err error
			// Mark the worker before the Store step so a reconcile between the
			// commit and beginControl cannot cancel it and drop the envelope.
			s.setSending(w, true)
			delivery, err = s.st.PrepareMCPDelivery(prepareCtx, id, w.generation, s.now().UTC(), func(sub store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
				return json.Marshal(s.envelope(sub, event))
			})
			if err != nil || delivery == nil || delivery.Control == nil {
				s.setSending(w, false)
			} else {
				s.beginControl(w, delivery.Control)
			}
			return err
		}
		var usedDeliveryGate bool
		var err error
		priority, usedDeliveryGate, err = s.workerOperation(ctx, w, priority, prepare)
		if priority && !usedDeliveryGate && err == nil && delivery == nil {
			err = s.deliveryOperation(ctx, func() error { return prepare(ctx) })
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !waitForWorker(ctx, w, time.Second) {
				return
			}
			continue
		}
		if delivery == nil {
			priority = false
			if !waitForWorker(ctx, w, time.Second) {
				return
			}
			continue
		}
		if delivery.More {
			continue
		}
		if delivery.Control != nil {
			if !s.sendControl(ctx, w, *delivery.Control, priority) {
				return
			}
			continue
		}
		var keepRunning bool
		priority, keepRunning = s.deliver(ctx, w, delivery.Subscription, priority)
		if !keepRunning {
			return
		}
	}
}

// beginControl runs inside the Store operation that committed a control step.
// It marks the worker as owing an envelope, so reconciliation does not cancel
// it mid-send, and for a gap it adopts the generation the reset just committed.
// The worker, not a replacement, then sends the envelope and keeps serving the
// subscription. Other generation changes, such as secret rotation, replay and
// unsubscribe, still end the worker and let reconciliation start a new one.
func (s *Service) beginControl(w *worker, control *store.MCPControl) {
	if control == nil {
		return
	}
	s.workersMu.Lock()
	w.sending = true
	if control.Type == "gap" {
		w.generation = control.Subscription.Generation
	}
	s.workersMu.Unlock()
}

func (s *Service) setSending(w *worker, sending bool) {
	s.workersMu.Lock()
	w.sending = sending
	s.workersMu.Unlock()
}

func (s *Service) endControl(w *worker) { s.setSending(w, false) }

// controlBody builds the signed body for a control envelope.
func (s *Service) controlBody(control store.MCPControl) ([]byte, error) {
	switch control.Type {
	case "gap":
		return json.Marshal(struct {
			Type   string `json:"type"`
			Cursor string `json:"cursor"`
		}{"gap", encodeCursor(s.key, control.Subscription.ID, control.Cursor.Epoch, control.Cursor.Seq)})
	case "terminated":
		e := &Error{Code: CodeForbidden, Reason: control.Reason}
		return json.Marshal(struct {
			Type  string `json:"type"`
			Error struct {
				Code    int            `json:"code"`
				Message string         `json:"message"`
				Data    map[string]any `json:"data"`
			} `json:"error"`
		}{Type: "terminated", Error: struct {
			Code    int            `json:"code"`
			Message string         `json:"message"`
			Data    map[string]any `json:"data"`
		}{e.Code, e.WireMessage(), e.WireData()}})
	}
	return nil, invalid("invalid_envelope")
}

// sendControl delivers a gap or terminated envelope after the Store step that
// produced it has committed. It runs outside the operation gate except for the
// dial guard, posts once and ignores the receiver's answer: control bodies are
// advisory, and a receiver that misses one learns the same fact from its next
// refresh. It reports whether the worker should keep running; a terminated
// subscription has nothing left to serve.
func (s *Service) sendControl(ctx context.Context, w *worker, control store.MCPControl, priority bool) bool {
	defer s.endControl(w)
	sub := control.Subscription
	keepRunning := control.Type == "gap"
	body, err := s.controlBody(control)
	if err != nil {
		slog.WarnContext(ctx, "MCP Events control envelope failed", "subscription_id", sub.ID, "type", control.Type, "reason", errorReason(err))
		return keepRunning
	}
	current, previous, err := s.deliverySecrets(sub)
	if err != nil {
		slog.WarnContext(ctx, "MCP Events control envelope failed", "subscription_id", sub.ID, "type", control.Type, "reason", "events_key_unavailable")
		return keepRunning
	}
	var guard func(context.Context) error
	if control.Type == "gap" {
		// The subscription is still active, so authorization is rechecked
		// before each dial. A terminated subscription is already stopped; its
		// scope is gone, so only the transport's address checks apply.
		guard = s.dialGuard(sub.Generation, sub, priority, false)
	}
	status, err := s.webhook.postControl(ctx, sub.CallbackURL, sub.ID, control.Type, body, current, previous, guard)
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		slog.WarnContext(ctx, "MCP Events control envelope failed", "subscription_id", sub.ID, "type", control.Type, "reason", errorReason(err))
		return keepRunning
	}
	slog.InfoContext(ctx, "MCP Events control envelope sent", "subscription_id", sub.ID, "type", control.Type, "status_class", mcpStatusClass(status))
	return keepRunning
}

func mcpStatusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	}
	return "other"
}

// checkDelivery verifies that sub may still receive its pending occurrence.
// It reads only its arguments, so it is safe on a transport dial goroutine.
func (s *Service) checkDelivery(ctx context.Context, generation int64, sub store.MCPSubscription, beforeDelivery bool) error {
	now := s.now().UTC()
	if err := s.st.CheckMCPSubscription(ctx, sub.ID, generation, now); err != nil {
		return safeStoreError(err)
	}
	if beforeDelivery {
		if err := s.st.CheckMCPDeliveryOccurrence(ctx, sub.ID, generation, sub.PendingSeq, now); err != nil {
			return safeStoreError(err)
		}
	}
	var args struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal(sub.Arguments, &args); err != nil {
		return invalid("invalid_subscription")
	}
	_, _, err := s.st.ValidateMCPEventScope(ctx, sub.Name, sub.ScopeKind, sub.ScopeID, requestedKinds(args.Kinds))
	return safeStoreError(err)
}

// retryableCheckError reports whether a delivery check or receipt update
// failed for a transient reason, such as storage or gate unavailability,
// rather than a definitive state or authorization change.
func retryableCheckError(err error) bool {
	safe, ok := errors.AsType[*Error](err)
	return !ok || safe.Reason == "events_storage_unavailable"
}

// errorReason returns an allowlisted reason for logging a delivery failure. It
// never includes URLs, response bodies, driver messages or secrets. Errors
// without a safe reason come from the Store or its gate.
func errorReason(err error) string {
	if safe, ok := errors.AsType[*Error](err); ok {
		if allowed, ok := SafeError(safe.Code, safe.Reason); ok {
			return allowed.Reason
		}
	}
	return "events_storage_unavailable"
}

func (s *Service) deliverySecrets(sub store.MCPSubscription) ([]byte, []byte, error) {
	current, err := decryptSecret(s.key, sub.ID, "current", sub.SecretEnc)
	if err != nil {
		return nil, nil, err
	}
	if !s.now().Before(sub.PreviousSecretUntil) || len(sub.PreviousSecretEnc) == 0 {
		return current, nil, nil
	}
	previous, err := decryptSecret(s.key, sub.ID, "previous", sub.PreviousSecretEnc)
	if err != nil {
		return nil, nil, err
	}
	return current, previous, nil
}

// deliveryAttempt carries one prepared occurrence through sending and
// receipt. It is owned by the worker goroutine.
type deliveryAttempt struct {
	s        *Service
	w        *worker
	sub      store.MCPSubscription
	priority bool
}

// deliver sends one prepared occurrence and records its outcome. It returns
// the worker's gate priority and whether the worker should keep running.
//
// Once PrepareMCPDelivery has counted an attempt, the worker holds the
// delivery in memory: a transient failure before sending is retried without
// preparing again, and a transient failure after sending retries the receipt
// without resending. A definitive check failure returns to
// PrepareMCPDelivery, which applies the subscription's durable state.
func (s *Service) deliver(ctx context.Context, w *worker, sub store.MCPSubscription, priority bool) (bool, bool) {
	a := &deliveryAttempt{s: s, w: w, sub: sub, priority: priority}
	current, previous, err := s.deliverySecrets(sub)
	if err != nil {
		// Startup refuses to run with undecryptable secrets, so this row changed
		// underneath the daemon. Retrying cannot succeed; a new secret or an
		// unsubscribe changes the generation and reconciliation replaces this
		// worker. Park without consuming further attempts.
		slog.ErrorContext(ctx, "MCP Events delivery stopped", "subscription_id", sub.ID, "reason", "events_key_unavailable")
		<-ctx.Done()
		return a.priority, false
	}
	status, header, sent, keepRunning := a.send(ctx, current, previous)
	if !sent {
		return a.priority, keepRunning
	}
	return a.priority, a.record(ctx, status, header)
}

func (a *deliveryAttempt) check(ctx context.Context, beforeDelivery bool) error {
	var err error
	a.priority, _, err = a.s.workerOperation(ctx, a.w, a.priority, func(operationCtx context.Context) error {
		return a.s.checkDelivery(operationCtx, a.w.generation, a.sub, beforeDelivery)
	})
	return err
}

// retry handles a failed check or receipt update. It reports whether to try
// again in place; otherwise keepRunning says whether the worker continues.
func (a *deliveryAttempt) retry(ctx context.Context, message string, err error) (again, keepRunning bool) {
	if ctx.Err() != nil {
		return false, false
	}
	if !retryableCheckError(err) {
		return false, waitForWorker(ctx, a.w, time.Second)
	}
	slog.WarnContext(ctx, message, "subscription_id", a.sub.ID, "seq", a.sub.PendingSeq, "attempt", a.sub.AttemptCount, "reason", errorReason(err))
	if !waitForWorkerRetry(ctx, time.Second) {
		return false, false
	}
	return true, true
}

// send posts the occurrence once authorization passes. sent is false when
// nothing may have reached the receiver, and keepRunning then says whether
// the worker continues.
func (a *deliveryAttempt) send(ctx context.Context, current, previous []byte) (status int, header string, sent, keepRunning bool) {
	eventID := encodeEventID(a.s.key, a.sub.ID, a.sub.PendingSeq)
	for {
		err := a.check(ctx, true)
		if err == nil {
			guard := a.s.dialGuard(a.w.generation, a.sub, a.priority, true)
			signing := previous
			if !a.s.now().Before(a.sub.PreviousSecretUntil) {
				// Checks and retries can outlast the rotation overlap.
				signing = nil
			}
			status, header, err = a.s.webhook.post(ctx, a.sub.CallbackURL, a.sub.ID, eventID, a.sub.PendingEnvelope, current, signing, guard)
			if ctx.Err() != nil {
				return 0, "", false, false
			}
			if err == nil {
				return status, header, true, true
			}
			if _, refused := errors.AsType[*dialRefusedError](err); !refused {
				slog.WarnContext(ctx, "MCP Events delivery failed", "subscription_id", a.sub.ID, "seq", a.sub.PendingSeq, "attempt", a.sub.AttemptCount, "reason", errorReason(err))
				return 0, "", true, true
			}
			// The request failed because its own dial guard refused, so nothing
			// was sent. A refusal on a dial the request abandoned for another
			// connection does not reach this error. The next pass
			// rechecks authorization and decides whether to retry in place.
			slog.WarnContext(ctx, "MCP Events delivery check failed", "subscription_id", a.sub.ID, "seq", a.sub.PendingSeq, "attempt", a.sub.AttemptCount, "reason", errorReason(err))
			if !waitForWorkerRetry(ctx, time.Second) {
				return 0, "", false, false
			}
			continue
		}
		again, keepRunning := a.retry(ctx, "MCP Events delivery check failed", err)
		if !again {
			return 0, "", false, keepRunning
		}
	}
}

// record rechecks authorization and stores the delivery outcome. The outcome
// stays in memory until it is recorded, so retries never resend.
func (a *deliveryAttempt) record(ctx context.Context, status int, header string) bool {
	now := a.s.now().UTC()
	retryAt := now.Add(retryDelay(status, header, a.sub.AttemptCount, now))
	for {
		err := a.check(ctx, false)
		if err == nil {
			finishNow := a.s.now().UTC()
			var control *store.MCPControl
			a.priority, _, err = a.s.workerOperation(ctx, a.w, a.priority, func(operationCtx context.Context) error {
				var finishErr error
				a.s.setSending(a.w, true)
				control, finishErr = a.s.st.FinishMCPDelivery(operationCtx, a.sub.ID, a.w.generation, a.sub.PendingSeq, finishNow, status, retryAt)
				if finishErr != nil || control == nil {
					a.s.setSending(a.w, false)
				} else {
					a.s.beginControl(a.w, control)
				}
				return finishErr
			})
			if err == nil {
				if control != nil {
					return a.s.sendControl(ctx, a.w, *control, a.priority)
				}
				return true
			}
		}
		again, keepRunning := a.retry(ctx, "MCP Events delivery receipt update failed", err)
		if !again {
			return keepRunning
		}
	}
}

// dialGuard rechecks authorization before each new connection's dial. When
// occurrence is true it also requires the pending occurrence to be retained;
// control envelopes carry none. It may
// run on a transport goroutine, so it uses a snapshot of the gate priority and
// never touches worker state.
func (s *Service) dialGuard(generation int64, sub store.MCPSubscription, priority, occurrence bool) func(context.Context) error {
	operation := s.operation
	if priority {
		operation = s.deliveryOperation
	}
	return func(ctx context.Context) error {
		return operation(ctx, func() error { return s.checkDelivery(ctx, generation, sub, occurrence) })
	}
}
