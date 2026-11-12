package metawebhook

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Change is one change of a delivery entry: entry.id names the Page or
// Instagram account. The worker passes one change at a time, so Values holds
// a single value.
type Change struct {
	App, EntryID string
	Values       []jsontext.Value
}

// Field selects the changes a FieldHandler applies, such as {"page", "feed"}.
type Field struct{ Object, Name string }

// FieldHandler applies one change for its field. An error that wraps
// ErrUnrecoverable is logged and the change skipped, and later changes still
// apply. An error that wraps ErrBusy waits for the source and retries for as
// long as it stays busy. Any other error retries the change with backoff
// within the delivery's retry budget. Changes that already succeeded are not
// applied again, except when the daemon restarts during a delivery: then the
// whole delivery is applied again, which the archive's upserts allow.
type FieldHandler func(ctx context.Context, change Change) error

// ErrUnrecoverable marks a change that retrying cannot apply.
var ErrUnrecoverable = errors.New("change cannot be applied")

// ErrBusy marks a change whose source is in use, such as by a polling sync.
// It is not a failure: the change is retried until the source is free, and
// the waits do not spend the delivery's retry budget.
var ErrBusy = errors.New("source is busy")

// maxRetryTime is one delivery's retry budget: the total time it may wait
// between retries of failed changes, and so about how long failures let it
// block the deliveries after it. Time spent in attempts and waiting for a
// busy source is not counted. Polling sync still archives the comments of
// changes that are skipped.
const maxRetryTime = time.Hour

// Worker applies stored deliveries oldest first, one change at a time, and
// deletes each delivery after it is applied. A failing or busy change blocks
// the changes and deliveries after it while it is retried, so a retried
// change never overwrites newer ones. Once a delivery has spent maxRetryTime
// waiting to retry failures, the failing change is logged and skipped, and
// each remaining change of that delivery gets a single attempt, still
// waiting out a busy source. A change that fails permanently is logged and
// skipped at once.
type Worker struct {
	pending  string
	handlers map[Field]FieldHandler
	wake     chan struct{}
}

func NewWorker(pending string, handlers map[Field]FieldHandler) *Worker {
	return &Worker{pending: pending, handlers: handlers, wake: make(chan struct{}, 1)}
}

// Wake makes Run look for new deliveries.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run applies deliveries, including any stored before a restart, until ctx
// ends or the pending directory cannot be used.
func (w *Worker) Run(ctx context.Context) error {
	for {
		entries, err := os.ReadDir(w.pending)
		if err != nil {
			return fmt.Errorf("read Meta webhook directory: %w", err)
		}
		applied := 0
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if err := w.process(ctx, entry.Name()); err != nil {
				return err
			}
			applied++
		}
		if applied > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.wake:
		}
	}
}

func (w *Worker) process(ctx context.Context, name string) error {
	path := filepath.Join(w.pending, name)
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Meta webhook delivery: %w", err)
	}
	_, rest, _ := strings.Cut(name, "-")
	app, _, _ := strings.Cut(rest, "-")
	var envelope envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		slog.Warn("Meta webhook delivery dropped", "delivery", name, "error", err)
		return remove(path)
	}
	// The retry budget covers the whole delivery, so failures block the queue
	// for about maxRetryTime at most.
	budget := maxRetryTime
	for _, entry := range envelope.Entry {
		for _, change := range entry.Changes {
			log := slog.With("delivery", name, "object", envelope.Object, "field", change.Field, "entry", entry.ID)
			handler, ok := w.handlers[Field{envelope.Object, change.Field}]
			if !ok {
				log.Info("Meta webhook field not handled")
				continue
			}
			apply := func() error {
				return handler(ctx, Change{App: app, EntryID: entry.ID, Values: []jsontext.Value{change.Value}})
			}
			err := applyWithRetry(ctx, log, &budget, apply)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				log.Warn("Meta webhook change skipped", "error", err)
			}
		}
	}
	return remove(path)
}

// applyWithRetry applies one change. A busy source is retried after a wait
// that grows from 1 second to 1 minute, for as long as it stays busy, and
// the wait is not charged to budget. Other failures are retried after a wait
// that grows from 1 second to 5 minutes, and each wait is subtracted from
// budget. When the next wait would overrun budget, the change fails and
// budget drops to zero, so the remaining changes of the delivery get one
// attempt each apart from busy waits.
func applyWithRetry(ctx context.Context, log *slog.Logger, budget *time.Duration, apply func() error) error {
	busyWait, failWait := time.Second, time.Second
	for {
		err := apply()
		var wait time.Duration
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrUnrecoverable):
			return fmt.Errorf("apply change: %w", err)
		case errors.Is(err, ErrBusy):
			if busyWait == time.Second { // log once per change, not every minute
				log.Info("Meta webhook source busy; waiting for it", "error", err)
			}
			wait, busyWait = busyWait, min(2*busyWait, time.Minute)
		case failWait > *budget:
			*budget = 0
			return fmt.Errorf("apply change: %w", err)
		default:
			wait, failWait = failWait, min(2*failWait, 5*time.Minute)
			*budget -= wait
			log.Warn("Meta webhook change failed; retrying", "retry_in", wait, "error", err)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func remove(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove applied Meta webhook delivery: %w", err)
	}
	return nil
}

type envelope struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Field string         `json:"field"`
			Value jsontext.Value `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}
