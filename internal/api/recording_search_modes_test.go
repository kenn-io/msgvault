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
			f := newMediaSearchFixture(t)
			status, _, raw := searchMediaFor(t, f.server(true), tc.query)
			require.Equal(t, tc.status, status, raw)
			if status != http.StatusOK {
				assert.Empty(t, f.requests)
				assert.Zero(t, f.validated)
				return
			}
			var report struct {
				RequestedMode string   `json:"requested_mode"`
				ActualMode    string   `json:"actual_mode"`
				Degradations  []string `json:"degradations"`
				Partial       bool     `json:"partial"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &report))
			assert.Equal(t, tc.requested, report.RequestedMode)
			assert.Equal(t, tc.actual, report.ActualMode)
			assert.Equal(t, tc.degradations, report.Degradations)
			assert.Equal(t, tc.requested != tc.actual, report.Partial)
		})
	}
}
