package mcpevents

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/msgvault/internal/changefeed"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/wakeups"
)

const Name = "msgvault.messages.changed"
const (
	InvalidParams     = -32602
	NotFound          = -32023
	Forbidden         = -32024
	ResourceExhausted = -32025
	Unsupported       = -32026
	CallbackEndpoint  = -32027
	Unavailable       = -32010
)

type Error struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string { return "MCP Events: " + e.Reason }
func (e *Error) Status() int {
	switch e.Code {
	case InvalidParams, Unsupported:
		return http.StatusBadRequest
	case CallbackEndpoint:
		return http.StatusBadGateway
	case Forbidden:
		return http.StatusForbidden
	case NotFound:
		return http.StatusNotFound
	case ResourceExhausted:
		return http.StatusTooManyRequests
	case CallbackEndpoint:
		return http.StatusBadGateway
	default:
		return http.StatusServiceUnavailable
	}
}

// CallbackReason limits diagnostics to transport-owned categories.
func CallbackReason(reason string) string {
	switch reason {
	case "challenge_failed", "timeout", "dns_error", "invalid_callback", "connection_refused", "tls_error", "http_3xx", "http_4xx", "http_5xx":
		return reason
	default:
		return "challenge_failed"
	}
}
func failure(code int, reason string) error { return &Error{code, reason} }

type MCPEventsArguments struct {
	PersonID     int64 `json:"person_id,omitzero" required:"false"`
	SourceID     int64 `json:"source_id,omitzero" required:"false"`
	IncomingOnly *bool `json:"incoming_only,omitempty"`
}

func (a MCPEventsArguments) filter() changefeed.Filter {
	f := changefeed.Filter{PersonID: a.PersonID, IncomingOnly: *a.IncomingOnly}
	if a.SourceID != 0 {
		f.SourceIDs = []int64{a.SourceID}
	}
	return f
}

type MCPEventsDelivery struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}
type TTL struct {
	set, null bool
	ms        int64
}

func (*TTL) Schema(_ huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeInteger, Nullable: true}
}

func (t *TTL) UnmarshalJSON(b []byte) error {
	t.set = true
	if string(b) == "null" {
		t.null = true
		return nil
	}
	return json.Unmarshal(b, &t.ms)
}
func (t *TTL) MarshalJSON() ([]byte, error) {
	if !t.set || t.null {
		return []byte("null"), nil
	}
	return json.Marshal(t.ms)
}
func (t *TTL) IsZero() bool { return !t.set }
func (t *TTL) Duration() time.Duration {
	if !t.set {
		return 24 * time.Hour
	}
	if t.null {
		return 30 * 24 * time.Hour
	}
	return time.Duration(max(int64(60000), min(t.ms, int64((30*24*time.Hour)/time.Millisecond)))) * time.Millisecond
}

type MCPEventsSubscribeRequest struct {
	Name      string             `json:"name"`
	Arguments MCPEventsArguments `json:"arguments"`
	Delivery  MCPEventsDelivery  `json:"delivery"`
	TTL       TTL                `json:"ttlMs,omitzero" required:"false"`
	Cursor    *string            `json:"cursor,omitempty"`
	MaxAgeMS  *int64             `json:"maxAgeMs,omitempty"`
	secret    []byte
}
type MCPEventsUnsubscribeRequest struct {
	Name      string             `json:"name"`
	Arguments MCPEventsArguments `json:"arguments"`
	Delivery  MCPEventsDelivery  `json:"delivery"`
}
type MCPEventsUnsubscribeResult struct{}
type MCPEventsDeliveryStatus struct {
	Active         bool       `json:"active"`
	LastDeliveryAt *time.Time `json:"lastDeliveryAt,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
	FailedSince    *time.Time `json:"failedSince,omitempty"`
}
type MCPEventsSubscribeResult struct {
	ID             string                  `json:"id"`
	RefreshBefore  time.Time               `json:"refreshBefore"`
	Cursor         *string                 `json:"cursor"`
	Truncated      bool                    `json:"truncated"`
	DeliveryStatus MCPEventsDeliveryStatus `json:"deliveryStatus"`
}
type MCPEventsDefinition struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Delivery      []string       `json:"delivery"`
	InputSchema   map[string]any `json:"inputSchema"`
	PayloadSchema map[string]any `json:"payloadSchema"`
}
type MCPEventsListResult struct {
	Enabled bool                  `json:"enabled"`
	Events  []MCPEventsDefinition `json:"events"`
}

func Definitions() []MCPEventsDefinition {
	return []MCPEventsDefinition{{Name: Name, Description: "Messages for this person or source may have changed. Read list_changes to catch up.", Delivery: []string{"webhook"}, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"person_id": map[string]any{"type": "integer", "minimum": 1}, "source_id": map[string]any{"type": "integer", "minimum": 1}, "incoming_only": map[string]any{"type": "boolean", "default": true}}, "oneOf": []any{map[string]any{"required": []string{"person_id"}}, map[string]any{"required": []string{"source_id"}}}, "additionalProperties": false}, PayloadSchema: map[string]any{"type": "object", "properties": map[string]any{"subscription_id": map[string]any{"type": "string"}}, "required": []string{"subscription_id"}}}}
}
func decode(raw []byte, out any) error {
	if len(raw) > 64<<10 {
		return failure(InvalidParams, "invalid_params")
	}
	var obj map[string]jsontext.Value
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return failure(InvalidParams, "invalid_params")
	}
	if string(obj["name"]) == "null" {
		return failure(InvalidParams, "invalid_params")
	}
	for _, key := range []string{"arguments", "delivery"} {
		if v, ok := obj[key]; ok {
			var fields map[string]jsontext.Value
			if json.Unmarshal(v, &fields) != nil || fields == nil {
				return failure(InvalidParams, "invalid_params")
			}
			for _, field := range fields {
				if string(field) == "null" {
					return failure(InvalidParams, "invalid_params")
				}
			}
			if key == "arguments" {
				for _, selector := range []string{"person_id", "source_id"} {
					if raw, present := fields[selector]; present {
						var id int64
						if json.Unmarshal(raw, &id) == nil && id <= 0 {
							return failure(InvalidParams, "invalid_arguments")
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(raw, out, json.RejectUnknownMembers(true)); err != nil {
		return failure(InvalidParams, "invalid_params")
	}
	return nil
}
func validate(name string, args *MCPEventsArguments, delivery MCPEventsDelivery, secret bool) error {
	if name != Name {
		return failure(NotFound, "event")
	}
	if (args.PersonID > 0) == (args.SourceID > 0) {
		return failure(InvalidParams, "invalid_arguments")
	}
	if args.IncomingOnly == nil {
		v := true
		args.IncomingOnly = &v
	}
	if (secret && delivery.Mode != "webhook") || (!secret && delivery.Mode != "" && delivery.Mode != "webhook") {
		return failure(Unsupported, "delivery")
	}
	if delivery.URL == "" || (secret && delivery.Secret == "") {
		return failure(InvalidParams, "invalid_delivery")
	}
	if config.ValidateWakeupDestination(delivery.URL) != nil {
		return failure(InvalidParams, "invalid_callback")
	}
	return nil
}
func DecodeSubscribe(raw []byte) (MCPEventsSubscribeRequest, error) {
	var v MCPEventsSubscribeRequest
	if err := decode(raw, &v); err != nil {
		return v, err
	}
	if err := validate(v.Name, &v.Arguments, v.Delivery, true); err != nil {
		return v, err
	}
	var err error
	v.secret, err = wakeups.DecodeSecret(v.Delivery.Secret)
	if err != nil {
		return v, failure(InvalidParams, "invalid_secret")
	}
	return v, nil
}
func DecodeUnsubscribe(raw []byte) (MCPEventsUnsubscribeRequest, error) {
	var v MCPEventsUnsubscribeRequest
	if err := decode(raw, &v); err != nil {
		return v, err
	}
	return v, validate(v.Name, &v.Arguments, v.Delivery, false)
}
