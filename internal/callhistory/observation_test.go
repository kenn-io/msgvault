package callhistory

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exampleObservation() Observation {
	return Observation{
		RecordID: "record-1",

		Version: 1, Provider: "signal", Account: "example-account",
		Kind: "call", Direction: "incoming", Outcome: "answered",
		OwnerParticipation: "joined", Precision: "instant",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.FixedZone("", -7*3600)),

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
		{"infinite duration", func(o *Observation) { n := math.Inf(1); o.DurationSeconds = &n }, false},
		{"nan duration", func(o *Observation) { n := math.NaN(); o.DurationSeconds = &n }, false},
		{"scheduled attendance", func(o *Observation) { o.Kind = "scheduled_meeting" }, false},
		{"unknown status", func(o *Observation) { o.Outcome = "unknown" }, true},
		{"invalid status", func(o *Observation) { o.Outcome = "perhaps" }, false},
		{"missing time", func(o *Observation) { o.OccurredAt = time.Time{} }, false},
		{"negative participant", func(o *Observation) { o.Participants[0].ID = -1 }, false},
		{"conflicting participant", func(o *Observation) {
			o.Participants = append(o.Participants, Participant{ID: 1, Participation: "unknown"})
		}, false},
		{"invalid raw", func(o *Observation) { o.Raw = []byte(`{`) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := exampleObservation()
			tc.change(&o)
			if tc.valid {
				require.NoError(t, o.Validate())
			} else {
				require.ErrorIs(t, o.Validate(), ErrInvalid)
			}
		})
	}
}

func FuzzCanonicalCallIdentity(f *testing.F) {
	f.Add("signal", "a", "one", "signal", "b", "one")
	f.Add("signal", "a", "one", "other", "a", "one")
	f.Add("signal", "a", "one", "signal", "a", "two")
	f.Add("signal", "a", "one", "signal", "a", "one")
	f.Add("signal", "a", "", "signal", "a", "one")
	f.Add("signal", "a", "one", "signal", "a", "")
	f.Add("signal", "a", "", "signal", "b", "")
	f.Fuzz(func(t *testing.T, providerA, accountA, idA, providerB, accountB, idB string) {
		a := identityEvidence(providerA, accountA, idA)
		b := identityEvidence(providerB, accountB, idB)
		if a.Validate() != nil || b.Validate() != nil {
			return
		}
		if idA == "" || idB == "" {
			assert.Equal(t, idA == "", a.CanonicalKey() == "")
			assert.Equal(t, idB == "", b.CanonicalKey() == "")
		} else if providerA == providerB && accountA == accountB && idA == idB {
			assert.Equal(t, a.CanonicalKey(), b.CanonicalKey())
		} else {
			assert.NotEqual(t, a.CanonicalKey(), b.CanonicalKey())
		}
	})
}

func identityEvidence(provider, account, nativeCallID string) Evidence {
	o := exampleObservation()
	o.Provider, o.Account, o.NativeCallID = provider, account, nativeCallID
	return o.Evidence
}

func FuzzObservationDurationValidation(f *testing.F) {
	f.Add(0.0, false, true)
	f.Add(12.5, true, false)
	f.Add(-1.0, false, true)
	f.Fuzz(func(t *testing.T, duration float64, scheduled, hasDuration bool) {
		o := exampleObservation()
		if hasDuration {
			o.DurationSeconds = &duration
		}
		if scheduled {
			o.Kind = "scheduled_meeting"
			o.Outcome, o.OwnerParticipation = "unknown", "unknown"
			o.Participants = nil
		}
		if hasDuration && (scheduled || duration < 0 || math.IsNaN(duration) || math.IsInf(duration, 0)) {
			require.ErrorIs(t, o.Validate(), ErrInvalid)
			return
		}
		require.NoError(t, o.Validate())
	})
}
