package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

const applicationJSONMediaType = "application/json"

// enforceRequestMediaType checks body-bearing unsafe requests against the
// operation's declared request media types. Parameters such as charset and
// multipart boundary are allowed. Bodyless requests pass unchanged.
func enforceRequestMediaType(content map[string]*huma.MediaType, next http.HandlerFunc) http.HandlerFunc {
	message := "Content-Type must be " + strings.Join(slices.Sorted(maps.Keys(content)), " or ")
	return func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) && r.ContentLength != 0 {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if _, allowed := content[mediaType]; err != nil || !allowed {
				writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", message)
				return
			}
		}
		next(w, r)
	}
}

// requireSingleJSONValue verifies no second JSON value follows the one dec
// already decoded, rejecting bodies like `{"a":1}{"b":2}` where a decoder
// would silently act on the first value only. code preserves each route's
// error-code idiom ("invalid_request", "invalid_json", ...).
func requireSingleJSONValue(w http.ResponseWriter, dec *jsontext.Decoder, code string) bool {
	if err := json.UnmarshalDecode(dec, &struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, code,
			"request body must contain exactly one JSON value")
		return false
	}
	return true
}
