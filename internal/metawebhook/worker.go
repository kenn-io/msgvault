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
// apply. Any other error retries the change with backoff. Changes that already
// succeeded are not applied again, except when the daemon restarts during a
// delivery: then the whole delivery is applied again, which the archive's
// upserts allow.
type FieldHandler func(ctx context.Context, change Change) error

// ErrUnrecoverable marks a change that retrying cannot apply.
var ErrUnrecoverable = errors.New("change cannot be applied")

// maxRetryTime bounds how long one delivery spends retrying failed changes,
// and so how long it blocks the deliveries after it. Polling sync still
// archives the comments of changes that are skipped.
const maxRetryTime = time.Hour

// Worker applies stored deliveries oldest first, one change at a time, and
// deletes each delivery after it is applied. A failing change blocks the
// changes and deliveries after it while it is retried, so a retried change
// never overwrites newer ones. Once a delivery has spent maxRetryTime
// retrying, the failing change is logged and skipped, and each remaining
// change of that delivery gets a single attempt. A change that fails
// permanently is logged and skipped at once.
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
	// The retry budget covers the whole delivery, so one delivery blocks the
	// queue for about maxRetryTime at most.
	deadline := time.Now().Add(maxRetryTime)
	type item struct {
		field string
		value jsontext.Value
	}
	for _, entry := range envelope.Entry {
		// An entry's messaging events follow its changes, as the "messages"
		// field.
		var items []item
		for _, change := range entry.Changes {
			items = append(items, item{change.Field, change.Value})
		}
		for _, event := range entry.Messaging {
			items = append(items, item{"messages", event})
		}
		for _, it := range items {
			log := slog.With("delivery", name, "object", envelope.Object, "field", it.field, "entry", entry.ID)
			handler, ok := w.handlers[Field{envelope.Object, it.field}]
			if !ok {
				log.Info("Meta webhook field not handled")
				continue
			}
			apply := func() error {
				return handler(ctx, Change{App: app, EntryID: entry.ID, Values: []jsontext.Value{it.value}})
			}
			err := applyWithRetry(ctx, log, &deadline, apply)
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

// applyWithRetry applies one change, retrying failures with backoff until
// deadline. Without budget left it makes a single attempt. When the budget
// runs out during the retries it moves deadline to now, so the remaining
// changes of the delivery get one attempt each.
func applyWithRetry(ctx context.Context, log *slog.Logger, deadline *time.Time, apply func() error) error {
	left := time.Until(*deadline)
	if left <= 0 {
		return apply()
	}
	retry := backoff.NewExponentialBackOff()
	retry.InitialInterval, retry.MaxInterval = time.Second, 5*time.Minute
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		err := apply()
		if errors.Is(err, ErrUnrecoverable) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(retry), backoff.WithMaxElapsedTime(left), backoff.WithNotify(func(err error, wait time.Duration) {
		log.Warn("Meta webhook change failed; retrying", "retry_in", wait, "error", err)
	}))
	if errors.Is(err, backoff.ErrMaxElapsedTime) {
		// Backoff stops before a wait that would overrun the budget.
		*deadline = time.Now()
	}
	if err != nil {
		return fmt.Errorf("apply change: %w", err)
	}
	return nil
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
		// Messaging holds Messenger and Instagram messaging events, which
		// FieldHandlers see as the "messages" field.
		Messaging []jsontext.Value `json:"messaging"`
	} `json:"entry"`
}
