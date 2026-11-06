package callhistory

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func exampleObservation() Observation {
	return Observation{
		RecordID: "record-1",

		Version: 1, Provider: "signal", Account: "example-account",
		Direction: "incoming", Outcome: "answered",
		OwnerParticipation: "joined",
		OccurredAt:         time.Date(2026, 10, 1, 9, 0, 0, 0, time.FixedZone("", -7*3600)),

		Participants: []Participant{{ID: 1, Participation: "joined"}},
		Raw:          []byte(`{"callId":"record-1","status":"Accepted"}`),
	}
}

func TestObservationValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Observation)
		valid  bool
	}{
		{"answered", func(*Observation) {}, true},
		{"unknown status", func(o *Observation) { o.Outcome = "unknown" }, true},
		{"invalid status", func(o *Observation) { o.Outcome = "perhaps" }, false},
		{"missing time", func(o *Observation) { o.OccurredAt = time.Time{} }, false},
		{"blank account", func(o *Observation) { o.Account = " " }, false},
		{"zero participant", func(o *Observation) { o.Participants[0].ID = 0 }, false},
		{"negative participant", func(o *Observation) { o.Participants[0].ID = -1 }, false},
		{"conflicting participant", func(o *Observation) {
			o.Participants = append(o.Participants, Participant{ID: 1, Participation: "unknown"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			o := exampleObservation()
			tc.change(&o)
			if tc.valid {
				require.NoError(o.Validate())
			} else {
				require.ErrorIs(o.Validate(), ErrInvalid)
			}
		})
	}
}

func FuzzObservationDurationValidation(f *testing.F) {
	f.Add(int64(0), true)
	f.Add(int64(25), false)
	f.Add(int64(-2), true)
	f.Fuzz(func(t *testing.T, halfSeconds int64, hasDuration bool) {
		duration := float64(halfSeconds) / 2
		o := exampleObservation()
		if hasDuration {
			o.DurationSeconds = &duration
		}
		if hasDuration && duration < 0 {
			require.ErrorIs(t, o.Validate(), ErrInvalid)
			return
		}
		require.NoError(t, o.Validate())
	})
}
