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
	"time"

	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

type Subscription struct {
	Name, URL     string
	Secret        []byte
	Filter        changefeed.Filter
	MaxMessageAge time.Duration
	Client        webhookClient
}

// Store supplies the bounded feed and durable positions for a pass.
type Store interface {
	LoadWakeupState(ctx context.Context, name string) (store.WakeupState, []byte, error)
	AdvanceWakeupState(ctx context.Context, name string, before []byte, after store.WakeupState) (bool, error)
	ListChangedMessages(ctx context.Context, cursor store.ChangedMessagesCursor, limit int) (store.ChangedMessagePage, error)
	ArchiveUIDContext(ctx context.Context) (string, error)
}

// Engine holds no delivery state; the archive stores the position and backoff.
type Engine struct {
	Store         Store
	Subscriptions []Subscription
	Gate          func(context.Context) (func(), bool)
	Logger        *slog.Logger
	Now           func() time.Time
}

func (e *Engine) gated(ctx context.Context, fn func() error) error {
	if e.Gate != nil {
		release, ok := e.Gate(ctx)
		if !ok {
			return errors.New("operation gate unavailable")
		}
		defer release()
	}
	return fn()
}
func retryDelay(failures int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < failures && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
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
	now := e.now()
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}
	pages := make(map[store.ChangedMessagesCursor]store.ChangedMessagePage)
	var headPage store.ChangedMessagePage
	headRead := false
	for _, sub := range e.Subscriptions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var state store.WakeupState
		var raw []byte
		var next, since store.ChangedMessagesCursor
		var page store.ChangedMessagePage
		var matches []store.ChangedMessage
		var uid string
		var ready bool
		advance := func(before []byte) error {
			swapped, err := e.Store.AdvanceWakeupState(ctx, sub.Name, before, state)
			if err == nil && !swapped {
				logger.Warn("wakeup state conflict", "name", sub.Name, "category", "state_conflict")
			}
			return err
		}
		err := e.gated(ctx, func() error {
			var err error
			state, raw, err = e.Store.LoadWakeupState(ctx, sub.Name)
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
				if !headRead {
					headPage, err = e.Store.ListChangedMessages(ctx, store.ChangedMessagesCursor{}, 1)
					if err != nil {
						return err
					}
					headRead = true
				}
				page = headPage
				if page.CompleteThrough.IsZero() {
					return nil
				}
				state.At = page.CompleteThrough
				return advance(nil)
			}
			filter, err := changefeed.ResolveFilter(ctx, e.Store, sub.Filter)
			if err != nil {
				if !errors.Is(err, store.ErrPersonNotFound) && !errors.Is(err, resolver.ErrEmptyPopulation) {
					return err
				}
				state.Failures++
				state.NextAttemptAt = now.Add(retryDelay(state.Failures))
				err = advance(raw)
				if err == nil {
					logger.Warn("wakeup failed", "name", sub.Name, "category", "person_unavailable", "failures", state.Failures)
				}
				return err
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
			ready = true
			rows, position, _ := changefeed.Scan(page, since, filter, changefeed.ScanBudget)
			next = position
			for _, row := range rows {
				if qualifies(row, now, sub.MaxMessageAge) {
					matches = append(matches, row)
				}
			}
			if len(matches) == 0 && state.Failures == 0 && len(page.Messages) > 0 {
				state.At = next.At()
				state.AfterID, state.AfterRow = next.AfterID()
				state.NextAttemptAt = time.Time{}
				err = advance(raw)
			}
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Warn("wakeup store step failed", "name", sub.Name, "category", "store_error")
			continue
		}
		if !ready || (len(matches) == 0 && state.Failures == 0) {
			continue
		}
		from := changefeed.EncodeCursor(uid, since, sub.Filter.Digest())
		to := changefeed.EncodeCursor(uid, next, sub.Filter.Digest())
		id := deliveryID(sub.Name, from, to)
		ids := make([]int64, 0, min(50, len(matches)))
		start := max(0, len(matches)-50)
		for _, m := range matches[start:] {
			ids = append(ids, m.ID)
		}
		body, err := json.Marshal(payload{1, "msgvault.wakeup", id, sub.Name, filterJSON{sub.Filter.PersonID, sub.Filter.SourceIDs, sub.Filter.ConversationIDs, sub.Filter.IncomingOnly}, from, to, matchedJSON{len(matches), ids, len(matches) > 50}, page.ServerTime})
		if err != nil {
			logger.Warn("wakeup failed", "name", sub.Name, "category", "invalid_callback")
			continue
		}
		category := sub.Client.send(ctx, sub.URL, id, sub.Secret, body)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if category == "" {
			state.At = next.At()
			state.AfterID, state.AfterRow = next.AfterID()
			state.Failures = 0
			state.NextAttemptAt = time.Time{}
		} else {
			state.Failures++
			state.NextAttemptAt = e.now().Add(retryDelay(state.Failures))
			logger.Warn("wakeup failed", "name", sub.Name, "category", category, "failures", state.Failures)
		}
		err = e.gated(ctx, func() error { return advance(raw) })
		if err != nil {
			logger.Warn("wakeup store step failed", "name", sub.Name, "category", "store_error")
		}
	}
	return ctx.Err()
}

// NewSubscription builds the runtime-only endpoint and pinned destination policy.
func NewSubscription(name, endpoint string, secret []byte, filter changefeed.Filter, maxAge, timeout time.Duration, trustedOrigin *url.URL, trusted []netip.Addr) Subscription {
	return Subscription{Name: name, URL: endpoint, Secret: secret, Filter: filter, MaxMessageAge: maxAge, Client: webhookClient{timeout: timeout, trustedOrigin: trustedOrigin, trusted: trusted}}
}

// FilterFromConfig uses selectors normalized by config validation.
func FilterFromConfig(entry config.WakeupConfig) changefeed.Filter {
	var personID int64
	if entry.PersonID != nil {
		personID = *entry.PersonID
	}
	return changefeed.Filter{PersonID: personID, SourceIDs: entry.SourceIDs, ConversationIDs: entry.ConversationIDs, IncomingOnly: *entry.IncomingOnly}
}
