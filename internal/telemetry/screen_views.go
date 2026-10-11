package telemetry

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/telemetry/posthog"
)

var screenNames = []string{"everything", "directory", "directory_review", "files", "operations", "relationships", "saved_views", "sources", "deletions", "settings", "message", "email", "texts", "meetings"}

const screenViewsFile = "telemetry-screen-views.json"

type screenViews struct {
	InstallID string          `json:"install_id"`
	Day       string          `json:"day"`
	Screens   map[string]bool `json:"screens"`
}

type screenCaptureHandler struct {
	reporter *posthog.Reporter
	capture  http.Handler
	dir      string
	now      func() time.Time
	mu       sync.Mutex
}

func (h *screenCaptureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		h.capture.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if _, large := errors.AsType[*http.MaxBytesError](err); large {
		http.Error(w, "telemetry request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	}
	if err != nil || json.Unmarshal(body, &req, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true), json.MatchCaseInsensitiveNames(true)) != nil {
		http.Error(w, "invalid telemetry request", http.StatusBadRequest)
		return
	}
	req.Event = strings.TrimSpace(req.Event)
	canonical, err := json.Marshal(req)
	if err != nil {
		http.Error(w, "invalid telemetry request", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(canonical))
	if req.Event != EventScreenViewed || !h.reporter.EventAllowed(req.Event) || !h.reporter.Enabled() {
		h.capture.ServeHTTP(w, r)
		return
	}
	req.Properties, _ = h.reporter.SanitizeProperties(req.Event, req.Properties)
	if req.Properties["screen"] == nil || req.Properties[propertySurface] == nil {
		acceptedScreen(w)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	path := filepath.Join(h.dir, screenViewsFile)
	var install posthog.Install
	identity, err := readScreenFile(filepath.Join(h.dir, posthog.InstallFileName))
	if err != nil || json.Unmarshal(identity, &install) != nil || install.ID == "" {
		h.storageError(w, errors.Join(err, errors.New("read telemetry install identity")))
		return
	}
	previous, err := readScreenFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		h.storageError(w, err)
		return
	}
	var state screenViews
	if len(previous) > 64<<10 || len(previous) > 0 && json.Unmarshal(previous, &state) != nil {
		h.storageError(w, errors.New("invalid screen views file"))
		return
	}
	day := h.now().UTC().Format(time.DateOnly)
	if state.InstallID != install.ID || state.Day != day {
		state = screenViews{InstallID: install.ID, Day: day, Screens: map[string]bool{}}
	}
	if state.Screens == nil {
		state.Screens = map[string]bool{}
	}
	screen, _ := req.Properties["screen"].(string)
	if state.Screens[screen] {
		acceptedScreen(w)
		return
	}
	state.Screens[screen] = true
	encoded, err := json.Marshal(state)
	if err == nil {
		err = writeScreenViews(path, encoded)
	}
	if err != nil {
		h.storageError(w, err)
		return
	}
	if captureErr := h.reporter.Capture(req.Event, req.Properties); captureErr != nil {
		if len(previous) == 0 {
			err = os.Remove(path)
		} else {
			err = writeScreenViews(path, previous)
		}
		if err != nil {
			slog.Warn("restore telemetry screen views", "error", err)
		}
		http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
		return
	}
	acceptedScreen(w)
}

func readScreenFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, (64<<10)+1))
}

func writeScreenViews(path string, data []byte) error {
	err := atomicfile.WriteFile(path, data, atomicfile.WithPerm(0o600))
	if errors.Is(err, atomicfile.ErrPublished) {
		slog.Warn("telemetry screen views publication", "error", err)
		return nil
	}
	return err
}

func (h *screenCaptureHandler) storageError(w http.ResponseWriter, err error) {
	slog.Warn("persist telemetry screen views", "error", err)
	http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
}

func acceptedScreen(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, "{\"status\":\"queued\"}\n")
}
