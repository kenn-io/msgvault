package macoscalls

import (
	"encoding/json/v2"
	"math"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exampleRecord() record {
	return record{"ZUNIQUE_ID": "synthetic-call", "ZDATE": float64(0), "ZANSWERED": int64(1), "ZORIGINATED": int64(0), "ZDURATION": float64(12.5), "ZADDRESS": "peer@example.com", "ZSERVICE_PROVIDER": "com.apple.FaceTime", "ZCALLTYPE": int64(8)}
}

func TestRecordEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, flag := range []any{nil, int64(-1), int64(0), int64(1), int64(2)} {
		r := exampleRecord()
		r["ZANSWERED"] = flag
		r["ZORIGINATED"] = flag
		o, err := r.observation("example")
		require.NoError(err)
		direction, outcome, owner := "unknown", "unknown", "unknown"
		if flag == int64(0) {
			direction, owner = "incoming", "not_joined"
		}
		if flag == int64(1) {
			direction, outcome, owner = "outgoing", "answered", "joined"
		}
		assert.Equal(direction, o.Direction)
		assert.Equal(outcome, o.Outcome)
		assert.Equal(owner, o.OwnerParticipation)
		assert.Equal(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), o.OccurredAt)
		assert.Equal("synthetic-call", o.NativeCallID)
		assert.Equal(o.NativeCallID, o.RecordID)
		assert.Empty(o.Participants)
		assert.Empty(o.AccessPoints)
		assert.Nil(o.EndedAt)
		assert.Empty(o.Timezone)
		if flag == int64(1) {
			require.NotNil(o.DurationSeconds)
			assert.InDelta(12.5, *o.DurationSeconds, 0)
		} else {
			assert.Nil(o.DurationSeconds)
		}
		var raw map[string]any
		require.NoError(json.Unmarshal(o.Raw, &raw))
		assert.Equal("peer@example.com", raw["ZADDRESS"])
	}
}

func TestRecordRejectsInvalidEvidence(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"ZUNIQUE_ID", " "},
		{"ZDATE", "123"}, {"ZDATE", math.Inf(1)}, {"ZDATE", math.NaN()}, {"ZDATE", math.MaxFloat64},
		{"ZDURATION", -1.0}, {"ZDURATION", math.NaN()}, {"ZDURATION", math.Inf(1)}, {"ZANSWERED", 1.0}, {"ZORIGINATED", "secret"}, {"ZADDRESS", int64(3)},
	} {
		r := exampleRecord()
		r[tc.key] = tc.value
		_, err := r.observation("example")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	}
}

func TestRecordStorageClasses(t *testing.T) {
	require := require.New(t)
	for _, c := range columns {
		r := exampleRecord()
		r[c.name] = []byte("private-sentinel")
		_, err := r.observation("example")
		require.Error(err)
		assert.NotContains(t, err.Error(), "private-sentinel")
		r[c.name] = nil
		_, err = r.observation("example")
		if c.name == "ZUNIQUE_ID" || c.name == "ZDATE" {
			require.Error(err)
		} else {
			require.NoError(err)
		}
	}
}

func FuzzRecordEpoch(f *testing.F) {
	for _, v := range []float64{0, -0.25, 0.123456789, 123456789.25, -63113904000, math.Nextafter(-63113904000, math.Inf(1)), -62135596801 - 978307200.0, 253402300800 - 978307200.0, math.Inf(1), math.NaN()} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, seconds float64) {
		r := exampleRecord()
		r["ZDATE"] = seconds
		o, err := r.observation("example")
		// The import contract rejects Go's zero time as a missing timestamp.
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= -62135596800-978307200.0 || seconds >= 253402300800-978307200.0 {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		// Exact rational arithmetic is independent of the mapper's floating-point split.
		nanos := new(big.Rat).Mul(new(big.Rat).SetFloat64(seconds), big.NewRat(1e9, 1))
		total, rem := new(big.Int), new(big.Int)
		total.QuoRem(nanos.Num(), nanos.Denom(), rem)
		if new(big.Int).Lsh(new(big.Int).Abs(rem), 1).Cmp(nanos.Denom()) >= 0 {
			total.Add(total, big.NewInt(int64(nanos.Sign())))
		}
		sec, ns := new(big.Int), new(big.Int)
		sec.QuoRem(total, big.NewInt(1e9), ns)
		assert.True(t, time.Unix(sec.Int64()+978307200, ns.Int64()).Equal(o.OccurredAt))
	})
}

func FuzzRecordDurationDoesNotEstablishAttendance(f *testing.F) {
	f.Add(int64(0), int64(0), 12.5, false, true)
	f.Add(int64(1), int64(1), 0.0, false, false)
	f.Add(int64(7), int64(-1), math.MaxFloat64, true, false)
	f.Fuzz(func(t *testing.T, answered, originated int64, duration float64, answerNull, originNull bool) {
		r := exampleRecord()
		r["ZANSWERED"], r["ZORIGINATED"], r["ZDURATION"] = answered, originated, duration
		if answerNull {
			r["ZANSWERED"] = nil
		}
		if originNull {
			r["ZORIGINATED"] = nil
		}
		o, err := r.observation("example")
		if duration < 0 || math.IsInf(duration, 0) || math.IsNaN(duration) {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		assert.Equal(t, !answerNull && answered == 1, o.Outcome == "answered")
		assert.Equal(t, !answerNull && answered == 1, o.OwnerParticipation == "joined")
		assert.Empty(t, o.Participants)
		direction := "unknown"
		if !originNull {
			if originated == 0 {
				direction = "incoming"
			}
			if originated == 1 {
				direction = "outgoing"
			}
		}
		assert.Equal(t, direction, o.Direction)
		if answerNull || answered != 1 {
			assert.Nil(t, o.DurationSeconds)
		} else {
			require.NotNil(t, o.DurationSeconds)
			assert.InDelta(t, duration, *o.DurationSeconds, 0)
		}
	})
}

func FuzzRecordIdentityIgnoresContactMetadata(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(-1))
	f.Fuzz(func(t *testing.T, suffix int64) {
		r := exampleRecord()
		before, err := r.observation("example")
		require.NoError(t, err)
		for _, key := range []string{"ZADDRESS", "ZNAME", "ZPARTICIPANTGROUPUUID", "ZORIGINATINGDEVICENAME"} {
			r[key] = "synthetic-" + strconv.FormatInt(suffix, 10)
		}
		after, err := r.observation("example")
		require.NoError(t, err)
		assert.Equal(t, before.CanonicalKey(), after.CanonicalKey())
		assert.Equal(t, before.SourceIdentifier(), after.SourceIdentifier())
		assert.Equal(t, before.RecordID, after.RecordID)
	})
}
