package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// paramError marks an invalid query-parameter value. Handlers translate it into
// a uniform 400 that names the parameter and the expected format so a client
// typo surfaces as a clear rejection instead of a silently ignored filter.
type paramError struct {
	param   string
	message string
}

func (e *paramError) Error() string { return e.message }

func newParamError(param, message string) *paramError {
	return &paramError{param: param, message: message}
}

// rejectBadParam writes a 400 for an invalid query parameter, using the
// parameter-specific error code (invalid_<param>) when err is a paramError.
func (s *Server) rejectBadParam(w http.ResponseWriter, err error) {
	var pe *paramError
	if errors.As(err, &pe) {
		writeError(w, http.StatusBadRequest, "invalid_"+pe.param, pe.message)
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
}

// apiHTTPErrorFromParam adapts a paramError into an *apiHTTPError for handlers
// that surface errors through writeAPIHTTPError. It preserves the
// parameter-specific error code so the response envelope matches rejectBadParam.
func apiHTTPErrorFromParam(err error) *apiHTTPError {
	var pe *paramError
	if errors.As(err, &pe) {
		return newAPIHTTPError(http.StatusBadRequest, "invalid_"+pe.param, pe.message)
	}
	return newAPIHTTPError(http.StatusBadRequest, "invalid_parameter", err.Error())
}

// queryInt parses an integer query parameter. ok is false when the parameter is
// absent; a present but non-numeric value returns a paramError.
func queryInt(r *http.Request, name string) (value int, ok bool, err error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false, nil
	}
	value, convErr := strconv.Atoi(raw)
	if convErr != nil {
		return 0, false, newParamError(name,
			fmt.Sprintf("query parameter %q must be an integer, got %q", name, raw))
	}
	return value, true, nil
}

// queryInt64 parses a 64-bit integer query parameter. ok is false when absent;
// a present but non-numeric value returns a paramError.
func queryInt64(r *http.Request, name string) (value int64, ok bool, err error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false, nil
	}
	value, convErr := strconv.ParseInt(raw, 10, 64)
	if convErr != nil {
		return 0, false, newParamError(name,
			fmt.Sprintf("query parameter %q must be an integer, got %q", name, raw))
	}
	return value, true, nil
}

// queryInt64s parses a repeated or comma-separated 64-bit integer query
// parameter. ok is false when absent; a present empty or non-numeric element
// returns a paramError rather than silently widening the requested scope.
func queryInt64s(r *http.Request, name string) (values []int64, ok bool, err error) {
	rawValues, ok := r.URL.Query()[name]
	if !ok {
		return nil, false, nil
	}
	for _, raw := range rawValues {
		for part := range strings.SplitSeq(raw, ",") {
			part = strings.TrimSpace(part)
			value, convErr := strconv.ParseInt(part, 10, 64)
			if convErr != nil {
				return nil, false, newParamError(name,
					fmt.Sprintf("query parameter %q must contain only integers, got %q", name, part))
			}
			values = append(values, value)
		}
	}
	return values, true, nil
}

// queryBool parses a boolean query parameter. ok is false when absent; a
// present but unparseable value returns a paramError.
func queryBool(r *http.Request, name string) (value bool, ok bool, err error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, false, nil
	}
	value, convErr := strconv.ParseBool(raw)
	if convErr != nil {
		return false, false, newParamError(name,
			fmt.Sprintf("query parameter %q must be a boolean, got %q", name, raw))
	}
	return value, true, nil
}

// queryFloat parses a floating-point query parameter. ok is false when absent;
// a present but non-numeric value returns a paramError.
func queryFloat(r *http.Request, name string) (value float64, ok bool, err error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false, nil
	}
	value, convErr := strconv.ParseFloat(raw, 64)
	if convErr != nil {
		return 0, false, newParamError(name,
			fmt.Sprintf("query parameter %q must be a number, got %q", name, raw))
	}
	return value, true, nil
}

// queryDate parses an RFC3339 or YYYY-MM-DD date parameter, normalized to UTC.
// ok is false when the parameter is absent; a present but unparseable value
// returns a paramError.
func queryDate(r *http.Request, name string) (value time.Time, ok bool, err error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, false, nil
	}
	value, parseErr := parseAPITime(raw)
	if parseErr != nil {
		return time.Time{}, false, newParamError(name,
			fmt.Sprintf("query parameter %q must be an RFC3339 or YYYY-MM-DD date, got %q", name, raw))
	}
	return value, true, nil
}

// enumParamError builds a paramError listing the accepted values for an enum
// parameter.
func enumParamError(name, raw string, allowed []string) *paramError {
	return newParamError(name,
		fmt.Sprintf("query parameter %q must be one of %s, got %q",
			name, strings.Join(allowed, ", "), raw))
}
