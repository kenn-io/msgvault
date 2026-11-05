package callhistory

import (
	"bytes"
	"encoding/json/v2"
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
		{"scheduled owner joined", func(o *Observation) {
			o.Kind, o.Outcome = "scheduled_meeting", "unknown"
			o.Participants[0].Participation = "invited"
		}, false},
		{"scheduled participant joined", func(o *Observation) {
			o.Kind, o.Outcome, o.OwnerParticipation = "scheduled_meeting", "unknown", "unknown"
		}, false},
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
			require := require.New(t)
			o := exampleObservation()
			tc.change(&o)
			if o.Kind == "scheduled_meeting" {
				record, err := json.Marshal(o)
				require.NoError(err)
				wire, err := json.Marshal(ImportRequest{Format: "normalized", Account: o.Account, Record: record})
				require.NoError(err)
				_, err = DecodeImport(bytes.NewReader(wire))
				require.ErrorIs(err, ErrInvalid)
			}
			if tc.valid {
				require.NoError(o.Validate())
			} else {
				require.ErrorIs(o.Validate(), ErrInvalid)
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
	f.Add(2.0, true, true)
	f.Add(0.0, true, true)
	f.Add(-1.0, false, true)
	f.Add(math.Inf(1), false, true)
	f.Add(math.NaN(), false, true)
	f.Fuzz(func(t *testing.T, duration float64, scheduled, hasDuration bool) {
		o := exampleObservation()
		if hasDuration {
			o.DurationSeconds = &duration
		}
		if scheduled {
			o.Kind = "scheduled_meeting"
			o.Outcome, o.OwnerParticipation = "unknown", "unknown"
			o.Participants[0].Participation = "invited"
		}
		if scheduled && hasDuration && !math.IsNaN(duration) && !math.IsInf(duration, 0) {
			record, err := json.Marshal(o)
			require.NoError(t, err)
			wire, err := json.Marshal(ImportRequest{Format: "normalized", Account: o.Account, Record: record})
			require.NoError(t, err)
			_, err = DecodeImport(bytes.NewReader(wire))
			require.ErrorIs(t, err, ErrInvalid)
		}
		if hasDuration && (scheduled || duration < 0 || math.IsNaN(duration) || math.IsInf(duration, 0)) {
			require.ErrorIs(t, o.Validate(), ErrInvalid)
			return
		}
		require.NoError(t, o.Validate())
	})
}

func FuzzNormalizedImportRoundtrip(f *testing.F) {
	f.Add(uint64(0), false, true)
	f.Add(uint64(0), false, false)
	f.Add(math.Float64bits(math.MaxFloat64), false, true)
	f.Add(uint64(0), true, false)
	f.Fuzz(func(t *testing.T, bits uint64, scheduled, hasDuration bool) {
		// Materialize any finite nonnegative IEEE value, without rejecting
		// generated inputs. Validation of negative/nonfinite inputs is separate.
		bits &^= uint64(1) << 63
		if bits>>52 == 0x7ff {
			bits &^= uint64(1) << 52
		}
		o := exampleObservation()
		if scheduled {
			o.Kind, o.Outcome, o.OwnerParticipation = "scheduled_meeting", "unknown", "unknown"
			o.Participants = []Participant{}
		} else if hasDuration {
			o.NativeCallID = "call-1"
			duration := math.Float64frombits(bits)
			o.DurationSeconds = &duration
		}
		record, err := json.Marshal(o)
		require.NoError(t, err)
		wire, err := json.Marshal(ImportRequest{Format: "normalized", Account: o.Account, Record: record})
		require.NoError(t, err)
		got, err := DecodeImport(bytes.NewReader(wire))
		require.NoError(t, err)
		assert.Equal(t, o.RecordID, got.RecordID)
		assert.Equal(t, o.NativeCallID, got.NativeCallID)
		assert.Equal(t, scheduled || !hasDuration, got.CanonicalKey() == "")
		assert.Equal(t, o.DurationSeconds, got.DurationSeconds)
		assert.True(t, o.OccurredAt.Equal(got.OccurredAt))
		assert.Equal(t, o.Participants, got.Participants)
		assert.Equal(t, o.CanonicalKey(), got.CanonicalKey())
	})
}
