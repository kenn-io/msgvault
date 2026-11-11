package mcpevents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"time"

	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/wakeups"
)

type entry struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Arguments Arguments `json:"arguments"`
	URL       string    `json:"url"`
	Secret    []byte    `json:"sealed_secret"`
	Expires   time.Time `json:"expires_at"`
}
type Service struct {
	Store         *store.Store
	Home          string
	Enabled       bool
	ConfigTargets int
	Now           func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
func (s *Service) catalog(ctx context.Context) ([]entry, []byte, error) {
	raw, err := s.Store.LoadMCPEventsCatalog(ctx)
	if err != nil {
		return nil, nil, err
	}
	var entries []entry
	if raw != nil {
		if err = json.Unmarshal(raw, &entries); err != nil {
			return nil, nil, err
		}
	}
	return entries, raw, nil
}
func (s *Service) key(ctx context.Context, entries []entry, create bool) (wakeups.EventKey, string, error) {
	uid, err := s.Store.ArchiveUIDContext(ctx)
	if err != nil {
		return wakeups.EventKey{}, "", err
	}
	var key wakeups.EventKey
	if create {
		key, err = wakeups.LoadEventKey(s.Home, len(entries) > 0)
	} else {
		key, err = wakeups.ReadEventKey(s.Home)
	}
	return key, uid, err
}
func (s *Service) Available(ctx context.Context) bool {
	if !s.Enabled || s.Store == nil {
		return false
	}
	entries, _, err := s.catalog(ctx)
	if err != nil {
		return false
	}
	_, _, err = s.key(ctx, entries, false)
	return err == nil || len(entries) == 0 && errors.Is(err, os.ErrNotExist)
}
func same(a, b Arguments) bool {
	return a.IncomingOnly != nil && b.IncomingOnly != nil && a.PersonID == b.PersonID && a.SourceID == b.SourceID && *a.IncomingOnly == *b.IncomingOnly
}
func find(entries []entry, args Arguments, url string) int {
	for i, e := range entries {
		if e.Owner == "owner" && same(e.Arguments, args) && e.URL == url {
			return i
		}
	}
	return -1
}
func (s *Service) Subscribe(ctx context.Context, in SubscribeRequest) (SubscribeResult, error) {
	var out SubscribeResult
	if !s.Enabled || s.Store == nil {
		return out, failure(Unavailable, "disabled")
	}
	if err := s.purgeExpired(ctx); err != nil {
		return out, failure(Unavailable, "store_unavailable")
	}
	filter := changefeed.Filter{PersonID: in.Arguments.PersonID, IncomingOnly: *in.Arguments.IncomingOnly}
	if in.Arguments.SourceID != 0 {
		filter.SourceIDs = []int64{in.Arguments.SourceID}
	}
	if _, err := changefeed.ResolveFilter(ctx, s.Store, filter); err != nil {
		return out, failure(InvalidParams, "invalid_scope")
	}
	for {
		entries, raw, err := s.catalog(ctx)
		if err != nil {
			return out, failure(Unavailable, "store_unavailable")
		}
		key, uid, err := s.key(ctx, entries, true)
		if err != nil {
			return out, failure(Unavailable, "key_unavailable")
		}
		index := find(entries, in.Arguments, in.Delivery.URL)
		id := ""
		var initial *store.WakeupState
		if index >= 0 {
			id = entries[index].ID
		} else {
			active := s.ConfigTargets
			for _, e := range entries {
				if s.now().Before(e.Expires) {
					active++
				}
			}
			if active >= 32 {
				return out, failure(ResourceExhausted, "subscriptions")
			}
			var b [16]byte
			if _, err = rand.Read(b[:]); err != nil {
				return out, failure(Unavailable, "random_unavailable")
			}
			id = hex.EncodeToString(b[:])
			page, err := s.Store.ListChangedMessages(ctx, store.ChangedMessagesCursor{}, 1)
			if err != nil || page.CompleteThrough.IsZero() {
				return out, failure(Unavailable, "head_unavailable")
			}
			initial = &store.WakeupState{At: page.CompleteThrough}
		}
		sealed, err := key.Seal(uid, id, in.secret)
		if err != nil {
			return out, failure(Unavailable, "key_unavailable")
		}
		expires := s.now().Add(in.TTL.Duration())
		item := entry{ID: id, Owner: "owner", Arguments: in.Arguments, URL: in.Delivery.URL, Secret: sealed, Expires: expires}
		if index >= 0 {
			entries[index] = item
		} else {
			entries = append(entries, item)
		}
		next, err := json.Marshal(entries)
		if err != nil {
			return out, failure(Unavailable, "store_unavailable")
		}
		createID := ""
		if initial != nil {
			createID = id
		}
		ok, err := s.Store.SwapMCPEventsCatalog(ctx, raw, next, createID, initial, "")
		if err != nil {
			return out, failure(Unavailable, "store_unavailable")
		}
		if !ok {
			continue
		}
		status := DeliveryStatus{Active: true}
		state, _, loadErr := s.Store.LoadWakeupStateKey(ctx, store.MCPEventsPositionKey(id))
		if loadErr != nil {
			return out, failure(Unavailable, "store_unavailable")
		}
		status.LastDeliveryAt = state.LastDeliveryAt
		status.LastError = state.LastError
		status.FailedSince = state.FailedSince
		return SubscribeResult{ID: id, RefreshBefore: expires, DeliveryStatus: status}, nil
	}
}
func (s *Service) Unsubscribe(ctx context.Context, in UnsubscribeRequest) error {
	if !s.Enabled || s.Store == nil {
		return failure(Unavailable, "disabled")
	}
	for {
		entries, raw, err := s.catalog(ctx)
		if err != nil {
			return failure(Unavailable, "store_unavailable")
		}
		idx := find(entries, in.Arguments, in.Delivery.URL)
		if idx < 0 {
			return failure(NotFound, "subscription")
		}
		id := entries[idx].ID
		entries = append(entries[:idx], entries[idx+1:]...)
		next, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		ok, err := s.Store.SwapMCPEventsCatalog(ctx, raw, next, "", nil, id)
		if err != nil {
			return failure(Unavailable, "store_unavailable")
		}
		if ok {
			return nil
		}
	}
}

// purgeExpired removes owner entries and positions without needing the key.
func (s *Service) purgeExpired(ctx context.Context) error {
	for {
		entries, raw, err := s.catalog(ctx)
		if err != nil {
			return err
		}
		index := -1
		for i, e := range entries {
			if e.Owner == "owner" && !s.now().Before(e.Expires) {
				index = i
				break
			}
		}
		if index < 0 {
			return nil
		}
		id := entries[index].ID
		entries = append(entries[:index:index], entries[index+1:]...)
		next, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		if _, err = s.Store.SwapMCPEventsCatalog(ctx, raw, next, "", nil, id); err != nil {
			return err
		}
	}
}

// Native loads live subscriptions and removes expired definitions before delivery.
func (s *Service) Native(ctx context.Context) ([]wakeups.Subscription, error) {
	if !s.Enabled {
		return nil, nil
	}
	if err := s.purgeExpired(ctx); err != nil {
		return nil, err
	}
	entries, _, err := s.catalog(ctx)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	key, uid, err := s.key(ctx, entries, false)
	if err != nil {
		return nil, err
	}
	var out []wakeups.Subscription
	for _, e := range entries {
		if e.Owner != "owner" {
			continue
		}
		secret, err := key.Open(uid, e.ID, e.Secret)
		if err != nil {
			return nil, errors.New("events secret unavailable")
		}
		if e.Arguments.IncomingOnly == nil {
			return nil, errors.New("invalid events catalog")
		}
		f := changefeed.Filter{PersonID: e.Arguments.PersonID, IncomingOnly: *e.Arguments.IncomingOnly}
		if e.Arguments.SourceID != 0 {
			f.SourceIDs = []int64{e.Arguments.SourceID}
		}
		sub := wakeups.NewSubscription(e.ID, e.URL, secret, f, 72*time.Hour, 15*time.Second, nil, nil)
		sub.Native = true
		sub.StateKey = store.MCPEventsPositionKey(e.ID)
		out = append(out, sub)
	}
	return out, nil
}
