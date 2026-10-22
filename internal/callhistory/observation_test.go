package callhistory

import (
	"bytes"
	"encoding/json/v2"
	"math"
	"strconv"
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

func FuzzNormalizedImportPreservesWireFields(f *testing.F) {
	f.Add(uint64(0), uint32(0), uint32(0), uint8(0), uint32(1))
	f.Add(math.Float64bits(42.5), uint32(1790841600), uint32(999999999), uint8(255), uint32(42))
	f.Fuzz(func(t *testing.T, durationBits uint64, seconds, nanoseconds uint32, peer uint8, callIDSuffix uint32) {
		assertions := assert.New(t)
		requirements := require.New(t)

		// Generate finite, nonnegative durations while preserving broad float coverage.
		durationBits &^= uint64(1) << 63
		if durationBits>>52 == 0x7ff {
			durationBits &^= uint64(1) << 52
		}
		duration := math.Float64frombits(durationBits)
		occurredAt := time.Unix(int64(seconds), int64(nanoseconds%1_000_000_000)).UTC()
		nativeCallID := "call-" + strconv.FormatUint(uint64(callIDSuffix), 10)
		participantID := int64(peer) + 1

		// Keep these public JSON field names independent of Observation's tags.
		record, err := json.Marshal(map[string]any{
			"record_id": "record-1", "version": 1, "provider": "signal",
			"account": "example-account", "native_call_id": nativeCallID,
			"kind": "call", "direction": "incoming", "outcome": "answered",
			"owner_participation": "joined", "occurred_at": occurredAt,
			"duration_seconds": duration, "precision": "instant",
			"participants": []map[string]any{{"participant_id": participantID, "participation": "joined"}},
			"raw":          map[string]any{"callId": nativeCallID, "status": "Accepted"},
		})
		requirements.NoError(err)
		wire, err := json.Marshal(ImportRequest{Format: "normalized", Account: "example-account", Record: record})
		requirements.NoError(err)
		got, err := DecodeImport(bytes.NewReader(wire))
		requirements.NoError(err)

		assertions.Equal("record-1", got.RecordID)
		assertions.Equal("example-account", got.Account)
		assertions.Equal(nativeCallID, got.NativeCallID)
		assertions.True(occurredAt.Equal(got.OccurredAt))
		requirements.NotNil(got.DurationSeconds)
		assertions.Equal(math.Float64bits(duration), math.Float64bits(*got.DurationSeconds))
		assertions.Equal([]Participant{{ID: participantID, Participation: "joined"}}, got.Participants)
		expectedKey := (Evidence{Provider: "signal", Account: "example-account", NativeCallID: nativeCallID}).CanonicalKey()
		assertions.Equal(expectedKey, got.CanonicalKey())
	})
}
