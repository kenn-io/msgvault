package metawebhook

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
)

// Terminal history pruning is maintenance, not part of each delivery drain.
const spoolPruneInterval = time.Hour

type WorkerOptions struct {
	// BeginWork protects each drain from idle shutdown without treating empty
	// maintenance ticks as new external activity.
	BeginWork func(context.Context) (func(), bool)
	// BeginApply protects archive handlers without gating spool or replay control.
	BeginApply func(context.Context) (func(), bool)
	// BeginApplyGroup optionally chooses an archive lease for one registered
	// object/field group. Other workers retain BeginApply behavior.
	BeginApplyGroup func(context.Context, string, string) (func(), bool)
	// ShouldYield reports queued interactive writers. Handler reads use a
	// cancellable context so unfinished work can resume without a failure.
	ShouldYield func() bool
	Spool       *Spool
	Registry    *Registry
	Batch       int
	Debounce    time.Duration
	Tick        time.Duration
	Retention   time.Duration
	OnApplied   func()
}
type DrainSummary struct{ Deliveries, Applied, Held, Retried, Failed int }
type Worker struct {
	o         WorkerOptions
	wake      chan struct{}
	lastPrune time.Time
	hasPruned bool
}

func NewWorker(o WorkerOptions) *Worker {
	if o.Batch <= 0 {
		o.Batch = 200
	}
	if o.Debounce <= 0 {
		o.Debounce = 2 * time.Second
	}
	if o.Tick <= 0 {
		o.Tick = 30 * time.Second
	}
	if o.Retention <= 0 {
		o.Retention = 720 * time.Hour
	}
	return &Worker{o: o, wake: make(chan struct{}, 1)}
}
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run owns all drains. Cancellation stops new batches, then lets the current
// batch finish settling so every accepted delivery remains recoverable.
func (w *Worker) Run(ctx context.Context) error {
	tick := time.NewTicker(w.o.Tick)
	defer tick.Stop()
	var timer *time.Timer
	var debounce <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.wake:
			if debounce == nil {
				timer = time.NewTimer(w.o.Debounce)
				debounce = timer.C
			}
		case <-tick.C:
			if _, err := w.DrainOnce(ctx); err != nil {
				return err
			}
			if err := w.pruneIfDue(); err != nil {
				return err
			}
		case <-debounce:
			debounce = nil
			if _, err := w.DrainOnce(ctx); err != nil {
				return err
			}
		}
	}
}

func (w *Worker) pruneIfDue() error {
	now := w.o.Spool.now()
	if w.hasPruned && now.Sub(w.lastPrune) < spoolPruneInterval {
		return nil
	}
	if _, err := w.o.Spool.Prune(w.o.Retention); err != nil {
		return err
	}
	w.lastPrune, w.hasPruned = now, true
	return nil
}

func (w *Worker) apply(ctx context.Context, group drainGroup) ([]ItemOutcome, bool) {
	if w.o.ShouldYield == nil {
		return group.handler(ctx, group.app, group.items), false
	}
	applyCtx, cancel := context.WithCancelCause(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-applyCtx.Done():
				return
			case <-ticker.C:
				if w.o.ShouldYield() {
					cancel(jobctx.ErrYieldedToWaiter)
					return
				}
			}
		}
	}()
	outcomes := group.handler(applyCtx, group.app, group.items)
	cancel(nil)
	<-watcherDone
	return outcomes, jobctx.YieldedToWaiter(applyCtx)
}

type drainDelivery struct {
	meta     DeliveryMeta
	items    []Item
	outcomes []ItemOutcome
}
type itemRef struct{ delivery, item int }
type drainGroup struct {
	object, field string
	app           string
	handler       ItemHandler
	items         []Item
	refs          []itemRef
}

// DrainOnce is the worker's single-consumer pass. No spool lock is retained
// during handler execution; Accept can continue while archive writes run.
func (w *Worker) DrainOnce(ctx context.Context) (sum DrainSummary, err error) {
	if w.o.BeginWork != nil {
		done, ok := w.o.BeginWork(ctx)
		if !ok {
			return sum, nil
		}
		defer done()
	}
	if err := w.o.Spool.ConsumeRequests(); err != nil {
		return sum, err
	}
	if err := ctx.Err(); err != nil {
		return sum, nil
	}
	due, err := w.o.Spool.Due(w.o.Batch)
	if err != nil {
		return sum, err
	}
	deliveries := make([]drainDelivery, len(due))
	groups := []drainGroup{}
	keys := map[string]int{}
	for d, meta := range due {
		body, err := w.o.Spool.Body(meta.AppID, meta.ID)
		if err != nil {
			return sum, err
		}
		items, decodeErr := Decode(meta.AppID, body)
		if decodeErr != nil {
			items = []Item{{AppID: meta.AppID, Object: "unknown", Field: "unknown", DeliveryID: meta.ID}}
		}
		delivery := drainDelivery{meta: meta, items: items, outcomes: make([]ItemOutcome, len(items))}
		order := meta.ReceiptOrder
		for i := range items {
			items[i].DeliveryID = meta.ID
			if order != 0 {
				items[i].ReceiptOrder = order
				order++
			}
		}
		start := 0
		if meta.NextItem >= 0 && meta.NextItem < len(items) {
			start = meta.NextItem
		}
		for offset := range items {
			i := (start + offset) % len(items)
			if decodeErr != nil {
				delivery.outcomes[i] = ItemOutcome{Class: Invalid, ErrorClass: "invalid_envelope"}
				continue
			}
			handler, ok := w.o.Registry.Lookup(items[i].Object, items[i].Field)
			if !ok {
				delivery.outcomes[i].Class = Unhandled
				continue
			}
			key := meta.AppID + "\x00" + items[i].Object + "\x00" + items[i].Field + "\x00" + items[i].EntryID
			g, ok := keys[key]
			if !ok {
				g = len(groups)
				keys[key] = g
				groups = append(groups, drainGroup{app: meta.AppID, object: items[i].Object, field: items[i].Field, handler: handler})
			}
			groups[g].items = append(groups[g].items, items[i])
			groups[g].refs = append(groups[g].refs, itemRef{d, i})
		}
		deliveries[d] = delivery
	}
	changed := false
	defer func() {
		if changed && w.o.OnApplied != nil {
			w.o.OnApplied()
		}
	}()
	yielded := false
	for _, group := range groups {
		if yielded {
			for _, ref := range group.refs {
				deliveries[ref.delivery].outcomes[ref.item] = ItemOutcome{Class: Retry, ErrorClass: "yielded_to_waiter"}
			}
			continue
		}
		var done func()
		ok := true
		if w.o.BeginApplyGroup != nil {
			done, ok = w.o.BeginApplyGroup(ctx, group.object, group.field)
		} else if w.o.BeginApply != nil {
			done, ok = w.o.BeginApply(ctx)
		}
		if !ok {
			return sum, nil
		}
		var outcomes []ItemOutcome
		outcomes, yielded = w.apply(ctx, group)
		if done != nil {
			done()
		}
		for i, ref := range group.refs {
			out := ItemOutcome{Class: Invalid, ErrorClass: "handler_outcome_missing"}
			if i < len(outcomes) {
				out = outcomes[i]
			}
			// Only interrupted reads lose their attempt. Earlier provider failures
			// remain failures even when a later item yields to an interactive writer.
			cancelled := out.ErrorClass == "cancelled_before_dispatch" || out.ErrorClass == "cancelled"
			if yielded && (i >= len(outcomes) || (out.Class == Retry && cancelled)) {
				out.Class, out.ErrorClass, out.CountAttempt = Retry, "yielded_to_waiter", false
			}
			changed = changed || out.ArchiveChanged || out.Class == Applied
			deliveries[ref.delivery].outcomes[ref.item] = out
		}
	}
	for _, delivery := range deliveries {
		settlement := Settle(delivery.outcomes, delivery.items)
		// Resume at the first budget-deferred item. Only a full circuit counts
		// as a delivery attempt, so every item gets a turn before exhaustion.
		start := 0
		if delivery.meta.NextItem >= 0 && delivery.meta.NextItem < len(delivery.items) {
			start = delivery.meta.NextItem
		}
		for offset := range delivery.items {
			i := (start + offset) % len(delivery.items)
			out := delivery.outcomes[i]
			if out.Class == Retry && out.ErrorClass == "request_budget" {
				settlement.NextItem = i
				if i >= start {
					settlement.CountAttempt = false
				}
				break
			}
		}
		settlement.Yielded = yielded && !settlement.CountAttempt
		if err := w.o.Spool.Settle(delivery.meta.AppID, delivery.meta.ID, settlement); err != nil {
			return sum, err
		}
		sum.Deliveries++
		for _, outcome := range delivery.outcomes {
			if outcome.Class == Applied {
				sum.Applied++
			}
		}
		switch settlement.State {
		case StateFailed, StateDone:
			// Terminal deliveries require no hold or retry accounting.
		case StateHeld:
			sum.Held++
		case StatePending:
			if settlement.CountAttempt && delivery.meta.Attempts+1 >= 12 {
				sum.Failed++
			} else {
				sum.Retried++
			}
		}
	}
	if yielded {
		w.Wake()
	}
	return sum, nil
}
