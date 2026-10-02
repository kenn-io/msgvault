package vector

import "time"

// GenerationStatusReport is the owning CLI's structured generation listing.
// Scope describes the resolved configuration used to compute live coverage.
// The report contains neither database paths nor accelerator diagnostics.
type GenerationStatusReport struct {
	ConfiguredGenerationFingerprint string             `json:"configured_generation_fingerprint"`
	SourceIDs                       []int64            `json:"source_ids"`
	MessageTypes                    []string           `json:"message_types"`
	Generations                     []GenerationStatus `json:"generations"`
}

type GenerationStatus struct {
	ID                GenerationID      `json:"id"`
	Model             string            `json:"model"`
	Dimension         int               `json:"dimension"`
	Fingerprint       string            `json:"fingerprint"`
	State             GenerationState   `json:"state"`
	StartedAt         time.Time         `json:"started_at"`
	SeededAt          *time.Time        `json:"seeded_at,omitempty"`
	CompletedAt       *time.Time        `json:"completed_at,omitempty"`
	ActivatedAt       *time.Time        `json:"activated_at,omitempty"`
	MessageCount      int64             `json:"message_count"`
	CoverageAvailable bool              `json:"coverage_available"`
	LiveCount         int64             `json:"live_count"`
	EmbeddedCount     int64             `json:"embedded_count"`
	BlankCount        int64             `json:"blank_count"`
	MissingCount      int64             `json:"missing_count"`
	Accelerator       AcceleratorStatus `json:"accelerator"`
}

type AcceleratorStatus struct {
	State        string     `json:"state"`
	IndexedCount int64      `json:"indexed_count"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	HasError     bool       `json:"has_error"`
}
