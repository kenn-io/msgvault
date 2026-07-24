package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// operationDeclaresJSONRequestBody reports whether a raw-route operation
// documents an application/json request body, which is how every JSON
// mutation route in this package registers (jsonRequestBodyFor). It keys the
// media-type gate in registerRawHumaRoute so bodyless routes and any future
// non-JSON upload route are untouched.
func operationDeclaresJSONRequestBody(op *huma.Operation) bool {
	if op.RequestBody == nil {
		return false
	}
	_, ok := op.RequestBody.Content["application/json"]
	return ok
}

// enforceJSONRequestMediaType wraps the handler of a route that decodes a
// JSON request body. Unsafe-method requests carrying a body must declare
// Content-Type application/json (media-type parameters such as charset are
// allowed); anything else — including the CORS-safelisted text/plain a
// browser can send cross-origin without a preflight — is rejected with 415
// before the handler's decoder ever sees the body. Bodyless requests
// (Content-Length: 0) pass so no-body POST clients are unaffected.
func enforceJSONRequestMediaType(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) && r.ContentLength != 0 && !hasJSONContentType(r) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"Content-Type must be application/json")
			return
		}
		next(w, r)
	}
}

func hasJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

// requireSingleJSONValue verifies no second JSON value follows the one dec
// already decoded, rejecting bodies like `{"a":1}{"b":2}` where a decoder
// would silently act on the first value only. code preserves each route's
// error-code idiom ("invalid_request", "invalid_json", ...).
func requireSingleJSONValue(w http.ResponseWriter, dec *json.Decoder, code string) bool {
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, code,
			"request body must contain exactly one JSON value")
		return false
	}
	return true
}
