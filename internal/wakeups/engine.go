package wakeups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/netip"
	"net/url"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/httpretry"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

type Subscription struct {
	Name, URL     string
	StateKey      string
	Native        bool
	Secret        []byte
	Filter        changefeed.Filter
	MaxMessageAge time.Duration
	Client        webhookClient
}

// Store supplies the bounded feed and durable positions for a pass.
type Store interface {
	LoadWakeupStateKey(ctx context.Context, key string) (store.WakeupState, []byte, error)
	AdvanceWakeupStateKey(ctx context.Context, key string, before []byte, after store.WakeupState) (bool, error)
	ListChangedMessages(ctx context.Context, cursor store.ChangedMessagesCursor, limit int) (store.ChangedMessagePage, error)
	ArchiveUIDContext(ctx context.Context) (string, error)
}

// Engine holds no delivery state; the archive stores the position and backoff.
type Engine struct {
	Store         Store
	Subscriptions []Subscription
	Native        func(context.Context) ([]Subscription, error)
	Gate          func(context.Context) (func(), bool)
	Logger        *slog.Logger
	Now           func() time.Time
}

// ErrGateBusy indicates that the operation gate could not be acquired.
var ErrGateBusy = errors.New("operation gate unavailable")

// StoreErrorCategory returns only safe diagnostic categories.
func StoreErrorCategory(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return ""
	case errors.Is(err, ErrGateBusy):
		return "gate_busy"
	case errors.Is(err, store.ErrNullChangeWatermark):
		return "null_watermark"
	default:
		return "store_error"
	}
}

// GatedSetup runs a store step under the operation gate.
func (e *Engine) GatedSetup(ctx context.Context, fn func() error) error {
	if e.Gate != nil {
		release, ok := e.Gate(ctx)
		if !ok {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrGateBusy
		}
		defer release()
	}
	return fn()
}

// initialHead shares one feed bound among missing subscriptions in a store step.
type initialHead struct {
	page store.ChangedMessagePage
	read bool
}

func (e *Engine) initializeMissing(ctx context.Context, name string, head *initialHead) error {
	if !head.read {
		page, err := e.Store.ListChangedMessages(ctx, store.ChangedMessagesCursor{}, 1)
		if err != nil {
			return err
		}
		head.page, head.read = page, true
	}
	if head.page.CompleteThrough.IsZero() {
		return nil
	}
	swapped, err := e.Store.AdvanceWakeupStateKey(ctx, store.WakeupStateKey(name), nil, store.WakeupState{At: head.page.CompleteThrough})
	if err == nil && !swapped {
		logger := e.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("wakeup state conflict", "name", name, "category", "state_conflict")
	}
	return err
}

// InitializeMissing initializes absent positions without delivering. The caller holds the setup gate.
func (e *Engine) InitializeMissing(ctx context.Context) error {
	var head initialHead
	var loadErr error
	for _, sub := range e.Subscriptions {
		_, raw, err := e.Store.LoadWakeupStateKey(ctx, sub.StateKey)
		if err != nil {
			if loadErr == nil {
				loadErr = err
			}
			continue
		}
		if raw == nil {
			if err := e.initializeMissing(ctx, sub.Name, &head); err != nil {
				return err
			}
		}
	}
	return loadErr
}

func retryDelay(failures int) time.Duration {
	return httpretry.RetryAfterAtWithBase("", max(failures-1, 0), 30*time.Second, time.Hour, time.Time{})
}
func qualifies(m store.ChangedMessage, now time.Time, maxAge time.Duration) bool {
	if m.DeletedAt != nil || m.DeletedFromSourceAt != nil {
		return false
	}
	date := m.SentAt
	if date == nil {
		date = m.ReceivedAt
	}
	if date == nil {
		date = m.InternalDate
	}
	return date != nil && !date.Before(now.Add(-maxAge))
}
func deliveryID(name, from, to string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + from + "\x00" + to))
	return "wk_" + hex.EncodeToString(sum[:8])
}

type filterJSON struct {
	PersonID        int64   `json:"person_id,omitzero"`
	SourceIDs       []int64 `json:"source_ids,omitzero"`
	ConversationIDs []int64 `json:"conversation_ids,omitzero"`
	IncomingOnly    bool    `json:"incoming_only"`
}
type matchedJSON struct {
	Count      int     `json:"count"`
	MessageIDs []int64 `json:"message_ids"`
	Truncated  bool    `json:"truncated"`
}
type payload struct {
	SchemaVersion int         `json:"schema_version"`
	Type          string      `json:"type"`
	DeliveryID    string      `json:"delivery_id"`
	Name          string      `json:"name"`
	Filter        filterJSON  `json:"filter"`
	CursorFrom    string      `json:"cursor_from"`
	CursorTo      string      `json:"cursor_to"`
	Matched       matchedJSON `json:"matched"`
	ServerTime    time.Time   `json:"server_time"`
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func (e *Engine) RunPass(ctx context.Context) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}
	subscriptions := slices.Clone(e.Subscriptions)
	if e.Native != nil {
		err := e.GatedSetup(ctx, func() error {
			native, err := e.Native(ctx)
			if err == nil {
				subscriptions = append(subscriptions, native...)
			}
			return err
		})
		if err != nil {
			if category := StoreErrorCategory(err); category != "" {
				logger.Warn("native wakeups unavailable", "category", category)
			}
		}
	}

	pages := make(map[store.ChangedMessagesCursor]store.ChangedMessagePage)
	var head initialHead
	for _, sub := range subscriptions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logStoreError := func(err error) {
			if category := StoreErrorCategory(err); category != "" {
				logger.Warn("wakeup store step failed", "name", sub.Name, "category", category)
			}
		}
		var state store.WakeupState
		var raw []byte
		var next, since store.ChangedMessagesCursor
		var page store.ChangedMessagePage
		var matches []store.ChangedMessage
		var uid string
		var ready bool
		recordFailure := func(category string) {
			if sub.Native && category != "" {
				state.LastError = category
				if state.FailedSince == nil {
					stamp := e.now()
					state.FailedSince = &stamp
				}
			}
		}
		advance := func(before []byte) (bool, error) {
			swapped, err := e.Store.AdvanceWakeupStateKey(ctx, sub.StateKey, before, state)
			if err == nil && !swapped {
				logger.Warn("wakeup state conflict", "name", sub.Name, "category", "state_conflict")
			}
			return swapped, err
		}
		skipTo := func(next store.ChangedMessagesCursor) {
			state.At = next.At()
			state.AfterID, state.AfterRow = next.AfterID()
			state.Failures = 0
			state.NextAttemptAt = time.Time{}
		}
		err := e.GatedSetup(ctx, func() error {
			now := e.now()
			var err error
			state, raw, err = e.Store.LoadWakeupStateKey(ctx, sub.StateKey)
			if err != nil {
				return err
			}
			if raw != nil && now.Before(state.NextAttemptAt) {
				return nil
			}
			since = store.ChangedMessagesFrom(state.At)
			if state.AfterRow {
				since = store.ChangedMessagesAfter(state.At, state.AfterID)
			}
			if raw == nil {
				if sub.Native {
					return nil
				}
				return e.initializeMissing(ctx, sub.Name, &head)
			}
			uid, err = e.Store.ArchiveUIDContext(ctx)
			if err != nil {
				return err
			}
			var ok bool
			page, ok = pages[since]
			if !ok {
				page, err = e.Store.ListChangedMessages(ctx, since, changefeed.ScanBudget+1)
				if err != nil {
					return err
				}
				pages[since] = page
			}
			filter, err := changefeed.ResolveFilter(ctx, e.Store, sub.Filter)
			if err != nil {
				if !errors.Is(err, store.ErrPersonNotFound) && !errors.Is(err, resolver.ErrEmptyPopulation) {
					return err
				}
				state.NextAttemptAt = now.Add(retryDelay(1))
				recordFailure("person_unavailable")
				_, err = advance(raw)
				if err == nil {
					logger.Warn("wakeup failed", "name", sub.Name, "category", "person_unavailable", "failures", state.Failures)
				}
				return err
			}
			recovered := sub.Native && state.Failures == 0 && state.LastError == "person_unavailable"
			if recovered {
				state.LastError = ""
				state.FailedSince = nil
			}
			rows, position, _ := changefeed.Scan(page, since, filter, changefeed.ScanBudget)
			next = position
			for _, row := range rows {
				if qualifies(row, now, sub.MaxMessageAge) {
					matches = append(matches, row)
				}
			}
			if len(matches) == 0 && state.Failures == 0 {
				if len(page.Messages) > 0 || (!page.CompleteThrough.IsZero() && since.At().After(page.ServerTime)) || recovered {
					if len(page.Messages) > 0 || (!page.CompleteThrough.IsZero() && since.At().After(page.ServerTime)) {
						skipTo(next)
					}
					_, err = advance(raw)
					return err
				}
				return nil
			}
			state.Failures++
			state.NextAttemptAt = now.Add(retryDelay(state.Failures))
			nextRaw, err := store.EncodeWakeupState(state)
			if err != nil {
				return err
			}
			swapped, err := advance(raw)
			if err != nil {
				return err
			}
			if !swapped {
				return nil
			}
			raw = nextRaw
			ready = true
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logStoreError(err)
			continue
		}
		if !ready {
			continue
		}
		from := changefeed.EncodeCursor(uid, since, sub.Filter.Digest())
		to := changefeed.EncodeCursor(uid, next, sub.Filter.Digest())
		id := deliveryID(sub.Name, from, to)
		if sub.Native {
			id = "evt_" + id[3:]
		}
		var body []byte
		if sub.Native {
			body, err = json.Marshal(struct {
				EventID   string            `json:"eventId"`
				Name      string            `json:"name"`
				Timestamp time.Time         `json:"timestamp"`
				Data      map[string]string `json:"data"`
				Cursor    *string           `json:"cursor"`
			}{id, "msgvault.messages.changed", page.ServerTime, map[string]string{"subscription_id": sub.Name}, nil})
		} else {
			ids := make([]int64, 0, min(50, len(matches)))
			start := max(0, len(matches)-50)
			for _, m := range matches[start:] {
				ids = append(ids, m.ID)
			}
			body, err = json.Marshal(payload{1, "msgvault.wakeup", id, sub.Name, filterJSON{sub.Filter.PersonID, sub.Filter.SourceIDs, sub.Filter.ConversationIDs, sub.Filter.IncomingOnly}, from, to, matchedJSON{len(matches), ids, len(matches) > 50}, page.ServerTime})
		}
		if err != nil {
			logger.Warn("wakeup failed", "name", sub.Name, "category", "invalid_callback")
			continue
		}
		subscriptionID := ""
		if sub.Native {
			subscriptionID = sub.Name
		}
		category, status, _ := sub.Client.request(ctx, sub.URL, id, subscriptionID, sub.Secret, body, false)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if category != "" {
			logger.Warn("wakeup failed", "name", sub.Name, "category", category, "failures", state.Failures)
			if !sub.Native {
				continue
			}
		}
		recordFailure(category)
		if category == "" || sub.Native && (status == 410 || status == 413 || state.Failures >= 5) {
			skipTo(next)
			if sub.Native && category == "" {
				state.LastError = ""
				state.FailedSince = nil
				stamp := e.now()
				state.LastDeliveryAt = &stamp
			}
		} else {
			state.NextAttemptAt = e.now().Add(retryDelay(state.Failures))
		}
		err = e.GatedSetup(ctx, func() error { _, err := advance(raw); return err })
		if err != nil {
			logStoreError(err)
		}
	}
	return ctx.Err()
}

// NewSubscription builds the runtime-only endpoint and pinned destination policy.
func NewSubscription(name, endpoint string, secret []byte, filter changefeed.Filter, maxAge, timeout time.Duration, trustedOrigin *url.URL, trusted []netip.Addr) Subscription {
	return Subscription{Name: name, StateKey: store.WakeupStateKey(name), URL: endpoint, Secret: secret, Filter: filter, MaxMessageAge: maxAge, Client: webhookClient{timeout: timeout, trustedOrigin: trustedOrigin, trusted: trusted}}
}

// FilterFromConfig uses selectors normalized by config validation.
func FilterFromConfig(entry config.WakeupConfig) changefeed.Filter {
	var personID int64
	if entry.PersonID != nil {
		personID = *entry.PersonID
	}
	return changefeed.Filter{PersonID: personID, SourceIDs: entry.SourceIDs, ConversationIDs: entry.ConversationIDs, IncomingOnly: *entry.IncomingOnly}
}
