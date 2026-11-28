package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/msgvault/internal/mcpevents"
)

type EventsBackend interface {
	MCPEventsList(ctx context.Context) (mcpevents.MCPEventsListResult, error)
	MCPEventsSubscribe(ctx context.Context, request mcpevents.MCPEventsSubscribeRequest) (mcpevents.MCPEventsSubscribeResult, error)
	MCPEventsUnsubscribe(ctx context.Context, request mcpevents.MCPEventsUnsubscribeRequest) error
}
type nativeEventParams struct {
	sdkmcp.ParamsBase

	raw jsontext.Value
}

func (p *nativeEventParams) UnmarshalJSON(b []byte) error {
	var m struct {
		Meta sdkmcp.Meta `json:"_meta,omitempty"`
	}
	if err := json.Unmarshal(b, &m, jsontext.AllowDuplicateNames(true)); err != nil {
		return err
	}
	p.Meta = m.Meta
	p.raw = append(p.raw[:0], b...)
	return nil
}
func (p *nativeEventParams) clean() ([]byte, error) {
	if p == nil || len(p.raw) > 64<<10 {
		return nil, rpcEventError(&mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_params"})
	}
	if len(p.raw) == 0 {
		return []byte(`{}`), nil
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(p.raw, &fields) != nil || fields == nil {
		return nil, rpcEventError(&mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_params"})
	}
	delete(fields, "_meta")
	return json.Marshal(fields)
}

type nativeEventResult struct {
	sdkmcp.ResultBase

	value    any
	complete bool
}

func (r *nativeEventResult) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(r.value)
	if err != nil {
		return nil, err
	}
	var wire map[string]jsontext.Value
	if err = json.Unmarshal(b, &wire); err != nil {
		return nil, err
	}
	if len(r.Meta) > 0 {
		meta := map[string]jsontext.Value{}
		if raw, ok := wire["_meta"]; ok {
			if err = json.Unmarshal(raw, &meta); err != nil {
				return nil, err
			}
		}
		for k, v := range r.Meta {
			if _, ok := meta[k]; !ok {
				meta[k], err = json.Marshal(v)
				if err != nil {
					return nil, err
				}
			}
		}
		wire["_meta"], err = json.Marshal(meta)
		if err != nil {
			return nil, err
		}
	}
	if r.complete {
		wire["resultType"] = jsontext.Value(`"complete"`)
	}
	return json.Marshal(wire)
}
func rpcEventError(err error, mode ...string) error {
	var e *mcpevents.Error
	if !errors.As(err, &e) {
		e = &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "daemon_unavailable"}
	}
	name := "Unavailable"
	switch e.Code {
	case mcpevents.InvalidParams:
		name = "InvalidParams"
	case mcpevents.NotFound:
		name = "NotFound"
	case mcpevents.Forbidden:
		name = "Forbidden"
	case mcpevents.CallbackEndpoint:
		name = "CallbackEndpointError"
	case mcpevents.ResourceExhausted:
		name = "ResourceExhausted"
	case mcpevents.Unsupported:
		name = "Unsupported"
	case mcpevents.Unavailable:
	default:
		e.Code = mcpevents.Unavailable
		e.Reason = "daemon_unavailable"
	}
	payload := map[string]any{"reason": e.Reason}
	switch e.Code {
	case mcpevents.NotFound:
		payload = map[string]any{"kind": e.Reason}
	case mcpevents.ResourceExhausted:
		payload = map[string]any{"limit": e.Reason}
	case mcpevents.Unsupported:
		payload = map[string]any{"feature": "deliveryMode"}
		if len(mode) != 0 {
			payload["value"] = mode[0]
		}
	}
	data, _ := json.Marshal(payload)
	return &jsonrpc.Error{Code: int64(e.Code), Message: name, Data: data}
}
func registerNativeEvents(server *sdkmcp.Server, backend EventsBackend, allowWrites bool) {
	_ = sdkmcp.AddReceivingCustomMethod(server, "events/list", func(ctx context.Context, _ *sdkmcp.ServerSession, p *nativeEventParams) (*nativeEventResult, error) {
		b := []byte(`{}`)
		var err error
		if p != nil {
			b, err = p.clean()
		}
		if err != nil {
			return nil, err
		}
		var v struct{}
		if json.Unmarshal(b, &v, json.RejectUnknownMembers(true)) != nil {
			return nil, rpcEventError(&mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_params"})
		}
		out, err := backend.MCPEventsList(ctx)
		if err != nil {
			return nil, rpcEventError(err)
		}
		return &nativeEventResult{value: map[string]any{"events": out.Events}, complete: p.modern()}, nil
	})
	if allowWrites {
		_ = sdkmcp.AddReceivingCustomMethod(server, "events/subscribe", func(ctx context.Context, _ *sdkmcp.ServerSession, p *nativeEventParams) (*nativeEventResult, error) {
			b, err := p.clean()
			if err != nil {
				return nil, err
			}
			in, err := mcpevents.DecodeSubscribe(b)
			if err != nil {
				return nil, rpcEventError(err, in.Delivery.Mode)
			}
			out, err := backend.MCPEventsSubscribe(ctx, in)
			if err != nil {
				return nil, rpcEventError(err, in.Delivery.Mode)
			}
			out.DeliveryStatus.LastError = mcpevents.DeliveryCategory(out.DeliveryStatus.LastError)
			return &nativeEventResult{value: out, complete: p.modern()}, nil
		})
		_ = sdkmcp.AddReceivingCustomMethod(server, "events/unsubscribe", func(ctx context.Context, _ *sdkmcp.ServerSession, p *nativeEventParams) (*nativeEventResult, error) {
			b, err := p.clean()
			if err != nil {
				return nil, err
			}
			in, err := mcpevents.DecodeUnsubscribe(b)
			if err != nil {
				return nil, rpcEventError(err, in.Delivery.Mode)
			}
			if err = backend.MCPEventsUnsubscribe(ctx, in); err != nil {
				return nil, rpcEventError(err, in.Delivery.Mode)
			}
			return &nativeEventResult{value: struct{}{}, complete: p.modern()}, nil
		})
	}
}
func (p *nativeEventParams) modern() bool {
	if p == nil {
		return false
	}
	version, _ := p.Meta[sdkmcp.MetaKeyProtocolVersion].(string)
	return version >= "2026-07-28"
}
