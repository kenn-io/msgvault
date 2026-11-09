package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These are configuration/reporting contracts. Real retrieval qualification
// lives in the separate suite connected to a production Docbank router.
func TestRecordingSearchModeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, query       string
		status            int
		requested, actual string
		degradations      []string
	}{
		{"default", "q=words", http.StatusOK, "lexical", "lexical", nil},
		{"hybrid without profile", "q=words&recordings_mode=hybrid", http.StatusOK, "hybrid", "unavailable", []string{"semantic_unconfigured", "empty_recording_scope"}},
		{"semantic without profile", "q=words&recordings_mode=semantic", http.StatusServiceUnavailable, "", "", nil},
		{"invalid", "q=words&recordings_mode=invalid", http.StatusBadRequest, "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newMediaSearchFixture(t)
			status, _, raw := searchMediaFor(t, f.server(true), tc.query)
			requirements.Equal(tc.status, status, raw)
			if status != http.StatusOK {
				assertions.Empty(f.requests)
				assertions.Zero(f.validated)
				return
			}
			var report struct {
				RequestedMode string   `json:"requested_mode"`
				ActualMode    string   `json:"actual_mode"`
				Degradations  []string `json:"degradations"`
				Partial       bool     `json:"partial"`
			}
			requirements.NoError(json.Unmarshal([]byte(raw), &report))
			assertions.Equal(tc.requested, report.RequestedMode)
			assertions.Equal(tc.actual, report.ActualMode)
			assertions.Equal(tc.degradations, report.Degradations)
			assertions.Equal(tc.requested != tc.actual, report.Partial)
		})
	}
}
