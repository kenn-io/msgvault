package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/twilio"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

var failureKinds = []string{"auth", "not_found", "rate_limited", "provider", "network", "other"}

// FailureKind returns a fixed category, without inspecting or reporting error text.
func FailureKind(err error) string {
	if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok {
		return StatusKind(apiErr.Status)
	}
	if apiErr, ok := errors.AsType[*googleapi.Error](err); ok {
		return StatusKind(apiErr.Code)
	}
	if provider, ok := errors.AsType[*gmail.StatusError](err); ok {
		return StatusKind(provider.StatusCode)
	}
	if _, ok := errors.AsType[*gmail.ThrottledError](err); ok {
		return "rate_limited"
	}
	if _, ok := errors.AsType[*gmail.NotFoundError](err); ok {
		return "not_found"
	}
	if provider, ok := errors.AsType[*twilio.APIError](err); ok {
		return StatusKind(provider.StatusCode)
	}
	if auth, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		switch auth.ErrorCode {
		case "invalid_grant", "invalid_client", "unauthorized_client", "access_denied", "invalid_token":
			return "auth"
		}
		if auth.Response != nil {
			return StatusKind(auth.Response.StatusCode)
		}
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) {
		return "network"
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, store.ErrMessageNotFound) || errors.Is(err, store.ErrSourceNotFound) {
		return "not_found"
	}
	return "other"
}

// StatusKind groups HTTP failures into the outcome taxonomy.
func StatusKind(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "auth"
	case status == http.StatusNotFound:
		return "not_found"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status >= 500 && status <= 599:
		return "provider"
	default:
		return "other"
	}
}

// Outcomes retains only source hashes for outages that have not recovered.
// The daemon owns one tracker shared by scheduled and CLI syncs.
type Outcomes struct {
	mu      sync.Mutex
	path    string
	capture func(string, map[string]any) error
}

// NewOutcomes shares persistent claims between daemon sync owners.
func NewOutcomes(dir string, capture func(string, map[string]any) error) *Outcomes {
	return &Outcomes{path: filepath.Join(dir, "telemetry-outages.json"), capture: capture}
}

// SyncResult claims a source's first failure until its next successful sync.
// An empty key denotes an all-sources CLI run; success clears every outage.
func (o *Outcomes) SyncResult(key string, err error) error {
	if o == nil || o.capture == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	state := map[string]bool{}
	data, readErr := readScreenFile(o.path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if len(data) > 64<<10 {
		return errors.New("telemetry outage file too large")
	}
	if len(data) > 0 {
		if decodeErr := json.Unmarshal(data, &state); decodeErr != nil {
			return decodeErr
		}
	}
	if state == nil {
		state = map[string]bool{}
	}
	hash := sha256.Sum256([]byte(key))
	source := hex.EncodeToString(hash[:])
	if err == nil {
		if key == "" {
			clear(state)
		} else {
			delete(state, source)
		}
	} else {
		if state[source] || len(state) >= 512 {
			return nil
		}
		state[source] = true
	}
	encoded, encodeErr := json.Marshal(state)
	if encodeErr != nil {
		return encodeErr
	}
	if writeErr := writeScreenViews(o.path, encoded); writeErr != nil {
		return writeErr
	}
	if err != nil {
		if captureErr := o.capture(EventSyncFailed, map[string]any{"reason": FailureKind(err)}); captureErr != nil {
			var restoreErr error
			if len(data) == 0 {
				restoreErr = os.Remove(o.path)
			} else {
				restoreErr = writeScreenViews(o.path, data)
			}
			return errors.Join(captureErr, restoreErr)
		}
	}
	return nil
}

func normalizeFailure(value any) string {
	kind, _ := value.(string)
	if slices.Contains(failureKinds, kind) {
		return kind
	}
	return "other"
}
