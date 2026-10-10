package mcpevents

import (
	"context"
	"encoding/json/v2"
	"math/rand/v2"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/store"
)

type worker struct {
	generation int64
	cancel     context.CancelFunc
	done       chan struct{}
	wake       chan struct{}
}

func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
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
	var rows []store.MCPSubscription
	err := s.operation(ctx, func() error { var err error; rows, err = s.st.ListMCPSubscriptions(ctx, s.principal); return err })
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
		// A failed post-delivery authority check leaves the pending event intact.
		// Reconcile a stopped worker even when its subscription generation is unchanged.
		stopped := false
		select {
		case <-w.done:
			stopped = true
		default:
		}
		if stopped || active[id] != w.generation {
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
	for id, generation := range active {
		w := s.workers[id]
		if w == nil {
			workerCtx, cancel := context.WithCancel(ctx)
			w = &worker{generation: generation, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
			s.workers[id] = w
			go func() { defer close(w.done); s.runWorker(workerCtx, id, w) }()
		}
		select {
		case w.wake <- struct{}{}:
		default:
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
	reconciliation := time.NewTicker(time.Second)
	defer reconciliation.Stop()
	expiry := time.NewTicker(time.Minute)
	defer expiry.Stop()
	pruning := time.NewTicker(time.Hour)
	defer pruning.Stop()
	if err := s.reconcile(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
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
		if err := s.reconcile(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
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
	for ctx.Err() == nil {
		var delivery *store.MCPDelivery
		err := s.operation(ctx, func() error {
			var err error
			delivery, err = s.st.PrepareMCPDelivery(ctx, id, w.generation, time.Now().UTC(), func(sub store.MCPSubscription, event store.MCPEvent) ([]byte, error) {
				return json.Marshal(s.envelope(sub, event))
			})
			return err
		})
		if err != nil || delivery == nil {
			if ctx.Err() != nil {
				return
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-w.wake:
				timer.Stop()
			case <-timer.C:
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
		check := func(ctx context.Context) error {
			return s.operation(ctx, func() error {
				if err := s.st.CheckMCPSubscription(ctx, id, w.generation, time.Now().UTC()); err != nil {
					return safeStoreError(err)
				}
				var args struct {
					Kinds []string `json:"kinds"`
				}
				if err := json.Unmarshal(sub.Arguments, &args); err != nil {
					return invalid("invalid_subscription")
				}
				_, _, err := s.st.ValidateMCPEventScope(ctx, sub.Name, sub.ScopeKind, sub.ScopeID, requestedKinds(args.Kinds))
				return safeStoreError(err)
			})
		}
		status, header, _ := s.webhook.post(ctx, sub.CallbackURL, id, encodeEventID(s.key, id, sub.PendingSeq), sub.PendingEnvelope, current, previous, check)
		if ctx.Err() != nil {
			return
		}
		if err := check(ctx); err != nil {
			return
		}
		now := time.Now().UTC()
		retryAt := now.Add(retryDelay(status, header, sub.AttemptCount, now))
		if err := s.operation(ctx, func() error {
			return s.st.FinishMCPDelivery(ctx, id, w.generation, sub.PendingSeq, now, status, retryAt)
		}); err != nil && ctx.Err() != nil {
			return
		}
	}
}
