package client

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestCallDurationPreservesUnknownAndZero(t *testing.T) {
	for _, value := range []string{"null", "0", "12.5"} {
		t.Run(value, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var observation generated.CallObservationView
			requirements.NoError(json.Unmarshal([]byte(`{"duration_seconds":`+value+`}`), &observation))
			encoded, err := json.Marshal(observation)
			requirements.NoError(err)
			var fields map[string]any
			requirements.NoError(json.Unmarshal(encoded, &fields))
			duration, present := fields["duration_seconds"]
			requirements.True(present)
			var want any
			requirements.NoError(json.Unmarshal([]byte(value), &want))
			assertions.Equal(want, duration)
		})
	}
}
