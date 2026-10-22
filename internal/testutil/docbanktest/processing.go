package docbanktest

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ProcessingHandler models Docbank's profile, plan, job stream, and status routes.
// Configure fields between worker passes; request records are guarded by Mu.
type ProcessingHandler struct {
	Profiles        int
	NodeIDs         map[string]int64
	JobFaults       map[string]int
	PollFaults      map[string]int
	Reasons         map[string]string
	Rendition       bool
	T               *testing.T
	Mu              sync.Mutex
	Fingerprint     string
	PlanFingerprint string
	Faults          map[string]int       // content version to plan HTTP status
	States          map[string][3]string // job ID to state, phase, and failure code
	Polls           []string
	Plans           []string
	Jobs            []string
}

func NewProcessingHandler(t *testing.T) *ProcessingHandler {
	t.Helper()
	return &ProcessingHandler{NodeIDs: map[string]int64{}, JobFaults: map[string]int{}, PollFaults: map[string]int{}, Reasons: map[string]string{}, T: t, Fingerprint: "profile-one", Rendition: true, PlanFingerprint: "plan-one", Faults: map[string]int{}, States: map[string][3]string{}}
}

func (h *ProcessingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Mu.Lock()
	defer h.Mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/processing/profiles":
		h.Profiles++
		assert.Equal(h.T, http.MethodGet, r.Method)
		assert.NoError(h.T, json.MarshalWrite(w, []map[string]any{{"name": "documents", "fingerprint": h.Fingerprint, "rendition": h.Rendition}}))
	case "/api/v1/processing/plans", "/api/v1/processing/jobs":
		assert.Equal(h.T, http.MethodPost, r.Method)
		var input struct {
			Selector struct {
				NodeID  int64  `json:"node_id"`
				Version string `json:"content_version_id"`
				Profile string `json:"profile"`
			} `json:"selector"`
			Fingerprint string `json:"plan_fingerprint"`
			Consent     *bool  `json:"consent"`
		}
		if !assert.NoError(h.T, json.UnmarshalRead(r.Body, &input)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Positive(h.T, input.Selector.NodeID)
		assert.Equal(h.T, "documents", input.Selector.Profile)
		version := input.Selector.Version
		h.NodeIDs[version] = input.Selector.NodeID
		if r.URL.Path == "/api/v1/processing/plans" {
			h.Plans = append(h.Plans, version)
			if status := h.Faults[version]; status != 0 {
				if status == FaultInvalidReceipt {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				w.WriteHeader(status)
				assert.NoError(h.T, json.MarshalWrite(w, map[string]string{"code": h.Reasons[version]}))
				return
			}
			assert.NoError(h.T, json.MarshalWrite(w, map[string]string{"fingerprint": h.PlanFingerprint}))
		} else {
			assert.Equal(h.T, h.PlanFingerprint, input.Fingerprint)
			if assert.NotNil(h.T, input.Consent) {
				assert.False(h.T, *input.Consent)
			}
			h.Jobs = append(h.Jobs, version)
			if status := h.JobFaults[version]; status != 0 {
				if status == FaultInvalidReceipt {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				w.WriteHeader(status)
				assert.NoError(h.T, json.MarshalWrite(w, map[string]string{"code": h.Reasons[version]}))
				return
			}
			state, ok := h.States["job-"+version]
			if !ok || state[0] == "failed" {
				h.States["job-"+version] = [3]string{"queued", "rendition", ""}
			}
			assert.NoError(h.T, json.MarshalWrite(w, map[string]any{"type": "job", "job": map[string]string{"id": "job-" + version}}))
		}
	default:
		if id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/processing/jobs/"); ok {
			h.Polls = append(h.Polls, id)
			if status := h.PollFaults[id]; status != 0 {
				w.WriteHeader(status)
				return
			}
			state, ok := h.States[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				assert.NoError(h.T, json.MarshalWrite(w, map[string]string{"code": "not_found"}))
				return
			}
			WriteJobStatus(w, id, state[0], state[1], state[2])
		} else {
			http.Error(w, "unexpected processing route "+r.URL.Path, http.StatusNotFound)
		}
	}
}

// WriteJobStatus writes the shared Docbank processing job response.
func WriteJobStatus(w http.ResponseWriter, id, state, phase, code string) {
	if err := json.MarshalWrite(w, map[string]any{"job_id": id, "state": state, "phase": phase,
		"failure_code": code, "embedding_job_ids": []string{}, "completed_bindings": 1}); err != nil {
		http.Error(w, "encode job status", http.StatusInternalServerError)
	}
}
