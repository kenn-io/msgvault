package metawebhook

import (
	"context"
	"time"
)

type WorkerOptions struct {
	Spool     *Spool
	Registry  *Registry
	Batch     int
	Debounce  time.Duration
	Tick      time.Duration
	Retention time.Duration
	OnApplied func()
	Snapshot  func() ReceiverSnapshot
}
type DrainSummary struct{ Deliveries, Applied, Held, Retried, Failed int }
type Worker struct {
	o    WorkerOptions
	wake chan struct{}
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
func (w *Worker) snapshot() error {
	if w.o.Snapshot == nil {
		return nil
	}
	snap := w.o.Snapshot()
	snap.WrittenAt = time.Now().UTC()
	return WriteReceiverSnapshot(w.o.Spool.dir, snap)
}

// Run owns all drains. Cancellation stops new batches, then lets the current
// batch finish settling so every accepted delivery remains recoverable.
func (w *Worker) Run(ctx context.Context) error {
	tick := time.NewTicker(w.o.Tick)
	defer tick.Stop()
	snapshots := time.NewTicker(5 * time.Second)
	defer snapshots.Stop()
	if err := w.snapshot(); err != nil {
		return err
	}
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
		case <-snapshots.C:
			if err := w.snapshot(); err != nil {
				return err
			}
		case <-tick.C:
			if err := w.snapshot(); err != nil {
				return err
			}
			if _, err := w.DrainOnce(ctx); err != nil {
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

type drainDelivery struct {
	meta     DeliveryMeta
	items    []Item
	outcomes []ItemOutcome
}
type itemRef struct{ delivery, item int }
type drainGroup struct {
	app     string
	handler ItemHandler
	items   []Item
	refs    []itemRef
}

// DrainOnce is the worker's single-consumer pass. No spool lock is retained
// during handler execution; Accept can continue while archive writes run.
func (w *Worker) DrainOnce(ctx context.Context) (sum DrainSummary, err error) {
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
		for i := range items {
			items[i].DeliveryID = meta.ID
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
				groups = append(groups, drainGroup{app: meta.AppID, handler: handler})
			}
			groups[g].items = append(groups[g].items, items[i])
			groups[g].refs = append(groups[g].refs, itemRef{d, i})
		}
		deliveries[d] = delivery
	}
	for _, group := range groups {
		outcomes := group.handler(ctx, group.app, group.items)
		for i, ref := range group.refs {
			out := ItemOutcome{Class: Invalid, ErrorClass: "handler_outcome_missing"}
			if i < len(outcomes) {
				out = outcomes[i]
			}
			deliveries[ref.delivery].outcomes[ref.item] = out
		}
	}
	defer func() {
		if sum.Applied > 0 && w.o.OnApplied != nil {
			w.o.OnApplied()
		}
	}()
	for _, delivery := range deliveries {
		settlement := Settle(delivery.outcomes, delivery.items)
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
	_, err = w.o.Spool.Prune(w.o.Retention)
	return sum, err
}
