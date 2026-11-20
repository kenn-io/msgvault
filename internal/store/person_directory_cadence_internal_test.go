package store

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectoryCadenceBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	last := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("source", 3600))
	due := last.UTC().AddDate(0, 0, 21)
	values := []contactFrequencyValue{{Days: 21, Valid: true}}
	exact := directoryCadence(ContactState{LastContactAt: &last}, values, due)
	require.NotNil(exact.DueAt)
	assert.Equal(due, *exact.DueAt)
	assert.Equal(CadenceOK, exact.Status)
	assert.Nil(exact.DaysOverdue)
	after := directoryCadence(ContactState{LastContactAt: &last}, values, due.Add(time.Nanosecond))
	assert.Equal(CadenceOverdue, after.Status)
	require.NotNil(after.DaysOverdue)
	assert.Zero(*after.DaysOverdue)
	stale := directoryCadence(ContactState{LastContactAt: &last, Stale: true}, values, due.Add(48*time.Hour))
	assert.Nil(stale.DueAt)
	assert.Equal(CadenceUnknown, stale.Status)
}

func FuzzDirectoryCadenceAssignment(f *testing.F) {
	for _, days := range []int64{0, -1, 21, 366*10000 + 1, 1<<63 - 1} {
		f.Add(days, false)
		f.Add(days, true)
	}
	f.Fuzz(func(t *testing.T, days int64, duplicate bool) {
		values := []contactFrequencyValue{{Days: days, Valid: true}}
		if duplicate {
			values = append(values, values[0])
		}
		cadence := directoryCadence(ContactState{Stale: true}, values, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		if days > 0 && !duplicate {
			require.NotNil(t, cadence.FrequencyDays)
			assert.Equal(t, days, *cadence.FrequencyDays)
		} else {
			assert.Nil(t, cadence.FrequencyDays)
		}
	})
}

func FuzzContactFrequencyColumnIsolation(f *testing.F) {
	columnMasks := [...]uint16{1, 2, 4, 8, 32, 64, 384}
	for column := range columnMasks {
		f.Add(uint8(column), int64(21), false)
		f.Add(uint8(column), int64(21), true)
	}
	f.Add(uint8(1), int64(0), true)
	f.Fuzz(func(t *testing.T, column uint8, days int64, integerKind bool) {
		mask := columnMasks[int(column)%len(columnMasks)]
		var fields [9]sql.NullString
		for i := range fields {
			if mask&(1<<i) != 0 {
				fields[i] = sql.NullString{Valid: true, String: strconv.FormatInt(days, 10)}
			}
		}
		kind := "text"
		if integerKind {
			kind = "integer"
		}
		value := decodeContactFrequency(kind, fields)
		wantValid := integerKind && mask == 2
		assert.Equal(t, wantValid, value.Valid)
		if wantValid {
			assert.Equal(t, days, value.Days)
		}
	})
}
