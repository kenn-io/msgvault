package mcpevents

import (
	"context"
	"crypto/hmac"
	"crypto/tls"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type Service struct {
	st            *store.Store
	opts          Options
	principal     string
	key           []byte
	caps          []store.MCPEventCapability
	webhook       *webhookClient
	wake          chan struct{}
	deliveryHints chan struct{}
	workersMu     sync.Mutex
	workers       map[string]*worker
	running       bool
}

// StoreCaptureConfig returns the Store runtime needed by ingesting processes
// that capture Events without running a delivery service.
func StoreCaptureConfig(opts Options) store.MCPEventsConfig {
	retention := opts.Retention
	if retention == 0 {
		retention = 7 * 24 * time.Hour
	}
	return store.MCPEventsConfig{
		Enabled:      opts.Enabled,
		Principal:    Principal(opts.OwnerKey),
		Capabilities: capabilities(opts.Enabled, opts.Sources),
		Retention:    retention,
	}
}

func New(ctx context.Context, st *store.Store, opts Options) (*Service, error) {
	if st == nil || opts.Enabled && opts.OwnerKey == "" {
		return nil, &Error{Code: -32012, Reason: "owner_required"}
	}
	if opts.Retention == 0 {
		opts.Retention = 7 * 24 * time.Hour
	}
	if opts.Enabled && (opts.Retention <= 0 || opts.Retention > 7*24*time.Hour) {
		return nil, invalid("invalid_retention")
	}
	s := &Service{st: st, opts: opts, principal: Principal(opts.OwnerKey), caps: capabilities(opts.Enabled, opts.Sources), wake: make(chan struct{}, 1), deliveryHints: make(chan struct{}, 1), workers: make(map[string]*worker)}
	if opts.Enabled {
		var rows []store.MCPSubscription
		err := s.operation(ctx, func() error { var err error; rows, err = st.ListMCPSubscriptionsForStartup(ctx); return err })
		if err != nil {
			return nil, safeStoreError(err)
		}
		s.key, err = loadKey(opts.KeyPath, rows)
		if err != nil {
			return nil, err
		}
		s.webhook, err = newWebhookClient(opts.TrustedCallbacks)
		if err != nil {
			return nil, err
		}
		if opts.LookupIP != nil {
			s.webhook.resolve = func(ctx context.Context, _ string, host string) ([]netip.Addr, error) {
				return opts.LookupIP(ctx, host)
			}
		}
		if opts.DialContext != nil {
			s.webhook.dial = opts.DialContext
		}
		if opts.TLSConfig != nil {
			if opts.TLSConfig.InsecureSkipVerify || opts.TLSConfig.ServerName != "" {
				return nil, invalid("invalid_callback")
			}
			s.webhook.transport.TLSClientConfig = opts.TLSConfig.Clone()
			if s.webhook.transport.TLSClientConfig.MinVersion == 0 {
				s.webhook.transport.TLSClientConfig.MinVersion = tls.VersionTLS12
			}
		}
	}
	err := s.operation(ctx, func() error {
		_, err := st.ConfigureMCPEvents(ctx, StoreCaptureConfig(opts))
		return err
	})
	if err != nil {
		return nil, safeStoreError(err)
	}
	if opts.Enabled {
		if err := s.cleanupRetention(ctx, time.Now().UTC()); err != nil {
			return nil, safeStoreError(err)
		}
	}
	st.SetMCPEventsWake(s.wakeAfterCommit)
	return s, nil
}
func (s *Service) operation(ctx context.Context, fn func() error) error {
	if s.opts.WithOperation != nil {
		return s.opts.WithOperation(ctx, fn)
	}
	return fn()
}

func (s *Service) deliveryOperation(ctx context.Context, fn func() error) error {
	if s.opts.WithDeliveryOperation != nil {
		return s.opts.WithDeliveryOperation(ctx, fn)
	}
	return s.operation(ctx, fn)
}

func (s *Service) cleanupRetention(ctx context.Context, now time.Time) error {
	if err := s.operation(ctx, func() error { return s.st.ExpireMCPSubscriptions(ctx, now) }); err != nil {
		return err
	}
	return s.operation(ctx, func() error { return s.st.PruneMCPEvents(ctx, now, s.opts.Retention) })
}
func (s *Service) authorize(principal string) error {
	if !s.opts.Enabled || principal != s.principal {
		return &Error{Code: -32012, Reason: "subscription_unavailable"}
	}
	return nil
}
func safeStoreError(err error) error {
	if err == nil {
		return nil
	}
	switch err.Error() {
	case "unknown_scope", "wrong_scope_type", "source_not_capable", "kind_not_capable", "invalid_cursor", "invalid_subscription", "event_unavailable", "message_unavailable":
		return invalid(err.Error())
	case "subscription_limit", "concurrent_update":
		return &Error{Code: -32013, Reason: err.Error()}
	case "subscription_unavailable", "principal_revoked", "subscription_inactive":
		return &Error{Code: -32012, Reason: "subscription_unavailable"}
	default:
		return &Error{Code: -32015, Reason: "events_storage_unavailable"}
	}
}
func (s *Service) Subscribe(ctx context.Context, principal string, req SubscribeRequest) (SubscribeResult, error) {
	var result SubscribeResult
	args, err := canonicalArguments(req.Name, req.Arguments)
	if err != nil {
		return result, err
	}
	if req.Delivery.Mode != "webhook" {
		return result, &Error{Code: -32014, Reason: "unsupported_delivery"}
	}
	if req.TTLMS != nil && (*req.TTLMS <= 0 || *req.TTLMS > int64(24*time.Hour/time.Millisecond)) {
		return result, invalid("invalid_ttl")
	}
	secret, err := decodeSecret(req.Delivery.Secret)
	if err != nil {
		return result, err
	}
	if err := s.authorize(principal); err != nil {
		return result, err
	}
	if _, err := callbackURL(req.Delivery.URL); err != nil {
		return result, err
	}
	id := subscriptionID(principal, req.Name, args.bytes, req.Delivery.URL)
	var old *store.MCPSubscription
	activation := store.MCPActivation{Subscription: store.MCPSubscription{ID: id, Principal: principal, Name: req.Name, Arguments: args.bytes, ScopeKind: args.scopeKind, ScopeID: args.scopeID, CallbackURL: req.Delivery.URL, SecretRevision: 1, VerifiedRevision: 1}}
	err = s.operation(ctx, func() error {
		if err := s.st.BindMCPSubscriptionScope(ctx, &activation.Subscription); err != nil {
			return err
		}
		old, err = s.st.GetMCPSubscription(ctx, id)
		return err
	})
	if err != nil {
		return result, safeStoreError(err)
	}
	var oldSecret []byte
	if old != nil {
		activation.ExpectedGeneration = old.Generation
		activation.ExpectedState = old.State
		activation.ExpectedSecretRevision = old.SecretRevision
		if old.StopReason == "principal_revoked" {
			return result, &Error{Code: -32012, Reason: "subscription_unavailable"}
		}
		if old.StopReason == "scope_removed" {
			return result, invalid("unknown_scope")
		}
		oldSecret, err = decryptSecret(s.key, id, "current", old.SecretEnc)
		if err != nil {
			return result, err
		}
		activation.Subscription.SecretRevision = old.SecretRevision
		if !hmac.Equal(secret, oldSecret) {
			activation.Subscription.SecretRevision++
		}
		activation.Subscription.VerifiedRevision = activation.Subscription.SecretRevision
	}
	if req.Cursor != nil && (old == nil || old.State != "stopped" || (old.StopReason != "retention" && old.StopReason != "capture_gap")) {
		activation.Replay = true
		activation.ReplayEpoch, activation.ReplaySeq, err = decodeCursor(s.key, id, *req.Cursor)
		if err != nil {
			return result, err
		}
	}
	verified := old != nil && hmac.Equal(secret, oldSecret) && old.VerifiedRevision == old.SecretRevision && old.State != "unsubscribed" && old.State != "gone"
	check := func(ctx context.Context) error {
		if err := s.authorize(principal); err != nil {
			return err
		}
		return s.operation(ctx, func() error {
			_, _, err := s.st.ValidateMCPEventScope(ctx, req.Name, args.scopeKind, args.scopeID, requestedKinds(args.kinds))
			return safeStoreError(err)
		})
	}
	if !verified {
		if err := s.webhook.verify(ctx, req.Delivery.URL, id, secret, check); err != nil {
			return result, err
		}
	}
	now := time.Now().UTC()
	ttl := 24 * time.Hour
	if req.TTLMS != nil {
		ttl = time.Duration(*req.TTLMS) * time.Millisecond
	}
	activation.Now = now
	activation.Subscription.ExpiresAt = now.Add(ttl)
	activation.Subscription.VerifiedAt = now
	activation.Subscription.SecretEnc, err = encryptSecret(s.key, id, "current", secret)
	if err != nil {
		return result, err
	}
	if old != nil {
		if !hmac.Equal(secret, oldSecret) && old.State == "active" {
			activation.Subscription.PreviousSecretEnc, err = encryptSecret(s.key, id, "previous", oldSecret)
			if err != nil {
				return result, err
			}
			activation.Subscription.PreviousSecretUntil = now.Add(time.Minute)
		} else if hmac.Equal(secret, oldSecret) {
			activation.Subscription.PreviousSecretEnc = old.PreviousSecretEnc
			activation.Subscription.PreviousSecretUntil = old.PreviousSecretUntil
		}
	}
	var sub *store.MCPSubscription
	var truncated bool
	err = s.operation(ctx, func() error { sub, truncated, err = s.st.ActivateMCPSubscription(ctx, activation); return err })
	if err != nil {
		return result, safeStoreError(err)
	}
	s.stopWorker(id, sub.Generation)
	s.Wake()
	return SubscribeResult{ID: id, RefreshBefore: sub.ExpiresAt.UnixMilli(), Cursor: encodeCursor(s.key, id, sub.CursorEpoch, sub.CursorSeq), Truncated: truncated}, nil
}
func requestedKinds(kinds []string) []string {
	if len(kinds) == 1 && kinds[0] == "*" {
		return nil
	}
	return kinds
}
func (s *Service) Unsubscribe(ctx context.Context, principal string, req UnsubscribeRequest) error {
	args, err := canonicalArguments(req.Name, req.Arguments)
	if err != nil {
		return err
	}
	if req.Delivery.Mode != "webhook" {
		return &Error{Code: -32014, Reason: "unsupported_delivery"}
	}
	if _, err := callbackURL(req.Delivery.URL); err != nil {
		return err
	}
	if err := s.authorize(principal); err != nil {
		return err
	}
	id := subscriptionID(principal, req.Name, args.bytes, req.Delivery.URL)
	err = s.operation(ctx, func() error { return s.st.EndMCPSubscription(ctx, id, principal, time.Now().UTC()) })
	if err != nil {
		return safeStoreError(err)
	}
	s.stopWorker(id, 0)
	s.Wake()
	return nil
}
func (s *Service) envelope(sub store.MCPSubscription, event store.MCPEvent) Envelope {
	return Envelope{EventID: encodeEventID(s.key, sub.ID, event.Seq), Name: event.Family, Timestamp: event.OccurredAt.UTC().Format(time.RFC3339Nano), Data: jsontext.Value(event.Data), Cursor: encodeCursor(s.key, sub.ID, event.Epoch, event.Seq)}
}
func (s *Service) GetEvent(ctx context.Context, principal, eventID string) (Envelope, error) {
	if err := s.authorize(principal); err != nil {
		return Envelope{}, err
	}
	id, seq, err := decodeEventID(s.key, eventID)
	if err != nil {
		return Envelope{}, err
	}
	var event store.MCPEvent
	err = s.operation(ctx, func() error { event, err = s.st.GetMCPEvent(ctx, id, seq, principal, time.Now().UTC()); return err })
	if err != nil {
		return Envelope{}, safeStoreError(err)
	}
	return s.envelope(store.MCPSubscription{ID: id}, event), nil
}
func (s *Service) GetMessage(ctx context.Context, principal, eventID string, messageID int64) (*query.MessageDetail, error) {
	if err := s.authorize(principal); err != nil {
		return nil, err
	}
	id, seq, err := decodeEventID(s.key, eventID)
	if err != nil {
		return nil, err
	}
	var detail *query.MessageDetail
	err = s.operation(ctx, func() error {
		return s.st.ReadMCPEventMessage(ctx, id, seq, principal, messageID, time.Now().UTC(), func(tx *sql.Tx) error {
			detail, err = query.GetMessageInSnapshot(ctx, tx, s.st.Rebind, messageID)
			return err
		})
	})
	if err != nil {
		return nil, safeStoreError(err)
	}
	return detail, nil
}
func (s *Service) CalendarSources(ctx context.Context, principal string) ([]CalendarSource, error) {
	if err := s.authorize(principal); err != nil {
		return nil, err
	}
	result := make([]CalendarSource, 0)
	err := s.operation(ctx, func() error {
		rows, err := s.st.ListSourcesContext(ctx, "")
		if err != nil {
			return err
		}
		for _, row := range rows {
			_, _, err := s.st.ValidateMCPEventScope(ctx, calendarFamily, "source", row.ID, nil)
			if err != nil {
				switch err.Error() {
				case "source_not_capable", "wrong_scope_type", "unknown_scope":
					continue
				default:
					return err
				}
			}
			var config struct {
				Account string `json:"account_email"`
			}
			if row.SyncConfig.Valid {
				if err := json.Unmarshal([]byte(row.SyncConfig.String), &config); err != nil {
					return errors.New("events_storage_unavailable")
				}
			}
			result = append(result, CalendarSource{SourceID: strconv.FormatInt(row.ID, 10), Summary: row.DisplayName.String, Account: config.Account})
		}
		return nil
	})
	if err != nil {
		return nil, safeStoreError(err)
	}
	return result, nil
}
func (s *Service) Status(ctx context.Context, principal string) ([]SubscriptionStatus, error) {
	if err := s.authorize(principal); err != nil {
		return nil, err
	}
	var rows []store.MCPSubscription
	err := s.operation(ctx, func() error { var err error; rows, err = s.st.ListMCPSubscriptions(ctx, principal); return err })
	if err != nil {
		return nil, safeStoreError(err)
	}
	result := make([]SubscriptionStatus, 0, len(rows))
	for _, row := range rows {
		result = append(result, SubscriptionStatus{ID: row.ID, Name: row.Name, ScopeKind: row.ScopeKind, ScopeID: strconv.FormatInt(row.ScopeID, 10), State: row.State, StopReason: row.StopReason, RefreshBefore: row.ExpiresAt.UnixMilli(), Cursor: encodeCursor(s.key, row.ID, row.CursorEpoch, row.CursorSeq), CursorEpoch: row.CursorEpoch, CursorSeq: row.CursorSeq, PendingAttempt: row.AttemptCount, LastOutcome: row.LastOutcome, DeadLetterCount: row.DeadLetterCount, LoopGuardSkips: row.LoopGuardSkips})
	}
	return result, nil
}
