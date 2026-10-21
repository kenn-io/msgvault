// Package mcpevents delivers scoped, durable archive occurrences to owner callbacks.
package mcpevents

import (
	"context"
	"crypto/tls"
	"encoding/json/jsontext"
	"net"
	"net/netip"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type Options struct {
	// Now supplies evaluation and webhook-signature time. Nil uses the wall
	// clock. It is an internal dependency, not configuration or wire input;
	// wakeups and cancellation still use real timers.
	Now               func() time.Time
	Enabled           bool
	Retention         time.Duration
	Sources           []string
	TrustedCallbacks  []TrustedCallback
	KeyPath, OwnerKey string
	// WithOperation takes the daemon's archive gate for a short Store operation.
	// Callback resolution, verification and delivery never run in this callback.
	WithOperation func(context.Context, func() error) error
	// WithDeliveryOperation takes the request-aware gate for Store operations
	// directly triggered by a committed event. Idle reconciliation keeps using
	// WithOperation so it does not preempt scheduled work.
	WithDeliveryOperation func(context.Context, func() error) error
	// These optional network dependencies support controlled receivers without
	// bypassing destination checks, pinned dialing or TLS hostname verification.
	LookupIP    func(context.Context, string) ([]netip.Addr, error)
	DialContext func(context.Context, string, string) (net.Conn, error)
	TLSConfig   *tls.Config
}
type TrustedCallback struct {
	Origin    string
	Addresses []string
}
type ListResult struct {
	Events []Definition `json:"events"`
}
type Definition struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Delivery      []string       `json:"delivery"`
	InputSchema   map[string]any `json:"inputSchema"`
	PayloadSchema map[string]any `json:"payloadSchema"`
}
type SubscribeRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Delivery  Delivery       `json:"delivery"`
	Cursor    *string        `json:"cursor,omitempty"`
	TTLMS     *int64         `json:"ttlMs,omitempty"`
	MaxAgeMS  *int64         `json:"maxAgeMs,omitempty"`
}
type Delivery struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}
type SubscribeResult struct {
	ID            string `json:"id"`
	RefreshBefore int64  `json:"refreshBefore"`
	Cursor        string `json:"cursor"`
	Truncated     bool   `json:"truncated"`
}
type UnsubscribeRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Delivery  Delivery       `json:"delivery"`
}
type Envelope struct {
	EventID   string         `json:"eventId"`
	Name      string         `json:"name"`
	Timestamp string         `json:"timestamp"`
	Data      jsontext.Value `json:"data"`
	Cursor    string         `json:"cursor"`
}
type CalendarSource struct {
	SourceID string `json:"source_id"`
	Summary  string `json:"summary"`
	Account  string `json:"account"`
}

// SubscriptionStatus intentionally excludes callback destinations and encrypted secrets.
type SubscriptionStatus struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ScopeKind       string `json:"scope_kind"`
	ScopeID         string `json:"scope_id"`
	State           string `json:"state"`
	StopReason      string `json:"stop_reason,omitempty"`
	RefreshBefore   int64  `json:"refreshBefore"`
	Cursor          string `json:"cursor"`
	CursorEpoch     int64  `json:"cursor_epoch"`
	CursorSeq       int64  `json:"cursor_seq"`
	PendingAttempt  int    `json:"pending_attempt"`
	LastOutcome     string `json:"last_delivery_outcome"`
	DeadLetterCount int    `json:"dead_letter_count"`
	LoopGuardSkips  int    `json:"loop_guard_skips"`
}

// Protocol error codes. The SEP-3415 codes are provisional numbers; the two
// implementation codes sit in the range the SEP leaves to implementations.
const (
	CodeInvalidParams         = -32602
	CodeNotFound              = -32023
	CodeForbidden             = -32024
	CodeResourceExhausted     = -32025
	CodeUnsupported           = -32026
	CodeCallbackEndpointError = -32027
	CodeConflict              = -32013 // implementation-defined
	CodeUnavailable           = -32015 // implementation-defined

	// MaxActiveSubscriptions is the per-owner active subscription quota that
	// the Store enforces.
	MaxActiveSubscriptions = store.MCPActiveSubscriptionLimit
)

type Error struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string     { return "mcp events: " + e.Reason }
func invalid(reason string) *Error { return &Error{Code: CodeInvalidParams, Reason: reason} }

// WireData is the JSON-RPC error data: the reason plus the structured fields
// the SEP defines for the condition. It accepts any reason so callers can
// describe conditions outside the SafeError allowlist.
func (e *Error) WireData() map[string]any {
	data := map[string]any{"reason": e.Reason}
	switch {
	case e.Code == CodeNotFound && e.Reason == "unknown_event":
		data["kind"] = "event"
	case e.Code == CodeNotFound && e.Reason == "subscription_not_found":
		data["kind"] = "subscription"
	case e.Code == CodeResourceExhausted && e.Reason == "subscription_limit":
		data["limit"] = "subscriptions"
		data["max"] = MaxActiveSubscriptions
	case e.Code == CodeUnsupported && e.Reason == "unsupported_delivery":
		data["feature"] = "deliveryMode"
	}
	return data
}

// WireMessage is the SEP error name for SEP codes and a fixed generic message
// otherwise, so no diagnostic text reaches the protocol.
func (e *Error) WireMessage() string {
	switch e.Code {
	case CodeNotFound:
		return "NotFound"
	case CodeForbidden:
		return "Forbidden"
	case CodeResourceExhausted:
		return "ResourceExhausted"
	case CodeUnsupported:
		return "Unsupported"
	case CodeCallbackEndpointError:
		return "CallbackEndpointError"
	}
	return "event request failed"
}

// SafeError validates an untrusted protocol error before its reason is exposed.
// Codes and reasons are paired so arbitrary daemon text cannot become RPC data.
func SafeError(code int, reason string) (*Error, bool) {
	var allowed bool
	switch code {
	case CodeInvalidParams:
		switch reason {
		case "invalid_resource_reference", "invalid_resource_query", "invalid_discovery_cursor", "resource_unavailable", "invalid_request", "invalid_arguments", "unknown_argument", "invalid_scope", "invalid_identifier", "invalid_boolean", "invalid_kinds", "invalid_cursor", "invalid_event_id", "invalid_secret", "invalid_callback", "invalid_trusted_callback", "invalid_ttl", "invalid_retention", "invalid_subscription", "unknown_scope", "wrong_scope_type", "source_not_capable", "kind_not_capable", "event_unavailable", "message_unavailable", "payload_too_large":
			allowed = true
		}
	case CodeNotFound:
		allowed = reason == "unknown_event" || reason == "subscription_not_found"
	case CodeForbidden:
		switch reason {
		case "native_authority_unavailable", "owner_required", "subscription_unavailable", "principal_revoked", "subscription_inactive":
			allowed = true
		}
	case CodeResourceExhausted:
		allowed = reason == "subscription_limit"
	case CodeConflict:
		allowed = reason == "concurrent_update" || reason == "events_already_running"
	case CodeUnsupported:
		allowed = reason == "unsupported_delivery"
	case CodeCallbackEndpointError:
		switch reason {
		case "connection_refused", "timeout", "tls_error", "http_4xx", "http_5xx", "challenge_failed":
			allowed = true
		}
	case CodeUnavailable:
		switch reason {
		case "events_storage_unavailable", "events_key_unavailable", "events_unavailable":
			allowed = true
		}
	}
	if !allowed {
		return nil, false
	}
	return &Error{Code: code, Reason: reason}, true
}
