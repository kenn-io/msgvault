package mcpevents

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/store"
)

type worker struct {
	generation   int64
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
func (s *Service) reconcile(ctx context.Context) error {
	return s.reconcileWithPriority(ctx, false)
}

func (s *Service) reconcileWithPriority(ctx context.Context, priority bool) error {
	var rows []store.MCPSubscription
	priority, err := s.reconcileOperation(ctx, priority, func() error { var err error; rows, err = s.st.ListMCPSubscriptions(ctx, s.principal); return err })
	if err != nil {
		return safeStoreError(err)
	}
	active := make(map[string]int64)
	now := time.Now()
	for _, row := range rows {
		if row.State == "active" && now.Before(row.ExpiresAt) {
			active[row.ID] = row.Generation
		}
	}
	s.workersMu.Lock()
	toStop := make([]*worker, 0)
	for id, w := range s.workers {
		if active[id] != w.generation {
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
func (s *Service) Run(ctx context.Context) error {
	if !s.opts.Enabled {
		return nil
	}
	s.workersMu.Lock()
	if s.running {
		s.workersMu.Unlock()
		return &Error{Code: -32013, Reason: "events_already_running"}
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
	if err := s.cleanupRetention(ctx, time.Now().UTC()); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return safeStoreError(err)
	}
	reconciliation := time.NewTicker(time.Second)
	defer reconciliation.Stop()
	expiry := time.NewTicker(time.Minute)
	defer expiry.Stop()
	pruning := time.NewTicker(retentionSweepInterval(s.opts.Retention))
	defer pruning.Stop()
	if err := s.reconcile(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	for {
		deliveryPriority := false
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-s.deliveryHints:
			deliveryPriority = true
		case <-reconciliation.C:
		case now := <-expiry.C:
			if err := s.operation(ctx, func() error { return s.st.ExpireMCPSubscriptions(ctx, now) }); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return safeStoreError(err)
			}
		case now := <-pruning.C:
			if err := s.operation(ctx, func() error { return s.st.PruneMCPEvents(ctx, now, s.opts.Retention) }); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return safeStoreError(err)
			}
		}
		if err := s.reconcileWithPriority(ctx, deliveryPriority); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
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

func retryDelay(status int, header string, attempt int, now time.Time) time.Duration {
	if status == 429 || status == 503 {
		if delay, ok := httpretry.ParseRetryAfter(header, time.Hour, now); ok {
			return delay
		}
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 12 {
		attempt = 12
	}
	base := time.Second * time.Duration(1<<uint(attempt-1))
	base = min(base, 15*time.Minute)
	return base + time.Duration(rand.Int64N(int64(base/4)+1)) //nolint:gosec // Retry jitter does not generate security tokens.
}
func (s *Service) runWorker(ctx context.Context, id string, w *worker) {
	priority := false
workerLoop:
	for ctx.Err() == nil {
		var delivery *store.MCPDelivery
		prepare := func(prepareCtx context.Context) error {
			var err error
			delivery, err = s.st.PrepareMCPDelivery(prepareCtx, id, w.generation, time.Now().UTC(), func(sub store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
				return json.Marshal(s.envelope(sub, event))
			})
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
		sub := delivery.Subscription
		current, err := decryptSecret(s.key, id, "current", sub.SecretEnc)
		if err != nil {
			return
		}
		var previous []byte
		if time.Now().Before(sub.PreviousSecretUntil) && len(sub.PreviousSecretEnc) > 0 {
			previous, err = decryptSecret(s.key, id, "previous", sub.PreviousSecretEnc)
			if err != nil {
				return
			}
		}
		check := func(checkCtx context.Context, beforeDelivery bool) error {
			var checkErr error
			priority, _, checkErr = s.workerOperation(checkCtx, w, priority, func(operationCtx context.Context) error {
				now := time.Now().UTC()
				if err := s.st.CheckMCPSubscription(operationCtx, id, w.generation, now); err != nil {
					return safeStoreError(err)
				}
				if beforeDelivery {
					if err := s.st.CheckMCPDeliveryOccurrence(operationCtx, id, w.generation, sub.PendingSeq, now); err != nil {
						return safeStoreError(err)
					}
				}
				var args struct {
					Kinds []string `json:"kinds"`
				}
				if err := json.Unmarshal(sub.Arguments, &args); err != nil {
					return invalid("invalid_subscription")
				}
				_, _, err := s.st.ValidateMCPEventScope(operationCtx, sub.Name, sub.ScopeKind, sub.ScopeID, requestedKinds(args.Kinds))
				return safeStoreError(err)
			})
			return checkErr
		}
		guard := func(checkCtx context.Context) error { return check(checkCtx, true) }
		status, header, _ := s.webhook.post(ctx, sub.CallbackURL, id, encodeEventID(s.key, id, sub.PendingSeq), sub.PendingEnvelope, current, previous, guard)
		if ctx.Err() != nil {
			return
		}
		if err := check(ctx, false); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Keep the durable pending envelope when authorization cannot be
			// rechecked after delivery. A later attempt revalidates before sending
			// and preserves the at-least-once event ID and bytes.
			if !waitForWorker(ctx, w, time.Second) {
				return
			}
			continue
		}
		now := time.Now().UTC()
		retryAt := now.Add(retryDelay(status, header, sub.AttemptCount, now))
		// Keep the ACK in memory and retry its idempotent receipt update instead
		// of counting the same acknowledged envelope as another delivery. If the
		// worker exits first, the durable pending envelope is replayed on restart.
		for {
			finishNow := time.Now().UTC()
			priority, _, err = s.workerOperation(ctx, w, priority, func(operationCtx context.Context) error {
				return s.st.FinishMCPDelivery(operationCtx, id, w.generation, sub.PendingSeq, finishNow, status, retryAt)
			})
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			slog.WarnContext(ctx, "MCP Events delivery receipt update failed",
				"subscription_id", id,
				"seq", sub.PendingSeq,
				"attempt", sub.AttemptCount,
				"error", safeStoreError(err).Error(),
			)
			delay := max(time.Until(retryAt), time.Second)
			if !waitForWorkerRetry(ctx, delay) {
				return
			}
			if err := check(ctx, false); err != nil {
				if ctx.Err() != nil {
					return
				}
				if !waitForWorker(ctx, w, time.Second) {
					return
				}
				continue workerLoop
			}
		}
	}
}
