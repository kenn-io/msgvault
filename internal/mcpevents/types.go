// Package mcpevents delivers scoped, durable archive occurrences to owner callbacks.
package mcpevents

import (
	"context"
	"crypto/tls"
	"encoding/json/jsontext"
	"net"
	"net/netip"
	"time"
)

type Options struct {
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
type Error struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string     { return "mcp events: " + e.Reason }
func invalid(reason string) *Error { return &Error{Code: -32602, Reason: reason} }

// SafeError validates an untrusted protocol error before its reason is exposed.
// Codes and reasons are paired so arbitrary daemon text cannot become RPC data.
func SafeError(code int, reason string) (*Error, bool) {
	var allowed bool
	switch code {
	case -32602:
		switch reason {
		case "invalid_request", "invalid_arguments", "unknown_argument", "unknown_event", "invalid_scope", "invalid_identifier", "invalid_boolean", "invalid_kinds", "invalid_cursor", "invalid_event_id", "invalid_secret", "invalid_callback", "invalid_trusted_callback", "invalid_ttl", "invalid_retention", "invalid_subscription", "unknown_scope", "wrong_scope_type", "source_not_capable", "kind_not_capable", "event_unavailable", "message_unavailable", "payload_too_large":
			allowed = true
		}
	case -32012:
		switch reason {
		case "owner_required", "subscription_unavailable", "principal_revoked", "subscription_inactive":
			allowed = true
		}
	case -32013:
		switch reason {
		case "subscription_limit", "concurrent_update", "events_already_running":
			allowed = true
		}
	case -32014:
		allowed = reason == "unsupported_delivery"
	case -32015:
		switch reason {
		case "connection_refused", "timeout", "tls_error", "http_4xx", "http_5xx", "challenge_failed", "events_storage_unavailable", "events_key_unavailable", "events_unavailable":
			allowed = true
		}
	}
	if !allowed {
		return nil, false
	}
	return &Error{Code: code, Reason: reason}, true
}
