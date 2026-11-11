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
	MCPEventsList(context.Context) (mcpevents.ListResult, error)
	MCPEventsSubscribe(context.Context, mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error)
	MCPEventsUnsubscribe(context.Context, mcpevents.UnsubscribeRequest) error
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
	var fields map[string]jsontext.Value
	if json.Unmarshal(p.raw, &fields) != nil || fields == nil {
		return nil, rpcEventError(&mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_params"})
	}
	delete(fields, "_meta")
	return json.Marshal(fields)
}
func (p *nativeEventParams) subscribe() (mcpevents.SubscribeRequest, error) {
	b, err := p.clean()
	if err != nil {
		return mcpevents.SubscribeRequest{}, err
	}
	v, err := mcpevents.DecodeSubscribe(b)
	if err != nil {
		return v, rpcEventError(err)
	}
	return v, nil
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
func rpcEventError(err error) error {
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
	case mcpevents.ResourceExhausted:
		name = "ResourceExhausted"
	case mcpevents.Unsupported:
		name = "Unsupported"
	default:
		e.Code = mcpevents.Unavailable
		e.Reason = "daemon_unavailable"
	}
	data, _ := json.Marshal(map[string]any{"reason": e.Reason})
	return &jsonrpc.Error{Code: int64(e.Code), Message: name, Data: data}
}
func registerNativeEvents(server *sdkmcp.Server, backend EventsBackend) {
	_ = sdkmcp.AddReceivingCustomMethod(server, "events/list", func(ctx context.Context, _ *sdkmcp.ServerSession, p *nativeEventParams) (*nativeEventResult, error) {
		b, err := p.clean()
		if err != nil {
			return nil, err
		}
		var v struct{}
		if json.Unmarshal(b, &v, json.RejectUnknownMembers(true)) != nil {
			return nil, rpcEventError(&mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_params"})
		}
		out, err := backend.MCPEventsList(ctx)
		if err != nil {
			out = mcpevents.ListResult{Events: []mcpevents.Definition{}}
		}
		return &nativeEventResult{value: map[string]any{"events": out.Events}, complete: true}, nil
	})
	_ = sdkmcp.AddReceivingCustomMethod(server, "events/subscribe", func(ctx context.Context, _ *sdkmcp.ServerSession, p *nativeEventParams) (*nativeEventResult, error) {
		in, err := p.subscribe()
		if err != nil {
			return nil, err
		}
		out, err := backend.MCPEventsSubscribe(ctx, in)
		if err != nil {
			return nil, rpcEventError(err)
		}
		return &nativeEventResult{value: out, complete: true}, nil
	})
	_ = sdkmcp.AddReceivingCustomMethod(server, "events/unsubscribe", func(ctx context.Context, _ *sdkmcp.ServerSession, p *nativeEventParams) (*nativeEventResult, error) {
		b, err := p.clean()
		if err != nil {
			return nil, err
		}
		in, err := mcpevents.DecodeUnsubscribe(b)
		if err != nil {
			return nil, rpcEventError(err)
		}
		if err = backend.MCPEventsUnsubscribe(ctx, in); err != nil {
			return nil, rpcEventError(err)
		}
		return &nativeEventResult{value: struct{}{}, complete: true}, nil
	})
	server.AddReceivingMiddleware(nativeEventsCapabilities)
}
func nativeEventsCapabilities(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
		result, err := next(ctx, method, request)
		if err != nil || (method != "initialize" && method != "server/discover") {
			return result, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		var wire map[string]any
		if err = json.Unmarshal(data, &wire); err != nil {
			return nil, err
		}
		caps, ok := wire["capabilities"].(map[string]any)
		if !ok {
			caps = map[string]any{}
			wire["capabilities"] = caps
		}
		caps["events"] = map[string]any{}
		ext, _ := caps["extensions"].(map[string]any)
		if ext == nil {
			ext = map[string]any{}
			caps["extensions"] = ext
		}
		ext["io.modelcontextprotocol/events"] = map[string]any{}
		complete := false
		if p := request.GetParams(); p != nil {
			version, _ := p.GetMeta()[sdkmcp.MetaKeyProtocolVersion].(string)
			complete = version >= "2026-07-28"
		}
		return &nativeEventResult{value: wire, complete: complete}, nil
	}
}
