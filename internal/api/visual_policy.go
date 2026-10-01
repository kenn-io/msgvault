package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"
)

// VisualRuntimePolicy describes the installed upload authority. Its fingerprint
// comes from the actual provider policy, rather than previously recorded consent.
type VisualRuntimePolicy struct {
	GenerationID             int64    `json:"generation_id"`
	GenerationFingerprint    string   `json:"generation_fingerprint"`
	CurrentPolicyFingerprint string   `json:"current_policy_fingerprint"`
	Provider                 string   `json:"provider"`
	Model                    string   `json:"model"`
	Dimension                int      `json:"dimension"`
	SourceIDs                []int64  `json:"source_ids" doc:"An empty list includes all archived sources within the configured message types"`
	MessageTypes             []string `json:"message_types" doc:"An empty list includes every message type within the source scope"`
	IncludeImages            bool     `json:"include_images"`
	IncludeVideo             bool     `json:"include_video"`
	IncludeAnimatedGIFs      bool     `json:"include_animated_gifs"`
	MaxContextChars          int      `json:"max_context_chars"`
	MaxMediaBytes            int64    `json:"max_media_bytes"`
	MaxMediaPixels           int64    `json:"max_media_pixels"`
	MaxOwnersPerPass         int      `json:"max_owners_per_pass"`
	RetentionPosture         string   `json:"retention_posture"`
	TrainingPosture          string   `json:"training_posture"`
	AuthorizedCapabilities   []string `json:"authorized_capabilities"`
}

func (p VisualRuntimePolicy) clone() VisualRuntimePolicy {
	p.SourceIDs = slices.Clone(p.SourceIDs)
	p.MessageTypes = slices.Clone(p.MessageTypes)
	p.AuthorizedCapabilities = slices.Clone(p.AuthorizedCapabilities)
	return p
}

// VisualOperationGuard optionally pins an operator request to the installed
// runtime. All three fields are required together; old unguarded callers remain
// supported by the owning API. MCP always supplies the complete guard.
type VisualOperationGuard struct {
	ExpectedGenerationID          *int64  `json:"expected_generation_id,omitempty" minimum:"1"`
	ExpectedGenerationFingerprint *string `json:"expected_generation_fingerprint,omitempty" minLength:"1"`
	ExpectedPolicyFingerprint     *string `json:"expected_policy_fingerprint,omitempty" minLength:"1"`
	specified                     bool
}

type visualGuardContextKey struct{}

// ErrVisualPolicyChanged refuses a guarded callback before any upload or consent.
var ErrVisualPolicyChanged = errors.New("visual runtime policy changed")

// VisualOperationGuardRequested distinguishes expected-policy requests from
// existing background and operator calls that retain their own consent checks.
func VisualOperationGuardRequested(ctx context.Context) bool {
	_, guarded := ctx.Value(visualGuardContextKey{}).(VisualOperationGuard)
	return guarded
}

// CheckVisualOperationGuard verifies the same request at the owning callback
// before consent, reconciliation or provider work. Background callers have no
// guard and retain their existing authority checks.
func CheckVisualOperationGuard(ctx context.Context, generationID int64, generationFingerprint, policyFingerprint string) error {
	guard, ok := ctx.Value(visualGuardContextKey{}).(VisualOperationGuard)
	if !ok {
		return nil
	}
	if guard.ExpectedGenerationID == nil || guard.ExpectedGenerationFingerprint == nil || guard.ExpectedPolicyFingerprint == nil || *guard.ExpectedGenerationID != generationID || *guard.ExpectedGenerationFingerprint != generationFingerprint || *guard.ExpectedPolicyFingerprint != policyFingerprint {
		return ErrVisualPolicyChanged
	}
	return nil
}

func decodeVisualOperationRequest(w http.ResponseWriter, r *http.Request, target any, guard *VisualOperationGuard, allowEmpty bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(data) == 0 && allowEmpty {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("visual request must be an object")
	}
	for _, key := range []string{"expected_generation_id", "expected_generation_fingerprint", "expected_policy_fingerprint"} {
		if _, exists := object[key]; exists {
			guard.specified = true
		}
	}
	return nil
}

func visualGuardMatches(guard VisualOperationGuard, policy *VisualRuntimePolicy) bool {
	return policy != nil && policy.GenerationID > 0 && policy.GenerationFingerprint != "" && policy.CurrentPolicyFingerprint != "" && guard.ExpectedGenerationID != nil && *guard.ExpectedGenerationID > 0 && guard.ExpectedGenerationFingerprint != nil && *guard.ExpectedGenerationFingerprint != "" && guard.ExpectedPolicyFingerprint != nil && *guard.ExpectedPolicyFingerprint != "" && *guard.ExpectedGenerationID == policy.GenerationID && *guard.ExpectedGenerationFingerprint == policy.GenerationFingerprint && *guard.ExpectedPolicyFingerprint == policy.CurrentPolicyFingerprint
}

func (s *Server) guardVisualCallback(ctx context.Context, guard VisualOperationGuard, observed *VisualRuntimePolicy, callback func(context.Context) error) error {
	if !guard.specified {
		return callback(ctx)
	}
	// Keep installation and execution in one runtime epoch. Status reads and
	// unrelated vector updates remain available during bounded provider work.
	s.visualRuntimeMu.RLock()
	defer s.visualRuntimeMu.RUnlock()
	s.vectorMu.RLock()
	current := s.visualPolicy
	s.vectorMu.RUnlock()
	if current != observed || !visualGuardMatches(guard, observed) {
		return ErrVisualPolicyChanged
	}
	return callback(context.WithValue(ctx, visualGuardContextKey{}, guard))
}

func checkVisualRequestGuard(w http.ResponseWriter, guard VisualOperationGuard, policy *VisualRuntimePolicy) bool {
	if guard.specified && !visualGuardMatches(guard, policy) {
		writeVisualPolicyChanged(w)
		return false
	}
	return true
}

func writeVisualPolicyChanged(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "visual_policy_changed", "The initialized visual generation or upload policy does not match the expected guard")
}

func (s *Server) handleVisualRuntimePolicy(w http.ResponseWriter, _ *http.Request) {
	s.vectorMu.RLock()
	policy := s.visualPolicy
	s.vectorMu.RUnlock()
	if policy == nil || policy.GenerationID <= 0 || policy.CurrentPolicyFingerprint == "" {
		writeError(w, http.StatusServiceUnavailable, "visual_policy_unavailable", "The initialized visual upload policy is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, policy.clone())
}
