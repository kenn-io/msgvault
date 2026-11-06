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

func FuzzNormalizedImportRoundtrip(f *testing.F) {
	f.Add(uint64(0), true)
	f.Add(uint64(0), false)
	f.Add(math.Float64bits(math.MaxFloat64), true)
	f.Fuzz(func(t *testing.T, bits uint64, hasDuration bool) {
		// Materialize any finite nonnegative IEEE value, without rejecting
		// generated inputs. Validation of negative/nonfinite inputs is separate.
		bits &^= uint64(1) << 63
		if bits>>52 == 0x7ff {
			bits &^= uint64(1) << 52
		}
		o := exampleObservation()
		if hasDuration {
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
		assert.Equal(t, o.DurationSeconds, got.DurationSeconds)
		assert.True(t, o.OccurredAt.Equal(got.OccurredAt))
		assert.Equal(t, o.Participants, got.Participants)
		assert.Equal(t, o.CanonicalKey(), got.CanonicalKey())
	})
}
