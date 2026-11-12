package mcpevents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/wakeups"
)

type entry struct {
	ID        string             `json:"id"`
	Owner     string             `json:"owner"`
	Arguments MCPEventsArguments `json:"arguments"`
	Origin    string             `json:"origin"`
	Expires   time.Time          `json:"expires_at"`
}
type privateCallback struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

type Service struct {
	Store         *store.Store
	TokensDir     string
	Grants        *agentgrant.Registry
	Enabled       bool
	ConfigTargets int
	Now           func() time.Time
	Logger        *slog.Logger
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
		if err = json.Unmarshal(raw, &entries, json.RejectUnknownMembers(true)); err != nil {
			return nil, nil, err
		}
	}
	return entries, raw, nil
}
func (s *Service) Available(_ context.Context) bool {
	return s.Enabled && s.Store != nil
}

// callbackOrigin deliberately excludes paths and queries from the credential binding.
func callbackOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse callback URL: %w", err)
	}
	return providercredentials.EndpointOrigin(u.Scheme + "://" + u.Host)
}

func (s *Service) credentialSnapshot(id string) (providercredentials.Snapshot, string, error) {
	snapshot, err := providercredentials.ReadEvents(s.TokensDir)
	if err != nil {
		return snapshot, "", err
	}
	revision, err := snapshot.Revision(providercredentials.EventsID(id))
	return snapshot, revision, err
}

func (s *Service) deleteCredential(id string) error {
	snapshot, revision, err := s.credentialSnapshot(id)
	if err != nil {
		return err
	}
	credentialID := providercredentials.EventsID(id)
	if !snapshot.Stored(credentialID) {
		return nil
	}
	_, err = providercredentials.DeleteIfRevision(s.TokensDir, revision, credentialID, func() error { return nil })
	return err
}
func subscriptionID(owner string, args MCPEventsArguments, url string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%t\x00%s", owner, Name, args.PersonID, args.SourceID, *args.IncomingOnly, url)))
	return hex.EncodeToString(sum[:16])
}
func find(entries []entry, owner string, args MCPEventsArguments, url string) int {
	id := subscriptionID(owner, args, url)
	for i, e := range entries {
		if e.Owner == owner && (e.ID == id || (owner != "owner" && len(e.ID) == 64 && strings.HasPrefix(e.ID, id))) {
			return i
		}
	}
	return -1
}

// Subscribe requires a request returned by DecodeSubscribe.
func (s *Service) Subscribe(ctx context.Context, in MCPEventsSubscribeRequest, owner string, verify func(context.Context, string, string, []byte) error, creationCheck func(context.Context) error) (MCPEventsSubscribeResult, error) {
	var out MCPEventsSubscribeResult
	if !s.Enabled || s.Store == nil || owner == "" {
		return out, failure(Unavailable, "disabled")
	}
	if err := s.purgeExpired(ctx); err != nil {
		return out, failure(Unavailable, "store_unavailable")
	}
	if owner == "owner" {
		filter := in.Arguments.filter()
		if _, err := changefeed.ResolveFilter(ctx, s.Store, filter); err != nil {
			if errors.Is(err, store.ErrPersonNotFound) || errors.Is(err, resolver.ErrEmptyPopulation) {
				return out, failure(InvalidParams, "invalid_scope")
			}
			return out, failure(Unavailable, "store_unavailable")
		}
		if in.Arguments.SourceID != 0 {
			if _, err := s.Store.GetSourceByIDContext(ctx, in.Arguments.SourceID); err != nil {
				if errors.Is(err, store.ErrSourceNotFound) {
					return out, failure(InvalidParams, "invalid_scope")
				}
				return out, failure(Unavailable, "store_unavailable")
			}
		}
	}
	entries, _, err := s.catalog(ctx)
	if err != nil {
		return out, failure(Unavailable, "store_unavailable")
	}
	index := find(entries, owner, in.Arguments, in.Delivery.URL)
	refresh := index >= 0
	var id string
	if refresh {
		if len(entries)+s.ConfigTargets > 32 {
			return out, failure(ResourceExhausted, "subscriptions")
		}
		id = entries[index].ID
		_, position, err := s.Store.LoadWakeupStateKey(ctx, store.MCPEventsPositionKey(id))
		if err != nil || position == nil {
			return out, failure(Unavailable, "store_unavailable")
		}
	} else {
		id = subscriptionID(owner, in.Arguments, in.Delivery.URL)
		if owner != "owner" {
			var b [16]byte
			if _, err = rand.Read(b[:]); err != nil {
				return out, failure(Unavailable, "random_unavailable")
			}
			id += hex.EncodeToString(b[:])
		}
	}
	if !refresh && s.subscriptionLimit(entries, owner) {
		return out, failure(ResourceExhausted, "subscriptions")
	}
	if !refresh && owner == "owner" {
		page, err := s.Store.ListChangedMessages(ctx, store.ChangedMessagesCursor{}, 1)
		if err != nil || page.CompleteThrough.IsZero() {
			return out, failure(Unavailable, "head_unavailable")
		}
	}
	if verify == nil {
		return out, failure(Unavailable, "unavailable")
	}
	if err := verify(ctx, in.Delivery.URL, id, in.secret); err != nil {
		var callbackErr *Error
		if errors.As(err, &callbackErr) && (callbackErr.Code == Unavailable || callbackErr.Code == Forbidden) {
			return out, callbackErr
		}
		reason := "challenge_failed"
		if errors.As(err, &callbackErr) {
			reason = CallbackReason(callbackErr.Reason)
		}
		return out, failure(CallbackEndpoint, reason)
	}

	challengedID, challengedRefresh := id, refresh
	for {
		if err := s.purgeExpired(ctx); err != nil {
			return out, failure(Unavailable, "store_unavailable")
		}
		entries, raw, err := s.catalog(ctx)
		if err != nil {
			return out, failure(Unavailable, "store_unavailable")
		}
		index := find(entries, owner, in.Arguments, in.Delivery.URL)
		if isGrantOwner(owner) && !s.liveGrant(owner) {
			return out, failure(Forbidden, "permission_denied")
		}
		refresh = index >= 0
		if owner != "owner" && ((challengedRefresh && !refresh) || (refresh && entries[index].ID != challengedID)) {
			return out, failure(Unavailable, "verification_changed")
		}
		if refresh {
			if len(entries)+s.ConfigTargets > 32 {
				return out, failure(ResourceExhausted, "subscriptions")
			}
			id = entries[index].ID
			_, position, err := s.Store.LoadWakeupStateKey(ctx, store.MCPEventsPositionKey(id))
			if err != nil || position == nil {
				return out, failure(Unavailable, "store_unavailable")
			}
		}
		if !refresh && creationCheck != nil {
			if err := creationCheck(ctx); err != nil {
				return out, err
			}
		}
		var initial *store.WakeupState
		if !refresh {
			if s.subscriptionLimit(entries, owner) {
				return out, failure(ResourceExhausted, "subscriptions")
			}
			page, err := s.Store.ListChangedMessages(ctx, store.ChangedMessagesCursor{}, 1)
			if err != nil || page.CompleteThrough.IsZero() {
				return out, failure(Unavailable, "head_unavailable")
			}
			initial = &store.WakeupState{At: page.CompleteThrough}
		}
		origin, err := callbackOrigin(in.Delivery.URL)
		if err != nil {
			return out, failure(InvalidParams, "invalid_callback")
		}
		previous, revision, credentialErr := s.credentialSnapshot(id)
		if credentialErr != nil {
			return out, failure(Unavailable, "credential_unavailable")
		}
		expires := s.now().Add(in.TTL.Duration())
		if grantID, ok := agentgrant.OwnerID(owner); ok {
			grant, live := s.Grants.LookupID(grantID, agentgrant.PermissionSearchRead)
			if !live {
				return out, failure(Forbidden, "permission_denied")
			}
			if !grant.ExpiresAt.IsZero() && grant.ExpiresAt.Before(expires) {
				expires = grant.ExpiresAt
			}
		}
		if refresh && !s.now().Before(entries[index].Expires) {
			continue
		}
		private, err := json.Marshal(privateCallback{URL: in.Delivery.URL, Secret: base64.StdEncoding.EncodeToString(in.secret)})
		if err != nil {
			return out, failure(Unavailable, "credential_unavailable")
		}
		item := entry{ID: id, Owner: owner, Arguments: in.Arguments, Origin: origin, Expires: expires}
		if refresh {
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
		_, err = providercredentials.PutIfRevision(s.TokensDir, revision, providercredentials.EventsID(id), origin, string(private))
		if err != nil {
			if !s.rollbackSubscribe(ctx, entries, raw, next, initial != nil) {
				return out, failure(Unavailable, "store_unavailable")
			}
			return out, failure(Unavailable, "credential_unavailable")
		}
		status := MCPEventsDeliveryStatus{Active: true}
		state, _, loadErr := s.Store.LoadWakeupStateKey(ctx, store.MCPEventsPositionKey(id))
		if loadErr != nil {
			return out, failure(Unavailable, "store_unavailable")
		}
		status.LastDeliveryAt = state.LastDeliveryAt
		status.LastError = state.LastError
		status.FailedSince = state.FailedSince
		if index >= 0 && !previous.Stored(providercredentials.EventsID(id)) {
			status.LastError = "secret_unavailable"
			status.FailedSince = nil
		}
		return MCPEventsSubscribeResult{ID: id, RefreshBefore: expires, DeliveryStatus: status}, nil
	}
}

func (s *Service) subscriptionLimit(entries []entry, owner string) bool {
	active, owned := len(entries)+s.ConfigTargets, 0
	for _, e := range entries {
		if s.now().Before(e.Expires) {
			if e.Owner == owner {
				owned++
			}
		}
	}
	return active >= 32 || (owner != "owner" && owned >= 4)
}

// rollbackSubscribe undoes the catalog write after the credential write failed:
// a new entry is removed, and a refresh gets the catalog bytes it replaced. Both
// are one compare-and-swap on the bytes this call published, so a catalog changed
// meanwhile is left alone. It reports whether the swap took effect.
func (s *Service) rollbackSubscribe(ctx context.Context, entries []entry, raw, next []byte, created bool) bool {
	var ok bool
	var err error
	if created {
		ok, err = s.removeEntry(ctx, entries, next, len(entries)-1)
	} else {
		ok, err = s.Store.SwapMCPEventsCatalog(ctx, next, raw, "", nil, "")
	}
	return err == nil && ok
}

// Unsubscribe requires a request returned by DecodeUnsubscribe.
func (s *Service) Unsubscribe(ctx context.Context, in MCPEventsUnsubscribeRequest, owner string) error {
	if s.Store == nil || owner == "" {
		return failure(Unavailable, "store_unavailable")
	}
	for {
		entries, raw, err := s.catalog(ctx)
		if err != nil {
			return failure(Unavailable, "store_unavailable")
		}
		idx := find(entries, owner, in.Arguments, in.Delivery.URL)
		if idx < 0 {
			return failure(NotFound, "subscription")
		}
		ok, err := s.removeEntryAndCredential(ctx, entries, raw, idx)
		if err != nil {
			return failure(Unavailable, "store_unavailable")
		}
		if ok {
			return nil
		}
	}
}

func (s *Service) removeEntry(ctx context.Context, entries []entry, raw []byte, index int) (bool, error) {
	id := entries[index].ID
	entries = append(entries[:index:index], entries[index+1:]...)
	next, err := json.Marshal(entries)
	if err != nil {
		return false, err
	}
	return s.Store.SwapMCPEventsCatalog(ctx, raw, next, "", nil, id)
}

func (s *Service) removeEntryAndCredential(ctx context.Context, entries []entry, raw []byte, index int) (bool, error) {
	id := entries[index].ID
	ok, err := s.removeEntry(ctx, entries, raw, index)
	if err != nil || !ok {
		return ok, err
	}
	if credentialErr := s.deleteCredential(id); credentialErr != nil {
		logger := s.logger()
		logger.Warn("wakeup credential cleanup failed", "id", id, "category", "credential_unavailable")
	}
	return ok, err
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// purgeExpired removes expired or revoked entries, credentials and positions.
func (s *Service) purgeExpired(ctx context.Context) error {
	for {
		entries, raw, err := s.catalog(ctx)
		if err != nil {
			return err
		}
		index := -1
		for i, e := range entries {
			if !s.now().Before(e.Expires) || (isGrantOwner(e.Owner) && !s.liveGrant(e.Owner)) {
				index = i
				break
			}
		}
		if index < 0 {
			return nil
		}
		if _, err = s.removeEntryAndCredential(ctx, entries, raw, index); err != nil {
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
	credentials, err := providercredentials.ReadEvents(s.TokensDir)
	if err != nil {
		return nil, err
	}
	var out []wakeups.Subscription
	logger := s.logger()
	for _, e := range entries {
		if e.Owner != "owner" && !isGrantOwner(e.Owner) {
			continue
		}
		if len(out) >= max(0, 32-s.ConfigTargets) {
			logger.Warn("native wakeup unavailable", "id", e.ID, "category", "over_limit")
			continue
		}
		if e.Arguments.IncomingOnly == nil {
			return nil, errors.New("invalid events catalog")
		}
		encoded, _, err := credentials.Resolve(providercredentials.EventsID(e.ID), e.Origin, "", nil)
		var private privateCallback
		decodeErr := json.Unmarshal([]byte(encoded), &private, json.RejectUnknownMembers(true))
		secret, secretErr := base64.StdEncoding.DecodeString(private.Secret)
		origin, originErr := callbackOrigin(private.URL)
		if err != nil || decodeErr != nil || secretErr != nil || len(secret) == 0 || originErr != nil || origin != e.Origin || config.ValidateWakeupDestination(private.URL) != nil || find([]entry{e}, e.Owner, e.Arguments, private.URL) < 0 {
			logger.Debug("native wakeup unavailable", "id", e.ID, "category", "secret_unavailable")
			continue
		}
		f := e.Arguments.filter()
		sub := wakeups.NewSubscription(e.ID, private.URL, secret, f, 72*time.Hour, 10*time.Second, nil, nil)
		sub.Native, sub.Expires = true, e.Expires
		if id, ok := agentgrant.OwnerID(e.Owner); ok {
			sub.GrantID, sub.Grants = id, s.Grants
		}
		sub.StateKey = store.MCPEventsPositionKey(e.ID)
		out = append(out, sub)
	}
	return out, nil
}

func (s *Service) liveGrant(owner string) bool {
	id, ok := agentgrant.OwnerID(owner)
	if !ok {
		return true
	}
	_, live := s.Grants.LookupID(id, agentgrant.PermissionSearchRead)
	return live
}

func isGrantOwner(owner string) bool { _, ok := agentgrant.OwnerID(owner); return ok }
