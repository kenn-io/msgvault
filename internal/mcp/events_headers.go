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
		if r.Method != http.MethodPost || !isEventsNameMethod(r.Header.Get("Mcp-Method")) {
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

// jsonMembers returns the top-level member values of a single JSON object,
// keyed case-sensitively, keeping every occurrence of a repeated name. Leading
// or trailing bytes, including a byte order mark, make it fail.
func jsonMembers(raw []byte) (map[string][]jsontext.Value, bool) {
	dec := jsontext.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)), jsontext.AllowDuplicateNames(true))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		return nil, false
	}
	members := make(map[string][]jsontext.Value)
	for dec.PeekKind() != '}' {
		key, err := dec.ReadToken()
		if err != nil || key.Kind() != '"' {
			return nil, false
		}
		name := key.String() // the token is voided by the next read
		value, err := dec.ReadValue()
		if err != nil {
			return nil, false
		}
		members[name] = append(members[name], append(jsontext.Value(nil), value...))
	}
	if _, err := dec.ReadToken(); err != nil {
		return nil, false
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return members, true
}

// eventsRequestName extracts params.name and the request id. ok is false for
// anything that is not one object holding exactly one params object with
// exactly one string name; id is then best effort.
func eventsRequestName(body []byte) (name string, id jsontext.Value, ok bool) {
	top, valid := jsonMembers(body)
	if !valid {
		return "", nil, false
	}
	if ids := top["id"]; len(ids) == 1 {
		id = ids[0]
	}
	if len(top["params"]) != 1 {
		return "", id, false
	}
	params, valid := jsonMembers(top["params"][0])
	if !valid || len(params["name"]) != 1 {
		return "", id, false
	}
	if err := json.Unmarshal(params["name"][0], &name); err != nil {
		return "", id, false
	}
	return name, id, true
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
