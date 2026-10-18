package jsonexact_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jsonexact"
)

type duplicateEnvelope struct {
	Name   string            `json:"name"`
	Nested *duplicateNested  `json:"nested"`
	Items  []duplicateNested `json:"items"`
}

type duplicateNested struct {
	Value string `json:"value"`
}

type timestampEnvelope struct {
	Timestamp *time.Time `json:"timestamp"`
}

func TestValidateRejectsDuplicateMembersRecursively(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"root", `{"name":"first","name":"second"}`},
		{"nested object", `{"nested":{"value":"first","value":"second"}}`},
		{"object in array", `{"items":[{"value":"first","value":"second"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := jsonexact.Validate([]byte(test.data), duplicateEnvelope{})
			require.Error(t, err)
			assert.ErrorContains(t, err, "duplicate")
		})
	}
}

func TestValidateAcceptsUniqueMembersRecursively(t *testing.T) {
	err := jsonexact.Validate(
		[]byte(`{"name":"one","nested":{"value":"two"},"items":[{"value":"three"}]}`),
		duplicateEnvelope{},
	)
	require.NoError(t, err)
}

func FuzzValidateTreatsTimeAsScalar(f *testing.F) {
	f.Add("2026-01-02T03:04:05Z")
	f.Add("not a timestamp")
	f.Fuzz(func(t *testing.T, timestamp string) {
		data, err := json.Marshal(struct {
			Timestamp string `json:"timestamp"`
		}{Timestamp: timestamp})
		require.NoError(t, err)
		require.NoError(t, jsonexact.Validate(data, timestampEnvelope{}))
	})
}
