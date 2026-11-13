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

	"github.com/cenkalti/backoff/v7"
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
// apply. Any other error retries the whole delivery with backoff; changes it
// already applied are applied again, which the archive's upserts allow.
type FieldHandler func(ctx context.Context, change Change) error

// ErrUnrecoverable marks a change that retrying cannot apply.
var ErrUnrecoverable = errors.New("change cannot be applied")

// maxRetryTime bounds how long one failing delivery blocks the deliveries
// after it. Polling sync still archives the comments it carried.
const maxRetryTime = time.Hour

// Worker applies stored deliveries oldest first and deletes each one after it
// is applied. A failing delivery blocks later ones while it is retried, so a
// retried delivery never overwrites newer changes. A delivery that still
// fails after maxRetryTime, or fails permanently, is logged and dropped.
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
	retry := backoff.NewExponentialBackOff()
	retry.InitialInterval, retry.MaxInterval = time.Second, 5*time.Minute
	_, err = backoff.Retry(ctx, func() (struct{}, error) {
		return struct{}{}, w.apply(ctx, app, envelope)
	}, backoff.WithBackOff(retry), backoff.WithMaxElapsedTime(maxRetryTime), backoff.WithNotify(func(err error, wait time.Duration) {
		slog.Warn("Meta webhook delivery failed; retrying", "delivery", name, "retry_in", wait, "error", err)
	}))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		slog.Warn("Meta webhook delivery dropped", "delivery", name, "error", err)
	}
	return remove(path)
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

// apply applies every change with a handler, in delivery order. Fields
// without a handler are logged and skipped.
func (w *Worker) apply(ctx context.Context, app string, envelope envelope) error {
	for _, entry := range envelope.Entry {
		for _, change := range entry.Changes {
			handler, ok := w.handlers[Field{envelope.Object, change.Field}]
			if !ok {
				slog.Info("Meta webhook field not handled", "object", envelope.Object, "field", change.Field, "entry", entry.ID)
				continue
			}
			err := handler(ctx, Change{App: app, EntryID: entry.ID, Values: []jsontext.Value{change.Value}})
			if errors.Is(err, ErrUnrecoverable) {
				slog.Warn("Meta webhook change skipped", "object", envelope.Object, "field", change.Field, "entry", entry.ID, "error", err)
			} else if err != nil {
				return err
			}
		}
	}
	return nil
}
