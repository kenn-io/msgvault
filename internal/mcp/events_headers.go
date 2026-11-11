package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxEventsBodyBytes bounds the body the Mcp-Name guard buffers. Events
// requests carry a name, arguments and a delivery target, so 1 MiB is ample;
// the larger transport limit exists for image-sized tools/call bodies.
const maxEventsBodyBytes = 1 << 20

// isEventsNameMethod reports whether method is a name-addressed events/*
// method that requires Mcp-Name.
func isEventsNameMethod(method string) bool {
	switch method {
	case "events/subscribe", "events/unsubscribe":
		return true
	}
	return false
}

// eventsNameHeaderHandler requires Mcp-Name to equal params.name for the
// name-addressed events/* methods. It belongs on the 2026-07-28 path only. It
// decides from the Mcp-Method header alone and never touches the body of other
// methods.
//
// This relies on the go-sdk's validateMcpHeaders: for protocol 2026-07-28
// single requests the SDK rejects (HTTP 400, -32020) any request whose
// Mcp-Method is missing or differs from the body's method. A request that
// lies about its method never reaches the inner handler. If an SDK upgrade
// drops that check, this guard must also validate the method.
//
// Like the SDK, a body over the limit is answered with HTTP 413 and a
// plain-text "request body exceeds N bytes".
func eventsNameHeaderHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Protocol-Version") < "2026-07-28" || r.Method != http.MethodPost || !isEventsNameMethod(r.Header.Get("Mcp-Method")) {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventsBodyBytes))
		if err != nil {
			if limit, ok := errors.AsType[*http.MaxBytesError](err); ok {
				http.Error(w, fmt.Sprintf("request body exceeds %d bytes", limit.Limit), http.StatusRequestEntityTooLarge)
				return
			}
			// Let the SDK report other unreadable bodies itself.
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{err}))
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		// Read members case-sensitively, like the events handler
		// (encoding/json/v2), and reject a repeated "params" or "name": the
		// guard and the handler must not disagree about which one is real.
		// The method comes from the Mcp-Method header only. Anything that is
		// not one object with a string, non-empty params.name fails closed.
		name, id, ok := eventsRequestName(body)
		if !ok || name == "" {
			writeHeaderMismatch(w, id)
			return
		}
		got, ok := decodeMcpNameHeader(r.Header.Get("Mcp-Name"))
		if r.Header.Get("Mcp-Name") == "" || !ok || got != name {
			writeHeaderMismatch(w, id)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// eventsRequestName rejects malformed or duplicate JSON before comparing names.
func eventsRequestName(body []byte) (name string, id jsontext.Value, ok bool) {
	var request struct {
		ID     jsontext.Value `json:"id"`
		Params *struct {
			Name *string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return "", nil, false
	}
	if request.Params == nil || request.Params.Name == nil {
		return "", request.ID, false
	}
	return *request.Params.Name, request.ID, true
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// decodeMcpNameHeader decodes the Base64 sentinel form =?base64?<b64>?=.
func decodeMcpNameHeader(v string) (string, bool) {
	const prefix, suffix = "=?base64?", "?="
	if !strings.HasPrefix(v, prefix) || !strings.HasSuffix(v, suffix) || len(v) < len(prefix)+len(suffix) {
		return v, true
	}
	decoded, err := base64.StdEncoding.DecodeString(v[len(prefix) : len(v)-len(suffix)])
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

func writeHeaderMismatch(w http.ResponseWriter, id jsontext.Value) {
	if len(id) == 0 {
		id = jsontext.Value("null")
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32020, "message": "HeaderMismatch", "data": map[string]string{"header": "Mcp-Name"}},
	})
	if err != nil {
		http.Error(w, "header mismatch: Mcp-Name", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(body)
}
